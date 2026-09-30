package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/generation"
	"github.com/jonradoff/lightcms/v7/internal/product/httpapi"
	"github.com/jonradoff/lightcms/v7/internal/product/idempotency"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/publicurl"
	"github.com/jonradoff/lightcms/v7/internal/product/storage"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"
	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"go.mongodb.org/mongo-driver/bson"
)

const extraLayout = `<html><head><title>{{.title}}</title></head><body><h1>{{.headline}}</h1></body></html>`

func newExtraSetup(t *testing.T) (*generation.Service, *httpapi.Handlers, generation.Actor) {
	t.Helper()
	db, _ := testutil.MustConnectTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.EnsureProductIndexes(ctx); err != nil {
		t.Fatalf("indexes: %v", err)
	}
	_ = db.Collection(generation.CollectionUpgradeJobs).Drop(ctx)
	root := t.TempDir()
	store := storage.NewFilesystemStore(root)
	tpls := templatecontract.NewService(db)
	repo := publication.NewRepository(db, nil)
	base, _ := url.Parse("http://localhost:8080")
	resolver, _ := publicurl.NewResolver(base)
	idem, _ := idempotency.NewService(db, idempotency.Options{})
	saga := publication.NewService(db, repo, store, publication.Options{Templates: tpls, Idem: idem, URLs: resolver})
	gen := generation.NewService(db, generation.Options{Templates: tpls, Pubs: saga, PubRepo: repo, Idem: idem, URLs: resolver})
	if _, _, err := tpls.Create(ctx, templatecontract.TemplateInput{
		Slug: "financial-news", Name: "FN", Category: "news", Status: "active",
		HTMLLayout: extraLayout,
		Fields: []models.TemplateField{{Name: "headline", Label: "H", Type: "text", Required: true}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	admin := generation.Actor{ID: "a1", Email: "a@e.com", Authenticated: true, IsAdmin: true, Scopes: []string{}}
	h := &httpapi.Handlers{Gen: gen,
		ActorExtractor: func(r *http.Request) (generation.Actor, error) { return admin, nil },
		IdempotencyExtractor: func(r *http.Request) (string, bool) {
			if k := r.Header.Get("Idempotency-Key"); k != "" {
				return k, true
			}
			return "", false
		},
	}
	_ = root
	_ = db
	return gen, h, admin
}

func doExtra(h http.HandlerFunc, method, path string, body any, headers map[string]string, vars map[string]string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if vars != nil {
		req = mux.SetURLVars(req, vars)
	}
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

// TestHTTP_StaleVersionZeroMutation proves publish after a template increment
// returns 409 TEMPLATE_VERSION_CHANGED with zero mutation via HTTP.
func TestHTTP_StaleVersionZeroMutation(t *testing.T) {
	db, _ := testutil.MustConnectTestDB(t)
	gen, h, _ := newExtraSetup(t)
	ctx := context.Background()
	c0, _ := db.Collection("content").CountDocuments(ctx, bson.M{})
	// Bump template to v2.
	tplRec, _ := templatecontract.NewRepository(db).FindTemplateBySlug(ctx, "financial-news")
	tpls := templatecontract.NewService(db)
	_, err := tpls.Update(ctx, tplRec.ID, 1, templatecontract.TemplateInput{
		Slug: "financial-news", Name: "FN", Category: "news", Status: "active",
		HTMLLayout: extraLayout + "<!-- v2 -->",
		Fields: []models.TemplateField{{Name: "headline", Label: "H", Type: "text", Required: true}},
	})
	if err != nil {
		t.Fatalf("bump: %v", err)
	}
	stale := int64(1)
	rr := doExtra(h.HandleGenerate, "POST", "/api/v1/page-generation",
		map[string]any{"template": "financial-news", "title": "S", "slug": "stale-http", "folder_path": "/n",
			"mode": "publish", "expected_template_version": stale, "data": map[string]any{"headline": "h"}},
		map[string]string{"Idempotency-Key": "k-stale-http"}, nil)
	if rr.Code != 409 {
		t.Fatalf("stale: %d %s", rr.Code, rr.Body.String())
	}
	var ebody map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &ebody)
	if em, _ := ebody["error"].(map[string]any); em["code"] != "TEMPLATE_VERSION_CHANGED" {
		t.Fatalf("code: %v", ebody)
	}
	_ = gen
	if c1, _ := db.Collection("content").CountDocuments(ctx, bson.M{}); c1 != c0 {
		t.Fatal("stale mutated via HTTP")
	}
}

// TestHTTP_RestoreRevertDistinct proves the two handlers are distinct commands
// with idempotency + preconditions.
func TestHTTP_RestoreRevertDistinct(t *testing.T) {
	db, _ := testutil.MustConnectTestDB(t)
	gen, h, admin := newExtraSetup(t)
	ctx := context.Background()
	_ = admin
	// Publish v1 + v2 via service.
	raw := []byte(`{"t":1}`)
	pctx := generation.WithIdempotency(ctx, generation.IdempotencyParams{Owner: "a1", Method: "POST", Path: "/api/v1/page-generation", Key: "k-rr-http-1", Body: raw})
	v1 := int64(1)
	r1, err := gen.Generate(pctx, generation.Actor{ID: "a1", Email: "a@e.com", Authenticated: true, IsAdmin: true, Scopes: []string{}},
		generation.GenerateRequest{Template: "financial-news", Title: "RR", Slug: "rr-http", FolderPath: "/n",
			Mode: "publish", ExpectedTemplateVersion: &v1, Data: map[string]any{"headline": "v1"}})
	if err != nil {
		t.Fatalf("v1: %v", err)
	}
	cid := r1.ID
	pid := *r1.PublicationID
	raw2 := []byte(`{"t":2}`)
	pctx2 := generation.WithIdempotency(ctx, generation.IdempotencyParams{Owner: "a1", Method: "POST", Path: "/api/v1/page-generation", Key: "k-rr-http-2", Body: raw2})
	_, err = gen.Generate(pctx2, generation.Actor{ID: "a1", Email: "a@e.com", Authenticated: true, IsAdmin: true, Scopes: []string{}},
		generation.GenerateRequest{Template: "financial-news", Title: "RR", Slug: "rr-http", FolderPath: "/n",
			Mode: "publish", Upsert: true, ExpectedTemplateVersion: &v1, Data: map[string]any{"headline": "v2"}})
	if err != nil {
		t.Fatalf("v2: %v", err)
	}
	// restore without key → 428
	rr := doExtra(h.HandleRestoreAndPublish, "POST", "/x", map[string]any{"version": 1}, nil, map[string]string{"id": cid})
	if rr.Code != 428 {
		t.Fatalf("restore 428: %d %s", rr.Code, rr.Body.String())
	}
	// restore with key → 200, mode distinct
	rr = doExtra(h.HandleRestoreAndPublish, "POST", "/x", map[string]any{"version": 1},
		map[string]string{"Idempotency-Key": "k-restore-http-1"}, map[string]string{"id": cid})
	if rr.Code != 200 {
		t.Fatalf("restore: %d %s", rr.Code, rr.Body.String())
	}
	var rout map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &rout)
	if rout["mode"] != "restore_and_publish" {
		t.Fatalf("restore mode: %v", rout)
	}
	// revert without key → 428
	rr = doExtra(h.HandleRevertLive, "POST", "/x", map[string]any{"source_publication_id": pid}, nil, map[string]string{"id": cid})
	if rr.Code != 428 {
		t.Fatalf("revert 428: %d %s", rr.Code, rr.Body.String())
	}
	// revert with key → 200, mode distinct and different publication from restore
	rr = doExtra(h.HandleRevertLive, "POST", "/x", map[string]any{"source_publication_id": pid},
		map[string]string{"Idempotency-Key": "k-revert-http-1"}, map[string]string{"id": cid})
	if rr.Code != 200 {
		t.Fatalf("revert: %d %s", rr.Code, rr.Body.String())
	}
	var vout map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &vout)
	if vout["mode"] != "revert_live" {
		t.Fatalf("revert mode: %v", vout)
	}
	if vout["publication_id"] == rout["publication_id"] {
		t.Fatal("restore and revert must mint distinct publications")
	}
	_ = db
}

// TestHTTP_ErrorRetryAfter proves 429/503/409 set Retry-After.
func TestHTTP_ErrorRetryAfter(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/x", nil)
	httpapi.WriteError(rr, req, &generation.Error{Code: generation.CodeRateLimited, Message: "slow", RetryAfter: 7})
	if rr.Code != 429 || rr.Header().Get("Retry-After") != "7" {
		t.Fatalf("429 mapping: %d %q", rr.Code, rr.Header().Get("Retry-After"))
	}
	rr = httptest.NewRecorder()
	httpapi.WriteError(rr, req, &generation.Error{Code: generation.CodeRequestInProgress, Message: "busy"})
	if rr.Code != 409 || rr.Header().Get("Retry-After") == "" {
		t.Fatalf("409 retry: %d %q", rr.Code, rr.Header().Get("Retry-After"))
	}
	rr = httptest.NewRecorder()
	httpapi.WriteError(rr, req, &generation.Error{Code: generation.CodeStoreUnavailable, Message: "down", RetryAfter: 30})
	if rr.Code != 503 || rr.Header().Get("Retry-After") != "30" {
		t.Fatalf("503 mapping: %d %q", rr.Code, rr.Header().Get("Retry-After"))
	}
	// status table spot checks for Task 18.
	cases := map[string]int{
		generation.CodeUnauthenticated: 401, generation.CodePermissionDenied: 403,
		generation.CodeTemplateNotFound: 404, generation.CodeFieldValidationFailed: 422,
		generation.CodeTemplatePreconditionRequired: 428, generation.CodeIdempotencyKeyRequired: 428,
		generation.CodePathConflict: 409, generation.CodeTemplateNotActive: 409,
		generation.CodeTemplateVersionChanged: 409, generation.CodeAgentSandboxRequired: 409,
	}
	for code, want := range cases {
		if got := generation.StatusForCode(code); got != want {
			t.Fatalf("status %s = %d, want %d", code, got, want)
		}
	}
}
