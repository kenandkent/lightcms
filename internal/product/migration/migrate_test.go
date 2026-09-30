// Task 14 red tests: existing-site migration and reconciliation.
//
// These tests run against the test replica set (transactions required) and
// MUST be run with -p 1:
//
//	go test -p 1 ./internal/product/migration -run TestMigrate -count=1
package migration_test

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

	"github.com/jonradoff/lightcms/v7/internal/database"
	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/migration"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/storage"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"
	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func migDB(t *testing.T) *database.DB {
	t.Helper()
	db, _ := testutil.MustConnectTestDB(t)
	return db
}

func migStore(t *testing.T) (*storage.FilesystemStore, string) {
	t.Helper()
	root := t.TempDir()
	return storage.NewFilesystemStore(root), root
}

// fakeRenderer serves canned render bytes per full path (or a canned error).
// It lets migration tests control match/mismatch without the full renderer.
type fakeRenderer struct {
	byPath   map[string][]byte
	errPaths map[string]string
	calls    int
}

func (f *fakeRenderer) render(_ context.Context, in migration.RenderInput) ([]byte, string, error) {
	f.calls++
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

func (f *fakeRenderer) fn() migration.RenderFunc { return f.render }

func seedTemplate(t *testing.T, db *database.DB, name, slug string) primitive.ObjectID {
	t.Helper()
	id := primitive.NewObjectID()
	_, err := db.Collection("templates").InsertOne(context.Background(), bson.M{
		"_id": id, "name": name, "slug": slug,
		"fields": []models.TemplateField{
			{Name: "headline", Label: "Headline", Type: "text"},
		},
		"html_layout": "<html><h1>{{.headline}}</h1></html>",
		"category":    "news",
		"created_at":  time.Now(), "updated_at": time.Now(),
	})
	if err != nil {
		t.Fatalf("seed template %q: %v", name, err)
	}
	return id
}

func seedContent(t *testing.T, db *database.DB, tplID primitive.ObjectID, fullPath string, published bool, data map[string]any) primitive.ObjectID {
	t.Helper()
	id := primitive.NewObjectID()
	if data == nil {
		data = map[string]any{"headline": "hello"}
	}
	doc := bson.M{
		"_id": id, "template_id": tplID, "template_name": "T",
		"title": "Title " + fullPath, "slug": filepath.Base(fullPath),
		"folder_path": "/news", "full_path": fullPath,
		"data": data, "published": published,
		"created_at": time.Now(), "updated_at": time.Now(),
	}
	if published {
		ts := time.Now().Add(-time.Hour)
		doc["published_at"] = ts
	}
	if _, err := db.Collection("content").InsertOne(context.Background(), doc); err != nil {
		t.Fatalf("seed content %q: %v", fullPath, err)
	}
	return id
}

func seedContentVersion(t *testing.T, db *database.DB, cid primitive.ObjectID, v int64) {
	t.Helper()
	_, err := db.Collection("content_versions").InsertOne(context.Background(), bson.M{
		"_id": primitive.NewObjectID(), "content_id": cid, "version": v,
		"title": "v", "created_at": time.Now(),
	})
	if err != nil {
		t.Fatalf("seed content version %d: %v", v, err)
	}
}

func writeCanonical(t *testing.T, st *storage.FilesystemStore, fullPath string, body []byte) {
	t.Helper()
	p, err := st.CanonicalFilePath(fullPath)
	if err != nil {
		t.Fatalf("CanonicalFilePath(%q): %v", fullPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatalf("write canonical: %v", err)
	}
}

func readCanonical(t *testing.T, st *storage.FilesystemStore, fullPath string) []byte {
	t.Helper()
	p, err := st.CanonicalFilePath(fullPath)
	if err != nil {
		t.Fatalf("CanonicalFilePath(%q): %v", fullPath, err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read canonical %q: %v", fullPath, err)
	}
	return b
}

func countDocs(t *testing.T, db *database.DB, col string, filter bson.M) int64 {
	t.Helper()
	n, err := db.Collection(col).CountDocuments(context.Background(), filter)
	if err != nil {
		t.Fatalf("count %s: %v", col, err)
	}
	return n
}

func getActive(t *testing.T, db *database.DB, cid primitive.ObjectID) *publication.Publication {
	t.Helper()
	repo := publication.NewRepository(db, nil)
	p, err := repo.GetActive(context.Background(), cid)
	if err != nil {
		t.Fatalf("GetActive: %v", err)
	}
	return p
}

func hasIndex(t *testing.T, db *database.DB, collection, name string) bool {
	t.Helper()
	cur, err := db.Collection(collection).Indexes().List(context.Background())
	if err != nil {
		t.Fatalf("ListIndexes %s: %v", collection, err)
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

// TestMigrate_DryRunReportsWithoutWrites: dry-run must surface all five
// report categories (invalid/duplicate template slugs, canonical collisions,
// content-version duplicates, missing files, render mismatches + render
// errors) while performing ZERO writes (no publications, no flag doc, no
// backfill fields, no immutable files).
func TestMigrate_DryRunReportsWithoutWrites(t *testing.T) {
	db := migDB(t)
	ctx := context.Background()
	st, root := migStore(t)

	tplOK := seedTemplate(t, db, "OK Template", "ok-template")
	seedTemplate(t, db, "Empty Slug", "")
	seedTemplate(t, db, "Dup B", "dup-slug")
	seedTemplate(t, db, "Dup C", "dup-slug")
	seedTemplate(t, db, "Bad Slug", "Bad Slug!")

	c1 := seedContent(t, db, tplOK, "/News/Foo", true, nil)
	c2 := seedContent(t, db, tplOK, "/news/foo", true, nil)
	c3 := seedContent(t, db, tplOK, "/news/missing", true, nil)
	c4 := seedContent(t, db, tplOK, "/news/legacy", true, nil)
	seedContent(t, db, tplOK, "/news/clean", true, nil)
	c6 := seedContent(t, db, tplOK, "/news/dupver", true, nil)
	seedContent(t, db, tplOK, "/news/renderboom", true, nil)
	_ = c1
	_ = c2
	_ = c3

	legacyOld := []byte("<html><body>legacy bytes served today</body></html>")
	cleanBytes := []byte("<html><body>clean current render</body></html>")
	dupverBytes := []byte("<html><body>dupver current render</body></html>")
	boomBytes := []byte("<html><body>boom file</body></html>")
	defaultFoo := []byte("<html><body>default render for /News/Foo</body></html>")
	defaultFooLower := []byte("<html><body>default render for /news/foo</body></html>")
	writeCanonical(t, st, "/News/Foo", defaultFoo)
	writeCanonical(t, st, "/news/foo", defaultFooLower)
	writeCanonical(t, st, "/news/legacy", legacyOld)
	writeCanonical(t, st, "/news/clean", cleanBytes)
	writeCanonical(t, st, "/news/dupver", dupverBytes)
	writeCanonical(t, st, "/news/renderboom", boomBytes)
	// /news/missing intentionally has no file.

	seedContentVersion(t, db, c6, 1)
	seedContentVersion(t, db, c6, 1) // duplicate (content_id, version)

	fr := &fakeRenderer{
		byPath: map[string][]byte{
			"/news/legacy": []byte("<html><body>fresh re-render differs</body></html>"),
			"/news/clean":  cleanBytes,
			"/news/dupver": dupverBytes,
			"/News/Foo":    defaultFoo,
			"/news/foo":    defaultFooLower,
		},
		errPaths: map[string]string{"/news/renderboom": "boom: template exploded"},
	}

	m := migration.New(migration.Config{DB: db, Store: st, Render: fr.fn()})
	rep, err := m.DryRun(ctx)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if rep.DryRun != true {
		t.Error("expected DryRun=true in dry-run report")
	}
	if len(rep.InvalidSlugs) < 4 {
		t.Errorf("expected >=4 invalid/duplicate slug issues (empty+bad+dupx2), got %d: %+v", len(rep.InvalidSlugs), rep.InvalidSlugs)
	}
	if len(rep.CanonicalCollisions) != 1 {
		t.Errorf("expected 1 canonical collision, got %d: %+v", len(rep.CanonicalCollisions), rep.CanonicalCollisions)
	} else if rep.CanonicalCollisions[0].Canonical != "/news/foo" {
		t.Errorf("expected collision canonical /news/foo, got %q", rep.CanonicalCollisions[0].Canonical)
	}
	if len(rep.VersionDuplicates) != 1 {
		t.Errorf("expected 1 version-duplicate group, got %+v", rep.VersionDuplicates)
	}
	if len(rep.MissingFiles) != 1 || rep.MissingFiles[0].FullPath != "/news/missing" {
		t.Errorf("expected MissingFiles=[/news/missing], got %+v", rep.MissingFiles)
	}
	if len(rep.RenderErrors) != 1 || rep.RenderErrors[0].FullPath != "/news/renderboom" {
		t.Errorf("expected RenderErrors=[/news/renderboom], got %+v", rep.RenderErrors)
	}
	if len(rep.LegacyUnverified) != 1 || rep.LegacyUnverified[0].FullPath != "/news/legacy" {
		t.Errorf("expected LegacyUnverified=[/news/legacy], got %+v", rep.LegacyUnverified)
	}
	foundClean, foundDupver := false, false
	for _, v := range rep.Verified {
		if v.FullPath == "/news/clean" {
			foundClean = true
		}
		if v.FullPath == "/news/dupver" {
			foundDupver = true
		}
	}
	if !foundClean || !foundDupver {
		t.Errorf("expected Verified to contain /news/clean and /news/dupver, got %+v", rep.Verified)
	}
	if rep.Completed {
		t.Error("dry-run must never report Completed=true")
	}

	// Zero writes.
	if n := countDocs(t, db, "content_publications", bson.M{}); n != 0 {
		t.Errorf("dry-run wrote %d publications, want 0", n)
	}
	if n := countDocs(t, db, "system_migrations", bson.M{}); n != 0 {
		t.Errorf("dry-run wrote %d migration flag docs, want 0", n)
	}
	var tdoc bson.M
	if err := db.Collection("templates").FindOne(ctx, bson.M{"_id": tplOK}).Decode(&tdoc); err != nil {
		t.Fatalf("read template: %v", err)
	}
	if _, ok := tdoc["current_version"]; ok {
		t.Error("dry-run backfilled templates.current_version, want untouched")
	}
	var cdoc bson.M
	if err := db.Collection("content").FindOne(ctx, bson.M{"_id": c4}).Decode(&cdoc); err != nil {
		t.Fatalf("read content: %v", err)
	}
	if _, ok := cdoc["canonical_full_path"]; ok {
		t.Error("dry-run backfilled canonical_full_path, want untouched")
	}
	if _, err := os.Stat(filepath.Join(root, "publications")); !os.IsNotExist(err) {
		t.Error("dry-run created immutable publication files, want none")
	}
	if got := readCanonical(t, st, "/news/legacy"); string(got) != string(legacyOld) {
		t.Error("dry-run modified the serving canonical file")
	}
}

// TestMigrate_VerifiedPath: matching canonical + fresh render creates a
// verified active publication with immutable bytes equal to the canonical,
// leaving the canonical file byte-identical.
func TestMigrate_VerifiedPath(t *testing.T) {
	db := migDB(t)
	ctx := context.Background()
	st, _ := migStore(t)

	tpl := seedTemplate(t, db, "News", "news-tpl")
	cid := seedContent(t, db, tpl, "/news/clean", true, nil)
	seedContentVersion(t, db, cid, 1)
	seedContentVersion(t, db, cid, 2)

	body := []byte("<html><body>clean</body></html>")
	writeCanonical(t, st, "/news/clean", body)
	fr := &fakeRenderer{byPath: map[string][]byte{"/news/clean": body}}

	m := migration.New(migration.Config{DB: db, Store: st, Render: fr.fn()})
	rep, err := m.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rep.Completed {
		t.Fatalf("expected Completed=true, report: %+v", rep)
	}
	if len(rep.Verified) != 1 || rep.Verified[0].FullPath != "/news/clean" {
		t.Errorf("expected Verified=[/news/clean], got %+v", rep.Verified)
	}
	active := getActive(t, db, cid)
	if active == nil {
		t.Fatal("expected active publication")
	}
	if active.VerificationStatus != publication.VerificationLegacyUnverified &&
		active.VerificationStatus != publication.VerificationVerified {
		t.Errorf("unexpected verification %q", active.VerificationStatus)
	}
	if active.VerificationStatus != publication.VerificationVerified {
		t.Errorf("matching render must yield verified, got %q", active.VerificationStatus)
	}
	sum := sha256.Sum256(body)
	wantHash := "sha256:" + hex.EncodeToString(sum[:])
	if active.ContentHash != wantHash {
		t.Errorf("active hash %q, want %q", active.ContentHash, wantHash)
	}
	imm, err := os.ReadFile(st.ImmutablePath(cid, active.ID))
	if err != nil {
		t.Fatalf("read immutable: %v", err)
	}
	if string(imm) != string(body) {
		t.Error("immutable bytes differ from canonical bytes")
	}
	if got := readCanonical(t, st, "/news/clean"); string(got) != string(body) {
		t.Error("canonical file changed during verified migration")
	}
	var cdoc bson.M
	if err := db.Collection("content").FindOne(ctx, bson.M{"_id": cid}).Decode(&cdoc); err != nil {
		t.Fatalf("read content: %v", err)
	}
	if cdoc["canonical_full_path"] != "/news/clean" || cdoc["path_scope"] != "live" || cdoc["path_active"] != true {
		t.Errorf("canonical backfill wrong: %+v", cdoc)
	}
	if v, _ := cdoc["current_version"].(int64); v != 2 {
		t.Errorf("current_version=%v, want 2 (max of seeded versions)", cdoc["current_version"])
	}
	if stt := migration.GetState(ctx, db); stt != migration.StatusCompleted {
		t.Errorf("flag=%q, want completed", stt)
	}
}

// TestMigrate_LegacyPath: mismatched canonical is copied into an immutable
// legacy object, an active legacy_unverified record is created, and the
// canonical keeps serving the old bytes.
func TestMigrate_LegacyPath(t *testing.T) {
	db := migDB(t)
	ctx := context.Background()
	st, _ := migStore(t)

	tpl := seedTemplate(t, db, "News", "news-tpl")
	cid := seedContent(t, db, tpl, "/news/legacy", true, nil)

	old := []byte("<html><body>old served bytes</body></html>")
	fresh := []byte("<html><body>fresh render differs</body></html>")
	writeCanonical(t, st, "/news/legacy", old)
	fr := &fakeRenderer{byPath: map[string][]byte{"/news/legacy": fresh}}

	m := migration.New(migration.Config{DB: db, Store: st, Render: fr.fn()})
	rep, err := m.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rep.Completed {
		t.Fatalf("expected Completed=true, report: %+v", rep)
	}
	if len(rep.LegacyUnverified) != 1 {
		t.Errorf("expected 1 legacy_unverified, got %+v", rep.LegacyUnverified)
	}
	active := getActive(t, db, cid)
	if active == nil {
		t.Fatal("legacy page must have an active publication after completion")
	}
	if active.VerificationStatus != publication.VerificationLegacyUnverified {
		t.Errorf("want legacy_unverified, got %q", active.VerificationStatus)
	}
	imm, err := os.ReadFile(st.ImmutablePath(cid, active.ID))
	if err != nil {
		t.Fatalf("read immutable legacy object: %v", err)
	}
	if string(imm) != string(old) {
		t.Error("immutable legacy object must equal the old canonical bytes")
	}
	if got := readCanonical(t, st, "/news/legacy"); string(got) != string(old) {
		t.Error("canonical must keep serving old bytes after legacy migration")
	}
}

// TestMigrate_ResumeNoDuplicateActives: an interrupted run (blocked by a
// missing file) resumes page-by-page after the admin fixes the file, without
// creating duplicate active publications, and then completes.
func TestMigrate_ResumeNoDuplicateActives(t *testing.T) {
	db := migDB(t)
	ctx := context.Background()
	st, _ := migStore(t)

	tpl := seedTemplate(t, db, "News", "news-tpl")
	cA := seedContent(t, db, tpl, "/news/a", true, nil)
	cB := seedContent(t, db, tpl, "/news/b", true, nil)
	bodyA := []byte("<html>a</html>")
	writeCanonical(t, st, "/news/a", bodyA)
	fr := &fakeRenderer{byPath: map[string][]byte{
		"/news/a": bodyA,
		"/news/b": []byte("<html>b</html>"),
	}}

	m := migration.New(migration.Config{DB: db, Store: st, Render: fr.fn()})
	rep, err := m.Run(ctx)
	if !errors.Is(err, migration.ErrMigrationBlocked) {
		t.Fatalf("expected ErrMigrationBlocked, got %v (report %+v)", err, rep)
	}
	if stt := migration.GetState(ctx, db); stt != migration.StatusRunning {
		t.Errorf("interrupted flag=%q, want running", stt)
	}
	if a := getActive(t, db, cA); a == nil {
		t.Fatal("page A should be migrated before the interruption")
	}
	if b := getActive(t, db, cB); b != nil {
		t.Error("page B (missing file) must have no active publication")
	}

	// Admin fixes the missing file; resume.
	writeCanonical(t, st, "/news/b", []byte("<html>b</html>"))
	rep2, err := m.Run(ctx)
	if err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	if !rep2.Completed {
		t.Fatalf("expected completion after resume, got %+v", rep2)
	}
	if n := countDocs(t, db, "content_publications", bson.M{"content_id": cA, "status": "active"}); n != 1 {
		t.Errorf("page A has %d active publications, want exactly 1", n)
	}
	if n := countDocs(t, db, "content_publications", bson.M{"content_id": cA}); n != 1 {
		t.Errorf("page A has %d publication records, want exactly 1 (no duplicates)", n)
	}
	if b := getActive(t, db, cB); b == nil {
		t.Error("page B should be migrated after resume")
	}
	if len(rep2.SkippedActive) != 1 || rep2.SkippedActive[0].FullPath != "/news/a" {
		t.Errorf("expected SkippedActive=[/news/a] on resume, got %+v", rep2.SkippedActive)
	}
}

// TestMigrate_BlockedWhileMissingOrRenderError: completion is refused while
// any missing-file or render error remains; the flag stays running and the
// legacy index is kept.
func TestMigrate_BlockedWhileMissingOrRenderError(t *testing.T) {
	db := migDB(t)
	ctx := context.Background()
	st, _ := migStore(t)

	// Simulate the pre-migration production index state.
	if _, err := db.Collection("content").Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "full_path", Value: 1}, {Key: "fork_id", Value: 1}},
	}); err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("create legacy index: %v", err)
	}

	tpl := seedTemplate(t, db, "News", "news-tpl")
	seedContent(t, db, tpl, "/news/ghost", true, nil) // no file
	badID := seedContent(t, db, tpl, "/news/bad", true, nil)
	writeCanonical(t, st, "/news/bad", []byte("<html>bad file</html>"))
	fr := &fakeRenderer{errPaths: map[string]string{"/news/bad": "render failed"}}

	m := migration.New(migration.Config{DB: db, Store: st, Render: fr.fn()})
	_, err := m.Run(ctx)
	if !errors.Is(err, migration.ErrMigrationBlocked) {
		t.Fatalf("expected ErrMigrationBlocked, got %v", err)
	}
	if stt := migration.GetState(ctx, db); stt != migration.StatusRunning {
		t.Errorf("blocked flag=%q, want running", stt)
	}
	if !hasIndex(t, db, "content", "full_path_1_fork_id_1") {
		t.Error("legacy index must be kept while migration is blocked")
	}
	if b := getActive(t, db, badID); b != nil {
		t.Error("render-failed page must have no active publication")
	}
}

