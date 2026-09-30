package publication_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/database"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// failOutbox is an OutboxInserter that always fails, proving the repository
// performs Content + publication + outbox writes in ONE transaction: when the
// outbox insert fails, the lifecycle and Content projection roll back too.
type failOutbox struct{}

func (failOutbox) InsertUnique(_ context.Context, _ string, _ primitive.ObjectID, _ map[string]any) error {
	return errors.New("testutil: injected outbox failure")
}

func testRepo(t *testing.T) (*database.DB, *publication.Repository) {
	t.Helper()
	db, _ := testutil.MustConnectTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.EnsureProductIndexes(ctx); err != nil {
		t.Fatalf("EnsureProductIndexes: %v", err)
	}
	return db, publication.NewRepository(db, nil)
}

func seedContent(t *testing.T, db *database.DB) primitive.ObjectID {
	t.Helper()
	ctx := context.Background()
	id := primitive.NewObjectID()
	_, err := db.Collection("content").InsertOne(ctx, bson.M{
		"_id":         id,
		"template_id": primitive.NewObjectID(),
		"title":       "Test page",
		"slug":        "test-page",
		"full_path":   "/news/test-page",
		"published":   false,
		"created_at":  time.Now(),
		"updated_at":  time.Now(),
	})
	if err != nil {
		t.Fatalf("seed content: %v", err)
	}
	return id
}

func stagedPub(contentID primitive.ObjectID) *publication.Publication {
	return &publication.Publication{
		ContentID:          contentID,
		ContentVersion:     1,
		TemplateVersionID:  primitive.NewObjectID(),
		FullPath:           "/news/test-page",
		ContentHash:        "sha256:abc123",
		StorageProvider:    "filesystem",
		VerificationStatus: publication.VerificationVerified,
		LogicalPublishedAt: time.Now().Truncate(time.Millisecond).UTC(),
	}
}

func getContent(t *testing.T, db *database.DB, id primitive.ObjectID) bson.M {
	t.Helper()
	var doc bson.M
	if err := db.Collection("content").FindOne(context.Background(), bson.M{"_id": id}).Decode(&doc); err != nil {
		t.Fatalf("load content: %v", err)
	}
	return doc
}

func outboxCount(t *testing.T, db *database.DB, eventType string, pubID primitive.ObjectID) int64 {
	t.Helper()
	n, err := db.Collection("webhook_outbox").CountDocuments(context.Background(), bson.M{
		"event_type":   eventType,
		"aggregate_id": pubID,
	})
	if err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	return n
}

// TestActivateCAS covers the happy path plus the Content projection sync:
// activation flips old active -> superseded, staged -> active, and sets
// Content.Published=true with PublishedAt = logical_published_at.
func TestActivateCAS(t *testing.T) {
	db, repo := testRepo(t)
	ctx := context.Background()
	contentID := seedContent(t, db)

	if got, err := repo.GetActive(ctx, contentID); err != nil || got != nil {
		t.Fatalf("GetActive on fresh content = (%v, %v), want (nil, nil)", got, err)
	}

	a := stagedPub(contentID)
	if err := repo.InsertStaged(ctx, a); err != nil {
		t.Fatalf("InsertStaged A: %v", err)
	}
	if got, _ := repo.GetActive(ctx, contentID); got != nil {
		t.Fatalf("staged insert must not create an active publication, got %v", got.ID.Hex())
	}

	// First publish: no expected old active.
	if err := repo.ActivateCAS(ctx, contentID, a.ID, nil); err != nil {
		t.Fatalf("ActivateCAS first publish: %v", err)
	}
	active, err := repo.GetActive(ctx, contentID)
	if err != nil || active == nil || active.ID != a.ID {
		t.Fatalf("GetActive after activation = (%v, %v), want A", active, err)
	}
	if active.Status != publication.StatusActive {
		t.Errorf("active status = %q, want active", active.Status)
	}
	if active.ActivatedAt == nil {
		t.Error("activated_at must be set on activation")
	}
	doc := getContent(t, db, contentID)
	if doc["published"] != true {
		t.Errorf("Content.Published = %v, want true", doc["published"])
	}
	gotAt, ok := doc["published_at"].(primitive.DateTime)
	if !ok {
		t.Fatalf("Content.PublishedAt missing after activation: %v", doc["published_at"])
	}
	if gotAt.Time().UTC().Truncate(time.Millisecond).Compare(a.LogicalPublishedAt) != 0 {
		t.Errorf("Content.PublishedAt = %v, want logical_published_at %v", gotAt.Time().UTC(), a.LogicalPublishedAt)
	}
	if v, _ := doc["has_unpublished_changes"].(bool); v {
		t.Error("HasUnpublishedChanges must be false after activation")
	}
	if n := outboxCount(t, db, "content.publish", a.ID); n != 1 {
		t.Errorf("expected 1 content.publish outbox row for A, got %d", n)
	}

	// Second publish with correct expected old active.
	b := stagedPub(contentID)
	b.ContentVersion = 2
	b.ContentHash = "sha256:def456"
	if err := repo.InsertStaged(ctx, b); err != nil {
		t.Fatalf("InsertStaged B: %v", err)
	}
	if err := repo.ActivateCAS(ctx, contentID, b.ID, &a.ID); err != nil {
		t.Fatalf("ActivateCAS B with expected A: %v", err)
	}
	active, _ = repo.GetActive(ctx, contentID)
	if active.ID != b.ID {
		t.Fatalf("active = %v, want B", active.ID.Hex())
	}
	old, err := repo.GetByID(ctx, a.ID)
	if err != nil || old.Status != publication.StatusSuperseded {
		t.Fatalf("old active A = (%v, %v), want superseded", old, err)
	}
	if old.SupersededAt == nil {
		t.Error("superseded_at must be set on the old active")
	}

	hist, err := repo.ListHistory(ctx, contentID)
	if err != nil || len(hist) != 2 {
		t.Fatalf("ListHistory = (%d items, %v), want 2 items", len(hist), err)
	}
}

