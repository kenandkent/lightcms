package handlers

// Lane 4B: admin content FORM publish delegation + RBAC gates.
//
// Spec §16.6 — PublicationService is the only module allowed to change live
// page state. These tests lock in:
//   - RBAC gates on CreateContent/UpdateContent/RevertContentVersion (Wave 1A
//     permission matrix: viewer 403, contributor reaches UpdateContent);
//   - the unwired legacy fallback (form checkbox still writes the flag
//     directly — zero behavior change for pre-V3 installs and existing tests);
//   - the wired path: the checkbox becomes an intent that runs through the
//     publication saga (publications row + flag projection owned by the saga);
//   - EditContent exposing ActivePublicationID for the Lane 4C publish form.

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/idempotency"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/publicurl"
	"github.com/jonradoff/lightcms/v7/internal/product/storage"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// TestRBAC4B_ContentFormGates: viewers are read-only on the content form
// POSTs; contributors (no content.edit, but content.submit_approval) must
// still reach UpdateContent so they can submit a draft for approval.
func TestRBAC4B_ContentFormGates(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()
	db := testDB(t)
	tmplID := seedTemplate(t, db, "Page", "page")
	contentID := seedContent(t, db, tmplID, "RBAC Target", "rbac-target", "/rbac-target")
	seedContentVersion(t, db, contentID, tmplID)

	// Viewer: 403 on create, update and revert.
	if rr := rbacPost(t, h.CreateContent, url.Values{
		"template_id": {tmplID.Hex()}, "title": {"Nope"}, "slug": {"nope"},
	}, nil, "viewer"); rr.Code != http.StatusForbidden {
		t.Errorf("viewer CreateContent: got %d, want 403 (%s)", rr.Code, rr.Body.String())
	}
	if rr := rbacPost(t, h.UpdateContent, url.Values{
		"title": {"Nope"}, "slug": {"rbac-target"},
	}, map[string]string{"id": contentID.Hex()}, "viewer"); rr.Code != http.StatusForbidden {
		t.Errorf("viewer UpdateContent: got %d, want 403 (%s)", rr.Code, rr.Body.String())
	}
	if rr := rbacPost(t, h.RevertContentVersion, nil,
		map[string]string{"id": contentID.Hex(), "version": "1"},
		"viewer"); rr.Code != http.StatusForbidden {
		t.Errorf("viewer RevertContentVersion: got %d, want 403 (%s)", rr.Code, rr.Body.String())
	}

	// Contributor: must NOT be 403 on UpdateContent (submit-for-approval path).
	// Checkbox off keeps this a plain draft save — no approval goroutine.
	rr := rbacPost(t, h.UpdateContent, url.Values{
		"title": {"Contributor Draft Save"}, "slug": {"rbac-target"},
	}, map[string]string{"id": contentID.Hex()}, "contributor")
	if rr.Code == http.StatusForbidden {
		t.Fatalf("contributor UpdateContent must not be 403, got body %s", rr.Body.String())
	}
	if rr.Code >= 500 {
		t.Errorf("contributor UpdateContent: server error %d (%s)", rr.Code, rr.Body.String())
	}
}

