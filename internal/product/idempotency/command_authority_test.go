package idempotency

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestFinalReviewCommandAuthorityAndReadOnlyLookup(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	body := []byte(`{"expected_active_id":null}`)
	if op, err := svc.Lookup(ctx, "key-a", "POST", "/command", "stable", body); err != nil || op != nil {
		t.Fatalf("lookup created an operation: %+v %v", op, err)
	}
	op, err := svc.Begin(ctx, "key-a", "POST", "/command", "stable", body)
	if err != nil {
		t.Fatal(err)
	}
	ctx = WithLeaseGeneration(ctx, op.LeaseGeneration)
	if err = svc.SetCommandKind(ctx, op, "created"); err != nil {
		t.Fatal(err)
	}
	if err = svc.SetCommandKind(ctx, op, "created"); err != nil {
		t.Fatal(err)
	}
	if err = svc.SetCommandKind(ctx, op, "updated"); CodeOf(err) != CodeLeaseLost {
		t.Fatalf("changed original command semantics: %v", err)
	}
	if err = svc.SetResponseMetadata(ctx, op, map[string]any{"warnings": []any{map[string]any{"code": "FIELD_DEFAULT_APPLIED", "field": "body", "message": "default"}}}); err != nil {
		t.Fatal(err)
	}
	read, err := svc.Lookup(ctx, "key-a", "POST", "/command", "stable", body)
	if err != nil || read == nil || read.CommandKind != "created" || read.Replay || read.ResponseMetadata["warnings"] == nil {
		t.Fatalf("durable command metadata: %+v %v", read, err)
	}
	if _, err = svc.Lookup(ctx, "key-a", "POST", "/command", "stable", []byte(`{"expected_active_id":"changed"}`)); CodeOf(err) != CodeConflict {
		t.Fatalf("changed request did not conflict: %v", err)
	}
	if read, err = svc.Lookup(ctx, "key-b", "POST", "/command", "stable", body); err != nil || read != nil {
		t.Fatalf("credential lookup crossed namespaces: %+v %v", read, err)
	}
	if err = svc.AssertOwned(ctx, op.ID, op.Attempt, op.LeaseGeneration); err != nil {
		t.Fatal(err)
	}
	if err = svc.AssertOwned(ctx, op.ID, op.Attempt, op.LeaseGeneration+1); CodeOf(err) != CodeLeaseLost {
		t.Fatalf("newer generation adopted: %v", err)
	}
	if err = GuardExecution(ctx, db); err != nil {
		t.Fatal(err)
	} // Unkeyed callers have no execution guard.
	guarded := WithExecutionLease(ctx, op.ID, op.Attempt, op.LeaseGeneration)
	if err = GuardExecution(guarded, db); err != nil {
		t.Fatal(err)
	}
	if err = GuardExecution(WithExecutionLease(ctx, op.ID, op.Attempt, op.LeaseGeneration+1), db); CodeOf(err) != CodeLeaseLost {
		t.Fatalf("stale transaction guard passed: %v", err)
	}
	if _, err = svc.Complete(ctx, op.ID, op.Attempt, 201, map[string]any{"action": "created"}, false); err != nil {
		t.Fatal(err)
	}
	read, err = svc.Lookup(ctx, "key-a", "POST", "/command", "stable", body)
	if err != nil || read == nil || !read.Replay || read.StatusCode != 201 {
		t.Fatalf("completed lookup: %+v %v", read, err)
	}
	if err = svc.SetResponseMetadata(ctx, op, map[string]any{}); CodeOf(err) != CodeLeaseLost {
		t.Fatalf("completed metadata changed: %v", err)
	}
	if err = svc.AssertOwned(ctx, primitive.NewObjectID(), 1, 1); CodeOf(err) != CodeNotFound {
		t.Fatalf("missing ownership: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err = svc.AssertOwned(cancelled, op.ID, op.Attempt, op.LeaseGeneration); err == nil {
		t.Fatal("cancelled worker remained owned")
	}
}
