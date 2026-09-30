// Task 17 E2E migration table (spec §35): verified legacy,
// legacy_unverified, missing file, interrupt-resume — with HTTP serving and
// scanner assertions on top of the Migrator. Legacy fixtures are seeded in
// pre-V3 shape (no canonical fields, legacy index present); the deterministic
// renderer below is the public migration.Config.Render seam (production
// default covered by migration unit tests).
package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/migration"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// detRenderer serves canned bytes per full path (or a canned error). Same
// input bytes for a path always hash identically.
type detRenderer struct {
	byPath   map[string][]byte
	errPaths map[string]string
}

func (f *detRenderer) render(_ context.Context, in migration.RenderInput) ([]byte, string, error) {
	if msg, ok := f.errPaths[in.Content.FullPath]; ok {
		return nil, "", errors.New(msg)
	}
	b := f.byPath[in.Content.FullPath]
	if b == nil {
		b = []byte("<html><body>default render for " + in.Content.FullPath + "</body></html>")
	}
	sum := sha256.Sum256(b)
	return b, "sha256:" + hex.EncodeToString(sum[:]), nil
}

// seedLegacyTemplate inserts a pre-V3 template doc (no CurrentVersion — the
// migrator backfills v1).
func seedLegacyTemplate(t *testing.T, e *testEnv, name, slug string) primitive.ObjectID {
	t.Helper()
	id := primitive.NewObjectID()
	_, err := e.db.Collection("templates").InsertOne(context.Background(), bson.M{
		"_id": id, "name": name, "slug": slug,
		"fields":      []models.TemplateField{{Name: "headline", Label: "Headline", Type: "text"}},
		"html_layout": "<html><h1>{{.headline}}</h1></html>",
		"category":    "news",
		"created_at":  time.Now(), "updated_at": time.Now(),
	})
	if err != nil {
		t.Fatalf("seed legacy template: %v", err)
	}
	return id
}

// seedLegacyContent inserts a pre-V3 published content + version 1 (no
// canonical fields — the migrator backfills them).
func seedLegacyContent(t *testing.T, e *testEnv, tplID primitive.ObjectID, fullPath string) primitive.ObjectID {
	t.Helper()
	ctx := context.Background()
	id := primitive.NewObjectID()
	_, err := e.db.Collection("content").InsertOne(ctx, bson.M{
		"_id": id, "template_id": tplID, "template_name": "Legacy",
		"title": "Title " + fullPath, "slug": filepath.Base(fullPath),
		"folder_path": "/news", "full_path": fullPath,
		"data": bson.M{"headline": "legacy hello"}, "published": true,
		"published_at": time.Now().Add(-time.Hour),
		"created_at":   time.Now(), "updated_at": time.Now(),
	})
	if err != nil {
		t.Fatalf("seed legacy content: %v", err)
	}
	_, err = e.db.Collection("content_versions").InsertOne(ctx, bson.M{
		"_id": primitive.NewObjectID(), "content_id": id, "version": int64(1),
		"title": "v", "created_at": time.Now(),
	})
	if err != nil {
		t.Fatalf("seed legacy version: %v", err)
	}
	return id
}

func writeCanonicalFile(t *testing.T, e *testEnv, fullPath string, body []byte) {
	t.Helper()
	p, err := e.store.CanonicalFilePath(fullPath)
	if err != nil {
		t.Fatalf("canonical path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatalf("write canonical: %v", err)
	}
}

// preMigrationIndexes simulates the pre-V3 index state: the new canonical
// unique index is absent and the legacy (full_path, fork_id) index exists.
func preMigrationIndexes(t *testing.T, e *testEnv) {
	t.Helper()
	ctx := context.Background()
	_, _ = e.db.Collection("content").Indexes().DropOne(ctx, "content_canonical_path_scope_unique")
	_, err := e.db.Collection("content").Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "full_path", Value: 1}, {Key: "fork_id", Value: 1}},
		Options: options.Index().SetName("full_path_1_fork_id_1").SetUnique(true).SetSparse(true),
	})
	if err != nil && !strings.Contains(err.Error(), "already exists") && !strings.Contains(err.Error(), "IndexKeySpecsConflict") {
		t.Fatalf("create legacy index: %v", err)
	}
}