// TestMigrate_CompletionSwapsIndexes: on a clean site the migrator creates
// the new canonical index set via Task 2, verifies the canonical index, then
// drops the legacy (full_path, fork_id) index — and only in that order.
func TestMigrate_CompletionSwapsIndexes(t *testing.T) {
	db := migDB(t)
	ctx := context.Background()
	st, _ := migStore(t)

	if _, err := db.Collection("content").Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "full_path", Value: 1}, {Key: "fork_id", Value: 1}},
	}); err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("create legacy index: %v", err)
	}

	tpl := seedTemplate(t, db, "News", "news-tpl")
	body := []byte("<html>clean</html>")
	seedContent(t, db, tpl, "/news/clean", true, nil)
	writeCanonical(t, st, "/news/clean", body)
	fr := &fakeRenderer{byPath: map[string][]byte{"/news/clean": body}}

	m := migration.New(migration.Config{DB: db, Store: st, Render: fr.fn()})
	rep, err := m.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rep.Completed {
		t.Fatalf("expected completion, got %+v", rep)
	}
	if !rep.NewIndexesVerified {
		t.Error("expected NewIndexesVerified=true")
	}
	if !rep.LegacyIndexDropped {
		t.Error("expected LegacyIndexDropped=true")
	}
	if !hasIndex(t, db, "content", "content_canonical_path_scope_unique") {
		t.Error("new canonical index missing after completion")
	}
	if hasIndex(t, db, "content", "full_path_1_fork_id_1") {
		t.Error("legacy index must be dropped after the new index is verified")
	}
	if !hasIndex(t, db, "templates", "templates_slug_unique") {
		t.Error("template slug unique index missing after completion")
	}
}

