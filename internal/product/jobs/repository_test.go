package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/metavert-io/lightcms/v7/internal/testutil"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

func TestWorkflowListingUsesCreationTimeAndAppendIsFenced(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	r := NewRepository(db, 10)
	for _, name := range []string{"workflow_jobs", "workflow_items", "workflow_capacity", "workflow_events"} {
		defer db.Collection(name).Drop(ctx)
	}
	older, err := r.Submit(ctx, Submission{Kind: "order", Key: "older", Actor: "editor"})
	if err != nil {
		t.Fatal(err)
	}
	newer, err := r.Submit(ctx, Submission{Kind: "order", Key: "newer", Actor: "editor"})
	if err != nil {
		t.Fatal(err)
	}
	// IDs are hashes, not chronological Mongo IDs.
	_, err = db.Collection("workflow_jobs").UpdateOne(ctx, bson.M{"_id": older.ID}, bson.M{"$set": bson.M{"created_at": time.Now().Add(-time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := r.List(ctx, "order", primitive.NilObjectID, 1)
	if err != nil || len(rows) != 1 || rows[0].ID != newer.ID {
		t.Fatalf("not newest first: %+v %v", rows, err)
	}
	rows, err = r.List(ctx, "order", newer.ID, 1)
	if err != nil || len(rows) != 1 || rows[0].ID != older.ID {
		t.Fatalf("cursor skipped older row: %+v %v", rows, err)
	}
	claimed, err := r.Claim(ctx, "order", "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Cancel(ctx, claimed.ID); err != nil {
		t.Fatal(err)
	}
	err = db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		return r.AppendItems(sc, *claimed, []Input{{Key: "late", Data: bson.M{"value": 1}}})
	})
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale append accepted: %v", err)
	}
	items, err := r.Items(ctx, claimed.ID, "", 100)
	if err != nil || len(items) != 0 {
		t.Fatalf("stale append persisted: %+v %v", items, err)
	}
}

func TestDurableLeaseTakeoverFencesOldWriter(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	r := NewRepository(db, 10)
	defer db.Collection("workflow_jobs").Drop(ctx)
	defer db.Collection("workflow_items").Drop(ctx)
	defer db.Collection("workflow_capacity").Drop(ctx)
	j, err := r.Submit(ctx, Submission{Kind: "test", Key: "one", Actor: "editor", Items: []Input{{Key: "page", Data: bson.M{"title": "frozen"}}}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := r.Claim(ctx, "test", "first", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Collection("workflow_jobs").UpdateOne(ctx, bson.M{"_id": j.ID}, bson.M{"$set": bson.M{"lease_until": time.Now().Add(-time.Minute)}}); err != nil {
		t.Fatal(err)
	}
	second, err := r.Claim(ctx, "test", "second", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if second.Fence <= first.Fence {
		t.Fatal("takeover did not advance fencing token")
	}
	wrote := false
	if err := r.CommitItem(ctx, *first, "page", func(sc mongo.SessionContext) (bson.M, error) { wrote = true; return bson.M{}, nil }); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale writer accepted: %v", err)
	}
	if wrote {
		t.Fatal("stale writer executed mutation")
	}
	if err := r.CommitItem(ctx, *second, "page", func(sc mongo.SessionContext) (bson.M, error) {
		_, err := db.InsertOne(sc, "workflow_test_output", bson.M{"title": "committed"})
		return bson.M{"saved": true}, err
	}); err != nil {
		t.Fatal(err)
	}
	defer db.Collection("workflow_test_output").Drop(ctx)
	if err := r.CommitItem(ctx, *second, "page", func(mongo.SessionContext) (bson.M, error) { t.Error("completed item ran again"); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx, *second, "succeeded", ""); err != nil {
		t.Fatal(err)
	}
	stored, err := r.Get(ctx, j.ID)
	if err != nil || stored.Completed != 1 || stored.State != "succeeded" {
		t.Fatalf("wrong progress: %+v %v", stored, err)
	}
}

func TestDurableCapacityIdempotencyAndCancellation(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	r := NewRepository(db, 1)
	for _, name := range []string{"workflow_jobs", "workflow_items", "workflow_capacity", "workflow_events"} {
		defer db.Collection(name).Drop(ctx)
	}
	request := Submission{Kind: "test", Key: "one", Actor: "editor", Items: []Input{{Key: "page", Data: bson.M{"v": 1}}}}
	j, err := r.Submit(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := r.Submit(ctx, request)
	if err != nil || replay.ID != j.ID {
		t.Fatalf("idempotency failed: %v", err)
	}
	request.Key = "two"
	if _, err := r.Submit(ctx, request); !errors.Is(err, ErrCapacity) {
		t.Fatalf("queue unbounded: %v", err)
	}
	if err := r.Cancel(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Claim(ctx, "test", "worker", time.Minute); !errors.Is(err, mongo.ErrNoDocuments) {
		t.Fatalf("claimed cancelled job: %v", err)
	}
	if _, err := r.Submit(ctx, request); err != nil {
		t.Fatalf("cancelled job leaked capacity: %v", err)
	}
	request.Items[0].Data = bson.M{"v": 2}
	if _, err := r.Submit(ctx, request); !errors.Is(err, ErrConflict) {
		t.Fatalf("idempotency changed input: %v", err)
	}
}