// TestFormPublish4B_LegacyUnwiredCreate pins the fallback contract: with no
// publication runtime wired the create form keeps writing the published flag
// directly, exactly as before this change.
func TestFormPublish4B_LegacyUnwiredCreate(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()
	db := testDB(t)
	ctx := context.Background()
	tmplID := seedTemplate(t, db, "Page", "page")

	if h.publicationService != nil {
		t.Fatal("runtime must be unwired for the legacy fallback test")
	}

	// Checkbox on → published: true + published_at stamped on the insert.
	if rr := postForm(t, h.CreateContent, url.Values{
		"template_id": {tmplID.Hex()}, "title": {"Legacy Live"},
		"slug": {"legacy-live"}, "published": {"on"},
	}, nil); rr.Code != http.StatusSeeOther {
		t.Fatalf("CreateContent(published=on): %d (%s)", rr.Code, rr.Body.String())
	}
	var live models.Content
	if err := db.FindOne(ctx, "content", bson.M{"slug": "legacy-live"}, &live); err != nil {
		t.Fatalf("load legacy live: %v", err)
	}
	if !live.Published {
		t.Error("unwired create with published=on must keep published=true")
	}
	if live.PublishedAt == nil {
		t.Error("unwired create with published=on must stamp published_at")
	}

	// Checkbox absent → draft.
	if rr := postForm(t, h.CreateContent, url.Values{
		"template_id": {tmplID.Hex()}, "title": {"Legacy Draft"},
		"slug": {"legacy-draft"},
	}, nil); rr.Code != http.StatusSeeOther {
		t.Fatalf("CreateContent(no checkbox): %d (%s)", rr.Code, rr.Body.String())
	}
	var draft models.Content
	if err := db.FindOne(ctx, "content", bson.M{"slug": "legacy-draft"}, &draft); err != nil {
		t.Fatalf("load legacy draft: %v", err)
	}
	if draft.Published {
		t.Error("unwired create without the checkbox must stay published=false")
	}
}

// TestFormPublish4B_SagaDelegation wires the shared V3 runtime the same way
// Test16C does (temp-dir filesystem store keeps canonical bytes out of the
// repo) and proves the form checkbox now routes through the saga: an active
// publication row is minted, the flag projection is the saga's, and
// unchecking runs Unpublish.
func TestFormPublish4B_SagaDelegation(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()
	db := testDB(t)
	ctx := context.Background()

	// Product indexes own the idempotency unique key + active pointer.
	if err := db.EnsureProductIndexes(ctx); err != nil {
		t.Fatalf("EnsureProductIndexes: %v", err)
	}

	// Active immutable template version (the saga pins this). The layout only
	// references keys the renderer always injects (.title) — missingkey=error.
	tplID := primitive.NewObjectID()
	if _, err := db.Collection("templates").InsertOne(ctx, bson.M{
		"_id": tplID, "name": "News", "slug": "news", "current_version": int64(1),
		"fields": bson.A{}, "created_at": time.Now(), "updated_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed template: %v", err)
	}
	if _, err := db.Collection("template_versions").InsertOne(ctx, bson.M{
		"_id": primitive.NewObjectID(), "template_id": tplID, "version": int64(1),
		"slug": "news", "name": "News", "status": templatecontract.StatusActive,
		"fields": []bson.M{}, "html_layout": "<html><body><h1>{{.title}}</h1></body></html>",
		"contract_hash": "sha256:c", "render_hash": "sha256:r",
		"created_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed template version: %v", err)
	}

	// Draft content row with V3 canonical bookkeeping (16A shape).
	contentID := primitive.NewObjectID()
	if _, err := db.Collection("content").InsertOne(ctx, bson.M{
		"_id": contentID, "template_id": tplID, "template_name": "News",
		"title": "Saga Page", "slug": "saga-page",
		"full_path": "/saga-page", "canonical_full_path": "/saga-page",
		"path_scope": "live", "path_active": true,
		"current_version": int64(1), "data": bson.M{},
		"published": false, "has_unpublished_changes": false,
		"created_at": time.Now(), "updated_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed content: %v", err)
	}
	// handlers cleanupCollections does not cover the V3 product collections.
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, col := range []string{"template_versions", "content_publications", "idempotency_records", "webhook_outbox"} {
			db.Collection(col).Drop(cctx) //nolint:errcheck
		}
	})

	// Wire the shared V3 runtime exactly like cmd/server/main.go (16C pattern);
	// the temp-dir store keeps generated canonical bytes out of the repo.
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
	pubs := publication.NewService(db, repo, store, publication.Options{
		Idem: idem, URLs: resolver, BuildSHA: "test-4b",
	})
	h.SetPublicationRuntime(pubs, idem, nil)

	vars := map[string]string{"id": contentID.Hex()}
	load := func() models.Content {
		t.Helper()
		var c models.Content
		if err := db.FindOne(ctx, "content", bson.M{"_id": contentID}, &c); err != nil {
			t.Fatalf("load content: %v", err)
		}
		return c
	}

	// 1. Checkbox on → saga publish: 303 (NOT the 200 failure page), flag
	// projection + publications row owned by the saga.
	rr := postForm(t, h.UpdateContent, url.Values{
		"title": {"Saga Page"}, "slug": {"saga-page"}, "published": {"on"},
	}, vars)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("form publish: got %d (%s)", rr.Code, rr.Body.String())
	}
	c := load()
	if !c.Published {
		t.Error("saga publish must set published=true")
	}
	if c.HasUnpublishedChanges {
		t.Error("saga activation must clear has_unpublished_changes")
	}
	if c.PublishedAt == nil {
		t.Error("saga activation must stamp published_at")
	}
	if _, err := os.Stat(filepath.Join(root, "generated", "saga-page.html")); err != nil {
		t.Fatalf("saga canonical missing in temp store: %v", err)
	}
	if cnt, err := db.Collection("content_publications").CountDocuments(ctx,
		bson.M{"content_id": contentID, "status": "active"}); err != nil || cnt != 1 {
		t.Fatalf("expected exactly 1 active publication row, got %d (err=%v)", cnt, err)
	}
	if active, err := repo.GetActive(ctx, contentID); err != nil || active == nil {
		t.Fatalf("GetActive after publish: active=%v err=%v", active, err)
	}

	// 2. Checkbox off → saga unpublish: 303, flag false, no active publication.
	rr = postForm(t, h.UpdateContent, url.Values{
		"title": {"Saga Page"}, "slug": {"saga-page"},
	}, vars)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("form unpublish: got %d (%s)", rr.Code, rr.Body.String())
	}
	c = load()
	if c.Published {
		t.Error("saga unpublish must set published=false")
	}
	if active, err := repo.GetActive(ctx, contentID); err != nil || active != nil {
		t.Fatalf("GetActive after unpublish: active=%v err=%v", active, err)
	}
}

