package publication_test

// R08/R11 regression: rename redirects commit atomically with activation,
// and indeterminate commits never trigger destructive compensation.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"
	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// TestActivateCASWithRedirectAtomic: the rename redirect lands in the SAME
// transaction as the active-pointer flip (spec §18.3) — and empty paths
// write no redirect.
func TestActivateCASWithRedirectAtomic(t *testing.T) {
	db, _ := testutil.MustConnectTestDB(t)
	ctx := context.Background()
	if err := db.EnsureProductIndexes(ctx); err != nil {
		t.Fatalf("EnsureProductIndexes: %v", err)
	}
	repo := publication.NewRepository(db, nil)
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, col := range []string{"content", "content_publications", "webhook_outbox", "redirects"} {
			db.Collection(col).Drop(cctx) //nolint:errcheck
		}
	})

	cid := primitive.NewObjectID()
	if _, err := db.Collection("content").InsertOne(ctx, bson.M{
		"_id": cid, "title": "R", "slug": "r", "full_path": "/new",
		"canonical_full_path": "/new", "path_scope": "live", "path_active": true,
		"current_version": int64(2), "published": true,
		"created_at": time.Now(), "updated_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed content: %v", err)
	}
	mkStaged := func() primitive.ObjectID {
		t.Helper()
		id := primitive.NewObjectID()
		if _, err := db.Collection("content_publications").InsertOne(ctx, bson.M{
			"_id": id, "content_id": cid, "content_version": int64(2),
			"full_path": "/new", "content_hash": "sha256:" + id.Hex(),
			"status": "staged", "verification_status": "verified", "storage_state": "present",
			"logical_published_at": time.Now(),
		}); err != nil {
			t.Fatalf("seed staged: %v", err)
		}
		return id
	}

	// Plain activation (no rename): no redirect row.
	p1 := mkStaged()
	if err := repo.ActivateCAS(ctx, cid, p1, nil); err != nil {
		t.Fatalf("ActivateCAS: %v", err)
	}
	if n, _ := db.Collection("redirects").CountDocuments(ctx, bson.M{}); n != 0 {
		t.Fatalf("non-rename activation wrote %d redirect(s)", n)
	}

	// Rename activation: flip + redirect atomically.
	p2 := mkStaged()
	active, err := repo.GetActive(ctx, cid)
	if err != nil || active == nil {
		t.Fatalf("GetActive: %v %v", active, err)
	}
	if err := repo.ActivateCASWithRedirect(ctx, cid, p2, &active.ID, "/old", "/new"); err != nil {
		t.Fatalf("ActivateCASWithRedirect: %v", err)
	}
	if active, err := repo.GetActive(ctx, cid); err != nil || active == nil || active.ID != p2 {
		t.Fatalf("active = %+v, err = %v (want P2)", active, err)
	}
	var redir bson.M
	if err := db.FindOne(ctx, "redirects", bson.M{"from_path": "/old"}, &redir); err != nil {
		t.Fatalf("redirect row missing: %v", err)
	}
	if redir["to_path"] != "/new" {
		t.Fatalf("redirect to_path = %v, want /new", redir["to_path"])
	}
}