func hasIndex(t *testing.T, e *testEnv, collection, name string) bool {
	t.Helper()
	cur, err := e.db.Collection(collection).Indexes().List(context.Background())
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	defer cur.Close(context.Background())
	for cur.Next(context.Background()) {
		var spec bson.M
		if err := cur.Decode(&spec); err != nil {
			continue
		}
		if n, _ := spec["name"].(string); n == name {
			return true
		}
	}
	return false
}

// TestE2E_MigrationDryRun proves dry-run reports every blocker category
// with ZERO writes: no publications, no flag doc, no backfill, no files.
func TestE2E_MigrationDryRun(t *testing.T) {
	e := newEnv(t, envOpts{})
	preMigrationIndexes(t, e)
	tplGood := seedLegacyTemplate(t, e, "Good", "good-news")
	seedLegacyTemplate(t, e, "Bad Slug Template", "Bad Slug!")
	// Verified candidate: canonical == render.
	seedLegacyContent(t, e, tplGood, "/news/verified-page")
	writeCanonicalFile(t, e, "/news/verified-page", []byte("<html><body>VERIFIED</body></html>"))
	// Legacy candidate: canonical != render.
	seedLegacyContent(t, e, tplGood, "/news/legacy-page")
	writeCanonicalFile(t, e, "/news/legacy-page", []byte("<html><body>OLD BYTES</body></html>"))
	// Collision: two live rows, one canonical.
	seedLegacyContent(t, e, tplGood, "/news/Collide")
	seedLegacyContent(t, e, tplGood, "/news/collide")
	writeCanonicalFile(t, e, "/news/collide", []byte("<html>collided</html>"))
	// Missing file + render error.
	seedLegacyContent(t, e, tplGood, "/news/missing-page")
	seedLegacyContent(t, e, tplGood, "/news/render-error-page")
	writeCanonicalFile(t, e, "/news/render-error-page", []byte("<html>whatever</html>"))

	ctx := context.Background()
	pubsBefore := e.count("content_publications", bson.M{})
	filesBefore := countFiles(t, e)
	m := migration.New(migration.Config{DB: e.db, Store: e.store, BaseURL: e.base, Render: (&detRenderer{
		byPath: map[string][]byte{
			"/news/verified-page": []byte("<html><body>VERIFIED</body></html>"),
			"/news/legacy-page":   []byte("<html><body>NEW RENDER</body></html>"),
		},
		errPaths: map[string]string{"/news/render-error-page": "boom"},
	}).render})
	rep, err := m.DryRun(ctx)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if !rep.DryRun || rep.Completed {
		t.Fatalf("dry-run flags: %+v", rep)
	}
	if len(rep.InvalidSlugs) == 0 {
		t.Fatal("dry-run missed invalid slugs")
	}
	if len(rep.CanonicalCollisions) == 0 {
		t.Fatal("dry-run missed canonical collisions")
	}
	if len(rep.MissingFiles) == 0 {
		t.Fatal("dry-run missed missing files")
	}
	if len(rep.RenderErrors) == 0 {
		t.Fatal("dry-run missed render errors")
	}
	if len(rep.Verified) == 0 {
		t.Fatal("dry-run missed verified candidate")
	}
	if len(rep.LegacyUnverified) == 0 {
		t.Fatal("dry-run missed legacy_unverified candidate")
	}
	// Zero writes.
	if n := e.count("content_publications", bson.M{}); n != pubsBefore {
		t.Fatalf("dry-run wrote publications (%d)", n)
	}
	if n := e.count("system_migrations", bson.M{}); n != 0 {
		t.Fatal("dry-run wrote the migration flag")
	}
	if n := countFiles(t, e); n != filesBefore {
		t.Fatal("dry-run changed files")
	}
	var probe bson.M
	if err := e.db.FindOne(ctx, "content", bson.M{"full_path": "/news/verified-page"}, &probe); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if _, ok := probe["canonical_full_path"]; ok {
		t.Fatal("dry-run backfilled canonical fields")
	}
}

