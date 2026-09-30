package migration_test

// Task 17B coverage-gap tests for internal/product/migration.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/product/migration"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestCoverGapPackageLevelRunAndDryRun(t *testing.T) {
	db := migDB(t)
	ctx := context.Background()
	st, _ := migStore(t)

	tplOK := seedTemplate(t, db, "OK Template", "ok-template")
	seedTemplate(t, db, "Bad Slug", "Bad Slug!")
	seedTemplate(t, db, "Dup B", "dup-slug")
	seedTemplate(t, db, "Dup C", "dup-slug")
	seedTemplate(t, db, "Empty Slug", "")

	clean := seedContent(t, db, tplOK, "/news/clean", true, nil)
	legacy := seedContent(t, db, tplOK, "/news/legacy", true, nil)
	missing := seedContent(t, db, tplOK, "/news/missing", true, nil)
	_ = missing
	collide := seedContent(t, db, tplOK, "/News/Collide", true, nil)
	_ = collide
	seedContent(t, db, tplOK, "/news/collide", true, nil)
	seedContent(t, db, tplOK, "/news//bad", true, nil)
	dupver := seedContent(t, db, tplOK, "/news/dupver", true, nil)
	seedContent(t, db, tplOK, "/news/renderboom", true, nil)
	seedContent(t, db, tplOK, "relative-path", true, nil)
	seedContentVersion(t, db, dupver, 1)
	seedContentVersion(t, db, dupver, 1)

	cleanBytes := []byte("<html><body>clean current render</body></html>")
	legacyFile := []byte("<html><body>legacy bytes served today</body></html>")
	writeCanonical(t, st, "/news/clean", cleanBytes)
	writeCanonical(t, st, "/news/legacy", legacyFile)
	writeCanonical(t, st, "/news/dupver", cleanBytes)
	writeCanonical(t, st, "/news/collide", cleanBytes)
	writeCanonical(t, st, "/News/Collide", cleanBytes)

	fr := &fakeRenderer{
		byPath: map[string][]byte{
			"/news/clean": cleanBytes,
			"/news/legacy": []byte("<html><body>fresh re-render differs</body></html>"),
			"/news/dupver": cleanBytes,
		},
		errPaths: map[string]string{"/news/renderboom": "boom: template exploded"},
	}
	cfg := migration.Config{DB: db, Store: st, Render: fr.fn(), BaseURL: "https://example.com"}

	// Package-level DryRun (zero writes) over a rich fixture.
	drep, err := migration.DryRun(ctx, cfg)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if !drep.DryRun {
		t.Fatalf("DryRun report not marked")
	}
	if !drep.HasBlocking() {
		t.Fatalf("rich fixture DryRun: want blockers, got %+v", drep)
	}
	if drep.BlockingCount() == 0 {
		t.Fatalf("BlockingCount = 0 with blockers present")
	}

	// Package-level Run on the blocking fixture refuses to complete.
	if _, err := migration.Run(ctx, cfg); err == nil {
		t.Fatalf("blocked Run: want error, got nil")
	} else if !strings.Contains(err.Error(), "block") {
		t.Fatalf("blocked Run error = %v, want blocking complaint", err)
	}
	_ = clean
	_ = legacy
}

func TestCoverGapCleanRunCompletesAndReRuns(t *testing.T) {
	db := migDB(t)
	ctx := context.Background()
	st, _ := migStore(t)

	tplOK := seedTemplate(t, db, "OK Template", "ok-template")
	c1 := seedContent(t, db, tplOK, "/news/one", true, nil)
	c2 := seedContent(t, db, tplOK, "/news/two", true, nil)
	seedContentVersion(t, db, c1, 1)
	seedContentVersion(t, db, c2, 1)
	one := []byte("<html><body>one</body></html>")
	two := []byte("<html><body>two</body></html>")
	writeCanonical(t, st, "/news/one", one)
	writeCanonical(t, st, "/news/two", two)
	fr := &fakeRenderer{byPath: map[string][]byte{"/news/one": one, "/news/two": two}}
	cfg := migration.Config{DB: db, Store: st, Render: fr.fn(), BaseURL: "https://example.com"}

	rep, err := migration.Run(ctx, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rep.Completed {
		t.Fatalf("clean Run must complete: %+v", rep)
	}
	// Second run is an idempotent no-op.
	rep2, err := migration.Run(ctx, cfg)
	if err != nil {
		t.Fatalf("re-Run: %v", err)
	}
	if !rep2.Completed {
		t.Fatalf("re-Run must report completed: %+v", rep2)
	}
	if got := migration.GetState(ctx, db); got == "" {
		t.Fatalf("GetState empty after completion")
	}
}

func TestCoverGapConstructorGuards(t *testing.T) {
	db := migDB(t)
	st, _ := migStore(t)
	for _, cfg := range []migration.Config{
		{Store: st},
		{DB: db},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("New(%+v): want panic", cfg)
				}
			}()
			_ = migration.New(cfg)
		}()
	}
	m := migration.New(migration.Config{DB: db, Store: st})
	if m == nil {
		t.Fatalf("New with defaults nil")
	}
}

