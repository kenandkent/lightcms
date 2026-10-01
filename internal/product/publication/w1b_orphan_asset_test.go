package publication_test

// Lane 1B fix 5b: the recovery orphan sweep must skip paths present in the
// assets collection — a *.html asset shares content/generated with page
// canonicals by design and must never read as an orphan canonical.

import (
	"context"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/product/publication"

	"go.mongodb.org/mongo-driver/bson"
)

func TestW1BOrphanSweep_SkipsAssetPaths(t *testing.T) {
	f := newRecFixture(t, nil)
	ctx := context.Background()
	setMigrationState(t, f.db, publication.MigrationCompleted)

	// Canonical file with NO owning content and NO asset row → quarantined
	// (control: sweep still works).
	recWriteCanonical(t, f, "/w1b-true-orphan", []byte("<html>orphan</html>"))

	// Canonical file with NO owning content but WITH an asset row → skipped.
	recWriteCanonical(t, f, "/w1b-asset-page", []byte("<html>asset</html>"))
	if _, err := f.db.Collection("assets").InsertOne(ctx, bson.M{
		"filename": "w1b-asset-page.html", "folder": "/",
		"full_path": "/w1b-asset-page.html", "serve_path": "/w1b-asset-page.html",
		"mime_type": "text/html", "size": int64(20),
		"created_at": time.Now(), "updated_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed asset: %v", err)
	}

	rpt, err := f.sc.ScanOnce(ctx)
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if rpt.OrphanQuarantined != 1 {
		t.Errorf("OrphanQuarantined = %d, want 1 (only the true orphan)", rpt.OrphanQuarantined)
	}
	if recCanonicalExists(f, "/w1b-asset-page") != true {
		t.Error("asset-owned canonical was quarantined — must be skipped")
	}
	if recCanonicalExists(f, "/w1b-true-orphan") {
		t.Error("true orphan was not quarantined — sweep regressed")
	}
}