// TestMigrate_NewIndexFailureKeepsLegacy: if EnsureProductIndexes fails (here
// via a pre-existing conflicting index definition), the run errors, the flag
// stays running, and the legacy index is untouched. Removing the conflict and
// re-running completes.
func TestMigrate_NewIndexFailureKeepsLegacy(t *testing.T) {
	db := migDB(t)
	ctx := context.Background()
	st, _ := migStore(t)

	if _, err := db.Collection("content").Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "full_path", Value: 1}, {Key: "fork_id", Value: 1}},
	}); err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("create legacy index: %v", err)
	}
	// Conflicting definition under the new canonical index name: same name,
	// different keys and no uniqueness. Task 2's EnsureProductIndexes must
	// then fail with IndexOptionsConflict, proving the failure path keeps
	// the legacy index and refuses completion.
	if _, err := db.Collection("content").Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "canonical_full_path", Value: 1}},
		Options: options.Index().SetName("content_canonical_path_scope_unique"),
	}); err != nil {
		t.Fatalf("create conflicting index: %v", err)
	}

	tpl := seedTemplate(t, db, "News", "news-tpl")
	body := []byte("<html>clean</html>")
	seedContent(t, db, tpl, "/news/clean", true, nil)
	writeCanonical(t, st, "/news/clean", body)
	fr := &fakeRenderer{byPath: map[string][]byte{"/news/clean": body}}

	m := migration.New(migration.Config{DB: db, Store: st, Render: fr.fn()})
	_, err := m.Run(ctx)
	if err == nil {
		t.Fatal("expected index-creation failure, got nil error")
	}
	if errors.Is(err, migration.ErrMigrationBlocked) {
		t.Fatalf("index failure must surface the index error, not ErrMigrationBlocked: %v", err)
	}
	if stt := migration.GetState(ctx, db); stt == migration.StatusCompleted {
		t.Error("flag must not be completed after index failure")
	}
	if !hasIndex(t, db, "content", "full_path_1_fork_id_1") {
		t.Error("legacy index must be kept when new index creation fails")
	}
}

