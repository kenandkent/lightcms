package services

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/metavert-io/lightcms/v7/internal/models"
	"github.com/metavert-io/lightcms/v7/internal/testutil"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestForkMergeFailedItemResumes(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	cs := NewContentService(db)
	s := NewForkService(db, cs)
	fork, err := s.Create(ctx, "Resume", "", primitive.NewObjectID(), "owner", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"one", "two"} {
		if err := cs.CreateContent(ctx, &models.Content{Title: slug, Slug: slug, ForkID: &fork.ID}); err != nil {
			t.Fatal(err)
		}
	}
	writes := 0
	db.SetFaultHook(func(op, collection string) error {
		if op == "InsertOne" && collection == "content" {
			writes++
			if writes == 2 {
				return errors.New("second page unavailable")
			}
		}
		return nil
	})
	result, err := s.Merge(ctx, fork.ID, primitive.NewObjectID(), "owner")
	db.SetFaultHook(nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != 1 || len(result.Failed) != 1 {
		t.Fatalf("unexpected partial result: %+v", result)
	}
	state, err := s.GetByID(ctx, fork.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status == "merged" {
		t.Fatal("failed merge marked complete")
	}
	if _, err := s.Merge(ctx, fork.ID, primitive.NewObjectID(), "owner"); err != nil {
		t.Fatal(err)
	}
	var pages []models.Content
	cursor, err := db.FindMany(ctx, "content", bson.M{"fork_id": bson.M{"$exists": false}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := cursor.All(ctx, &pages); err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 {
		t.Fatalf("expected two drafts, got %d", len(pages))
	}
	for _, page := range pages {
		if page.CurrentVersion != 1 || page.Published {
			t.Fatalf("completed item was reapplied or published: %+v", page)
		}
	}
}

func TestForkMergeConcurrentCheckpoint(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	cs := NewContentService(db)
	s := NewForkService(db, cs)
	fork, err := s.Create(ctx, "Concurrent merge", "", primitive.NewObjectID(), "owner", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.CreateContent(ctx, &models.Content{Title: "Once", Slug: "merge-once", ForkID: &fork.ID}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; _, _ = s.Merge(ctx, fork.ID, primitive.NewObjectID(), "owner") }()
	}
	close(start)
	wg.Wait()
	var page models.Content
	if err := db.FindOne(ctx, "content", bson.M{"full_path": "/merge-once", "fork_id": bson.M{"$exists": false}}, &page); err != nil {
		t.Fatal(err)
	}
	if page.CurrentVersion != 1 {
		t.Fatalf("page applied repeatedly: version %d", page.CurrentVersion)
	}
	state, err := s.GetByID(ctx, fork.ID)
	if err != nil || state.Status != "merged" {
		t.Fatalf("merge did not finish: %+v %v", state, err)
	}
}
