package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/jonradoff/lightcms/v7/internal/auth"
	"github.com/jonradoff/lightcms/v7/internal/database"
	"github.com/jonradoff/lightcms/v7/internal/middleware"
	"github.com/jonradoff/lightcms/v7/internal/product/idempotency"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/publicurl"
	"github.com/jonradoff/lightcms/v7/internal/product/storage"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Lane 2A red-green: publish-path contract (scope, version precondition,
// idempotency). Shared V3 wiring mirrors cmd/server/main.go.
func setup2APublish(t *testing.T, n int) (*APIHandler, *database.DB, func(), []primitive.ObjectID) {
	t.Helper()
	ah, db, cleanup := newTestAPIHandler(t)
	ctx := context.Background()
	if err := db.EnsureProductIndexes(ctx); err != nil {
		t.Fatalf("EnsureProductIndexes: %v", err)
	}
	tplID := primitive.NewObjectID()
	tvID := primitive.NewObjectID()
	if _, err := db.Collection("templates").InsertOne(ctx, bson.M{
		"_id": tplID, "name": "News", "slug": "news", "current_version": int64(1),
		"created_at": time.Now(), "updated_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed template: %v", err)
	}
	if _, err := db.Collection("template_versions").InsertOne(ctx, bson.M{
		"_id": tvID, "template_id": tplID, "version": int64(1),
		"slug": "news", "name": "News", "status": templatecontract.StatusActive,
		"fields": []bson.M{}, "html_layout": "<html><body>{{.headline}}</body></html>",
		"contract_hash": "sha256:c", "render_hash": "sha256:r",
		"created_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed template version: %v", err)
	}
	var ids []primitive.ObjectID
	for i := 0; i < n; i++ {
		cid := primitive.NewObjectID()
		slug := fmt.Sprintf("t2a-%d-%s", i, cid.Hex()[:8])
		if _, err := db.Collection("content").InsertOne(ctx, bson.M{
			"_id": cid, "template_id": tplID, "template_name": "News",
			"title": fmt.Sprintf("T2A %d", i), "slug": slug, "folder_path": "/news", "full_path": "/news/" + slug,
			"canonical_full_path": "/news/" + slug, "path_scope": "live", "path_active": true,
			"current_version": int64(1), "data": bson.M{"headline": "hi"},
			"published": false, "created_at": time.Now(), "updated_at": time.Now(),
		}); err != nil {
			t.Fatalf("seed content: %v", err)
		}
		ids = append(ids, cid)
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
	pubs := publication.NewService(db, repo, store, publication.Options{
		Idem: idem, URLs: resolver, BuildSHA: "test-2a",
	})
	ah.SetPublicationRuntime(pubs, idem, nil)
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, col := range []string{"templates", "template_versions", "content", "content_publications", "idempotency_records", "webhook_outbox"} {
			db.Collection(col).Drop(cctx) //nolint:errcheck
		}
	})
	return ah, db, cleanup, ids
}

func count2ADocs(t *testing.T, db *database.DB, col string, filter bson.M) int64 {
	t.Helper()
	n, err := db.Collection(col).CountDocuments(context.Background(), filter)
	if err != nil {
		t.Fatalf("count %s: %v", col, err)
	}
	return n
}

func content2APublished(t *testing.T, db *database.DB, id primitive.ObjectID) bool {
	t.Helper()
	var doc bson.M
	if err := db.Collection("content").FindOne(context.Background(), bson.M{"_id": id}).Decode(&doc); err != nil {
		t.Fatalf("read content: %v", err)
	}
	pub, _ := doc["published"].(bool)
	return pub
}