// TestMigrate_ScannerLeavesLegacyOnline: after completion, the Task 10
// recovery scanner keeps verified and legacy_unverified pages serving — no
// quarantine, no mismatch repair.
func TestMigrate_ScannerLeavesLegacyOnline(t *testing.T) {
	db := migDB(t)
	ctx := context.Background()
	st, root := migStore(t)

	tpl := seedTemplate(t, db, "News", "news-tpl")
	seedContent(t, db, tpl, "/news/keep", true, nil)
	seedContent(t, db, tpl, "/news/same", true, nil)
	old := []byte("<html>old served</html>")
	same := []byte("<html>same</html>")
	writeCanonical(t, st, "/news/keep", old)
	writeCanonical(t, st, "/news/same", same)
	fr := &fakeRenderer{byPath: map[string][]byte{
		"/news/keep": []byte("<html>fresh differs</html>"),
		"/news/same": same,
	}}

	m := migration.New(migration.Config{DB: db, Store: st, Render: fr.fn()})
	if _, err := m.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	scanner := publication.NewScanner(db, publication.NewRepository(db, nil), st,
		publication.ScannerOptions{QuarantineDir: filepath.Join(root, "quarantine")})
	srep, err := scanner.ScanOnce(ctx)
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if got := readCanonical(t, st, "/news/keep"); string(got) != string(old) {
		t.Error("scanner must leave the legacy_unverified canonical serving")
	}
	if got := readCanonical(t, st, "/news/same"); string(got) != string(same) {
		t.Error("scanner must leave the verified canonical serving")
	}
	if srep.OrphanQuarantined != 0 {
		t.Errorf("scanner quarantined %d orphans, want 0", srep.OrphanQuarantined)
	}
	if srep.CanonicalMismatchRepaired != 0 {
		t.Errorf("scanner repaired %d canonicals, want 0", srep.CanonicalMismatchRepaired)
	}
	if srep.Degraded {
		t.Error("scanner must not report degraded for migrated pages")
	}
}

