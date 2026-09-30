package publication_test

// Task 10 (recovery scanner) red-green tests.
//
// Scope (plan Task 10; spec §17.5, §17.6, §35):
//   - active DB + missing canonical is rebuilt from immutable bytes (WARN);
//   - active DB + mismatched canonical: bad bytes quarantined BEFORE Restore,
//     canonical rebuilt, P0;
//   - active DB + missing immutable: P0, degraded, NO guessed replacement;
//   - legacy_unverified active pages keep serving, never quarantined;
//   - migration running + canonical-with-no-active is reported, not
//     quarantined; only completed permits orphan quarantine; quarantine never
//     invents an active Publication;
//   - stale stage (>15min) marked failed + aborted; stale .next-* cleaned;
//     held publish locks skipped;
//   - .previous-* + DB old active (Task 8 ErrStopAfterRename fixture) is
//     restart repair, not failure; .previous-* + DB new active is cleanup;
//   - .unpublish-backup-* restored when DB active, removed when unpublished;
//   - Inspect hard failure (unreadable sidecar) is a P0, not clean.
//
// DB note: these tests REQUIRE the Task 0 replica-set fixture
// (MONGODB_URI + test-named DATABASE_NAME); skips are not green evidence.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/database"
	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/storage"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"
	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type recAudit struct {
	action string
	fields map[string]any
}

type recFixture struct {
	db     *database.DB
	repo   *publication.Repository
	store  storage.Store
	root   string
	sc     *publication.Scanner
	mu     sync.Mutex
	alerts []publication.Alert
	audits []recAudit
}

func newRecFixture(t *testing.T, mutate func(*publication.ScannerOptions)) *recFixture {
	t.Helper()
	db, _ := testutil.MustConnectTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.EnsureProductIndexes(ctx); err != nil {
		t.Fatalf("EnsureProductIndexes: %v", err)
	}
	root := t.TempDir()
	store := storage.NewFilesystemStore(root)
	f := &recFixture{db: db, repo: publication.NewRepository(db, nil), store: store, root: root}
	opts := publication.ScannerOptions{
		QuarantineDir: filepath.Join(root, "quarantine"),
		Now:           time.Now,
		Audit: func(_ context.Context, action string, fields map[string]any) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.audits = append(f.audits, recAudit{action: action, fields: fields})
		},
		Alert: func(a publication.Alert) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.alerts = append(f.alerts, a)
		},
	}
	if mutate != nil {
		mutate(&opts)
	}
	f.sc = publication.NewScanner(db, f.repo, store, opts)
	return f
}

func (f *recFixture) alertCodes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.alerts))
	for _, a := range f.alerts {
		out = append(out, a.Code)
	}
	return out
}

func (f *recFixture) hasAlert(code string) bool {
	for _, c := range f.alertCodes() {
		if c == code {
			return true
		}
	}
	return false
}