// TestE2E_MigrationApply is the happy path: verified + legacy_unverified
// pages reconcile, the flag completes, indexes swap, and both URLs keep
// serving (legacy bytes unchanged) with active records.
func TestE2E_MigrationApply(t *testing.T) {
	e := newEnv(t, envOpts{})
	preMigrationIndexes(t, e)
	if !hasIndex(t, e, "content", "full_path_1_fork_id_1") {
		t.Fatal("legacy index fixture missing")
	}
	tpl := seedLegacyTemplate(t, e, "Good", "good-news")
	cidV := seedLegacyContent(t, e, tpl, "/news/verified-page")
	writeCanonicalFile(t, e, "/news/verified-page", []byte("<html><body>VERIFIED</body></html>"))
	cidL := seedLegacyContent(t, e, tpl, "/news/legacy-page")
	legacyBytes := []byte("<html><body>OLD BYTES</body></html>")
	writeCanonicalFile(t, e, "/news/legacy-page", legacyBytes)

	m := migration.New(migration.Config{DB: e.db, Store: e.store, BaseURL: e.base, Render: (&detRenderer{
		byPath: map[string][]byte{
			"/news/verified-page": []byte("<html><body>VERIFIED</body></html>"),
			"/news/legacy-page":   []byte("<html><body>NEW RENDER</body></html>"),
		},
	}).render})
	rep, err := m.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (report %+v)", err, rep)
	}
	if !rep.Completed || rep.MigrationTo != migration.StatusCompleted {
		t.Fatalf("not completed: %+v", rep)
	}
	if !rep.NewIndexesVerified || !rep.LegacyIndexDropped {
		t.Fatalf("index swap not evidenced: %+v", rep)
	}
	if hasIndex(t, e, "content", "full_path_1_fork_id_1") {
		t.Fatal("legacy index still present after completion")
	}
	if !hasIndex(t, e, "content", "content_canonical_path_scope_unique") {
		t.Fatal("new canonical index missing after completion")
	}
	// Verified page: active verified publication, bytes served.
	activeV := e.activePub(t, cidV)
	if activeV == nil || activeV.VerificationStatus != publication.VerificationVerified {
		t.Fatalf("verified active = %+v", activeV)
	}
	if c, body := e.getAnon("/news/verified-page"); c != 200 || string(body) != "<html><body>VERIFIED</body></html>" {
		t.Fatalf("verified page wrong: %d %s", c, body)
	}
	// Legacy page: active legacy_unverified, OLD bytes still served.
	activeL := e.activePub(t, cidL)
	if activeL == nil || activeL.VerificationStatus != publication.VerificationLegacyUnverified {
		t.Fatalf("legacy active = %+v", activeL)
	}
	if c, body := e.getAnon("/news/legacy-page"); c != 200 || string(body) != string(legacyBytes) {
		t.Fatalf("legacy page not serving old bytes: %d %s", c, body)
	}
	// Every serving canonical has an active record.
	if n := e.count("content_publications", bson.M{"status": "active"}); n != 2 {
		t.Fatalf("active publications = %d, want 2", n)
	}
}