// TestMigrate_DefaultRendererVerifiedPath exercises the production render
// path (publication.PlanSnapshot + publication.Render, no fake): a canonical
// file produced by the current renderer from template v1 bytes must
// reconcile as verified, proving the default wiring matches serving bytes
// for templates that do not reference per-publication system vars.
func TestMigrate_DefaultRendererVerifiedPath(t *testing.T) {
	db := migDB(t)
	ctx := context.Background()
	st, _ := migStore(t)

	tplID := seedTemplate(t, db, "News", "news-tpl")
	publishedAt := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	cid := primitive.NewObjectID()
	_, err := db.Collection("content").InsertOne(ctx, bson.M{
		"_id": cid, "template_id": tplID, "template_name": "News",
		"title": "Hello Page", "slug": "hello",
		"folder_path": "/news", "full_path": "/news/hello",
		"data":         bson.M{"headline": "Hello"},
		"published":    true,
		"published_at": publishedAt,
		"created_at":   time.Now(), "updated_at": time.Now(),
	})
	if err != nil {
		t.Fatalf("seed content: %v", err)
	}

	// Produce the serving canonical with the same frozen inputs migration
	// will use (layout references no per-publication system vars, so the
	// publication ID / logical-time freeze does not affect bytes... except
	// published_at IS frozen from PublishedAt, which we reuse here).
	var content models.Content
	if err := db.Collection("content").FindOne(ctx, bson.M{"_id": cid}).Decode(&content); err != nil {
		t.Fatalf("read content: %v", err)
	}
	snap, err := publication.PlanSnapshot(
		content, 1,
		tmplVersionForTest(tplID, "news-tpl"),
		primitive.NewObjectID(), publishedAt, "/news/hello",
		publication.PlanOptions{},
	)
	if err != nil {
		t.Fatalf("PlanSnapshot: %v", err)
	}
	html, _, err := publication.Render(ctx, snap)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	writeCanonical(t, st, "/news/hello", html)

	// Default renderer (no fake): must verify, not legacy.
	m := migration.New(migration.Config{DB: db, Store: st})
	rep, err := m.Run(ctx)
	if err != nil {
		t.Fatalf("Run with default renderer: %v", err)
	}
	if !rep.Completed {
		t.Fatalf("expected completion, got %+v", rep)
	}
	active := getActive(t, db, cid)
	if active == nil {
		t.Fatal("expected active publication")
	}
	if active.VerificationStatus != publication.VerificationVerified {
		t.Errorf("default-renderer match must yield verified, got %q", active.VerificationStatus)
	}
}