func shaHexStr(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func seedRecContent(t *testing.T, db *database.DB, fullPath string) primitive.ObjectID {
	t.Helper()
	id := primitive.NewObjectID()
	_, err := db.Collection("content").InsertOne(context.Background(), bson.M{
		"_id": id, "title": "Recovery page", "slug": "recovery-page",
		"full_path": fullPath, "published": false,
		"created_at": time.Now(), "updated_at": time.Now(),
	})
	if err != nil {
		t.Fatalf("seed content: %v", err)
	}
	return id
}

// recSeedActive stages immutable bytes, activates the record, and optionally
// writes the canonical file. It returns the publication and the raw bytes.
func recSeedActive(t *testing.T, f *recFixture, contentID primitive.ObjectID, fullPath string, html []byte, ver publication.VerificationStatus) *publication.Publication {
	t.Helper()
	ctx := context.Background()
	pub := &publication.Publication{
		ContentID: contentID, ContentVersion: 1, TemplateVersionID: primitive.NewObjectID(),
		FullPath: fullPath, ContentHash: "sha256:" + shaHexStr(html),
		VerificationStatus: ver, LogicalPublishedAt: time.Now().UTC().Truncate(time.Millisecond),
	}
	if err := f.repo.InsertStaged(ctx, pub); err != nil {
		t.Fatalf("InsertStaged: %v", err)
	}
	staged, err := f.store.Stage(ctx, storage.StageRequest{
		ContentID: contentID, PublicationID: pub.ID, CanonicalPath: fullPath, HTML: html,
	})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	_ = staged
	if _, err := f.db.Collection(publication.CollectionPublications).UpdateOne(ctx,
		bson.M{"_id": pub.ID},
		bson.M{"$set": bson.M{
			"verification_status": string(ver),
			"storage_state":       string(publication.StoragePresent),
			"storage_path":        f.store.ImmutablePath(contentID, pub.ID),
		}}); err != nil {
		t.Fatalf("mark verified/present: %v", err)
	}
	pub.VerificationStatus = ver
	if err := f.repo.ActivateCAS(ctx, contentID, pub.ID, nil); err != nil {
		t.Fatalf("ActivateCAS: %v", err)
	}
	return pub
}

func recWriteCanonical(t *testing.T, f *recFixture, fullPath string, html []byte) {
	t.Helper()
	p, err := f.store.CanonicalFilePath(fullPath)
	if err != nil {
		t.Fatalf("CanonicalFilePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, html, 0o644); err != nil {
		t.Fatalf("write canonical: %v", err)
	}
}

func recReadCanonical(t *testing.T, f *recFixture, fullPath string) []byte {
	t.Helper()
	p, err := f.store.CanonicalFilePath(fullPath)
	if err != nil {
		t.Fatalf("CanonicalFilePath: %v", err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read canonical: %v", err)
	}
	return b
}

func recCanonicalExists(f *recFixture, fullPath string) bool {
	p, err := f.store.CanonicalFilePath(fullPath)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

func setMigrationState(t *testing.T, db *database.DB, state string) {
	t.Helper()
	ctx := context.Background()
	_, err := db.Collection("system_migrations").UpdateOne(ctx,
		bson.M{"key": publication.MigrationKeyPublicationModelV1},
		bson.M{"$set": bson.M{
			"key":        publication.MigrationKeyPublicationModelV1,
			"status":     state,
			"updated_at": time.Now(),
		}},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		t.Fatalf("set migration state: %v", err)
	}
}

// seedRecTemplate inserts a minimal active template version for saga-driven
// crash tests and returns (templateID, versionID).
func seedRecTemplate(t *testing.T, db *database.DB) (primitive.ObjectID, primitive.ObjectID) {
	t.Helper()
	tplID := primitive.NewObjectID()
	tvID := primitive.NewObjectID()
	_, err := db.Collection("template_versions").InsertOne(context.Background(), bson.M{
		"_id": tvID, "template_id": tplID, "version": int64(1),
		"slug": "financial-news", "name": "Financial News", "category": "news",
		"status": templatecontract.StatusActive,
		"fields": []models.TemplateField{
			{Name: "headline", Label: "Headline", Type: "text", Required: false},
		},
		"html_layout":   `<html><h1>{{.headline}}</h1></html>`,
		"contract_hash": "sha256:contract", "render_hash": "sha256:render",
		"created_at": time.Now(),
	})
	if err != nil {
		t.Fatalf("seed template version: %v", err)
	}
	return tplID, tvID
}

// seedRecContentFull inserts a saga-publishable content document.
func seedRecContentFull(t *testing.T, db *database.DB, tplID primitive.ObjectID, fullPath string, version int64, data map[string]any) primitive.ObjectID {
	t.Helper()
	id := primitive.NewObjectID()
	_, err := db.Collection("content").InsertOne(context.Background(), bson.M{
		"_id": id, "template_id": tplID, "template_name": "Financial News",
		"title": "Recovery saga page", "slug": "recovery-saga",
		"folder_path": "/news", "full_path": fullPath,
		"current_version": version, "data": data,
		"published": false, "created_at": time.Now(), "updated_at": time.Now(),
	})
	if err != nil {
		t.Fatalf("seed content: %v", err)
	}
	return id
}

func isCrashStop(err error) bool { return errors.Is(err, publication.ErrStopAfterRename) }

// TestRecovery_ActiveMissingCanonicalRebuilt: active + immutable present but
// no canonical file is rebuilt from immutable bytes with a WARN (not P0).
func TestRecovery_ActiveMissingCanonicalRebuilt(t *testing.T) {
	f := newRecFixture(t, nil)
	ctx := context.Background()
	contentID := seedRecContent(t, f.db, "/news/rebuilt")
	html := []byte("<html>authoritative bytes</html>")
	recSeedActive(t, f, contentID, "/news/rebuilt", html, publication.VerificationVerified)

	if recCanonicalExists(f, "/news/rebuilt") {
		t.Fatal("setup: canonical must be absent before scan")
	}
	rpt, err := f.sc.ScanOnce(ctx)
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if rpt.CanonicalRebuilt != 1 {
		t.Errorf("CanonicalRebuilt = %d, want 1", rpt.CanonicalRebuilt)
	}
	if got := string(recReadCanonical(t, f, "/news/rebuilt")); got != string(html) {
		t.Errorf("rebuilt canonical = %q, want %q", got, html)
	}
	if f.hasAlert(publication.CodeImmutableMissing) {
		t.Error("rebuild from retained bytes must not raise IMMUTABLE_MISSING")
	}
	active, err := f.repo.GetActive(ctx, contentID)
	if err != nil || active == nil {
		t.Fatalf("active must survive the scan: %v %v", active, err)
	}
}

// TestRecovery_ActiveMismatchQuarantineAndRebuild: a mismatched canonical is
// preserved to quarantine BEFORE Restore replaces it (Task 6 Restore does not
// quarantine), then rebuilt; P0 is raised.
func TestRecovery_ActiveMismatchQuarantineAndRebuild(t *testing.T) {
	f := newRecFixture(t, nil)
	ctx := context.Background()
	contentID := seedRecContent(t, f.db, "/news/mismatch")
	good := []byte("<html>good bytes</html>")
	bad := []byte("<html>attacker bytes</html>")
	recSeedActive(t, f, contentID, "/news/mismatch", good, publication.VerificationVerified)
	recWriteCanonical(t, f, "/news/mismatch", bad)

	rpt, err := f.sc.ScanOnce(ctx)
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if rpt.CanonicalMismatchRepaired != 1 {
		t.Errorf("CanonicalMismatchRepaired = %d, want 1", rpt.CanonicalMismatchRepaired)
	}
	if got := string(recReadCanonical(t, f, "/news/mismatch")); got != string(good) {
		t.Errorf("canonical after repair = %q, want %q", got, good)
	}
	if len(rpt.Quarantined) != 1 {
		t.Fatalf("Quarantined records = %d, want 1 (bad bytes preserved)", len(rpt.Quarantined))
	}
	qb, err := os.ReadFile(rpt.Quarantined[0].QuarantinePath)
	if err != nil {
		t.Fatalf("read quarantined bytes: %v", err)
	}
	if string(qb) != string(bad) {
		t.Errorf("quarantined bytes = %q, want the displaced bad bytes %q", qb, bad)
	}
	if !f.hasAlert(publication.CodeCanonicalMismatch) {
		t.Errorf("expected P0 %s, got %v", publication.CodeCanonicalMismatch, f.alertCodes())
	}
}

// TestRecovery_ActiveMissingImmutableP0: no immutable object means no repair
// guess: P0, degraded, and no canonical file is created.
func TestRecovery_ActiveMissingImmutableP0(t *testing.T) {
	f := newRecFixture(t, nil)
	ctx := context.Background()
	contentID := seedRecContent(t, f.db, "/news/nobytes")
	html := []byte("<html>lost bytes</html>")
	pub := recSeedActive(t, f, contentID, "/news/nobytes", html, publication.VerificationVerified)
	// Simulate total object loss: remove the immutable object after activation.
	if err := f.store.DeleteImmutable(ctx, contentID, pub.ID); err != nil {
		t.Fatalf("DeleteImmutable: %v", err)
	}

	rpt, err := f.sc.ScanOnce(ctx)
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if rpt.ImmutableMissingP0 != 1 {
		t.Errorf("ImmutableMissingP0 = %d, want 1", rpt.ImmutableMissingP0)
	}
	if !rpt.Degraded || !f.sc.Degraded() {
		t.Error("site must be marked degraded when active bytes are unrecoverable")
	}
	if !f.hasAlert(publication.CodeImmutableMissing) {
		t.Errorf("expected P0 %s, got %v", publication.CodeImmutableMissing, f.alertCodes())
	}
	if recCanonicalExists(f, "/news/nobytes") {
		t.Error("scanner must not guess a replacement canonical without immutable bytes")
	}
}

// TestRecovery_LegacyUnverifiedServed: a legacy_unverified active page keeps
// serving and is never quarantined, before and after migration completion.
func TestRecovery_LegacyUnverifiedServed(t *testing.T) {
	for _, state := range []string{publication.MigrationRunning, publication.MigrationCompleted} {
		t.Run("migration="+state, func(t *testing.T) {
			f := newRecFixture(t, nil)
			ctx := context.Background()
			setMigrationState(t, f.db, state)
			contentID := seedRecContent(t, f.db, "/news/legacy")
			html := []byte("<html>legacy bytes</html>")
			recSeedActive(t, f, contentID, "/news/legacy", html, publication.VerificationLegacyUnverified)
			recWriteCanonical(t, f, "/news/legacy", html)

			rpt, err := f.sc.ScanOnce(ctx)
			if err != nil {
				t.Fatalf("ScanOnce: %v", err)
			}
			if got := string(recReadCanonical(t, f, "/news/legacy")); got != string(html) {
				t.Errorf("legacy canonical changed to %q", got)
			}
			if rpt.OrphanQuarantined != 0 || len(rpt.Quarantined) != 0 {
				t.Errorf("legacy_unverified must never be quarantined: %+v", rpt)
			}
			if f.hasAlert(publication.CodeImmutableMissing) || f.hasAlert(publication.CodeCanonicalMismatch) {
				t.Errorf("legacy served page must not raise data-loss alerts: %v", f.alertCodes())
			}
			active, _ := f.repo.GetActive(ctx, contentID)
			if active == nil || !active.IsServable() {
				t.Error("legacy_unverified active must stay servable")
			}
		})
	}
}

// TestRecovery_MigrationRunningReportsOrphan: canonical exists + no active
// while migration is running (or flag absent) is reported, never quarantined.
func TestRecovery_MigrationRunningReportsOrphan(t *testing.T) {
	for _, state := range []string{"", publication.MigrationRunning} {
		t.Run("migration="+state, func(t *testing.T) {
			f := newRecFixture(t, nil)
			ctx := context.Background()
			if state != "" {
				setMigrationState(t, f.db, state)
			}
			contentID := seedRecContent(t, f.db, "/news/orphan")
			_ = contentID
			recWriteCanonical(t, f, "/news/orphan", []byte("<html>orphan</html>"))

			rpt, err := f.sc.ScanOnce(ctx)
			if err != nil {
				t.Fatalf("ScanOnce: %v", err)
			}
			if rpt.OrphanReported == 0 {
				t.Errorf("running migration must report the orphan, got %+v", rpt)
			}
			if rpt.OrphanQuarantined != 0 {
				t.Errorf("running migration must not quarantine, got %+v", rpt)
			}
			if !recCanonicalExists(f, "/news/orphan") {
				t.Error("reported orphan file must be left in place")
			}
		})
	}
}

// TestRecovery_MigrationCompletedQuarantinesOrphan: only a completed
// migration permits orphan quarantine, and quarantine never invents an
// active Publication from files.
func TestRecovery_MigrationCompletedQuarantinesOrphan(t *testing.T) {
	f := newRecFixture(t, nil)
	ctx := context.Background()
	setMigrationState(t, f.db, publication.MigrationCompleted)
	seedRecContent(t, f.db, "/news/orphan-gone")
	recWriteCanonical(t, f, "/news/orphan-gone", []byte("<html>orphan</html>"))

	rpt, err := f.sc.ScanOnce(ctx)
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if rpt.OrphanQuarantined != 1 {
		t.Errorf("OrphanQuarantined = %d, want 1", rpt.OrphanQuarantined)
	}
	if recCanonicalExists(f, "/news/orphan-gone") {
		t.Error("quarantined orphan must no longer be served")
	}
	if len(rpt.Quarantined) != 1 {
		t.Fatalf("expected 1 quarantine record, got %+v", rpt.Quarantined)
	}
	var n int64
	n, err = f.db.Collection(publication.CollectionPublications).CountDocuments(ctx, bson.M{})
	if err != nil {
		t.Fatalf("count publications: %v", err)
	}
	if n != 0 {
		t.Errorf("quarantine must never infer an active Publication; found %d records", n)
	}
}

// TestRecovery_StaleStageMarkedFailed: a staged record older than the stage
// timeout is marked failed and its object aborted; fresh staged is untouched.
func TestRecovery_StaleStageMarkedFailed(t *testing.T) {
	f := newRecFixture(t, func(o *publication.ScannerOptions) {
		o.StageTimeout = 15 * time.Minute
	})
	ctx := context.Background()
	contentID := seedRecContent(t, f.db, "/news/stale-stage")

	stale := &publication.Publication{
		ContentID: contentID, ContentVersion: 1, TemplateVersionID: primitive.NewObjectID(),
		FullPath: "/news/stale-stage", ContentHash: "sha256:" + strings.Repeat("b", 64),
		VerificationStatus: publication.VerificationPending,
		LogicalPublishedAt: time.Now().UTC().Add(-time.Hour),
		CreatedAt:          time.Now().Add(-time.Hour),
	}
	if err := f.repo.InsertStaged(ctx, stale); err != nil {
		t.Fatalf("InsertStaged stale: %v", err)
	}
	staleObj, err := f.store.Stage(ctx, storage.StageRequest{
		ContentID: contentID, PublicationID: stale.ID,
		CanonicalPath: "/news/stale-stage", HTML: []byte("<html>stale</html>"),
	})
	if err != nil {
		t.Fatalf("Stage stale: %v", err)
	}

	fresh := &publication.Publication{
		ContentID: contentID, ContentVersion: 1, TemplateVersionID: primitive.NewObjectID(),
		FullPath: "/news/stale-stage", ContentHash: "sha256:" + strings.Repeat("c", 64),
		VerificationStatus: publication.VerificationPending,
		LogicalPublishedAt: time.Now().UTC(),
	}
	if err := f.repo.InsertStaged(ctx, fresh); err != nil {
		t.Fatalf("InsertStaged fresh: %v", err)
	}

	rpt, err := f.sc.ScanOnce(ctx)
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if rpt.StaleStagedFailed != 1 {
		t.Errorf("StaleStagedFailed = %d, want 1", rpt.StaleStagedFailed)
	}
	got, err := f.repo.GetByID(ctx, stale.ID)
	if err != nil {
		t.Fatalf("GetByID stale: %v", err)
	}
	if got.Status != publication.StatusFailed {
		t.Errorf("stale staged status = %q, want failed", got.Status)
	}
	if err := f.store.Verify(ctx, staleObj); err == nil {
		t.Error("stale staged object must be aborted")
	}
	gotFresh, err := f.repo.GetByID(ctx, fresh.ID)
	if err != nil {
		t.Fatalf("GetByID fresh: %v", err)
	}
	if gotFresh.Status != publication.StatusStaged {
		t.Errorf("fresh staged status = %q, want staged (untouched)", gotFresh.Status)
	}
}

// TestRecovery_StaleNextCleaned: orphan .next-* files older than the stage
// timeout are deleted; fresh ones are kept; held publish locks are skipped.
func TestRecovery_StaleNextCleaned(t *testing.T) {
	f := newRecFixture(t, func(o *publication.ScannerOptions) {
		o.StageTimeout = 15 * time.Minute
	})
	ctx := context.Background()
	contentID := seedRecContent(t, f.db, "/news/next-clean")
	html := []byte("<html>live</html>")
	recSeedActive(t, f, contentID, "/news/next-clean", html, publication.VerificationVerified)
	recWriteCanonical(t, f, "/news/next-clean", html)

	canon, err := f.store.CanonicalFilePath("/news/next-clean")
	if err != nil {
		t.Fatalf("CanonicalFilePath: %v", err)
	}
	staleNext := canon + ".next-" + primitive.NewObjectID().Hex()
	if err := os.WriteFile(staleNext, []byte("partial"), 0o644); err != nil {
		t.Fatalf("write stale next: %v", err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(staleNext, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	freshNext := canon + ".next-" + primitive.NewObjectID().Hex()
	if err := os.WriteFile(freshNext, []byte("partial"), 0o644); err != nil {
		t.Fatalf("write fresh next: %v", err)
	}

	rpt, err := f.sc.ScanOnce(ctx)
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if rpt.StaleNextCleaned != 1 {
		t.Errorf("StaleNextCleaned = %d, want 1", rpt.StaleNextCleaned)
	}
	if _, err := os.Stat(staleNext); !os.IsNotExist(err) {
		t.Errorf("stale .next must be deleted: %v", err)
	}
	if _, err := os.Stat(freshNext); err != nil {
		t.Errorf("fresh .next must be kept: %v", err)
	}
}

// TestRecovery_CrashWindowPreviousRestored replays Task 8's crash fixture
// verbatim: ErrStopAfterRename leaves new canonical + .previous-{oldID} on
// disk with OLD active in DB. The scanner must repair it, not fail.
func TestRecovery_CrashWindowPreviousRestored(t *testing.T) {
	f := newRecFixture(t, nil)
	ctx := context.Background()
	tplID, tvID := seedRecTemplate(t, f.db)
	contentID := seedRecContentFull(t, f.db, tplID, "/news/crash-window", 1, map[string]any{"headline": "v1 live"})

	good := publication.NewService(f.db, publication.NewRepository(f.db, nil), f.store, publication.Options{})
	first, err := good.Publish(ctx, publication.PublishRequest{
		ContentID: contentID, ContentVersion: 1, TemplateVersionID: tvID,
	})
	if err != nil {
		t.Fatalf("setup publish v1: %v", err)
	}
	if _, err := f.db.Collection("content").UpdateOne(ctx, bson.M{"_id": contentID}, bson.M{"$set": bson.M{
		"current_version": int64(2), "data": map[string]any{"headline": "v2 candidate"},
	}}); err != nil {
		t.Fatalf("bump to v2: %v", err)
	}
	crasher := publication.NewService(f.db, publication.NewRepository(f.db, nil), f.store, publication.Options{
		Faults: publication.Faults{BeforeCommit: func(context.Context) error {
			return publication.ErrStopAfterRename
		}},
	})
	_, err = crasher.Publish(ctx, publication.PublishRequest{
		ContentID: contentID, ContentVersion: 2, TemplateVersionID: tvID,
	})
	if err == nil || !isCrashStop(err) {
		t.Fatalf("fixture publish must stop after rename, got %v", err)
	}

	rpt, err := f.sc.ScanOnce(ctx)
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if rpt.PreviousRestored != 1 {
		t.Errorf("PreviousRestored = %d, want 1 (restart repair, not failure)", rpt.PreviousRestored)
	}
	// Canonical serves the DB-active (old) bytes again.
	active, err := f.repo.GetActive(ctx, contentID)
	if err != nil || active == nil || active.ID != first.PublicationID {
		t.Fatalf("old active must stay active: %v %v", active, err)
	}
	if got := string(recReadCanonical(t, f, "/news/crash-window")); !strings.Contains(got, "v1 live") {
		t.Errorf("canonical must serve v1 after repair, got %q", got)
	}
	// The displaced uncommitted v2 bytes are preserved in quarantine.
	foundV2 := false
	for _, q := range rpt.Quarantined {
		qb, rerr := os.ReadFile(q.QuarantinePath)
		if rerr != nil {
			t.Fatalf("read quarantine: %v", rerr)
		}
		if strings.Contains(string(qb), "v2 candidate") {
			foundV2 = true
		}
	}
	if !foundV2 {
		t.Errorf("displaced uncommitted v2 bytes must be quarantined, got %+v", rpt.Quarantined)
	}
	// No leftover .previous sidecar may hide the repaired state.
	info, err := f.store.Inspect(ctx, "/news/crash-window")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	for _, sc := range info.Sidecars {
		if sc.Kind == storage.SidecarPrevious {
			t.Errorf("stale .previous after repair: %s", sc.Path)
		}
	}
}

// TestRecovery_PreviousCleanedAfterCommit: .previous-* + DB new active means
// the commit landed and only cleanup was missed: confirm hash, delete backup.
func TestRecovery_PreviousCleanedAfterCommit(t *testing.T) {
	f := newRecFixture(t, nil)
	ctx := context.Background()
	contentID := seedRecContent(t, f.db, "/news/prev-clean")
	v1 := []byte("<html>v1</html>")
	v2 := []byte("<html>v2</html>")
	old := recSeedActive(t, f, contentID, "/news/prev-clean", v1, publication.VerificationVerified)
	recWriteCanonical(t, f, "/news/prev-clean", v1)
	cur := &publication.Publication{
		ContentID: contentID, ContentVersion: 2, TemplateVersionID: primitive.NewObjectID(),
		FullPath: "/news/prev-clean", ContentHash: "sha256:" + shaHexStr(v2),
		VerificationStatus: publication.VerificationVerified,
		LogicalPublishedAt: time.Now().UTC(),
	}
	if err := f.repo.InsertStaged(ctx, cur); err != nil {
		t.Fatalf("InsertStaged v2: %v", err)
	}
	staged, err := f.store.Stage(ctx, storage.StageRequest{
		ContentID: contentID, PublicationID: cur.ID, CanonicalPath: "/news/prev-clean", HTML: v2,
	})
	if err != nil {
		t.Fatalf("Stage v2: %v", err)
	}
	if _, err := f.db.Collection(publication.CollectionPublications).UpdateOne(ctx,
		bson.M{"_id": cur.ID},
		bson.M{"$set": bson.M{"verification_status": "verified", "storage_state": "present"}}); err != nil {
		t.Fatalf("mark v2: %v", err)
	}
	if err := f.store.ActivateWithPrevious(ctx, staged, "/news/prev-clean", &old.ID); err != nil {
		t.Fatalf("ActivateWithPrevious: %v", err)
	}
	// Commit landed, cleanup missed.
	if err := f.repo.ActivateCAS(ctx, contentID, cur.ID, &old.ID); err != nil {
		t.Fatalf("ActivateCAS v2: %v", err)
	}

	rpt, err := f.sc.ScanOnce(ctx)
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if rpt.PreviousCleaned != 1 {
		t.Errorf("PreviousCleaned = %d, want 1", rpt.PreviousCleaned)
	}
	if rpt.PreviousRestored != 0 || rpt.CanonicalMismatchRepaired != 0 {
		t.Errorf("healthy post-commit state needs no repair: %+v", rpt)
	}
	if got := string(recReadCanonical(t, f, "/news/prev-clean")); got != string(v2) {
		t.Errorf("canonical must stay v2, got %q", got)
	}
}

// TestRecovery_UnpublishBackupRestored: backup + DB still active (crash
// between backup rename and transaction) restores the backup to canonical.
func TestRecovery_UnpublishBackupRestored(t *testing.T) {
	f := newRecFixture(t, nil)
	ctx := context.Background()
	contentID := seedRecContent(t, f.db, "/news/backup-restore")
	html := []byte("<html>still live</html>")
	pub := recSeedActive(t, f, contentID, "/news/backup-restore", html, publication.VerificationVerified)
	recWriteCanonical(t, f, "/news/backup-restore", html)
	// Simulate the unpublish crash window: canonical renamed away, DB active.
	if _, err := f.store.StageUnpublishBackup(ctx, "/news/backup-restore", pub.ID); err != nil {
		t.Fatalf("StageUnpublishBackup: %v", err)
	}

	rpt, err := f.sc.ScanOnce(ctx)
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if rpt.BackupRestored != 1 {
		t.Errorf("BackupRestored = %d, want 1", rpt.BackupRestored)
	}
	if got := string(recReadCanonical(t, f, "/news/backup-restore")); got != string(html) {
		t.Errorf("canonical must be restored to live bytes, got %q", got)
	}
}

// TestRecovery_UnpublishBackupCleaned: backup + DB unpublished (commit
// landed, cleanup missed) removes the backup once canonical is absent.
func TestRecovery_UnpublishBackupCleaned(t *testing.T) {
	f := newRecFixture(t, nil)
	ctx := context.Background()
	contentID := seedRecContent(t, f.db, "/news/backup-clean")
	html := []byte("<html>bye</html>")
	pub := recSeedActive(t, f, contentID, "/news/backup-clean", html, publication.VerificationVerified)
	recWriteCanonical(t, f, "/news/backup-clean", html)
	if _, err := f.store.StageUnpublishBackup(ctx, "/news/backup-clean", pub.ID); err != nil {
		t.Fatalf("StageUnpublishBackup: %v", err)
	}
	if _, err := f.repo.UnpublishCAS(ctx, contentID, nil); err != nil {
		t.Fatalf("UnpublishCAS: %v", err)
	}

	rpt, err := f.sc.ScanOnce(ctx)
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if rpt.BackupCleaned != 1 {
		t.Errorf("BackupCleaned = %d, want 1", rpt.BackupCleaned)
	}
	if recCanonicalExists(f, "/news/backup-clean") {
		t.Error("unpublished page must not serve a canonical file")
	}
}

// TestRecovery_InspectHardErrorIsP0: an unreadable sidecar makes Inspect fail
// hard; the scanner raises P0 and skips the content instead of calling it
// clean or repairing blindly.
func TestRecovery_InspectHardErrorIsP0(t *testing.T) {
	f := newRecFixture(t, nil)
	ctx := context.Background()
	contentID := seedRecContent(t, f.db, "/news/inspect-fail")
	html := []byte("<html>live</html>")
	recSeedActive(t, f, contentID, "/news/inspect-fail", html, publication.VerificationVerified)
	recWriteCanonical(t, f, "/news/inspect-fail", html)

	canon, err := f.store.CanonicalFilePath("/news/inspect-fail")
	if err != nil {
		t.Fatalf("CanonicalFilePath: %v", err)
	}
	// Dangling symlink with a valid sidecar name: Inspect's re-read fails
	// hard on every platform, even as root (chmod-based faults do not).
	ghost := canon + ".previous-" + primitive.NewObjectID().Hex()
	if err := os.Symlink(filepath.Join("no-such-dir", "gone.html"), ghost); err != nil {
		t.Fatalf("symlink sidecar: %v", err)
	}
	defer os.Remove(ghost)

	rpt, err := f.sc.ScanOnce(ctx)
	if err != nil {
		t.Fatalf("ScanOnce itself must survive an inspect failure: %v", err)
	}
	if rpt.InspectErrors != 1 {
		t.Errorf("InspectErrors = %d, want 1", rpt.InspectErrors)
	}
	if !f.hasAlert(publication.CodeInspectFailed) {
		t.Errorf("expected P0 %s, got %v", publication.CodeInspectFailed, f.alertCodes())
	}
	if rpt.CanonicalRebuilt != 0 || rpt.CanonicalMismatchRepaired != 0 {
		t.Errorf("content with failed inspection must not be repaired blindly: %+v", rpt)
	}
	if got := string(recReadCanonical(t, f, "/news/inspect-fail")); got != string(html) {
		t.Errorf("canonical must be untouched, got %q", got)
	}
}

// TestRecovery_MultipleActiveIsP0: two active records stop auto-repair for
// that content; the scanner alerts P0 and changes nothing.
func TestRecovery_MultipleActiveIsP0(t *testing.T) {
	f := newRecFixture(t, nil)
	ctx := context.Background()
	// The partial unique active index would prevent the fixture; drop it for
	// this test only (test-local, recreated by the next fixture setup).
	_, _ = f.db.Collection(publication.CollectionPublications).Indexes().DropOne(ctx, "content_publications_active_unique")
	contentID := seedRecContent(t, f.db, "/news/two-active")
	html := []byte("<html>live</html>")
	a := recSeedActive(t, f, contentID, "/news/two-active", html, publication.VerificationVerified)
	b := &publication.Publication{
		ContentID: contentID, ContentVersion: 1, TemplateVersionID: primitive.NewObjectID(),
		FullPath: "/news/two-active", ContentHash: "sha256:" + shaHexStr(html),
		VerificationStatus: publication.VerificationVerified,
		LogicalPublishedAt: time.Now().UTC(),
	}
	if err := f.repo.InsertStaged(ctx, b); err != nil {
		t.Fatalf("InsertStaged B: %v", err)
	}
	if _, err := f.db.Collection(publication.CollectionPublications).UpdateOne(ctx,
		bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"status": string(publication.StatusActive)}}); err != nil {
		t.Fatalf("force second active: %v", err)
	}
	_ = a
	recWriteCanonical(t, f, "/news/two-active", []byte("<html>intruder</html>"))

	rpt, err := f.sc.ScanOnce(ctx)
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if rpt.MultipleActiveP0 != 1 {
		t.Errorf("MultipleActiveP0 = %d, want 1", rpt.MultipleActiveP0)
	}
	if !f.hasAlert(publication.CodeMultipleActive) {
		t.Errorf("expected P0 %s, got %v", publication.CodeMultipleActive, f.alertCodes())
	}
	if got := string(recReadCanonical(t, f, "/news/two-active")); got != "<html>intruder</html>" {
		t.Errorf("multiple-active content must not be auto-repaired, got %q", got)
	}
}

// TestRecovery_SkipsHeldPublishLock: a publish in-flight inside its page lock
// (blocked pre-commit hook) makes the scanner skip that content.
func TestRecovery_SkipsHeldPublishLock(t *testing.T) {
	f := newRecFixture(t, nil)
	ctx := context.Background()
	tplID, tvID := seedRecTemplate(t, f.db)
	contentID := seedRecContentFull(t, f.db, tplID, "/news/locked", 1, map[string]any{"headline": "v1"})

	good := publication.NewService(f.db, publication.NewRepository(f.db, nil), f.store, publication.Options{})
	if _, err := good.Publish(ctx, publication.PublishRequest{
		ContentID: contentID, ContentVersion: 1, TemplateVersionID: tvID,
	}); err != nil {
		t.Fatalf("setup publish: %v", err)
	}
	if _, err := f.db.Collection("content").UpdateOne(ctx, bson.M{"_id": contentID}, bson.M{"$set": bson.M{
		"current_version": int64(2), "data": map[string]any{"headline": "v2"},
	}}); err != nil {
		t.Fatalf("bump: %v", err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	gated := publication.NewService(f.db, publication.NewRepository(f.db, nil), f.store, publication.Options{
		Faults: publication.Faults{BeforeCommit: func(context.Context) error {
			once.Do(func() { close(entered) })
			<-release
			return nil
		}},
	})
	type pubOutcome struct {
		res publication.PublicationResult
		err error
	}
	done := make(chan pubOutcome, 1)
	go func() {
		res, err := gated.Publish(ctx, publication.PublishRequest{
			ContentID: contentID, ContentVersion: 2, TemplateVersionID: tvID,
		})
		done <- pubOutcome{res: res, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("gated publish never reached the pre-commit hook")
	}

	rpt, err := f.sc.ScanOnce(ctx)
	if err != nil {
		t.Fatalf("ScanOnce during held lock: %v", err)
	}
	if rpt.ContentsSkippedLocked == 0 {
		t.Errorf("scanner must skip content with a held publish lock: %+v", rpt)
	}

	close(release)
	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("gated publish: %v", out.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("gated publish never finished")
	}
}

// TestRecovery_ScanOnceSweepsPoisonedOutbox wires the Task 9 handoff: the
// scanner's outbox sweep poisons undeliverable rows instead of retrying them
// forever (poisoning rule owned by Task 10, delivery by Task 9).
func TestRecovery_ScanOnceSweepsPoisonedOutbox(t *testing.T) {
	f := newRecFixture(t, nil)
	ctx := context.Background()
	pubID := primitive.NewObjectID()
	ob := publication.NewOutbox(f.db)
	if err := ob.InsertUnique(ctx, publication.EventPublished, pubID, map[string]any{"k": "v"}); err != nil {
		t.Fatalf("InsertUnique: %v", err)
	}
	if _, err := f.db.Collection(publication.CollectionOutbox).UpdateOne(ctx,
		bson.M{"aggregate_id": pubID},
		bson.M{"$set": bson.M{"attempt": publication.DefaultOutboxMaxAttempts, "next_attempt_at": time.Now().Add(-time.Hour)}}); err != nil {
		t.Fatalf("age row: %v", err)
	}

	rpt, err := f.sc.ScanOnce(ctx)
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if rpt.Outbox.Poisoned != 1 {
		t.Errorf("Outbox.Poisoned = %d, want 1", rpt.Outbox.Poisoned)
	}
	if !f.hasAlert(publication.CodeOutboxPoisoned) {
		t.Errorf("expected P0 %s, got %v", publication.CodeOutboxPoisoned, f.alertCodes())
	}
}
