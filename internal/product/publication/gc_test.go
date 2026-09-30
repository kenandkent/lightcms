package publication_test

// Task 10 (retention GC) red-green tests.
//
// Scope (plan Task 10; spec §17.5 row "failed/superseded object 超过
// retention", §17.6, §18.1):
//   - active and pinned objects are never GC'd;
//   - superseded/unpublished default to 90 days (superseded_at /
//     unpublished_at, falling back to created_at);
//   - failed/staged default to 7 days (created_at);
//   - quarantine files default to 30 days (file modtime);
//   - storage deletion precedes storage_state=deleted: a failing object
//     delete leaves metadata untouched;
//   - publication metadata rows are retained (only storage_state flips);
//   - outbox poisoning rule (Task 10 owns it; Task 9 reserved `failed`):
//     undeliverable rows go failed with P0 after MaxAttempts or MaxAge;
//     terminal (delivered/failed) rows expire after terminal retention.
//
// DB note: replica-set fixture required; skips are not green evidence.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/storage"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// gcSeedHistory creates a non-active publication with explicit lifecycle,
// timestamps, pin, storage state, and (optionally) staged immutable bytes.
func gcSeedHistory(t *testing.T, f *recFixture, contentID primitive.ObjectID, fullPath string, html []byte, status publication.Status, created, edge time.Time, pinned bool) *publication.Publication {
	t.Helper()
	ctx := context.Background()
	pub := &publication.Publication{
		ContentID: contentID, ContentVersion: 1, TemplateVersionID: primitive.NewObjectID(),
		FullPath: fullPath, ContentHash: "sha256:" + shaHexStr(html),
		VerificationStatus: publication.VerificationVerified,
		LogicalPublishedAt: created, CreatedAt: created, Pinned: pinned,
	}
	if err := f.repo.InsertStaged(ctx, pub); err != nil {
		t.Fatalf("InsertStaged: %v", err)
	}
	if _, err := f.store.Stage(ctx, storage.StageRequest{
		ContentID: contentID, PublicationID: pub.ID, CanonicalPath: fullPath, HTML: html,
	}); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	set := bson.M{
		"status": string(status), "storage_state": string(publication.StoragePresent),
		"storage_path": f.store.ImmutablePath(contentID, pub.ID),
	}
	switch status {
	case publication.StatusSuperseded:
		set["superseded_at"] = edge
	case publication.StatusUnpublished:
		set["unpublished_at"] = edge
	}
	if _, err := f.db.Collection(publication.CollectionPublications).UpdateOne(ctx,
		bson.M{"_id": pub.ID}, bson.M{"$set": set}); err != nil {
		t.Fatalf("set lifecycle: %v", err)
	}
	return pub
}

func gcStorageState(t *testing.T, f *recFixture, id primitive.ObjectID) publication.StorageState {
	t.Helper()
	got, err := f.repo.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	return got.StorageState
}

func gcImmutableExists(f *recFixture, contentID, pubID primitive.ObjectID) bool {
	_, err := os.Stat(f.store.ImmutablePath(contentID, pubID))
	return err == nil
}

