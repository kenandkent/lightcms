package publication_test

// Task 8 (publish/unpublish saga) red-green tests.
//
// Scope (plan Task 8; spec §16, §17.4, §18.1, §20.6, §21):
//   - publish holds content/path leases, freezes versions + logical time,
//     persists the idempotency execution snapshot, renders, stages, verifies,
//     cuts the canonical file, commits Mongo active/Content/outbox, and
//     returns the URL only after success;
//   - fault injection at render/stage/verify/rename/commit preserves the old
//     canonical + old active, marks the new attempt failed, emits no success
//     event;
//   - crash fixture stops after canonical rename before Mongo commit and
//     preserves on-disk files for Task 10 (this task asserts the fixture
//     state; immediate compensation on a returned transaction error is
//     asserted by the commit-failure tests);
//   - unpublish renames to .unpublish-backup, commits lifecycle + Content +
//     outbox in ONE transaction, restores on failure; second call is 200 with
//     no new event;
//   - rename-and-publish is one path-level saga with stable lock order and
//     joint compensation;
//   - rollback mints a new publication from retained immutable bytes, with an
//     explicitly weaker re-render path past retention.
//
// DB note: these tests REQUIRE the Task 0 replica-set fixture
// (MONGODB_URI + test-named DATABASE_NAME); skips are not green evidence.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/database"
	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/idempotency"
	"github.com/jonradoff/lightcms/v7/internal/product/pathkey"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/publicurl"
	"github.com/jonradoff/lightcms/v7/internal/product/storage"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"
	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"net/url"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// sagaFailOutbox forces the activation/unpublish transactions to fail AFTER
// the file cutover, proving the saga compensates the canonical file. Only
// the FIRST insert fails (transient outage model): the saga's follow-up
// MarkFailed transaction then proves the failed attempt is recorded instead
// of left staged.
type sagaFailOutbox struct {
	mu        sync.Mutex
	failed    bool
	permanent bool
}

func (f *sagaFailOutbox) InsertUnique(_ context.Context, _ string, _ primitive.ObjectID, _ map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.permanent || !f.failed {
		f.failed = true
		return errors.New("saga-test: injected outbox failure")
	}
	return nil
}

// sagaFaultStore decorates a storage.Store with per-step failures.
type sagaFaultStore struct {
	storage.Store
	failStage    error
	failVerify   error
	failActivate error
}

func (f *sagaFaultStore) Stage(ctx context.Context, req storage.StageRequest) (storage.StagedObject, error) {
	if f.failStage != nil {
		return storage.StagedObject{}, f.failStage
	}
	return f.Store.Stage(ctx, req)
}

func (f *sagaFaultStore) Verify(ctx context.Context, obj storage.StagedObject) error {
	if f.failVerify != nil {
		return f.failVerify
	}
	return f.Store.Verify(ctx, obj)
}

func (f *sagaFaultStore) Activate(ctx context.Context, obj storage.StagedObject, publicPath string) error {
	if f.failActivate != nil {
		return f.failActivate
	}
	return f.Store.Activate(ctx, obj, publicPath)
}

func (f *sagaFaultStore) ActivateWithPrevious(ctx context.Context, obj storage.StagedObject, publicPath string, oldID *primitive.ObjectID) error {
	if f.failActivate != nil {
		return f.failActivate
	}
	return f.Store.ActivateWithPrevious(ctx, obj, publicPath, oldID)
}

type sagaSetup struct {
	db    *database.DB
	repo  *publication.Repository
	store storage.Store
	svc   *publication.Service
	root  string
	tplID primitive.ObjectID
	tvID  primitive.ObjectID
}

const sagaLayout = `<html><head><title>{{.title}}</title></head><body><h1>{{.headline}}</h1><p>{{.public_url}}</p><span>{{.publication_id}}</span><time>{{.published_at}}</time><em>{{.template_version}}</em></body></html>`

func newSagaSetup(t *testing.T, outbox publication.OutboxInserter, storeOverride storage.Store, opts publication.Options) *sagaSetup {
	t.Helper()
	db, _ := testutil.MustConnectTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.EnsureProductIndexes(ctx); err != nil {
		t.Fatalf("EnsureProductIndexes: %v", err)
	}
	root := t.TempDir()
	var store storage.Store = storage.NewFilesystemStore(root)
	if storeOverride != nil {
		store = storeOverride
	}
	repo := publication.NewRepository(db, outbox)
	if opts.Templates == nil {
		opts.Templates = templatecontract.NewService(db)
	}
	if opts.URLs == nil {
		base, _ := url.Parse("http://localhost:8080")
		resolver, err := publicurl.NewResolver(base)
		if err != nil {
			t.Fatalf("NewResolver: %v", err)
		}
		opts.URLs = resolver
	}
	svc := publication.NewService(db, repo, store, opts)
	return &sagaSetup{db: db, repo: repo, store: store, svc: svc, root: root}
}

// seedSagaTemplateVersion inserts a template_versions document and returns
// (templateID, versionID).
func seedSagaTemplateVersion(t *testing.T, db *database.DB, status string, requiredHeadline bool) (primitive.ObjectID, primitive.ObjectID) {
	t.Helper()
	tplID := primitive.NewObjectID()
	tvID := primitive.NewObjectID()
	fields := []models.TemplateField{}
	if requiredHeadline {
		fields = append(fields, models.TemplateField{Name: "headline", Label: "Headline", Type: "text", Required: true})
	}
	if status == "" {
		status = templatecontract.StatusActive
	}
	_, err := db.Collection("template_versions").InsertOne(context.Background(), bson.M{
		"_id": tvID, "template_id": tplID, "version": int64(3),
		"slug": "financial-news", "name": "Financial News", "category": "news",
		"status": status, "fields": fields, "html_layout": sagaLayout,
		"contract_hash": "sha256:contract", "render_hash": "sha256:render",
		"created_at": time.Now(),
	})
	if err != nil {
		t.Fatalf("seed template version: %v", err)
	}
	return tplID, tvID
}

// seedSagaContent inserts a live content document with canonical path fields.
func seedSagaContent(t *testing.T, db *database.DB, tplID primitive.ObjectID, fullPath string, version int64, data map[string]any) primitive.ObjectID {
	t.Helper()
	canonical, err := pathkey.Canonical(fullPath)
	if err != nil {
		t.Fatalf("canonical(%q): %v", fullPath, err)
	}
	id := primitive.NewObjectID()
	if data == nil {
		data = map[string]any{"headline": "Bitcoin Rallies"}
	}
	_, err = db.Collection("content").InsertOne(context.Background(), bson.M{
		"_id": id, "template_id": tplID, "template_name": "Financial News",
		"title": "Bitcoin Market Update", "slug": "bitcoin-market-update",
		"folder_path": "/news", "full_path": fullPath,
		"canonical_full_path": canonical, "path_scope": "live", "path_active": true,
		"current_version": version, "data": data,
		"published": false, "created_at": time.Now(), "updated_at": time.Now(),
	})
	if err != nil {
		t.Fatalf("seed content: %v", err)
	}
	return id
}