// publish-only scoped key (role editor narrowed to content.publish) must get
// 403 with zero mutation on single publish-by-ID.
func Test2A_PublishOnlyScopeKeyForbiddenSingle(t *testing.T) {
	ah, db, cleanup, ids := setup2APublish(t, 1)
	defer cleanup()
	ctx := context.Background()
	id := ids[0]

	scoped := &auth.SessionUser{
		ID: "0000000000000000000002a1", Email: "pubonly@test",
		Role: "editor", ViaAPIKey: true, Scopes: []string{auth.PermContentPublish},
	}
	admin := &auth.SessionUser{ID: primitive.NewObjectID().Hex(), Email: "admin@test", Role: "admin"}

	call := func(user *auth.SessionUser, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/v1/content/"+id.Hex()+"/publish", strings.NewReader(""))
		req = mux.SetURLVars(req, map[string]string{"id": id.Hex()})
		req = req.WithContext(middleware.InjectAPIUser(req.Context(), user))
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		rr := httptest.NewRecorder()
		ah.APIPublishContent(rr, req)
		return rr
	}

	pubsBefore := count2ADocs(t, db, "content_publications", bson.M{})
	idemBefore := count2ADocs(t, db, "idempotency_records", bson.M{})
	_ = ctx

	rr := call(scoped, "k-2a-scope-single")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("publish-only key: expected 403, got %d (%s)", rr.Code, rr.Body.String())
	}
	if got := count2ADocs(t, db, "content_publications", bson.M{}); got != pubsBefore {
		t.Fatalf("403 must mint zero publications (before=%d after=%d)", pubsBefore, got)
	}
	if got := count2ADocs(t, db, "idempotency_records", bson.M{}); got != idemBefore {
		t.Fatalf("403 must mint zero idempotency records (before=%d after=%d)", idemBefore, got)
	}
	if content2APublished(t, db, id) {
		t.Fatal("403 must leave content unpublished")
	}

	// Positive control: full-perm admin with a fresh key publishes fine.
	rr = call(admin, "k-2a-scope-single-admin")
	if rr.Code != http.StatusOK {
		t.Fatalf("admin publish: expected 200, got %d (%s)", rr.Code, rr.Body.String())
	}
}

// Same contract on the batch path.
func Test2A_PublishOnlyScopeKeyForbiddenBatch(t *testing.T) {
	ah, db, cleanup, ids := setup2APublish(t, 1)
	defer cleanup()

	scoped := &auth.SessionUser{
		ID: "0000000000000000000002a2", Email: "pubonly@test",
		Role: "editor", ViaAPIKey: true, Scopes: []string{auth.PermContentPublish},
	}

	call := func(user *auth.SessionUser, key string) *httptest.ResponseRecorder {
		body := `{"ids":["` + ids[0].Hex() + `"]}`
		req := httptest.NewRequest("POST", "/api/v1/content/batch-publish", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(middleware.InjectAPIUser(req.Context(), user))
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		rr := httptest.NewRecorder()
		ah.APIBatchPublishContent(rr, req)
		return rr
	}

	pubsBefore := count2ADocs(t, db, "content_publications", bson.M{})
	idemBefore := count2ADocs(t, db, "idempotency_records", bson.M{})

	rr := call(scoped, "k-2a-scope-batch")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("publish-only key batch: expected 403, got %d (%s)", rr.Code, rr.Body.String())
	}
	if got := count2ADocs(t, db, "content_publications", bson.M{}); got != pubsBefore {
		t.Fatalf("403 must mint zero publications (before=%d after=%d)", pubsBefore, got)
	}
	if got := count2ADocs(t, db, "idempotency_records", bson.M{}); got != idemBefore {
		t.Fatalf("403 must mint zero idempotency records (before=%d after=%d)", idemBefore, got)
	}
	if content2APublished(t, db, ids[0]) {
		t.Fatal("403 must leave content unpublished")
	}
}

// Search-replace execute (global + scoped) requires Idempotency-Key: 428 with
// zero mutation when absent.
func Test2A_SearchReplaceExecuteRequiresKey(t *testing.T) {
	ah, db, cleanup := newTestAPIHandler(t)
	defer cleanup()

	tmplID := seedTemplate(t, db, "Page", "page-2a-sr")
	gID := seedContent(t, db, tmplID, "2A Global Key Page", "s2a-global", "/s2a-global")
	sID := seedContent(t, db, tmplID, "2A Scoped Key Page", "s2a-scoped", "/s2a-scoped")

	titleOf := func(id primitive.ObjectID) string {
		var doc bson.M
		if err := db.Collection("content").FindOne(context.Background(), bson.M{"_id": id}).Decode(&doc); err != nil {
			t.Fatalf("read content: %v", err)
		}
		s, _ := doc["title"].(string)
		return s
	}
	callGlobal := func(key, search, replace string) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"search":%q,"replace":%q}`, search, replace)
		req := authReq(http.MethodPost, "/api/v1/content/search-replace/execute", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		rr := httptest.NewRecorder()
		ah.APISearchReplaceExecute(rr, req)
		return rr
	}
	callScoped := func(key, search, replace string) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"search":%q,"replace":%q,"scope":{"content_ids":[%q]}}`, search, replace, sID.Hex())
		req := authReq(http.MethodPost, "/api/v1/content/scoped-search-replace/execute", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		rr := httptest.NewRecorder()
		ah.APIScopedSearchReplaceExecute(rr, req)
		return rr
	}

	// Global without key → 428, content untouched.
	if rr := callGlobal("", "Global Key", "Global Done"); rr.Code != 428 {
		t.Fatalf("global execute without key: expected 428, got %d (%s)", rr.Code, rr.Body.String())
	}
	if got := titleOf(gID); got != "2A Global Key Page" {
		t.Fatalf("428 must not mutate content, title=%q", got)
	}
	// Global with key → 200 and the replacement lands.
	if rr := callGlobal("k-2a-sr-global", "Global Key", "Global Done"); rr.Code != http.StatusOK {
		t.Fatalf("global execute with key: expected 200, got %d (%s)", rr.Code, rr.Body.String())
	}
	if got := titleOf(gID); got != "2A Global Done Page" {
		t.Fatalf("keyed execute must apply replacement, title=%q", got)
	}

	// Scoped without key → 428, content untouched.
	if rr := callScoped("", "Scoped Key", "Scoped Done"); rr.Code != 428 {
		t.Fatalf("scoped execute without key: expected 428, got %d (%s)", rr.Code, rr.Body.String())
	}
	if got := titleOf(sID); got != "2A Scoped Key Page" {
		t.Fatalf("428 must not mutate scoped content, title=%q", got)
	}
	// Scoped with key → 200 and the replacement lands.
	if rr := callScoped("k-2a-sr-scoped", "Scoped Key", "Scoped Done"); rr.Code != http.StatusOK {
		t.Fatalf("scoped execute with key: expected 200, got %d (%s)", rr.Code, rr.Body.String())
	}
	if got := titleOf(sID); got != "2A Scoped Done Page" {
		t.Fatalf("keyed scoped execute must apply replacement, title=%q", got)
	}
}

