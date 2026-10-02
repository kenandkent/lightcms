package main

import (
	"context"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/testutil"
	"go.mongodb.org/mongo-driver/bson"
)

func TestFinalReviewHeartbeatMissingRowLosesGate(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	// Missing ownership must never be treated as a successful renewal.
	if err := refreshInstanceHeartbeat(context.Background(), db, "review_gate_missing", "missing-worker"); err == nil {
		t.Fatal("missing gate row renewed successfully")
	}
}

func TestFinalReviewGateLossStopsRenewal(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	const coll = "review_gate_loss"
	defer db.Collection(coll).Drop(ctx)
	old := instanceHeartbeatInterval
	instanceHeartbeatInterval = 20 * time.Millisecond
	defer func() { instanceHeartbeatInterval = old }()
	lost := make(chan error, 1)
	stop, err := enforceSingleInstanceOnCollection(ctx, db, coll, "review-worker", "test", func(err error) { lost <- err })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if _, err = db.Collection(coll).DeleteOne(ctx, bson.M{"_id": "review-worker"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-lost:
		if err == nil {
			t.Fatal("nil ownership loss")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gate loss only logged; runtime not stopped")
	}
}
