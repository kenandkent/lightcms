package staticdelivery

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/metavert-io/lightcms/v7/internal/database"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func connectDeliveryTestDB(t *testing.T) (*Repository, context.Context) {
	t.Helper()
	uri := os.Getenv("STATIC_DELIVERY_TEST_MONGODB_URI")
	if uri == "" {
		if os.Getenv("STATIC_DELIVERY_REQUIRE_MONGO") == "1" {
			t.Fatal("STATIC_DELIVERY_TEST_MONGODB_URI is required for integration gate")
		}
		t.Skip("delivery integration not run: isolated test Mongo URI is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	name := "static-delivery-test-" + primitive.NewObjectID().Hex()
	db, err := database.Connect(ctx, uri, name, options.Client().SetServerSelectionTimeout(5*time.Second))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Collection("_cleanup").Database().Drop(context.Background())
		_ = db.Disconnect(context.Background())
		cancel()
	})
	r := NewRepository(db)
	if err := r.EnsureIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	return r, ctx
}

func TestOperationCanonicalRequestHash(t *testing.T) {
	a := OperationRequest{Owner: "user:1", Action: "export", Key: "key", Body: json.RawMessage(`{"items":[{"content_id":"000000000000000000000001"}],"x":1}`)}
	b := a
	b.Body = json.RawMessage(`{"x":1,"items":[{"content_id":"000000000000000000000001"}]}`)
	ha, err := OperationRequestHash(a)
	if err != nil {
		t.Fatal(err)
	}
	hb, err := OperationRequestHash(b)
	if err != nil || ha != hb {
		t.Fatal("JSON member order changed request identity")
	}
	b.Body = json.RawMessage(`{"x":2,"items":[{"content_id":"000000000000000000000001"}]}`)
	hb, _ = OperationRequestHash(b)
	if ha == hb {
		t.Fatal("different request accepted as replay")
	}
	a.Body = json.RawMessage(`{"x":9007199254740993}`)
	b.Body = json.RawMessage(`{"x":9007199254740992}`)
	ha, _ = OperationRequestHash(a)
	hb, _ = OperationRequestHash(b)
	if ha == hb {
		t.Fatal("large integer rounded during canonicalization")
	}
}

func TestRepositoryReserveOperationAcrossConcurrentRequests(t *testing.T) {
	r, ctx := connectDeliveryTestDB(t)
	req := OperationRequest{Owner: "user:1", Action: "export", Key: "same", Body: json.RawMessage(`{"items":[1]}`)}
	var wg sync.WaitGroup
	ids := make(chan primitive.ObjectID, 50)
	errs := make(chan error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			op, _, err := r.ReserveOperation(ctx, req)
			if err != nil {
				errs <- err
				return
			}
			ids <- op.ID
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	var first primitive.ObjectID
	for id := range ids {
		if first.IsZero() {
			first = id
		}
		if id != first {
			t.Fatal("duplicate logical operations")
		}
	}
	op, created, err := r.ReserveOperation(ctx, req)
	if err != nil || created || op.JobID.IsZero() || op.ExportID.IsZero() {
		t.Fatalf("replay: %+v %v %v", op, created, err)
	}
	req.Body = json.RawMessage(`{"items":[2]}`)
	_, _, err = r.ReserveOperation(ctx, req)
	if CodeOf(err) != CodeIdempotencyConflict {
		t.Fatalf("different request: %v", err)
	}
}

func TestRepositoryTargetClaimAndCommitAreConditional(t *testing.T) {
	r, ctx := connectDeliveryTestDB(t)
	targetID, jobID, otherJob, releaseID := primitive.NewObjectID(), primitive.NewObjectID(), primitive.NewObjectID(), primitive.NewObjectID()
	_, err := r.collection("targets").InsertOne(ctx, Target{Record: Record{ID: targetID}, CurrentRevision: 7, ConfigRevision: 2, Status: "ready"})
	if err != nil {
		t.Fatal(err)
	}
	if err = r.ClaimTarget(ctx, targetID, jobID, 7); err != nil {
		t.Fatal(err)
	}
	if err = r.ClaimTarget(ctx, targetID, otherJob, 7); CodeOf(err) != CodeTargetBusy {
		t.Fatalf("claim %v", err)
	}
	if err = r.CommitTarget(ctx, targetID, otherJob, releaseID, 7); CodeOf(err) != CodeRevisionConflict {
		t.Fatalf("wrong owner committed: %v", err)
	}
	if err = r.CommitTarget(ctx, targetID, jobID, releaseID, 6); CodeOf(err) != CodeRevisionConflict {
		t.Fatalf("stale revision committed: %v", err)
	}
	if err = r.CommitTarget(ctx, targetID, jobID, releaseID, 7); err != nil {
		t.Fatal(err)
	}
	var target Target
	if err = r.collection("targets").FindOne(ctx, bson.M{"_id": targetID}).Decode(&target); err != nil {
		t.Fatal(err)
	}
	if target.CurrentRevision != 8 || target.CurrentReleaseID != releaseID || !target.ActiveJobID.IsZero() {
		t.Fatalf("partial commit: %+v", target)
	}
	if err = r.CommitTarget(ctx, targetID, jobID, releaseID, 7); err != nil {
		t.Fatalf("commit recovery not idempotent: %v", err)
	}
}

func TestRepositoryFreezeSnapshotOnlyOnce(t *testing.T) {
	r, ctx := connectDeliveryTestDB(t)
	op, _, err := r.ReserveOperation(ctx, OperationRequest{Owner: "user:1", Action: "export", Key: "freeze", Body: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	first, err := r.FreezeOperation(ctx, op.ID, []byte(`{"publication_id":"first"}`))
	if err != nil {
		t.Fatal(err)
	}
	replay, err := r.FreezeOperation(ctx, op.ID, []byte(`{"publication_id":"newer"}`))
	if err != nil || string(first.Snapshot) != string(replay.Snapshot) {
		t.Fatalf("snapshot replaced: %+v %v", replay, err)
	}
}
