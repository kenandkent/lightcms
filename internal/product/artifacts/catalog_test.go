package artifacts

import (
	"bytes"
	"context"
	"github.com/metavert-io/lightcms/v7/internal/testutil"
	"go.mongodb.org/mongo-driver/bson"
	"testing"
	"time"
)

func TestCleanupRechecksNewReferencesAndPins(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	for _, name := range []string{"artifact_objects", "artifact_refs", "artifact_cleanup_plans"} {
		defer db.Collection(name).Drop(ctx)
	}
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	catalog := NewCatalog(db, store, 90, 7)
	if err := catalog.EnsureIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	if err := catalog.EnsureIndexes(ctx); err != nil {
		t.Fatal("repeated index initialization", err)
	}
	ref, err := store.Put(ctx, bytes.NewBufferString("retain"), "text/plain", 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Register(ctx, ref, false); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Collection("artifact_objects").UpdateOne(ctx, bson.M{"_id": ref.Hash}, bson.M{"$set": bson.M{"created_at": time.Now().Add(-100 * 24 * time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	plan, err := catalog.PreviewCleanup(ctx, "admin")
	if err != nil || len(plan.Items) != 1 {
		t.Fatalf("missing cleanup candidate: %+v %v", plan, err)
	}
	if err := catalog.Reference(ctx, "job:active", ref, nil); err != nil {
		t.Fatal(err)
	}
	if err := catalog.ConfirmCleanup(ctx, "admin", plan.Digest, true); err == nil {
		t.Fatal("cleanup deleted newly referenced object")
	}
	reader, err := store.Open(ctx, ref)
	if err != nil {
		t.Fatal("referenced object lost", err)
	}
	reader.Close()
	if err := catalog.ReleaseOwner(ctx, "job:active"); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Pin(ctx, ref.Hash, true); err != nil {
		t.Fatal(err)
	}
	plan, err = catalog.PreviewCleanup(ctx, "admin")
	if err != nil || len(plan.Items) != 0 {
		t.Fatalf("pinned object eligible: %+v %v", plan, err)
	}
	if err := catalog.Pin(ctx, ref.Hash, false); err != nil {
		t.Fatal(err)
	}
	plan, err = catalog.PreviewCleanup(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.ConfirmCleanup(ctx, "other-user", plan.Digest, true); err == nil {
		t.Fatal("another owner confirmed plan")
	}
	if err := catalog.ConfirmCleanup(ctx, "admin", plan.Digest, false); err == nil {
		t.Fatal("missing confirmation accepted")
	}
	if err := catalog.ConfirmCleanup(ctx, "admin", plan.Digest, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(ctx, ref); err == nil {
		t.Fatal("unreferenced object not removed")
	}
	if err := catalog.Reference(ctx, "late", ref, nil); err == nil {
		t.Fatal("tombstoned object acquired reference")
	}
}

func TestCleanupResumesOnlyAlreadyFencedObjectsAfterExpiry(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	for _, name := range []string{"artifact_objects", "artifact_refs", "artifact_cleanup_plans"} {
		defer db.Collection(name).Drop(ctx)
	}
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	catalog := NewCatalog(db, store, 90, 7)
	refs := []Ref{}
	for _, text := range []string{"interrupted", "not-started"} {
		ref, err := store.Put(ctx, bytes.NewBufferString(text), "text/plain", 100)
		if err != nil {
			t.Fatal(err)
		}
		if err = catalog.Register(ctx, ref, false); err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ref)
	}
	if _, err = db.Collection("artifact_objects").UpdateMany(ctx, bson.M{}, bson.M{"$set": bson.M{"created_at": time.Now().Add(-100 * 24 * time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	plan, err := catalog.PreviewCleanup(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Collection("artifact_objects").UpdateOne(ctx, bson.M{"_id": refs[0].Hash}, bson.M{"$set": bson.M{"state": "deleting", "delete_plan": plan.Digest}}); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Collection("artifact_cleanup_plans").UpdateOne(ctx, bson.M{"_id": plan.Digest}, bson.M{"$set": bson.M{"expires_at": time.Now().Add(-time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	if err = catalog.ResumeCleanup(ctx, "other", plan.Digest, true); err == nil {
		t.Fatal("other owner resumed cleanup")
	}
	if err = catalog.ResumeCleanup(ctx, "admin", plan.Digest, true); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Open(ctx, refs[0]); err == nil {
		t.Fatal("fenced deletion not recovered")
	}
	reader, err := store.Open(ctx, refs[1])
	if err != nil {
		t.Fatal("expired plan deleted a new candidate", err)
	}
	reader.Close()
}