// TestE2E_MigrationBlocked proves a missing file blocks completion: the
// flag stays running, no active is invented for the missing page, and the
// error is ErrMigrationBlocked.
func TestE2E_MigrationBlocked(t *testing.T) {
	e := newEnv(t, envOpts{})
	preMigrationIndexes(t, e)
	tpl := seedLegacyTemplate(t, e, "Good", "good-news")
	seedLegacyContent(t, e, tpl, "/news/verified-page")
	writeCanonicalFile(t, e, "/news/verified-page", []byte("<html><body>VERIFIED</body></html>"))
	cidM := seedLegacyContent(t, e, tpl, "/news/missing-page")

	m := migration.New(migration.Config{DB: e.db, Store: e.store, BaseURL: e.base, Render: (&detRenderer{
		byPath: map[string][]byte{"/news/verified-page": []byte("<html><body>VERIFIED</body></html>")},
	}).render})
	rep, err := m.Run(context.Background())
	if err == nil || !errors.Is(err, migration.ErrMigrationBlocked) {
		t.Fatalf("Run err = %v (want ErrMigrationBlocked), report %+v", err, rep)
	}
	if len(rep.MissingFiles) == 0 {
		t.Fatalf("missing file not reported: %+v", rep)
	}
	if rep.Completed || migration.GetState(context.Background(), e.db) == migration.StatusCompleted {
		t.Fatal("migration completed despite missing file")
	}
	if active := e.activePub(t, cidM); active != nil {
		t.Fatalf("active invented for missing-file page: %s", active.ID.Hex())
	}
	if c, _ := e.getAnon("/news/missing-page"); c != 404 {
		t.Fatalf("missing page served: %d", c)
	}
}

// TestE2E_MigrationResume proves interrupt-resume: a render error blocks
// page 1 while page 2 completes; fixing the renderer and re-running
// completes without duplicating page 2's active.
func TestE2E_MigrationResume(t *testing.T) {
	e := newEnv(t, envOpts{})
	preMigrationIndexes(t, e)
	tpl := seedLegacyTemplate(t, e, "Good", "good-news")
	seedLegacyContent(t, e, tpl, "/news/good-page")
	writeCanonicalFile(t, e, "/news/good-page", []byte("<html><body>GOOD</body></html>"))
	cidBad := seedLegacyContent(t, e, tpl, "/news/bad-page")
	writeCanonicalFile(t, e, "/news/bad-page", []byte("<html>bad</html>"))

	r := &detRenderer{
		byPath:   map[string][]byte{"/news/good-page": []byte("<html><body>GOOD</body></html>")},
		errPaths: map[string]string{"/news/bad-page": "transient render boom"},
	}
	m := migration.New(migration.Config{DB: e.db, Store: e.store, BaseURL: e.base, Render: r.render})
	rep, err := m.Run(context.Background())
	if err == nil || !errors.Is(err, migration.ErrMigrationBlocked) {
		t.Fatalf("first Run err = %v (want blocked)", err)
	}
	if len(rep.RenderErrors) == 0 {
		t.Fatalf("render error not reported: %+v", rep)
	}
	if active := e.activePub(t, cidBad); active != nil {
		t.Fatal("active created for render-error page")
	}
	// Fix the renderer (operator action) and re-run: resume, no duplicates.
	r.errPaths = map[string]string{}
	r.byPath["/news/bad-page"] = []byte("<html>bad</html>")
	m2 := migration.New(migration.Config{DB: e.db, Store: e.store, BaseURL: e.base, Render: r.render})
	rep2, err := m2.Run(context.Background())
	if err != nil {
		t.Fatalf("resume Run: %v (%+v)", err, rep2)
	}
	if !rep2.Completed {
		t.Fatalf("resume not completed: %+v", rep2)
	}
	if n := e.count("content_publications", bson.M{"status": "active"}); n != 2 {
		t.Fatalf("active publications = %d, want 2 (no duplicates)", n)
	}
	if len(rep2.SkippedActive) == 0 {
		t.Fatalf("resume did not record skipped-active: %+v", rep2)
	}
	if c, body := e.getAnon("/news/good-page"); c != 200 || !strings.Contains(string(body), "GOOD") {
		t.Fatalf("good page not served after resume: %d %s", c, body)
	}
}