// TestActivateCASDoubleActivateConflict: a stale expected-old-active ID yields
// PUBLICATION_CONFLICT and changes neither publication.
func TestActivateCASDoubleActivateConflict(t *testing.T) {
	db, repo := testRepo(t)
	ctx := context.Background()
	contentID := seedContent(t, db)

	a := stagedPub(contentID)
	if err := repo.InsertStaged(ctx, a); err != nil {
		t.Fatalf("InsertStaged A: %v", err)
	}
	if err := repo.ActivateCAS(ctx, contentID, a.ID, nil); err != nil {
		t.Fatalf("ActivateCAS A: %v", err)
	}

	b := stagedPub(contentID)
	b.ContentVersion = 2
	b.ContentHash = "sha256:second"
	if err := repo.InsertStaged(ctx, b); err != nil {
		t.Fatalf("InsertStaged B: %v", err)
	}
	c := stagedPub(contentID)
	c.ContentVersion = 3
	c.ContentHash = "sha256:third"
	if err := repo.InsertStaged(ctx, c); err != nil {
		t.Fatalf("InsertStaged C: %v", err)
	}

	// B wins the race with the correct expected ID.
	if err := repo.ActivateCAS(ctx, contentID, b.ID, &a.ID); err != nil {
		t.Fatalf("ActivateCAS B: %v", err)
	}
	// C retries with the now-stale expected ID (A). Must conflict.
	stale := a.ID
	if err := repo.ActivateCAS(ctx, contentID, c.ID, &stale); !publication.IsConflict(err) {
		t.Fatalf("stale ActivateCAS = %v, want PUBLICATION_CONFLICT", err)
	}
	// Neither record changed: B still active, C still staged, A still superseded.
	if active, _ := repo.GetActive(ctx, contentID); active.ID != b.ID {
		t.Errorf("active changed by conflicted activation: %v", active.ID.Hex())
	}
	if got, _ := repo.GetByID(ctx, c.ID); got.Status != publication.StatusStaged {
		t.Errorf("loser C status = %q, want staged", got.Status)
	}
	if got, _ := repo.GetByID(ctx, a.ID); got.Status != publication.StatusSuperseded {
		t.Errorf("A status = %q, want superseded", got.Status)
	}
	doc := getContent(t, db, contentID)
	if doc["published"] != true {
		t.Error("Content.Published must stay true after conflicted activation")
	}
}

