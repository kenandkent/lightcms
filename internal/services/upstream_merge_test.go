package services

import (
	"context"
	"github.com/metavert-io/lightcms/v7/internal/models"
	"github.com/metavert-io/lightcms/v7/internal/product/jobs"
	"github.com/metavert-io/lightcms/v7/internal/product/publication"
	"github.com/metavert-io/lightcms/v7/internal/testutil"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"testing"
	"time"
)

func TestDurableMergeReportsEditsArrivingDuringFinalization(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	cs := NewContentService(db)
	s := NewForkService(db, cs)
	runner := jobs.NewRunner(jobs.NewRepository(db, 10), 1)
	s.EnableDurable(runner, func(context.Context, jobs.Job) error { return nil })
	actor := primitive.NewObjectID()
	fork, err := s.Create(ctx, "Finalization edit", "", actor, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.CreateContent(ctx, &models.Content{Title: "Submitted", Slug: "submitted-finalization", ForkID: &fork.ID}); err != nil {
		t.Fatal(err)
	}
	submitted, err := s.SubmitMerge(ctx, fork.ID, actor, "owner")
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := runner.Repo.Claim(ctx, "fork_merge", "finalization-test", time.Minute)
	if err != nil || claimed.ID != submitted.ID {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	if err := s.runDurableMerge(ctx, *claimed); err != nil {
		t.Fatal(err)
	}
	late := &models.Content{Title: "Late", Slug: "late-finalization", ForkID: &fork.ID}
	if err := cs.CreateContent(ctx, late); err != nil {
		t.Fatal(err)
	}
	if err := runner.Repo.FinishWith(ctx, *claimed, "succeeded", "", s.finalizeDurableMerge); err != nil {
		t.Fatal(err)
	}
	result, err := s.DurableMergeResult(ctx, submitted.ID)
	if err != nil || result.Created != 1 || len(result.Failed) != 1 {
		t.Fatalf("finalization hid preserved edits: %+v %v", result, err)
	}
	if _, err := cs.GetContent(ctx, late.ID); err != nil {
		t.Fatalf("late copy was deleted: %v", err)
	}
}

func TestDurableMergePersistsPublishIntentAfterRequestCancellation(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	previous, previousIdem := legacyPublicationSaga, internalIdem
	defer func() { legacyPublicationSaga = previous; internalIdem = previousIdem }()
	recorder := &actorRecordingPublisher{}
	legacyPublicationSaga = recorder
	internalIdem = nil
	cs := NewContentService(db)
	s := NewForkService(db, cs)
	runner := jobs.NewRunner(jobs.NewRepository(db, 10), 1)
	s.EnableDurable(runner, func(context.Context, jobs.Job) error { return nil })
	ctx := WithProvenance(context.Background(), Provenance{Actor: "human", Via: "ui"})
	ctx = WithAuthorIsAdmin(ctx, true)
	actor := primitive.NewObjectID()
	fork, err := s.Create(ctx, "Persistent publish choice", "", actor, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.CreateContent(ctx, &models.Content{Title: "Frozen publish", Slug: "frozen-publish", ForkID: &fork.ID}); err != nil {
		t.Fatal(err)
	}
	request, cancel := context.WithCancel(ctx)
	job, err := s.SubmitMerge(request, fork.ID, actor, "owner", true)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	fresh := NewForkService(db, cs)
	resumed := jobs.NewRunner(jobs.NewRepository(db, 10), 1)
	fresh.EnableDurable(resumed, func(context.Context, jobs.Job) error { return nil })
	if err := resumed.RunOnce(context.Background(), "fork_merge"); err != nil {
		t.Fatal(err)
	}
	result, err := fresh.DurableMergeResult(context.Background(), job.ID)
	if err != nil || len(result.CreatedIDs) != 1 {
		t.Fatalf("merge result: %+v %v", result, err)
	}
	if recorder.last.ContentID != result.CreatedIDs[0] || recorder.last.Actor != "human" || !recorder.last.AuthorIsAdmin {
		t.Fatalf("persisted publication intent or attribution lost: %+v", recorder.last)
	}
	if _, err := fresh.SubmitMerge(context.Background(), fork.ID, actor, "owner", false); err == nil {
		t.Fatal("retry silently changed frozen publication choice")
	}
}

type mergeRetryPublisher struct{ calls int }

func (p *mergeRetryPublisher) Publish(_ context.Context, req publication.PublishRequest) (publication.PublicationResult, error) {
	p.calls++
	if p.calls == 1 {
		return publication.PublicationResult{}, ErrPublishRetryLater
	}
	return publication.PublicationResult{ContentID: req.ContentID}, nil
}
func (p *mergeRetryPublisher) Unpublish(context.Context, publication.UnpublishRequest) error {
	return nil
}

func TestDurableMergePublicationRetryRetainsFrozenVersion(t *testing.T) {
	for _, edited := range []bool{false, true} {
		name := "retry"
		if edited {
			name = "later-draft"
		}
		t.Run(name, func(t *testing.T) {
			db, cleanup := testutil.MustConnectTestDB(t)
			defer cleanup()
			ctx := context.Background()
			old, oldIdem := legacyPublicationSaga, internalIdem
			defer func() { legacyPublicationSaga = old; internalIdem = oldIdem }()
			fake := &mergeRetryPublisher{}
			legacyPublicationSaga = fake
			internalIdem = nil
			cs := NewContentService(db)
			s := NewForkService(db, cs)
			runner := jobs.NewRunner(jobs.NewRepository(db, 10), 1)
			s.EnableDurable(runner, func(context.Context, jobs.Job) error { return nil })
			actor := primitive.NewObjectID()
			fork, err := s.Create(ctx, "Retry frozen", "", actor, "owner")
			if err != nil {
				t.Fatal(err)
			}
			if err := cs.CreateContent(ctx, &models.Content{Title: "v1", Slug: "retry-frozen", ForkID: &fork.ID}); err != nil {
				t.Fatal(err)
			}
			job, err := s.SubmitMerge(ctx, fork.ID, actor, "owner", true)
			if err != nil {
				t.Fatal(err)
			}
			if err := runner.RunOnce(ctx, "fork_merge"); err != nil {
				t.Fatal(err)
			}
			queued, err := runner.Repo.Get(ctx, job.ID)
			if err != nil || queued.State != "queued" {
				t.Fatalf("retry became terminal: %+v %v", queued, err)
			}
			items, err := runner.Repo.Items(ctx, job.ID, "", 100)
			if err != nil || len(items) != 1 || items[0].Done {
				t.Fatal("transient publication was marked complete", err)
			}
			if edited {
				var live models.Content
				if err := db.FindOne(ctx, "content", bson.M{"full_path": "/retry-frozen", "fork_id": nil}, &live); err != nil {
					t.Fatal(err)
				}
				live.Title = "v2"
				if err := cs.UpdateContent(ctx, &live); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.UpdateOne(ctx, "workflow_jobs", bson.M{"_id": job.ID}, bson.M{"$set": bson.M{"next_run_at": time.Now().Add(-time.Minute)}}); err != nil {
				t.Fatal(err)
			}
			if err := runner.RunOnce(ctx, "fork_merge"); err != nil {
				t.Fatal(err)
			}
			result, err := s.DurableMergeResult(ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if edited {
				if fake.calls != 1 || len(result.NotPublished) != 1 {
					t.Fatalf("retry published the later draft: calls=%d %+v", fake.calls, result)
				}
			} else if fake.calls != 2 || len(result.NotPublished) != 0 {
				t.Fatalf("same frozen publication was not retried: calls=%d %+v", fake.calls, result)
			}
		})
	}
}

func TestForkCleanupPreservesNewAndChangedCopies(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	cs := NewContentService(db)
	s := NewForkService(db, cs)
	fork, err := s.Create(ctx, "Retain edits", "", primitive.NewObjectID(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	changed := &models.Content{Title: "Original", Slug: "changed-copy", ForkID: &fork.ID}
	clean := &models.Content{Title: "Merged copy", Slug: "clean-copy", ForkID: &fork.ID}
	for _, page := range []*models.Content{changed, clean} {
		if err := cs.CreateContent(ctx, page); err != nil {
			t.Fatal(err)
		}
		stored, err := cs.GetContent(ctx, page.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.mergePageAtomic(ctx, fork.ID, *stored); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.UpdateOne(ctx, "content", bson.M{"_id": changed.ID}, bson.M{"$set": bson.M{"title": "Late raw edit"}}); err != nil {
		t.Fatal(err)
	}
	late := &models.Content{Title: "Added after submission", Slug: "late-copy", ForkID: &fork.ID}
	if err := cs.CreateContent(ctx, late); err != nil {
		t.Fatal(err)
	}
	var remains bool
	if err := db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		var err error
		remains, err = s.cleanupMergedCopies(sc, fork.ID, true)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !remains {
		t.Fatal("new edits not reported")
	}
	for _, id := range []primitive.ObjectID{changed.ID, late.ID} {
		n, _ := db.Count(ctx, "content", bson.M{"_id": id})
		if n != 1 {
			t.Fatalf("new edit %s deleted", id)
		}
	}
	if n, _ := db.Count(ctx, "content", bson.M{"_id": clean.ID}); n != 0 {
		t.Fatal("unchanged merged copy was not cleaned")
	}
}