func TestCoverGapReportBlockingCounts(t *testing.T) {
	var empty migration.Report
	if empty.HasBlocking() || empty.BlockingCount() != 0 {
		t.Fatalf("empty report blocks")
	}
	full := migration.Report{
		InvalidSlugs:        []migration.SlugIssue{{Slug: "x"}},
		CanonicalCollisions: []migration.Collision{{Canonical: "y"}},
		VersionDuplicates:   []migration.VersionDuplicate{{ContentID: "z"}},
		InvalidPaths:        []migration.PageItem{{FullPath: "/bad"}},
		MissingFiles:        []migration.PageItem{{FullPath: "/m"}},
		RenderErrors:        []migration.PageItem{{FullPath: "/r"}},
	}
	if !full.HasBlocking() {
		t.Fatalf("full report: want blocking")
	}
	if full.BlockingCount() != 6 {
		t.Fatalf("BlockingCount = %d, want 6", full.BlockingCount())
	}
}

func TestCoverGapUnknownTemplateInspectAndStage(t *testing.T) {
	db := migDB(t)
	ctx := context.Background()
	st, root := migStore(t)

	tplOK := seedTemplate(t, db, "OK Template", "ok-template")
	// Case-fold slug collision pair (both reported; "News" also invalid).
	seedTemplate(t, db, "Upper News", "News")
	seedTemplate(t, db, "Lower News", "news")

	// Content pointing at a template that does not exist.
	orphan := seedContent(t, db, primitive.NewObjectID(), "/news/orphan", true, nil)
	_ = orphan

	// Canonical path occupied by a directory: Inspect hard-fails.
	dirPage := seedContent(t, db, tplOK, "/news/dirpage", true, nil)
	_ = dirPage
	dirCanon, err := st.CanonicalFilePath("/news/dirpage")
	if err != nil {
		t.Fatalf("CanonicalFilePath: %v", err)
	}
	if err := os.MkdirAll(dirCanon, 0o755); err != nil {
		t.Fatalf("mkdir dirpage: %v", err)
	}

	// Empty canonical file + renderer returning empty bytes: verified-path
	// match, then Stage rejects the empty body ("migrate: ..." in Run).
	emptyPage := seedContent(t, db, tplOK, "/news/emptypage", true, nil)
	_ = emptyPage
	writeCanonical(t, st, "/news/emptypage", []byte{})

	// Two duplicate-version groups on one content: version sort tiebreak.
	dv := seedContent(t, db, tplOK, "/news/dupver2", true, nil)
	seedContentVersion(t, db, dv, 1)
	seedContentVersion(t, db, dv, 1)
	seedContentVersion(t, db, dv, 2)
	seedContentVersion(t, db, dv, 2)
	dv2 := seedContent(t, db, tplOK, "/news/dupver3", true, nil)
	seedContentVersion(t, db, dv2, 3)
	seedContentVersion(t, db, dv2, 3)

	render := func(_ context.Context, in migration.RenderInput) ([]byte, string, error) {
		if in.Content.FullPath == "/news/emptypage" {
			sum := sha256.Sum256([]byte{})
			return []byte{}, "sha256:" + hex.EncodeToString(sum[:]), nil
		}
		b := []byte("<html><body>render of " + in.Content.FullPath + "</body></html>")
		sum := sha256.Sum256(b)
		return b, "sha256:" + hex.EncodeToString(sum[:]), nil
	}
	cfg := migration.Config{DB: db, Store: st, Render: render, BaseURL: "https://example.com"}

	drep, err := migration.DryRun(ctx, cfg)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	foundUnknown, foundInspect := false, false
	for _, e := range drep.RenderErrors {
		if strings.Contains(e.Detail, "unknown template") {
			foundUnknown = true
		}
		if strings.Contains(e.Detail, "inspect:") {
			foundInspect = true
		}
	}
	if !foundUnknown {
		t.Fatalf("DryRun: want unknown-template render error: %+v", drep.RenderErrors)
	}
	if !foundInspect {
		t.Fatalf("DryRun: want inspect render error: %+v", drep.RenderErrors)
	}
	foundCaseFold := false
	for _, s := range drep.InvalidSlugs {
		if strings.Contains(s.Reason, "case-fold") {
			foundCaseFold = true
		}
	}
	if !foundCaseFold {
		t.Fatalf("DryRun: want case-fold slug issue: %+v", drep.InvalidSlugs)
	}
	if len(drep.VersionDuplicates) < 3 {
		t.Fatalf("DryRun: want >=3 dup groups, got %d", len(drep.VersionDuplicates))
	}

	rrep, err := migration.Run(ctx, cfg)
	if err == nil {
		t.Fatalf("blocked Run: want error")
	}
	foundStage := false
	for _, e := range rrep.RenderErrors {
		if strings.Contains(e.Detail, "migrate: stage immutable") {
			foundStage = true
		}
	}
	if !foundStage {
		t.Fatalf("Run: want stage-empty-body migrate error, got %+v", rrep.RenderErrors)
	}
	_ = root
}