// TestActivateCASRejectsTerminal: failed, superseded, and unpublished records
// may never become active; rollback creates a new record (Task 8).
func TestActivateCASRejectsTerminal(t *testing.T) {
	db, repo := testRepo(t)
	ctx := context.Background()
	contentID := seedContent(t, db)

	a := stagedPub(contentID)
	if err := repo.InsertStaged(ctx, a); err != nil {
		t.Fatalf("InsertStaged A: %v", err)
	}
	if err := repo.MarkFailed(ctx, a.ID, "stage write error"); err != nil {
		t.Fatalf("MarkFailed A: %v", err)
	}
	if err := repo.ActivateCAS(ctx, contentID, a.ID, nil); err == nil {
		t.Fatal("activating a failed publication must fail")
	} else if publication.IsConflict(err) {
		t.Fatalf("failed->active must be an invalid-transition error, not conflict: %v", err)
	}
	if got, _ := repo.GetByID(ctx, a.ID); got.Status != publication.StatusFailed {
		t.Errorf("A status = %q, want failed", got.Status)
	}
	if got, _ := repo.GetActive(ctx, contentID); got != nil {
		t.Errorf("failed activation must not create an active pointer: %v", got.ID.Hex())
	}
}

// TestUnpublishCAS covers the one-transaction sync (lifecycle + Content
// projection + outbox) and the idempotent second-unpublish rule.
func TestUnpublishCAS(t *testing.T) {
	db, repo := testRepo(t)
	ctx := context.Background()
	contentID := seedContent(t, db)

	a := stagedPub(contentID)
	if err := repo.InsertStaged(ctx, a); err != nil {
		t.Fatalf("InsertStaged: %v", err)
	}
	if err := repo.ActivateCAS(ctx, contentID, a.ID, nil); err != nil {
		t.Fatalf("ActivateCAS: %v", err)
	}

	did, err := repo.UnpublishCAS(ctx, contentID, nil)
	if err != nil || !did {
		t.Fatalf("UnpublishCAS = (%v, %v), want (true, nil)", did, err)
	}
	got, err := repo.GetByID(ctx, a.ID)
	if err != nil || got.Status != publication.StatusUnpublished {
		t.Fatalf("A after unpublish = (%v, %v), want unpublished", got, err)
	}
	if got.UnpublishedAt == nil {
		t.Error("unpublished_at must be set")
	}
	if active, _ := repo.GetActive(ctx, contentID); active != nil {
		t.Errorf("GetActive after unpublish = %v, want nil", active.ID.Hex())
	}
	doc := getContent(t, db, contentID)
	if doc["published"] != false {
		t.Errorf("Content.Published = %v, want false", doc["published"])
	}
	if _, exists := doc["published_at"]; exists {
		t.Errorf("Content.PublishedAt must be nil after unpublish, got %v", doc["published_at"])
	}
	if n := outboxCount(t, db, "content.unpublish", a.ID); n != 1 {
		t.Errorf("expected 1 content.unpublish outbox row, got %d", n)
	}

	// Second unpublish: idempotent success, no new event.
	did, err = repo.UnpublishCAS(ctx, contentID, nil)
	if err != nil || did {
		t.Fatalf("second UnpublishCAS = (%v, %v), want (false, nil)", did, err)
	}
	if n := outboxCount(t, db, "content.unpublish", a.ID); n != 1 {
		t.Errorf("second unpublish must not insert a new event, count = %d", n)
	}

	// An unpublished record may never become active again.
	if err := repo.ActivateCAS(ctx, contentID, a.ID, nil); err == nil {
		t.Fatal("activating an unpublished publication must fail")
	}
}

// TestUnpublishCASConflict: wrong expected active ID leaves everything
// untouched.
func TestUnpublishCASConflict(t *testing.T) {
	db, repo := testRepo(t)
	ctx := context.Background()
	contentID := seedContent(t, db)

	a := stagedPub(contentID)
	if err := repo.InsertStaged(ctx, a); err != nil {
		t.Fatalf("InsertStaged: %v", err)
	}
	if err := repo.ActivateCAS(ctx, contentID, a.ID, nil); err != nil {
		t.Fatalf("ActivateCAS: %v", err)
	}
	other := primitive.NewObjectID()
	if _, err := repo.UnpublishCAS(ctx, contentID, &other); !publication.IsConflict(err) {
		t.Fatalf("UnpublishCAS with wrong expected ID = %v, want PUBLICATION_CONFLICT", err)
	}
	if active, _ := repo.GetActive(ctx, contentID); active == nil || active.ID != a.ID {
		t.Fatalf("conflicted unpublish changed the active pointer: %v", active)
	}
}

