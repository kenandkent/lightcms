package services

import (
	"context"
	"errors"
	"fmt"
	"github.com/metavert-io/lightcms/v7/internal/models"
	"github.com/metavert-io/lightcms/v7/internal/product/jobs"
	"github.com/metavert-io/lightcms/v7/internal/testutil"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"testing"
)

func TestDurableForkPartialRetrySkipsCompletedDraft(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	defer db.SetFaultHook(nil)
	ctx := context.Background()
	cs := NewContentService(db)
	s := NewForkService(db, cs)
	runner := jobs.NewRunner(jobs.NewRepository(db, 10), 1)
	s.EnableDurable(runner, func(context.Context, jobs.Job) error { return nil })
	uid := primitive.NewObjectID()
	fork, err := s.Create(ctx, "Durable partial", "", uid, "owner", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"durable-one", "durable-two"} {
		if err := cs.CreateContent(ctx, &models.Content{Title: slug, Slug: slug, ForkID: &fork.ID}); err != nil {
			t.Fatal(err)
		}
	}
	job, err := s.SubmitMerge(ctx, fork.ID, uid, "owner")
	if err != nil {
		t.Fatal(err)
	}
	writes := 0
	db.SetFaultHook(func(op, collection string) error {
		if op == "InsertOne" && collection == "content" {
			writes++
			if writes == 2 {
				return errors.New("second draft failed")
			}
		}
		return nil
	})
	if err := runner.RunOnce(ctx, "fork_merge"); err != nil {
		t.Fatal(err)
	}
	db.SetFaultHook(nil)
	partial, err := runner.Repo.Get(ctx, job.ID)
	if err != nil || partial.State != "partial" {
		t.Fatalf("partial not retained: %+v %v", partial, err)
	}
	result, err := s.DurableMergeResult(ctx, job.ID)
	if err != nil || result.Created != 1 || len(result.Failed) != 1 {
		t.Fatalf("partial items wrong: %+v %v", result, err)
	}
	replay, err := s.SubmitMerge(ctx, fork.ID, uid, "owner")
	if err != nil || replay.ID != job.ID {
		t.Fatalf("retry lost identity: %v", err)
	}
	if err := runner.RunOnce(ctx, "fork_merge"); err != nil {
		t.Fatal(err)
	}
	result, err = s.DurableMergeResult(ctx, job.ID)
	if err != nil || result.Created != 2 || len(result.Failed) != 0 {
		t.Fatalf("local retry lost items: %+v %v", result, err)
	}
	for _, id := range result.ContentIDs {
		contentID, err := primitive.ObjectIDFromHex(id)
		if err != nil {
			t.Fatal(err)
		}
		var page models.Content
		if err := db.FindOne(ctx, "content", bson.M{"_id": contentID}, &page); err != nil {
			t.Fatal(err)
		}
		if page.CurrentVersion != 1 || page.Published {
			t.Fatalf("completed item edited again or published: %+v", page)
		}
	}
}

func TestDurableForkMergeFreezesInputAndDoesNotPublish(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	for _, name := range []string{"workflow_jobs", "workflow_items", "workflow_capacity", "workflow_events"} {
		defer db.Collection(name).Drop(ctx)
	}
	cs := NewContentService(db)
	s := NewForkService(db, cs)
	runner := jobs.NewRunner(jobs.NewRepository(db, 10), 1)
	s.EnableDurable(runner, func(context.Context, jobs.Job) error { return nil })
	uid := primitive.NewObjectID()
	fork, err := s.Create(ctx, "Durable", "", uid, "owner", "")
	if err != nil {
		t.Fatal(err)
	}
	page := &models.Content{Title: "Frozen fork", Slug: "durable-fork", ForkID: &fork.ID}
	if err := cs.CreateContent(ctx, page); err != nil {
		t.Fatal(err)
	}
	job, err := s.SubmitMerge(ctx, fork.ID, uid, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Collection("content").UpdateOne(ctx, bson.M{"_id": page.ID}, bson.M{"$set": bson.M{"title": "Later edit"}}); err != nil {
		t.Fatal(err)
	}
	freshRunner := jobs.NewRunner(jobs.NewRepository(db, 10), 1)
	fresh := NewForkService(db, cs)
	fresh.EnableDurable(freshRunner, func(context.Context, jobs.Job) error { return nil })
	if err := freshRunner.RunOnce(ctx, "fork_merge"); err != nil {
		t.Fatal(err)
	}
	var live models.Content
	if err := db.FindOne(ctx, "content", bson.M{"full_path": "/durable-fork", "fork_id": bson.M{"$exists": false}}, &live); err != nil {
		t.Fatal(err)
	}
	if live.Title != "Frozen fork" || live.Published {
		t.Fatalf("merge used mutable input or published: %+v", live)
	}
	stored, err := freshRunner.Repo.Get(ctx, job.ID)
	if err != nil || stored.State != "partial" {
		t.Fatalf("new fork edits were not reported for review: %+v %v", stored, err)
	}
	result, err := fresh.DurableMergeResult(ctx, job.ID)
	if err != nil || result.Created != 1 || len(result.Failed) != 1 {
		t.Fatalf("late edits were hidden from the merge result: %+v %v", result, err)
	}
	var retained models.Content
	if err := db.FindOne(ctx, "content", bson.M{"_id": page.ID}, &retained); err != nil || retained.Title != "Later edit" {
		t.Fatalf("post-submission edit lost: %+v %v", retained, err)
	}
	var merged struct {
		Email string `bson:"merged_by_email"`
	}
	if err := db.FindOne(ctx, "content_forks", bson.M{"_id": fork.ID}, &merged); err != nil || merged.Email != "owner" {
		t.Fatalf("merge lost frozen attribution: %+v %v", merged, err)
	}
}

func TestDurableForkSubmissionRechecksAuthorityAndArchivedState(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	s := NewForkService(db, NewContentService(db))
	runner := jobs.NewRunner(jobs.NewRepository(db, 10), 1)
	revoked := false
	s.EnableDurable(runner, func(context.Context, jobs.Job) error {
		if revoked {
			return fmt.Errorf("revoked")
		}
		return nil
	})
	uid := primitive.NewObjectID()
	fork, err := s.Create(ctx, "Submit guard", "", uid, "owner", "")
	if err != nil {
		t.Fatal(err)
	}
	revoked = true
	if _, err := s.SubmitMerge(ctx, fork.ID, uid, "owner"); err == nil {
		t.Fatal("revoked actor submitted merge")
	}
	revoked = false
	if _, err := s.SubmitMerge(ctx, fork.ID, uid, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateOne(ctx, "content_forks", bson.M{"_id": fork.ID}, bson.M{"$set": bson.M{"status": "archived"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitMerge(ctx, fork.ID, uid, "owner"); err == nil {
		t.Fatal("archived fork replay accepted")
	}
}