// TestE2E_MigrationScannerKeepsLegacyOnline proves the §35 hold: after
// completion the scanner leaves legacy_unverified pages served (never
// orphan-quarantined) and invents no actives.
func TestE2E_MigrationScannerKeepsLegacyOnline(t *testing.T) {
	e := newEnv(t, envOpts{})
	preMigrationIndexes(t, e)
	tpl := seedLegacyTemplate(t, e, "Good", "good-news")
	cidL := seedLegacyContent(t, e, tpl, "/news/legacy-page")
	legacyBytes := []byte("<html><body>OLD BYTES</body></html>")
	writeCanonicalFile(t, e, "/news/legacy-page", legacyBytes)

	m := migration.New(migration.Config{DB: e.db, Store: e.store, BaseURL: e.base, Render: (&detRenderer{
		byPath: map[string][]byte{"/news/legacy-page": []byte("<html><body>NEW RENDER</body></html>")},
	}).render})
	if _, err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	rep, err := e.scanner.ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if rep.OrphanQuarantined != 0 {
		t.Fatalf("scanner quarantined legacy pages: %+v", rep)
	}
	if c, body := e.getAnon("/news/legacy-page"); c != 200 || string(body) != string(legacyBytes) {
		t.Fatalf("legacy page disappeared after scan: %d %s", c, body)
	}
	if active := e.activePub(t, cidL); active == nil || active.VerificationStatus != publication.VerificationLegacyUnverified {
		t.Fatalf("legacy active disturbed by scanner: %+v", active)
	}
	// Re-running migration after completion is a no-op (no new actives).
	nBefore := e.count("content_publications", bson.M{})
	m2 := migration.New(migration.Config{DB: e.db, Store: e.store, BaseURL: e.base, Render: (&detRenderer{}).render})
	rep2, err := m2.Run(context.Background())
	if err != nil || !rep2.Completed {
		t.Fatalf("re-run = %v %+v, want completed no-op", err, rep2)
	}
	if n := e.count("content_publications", bson.M{}); n != nBefore {
		t.Fatalf("re-run minted publications (%d -> %d)", nBefore, n)
	}
}

func countFiles(t *testing.T, e *testEnv) int64 {
	t.Helper()
	var n int64
	_ = filepath.Walk(e.root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			n++
		}
		return nil
	})
	return n
}

// TestE2E_MigrationDefaultRenderer runs one apply with the PRODUCTION
// renderer (nil Config.Render → PlanSnapshot + Render): the legacy page
// reconciles as legacy_unverified with old bytes served. This exercises the
// Task 7 snapshot/render path end to end (no fake).
func TestE2E_MigrationDefaultRenderer(t *testing.T) {
	e := newEnv(t, envOpts{})
	preMigrationIndexes(t, e)
	tpl := seedLegacyTemplate(t, e, "Good", "good-news")
	cid := seedLegacyContent(t, e, tpl, "/news/legacy-default-render")
	legacyBytes := []byte("<html><body>OLD BYTES, STALE THEME</body></html>")
	writeCanonicalFile(t, e, "/news/legacy-default-render", legacyBytes)

	m := migration.New(migration.Config{DB: e.db, Store: e.store, BaseURL: e.base})
	rep, err := m.Run(context.Background())
	if err != nil {
		t.Fatalf("Run with production renderer: %v (%+v)", err, rep)
	}
	if !rep.Completed {
		t.Fatalf("not completed: %+v", rep)
	}
	active := e.activePub(t, cid)
	if active == nil || active.VerificationStatus != publication.VerificationLegacyUnverified {
		t.Fatalf("active = %+v, want legacy_unverified", active)
	}
	if c, body := e.getAnon("/news/legacy-default-render"); c != 200 || string(body) != string(legacyBytes) {
		t.Fatalf("old bytes not served: %d %s", c, body)
	}
	// Package-level helpers + report predicate (thin wrappers).
	if st := migration.GetState(context.Background(), e.db); st != migration.StatusCompleted {
		t.Fatalf("GetState = %q", st)
	}
	if rep.HasBlocking() {
		t.Fatalf("HasBlocking on clean report: %+v", rep)
	}
	rep2, err := migration.DryRun(context.Background(), migration.Config{DB: e.db, Store: e.store, BaseURL: e.base})
	if err != nil {
		t.Fatalf("package DryRun: %v", err)
	}
	_ = rep2
	rep3, err := migration.Run(context.Background(), migration.Config{DB: e.db, Store: e.store, BaseURL: e.base})
	if err != nil || !rep3.Completed {
		t.Fatalf("package Run = %v %+v", err, rep3)
	}
}