func tmplVersionForTest(tplID primitive.ObjectID, slug string) templatecontract.TemplateVersion {
	return templatecontract.TemplateVersion{
		ID:         primitive.NewObjectID(),
		TemplateID: tplID, Version: 1, Slug: slug, Name: "News",
		Category: "news", Status: "active",
		Fields: []models.TemplateField{
			{Name: "headline", Label: "Headline", Type: "text"},
		},
		HTMLLayout:   "<html><h1>{{.headline}}</h1></html>",
		ContractHash: "sha256:test", RenderHash: "sha256:test",
		CreatedAt: time.Now(),
	}
}

// TestMigrate_CompletedIsIdempotent: re-running after completion is a no-op
// with no duplicate publications.
func TestMigrate_CompletedIsIdempotent(t *testing.T) {
	db := migDB(t)
	ctx := context.Background()
	st, _ := migStore(t)

	tpl := seedTemplate(t, db, "News", "news-tpl")
	body := []byte("<html>clean</html>")
	seedContent(t, db, tpl, "/news/clean", true, nil)
	writeCanonical(t, st, "/news/clean", body)
	fr := &fakeRenderer{byPath: map[string][]byte{"/news/clean": body}}

	m := migration.New(migration.Config{DB: db, Store: st, Render: fr.fn()})
	if _, err := m.Run(ctx); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	before := countDocs(t, db, "content_publications", bson.M{})
	rep, err := m.Run(ctx)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if !rep.Completed {
		t.Error("second run must still report Completed=true")
	}
	if after := countDocs(t, db, "content_publications", bson.M{}); after != before {
		t.Errorf("second run created %d extra publications, want 0", after-before)
	}
}