// TestGC_RetentionMatrix proves the §17.6 matrix: old superseded /
// unpublished / failed objects are deleted and marked storage_state=deleted
// with metadata retained; young and pinned objects are untouched.
func TestGC_RetentionMatrix(t *testing.T) {
	f := newRecFixture(t, nil)
	ctx := context.Background()
	now := time.Now()

	contentID := seedRecContent(t, f.db, "/news/gc-matrix")
	old := now.Add(-91 * 24 * time.Hour)

	oldSup := gcSeedHistory(t, f, contentID, "/news/gc-matrix", []byte("old-sup"), publication.StatusSuperseded, old, old, false)
	youngSup := gcSeedHistory(t, f, contentID, "/news/gc-matrix", []byte("young-sup"), publication.StatusSuperseded, now.Add(-10*24*time.Hour), now.Add(-10*24*time.Hour), false)
	pinnedSup := gcSeedHistory(t, f, contentID, "/news/gc-matrix", []byte("pinned-sup"), publication.StatusSuperseded, old, old, true)
	oldUnpub := gcSeedHistory(t, f, contentID, "/news/gc-matrix", []byte("old-unpub"), publication.StatusUnpublished, old, old, false)
	oldFailed := gcSeedHistory(t, f, contentID, "/news/gc-matrix", []byte("old-failed"), publication.StatusFailed, now.Add(-8*24*time.Hour), time.Time{}, false)
	youngFailed := gcSeedHistory(t, f, contentID, "/news/gc-matrix", []byte("young-failed"), publication.StatusFailed, now.Add(-24*time.Hour), time.Time{}, false)

	rpt, err := f.sc.SweepRetention(ctx)
	if err != nil {
		t.Fatalf("SweepRetention: %v", err)
	}
	if rpt.ObjectsDeleted != 3 {
		t.Errorf("ObjectsDeleted = %d, want 3 (old superseded + old unpublished + old failed)", rpt.ObjectsDeleted)
	}

	for id, want := range map[primitive.ObjectID]publication.StorageState{
		oldSup.ID:      publication.StorageDeleted,
		oldUnpub.ID:    publication.StorageDeleted,
		oldFailed.ID:   publication.StorageDeleted,
		youngSup.ID:    publication.StoragePresent,
		pinnedSup.ID:   publication.StoragePresent,
		youngFailed.ID: publication.StoragePresent,
	} {
		if got := gcStorageState(t, f, id); got != want {
			t.Errorf("publication %s storage_state = %q, want %q", id.Hex(), got, want)
		}
	}
	for _, id := range []primitive.ObjectID{oldSup.ID, oldUnpub.ID, oldFailed.ID} {
		if gcImmutableExists(f, contentID, id) {
			t.Errorf("expired object %s must be deleted from storage", id.Hex())
		}
		// Metadata is retained: the row still exists with lifecycle intact.
		if _, err := f.repo.GetByID(ctx, id); err != nil {
			t.Errorf("expired metadata %s must be retained: %v", id.Hex(), err)
		}
	}
	for _, id := range []primitive.ObjectID{youngSup.ID, pinnedSup.ID, youngFailed.ID} {
		if !gcImmutableExists(f, contentID, id) {
			t.Errorf("retained object %s must stay in storage", id.Hex())
		}
	}
}

// TestGC_ActiveNeverCollected: an ancient active publication (and an ancient
// pinned unpublished one) keeps its bytes regardless of retention age.
func TestGC_ActiveNeverCollected(t *testing.T) {
	f := newRecFixture(t, nil)
	ctx := context.Background()
	contentID := seedRecContent(t, f.db, "/news/gc-active")
	ancient := time.Now().Add(-400 * 24 * time.Hour)
	html := []byte("<html>ancient live</html>")
	pub := &publication.Publication{
		ContentID: contentID, ContentVersion: 1, TemplateVersionID: primitive.NewObjectID(),
		FullPath: "/news/gc-active", ContentHash: "sha256:" + shaHexStr(html),
		VerificationStatus: publication.VerificationVerified,
		LogicalPublishedAt: ancient, CreatedAt: ancient,
	}
	if err := f.repo.InsertStaged(ctx, pub); err != nil {
		t.Fatalf("InsertStaged: %v", err)
	}
	if _, err := f.store.Stage(ctx, storage.StageRequest{
		ContentID: contentID, PublicationID: pub.ID, CanonicalPath: "/news/gc-active", HTML: html,
	}); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if err := f.repo.ActivateCAS(ctx, contentID, pub.ID, nil); err != nil {
		t.Fatalf("ActivateCAS: %v", err)
	}
	if _, err := f.db.Collection(publication.CollectionPublications).UpdateOne(ctx,
		bson.M{"_id": pub.ID},
		bson.M{"$set": bson.M{"storage_state": "present", "created_at": ancient}}); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	rpt, err := f.sc.SweepRetention(ctx)
	if err != nil {
		t.Fatalf("SweepRetention: %v", err)
	}
	if rpt.ObjectsDeleted != 0 {
		t.Errorf("ObjectsDeleted = %d, want 0 (active is never collected)", rpt.ObjectsDeleted)
	}
	if !gcImmutableExists(f, contentID, pub.ID) {
		t.Error("active bytes must survive GC at any age")
	}
	if got := gcStorageState(t, f, pub.ID); got != publication.StoragePresent {
		t.Errorf("active storage_state = %q, want present", got)
	}
}

