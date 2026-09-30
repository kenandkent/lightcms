package publication_test

// Task 17B coverage-gap tests round 3: sweep transport errors, orphan
// families with sidecars, quarantine failure, rebuild refusal (external).

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/storage"
	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestCoverGapSweepTransportErrors(t *testing.T) {
	bdb := testutil.MustConnectBrokenDB(t)
	ctx := context.Background()
	bstore := storage.NewFilesystemStore(t.TempDir())
	bsvc := publication.NewScanner(bdb, publication.NewRepository(bdb, nil), bstore, publication.ScannerOptions{})
	if _, err := bsvc.SweepRetention(ctx); err == nil {
		t.Fatalf("SweepRetention broken: want error")
	}
	if _, err := bsvc.SweepOutbox(ctx); err == nil {
		t.Fatalf("SweepOutbox broken: want error")
	}
	if _, err := bsvc.ScanOnce(ctx); err == nil {
		t.Fatalf("ScanOnce broken: want error")
	}
	if _, err := bsvc.MigrationState(ctx); err == nil {
		t.Fatalf("MigrationState broken: want error")
	}
}

func TestCoverGapOrphanFamilyWithSidecars(t *testing.T) {
	f := newRecFixture(t, func(o *publication.ScannerOptions) {
		o.StageTimeout = 15 * time.Minute
	})
	ctx := context.Background()
	// Orphan canonical + previous + unpublish-backup sidecars + stale .next,
	// with migration completed so the family is quarantined.
	canon, err := f.store.CanonicalFilePath("/news/orphan-fam")
	if err != nil {
		t.Fatalf("CanonicalFilePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(canon), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(canon, []byte("<html>orphan</html>"), 0o644); err != nil {
		t.Fatalf("write canon: %v", err)
	}
	pid := primitive.NewObjectID()
	for _, side := range []string{
		canon + ".previous-" + pid.Hex(),
		canon + ".unpublish-backup-" + pid.Hex(),
	} {
		if err := os.WriteFile(side, []byte("side"), 0o644); err != nil {
			t.Fatalf("write sidecar: %v", err)
		}
	}
	staleNext := canon + ".next-" + primitive.NewObjectID().Hex()
	if err := os.WriteFile(staleNext, []byte("partial"), 0o644); err != nil {
		t.Fatalf("write next: %v", err)
	}
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(staleNext, old, old)
	if err := writeMigrationCompleted(t, f); err != nil {
		t.Fatalf("migration flag: %v", err)
	}

	rpt, err := f.sc.ScanOnce(ctx)
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if rpt.OrphanQuarantined < 1 {
		t.Fatalf("OrphanQuarantined = %d: %+v", rpt.OrphanQuarantined, rpt)
	}
	if _, err := os.Stat(canon); !os.IsNotExist(err) {
		t.Fatalf("orphan canonical must be quarantined")
	}
	if _, err := os.Stat(staleNext); !os.IsNotExist(err) {
		t.Fatalf("stale next must be cleaned")
	}

	// Quarantine directory blocked by a file → quarantine fails loudly (P0).
	f2 := newRecFixture(t, func(o *publication.ScannerOptions) {
		o.StageTimeout = 15 * time.Minute
	})
	canon2, err := f2.store.CanonicalFilePath("/news/orphan-blocked")
	if err != nil {
		t.Fatalf("CanonicalFilePath2: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(canon2), 0o755); err != nil {
		t.Fatalf("mkdir2: %v", err)
	}
	if err := os.WriteFile(canon2, []byte("<html>orphan</html>"), 0o644); err != nil {
		t.Fatalf("write canon2: %v", err)
	}
	qdir := filepath.Join(f2.root, "quarantine")
	if err := os.WriteFile(qdir, []byte("block"), 0o644); err != nil {
		t.Fatalf("block qdir: %v", err)
	}
	if err := writeMigrationCompleted(t, f2); err != nil {
		t.Fatalf("migration flag2: %v", err)
	}
	rpt2, err := f2.sc.ScanOnce(ctx)
	if err != nil {
		t.Fatalf("ScanOnce2: %v", err)
	}
	if rpt2.RepairErrors < 1 {
		t.Fatalf("blocked quarantine must record repair errors: %+v", rpt2)
	}
}

func writeMigrationCompleted(t *testing.T, f *recFixture) error {
	t.Helper()
	_, err := f.db.Collection(publication.CollectionMigrations).InsertOne(context.Background(), map[string]any{
		"_id": publication.MigrationKeyPublicationModelV1, "status": "completed",
	})
	return err
}

func TestCoverGapRebuildRefused(t *testing.T) {
	f := newRecFixture(t, nil)
	ctx := context.Background()
	contentID := seedRecContent(t, f.db, "/news/refused")
	html := []byte("<html>live</html>")
	pub := recSeedActive(t, f, contentID, "/news/refused", html, publication.VerificationVerified)
	// Canonical missing; immutable object replaced by a directory so the
	// rebuild read fails with IO (not NotFound/Mismatch) → WARN refusal.
	imm := f.store.ImmutablePath(contentID, pub.ID)
	if err := os.Remove(imm); err != nil {
		t.Fatalf("remove immutable: %v", err)
	}
	if err := os.MkdirAll(imm, 0o755); err != nil {
		t.Fatalf("mkdir immutable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(imm, "child"), []byte("x"), 0o644); err != nil {
		t.Fatalf("imm child: %v", err)
	}
	rpt, err := f.sc.ScanOnce(ctx)
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if rpt.RepairErrors < 1 {
		t.Fatalf("refused rebuild must record repair errors: %+v", rpt)
	}
	if f.sc.Degraded() {
		t.Fatalf("refusal is WARN, must not latch degraded")
	}
}

func TestCoverGapSnapshotCopiesAndNilCtx(t *testing.T) {
	content := gapSnapshotContent()
	tv := gapSnapshotTemplate()
	tv.Fields = []models.TemplateField{{
		Name: "u", Type: "url",
		Validation: models.FieldValidation{AllowedProtocols: []string{"https", "http"}},
	}}
	snap, err := publication.PlanSnapshot(content, 2, tv, primitive.NewObjectID(),
		time.Now(), "https://example.com/news/gap", publication.PlanOptions{
			DependencySnapshot: map[string]any{"a": "b"},
		})
	if err != nil {
		t.Fatalf("PlanSnapshot with fields: %v", err)
	}
	if len(snap.Fields) != 1 || len(snap.Fields[0].Validation.AllowedProtocols) != 2 {
		t.Fatalf("fields not deep-copied: %+v", snap.Fields)
	}
	// Nil data + nil maps.
	tv2 := gapSnapshotTemplate()
	emptyContent := gapSnapshotContent()
	emptyContent.Data = nil
	snap2, err := publication.PlanSnapshot(emptyContent, 1, tv2, primitive.NewObjectID(),
		time.Now(), "https://example.com/x", publication.PlanOptions{})
	if err != nil {
		t.Fatalf("PlanSnapshot nil data: %v", err)
	}
	if snap2.Data == nil {
		t.Fatalf("nil data not initialized")
	}
	// Nil context passes checkCtx (nil-safe) and renders.
	snap3 := gapRenderSnap()
	if _, _, err := publication.Render(nil, snap3); err != nil {
		t.Fatalf("Render nil ctx: %v", err)
	}
}

func TestCoverGapWorkerRunBacklogError(t *testing.T) {
	bdb := testutil.MustConnectBrokenDB(t)
	w := publication.NewOutboxWorker(bdb, func(ctx context.Context, e, id string, p map[string]any) error {
		return nil
	}, publication.WorkerOptions{PollInterval: 20 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	select {
	case <-done:
		t.Fatalf("Run returned before cancel")
	case <-time.After(300 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("broken Run did not stop")
	}
}

func TestCoverGapRollbackExpectedConflict(t *testing.T) {
	s := newSagaSetup(t, nil, nil, publication.Options{})
	ctx := context.Background()
	tplID, tvID := seedSagaTemplateVersion(t, s.db, "", true)
	cid := seedSagaContent(t, s.db, tplID, "/news/gap-rb-exp", 1, nil)
	res, err := s.svc.Publish(ctx, publication.PublishRequest{
		ContentID: cid, ContentVersion: 1, TemplateVersionID: tvID,
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	ghost := primitive.NewObjectID()
	if _, err := s.svc.Rollback(ctx, publication.RollbackRequest{
		ContentID: cid, SourcePublicationID: res.PublicationID, ExpectedActiveID: &ghost,
	}); err == nil {
		t.Fatalf("Rollback stale expected: want error")
	}
	// Unknown source publication.
	if _, err := s.svc.Rollback(ctx, publication.RollbackRequest{
		ContentID: cid, SourcePublicationID: primitive.NewObjectID(),
	}); err == nil {
		t.Fatalf("Rollback unknown source: want error")
	}
}
