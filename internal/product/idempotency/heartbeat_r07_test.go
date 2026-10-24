package idempotency

// R07 regression: the worker heartbeat (spec §21.5) renews the lease while
// the operation runs and cancels the worker context on lease loss, so at
// most one effective executor mutates state.

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

func TestHeartbeatRenewsAndCancelsOnLoss(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()

	// Sub-second lease (in-package override; Options floor at 1 minute).
	svc.lease = 300 * time.Millisecond

	op, err := svc.Begin(ctx, "owner-hb", "POST", "/hb", "k-hb-1", []byte(`{"k":1}`))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	firstExpiry := op.LeaseExpiresAt

	hctx, stop := svc.Heartbeat(ctx, op.ID, op.Attempt, op.LeaseGeneration)
	defer stop()

	// Let several ticks fire: the expiry must advance past the original.
	time.Sleep(2200 * time.Millisecond)
	cur, err := svc.Get(ctx, op.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !cur.LeaseExpiresAt.After(firstExpiry) {
		t.Fatalf("heartbeat did not renew lease: first=%v cur=%v", firstExpiry, cur.LeaseExpiresAt)
	}
	if hctx.Err() != nil {
		t.Fatal("heartbeat context cancelled while lease held")
	}

	// Simulate a newer worker's takeover by bumping the generation
	// directly: the next tick must observe the loss and cancel.
	if _, err := db.Collection("idempotency_records").UpdateOne(ctx,
		bson.M{"_id": op.ID},
		bson.M{"$set": bson.M{
			"lease_generation":      op.LeaseGeneration + 1,
			"processing_expires_at": time.Now().Add(time.Minute),
		}}); err != nil {
		t.Fatalf("simulate takeover: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for hctx.Err() == nil && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if hctx.Err() == nil {
		t.Fatal("heartbeat context not cancelled after lease loss")
	}

	// stop is idempotent and terminates the goroutine (no hang on return).
	stop()
	stop()
}