// failDeleteStore proves the §17.6 ordering invariant: storage deletion
// precedes storage_state=deleted, so a failing delete leaves metadata alone.
type failDeleteStore struct {
	storage.Store
	err error
}

func (s failDeleteStore) DeleteImmutable(_ context.Context, _ primitive.ObjectID, _ primitive.ObjectID) error {
	return s.err
}

// TestGC_StorageDeletionPrecedesStateFlip: when the object delete fails, the
// record keeps its storage_state (never deleted-first).
func TestGC_StorageDeletionPrecedesStateFlip(t *testing.T) {
	f := newRecFixture(t, nil)
	ctx := context.Background()
	contentID := seedRecContent(t, f.db, "/news/gc-order")
	old := time.Now().Add(-100 * 24 * time.Hour)
	victim := gcSeedHistory(t, f, contentID, "/news/gc-order", []byte("victim"), publication.StatusSuperseded, old, old, false)

	broken := failDeleteStore{Store: f.store, err: context.DeadlineExceeded}
	sc := publication.NewScanner(f.db, f.repo, broken, publication.ScannerOptions{
		QuarantineDir: filepath.Join(f.root, "quarantine"),
	})
	rpt, err := sc.SweepRetention(ctx)
	if err != nil {
		t.Fatalf("SweepRetention: %v", err)
	}
	if rpt.ObjectDeleteErrors != 1 {
		t.Errorf("ObjectDeleteErrors = %d, want 1", rpt.ObjectDeleteErrors)
	}
	if got := gcStorageState(t, f, victim.ID); got != publication.StoragePresent {
		t.Errorf("failed delete must not flip storage_state, got %q", got)
	}
	if !gcImmutableExists(f, contentID, victim.ID) {
		t.Error("object must still exist after a failed delete")
	}
}

// TestGC_QuarantineRetention: quarantine files older than 30 days are
// deleted; fresh ones are kept for forensics.
func TestGC_QuarantineRetention(t *testing.T) {
	f := newRecFixture(t, nil)
	ctx := context.Background()
	qdir := filepath.Join(f.root, "quarantine")
	if err := os.MkdirAll(qdir, 0o755); err != nil {
		t.Fatalf("mkdir quarantine: %v", err)
	}
	oldFile := filepath.Join(qdir, "old.html.q-1")
	newFile := filepath.Join(qdir, "new.html.q-2")
	if err := os.WriteFile(oldFile, []byte("old"), 0o644); err != nil {
		t.Fatalf("write old: %v", err)
	}
	if err := os.WriteFile(newFile, []byte("new"), 0o644); err != nil {
		t.Fatalf("write new: %v", err)
	}
	old := time.Now().Add(-31 * 24 * time.Hour)
	if err := os.Chtimes(oldFile, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	rpt, err := f.sc.SweepRetention(ctx)
	if err != nil {
		t.Fatalf("SweepRetention: %v", err)
	}
	if rpt.QuarantineFilesDeleted != 1 {
		t.Errorf("QuarantineFilesDeleted = %d, want 1", rpt.QuarantineFilesDeleted)
	}
	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Errorf("expired quarantine file must be deleted: %v", err)
	}
	if _, err := os.Stat(newFile); err != nil {
		t.Errorf("fresh quarantine file must be kept: %v", err)
	}
}