// TestFormPublish4B_EditContentActivePublication: the edit page renders with
// and without an active publication row (Lane 4C consumes ActivePublicationID;
// with no template reference yet the observable contract is "never breaks").
func TestFormPublish4B_EditContentActivePublication(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()
	db := testDB(t)
	ctx := context.Background()
	tmplID := seedTemplate(t, db, "Page", "page")
	contentID := seedContent(t, db, tmplID, "Pubbed", "pubbed", "/pubbed")

	rr := getPage(t, h.EditContent, map[string]string{"id": contentID.Hex()})
	if rr.Code != http.StatusOK {
		t.Fatalf("EditContent (no publication): %d (%s)", rr.Code, rr.Body.String())
	}

	// Seed an active publication row → the lookup path runs and still renders.
	pubID := primitive.NewObjectID()
	if _, err := db.Collection("content_publications").InsertOne(ctx, bson.M{
		"_id": pubID, "content_id": contentID, "status": "active",
		"content_version": int64(1), "template_version": int64(1),
		"full_path": "/pubbed", "created_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed publication: %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		db.Collection("content_publications").Drop(cctx) //nolint:errcheck
	})

	rr = getPage(t, h.EditContent, map[string]string{"id": contentID.Hex()})
	if rr.Code != http.StatusOK {
		t.Fatalf("EditContent (active publication): %d (%s)", rr.Code, rr.Body.String())
	}

	// Post-merge contract (4B data key + 4C template): the publish form must
	// carry the active publication as the expected_active_id CAS precondition.
	if body := rr.Body.String(); !strings.Contains(body, `name="expected_active_id" value="`+pubID.Hex()+`"`) {
		t.Errorf("edit page missing expected_active_id=%s hidden input in publish form", pubID.Hex())
	}
}