// TestUnpublishCASTransactional: when the outbox insert fails, the lifecycle
// and Content projection must roll back together (single transaction).
func TestUnpublishCASTransactional(t *testing.T) {
	db, _ := testRepo(t)
	ctx := context.Background()
	repo := publication.NewRepository(db, failOutbox{})
	contentID := seedContent(t, db)

	// Seed with a working repo (failOutbox would break activation too).
	good := publication.NewRepository(db, nil)
	a := stagedPub(contentID)
	if err := good.InsertStaged(ctx, a); err != nil {
		t.Fatalf("InsertStaged: %v", err)
	}
	if err := good.ActivateCAS(ctx, contentID, a.ID, nil); err != nil {
		t.Fatalf("ActivateCAS: %v", err)
	}

	if _, err := repo.UnpublishCAS(ctx, contentID, nil); err == nil {
		t.Fatal("expected outbox failure to abort UnpublishCAS")
	}
	if active, _ := good.GetActive(ctx, contentID); active == nil || active.ID != a.ID {
		t.Fatal("failed unpublish must leave the old active in place")
	}
	doc := getContent(t, db, contentID)
	if doc["published"] != true {
		t.Error("failed unpublish must leave Content.Published=true")
	}
}

// TestActivateCASTransactional: same atomicity proof for the activate path.
func TestActivateCASTransactional(t *testing.T) {
	db, _ := testRepo(t)
	ctx := context.Background()
	contentID := seedContent(t, db)

	good := publication.NewRepository(db, nil)
	a := stagedPub(contentID)
	if err := good.InsertStaged(ctx, a); err != nil {
		t.Fatalf("InsertStaged: %v", err)
	}

	bad := publication.NewRepository(db, failOutbox{})
	if err := bad.ActivateCAS(ctx, contentID, a.ID, nil); err == nil {
		t.Fatal("expected outbox failure to abort ActivateCAS")
	}
	if got, _ := good.GetByID(ctx, a.ID); got.Status != publication.StatusStaged {
		t.Errorf("aborted activation changed status to %q, want staged", got.Status)
	}
	if active, _ := good.GetActive(ctx, contentID); active != nil {
		t.Errorf("aborted activation created an active pointer: %v", active.ID.Hex())
	}
	doc := getContent(t, db, contentID)
	if doc["published"] != false {
		t.Error("aborted activation must leave Content.Published=false")
	}
}

// TestMarkFailedOutbox: staged -> failed records the reason and inserts the
// publication.failed event in the same transaction.
func TestMarkFailedOutbox(t *testing.T) {
	db, repo := testRepo(t)
	ctx := context.Background()
	contentID := seedContent(t, db)

	a := stagedPub(contentID)
	if err := repo.InsertStaged(ctx, a); err != nil {
		t.Fatalf("InsertStaged: %v", err)
	}
	if err := repo.MarkFailed(ctx, a.ID, "verify hash mismatch"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	got, _ := repo.GetByID(ctx, a.ID)
	if got.Status != publication.StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if got.FailureReason != "verify hash mismatch" {
		t.Errorf("failure_reason = %q", got.FailureReason)
	}
	if n := outboxCount(t, db, "publication.failed", a.ID); n != 1 {
		t.Errorf("expected 1 publication.failed outbox row, got %d", n)
	}
	// Idempotent repeat.
	if err := repo.MarkFailed(ctx, a.ID, "verify hash mismatch"); err != nil {
		t.Errorf("second MarkFailed = %v, want nil (idempotent)", err)
	}
}

// TestLegacyUnverifiedServable: a migration-approved legacy_unverified record
// can become active and stays servable (never scanner-quarantined).
func TestLegacyUnverifiedServable(t *testing.T) {
	db, repo := testRepo(t)
	ctx := context.Background()
	contentID := seedContent(t, db)

	a := stagedPub(contentID)
	a.VerificationStatus = publication.VerificationLegacyUnverified
	if err := repo.InsertStaged(ctx, a); err != nil {
		t.Fatalf("InsertStaged legacy: %v", err)
	}
	if err := repo.ActivateCAS(ctx, contentID, a.ID, nil); err != nil {
		t.Fatalf("ActivateCAS legacy_unverified: %v", err)
	}
	active, err := repo.GetActive(ctx, contentID)
	if err != nil || active == nil || active.ID != a.ID {
		t.Fatalf("GetActive = (%v, %v), want legacy A", active, err)
	}
	if !active.IsServable() {
		t.Error("active legacy_unverified must stay servable")
	}
}
