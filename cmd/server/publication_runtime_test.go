package main

// Lane 2B Fix 1 (red): a legacy DB with canonical collisions must boot
// degraded (report-and-continue with migration-required state), never
// log.Fatalf — and `migrate-publications --dry-run` must still report the
// blockers as the diagnostic path.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/product/migration"
	"github.com/jonradoff/lightcms/v7/internal/product/storage"
	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"go.mongodb.org/mongo-driver/bson"
)

func TestBootDegradedOnCanonicalCollision(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	defer resetMigrationRequired()
	ctx := context.Background()

	// Legacy state: no canonical unique index, two live rows sharing one
	// canonical key (/About vs /about).
	_, _ = db.Collection("content").Indexes().DropOne(ctx, "content_canonical_path_scope_unique")
	now := time.Now()
	for _, fp := range []string{"/About", "/about"} {
		doc := bson.M{
			"title":               "Legacy " + fp,
			"slug":                strings.TrimPrefix(fp, "/"),
			"full_path":           fp,
			"canonical_full_path": "/about",
			"path_scope":          "live",
			"path_active":         true,
			"current_version":     int64(1),
			"published":           false,
			"created_at":          now,
			"updated_at":          now,
		}
		if _, err := db.Collection("content").InsertOne(ctx, doc); err != nil {
			t.Fatalf("seed legacy row %s: %v", fp, err)
		}
	}

	// Sanity: the raw index build fails on the collision (today's boot crash).
	if err := db.EnsureProductIndexes(ctx); err == nil {
		t.Fatal("expected EnsureProductIndexes to fail on canonical collision")
	} else if !isIndexCollisionError(err) {
		t.Fatalf("expected a collision-class error, got: %v", err)
	}

	// Boot path: report-and-continue, never fatal.
	degraded, reason, fatalErr := ensureProductIndexesOrDegraded(ctx, db)
	if fatalErr != nil {
		t.Fatalf("collision-class failure must degrade, not fatal: %v", fatalErr)
	}
	if !degraded {
		t.Fatal("expected degraded=true on canonical collision (boot must not fatal)")
	}
	if !strings.Contains(reason, "migrate-publications --dry-run") {
		t.Fatalf("degraded reason must point at the dry-run diagnostic path, got: %q", reason)
	}

	// Migration-required state is latched for health reporting.
	setMigrationRequired(reason)
	if ok, got := migrationRequiredState(); !ok || got == "" {
		t.Fatalf("migration-required latch not set: ok=%v reason=%q", ok, got)
	}

	// Diagnostic path: dry-run still reports the collision (zero writes).
	migrator := migration.New(migration.Config{
		DB:    db,
		Store: storage.NewFilesystemStore(t.TempDir()),
	})
	rep, err := migrator.DryRun(ctx)
	if err != nil {
		t.Fatalf("dry-run must run on the colliding DB: %v", err)
	}
	if len(rep.CanonicalCollisions) == 0 {
		t.Fatal("dry-run must report the canonical collision")
	}
	found := false
	for _, c := range rep.CanonicalCollisions {
		if c.Canonical == "/about" && len(c.FullPaths) == 2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("dry-run collisions missing /about pair: %+v", rep.CanonicalCollisions)
	}
}

func TestBootNotDegradedWhenIndexesHealthy(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	defer resetMigrationRequired()
	ctx := context.Background()

	if err := db.EnsureProductIndexes(ctx); err != nil {
		t.Fatalf("seed healthy indexes: %v", err)
	}
	degraded, reason, fatalErr := ensureProductIndexesOrDegraded(ctx, db)
	if fatalErr != nil {
		t.Fatalf("healthy indexes must not error: %v", fatalErr)
	}
	if degraded {
		t.Fatalf("healthy DB must not boot degraded: %q", reason)
	}
	if ok, _ := migrationRequiredState(); ok {
		t.Fatal("migration-required latch must stay clear on a healthy DB")
	}
}

func TestIsIndexCollisionErrorClassifier(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"E11000 duplicate key error collection: lightcms.content index: content_canonical_path_scope_unique", true},
		{"IndexOptionsConflict: existing index has different options", true},
		{"IndexKeySpecsConflict: existing index ...", true},
		{"index already exists with a different name", true},
		{"connection refused", false},
		{"server selection timeout", false},
		{"", false},
	}
	for _, c := range cases {
		var err error
		if c.msg != "" {
			err = errors.New(c.msg)
		}
		if got := isIndexCollisionError(err); got != c.want {
			t.Errorf("isIndexCollisionError(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
	if isIndexCollisionError(nil) {
		t.Error("nil error is not a collision")
	}
}
