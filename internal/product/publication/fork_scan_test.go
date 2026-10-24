// Task 19 gap fix: the recovery scanner must ignore fork copies. Fork
// content shares its full_path with the live page by design (sparse
// copy-on-write); enumerating it as a live page makes reconcileNoActive
// quarantine the LIVE canonical out from under a serving page once migration
// is completed.
package publication_test

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestScannerForkCopyKeepsLiveOnline(t *testing.T) {
	f := newRecFixture(t, nil)
	ctx := context.Background()
	const path = "/t19fork/live-page"
	html := []byte("<html><body>live v1</body></html>")

	liveID := seedRecContent(t, f.db, path)
	recSeedActive(t, f, liveID, path, html, "verified")
	recWriteCanonical(t, f, path, html)
	setMigrationState(t, f.db, "completed")

	// Fork copy sharing the live path (agent sandbox / editor fork).
	forkID := primitive.NewObjectID()
	if _, err := f.db.Collection("content").InsertOne(ctx, bson.M{
		"_id": primitive.NewObjectID(), "template_id": primitive.NewObjectID(),
		"title": "Fork copy", "slug": "live-page", "folder_path": "/t19fork",
		"full_path": path, "fork_id": forkID, "published": false,
		"data": bson.M{"headline": "fork edit"},
	}); err != nil {
		t.Fatalf("seed fork copy: %v", err)
	}

	rep, err := f.sc.ScanOnce(ctx)
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if rep.OrphanQuarantined != 0 {
		t.Fatalf("OrphanQuarantined = %d, want 0 (fork copy must not orphan the live canonical)", rep.OrphanQuarantined)
	}
	if !recCanonicalExists(f, path) {
		t.Fatal("live canonical was quarantined while a fork copy exists")
	}
	if got := recReadCanonical(t, f, path); string(got) != string(html) {
		t.Fatalf("live bytes changed: %q", got)
	}
	if f.hasAlert("RECOVERY_ORPHAN_QUARANTINED") {
		t.Fatal("orphan-quarantined alert fired for a forked live page")
	}
}