// Batch same-key retry must replay cached per-item responses instead of
// minting duplicate publications (single publication set).
func Test2A_BatchSameKeyRetrySinglePublicationSet(t *testing.T) {
	ah, db, cleanup, ids := setup2APublish(t, 2)
	defer cleanup()
	admin := &auth.SessionUser{ID: primitive.NewObjectID().Hex(), Email: "admin@test", Role: "admin"}

	call := func(key string) (int, map[string]any) {
		parts := make([]string, 0, len(ids))
		for _, id := range ids {
			parts = append(parts, `"`+id.Hex()+`"`)
		}
		body := `{"ids":[` + strings.Join(parts, ",") + `]}`
		req := httptest.NewRequest("POST", "/api/v1/content/batch-publish", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(middleware.InjectAPIUser(req.Context(), admin))
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		rr := httptest.NewRecorder()
		ah.APIBatchPublishContent(rr, req)
		var decoded map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &decoded)
		return rr.Code, decoded
	}

	code, first := call("k-2a-batch-retry")
	if code != http.StatusOK {
		t.Fatalf("first batch: expected 200, got %d (%v)", code, first)
	}
	firstItems, _ := first["publications"].([]any)
	if len(firstItems) != 2 {
		t.Fatalf("first batch: expected 2 publication entries, got %v", first)
	}
	firstIDs := map[string]string{}
	for _, it := range firstItems {
		m, _ := it.(map[string]any)
		id, _ := m["id"].(string)
		pid, _ := m["publication_id"].(string)
		if id == "" || pid == "" {
			t.Fatalf("batch item must carry id + publication_id, got %v", it)
		}
		firstIDs[id] = pid
	}
	if got := count2ADocs(t, db, "content_publications", bson.M{}); got != 2 {
		t.Fatalf("first batch must mint exactly 2 publications, got %d", got)
	}

	// Same-key retry: same publication IDs, no new rows.
	code, second := call("k-2a-batch-retry")
	if code != http.StatusOK {
		t.Fatalf("retry batch: expected 200, got %d (%v)", code, second)
	}
	secondItems, _ := second["publications"].([]any)
	if len(secondItems) != 2 {
		t.Fatalf("retry batch: expected 2 publication entries, got %v", second)
	}
	for _, it := range secondItems {
		m, _ := it.(map[string]any)
		id, _ := m["id"].(string)
		pid, _ := m["publication_id"].(string)
		if firstIDs[id] != pid {
			t.Fatalf("same-key retry must replay publication %s for %s, got %s", firstIDs[id], id, pid)
		}
	}
	if got := count2ADocs(t, db, "content_publications", bson.M{}); got != 2 {
		t.Fatalf("same-key retry must not mint duplicates (publications=%d, want 2)", got)
	}
	// Per-item idempotency records exist (path-scoped per content item).
	if got := count2ADocs(t, db, "idempotency_records", bson.M{"path": bson.M{"$regex": "/items/"}}); got != 2 {
		t.Fatalf("expected 2 per-item idempotency records, got %d", got)
	}
}
