package idempotency

// R07 fencing regression: idempotency mutations CAS on the stashed lease
// generation, so a worker that lost its lease to a takeover cannot write
// onto the new owner's attempt (Complete/Bind/Freeze/MarkTerminal all
// reject with IDEMPOTENCY_LEASE_LOST; the fresh generation proceeds).

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestFencedMutationsRejectStaleGeneration(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()

	op, err := svc.Begin(ctx, "owner-fence", "POST", "/fence", "k-fence-1", []byte(`{"k":1}`))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	staleCtx := WithLeaseGeneration(ctx, op.LeaseGeneration)

	// Expire + take over: generation bumps.
	if _, err := db.Collection("idempotency_records").UpdateOne(ctx,
		bson.M{"_id": op.ID},
		bson.M{"$set": bson.M{"processing_expires_at": time.Now().Add(-time.Minute)}}); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	top, err := svc.TakeOver(ctx, op.ID, op.LeaseGeneration)
	if err != nil {
		t.Fatalf("TakeOver: %v", err)
	}
	if top.LeaseGeneration != op.LeaseGeneration+1 {
		t.Fatalf("takeover generation = %d, want %d", top.LeaseGeneration, op.LeaseGeneration+1)
	}

	cid := primitive.NewObjectID()
	if _, err := svc.Complete(staleCtx, op.ID, op.Attempt, 200, map[string]any{"ok": true}, false); CodeOf(err) != CodeLeaseLost {
		t.Fatalf("stale Complete = %v, want IDEMPOTENCY_LEASE_LOST", err)
	}
	if err := svc.BindContentAndVersion(staleCtx, nil, op.ID, cid, 1, "/fence"); CodeOf(err) != CodeLeaseLost {
		t.Fatalf("stale Bind = %v, want IDEMPOTENCY_LEASE_LOST", err)
	}
	if err := svc.FreezeExecution(staleCtx, op.ID, op.Attempt, primitive.NewObjectID(), time.Now(), primitive.NewObjectID()); CodeOf(err) != CodeLeaseLost {
		t.Fatalf("stale Freeze = %v, want IDEMPOTENCY_LEASE_LOST", err)
	}
	if _, err := svc.MarkTerminal(staleCtx, op.ID, op.Attempt, "X"); CodeOf(err) != CodeLeaseLost {
		t.Fatalf("stale MarkTerminal = %v, want IDEMPOTENCY_LEASE_LOST", err)
	}

	// Fresh generation proceeds normally.
	freshCtx := WithLeaseGeneration(ctx, top.LeaseGeneration)
	if _, err := svc.Complete(freshCtx, op.ID, top.Attempt, 200, map[string]any{"ok": true}, false); err != nil {
		t.Fatalf("fresh Complete: %v", err)
	}

	// Unstashed callers keep the legacy unchecked behavior (backward
	// compatible): a fresh op completes without fencing.
	op2, err := svc.Begin(ctx, "owner-fence", "POST", "/fence", "k-fence-2", []byte(`{"k":2}`))
	if err != nil {
		t.Fatalf("Begin 2: %v", err)
	}
	if _, err := svc.Complete(ctx, op2.ID, op2.Attempt, 200, map[string]any{"ok": true}, false); err != nil {
		t.Fatalf("unstashed Complete: %v", err)
	}
}
