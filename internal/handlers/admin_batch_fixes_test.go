package handlers

// Batch regression for the self-review findings B1/B3/B4/B5/M6–M14 (admin +
// API layer). B2 has its own file (admin_mutation_rbac_test.go).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/jonradoff/lightcms/v7/internal/auth"
	"github.com/jonradoff/lightcms/v7/internal/middleware"
	"github.com/jonradoff/lightcms/v7/internal/product/idempotency"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/publicurl"
	"github.com/jonradoff/lightcms/v7/internal/product/storage"
	"github.com/jonradoff/lightcms/v7/internal/services"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// B1: soft-deleted pages (and deleted+published rows) never serve publicly.
func TestServePageDeletedNeverServes(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()
	ctx := context.Background()
	tmplID := seedTemplate(t, h.db, "B1 Template", "b1-template")
	cid := primitive.NewObjectID()
	if _, err := h.db.Collection("content").InsertOne(ctx, bson.M{
		"_id": cid, "template_id": tmplID, "title": "B1", "slug": "b1-page",
		"full_path": "/b1-page", "published": true, "deleted": false,
		"data": bson.M{}, "created_at": time.Now(), "updated_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	serve := func(path string) int {
		r := mux.NewRouter()
		r.HandleFunc("/{slug:.*}", h.ServePage)
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		return rr.Code
	}
	if code := serve("/b1-page"); code != http.StatusOK {
		t.Fatalf("live page: got %d, want 200", code)
	}
	// Soft delete (the DeleteContent shape): path moved, unpublished.
	if _, err := h.db.Collection("content").UpdateOne(ctx, bson.M{"_id": cid}, bson.M{"$set": bson.M{
		"deleted": true, "published": false, "full_path": "__deleted__/" + cid.Hex(),
	}}); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if code := serve("/b1-page"); code != http.StatusNotFound {
		t.Fatalf("deleted page: got %d, want 404", code)
	}
	// Belt and suspenders: a row that is BOTH deleted and published (only
	// reachable via the old single-point bypass) must still 404.
	if _, err := h.db.Collection("content").UpdateOne(ctx, bson.M{"_id": cid}, bson.M{"$set": bson.M{
		"published": true, "full_path": "/b1-page",
	}}); err != nil {
		t.Fatalf("republish deleted: %v", err)
	}
	if code := serve("/b1-page"); code != http.StatusNotFound {
		t.Fatalf("deleted+published page: got %d, want 404", code)
	}
}

// M8: the role gate expression must render bare (html/template already
// quotes in script context) — the printf "%q" double-wrap broke the
// admin-only comment button the same way CSRF broke before it.
func TestEditPageRoleNotDoubleEscaped(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()
	tmplID := seedTemplate(t, h.db, "M8 Template", "m8-template")
	cid := primitive.NewObjectID()
	if _, err := h.db.Collection("content").InsertOne(context.Background(), bson.M{
		"_id": cid, "template_id": tmplID, "title": "M8", "slug": "m8-page",
		"full_path": "/m8-page", "published": false,
		"data": bson.M{}, "created_at": time.Now(), "updated_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	req := rbacSessionReq(t, "admin", http.MethodGet, "/cm/content/"+cid.Hex(), nil,
		map[string]string{"id": cid.Hex()})
	rr := httptest.NewRecorder()
	h.EditContent(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("edit page: got %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `const currentUserRole = "admin"`) {
		t.Fatalf("role expression missing or mis-rendered:\n%s", body)
	}
	if strings.Contains(body, `\\"admin\\"`) || strings.Contains(body, `&quot;admin`) {
		t.Fatal("role expression is double-escaped")
	}
}

// B3: REST single + batch publishes mint attributed Publication records.
func TestRESTPublishAttribution(t *testing.T) {
	ah, db, cleanup, ids := setup2APublish(t, 2)
	defer cleanup()
	ctx := context.Background()
	admin := &auth.SessionUser{ID: primitive.NewObjectID().Hex(), Email: "admin@test", Role: "admin"}

	call := func(id primitive.ObjectID, session string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/v1/content/"+id.Hex()+"/publish", strings.NewReader(""))
		req = mux.SetURLVars(req, map[string]string{"id": id.Hex()})
		req = req.WithContext(middleware.InjectAPIUser(req.Context(), admin))
		req.Header.Set("Idempotency-Key", "k-b3-"+id.Hex()[:8])
		if session != "" {
			req.Header.Set("X-Agent-Session", session)
		}
		rr := httptest.NewRecorder()
		ah.APIPublishContent(rr, req)
		return rr
	}
	if rr := call(ids[0], "testsess-1"); rr.Code != http.StatusOK {
		t.Fatalf("agent publish: got %d (%s)", rr.Code, rr.Body.String())
	}
	var pub bson.M
	if err := db.Collection("content_publications").FindOne(ctx,
		bson.M{"content_id": ids[0]}).Decode(&pub); err != nil {
		t.Fatalf("publication row: %v", err)
	}
	if pub["actor"] != "agent" || pub["via"] != "api" || pub["agent_session"] != "testsess-1" {
		t.Fatalf("publication attribution = %v (want agent/api/testsess-1)", pub)
	}
	if rr := call(ids[1], ""); rr.Code != http.StatusOK {
		t.Fatalf("human publish: got %d (%s)", rr.Code, rr.Body.String())
	}
	var pub2 bson.M
	if err := db.Collection("content_publications").FindOne(ctx,
		bson.M{"content_id": ids[1]}).Decode(&pub2); err != nil {
		t.Fatalf("publication row: %v", err)
	}
	if pub2["actor"] != "human" || pub2["via"] != "api" {
		t.Fatalf("publication attribution = %v (want human/api)", pub2)
	}
	if sess, _ := pub2["agent_session"].(string); sess != "" {
		t.Fatalf("human publish must not carry a session, got %q", sess)
	}
}

// B4: API-created comments carry Via/Session provenance (never blank).
func TestCommentProvenanceStamped(t *testing.T) {
	ah, db, cleanup := newTestAPIHandler(t)
	defer cleanup()
	ctx := context.Background()
	tmplID := seedTemplate(t, db, "B4 Template", "b4-template")
	cid := primitive.NewObjectID()
	if _, err := db.Collection("content").InsertOne(ctx, bson.M{
		"_id": cid, "template_id": tmplID, "title": "B4", "slug": "b4-page",
		"full_path": "/b4-page", "published": false,
		"data": bson.M{}, "created_at": time.Now(), "updated_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	admin := &auth.SessionUser{ID: primitive.NewObjectID().Hex(), Email: "admin@test", Role: "admin"}
	req := httptest.NewRequest("POST", "/api/v1/content/"+cid.Hex()+"/comments",
		strings.NewReader(`{"text":"hello agent"}`))
	req = mux.SetURLVars(req, map[string]string{"id": cid.Hex()})
	req = req.WithContext(middleware.InjectAPIUser(req.Context(), admin))
	req.Header.Set("X-Agent-Session", "sess-b4")
	rr := httptest.NewRecorder()
	ah.APICreateComment(rr, req)
	if rr.Code != http.StatusOK && rr.Code != http.StatusCreated {
		t.Fatalf("create comment: got %d (%s)", rr.Code, rr.Body.String())
	}
	var c bson.M
	if err := db.Collection("content_comments").FindOne(ctx,
		bson.M{"content_id": cid}).Decode(&c); err != nil {
		t.Fatalf("comment row: %v", err)
	}
	if c["actor"] != "agent" || c["via"] != "api" || c["agent_session"] != "sess-b4" {
		t.Fatalf("comment provenance = %v (want agent/api/sess-b4)", c)
	}
}

// M10: single create/upsert reject published:true; updates allow only the
// no-flip restatement. M11: CAS losers map to 409, never 500.
func TestDraftOnlyAndVersionConflict(t *testing.T) {
	ah, db, cleanup, ids := setup2APublish(t, 1)
	defer cleanup()
	ctx := context.Background()
	admin := &auth.SessionUser{ID: primitive.NewObjectID().Hex(), Email: "admin@test", Role: "admin"}
	id := ids[0]

	var tpl bson.M
	if err := db.Collection("templates").FindOne(ctx, bson.M{}).Decode(&tpl); err != nil {
		t.Fatalf("template: %v", err)
	}
	callJSON := func(method, target string, vars map[string]string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, target, strings.NewReader(string(raw)))
		req.Header.Set("Content-Type", "application/json")
		req = mux.SetURLVars(req, vars)
		req = req.WithContext(middleware.InjectAPIUser(req.Context(), admin))
		rr := httptest.NewRecorder()
		switch {
		case strings.HasSuffix(target, "/content") && method == "POST":
			ah.APICreateContent(rr, req)
		default:
			ah.APIUpdateContent(rr, req)
		}
		return rr
	}
	// Create with published:true → 400 (bulk parity).
	if rr := callJSON("POST", "/api/v1/content", nil, map[string]any{
		"template_id": tpl["_id"].(primitive.ObjectID).Hex(),
		"title":       "M10", "slug": "m10-page", "published": true,
	}); rr.Code != http.StatusBadRequest {
		t.Fatalf("create published:true: got %d, want 400 (%s)", rr.Code, rr.Body.String())
	}
	// Update flipping false→true → 400.
	if rr := callJSON("PUT", "/api/v1/content/"+id.Hex(), map[string]string{"id": id.Hex()},
		map[string]any{"published": true}); rr.Code != http.StatusBadRequest {
		t.Fatalf("update flip to published: got %d, want 400 (%s)", rr.Code, rr.Body.String())
	}
	// Round-trip restatement on an already-live row → allowed.
	if _, err := db.Collection("content").UpdateOne(ctx, bson.M{"_id": id},
		bson.M{"$set": bson.M{"published": true}}); err != nil {
		t.Fatalf("pre-live row: %v", err)
	}
	if rr := callJSON("PUT", "/api/v1/content/"+id.Hex(), map[string]string{"id": id.Hex()},
		map[string]any{"title": "M10 retitled", "published": true}); rr.Code != http.StatusOK {
		t.Fatalf("live round-trip: got %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	// Concurrent bump between load and save → 409 CONFLICT (never 500).
	// UpdateContent re-reads the row internally, so a true CAS loss needs a
	// real race (covered by content_concurrency_2b_test.go); what M11 owns
	// is the deterministic mapping below — a lost CAS surfaces 409
	// CONFLICT with a machine-readable code instead of 500 INTERNAL_ERROR.
	for _, tc := range []struct {
		err  error
		code int
		want string
	}{
		{services.ErrVersionConflict, http.StatusConflict, "CONFLICT"},
		{errors.New("boom"), http.StatusInternalServerError, ""},
	} {
		rr := httptest.NewRecorder()
		ah.contentWriteError(rr, tc.err)
		if rr.Code != tc.code {
			t.Fatalf("contentWriteError(%v): got %d, want %d", tc.err, rr.Code, tc.code)
		}
		if tc.want != "" {
			var body map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || body["code"] != tc.want {
				t.Fatalf("contentWriteError code = %v, want %s", body, tc.want)
			}
		}
	}
}

// M12: copilot publish requires content.edit first (REST parity) — an
// unknown role must be denied on edit, not publish.
func TestCopilotPublishNeedsEdit(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()
	out, _ := h.executeCopilotTool(context.Background(), "ghost", "sess", "publish_content",
		map[string]interface{}{"id": primitive.NewObjectID().Hex()})
	if !strings.Contains(out, "content.edit") {
		t.Fatalf("ghost publish denial must cite content.edit, got: %s", out)
	}
}

// M9: admin delete writes an attributed version, an audit row, and fires
// the webhook contract (no more provenance-blind direct UpdateOne).
func TestDeleteContentAuditTrail(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()
	ctx := context.Background()
	tmplID := seedTemplate(t, h.db, "M9 Template", "m9-template")
	cid := primitive.NewObjectID()
	if _, err := h.db.Collection("content").InsertOne(ctx, bson.M{
		"_id": cid, "template_id": tmplID, "title": "M9", "slug": "m9-page",
		"full_path": "/m9-page", "published": false, "current_version": int64(1),
		"data": bson.M{}, "created_at": time.Now(), "updated_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rr := rbacPost(t, h.DeleteContent, nil, map[string]string{"id": cid.Hex()}, "editor")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("delete: got %d (%s)", rr.Code, rr.Body.String())
	}
	var ver bson.M
	if err := h.db.Collection("content_versions").FindOne(ctx,
		bson.M{"content_id": cid},
		options.FindOne().SetSort(bson.M{"version": -1})).Decode(&ver); err != nil {
		t.Fatalf("delete version row missing: %v", err)
	}
	if ver["comment"] != "删除页面" || ver["actor"] != "human" {
		t.Fatalf("delete version = %v (want 删除页面/human)", ver)
	}
	var audit bson.M
	// LogAsync is fire-and-forget: poll for the row (audit delivery is
	// not part of the request path).
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := h.db.Collection("audit_logs").FindOne(ctx, bson.M{
			"action": "content.delete", "resource_id": cid.Hex(),
		}).Decode(&audit)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("delete audit row missing: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// M6+M7: the standalone publish button is idempotent (same key replays,
// missing key fails closed) and attributed (human/ui); the checkbox path
// carries the CAS expectation (stale → 409, no duplicate).
func TestAdminPublishIdempotencyAndAttribution(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()
	db := testDB(t)
	ctx := context.Background()
	if err := db.EnsureProductIndexes(ctx); err != nil {
		t.Fatalf("EnsureProductIndexes: %v", err)
	}
	tplID := primitive.NewObjectID()
	if _, err := db.Collection("templates").InsertOne(ctx, bson.M{
		"_id": tplID, "name": "News", "slug": "news", "current_version": int64(1),
		"fields": bson.A{}, "created_at": time.Now(), "updated_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed template: %v", err)
	}
	tvID := primitive.NewObjectID()
	if _, err := db.Collection("template_versions").InsertOne(ctx, bson.M{
		"_id": tvID, "template_id": tplID, "version": int64(1),
		"slug": "news", "name": "News", "status": "active",
		"fields": []bson.M{}, "html_layout": "<html><body><h1>{{.title}}</h1></body></html>",
		"contract_hash": "sha256:c", "render_hash": "sha256:r",
		"created_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed template version: %v", err)
	}
	seedRow := func(slug, path string) primitive.ObjectID {
		t.Helper()
		cid := primitive.NewObjectID()
		if _, err := db.Collection("content").InsertOne(ctx, bson.M{
			"_id": cid, "template_id": tplID, "template_name": "News",
			"title": "M6 " + slug, "slug": slug, "full_path": path,
			"canonical_full_path": path, "path_scope": "live", "path_active": true,
			"current_version": int64(1), "data": bson.M{},
			"published": false, "created_at": time.Now(), "updated_at": time.Now(),
		}); err != nil {
			t.Fatalf("seed content: %v", err)
		}
		return cid
	}
	btnID := seedRow("m6-btn", "/m6-btn")
	boxID := seedRow("m6-box", "/m6-box")
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, col := range []string{"template_versions", "content_publications", "idempotency_records", "webhook_outbox"} {
			db.Collection(col).Drop(cctx) //nolint:errcheck
		}
	})

	root := t.TempDir()
	store := storage.NewFilesystemStore(root)
	_ = os.MkdirAll(filepath.Dir(filepath.Join(root, "x")), 0o755)
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
		Idem: idem, URLs: resolver, BuildSHA: "test-batch",
	})
	h.SetPublicationRuntime(pubs, idem, nil)

	vars := map[string]string{"id": btnID.Hex()}
	activeCount := func() int64 {
		t.Helper()
		n, err := db.Collection("content_publications").CountDocuments(ctx, bson.M{"status": "active"})
		if err != nil {
			t.Fatalf("count active: %v", err)
		}
		return n
	}
	attrOf := func(cid primitive.ObjectID) bson.M {
		t.Helper()
		var pub bson.M
		if err := db.Collection("content_publications").FindOne(ctx,
			bson.M{"content_id": cid, "status": "active"}).Decode(&pub); err != nil {
			t.Fatalf("active publication for %s: %v", cid.Hex(), err)
		}
		return pub
	}

	// (a) Button with render-time key → success, exactly one active, human/ui.
	key := newIdempotencyKey()
	rr := rbacPost(t, h.AdminProductPublish,
		url.Values{"idempotency_key": {key}}, vars, "admin")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "已发布") {
		t.Fatalf("button publish: got %d (%s)", rr.Code, rr.Body.String())
	}
	if n := activeCount(); n != 1 {
		t.Fatalf("active publications = %d, want 1", n)
	}
	if pub := attrOf(btnID); pub["actor"] != "human" || pub["via"] != "ui" {
		t.Fatalf("button publish attribution = %v (want human/ui)", pub)
	}
	firstPub := attrOf(btnID)["_id"]

	// (b) Same key again → replay: still exactly one active, same record.
	rr = rbacPost(t, h.AdminProductPublish,
		url.Values{"idempotency_key": {key}}, vars, "admin")
	if rr.Code != http.StatusOK {
		t.Fatalf("replay publish: got %d (%s)", rr.Code, rr.Body.String())
	}
	if n := activeCount(); n != 1 {
		t.Fatalf("replay minted a duplicate: active = %d", n)
	}
	if again := attrOf(btnID)["_id"]; again != firstPub {
		t.Fatal("replay must return the same publication")
	}

	// (c) No key (idem wired) → fail-closed failure page.
	rr = rbacPost(t, h.AdminProductPublish, url.Values{}, vars, "admin")
	if rr.Code == http.StatusOK && strings.Contains(rr.Body.String(), "已发布") {
		t.Fatal("keyless publish must fail closed")
	}
	if n := activeCount(); n != 1 {
		t.Fatalf("keyless publish mutated publications: active = %d", n)
	}

	// (d) Checkbox with a stale expectation → failure page, no duplicate.
	boxVars := map[string]string{"id": boxID.Hex()}
	rr = rbacPost(t, h.UpdateContent,
		url.Values{
			"title": {"M6 box"}, "slug": {"m6-box"}, "published": {"on"},
			"expected_active_id": {primitive.NewObjectID().Hex()},
		}, boxVars, "editor")
	if rr.Code == http.StatusSeeOther {
		t.Fatal("stale-expectation checkbox publish must not 303")
	}
	if n := activeCount(); n != 1 {
		t.Fatalf("stale checkbox publish minted: active = %d", n)
	}

	// (e) Checkbox with the live expectation → saga publish, human/ui.
	// (The box row starts unpublished, so the form omits expected_active_id.)
	if n, _ := db.Collection("content_publications").CountDocuments(ctx,
		bson.M{"content_id": boxID}); n != 0 {
		t.Fatalf("box row must start unpublished, has %d publications", n)
	}
	rr = rbacPost(t, h.UpdateContent,
		url.Values{"title": {"M6 box"}, "slug": {"m6-box"}, "published": {"on"}},
		boxVars, "editor")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("checkbox publish: got %d (%s)", rr.Code, rr.Body.String())
	}
	if pub := attrOf(boxID); pub["actor"] != "human" || pub["via"] != "ui" {
		t.Fatalf("checkbox publish attribution = %v (want human/ui)", pub)
	}
}