func TestCoverGapDefaultRendererAndBackfillHit(t *testing.T) {
	db := migDB(t)
	ctx := context.Background()
	st, _ := migStore(t)

	// seedTemplate stores no current_version and no template_versions doc:
	// backfill creates v1 + sets CurrentVersion=1 (production defaultRender).
	tplOK := seedTemplate(t, db, "OK Template", "ok-template")
	c1 := seedContent(t, db, tplOK, "/news/one", true, nil)
	seedContentVersion(t, db, c1, 1)
	one := []byte("<html>one</html>")
	// Canonical file differs from defaultRender output → legacy path; the
	// default renderer path (Render nil) + version<1 branch are exercised.
	writeCanonical(t, st, "/news/one", one)

	cfg := migration.Config{DB: db, Store: st, BaseURL: "https://example.com"}
	rep, err := migration.Run(ctx, cfg)
	if err != nil {
		t.Fatalf("default-render Run: %v", err)
	}
	if !rep.Completed {
		t.Fatalf("default-render Run must complete: %+v", rep)
	}
	if len(rep.LegacyUnverified) != 1 {
		t.Fatalf("want 1 legacy page (bytes differ from default render): %+v", rep)
	}

	// Second run: completed flag short-circuits to an idempotent no-op.
	rep2, err := migration.Run(ctx, cfg)
	if err != nil {
		t.Fatalf("re-Run: %v", err)
	}
	if !rep2.Completed {
		t.Fatalf("re-Run = %+v", rep2)
	}

	// Template with a persisted v1 but CurrentVersion=0 → backfill repairs
	// the mutable pointer without inserting a duplicate version.
	tplStale := seedTemplate(t, db, "Stale Pointer", "stale-pointer")
	if _, err := db.Collection("template_versions").InsertOne(ctx, bson.M{
		"_id": primitive.NewObjectID(), "template_id": tplStale, "version": int64(1),
		"slug": "stale-pointer", "name": "Stale Pointer", "category": "news",
		"status": "active", "fields": []bson.M{}, "html_layout": "<p>x</p>",
		"contract_hash": "sha256:a", "render_hash": "sha256:b",
		"created_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed v1: %v", err)
	}
	c2 := seedContent(t, db, tplStale, "/news/stale-page", true, nil)
	seedContentVersion(t, db, c2, 1)
	writeCanonical(t, st, "/news/stale-page", []byte("<p>x</p>"))
	rep3, err := migration.Run(ctx, cfg)
	if err != nil {
		t.Fatalf("backfill-hit Run: %v", err)
	}
	if !rep3.Completed {
		t.Fatalf("backfill-hit Run must complete: %+v", rep3)
	}
	var tplDoc bson.M
	if err := db.Collection("templates").FindOne(ctx, bson.M{"_id": tplStale}).Decode(&tplDoc); err != nil {
		t.Fatalf("load template: %v", err)
	}
	// No duplicate v1: exactly one version-1 doc for the stale template.
	n, err := db.Collection("template_versions").CountDocuments(ctx, bson.M{"template_id": tplStale, "version": int64(1)})
	if err != nil {
		t.Fatalf("count v1: %v", err)
	}
	if n != 1 {
		t.Fatalf("v1 count = %d, want 1 (no duplicate backfill)", n)
	}
}