// TestGC_OutboxPoisoningRule defines the Task 10 poisoning rule deferred by
// Task 9: a pending row at MaxAttempts (or older than MaxAge) is terminally
// failed with P0 instead of retried forever; terminal rows expire after the
// message TTL.
func TestGC_OutboxPoisoningRule(t *testing.T) {
	f := newRecFixture(t, nil)
	ctx := context.Background()

	poisonAttempts := primitive.NewObjectID()
	poisonAge := primitive.NewObjectID()
	healthy := primitive.NewObjectID()
	ob := publication.NewOutbox(f.db)
	for _, id := range []primitive.ObjectID{poisonAttempts, poisonAge, healthy} {
		if err := ob.InsertUnique(ctx, publication.EventPublished, id, map[string]any{"k": "v"}); err != nil {
			t.Fatalf("InsertUnique: %v", err)
		}
	}
	if _, err := f.db.Collection(publication.CollectionOutbox).UpdateOne(ctx,
		bson.M{"aggregate_id": poisonAttempts},
		bson.M{"$set": bson.M{"attempt": publication.DefaultOutboxMaxAttempts, "next_attempt_at": time.Now().Add(-time.Hour)}}); err != nil {
		t.Fatalf("age attempts row: %v", err)
	}
	if _, err := f.db.Collection(publication.CollectionOutbox).UpdateOne(ctx,
		bson.M{"aggregate_id": poisonAge},
		bson.M{"$set": bson.M{"attempt": 3, "created_at": time.Now().Add(-8 * 24 * time.Hour)}}); err != nil {
		t.Fatalf("age old row: %v", err)
	}

	rpt, err := f.sc.SweepOutbox(ctx)
	if err != nil {
		t.Fatalf("SweepOutbox: %v", err)
	}
	if rpt.Poisoned != 2 {
		t.Errorf("Poisoned = %d, want 2 (max-attempts + max-age)", rpt.Poisoned)
	}
	for _, id := range []primitive.ObjectID{poisonAttempts, poisonAge} {
		var row bson.M
		if err := f.db.Collection(publication.CollectionOutbox).FindOne(ctx, bson.M{"aggregate_id": id}).Decode(&row); err != nil {
			t.Fatalf("load poisoned row: %v", err)
		}
		if row["state"] != publication.OutboxStateFailed {
			t.Errorf("poisoned row state = %v, want failed", row["state"])
		}
	}
	var healthyRow bson.M
	if err := f.db.Collection(publication.CollectionOutbox).FindOne(ctx, bson.M{"aggregate_id": healthy}).Decode(&healthyRow); err != nil {
		t.Fatalf("load healthy row: %v", err)
	}
	if healthyRow["state"] != publication.OutboxStatePending {
		t.Errorf("healthy row state = %v, want pending", healthyRow["state"])
	}
	if !f.hasAlert(publication.CodeOutboxPoisoned) {
		t.Errorf("expected P0 %s, got %v", publication.CodeOutboxPoisoned, f.alertCodes())
	}
}

// TestGC_OutboxMessageTTL: terminal (delivered/failed) rows older than the
// message TTL are deleted; recent terminal rows are kept. Receivers still
// dedupe on the stable event ID if a late duplicate is ever recreated.
func TestGC_OutboxMessageTTL(t *testing.T) {
	f := newRecFixture(t, nil)
	ctx := context.Background()
	oldDelivered := primitive.NewObjectID()
	newDelivered := primitive.NewObjectID()
	ob := publication.NewOutbox(f.db)
	for _, id := range []primitive.ObjectID{oldDelivered, newDelivered} {
		if err := ob.InsertUnique(ctx, publication.EventPublished, id, map[string]any{"k": "v"}); err != nil {
			t.Fatalf("InsertUnique: %v", err)
		}
		if _, err := f.db.Collection(publication.CollectionOutbox).UpdateOne(ctx,
			bson.M{"aggregate_id": id},
			bson.M{"$set": bson.M{"state": publication.OutboxStateDelivered, "delivered_at": time.Now()}}); err != nil {
			t.Fatalf("mark delivered: %v", err)
		}
	}
	if _, err := f.db.Collection(publication.CollectionOutbox).UpdateOne(ctx,
		bson.M{"aggregate_id": oldDelivered},
		bson.M{"$set": bson.M{
			"delivered_at": time.Now().Add(-100 * 24 * time.Hour),
			"created_at":   time.Now().Add(-100 * 24 * time.Hour),
		}}); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	rpt, err := f.sc.SweepOutbox(ctx)
	if err != nil {
		t.Fatalf("SweepOutbox: %v", err)
	}
	if rpt.ExpiredDeleted != 1 {
		t.Errorf("ExpiredDeleted = %d, want 1", rpt.ExpiredDeleted)
	}
	var row bson.M
	if err := f.db.Collection(publication.CollectionOutbox).FindOne(ctx, bson.M{"aggregate_id": oldDelivered}).Decode(&row); err == nil {
		t.Error("expired terminal row must be deleted")
	}
	if err := f.db.Collection(publication.CollectionOutbox).FindOne(ctx, bson.M{"aggregate_id": newDelivered}).Decode(&row); err != nil {
		t.Errorf("recent terminal row must be kept: %v", err)
	}
}
