package services

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/product/idempotency"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/publicurl"
	"github.com/jonradoff/lightcms/v7/internal/product/storage"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"
	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Task 16D red-green: background publish under a stable operation key —
// a retry replays without a duplicate Publication/outbox row, and a retry
// after a terminal pre-activation failure allocates a new attempt (still
// zero new rows when the failure persists).
func Test16D_StableKeyRetryReplays(t *testing.T) {
	db, _ := testutil.MustConnectTestDB(t)
	ctx := context.Background()
	if err := db.EnsureProductIndexes(ctx); err != nil {
		t.Fatalf("EnsureProductIndexes: %v", err)
	}

	tplID := primitive.NewObjectID()
	if _, err := db.Collection("templates").InsertOne(ctx, bson.M{
		"_id": tplID, "name": "News", "slug": "news16d", "current_version": int64(1),
		"created_at": time.Now(), "updated_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed template: %v", err)
	}
	if _, err := db.Collection("template_versions").InsertOne(ctx, bson.M{
		"_id": primitive.NewObjectID(), "template_id": tplID, "version": int64(1),
		"slug": "news16d", "name": "News", "status": templatecontract.StatusActive,
		"fields": []bson.M{}, "html_layout": "<html><body>{{.headline}}</body></html>",
		"contract_hash": "sha256:c", "render_hash": "sha256:r",
		"created_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed template version: %v", err)
	}
	contentID := primitive.NewObjectID()
	if _, err := db.Collection("content").InsertOne(ctx, bson.M{
		"_id": contentID, "template_id": tplID, "template_name": "News",
		"title": "T", "slug": "t16d", "folder_path": "/news", "full_path": "/news/t16d",
		"canonical_full_path": "/news/t16d", "path_scope": "live", "path_active": true,
		"current_version": int64(1), "data": bson.M{"headline": "hi"},
		"published": false, "created_at": time.Now(), "updated_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed content: %v", err)
	}

	root := t.TempDir()
	store := storage.NewFilesystemStore(root)
	repo := publication.NewRepository(db, nil)
	idem, err := idempotency.NewService(db, idempotency.Options{})
	if err != nil {
		t.Fatalf("idempotency: %v", err)
	}
	base, _ := url.Parse("http://localhost:8082")
	resolver, err := publicurl.NewResolver(base)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	saga := publication.NewService(db, repo, store, publication.Options{
		Idem: idem, URLs: resolver, BuildSHA: "test-16d",
	})
	// Wire the package seams exactly like production main.go; restore the
	// unwired state afterwards so sibling tests keep legacy behavior.
	prevSaga, prevIdem := legacyPublicationSaga, internalIdem
	SetPublicationPublisher(saga)
	SetInternalIdempotency(idem)
	defer func() {
		legacyPublicationSaga, internalIdem = prevSaga, prevIdem
	}()

	cs := NewContentService(db)
	key := SchedulerOpKey(contentID, 1)

	if err := cs.PublishInternal(ctx, contentID, "scheduler", "/internal/scheduler/publish", key); err != nil {
		t.Fatalf("first PublishInternal: %v", err)
	}
	// Retry with the SAME stable key (lost-response/tick resume).
	if err := cs.PublishInternal(ctx, contentID, "scheduler", "/internal/scheduler/publish", key); err != nil {
		t.Fatalf("retry PublishInternal: %v", err)
	}

	pubCount, err := db.Collection("content_publications").CountDocuments(ctx, bson.M{"content_id": contentID})
	if err != nil {
		t.Fatalf("count publications: %v", err)
	}
	if pubCount != 1 {
		t.Fatalf("stable-key retry must not duplicate publications (count=%d)", pubCount)
	}
	var pub struct {
		ID primitive.ObjectID `bson:"_id"`
	}
	if err := db.Collection("content_publications").FindOne(ctx, bson.M{"content_id": contentID}).Decode(&pub); err != nil {
		t.Fatalf("read publication: %v", err)
	}
	outboxCount, err := db.Collection("webhook_outbox").CountDocuments(ctx, bson.M{"aggregate_id": pub.ID})
	if err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if outboxCount != 1 {
		t.Fatalf("stable-key retry must not duplicate outbox rows (count=%d)", outboxCount)
	}

	// An uncertain cutover is NOT proof of terminal pre-activation failure.
	// Background wrappers must preserve the same attempt for scanner/takeover.
	SetPublicationPublisher(publication.NewService(db, repo, store, publication.Options{Idem: idem, URLs: resolver, Faults: publication.Faults{BeforeCommit: func(context.Context) error { return publication.ErrStopAfterRename }}}))
	crashKey := "scheduler/crash/" + contentID.Hex()
	if err := cs.PublishInternal(ctx, contentID, "scheduler", "/internal/scheduler/publish", crashKey); err == nil {
		t.Fatal("crash fixture did not run")
	}
	var crashed idempotency.Operation
	if err := db.FindOne(ctx, idempotency.CollectionName, bson.M{"key": crashKey}, &crashed); err != nil {
		t.Fatal(err)
	}
	if crashed.AttemptState != idempotency.AttemptProcessing {
		t.Fatalf("uncertain cutover incorrectly declared terminal: %s", crashed.AttemptState)
	}
	SetPublicationPublisher(saga)
	if _, err := db.Collection(idempotency.CollectionName).UpdateOne(ctx, bson.M{"_id": crashed.ID}, bson.M{"$set": bson.M{"processing_expires_at": time.Now().Add(-time.Minute)}}); err != nil {
		t.Fatal(err)
	}
	if err := cs.PublishInternal(ctx, contentID, "scheduler", "/internal/scheduler/publish", crashKey); err != nil {
		t.Fatal(err)
	}
	active, err := repo.GetActive(ctx, contentID)
	if err != nil {
		t.Fatal(err)
	}
	if crashed.PublicationID == nil || active.ID != *crashed.PublicationID {
		t.Fatal("background recovery minted another Publication")
	}

	// Terminal path: missing content fails pre-activation; the retry must
	// execute a NEW attempt (surface the saga error again), never replay
	// success and never mint a row.
	missing := primitive.NewObjectID()
	missingKey := SchedulerOpKey(missing, 0)
	if err := cs.PublishInternal(ctx, missing, "scheduler", "/internal/scheduler/publish", missingKey); err == nil {
		t.Fatalf("expected failure for missing content")
	}
	if err := cs.PublishInternal(ctx, missing, "scheduler", "/internal/scheduler/publish", missingKey); err == nil {
		t.Fatalf("terminal retry must re-execute and fail, not replay")
	}
	pubCount, err = db.Collection("content_publications").CountDocuments(ctx, bson.M{"content_id": missing})
	if err != nil {
		t.Fatalf("count publications: %v", err)
	}
	if pubCount != 0 {
		t.Fatalf("terminal retries must not mint publications (count=%d)", pubCount)
	}
}
