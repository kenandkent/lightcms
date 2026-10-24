package idempotency

import (
	"bytes"
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestFinalReviewDurableRenderSnapshot(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	op, err := svc.Begin(ctx, "render-key", "POST", "/render", "snapshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	pub, template := primitive.NewObjectID(), primitive.NewObjectID()
	ctx = WithLeaseGeneration(ctx, op.LeaseGeneration)
	if err = svc.FreezeExecution(ctx, op.ID, op.Attempt, pub, time.Now().UTC(), template); err != nil {
		t.Fatal(err)
	}
	op, err = svc.Get(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FreezeRenderSnapshot(ctx, db, op, make([]byte, (8<<20)+1)); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("oversize snapshot: %v", err)
	}
	payload := []byte(`{"literal":"Original"}`)
	first, err := FreezeRenderSnapshot(ctx, db, op, payload)
	if err != nil || !bytes.Equal(first, payload) {
		t.Fatalf("freeze: %s %v", first, err)
	}
	again, err := FreezeRenderSnapshot(ctx, db, op, []byte(`{"literal":"Changed"}`))
	if err != nil || !bytes.Equal(again, payload) {
		t.Fatalf("snapshot overwritten: %s %v", again, err)
	}
	loaded, err := RenderSnapshotForPublication(ctx, db, pub)
	if err != nil || loaded == nil || !bytes.Equal(loaded.RenderSnapshot, payload) {
		t.Fatalf("durable snapshot missing: %+v %v", loaded, err)
	}
	if _, err := RenderSnapshotForPublication(context.Background(), db, pub); CodeOf(err) != CodeLeaseLost {
		t.Fatalf("unowned snapshot read: %v", err)
	}
	if got, err := RenderSnapshotForPublication(ctx, db, primitive.NewObjectID()); err != nil || got != nil {
		t.Fatalf("unknown publication: %v %v", got, err)
	}
	_, err = db.Collection(CollectionName).UpdateOne(ctx, bson.M{"_id": op.ID}, bson.M{"$set": bson.M{"processing_expires_at": time.Now().Add(-time.Minute)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RenewLease(ctx, op.ID, op.Attempt, op.LeaseGeneration); err == nil {
		t.Fatal("expired worker resurrected its lease")
	}
	if _, err := RenderSnapshotForPublication(ctx, db, pub); CodeOf(err) != CodeLeaseLost {
		t.Fatalf("expired snapshot read: %v", err)
	}
}