func readSagaCanonical(t *testing.T, root, fullPath string) string {
	t.Helper()
	rel := strings.TrimPrefix(fullPath, "/") + ".html"
	b, err := os.ReadFile(filepath.Join(root, "generated", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read canonical %s: %v", fullPath, err)
	}
	return string(b)
}

func sagaCanonicalExists(root, fullPath string) bool {
	rel := strings.TrimPrefix(fullPath, "/") + ".html"
	_, err := os.Stat(filepath.Join(root, "generated", filepath.FromSlash(rel)))
	return err == nil
}

func sagaOutboxCount(t *testing.T, db *database.DB, eventType string, pubID primitive.ObjectID) int64 {
	t.Helper()
	n, err := db.Collection("webhook_outbox").CountDocuments(context.Background(), bson.M{
		"event_type": eventType, "aggregate_id": pubID,
	})
	if err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	return n
}

func sagaPubCount(t *testing.T, db *database.DB, contentID primitive.ObjectID) int64 {
	t.Helper()
	n, err := db.Collection("content_publications").CountDocuments(context.Background(), bson.M{"content_id": contentID})
	if err != nil {
		t.Fatalf("count publications: %v", err)
	}
	return n
}

// TestPublishSaga_FirstPublish is the happy path: version + logical time are
// frozen, bytes render deterministically, the canonical file is cut over, one
// transaction flips active + Content projection + outbox, and the URL is
// returned only after success.
func TestPublishSaga_FirstPublish(t *testing.T) {
	s := newSagaSetup(t, nil, nil, publication.Options{})
	ctx := context.Background()
	tplID, tvID := seedSagaTemplateVersion(t, s.db, "", true)
	contentID := seedSagaContent(t, s.db, tplID, "/news/bitcoin-market-update", 1, nil)

	res, err := s.svc.Publish(ctx, publication.PublishRequest{
		ContentID: contentID, ContentVersion: 1, TemplateVersionID: tvID,
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if res.PublicationID.IsZero() || res.ContentID != contentID || res.ContentVersion != 1 {
		t.Fatalf("bad result identity: %+v", res)
	}
	if res.TemplateVersionID != tvID {
		t.Fatalf("result template version = %s, want %s", res.TemplateVersionID.Hex(), tvID.Hex())
	}
	if !strings.HasPrefix(res.ContentHash, "sha256:") || len(res.ContentHash) != 71 {
		t.Fatalf("result hash = %q, want sha256:<64hex>", res.ContentHash)
	}
	if res.LogicalPublishedAt.IsZero() {
		t.Fatal("logical publish time not frozen")
	}
	if !strings.Contains(res.PublicURL, "/news/bitcoin-market-update") {
		t.Fatalf("public URL = %q", res.PublicURL)
	}

	body := readSagaCanonical(t, s.root, "/news/bitcoin-market-update")
	if !strings.Contains(body, "<h1>Bitcoin Rallies</h1>") || !strings.Contains(body, res.PublicURL) {
		t.Fatalf("canonical body missing render context:\n%s", body)
	}
	if !strings.Contains(body, res.PublicationID.Hex()) {
		t.Fatal("canonical body missing publication_id snapshot")
	}

	active, err := s.repo.GetActive(ctx, contentID)
	if err != nil || active == nil || active.ID != res.PublicationID {
		t.Fatalf("active = (%v, %v), want %s", active, err, res.PublicationID.Hex())
	}
	// Task 16E: the resolved public URL is persisted on the record so the
	// activation-transaction outbox insert carries it (no post-commit join).
	if active.PublicURL != res.PublicURL {
		t.Fatalf("active public_url = %q, want %q", active.PublicURL, res.PublicURL)
	}
	if active.VerificationStatus != publication.VerificationVerified {
		t.Fatalf("verification = %q, want verified", active.VerificationStatus)
	}
	var content models.Content
	if err := s.db.FindOne(ctx, "content", bson.M{"_id": contentID}, &content); err != nil {
		t.Fatalf("load content: %v", err)
	}
	if !content.Published || content.PublishedAt == nil || !content.PublishedAt.Equal(res.LogicalPublishedAt) {
		t.Fatalf("projection published=%v at=%v, want true/%v", content.Published, content.PublishedAt, res.LogicalPublishedAt)
	}
	if sagaOutboxCount(t, s.db, "content.publish", res.PublicationID) != 1 {
		t.Fatal("expected exactly one content.publish outbox event")
	}
	// Task 5's outbox payload lacks PublicURL — the saga enriches it at
	// activation time.
	var evt bson.M
	if err := s.db.Collection("webhook_outbox").FindOne(ctx, bson.M{
		"event_type": "content.publish", "aggregate_id": res.PublicationID,
	}).Decode(&evt); err != nil {
		t.Fatalf("load outbox: %v", err)
	}
	payload, _ := evt["payload"].(bson.M)
	if payload == nil || payload["public_url"] != res.PublicURL {
		t.Fatalf("outbox payload public_url = %v, want %q", payload, res.PublicURL)
	}
}

// TestPublishSaga_RepublishUsesPreviousID covers the Task 6 handoff: activate
// over an existing canonical must go through ActivateWithPrevious with the
// frozen old active ID; the old immutable object is retained and the
// .previous sidecar is confirmed away after commit.
func TestPublishSaga_RepublishUsesPreviousID(t *testing.T) {
	s := newSagaSetup(t, nil, nil, publication.Options{})
	ctx := context.Background()
	tplID, tvID := seedSagaTemplateVersion(t, s.db, "", true)
	contentID := seedSagaContent(t, s.db, tplID, "/news/republish-me", 1, map[string]any{"headline": "v1 headline"})

	first, err := s.svc.Publish(ctx, publication.PublishRequest{ContentID: contentID, ContentVersion: 1, TemplateVersionID: tvID})
	if err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	oldBody := readSagaCanonical(t, s.root, "/news/republish-me")

	// Draft v2 on the live record.
	if err := s.db.Collection("content").FindOneAndUpdate(ctx,
		bson.M{"_id": contentID},
		bson.M{"$set": bson.M{"current_version": int64(2), "data": map[string]any{"headline": "v2 headline"}}},
	).Err(); err != nil {
		t.Fatalf("bump version: %v", err)
	}
	second, err := s.svc.Publish(ctx, publication.PublishRequest{
		ContentID: contentID, ContentVersion: 2, TemplateVersionID: tvID,
		ExpectedActiveID: &first.PublicationID,
	})
	if err != nil {
		t.Fatalf("second Publish: %v", err)
	}
	if second.PublicationID == first.PublicationID {
		t.Fatal("republish must mint a new publication ID")
	}
	body := readSagaCanonical(t, s.root, "/news/republish-me")
	if body == oldBody || !strings.Contains(body, "v2 headline") {
		t.Fatalf("canonical not cut over to v2:\n%s", body)
	}
	active, _ := s.repo.GetActive(ctx, contentID)
	if active == nil || active.ID != second.PublicationID {
		t.Fatalf("active = %v, want %s", active, second.PublicationID.Hex())
	}
	prev, err := s.repo.GetByID(ctx, first.PublicationID)
	if err != nil || prev.Status != publication.StatusSuperseded {
		t.Fatalf("old publication = (%v, %v), want superseded", prev, err)
	}
	// Old immutable bytes retained for exact rollback.
	if _, err := os.Stat(s.store.ImmutablePath(contentID, first.PublicationID)); err != nil {
		t.Fatalf("old immutable object must be retained: %v", err)
	}
	// .previous sidecar confirmed away.
	info, err := s.store.Inspect(ctx, "/news/republish-me")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	for _, sc := range info.Sidecars {
		if sc.Kind == storage.SidecarPrevious {
			t.Fatalf("stale .previous sidecar after commit: %s", sc.Path)
		}
	}
}

// TestPublishSaga_StaleExpectedActive proves optimistic concurrency: a stale
// ExpectedActiveID fails with PUBLICATION_CONFLICT and zero side effects.
func TestPublishSaga_StaleExpectedActive(t *testing.T) {
	s := newSagaSetup(t, nil, nil, publication.Options{})
	ctx := context.Background()
	tplID, tvID := seedSagaTemplateVersion(t, s.db, "", true)
	contentID := seedSagaContent(t, s.db, tplID, "/news/conflict-page", 1, nil)
	first, err := s.svc.Publish(ctx, publication.PublishRequest{ContentID: contentID, ContentVersion: 1, TemplateVersionID: tvID})
	if err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	before := readSagaCanonical(t, s.root, "/news/conflict-page")
	stale := primitive.NewObjectID()
	_, err = s.svc.Publish(ctx, publication.PublishRequest{
		ContentID: contentID, ContentVersion: 1, TemplateVersionID: tvID, ExpectedActiveID: &stale,
	})
	if publication.CodeOf(err) != publication.CodeConflict {
		t.Fatalf("stale expected: got %v, want PUBLICATION_CONFLICT", err)
	}
	if got := readSagaCanonical(t, s.root, "/news/conflict-page"); got != before {
		t.Fatal("canonical changed on conflict")
	}
	active, _ := s.repo.GetActive(ctx, contentID)
	if active == nil || active.ID != first.PublicationID {
		t.Fatalf("active moved on conflict: %v", active)
	}
	if n := sagaPubCount(t, s.db, contentID); n != 1 {
		t.Fatalf("publications = %d, want 1 (no record on pre-write conflict)", n)
	}
}

// TestPublishSaga_RenderFailure injects a render error: the old live page and
// active pointer stay, and no success event is created.
func TestPublishSaga_RenderFailure(t *testing.T) {
	renderErr := errors.New("saga-test: injected render failure")
	s := newSagaSetup(t, nil, nil, publication.Options{
		Renderer: func(_ context.Context, _ publication.RenderInput) ([]byte, error) {
			return nil, renderErr
		},
	})
	ctx := context.Background()
	tplID, tvID := seedSagaTemplateVersion(t, s.db, "", true)
	contentID := seedSagaContent(t, s.db, tplID, "/news/render-fail", 1, nil)
	first, err := publication.NewService(s.db, s.repo, s.store, publication.Options{}).Publish(ctx,
		publication.PublishRequest{ContentID: contentID, ContentVersion: 1, TemplateVersionID: tvID})
	if err != nil {
		t.Fatalf("setup publish: %v", err)
	}
	// v2 render fails.
	if err := s.db.Collection("content").FindOneAndUpdate(ctx, bson.M{"_id": contentID},
		bson.M{"$set": bson.M{"current_version": int64(2)}}).Err(); err != nil {
		t.Fatalf("bump: %v", err)
	}
	_, err = s.svc.Publish(ctx, publication.PublishRequest{ContentID: contentID, ContentVersion: 2, TemplateVersionID: tvID})
	if !errors.Is(err, renderErr) {
		t.Fatalf("render failure: got %v", err)
	}
	active, _ := s.repo.GetActive(ctx, contentID)
	if active == nil || active.ID != first.PublicationID {
		t.Fatalf("active moved on render failure: %v", active)
	}
	if n := sagaPubCount(t, s.db, contentID); n != 1 {
		t.Fatalf("render failure must not create a record, got %d", n)
	}
}

// TestPublishSaga_StageFailure injects a stage error and proves the terminal
// attempt is recorded for the idempotency retry path.
func TestPublishSaga_StageFailure(t *testing.T) {
	s := newSagaSetup(t, nil, nil, publication.Options{})
	ctx := context.Background()
	tplID, tvID := seedSagaTemplateVersion(t, s.db, "", true)
	contentID := seedSagaContent(t, s.db, tplID, "/news/stage-fail", 1, nil)
	first, err := s.svc.Publish(ctx, publication.PublishRequest{ContentID: contentID, ContentVersion: 1, TemplateVersionID: tvID})
	if err != nil {
		t.Fatalf("setup publish: %v", err)
	}
	before := readSagaCanonical(t, s.root, "/news/stage-fail")

	faulty := &sagaFaultStore{Store: s.store, failStage: errors.New("saga-test: injected stage failure")}
	svc2 := publication.NewService(s.db, s.repo, faulty, publication.Options{})
	if err := s.db.Collection("content").FindOneAndUpdate(ctx, bson.M{"_id": contentID},
		bson.M{"$set": bson.M{"current_version": int64(2)}}).Err(); err != nil {
		t.Fatalf("bump: %v", err)
	}
	_, err = svc2.Publish(ctx, publication.PublishRequest{ContentID: contentID, ContentVersion: 2, TemplateVersionID: tvID})
	if publication.CodeOf(err) != publication.CodeStageFailed {
		t.Fatalf("stage failure: got %v, want PUBLICATION_STAGE_FAILED", err)
	}
	if got := readSagaCanonical(t, s.root, "/news/stage-fail"); got != before {
		t.Fatal("canonical changed on stage failure")
	}
	active, _ := s.repo.GetActive(ctx, contentID)
	if active == nil || active.ID != first.PublicationID {
		t.Fatalf("active moved on stage failure: %v", active)
	}
	hist, err := s.repo.ListHistory(ctx, contentID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	var failed *publication.Publication
	for i := range hist {
		if hist[i].Status == publication.StatusFailed {
			failed = &hist[i]
		}
	}
	if failed == nil {
		t.Fatal("expected the failed attempt to be recorded")
	}
	if sagaOutboxCount(t, s.db, "content.publish", failed.ID) != 0 {
		t.Fatal("failed attempt must create no success event")
	}
}

// TestPublishSaga_VerifyFailure injects a verify error: staged bytes are
// aborted and the live page is untouched.
func TestPublishSaga_VerifyFailure(t *testing.T) {
	s := newSagaSetup(t, nil, nil, publication.Options{})
	ctx := context.Background()
	tplID, tvID := seedSagaTemplateVersion(t, s.db, "", true)
	contentID := seedSagaContent(t, s.db, tplID, "/news/verify-fail", 1, nil)
	first, err := s.svc.Publish(ctx, publication.PublishRequest{ContentID: contentID, ContentVersion: 1, TemplateVersionID: tvID})
	if err != nil {
		t.Fatalf("setup publish: %v", err)
	}
	before := readSagaCanonical(t, s.root, "/news/verify-fail")
	faulty := &sagaFaultStore{Store: s.store, failVerify: errors.New("saga-test: injected verify failure")}
	svc2 := publication.NewService(s.db, s.repo, faulty, publication.Options{})
	if err := s.db.Collection("content").FindOneAndUpdate(ctx, bson.M{"_id": contentID},
		bson.M{"$set": bson.M{"current_version": int64(2)}}).Err(); err != nil {
		t.Fatalf("bump: %v", err)
	}
	_, err = svc2.Publish(ctx, publication.PublishRequest{ContentID: contentID, ContentVersion: 2, TemplateVersionID: tvID})
	if publication.CodeOf(err) != publication.CodeVerifyFailed {
		t.Fatalf("verify failure: got %v, want PUBLICATION_VERIFY_FAILED", err)
	}
	if got := readSagaCanonical(t, s.root, "/news/verify-fail"); got != before {
		t.Fatal("canonical changed on verify failure")
	}
	active, _ := s.repo.GetActive(ctx, contentID)
	if active == nil || active.ID != first.PublicationID {
		t.Fatalf("active moved on verify failure: %v", active)
	}
}

// TestPublishSaga_ActivateFailure injects a rename/cutover error: the old
// canonical is restored by the store and the attempt is failed.
func TestPublishSaga_ActivateFailure(t *testing.T) {
	s := newSagaSetup(t, nil, nil, publication.Options{})
	ctx := context.Background()
	tplID, tvID := seedSagaTemplateVersion(t, s.db, "", true)
	contentID := seedSagaContent(t, s.db, tplID, "/news/activate-fail", 1, nil)
	first, err := s.svc.Publish(ctx, publication.PublishRequest{ContentID: contentID, ContentVersion: 1, TemplateVersionID: tvID})
	if err != nil {
		t.Fatalf("setup publish: %v", err)
	}
	before := readSagaCanonical(t, s.root, "/news/activate-fail")
	faulty := &sagaFaultStore{Store: s.store, failActivate: errors.New("saga-test: injected rename failure")}
	svc2 := publication.NewService(s.db, s.repo, faulty, publication.Options{})
	if err := s.db.Collection("content").FindOneAndUpdate(ctx, bson.M{"_id": contentID},
		bson.M{"$set": bson.M{"current_version": int64(2)}}).Err(); err != nil {
		t.Fatalf("bump: %v", err)
	}
	_, err = svc2.Publish(ctx, publication.PublishRequest{ContentID: contentID, ContentVersion: 2, TemplateVersionID: tvID})
	if publication.CodeOf(err) != publication.CodeActivateFailed {
		t.Fatalf("activate failure: got %v, want PUBLICATION_ACTIVATE_FAILED", err)
	}
	if got := readSagaCanonical(t, s.root, "/news/activate-fail"); got != before {
		t.Fatal("canonical changed on activate failure")
	}
	active, _ := s.repo.GetActive(ctx, contentID)
	if active == nil || active.ID != first.PublicationID {
		t.Fatalf("active moved on activate failure: %v", active)
	}
}

// TestPublishSaga_CommitFailureCompensates forces the Mongo activation
// transaction to fail after a successful cutover: the saga must restore the
// previous canonical, keep the old active, fail the new attempt, and emit no
// success event.
func TestPublishSaga_CommitFailureCompensates(t *testing.T) {
	s := newSagaSetup(t, &sagaFailOutbox{}, nil, publication.Options{})
	// Setup publish needs a working outbox: use a sibling service.
	good := publication.NewService(s.db, publication.NewRepository(s.db, nil), s.store, publication.Options{})
	ctx := context.Background()
	tplID, tvID := seedSagaTemplateVersion(t, s.db, "", true)
	contentID := seedSagaContent(t, s.db, tplID, "/news/commit-fail", 1, map[string]any{"headline": "v1 live"})
	first, err := good.Publish(ctx, publication.PublishRequest{ContentID: contentID, ContentVersion: 1, TemplateVersionID: tvID})
	if err != nil {
		t.Fatalf("setup publish: %v", err)
	}
	before := readSagaCanonical(t, s.root, "/news/commit-fail")
	if err := s.db.Collection("content").FindOneAndUpdate(ctx, bson.M{"_id": contentID},
		bson.M{"$set": bson.M{"current_version": int64(2), "data": map[string]any{"headline": "v2 candidate"}}}).Err(); err != nil {
		t.Fatalf("bump: %v", err)
	}
	_, err = s.svc.Publish(ctx, publication.PublishRequest{ContentID: contentID, ContentVersion: 2, TemplateVersionID: tvID})
	if err == nil {
		t.Fatal("expected commit failure")
	}
	if got := readSagaCanonical(t, s.root, "/news/commit-fail"); got != before {
		t.Fatalf("compensation did not restore old canonical:\n%s", got)
	}
	active, _ := s.repo.GetActive(ctx, contentID)
	if active == nil || active.ID != first.PublicationID {
		t.Fatalf("active moved on commit failure: %v", active)
	}
	hist, _ := s.repo.ListHistory(ctx, contentID)
	failed := 0
	for _, p := range hist {
		if p.Status == publication.StatusFailed && sagaOutboxCount(t, s.db, "content.publish", p.ID) != 0 {
			t.Fatalf("failed attempt %s has a success event", p.ID.Hex())
		}
		if p.Status == publication.StatusFailed {
			failed++
		}
	}
	if failed == 0 {
		t.Fatal("expected the new attempt to be marked failed")
	}
	// No leftover .previous sidecar may hide the compensated state.
	info, err := s.store.Inspect(ctx, "/news/commit-fail")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	for _, sc := range info.Sidecars {
		if sc.Kind == storage.SidecarPrevious {
			t.Fatalf("stale .previous after compensation: %s", sc.Path)
		}
	}
}

// TestPublishSaga_CommitFailureFirstPublishRemovesCanonical proves the
// first-publish compensation: with no previous file, a failed commit must
// remove the new canonical instead of restoring.
func TestPublishSaga_CommitFailureFirstPublishRemovesCanonical(t *testing.T) {
	s := newSagaSetup(t, &sagaFailOutbox{}, nil, publication.Options{})
	ctx := context.Background()
	tplID, tvID := seedSagaTemplateVersion(t, s.db, "", true)
	contentID := seedSagaContent(t, s.db, tplID, "/news/first-commit-fail", 1, nil)
	_, err := s.svc.Publish(ctx, publication.PublishRequest{ContentID: contentID, ContentVersion: 1, TemplateVersionID: tvID})
	if err == nil {
		t.Fatal("expected commit failure")
	}
	if sagaCanonicalExists(s.root, "/news/first-commit-fail") {
		t.Fatal("first-publish compensation must remove the uncommitted canonical")
	}
	if active, _ := s.repo.GetActive(ctx, contentID); active != nil {
		t.Fatalf("no active expected, got %s", active.ID.Hex())
	}
}

// TestPublishSaga_CrashFixturePreservesFiles implements the Task 10 handoff:
// the fault stops after canonical rename before Mongo commit and preserves
// the on-disk files (new canonical + .previous) with the old active still in
// Mongo. Task 10's scanner owns restart repair; this task only proves the
// fixture state.
func TestPublishSaga_CrashFixturePreservesFiles(t *testing.T) {
	s := newSagaSetup(t, nil, nil, publication.Options{
		Faults: publication.Faults{BeforeCommit: func(_ context.Context) error {
			return publication.ErrStopAfterRename
		}},
	})
	good := publication.NewService(s.db, publication.NewRepository(s.db, nil), s.store, publication.Options{})
	ctx := context.Background()
	tplID, tvID := seedSagaTemplateVersion(t, s.db, "", true)
	contentID := seedSagaContent(t, s.db, tplID, "/news/crash-window", 1, map[string]any{"headline": "v1 live"})
	first, err := good.Publish(ctx, publication.PublishRequest{ContentID: contentID, ContentVersion: 1, TemplateVersionID: tvID})
	if err != nil {
		t.Fatalf("setup publish: %v", err)
	}
	if err := s.db.Collection("content").FindOneAndUpdate(ctx, bson.M{"_id": contentID},
		bson.M{"$set": bson.M{"current_version": int64(2), "data": map[string]any{"headline": "v2 candidate"}}}).Err(); err != nil {
		t.Fatalf("bump: %v", err)
	}
	_, err = s.svc.Publish(ctx, publication.PublishRequest{ContentID: contentID, ContentVersion: 2, TemplateVersionID: tvID})
	if !errors.Is(err, publication.ErrStopAfterRename) {
		t.Fatalf("crash fixture: got %v, want ErrStopAfterRename", err)
	}
	// Files preserved for Task 10: new canonical + .previous-{oldID}.
	info, ierr := s.store.Inspect(ctx, "/news/crash-window")
	if ierr != nil {
		t.Fatalf("inspect: %v", ierr)
	}
	if !info.Exists {
		t.Fatal("crash fixture must preserve the new canonical file")
	}
	foundPrev := false
	for _, sc := range info.Sidecars {
		if sc.Kind == storage.SidecarPrevious && sc.PublicationID == first.PublicationID {
			foundPrev = true
		}
	}
	if !foundPrev {
		t.Fatalf("crash fixture must preserve .previous-%s, sidecars=%v", first.PublicationID.Hex(), info.Sidecars)
	}
	// No commit happened: old active still controls the plane.
	active, _ := s.repo.GetActive(ctx, contentID)
	if active == nil || active.ID != first.PublicationID {
		t.Fatalf("old active must stay active across the crash window: %v", active)
	}
}

// TestPublishSaga_RejectsLegacyUnverified enforces the Task 5 handoff: the
// repository permits legacy_unverified for migration, but ordinary business
// publish (including rollback) must never activate it.
func TestPublishSaga_RejectsLegacyUnverified(t *testing.T) {
	s := newSagaSetup(t, nil, nil, publication.Options{})
	ctx := context.Background()
	tplID, tvID := seedSagaTemplateVersion(t, s.db, "", true)
	contentID := seedSagaContent(t, s.db, tplID, "/news/legacy-page", 1, map[string]any{"headline": "legacy bytes"})

	// Migration-style active record with unverified bytes.
	legacy := &publication.Publication{
		ContentID: contentID, ContentVersion: 1, TemplateVersionID: tvID,
		FullPath: "/news/legacy-page", ContentHash: "sha256:" + strings.Repeat("a", 64),
		VerificationStatus: publication.VerificationLegacyUnverified,
		LogicalPublishedAt: time.Now().UTC(),
	}
	if err := s.repo.InsertStaged(ctx, legacy); err != nil {
		t.Fatalf("insert legacy staged: %v", err)
	}
	html := []byte("<html>legacy bytes</html>")
	staged, err := s.store.Stage(ctx, storage.StageRequest{
		ContentID: contentID, PublicationID: legacy.ID,
		CanonicalPath: "/news/legacy-page", HTML: html,
	})
	if err != nil {
		t.Fatalf("stage legacy: %v", err)
	}
	legacy.ContentHash = "sha256:" + staged.SHA256
	if _, err := s.db.Collection("content_publications").UpdateOne(ctx,
		bson.M{"_id": legacy.ID}, bson.M{"$set": bson.M{"content_hash": legacy.ContentHash}}); err != nil {
		t.Fatalf("fix hash: %v", err)
	}
	// Migration path activates directly through the repository (allowed).
	if err := s.repo.ActivateCAS(ctx, contentID, legacy.ID, nil); err != nil {
		t.Fatalf("migration ActivateCAS: %v", err)
	}

	// Ordinary rollback through the saga must refuse to reactivate it.
	_, err = s.svc.Rollback(ctx, publication.RollbackRequest{
		ContentID: contentID, SourcePublicationID: legacy.ID,
	})
	if publication.CodeOf(err) != publication.CodeActivateFailed {
		t.Fatalf("legacy rollback: got %v, want PUBLICATION_ACTIVATE_FAILED", err)
	}
	active, _ := s.repo.GetActive(ctx, contentID)
	if active == nil || active.ID != legacy.ID {
		t.Fatalf("legacy active moved: %v", active)
	}
}

// TestUnpublishSaga covers the §18.1 flow: backup rename, ONE transaction for
// lifecycle + Content + outbox, post-commit cleanup, and idempotent retry.
func TestUnpublishSaga(t *testing.T) {
	s := newSagaSetup(t, nil, nil, publication.Options{})
	ctx := context.Background()
	tplID, tvID := seedSagaTemplateVersion(t, s.db, "", true)
	contentID := seedSagaContent(t, s.db, tplID, "/news/unpublish-me", 1, nil)
	pub, err := s.svc.Publish(ctx, publication.PublishRequest{ContentID: contentID, ContentVersion: 1, TemplateVersionID: tvID})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := s.svc.Unpublish(ctx, publication.UnpublishRequest{ContentID: contentID}); err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	if sagaCanonicalExists(s.root, "/news/unpublish-me") {
		t.Fatal("canonical must be gone after unpublish")
	}
	if active, _ := s.repo.GetActive(ctx, contentID); active != nil {
		t.Fatalf("active still present: %s", active.ID.Hex())
	}
	var content models.Content
	if err := s.db.FindOne(ctx, "content", bson.M{"_id": contentID}, &content); err != nil {
		t.Fatalf("load content: %v", err)
	}
	if content.Published || content.PublishedAt != nil {
		t.Fatalf("projection published=%v at=%v, want false/nil", content.Published, content.PublishedAt)
	}
	if sagaOutboxCount(t, s.db, "content.unpublish", pub.PublicationID) != 1 {
		t.Fatal("expected exactly one content.unpublish event")
	}
	info, err := s.store.Inspect(ctx, "/news/unpublish-me")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	for _, sc := range info.Sidecars {
		if sc.Kind == storage.SidecarUnpublishBackup {
			t.Fatalf("backup not cleaned post-commit: %s", sc.Path)
		}
	}
	// Second call: 200 with no new event.
	if err := s.svc.Unpublish(ctx, publication.UnpublishRequest{ContentID: contentID}); err != nil {
		t.Fatalf("second unpublish: %v", err)
	}
	if sagaOutboxCount(t, s.db, "content.unpublish", pub.PublicationID) != 1 {
		t.Fatal("second unpublish must not create a new event")
	}
}

// TestUnpublishSaga_TxnFailureRestores forces the unpublish transaction to
// fail: the backup must be restored and the page stays live.
func TestUnpublishSaga_TxnFailureRestores(t *testing.T) {
	s := newSagaSetup(t, &sagaFailOutbox{}, nil, publication.Options{})
	good := publication.NewService(s.db, publication.NewRepository(s.db, nil), s.store, publication.Options{})
	ctx := context.Background()
	tplID, tvID := seedSagaTemplateVersion(t, s.db, "", true)
	contentID := seedSagaContent(t, s.db, tplID, "/news/unpublish-fail", 1, nil)
	pub, err := good.Publish(ctx, publication.PublishRequest{ContentID: contentID, ContentVersion: 1, TemplateVersionID: tvID})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	before := readSagaCanonical(t, s.root, "/news/unpublish-fail")
	if err := s.svc.Unpublish(ctx, publication.UnpublishRequest{ContentID: contentID}); err == nil {
		t.Fatal("expected unpublish transaction failure")
	}
	if got := readSagaCanonical(t, s.root, "/news/unpublish-fail"); got != before {
		t.Fatal("backup was not restored after transaction failure")
	}
	active, _ := s.repo.GetActive(ctx, contentID)
	if active == nil || active.ID != pub.PublicationID {
		t.Fatalf("active moved on unpublish failure: %v", active)
	}
}

// TestRenamePublishSaga covers rename-and-publish as one path-level saga:
// stable lock order, redirect committed with activation, old path retired.
func TestRenamePublishSaga(t *testing.T) {
	s := newSagaSetup(t, nil, nil, publication.Options{})
	ctx := context.Background()
	tplID, tvID := seedSagaTemplateVersion(t, s.db, "", true)
	contentID := seedSagaContent(t, s.db, tplID, "/news/old-slug", 1, map[string]any{"headline": "rename v1"})
	first, err := s.svc.Publish(ctx, publication.PublishRequest{ContentID: contentID, ContentVersion: 1, TemplateVersionID: tvID})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	// Draft rename: new version at the new path.
	if err := s.db.Collection("content").FindOneAndUpdate(ctx, bson.M{"_id": contentID},
		bson.M{"$set": bson.M{
			"current_version": int64(2), "full_path": "/news/new-slug",
			"slug": "new-slug", "canonical_full_path": "/news/new-slug",
			"data": map[string]any{"headline": "rename v2"},
		}}).Err(); err != nil {
		t.Fatalf("rename draft: %v", err)
	}
	res, err := s.svc.Publish(ctx, publication.PublishRequest{ContentID: contentID, ContentVersion: 2, TemplateVersionID: tvID})
	if err != nil {
		t.Fatalf("rename publish: %v", err)
	}
	if res.FullPath != "/news/new-slug" {
		t.Fatalf("result path = %q", res.FullPath)
	}
	body := readSagaCanonical(t, s.root, "/news/new-slug")
	if !strings.Contains(body, "rename v2") {
		t.Fatalf("new canonical missing v2 bytes:\n%s", body)
	}
	if sagaCanonicalExists(s.root, "/news/old-slug") {
		t.Fatal("old canonical must be retired after rename publish")
	}
	var redir bson.M
	if err := s.db.Collection("redirects").FindOne(ctx, bson.M{"from_path": "/news/old-slug"}).Decode(&redir); err != nil {
		t.Fatalf("redirect missing: %v", err)
	}
	if redir["to_path"] != "/news/new-slug" {
		t.Fatalf("redirect target = %v", redir["to_path"])
	}
	active, _ := s.repo.GetActive(ctx, contentID)
	if active == nil || active.ID != res.PublicationID || active.FullPath != "/news/new-slug" {
		t.Fatalf("active = %v, want %s at new path", active, res.PublicationID.Hex())
	}
	prev, _ := s.repo.GetByID(ctx, first.PublicationID)
	if prev.Status != publication.StatusSuperseded || prev.FullPath != "/news/old-slug" {
		t.Fatalf("old publication mutated: %+v", prev)
	}
}

// TestRenamePublishSaga_FailureCompensatesBothPaths proves joint compensation:
// a cutover failure leaves the old canonical live, creates no redirect, fails
// the attempt, and emits no success event.
func TestRenamePublishSaga_FailureCompensatesBothPaths(t *testing.T) {
	s := newSagaSetup(t, nil, nil, publication.Options{})
	ctx := context.Background()
	tplID, tvID := seedSagaTemplateVersion(t, s.db, "", true)
	contentID := seedSagaContent(t, s.db, tplID, "/news/old-kept", 1, map[string]any{"headline": "old live"})
	first, err := s.svc.Publish(ctx, publication.PublishRequest{ContentID: contentID, ContentVersion: 1, TemplateVersionID: tvID})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	before := readSagaCanonical(t, s.root, "/news/old-kept")
	if err := s.db.Collection("content").FindOneAndUpdate(ctx, bson.M{"_id": contentID},
		bson.M{"$set": bson.M{
			"current_version": int64(2), "full_path": "/news/new-aborted",
			"slug": "new-aborted", "canonical_full_path": "/news/new-aborted",
		}}).Err(); err != nil {
		t.Fatalf("rename draft: %v", err)
	}
	faulty := &sagaFaultStore{Store: s.store, failActivate: errors.New("saga-test: injected rename cutover failure")}
	svc2 := publication.NewService(s.db, s.repo, faulty, publication.Options{})
	_, err = svc2.Publish(ctx, publication.PublishRequest{ContentID: contentID, ContentVersion: 2, TemplateVersionID: tvID})
	if publication.CodeOf(err) != publication.CodeActivateFailed {
		t.Fatalf("rename failure: got %v, want PUBLICATION_ACTIVATE_FAILED", err)
	}
	if got := readSagaCanonical(t, s.root, "/news/old-kept"); got != before {
		t.Fatal("old canonical changed on rename failure")
	}
	if sagaCanonicalExists(s.root, "/news/new-aborted") {
		t.Fatal("aborted new canonical must not exist")
	}
	active, _ := s.repo.GetActive(ctx, contentID)
	if active == nil || active.ID != first.PublicationID {
		t.Fatalf("active moved on rename failure: %v", active)
	}
	if err := s.db.Collection("redirects").FindOne(ctx, bson.M{"from_path": "/news/old-kept"}).Decode(&bson.M{}); err == nil {
		t.Fatal("redirect must not exist after failed rename")
	}
}

// TestRollbackSaga proves exact rollback: a NEW publication restores the
// retained v1 bytes byte-for-byte.
func TestRollbackSaga(t *testing.T) {
	s := newSagaSetup(t, nil, nil, publication.Options{})
	ctx := context.Background()
	tplID, tvID := seedSagaTemplateVersion(t, s.db, "", true)
	contentID := seedSagaContent(t, s.db, tplID, "/news/rollback-me", 1, map[string]any{"headline": "v1 exact"})
	v1, err := s.svc.Publish(ctx, publication.PublishRequest{ContentID: contentID, ContentVersion: 1, TemplateVersionID: tvID})
	if err != nil {
		t.Fatalf("publish v1: %v", err)
	}
	v1body := readSagaCanonical(t, s.root, "/news/rollback-me")
	if err := s.db.Collection("content").FindOneAndUpdate(ctx, bson.M{"_id": contentID},
		bson.M{"$set": bson.M{"current_version": int64(2), "data": map[string]any{"headline": "v2 live"}}}).Err(); err != nil {
		t.Fatalf("bump: %v", err)
	}
	v2, err := s.svc.Publish(ctx, publication.PublishRequest{ContentID: contentID, ContentVersion: 2, TemplateVersionID: tvID})
	if err != nil {
		t.Fatalf("publish v2: %v", err)
	}
	res, err := s.svc.Rollback(ctx, publication.RollbackRequest{ContentID: contentID, SourcePublicationID: v1.PublicationID})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if res.PublicationID == v1.PublicationID || res.PublicationID == v2.PublicationID {
		t.Fatal("rollback must mint a new publication ID")
	}
	if got := readSagaCanonical(t, s.root, "/news/rollback-me"); got != v1body {
		t.Fatalf("rollback bytes differ from v1:\n got: %s\nwant: %s", got, v1body)
	}
	active, _ := s.repo.GetActive(ctx, contentID)
	if active == nil || active.ID != res.PublicationID {
		t.Fatalf("active = %v, want %s", active, res.PublicationID.Hex())
	}
	if active.ContentVersion != 1 {
		t.Fatalf("rollback content version = %d, want 1", active.ContentVersion)
	}
}

// TestRollbackSaga_RerenderAfterRetention proves the weaker path: when the
// source immutable object is gone, rollback re-renders from the retained
// version + template snapshots instead of failing.
func TestRollbackSaga_RerenderAfterRetention(t *testing.T) {
	s := newSagaSetup(t, nil, nil, publication.Options{})
	ctx := context.Background()
	tplID, tvID := seedSagaTemplateVersion(t, s.db, "", true)
	contentID := seedSagaContent(t, s.db, tplID, "/news/rerender-rb", 1, map[string]any{"headline": "v1 kept"})
	v1, err := s.svc.Publish(ctx, publication.PublishRequest{ContentID: contentID, ContentVersion: 1, TemplateVersionID: tvID})
	if err != nil {
		t.Fatalf("publish v1: %v", err)
	}
	// Retain the version snapshot the re-render path reads.
	if _, err := s.db.Collection("content_versions").InsertOne(ctx, bson.M{
		"content_id": contentID, "version": int64(1),
		"template_id": tplID, "title": "Bitcoin Market Update",
		"full_path":  "/news/rerender-rb",
		"data":       map[string]any{"headline": "v1 kept"},
		"created_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed version: %v", err)
	}
	if err := s.db.Collection("content").FindOneAndUpdate(ctx, bson.M{"_id": contentID},
		bson.M{"$set": bson.M{"current_version": int64(2), "data": map[string]any{"headline": "v2 live"}}}).Err(); err != nil {
		t.Fatalf("bump: %v", err)
	}
	if _, err := s.svc.Publish(ctx, publication.PublishRequest{ContentID: contentID, ContentVersion: 2, TemplateVersionID: tvID}); err != nil {
		t.Fatalf("publish v2: %v", err)
	}
	// Simulate retention GC of the v1 immutable object.
	if err := s.store.DeleteImmutable(ctx, contentID, v1.PublicationID); err != nil {
		t.Fatalf("gc v1 object: %v", err)
	}
	res, err := s.svc.Rollback(ctx, publication.RollbackRequest{ContentID: contentID, SourcePublicationID: v1.PublicationID})
	if err != nil {
		t.Fatalf("re-render rollback: %v", err)
	}
	body := readSagaCanonical(t, s.root, "/news/rerender-rb")
	if !strings.Contains(body, "v1 kept") {
		t.Fatalf("re-render rollback missing v1 data:\n%s", body)
	}
	active, _ := s.repo.GetActive(ctx, contentID)
	if active == nil || active.ID != res.PublicationID {
		t.Fatalf("active = %v, want %s", active, res.PublicationID.Hex())
	}
}

// TestPublishSaga_ConcurrentPublishConflict proves single-flight per content:
// a second publisher while one holds the saga lock gets 409.
func TestPublishSaga_ConcurrentPublishConflict(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	s := newSagaSetup(t, nil, nil, publication.Options{
		Faults: publication.Faults{BeforeCommit: func(_ context.Context) error {
			once.Do(func() { close(entered) })
			<-release
			return nil
		}},
	})
	ctx := context.Background()
	tplID, tvID := seedSagaTemplateVersion(t, s.db, "", true)
	contentID := seedSagaContent(t, s.db, tplID, "/news/single-flight", 1, nil)

	firstErr := make(chan error, 1)
	go func() {
		_, err := s.svc.Publish(context.Background(), publication.PublishRequest{
			ContentID: contentID, ContentVersion: 1, TemplateVersionID: tvID,
		})
		firstErr <- err
	}()
	<-entered // first publisher holds content+path locks inside BeforeCommit
	_, err := s.svc.Publish(ctx, publication.PublishRequest{ContentID: contentID, ContentVersion: 1, TemplateVersionID: tvID})
	if publication.CodeOf(err) != publication.CodePagePublishInProgress {
		t.Fatalf("concurrent publish: got %v, want PAGE_PUBLISH_IN_PROGRESS", err)
	}
	close(release)
	if err := <-firstErr; err != nil {
		t.Fatalf("first publish: %v", err)
	}
}

// TestPublishSaga_TerminalRetryAllocatesNewAttempt proves the §21 attempt
// semantics through the saga: a terminal pre-activation failure is retried
// via Begin (attempt++), never TakeOver, with a NEW publication ID.
func TestPublishSaga_TerminalRetryAllocatesNewAttempt(t *testing.T) {
	s := newSagaSetup(t, nil, nil, publication.Options{})
	ctx := context.Background()
	tplID, tvID := seedSagaTemplateVersion(t, s.db, "", true)
	contentID := seedSagaContent(t, s.db, tplID, "/news/idem-retry", 1, nil)

	idem, err := idempotency.NewService(s.db, idempotency.Options{})
	if err != nil {
		t.Fatalf("idem service: %v", err)
	}
	body := []byte(`{"content_id":"` + contentID.Hex() + `","version":1}`)
	op, err := idem.Begin(ctx, "test-owner", "POST", "/api/v1/page-generation", "key-terminal-1", body)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	svc := publication.NewService(s.db, s.repo, s.store, publication.Options{Idem: idem})
	if err := s.db.Collection("content").FindOneAndUpdate(ctx, bson.M{"_id": contentID},
		bson.M{"$set": bson.M{"current_version": int64(1)}}).Err(); err != nil {
		t.Fatalf("touch: %v", err)
	}

	// Attempt 1 fails at stage with a faulty store.
	faulty := &sagaFaultStore{Store: s.store, failStage: errors.New("saga-test: stage blowup")}
	svcFaulty := publication.NewService(s.db, s.repo, faulty, publication.Options{Idem: idem})
	_, err = svcFaulty.Publish(ctx, publication.PublishRequest{
		ContentID: contentID, ContentVersion: 1, TemplateVersionID: tvID, IdempotencyRecord: &op.ID,
	})
	if publication.CodeOf(err) != publication.CodeStageFailed {
		t.Fatalf("attempt 1: got %v, want PUBLICATION_STAGE_FAILED", err)
	}
	stored, err := idem.Get(ctx, op.ID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if stored.AttemptState != idempotency.AttemptTerminal {
		t.Fatalf("attempt state = %q, want terminal", stored.AttemptState)
	}
	failedPub := stored.PublicationID
	if failedPub == nil {
		t.Fatal("execution snapshot must retain the failed publication ID")
	}

	// Same key + same body after a terminal attempt advances the attempt.
	op2, err := idem.Begin(ctx, "test-owner", "POST", "/api/v1/page-generation", "key-terminal-1", body)
	if err != nil {
		t.Fatalf("begin retry: %v", err)
	}
	if op2.Attempt != 2 {
		t.Fatalf("retry attempt = %d, want 2", op2.Attempt)
	}
	res, err := svc.Publish(ctx, publication.PublishRequest{
		ContentID: contentID, ContentVersion: 1, TemplateVersionID: tvID, IdempotencyRecord: &op2.ID,
	})
	if err != nil {
		t.Fatalf("attempt 2 publish: %v", err)
	}
	if res.PublicationID == *failedPub {
		t.Fatal("terminal retry must allocate a new publication ID")
	}
	active, _ := s.repo.GetActive(ctx, contentID)
	if active == nil || active.ID != res.PublicationID {
		t.Fatalf("active = %v, want %s", active, res.PublicationID.Hex())
	}
}

// TestPublishSaga_MissingRequiredFieldFailsBeforeStage proves strict field
// validation runs before any InsertStaged or file write.
func TestPublishSaga_MissingRequiredFieldFailsBeforeStage(t *testing.T) {
	s := newSagaSetup(t, nil, nil, publication.Options{})
	ctx := context.Background()
	tplID, tvID := seedSagaTemplateVersion(t, s.db, "", true)
	contentID := seedSagaContent(t, s.db, tplID, "/news/field-required", 1, map[string]any{"headline": ""})
	_, err := s.svc.Publish(ctx, publication.PublishRequest{ContentID: contentID, ContentVersion: 1, TemplateVersionID: tvID})
	if publication.CodeOf(err) != publication.CodeValidationFailed {
		t.Fatalf("missing field: got %v, want FIELD_VALIDATION_FAILED", err)
	}
	if n := sagaPubCount(t, s.db, contentID); n != 0 {
		t.Fatalf("validation failure created %d records", n)
	}
	if sagaCanonicalExists(s.root, "/news/field-required") {
		t.Fatal("validation failure must not write any file")
	}
}

// TestPublishSaga_DeprecatedTemplateRejected proves ordinary publish refuses
// a deprecated template version while history stays intact.
func TestPublishSaga_DeprecatedTemplateRejected(t *testing.T) {
	s := newSagaSetup(t, nil, nil, publication.Options{})
	ctx := context.Background()
	tplID, tvID := seedSagaTemplateVersion(t, s.db, templatecontract.StatusDeprecated, false)
	contentID := seedSagaContent(t, s.db, tplID, "/news/deprecated-tpl", 1, nil)
	_, err := s.svc.Publish(ctx, publication.PublishRequest{ContentID: contentID, ContentVersion: 1, TemplateVersionID: tvID})
	if publication.CodeOf(err) != publication.CodeTemplateNotActive {
		t.Fatalf("deprecated template: got %v, want TEMPLATE_NOT_ACTIVE", err)
	}
	if n := sagaPubCount(t, s.db, contentID); n != 0 {
		t.Fatalf("rejected publish created %d records", n)
	}
}

// TestPublishSaga_ResolvesCurrentVersions proves zero-value request fields
// freeze to the latest content version and current template version under lock.
func TestPublishSaga_ResolvesCurrentVersions(t *testing.T) {
	s := newSagaSetup(t, nil, nil, publication.Options{})
	ctx := context.Background()
	tplID, tvID := seedSagaTemplateVersion(t, s.db, "", true)
	// Seed the mutable template row so GetCurrent can resolve it.
	if _, err := s.db.Collection("templates").InsertOne(ctx, bson.M{
		"_id": tplID, "name": "Financial News", "slug": "financial-news",
		"category": "news", "status": templatecontract.StatusActive,
		"current_version": int64(3), "created_at": time.Now(), "updated_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed template: %v", err)
	}
	contentID := seedSagaContent(t, s.db, tplID, "/news/resolve-current", 7, nil)
	res, err := s.svc.Publish(ctx, publication.PublishRequest{ContentID: contentID})
	if err != nil {
		t.Fatalf("publish with resolved versions: %v", err)
	}
	if res.ContentVersion != 7 || res.TemplateVersionID != tvID {
		t.Fatalf("resolved versions = (%d, %s), want (7, %s)",
			res.ContentVersion, res.TemplateVersionID.Hex(), tvID.Hex())
	}
}