// TestActivationUnknownNoCompensation (R11): commit error + read-back error
// ⇒ ACTIVATION_UNKNOWN with zero compensation — staged record kept staged,
// new canonical bytes kept, previous backup kept — for scanner/retry.
func TestActivationUnknownNoCompensation(t *testing.T) {
	s := newSagaSetup(t, nil, nil, publication.Options{})
	ctx := context.Background()
	tplID, tvID := seedSagaTemplateVersion(t, s.db, "", true)
	cid := seedSagaContent(t, s.db, tplID, "/news/r11-page", 1, map[string]any{"headline": "v1"})

	// v1 live.
	if _, err := s.svc.Publish(ctx, publication.PublishRequest{ContentID: cid, TemplateVersionID: tvID}); err != nil {
		t.Fatalf("publish v1: %v", err)
	}
	oldCanon, err := os.ReadFile(filepath.Join(s.root, "generated", "news", "r11-page.html"))
	if err != nil || !strings.Contains(string(oldCanon), "v1") {
		t.Fatalf("v1 canonical: %v %q", err, oldCanon)
	}

	// v2 with a commit-time conflict (a concurrent activation supersedes P1
	// inside BeforeCommit, after cutover but before the txn) + broken
	// read-back (fault) ⇒ unknown, not compensated.
	p1, err := s.repo.GetActive(ctx, cid)
	if err != nil || p1 == nil {
		t.Fatalf("P1 active: %+v %v", p1, err)
	}
	faulty := publication.NewService(s.db, s.repo, s.store, publication.Options{
		Templates: templatecontract.NewService(s.db),
		Faults: publication.Faults{
			CommitReadError: errors.New("read-back down"),
			BeforeCommit: func(bctx context.Context) error {
				// Simulate a concurrent activation landing first.
				_, _ = s.db.Collection("content_publications").UpdateOne(bctx,
					bson.M{"_id": p1.ID}, bson.M{"$set": bson.M{"status": "superseded"}})
				return nil
			},
		},
	})
	if _, err := s.db.Collection("content").UpdateOne(ctx, bson.M{"_id": cid}, bson.M{"$set": bson.M{
		"current_version": int64(2), "title": "v2 title",
		"data":       bson.M{"headline": "v2"},
		"updated_at": time.Now(),
	}}); err != nil {
		t.Fatalf("bump to v2: %v", err)
	}
	_, err = faulty.Publish(ctx, publication.PublishRequest{ContentID: cid, TemplateVersionID: tvID, Reason: "r11"})
	if err == nil || publication.CodeOf(err) != publication.CodeActivationUnknown {
		t.Fatalf("want ACTIVATION_UNKNOWN, got %v", err)
	}

	// Staged P2 retained (not failed)...
	var staged bson.M
	if err := s.db.FindOne(ctx, "content_publications",
		bson.M{"content_id": cid, "status": "staged"}, &staged); err != nil {
		t.Fatalf("staged P2 must be retained (not failed): %v", err)
	}
	// ... new canonical bytes kept (no compensation) ...
	now, err := os.ReadFile(filepath.Join(s.root, "generated", "news", "r11-page.html"))
	if err != nil || !strings.Contains(string(now), "v2") {
		t.Fatalf("canonical must keep v2 bytes (no compensation): %v %q", err, now)
	}
	// ... previous backup kept ...
	matches, _ := filepath.Glob(filepath.Join(s.root, "generated", "news", "r11-page.html.previous-*"))
	if len(matches) == 0 {
		t.Fatal("previous backup must be retained for recovery")
	}
	// ... and the old publication is NOT replaced by P2 (in this
	// simulation the hook superseded P1, so no row is active; in production
	// the row is either the old or the new one — never assumed, which is
	// exactly why compensation is skipped).
	if active, err := s.repo.GetActive(ctx, cid); err != nil || active != nil {
		t.Fatalf("P2 must not be active after unknown: %+v %v", active, err)
	}
}

// TestRenamePublishWritesRedirectEndToEnd (R08 wiring): a saga rename publish
// leaves the redirect row, retires the old canonical, and serves the new one.
func TestRenamePublishWritesRedirectEndToEnd(t *testing.T) {
	s := newSagaSetup(t, nil, nil, publication.Options{})
	ctx := context.Background()
	tplID, tvID := seedSagaTemplateVersion(t, s.db, "", true)
	cid := seedSagaContent(t, s.db, tplID, "/news/r08-old", 1, map[string]any{"headline": "v1"})

	if _, err := s.svc.Publish(ctx, publication.PublishRequest{ContentID: cid, TemplateVersionID: tvID}); err != nil {
		t.Fatalf("publish v1: %v", err)
	}
	// Rename: move the live row to the new path with new bytes.
	if _, err := s.db.Collection("content").UpdateOne(ctx, bson.M{"_id": cid}, bson.M{"$set": bson.M{
		"full_path": "/news/r08-new", "canonical_full_path": "/news/r08-new",
		"current_version": int64(2), "data": bson.M{"headline": "v2"},
		"updated_at": time.Now(),
	}}); err != nil {
		t.Fatalf("rename row: %v", err)
	}
	if _, err := s.svc.Publish(ctx, publication.PublishRequest{ContentID: cid, TemplateVersionID: tvID}); err != nil {
		t.Fatalf("rename publish: %v", err)
	}
	var redir bson.M
	if err := s.db.FindOne(ctx, "redirects", bson.M{"from_path": "/news/r08-old"}, &redir); err != nil {
		t.Fatalf("rename redirect missing: %v", err)
	}
	if redir["to_path"] != "/news/r08-new" {
		t.Fatalf("redirect to_path = %v", redir["to_path"])
	}
	now, err := os.ReadFile(filepath.Join(s.root, "generated", "news", "r08-new.html"))
	if err != nil || !strings.Contains(string(now), "v2") {
		t.Fatalf("new canonical: %v %q", err, now)
	}
	if _, err := os.Stat(filepath.Join(s.root, "generated", "news", "r08-old.html")); !os.IsNotExist(err) {
		t.Fatalf("old canonical must be retired: %v", err)
	}
	if active, err := s.repo.GetActive(ctx, cid); err != nil || active == nil || active.FullPath != "/news/r08-new" {
		t.Fatalf("active = %+v, err = %v", active, err)
	}
}
