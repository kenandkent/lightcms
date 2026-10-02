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
	"go.mongodb.org/mongo-driver/bson/primitive"
)

const httpLayout = `<html><head><title>{{.title}}</title></head><body><h1>{{.headline}}</h1></body></html>`

type httpSetup struct {
	gen *generation.Service
	h   *httpapi.Handlers
	db  interface {
		Collection(string) interface{}
	}
	dbRaw interface{}
}

func newHTTPSetup(t *testing.T) (*generation.Service, *httpapi.Handlers) {
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
	gen := generation.NewService(db, generation.Options{
		Templates: tpls, Pubs: saga, PubRepo: repo, Idem: idem, URLs: resolver,
	})
	actor := generation.Actor{Role: "admin", ID: "u1", Email: "u@e.com", Authenticated: true, Scopes: []string{}}
	h := &httpapi.Handlers{
		Gen: gen,
		ActorExtractor: func(r *http.Request) (generation.Actor, error) {
			return actor, nil
		},
		IdempotencyExtractor: func(r *http.Request) (string, bool) {
			if k := r.Header.Get("Idempotency-Key"); k != "" {
				return k, true
			}
			return "", false
		},
	}
	// stash db/root for helpers via context? return gen + h; tests use db directly.
	t.Cleanup(func() {})
	_ = root
	_ = dbRaw(db)
	return gen, h
}

func dbRaw(db interface{}) interface{} { return db }

func seedHTTPTemplate(t *testing.T, gen *generation.Service, db interface {
	Collection(string) interface{}
}) {
	t.Helper()
}

func doRequest(h http.HandlerFunc, method, path string, body any, headers map[string]string, vars map[string]string) *httptest.ResponseRecorder {
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

func TestHTTP_GenerateDraftPreview(t *testing.T) {
	db, _ := testutil.MustConnectTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = db.EnsureProductIndexes(ctx)
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
		HTMLLayout: httpLayout,
		Fields:     []models.TemplateField{{Name: "headline", Label: "H", Type: "text", Required: true}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	actor := generation.Actor{Role: "admin", ID: "u1", Email: "u@e.com", Authenticated: true, Scopes: []string{}}
	h := &httpapi.Handlers{Gen: gen,
		ActorExtractor: func(r *http.Request) (generation.Actor, error) { return actor, nil },
		IdempotencyExtractor: func(r *http.Request) (string, bool) {
			if k := r.Header.Get("Idempotency-Key"); k != "" {
				return k, true
			}
			return "", false
		},
	}

	// draft → 201, requires_publish=true
	rr := doRequest(h.HandleGenerate, "POST", "/api/v1/page-generation",
		map[string]any{"template": "financial-news", "title": "T", "slug": "http-draft", "folder_path": "/n", "mode": "draft", "data": map[string]any{"headline": "h"}},
		nil, nil)
	if rr.Code != 201 {
		t.Fatalf("draft status = %d, body %s", rr.Code, rr.Body.String())
	}
	var draft map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &draft)
	if draft["requires_publish"] != true || draft["published"] != false {
		t.Fatalf("draft flags: %v", draft)
	}

	// preview → 200, requires_publish=false, writes nothing
	before, _ := db.Collection("content").CountDocuments(ctx, bson.M{})
	rr = doRequest(h.HandleGenerate, "POST", "/api/v1/page-generation",
		map[string]any{"template": "financial-news", "title": "P", "slug": "http-prev", "folder_path": "/n", "mode": "preview", "data": map[string]any{"headline": "h"}},
		nil, nil)
	if rr.Code != 200 {
		t.Fatalf("preview status = %d %s", rr.Code, rr.Body.String())
	}
	var prev map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &prev)
	if prev["requires_publish"] != false {
		t.Fatalf("preview requires_publish: %v", prev)
	}
	after, _ := db.Collection("content").CountDocuments(ctx, bson.M{})
	if after != before {
		t.Fatal("preview wrote content")
	}

	// unknown top-level → 422 FIELD_VALIDATION_FAILED
	rr = doRequest(h.HandleGenerate, "POST", "/api/v1/page-generation",
		map[string]any{"template": "financial-news", "title": "T", "slug": "x", "folder_path": "/n", "mode": "draft", "data": map[string]any{"headline": "h"}, "bogus": 1},
		nil, nil)
	if rr.Code != 422 {
		t.Fatalf("unknown top: %d %s", rr.Code, rr.Body.String())
	}
	var ebody map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &ebody)
	if em, ok := ebody["error"].(map[string]any); !ok || em["code"] != "FIELD_VALIDATION_FAILED" {
		t.Fatalf("unknown top code: %v", ebody)
	}

	// null data → 422
	rr = doRequest(func(w http.ResponseWriter, r *http.Request) {
		// raw null data body
		raw := `{"template":"financial-news","title":"T","slug":"null-http","folder_path":"/n","mode":"draft","data":null}`
		req := httptest.NewRequest("POST", "/api/v1/page-generation", bytes.NewBufferString(raw))
		h.HandleGenerate(w, req)
	}, "POST", "/x", nil, nil, nil)
	if rr.Code != 422 {
		t.Fatalf("null data: %d %s", rr.Code, rr.Body.String())
	}

	// unauthenticated → 401
	h401 := &httpapi.Handlers{Gen: gen,
		ActorExtractor: func(r *http.Request) (generation.Actor, error) { return generation.Actor{Role: "admin"}, nil },
	}
	rr = doRequest(h401.HandleGenerate, "POST", "/api/v1/page-generation",
		map[string]any{"template": "financial-news", "title": "T", "slug": "a", "folder_path": "/n", "mode": "draft", "data": map[string]any{"headline": "h"}},
		nil, nil)
	if rr.Code != 401 {
		t.Fatalf("401: %d", rr.Code)
	}

	// publish without key/version → 428
	rr = doRequest(h.HandleGenerate, "POST", "/api/v1/page-generation",
		map[string]any{"template": "financial-news", "title": "P", "slug": "pub428", "folder_path": "/n", "mode": "publish", "data": map[string]any{"headline": "h"}},
		nil, nil)
	if rr.Code != 428 {
		t.Fatalf("428: %d %s", rr.Code, rr.Body.String())
	}
}

func TestHTTP_SchemaETag(t *testing.T) {
	db, _ := testutil.MustConnectTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = db.EnsureProductIndexes(ctx)
	root := t.TempDir()
	store := storage.NewFilesystemStore(root)
	tpls := templatecontract.NewService(db)
	repo := publication.NewRepository(db, nil)
	base, _ := url.Parse("http://localhost:8080")
	resolver, _ := publicurl.NewResolver(base)
	idem, _ := idempotency.NewService(db, idempotency.Options{})
	saga := publication.NewService(db, repo, store, publication.Options{Templates: tpls, Idem: idem, URLs: resolver})
	gen := generation.NewService(db, generation.Options{Templates: tpls, Pubs: saga, PubRepo: repo, Idem: idem, URLs: resolver})
	tvBefore, _, err := tpls.Create(ctx, templatecontract.TemplateInput{
		Slug: "financial-news", Name: "FN", Category: "news", Status: "active",
		HTMLLayout: httpLayout,
		Fields:     []models.TemplateField{{Name: "headline", Label: "H", Type: "text", Required: true}},
	})
	_ = tvBefore
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	actor := generation.Actor{Role: "admin", ID: "u1", Email: "u@e.com", Authenticated: true, Scopes: []string{}}
	h := &httpapi.Handlers{Gen: gen,
		ActorExtractor: func(r *http.Request) (generation.Actor, error) { return actor, nil },
	}
	rr := doRequest(h.HandleTemplateSchema, "GET", "/api/v1/templates/financial-news/schema", nil, nil,
		map[string]string{"slug": "financial-news"})
	if rr.Code != 200 {
		t.Fatalf("schema: %d %s", rr.Code, rr.Body.String())
	}
	etag := rr.Header().Get("ETag")
	if etag != `"template-version-1"` {
		t.Fatalf("ETag = %q, want template-version-1", etag)
	}
	var body map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if int(body["template_version"].(float64)) != 1 {
		t.Fatalf("schema version: %v", body)
	}
	js, _ := body["json_schema"].(map[string]any)
	if js["additionalProperties"] != false {
		t.Fatalf("additionalProperties: %v", js)
	}
	// Bump → ETag moves with the same version object.
	tplRec, _ := templatecontract.NewRepository(db).FindTemplateBySlug(ctx, "financial-news")
	_, err = tpls.Update(ctx, tplRec.ID, 1, templatecontract.TemplateInput{
		Slug: "financial-news", Name: "FN", Category: "news", Status: "active",
		HTMLLayout: httpLayout + "<!-- v2 -->",
		Fields:     []models.TemplateField{{Name: "headline", Label: "H", Type: "text", Required: true}},
	})
	if err != nil {
		t.Fatalf("bump: %v", err)
	}
	rr = doRequest(h.HandleTemplateSchema, "GET", "/api/v1/templates/financial-news/schema", nil, nil,
		map[string]string{"slug": "financial-news"})
	if rr.Header().Get("ETag") != `"template-version-2"` {
		t.Fatalf("ETag2 = %q", rr.Header().Get("ETag"))
	}
	_ = tvBefore
}

func TestHTTP_PublishScopesAnd403ZeroMutation(t *testing.T) {
	db, _ := testutil.MustConnectTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = db.EnsureProductIndexes(ctx)
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
		HTMLLayout: httpLayout,
		Fields:     []models.TemplateField{{Name: "headline", Label: "H", Type: "text", Required: true}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	pubOnly := generation.Actor{Role: "admin", ID: "k-pub", Email: "p@e.com", Authenticated: true, Scopes: []string{generation.ScopeContentPublish}}
	h := &httpapi.Handlers{Gen: gen,
		ActorExtractor:       func(r *http.Request) (generation.Actor, error) { return pubOnly, nil },
		IdempotencyExtractor: func(r *http.Request) (string, bool) { return "k-http-403", true },
	}
	c0, _ := db.Collection("content").CountDocuments(ctx, bson.M{})
	rr := doRequest(h.HandleGenerate, "POST", "/api/v1/page-generation",
		map[string]any{"template": "financial-news", "title": "P", "slug": "http403", "folder_path": "/n",
			"mode": "publish", "expected_template_version": 1, "data": map[string]any{"headline": "h"}},
		map[string]string{"Idempotency-Key": "k-http-403"}, nil)
	if rr.Code != 403 {
		t.Fatalf("403: %d %s", rr.Code, rr.Body.String())
	}
	c1, _ := db.Collection("content").CountDocuments(ctx, bson.M{})
	if c1 != c0 {
		t.Fatal("403 mutated content")
	}
	var ebody map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &ebody)
	if em, _ := ebody["error"].(map[string]any); em["code"] != "PERMISSION_DENIED" {
		t.Fatalf("403 code: %v", ebody)
	}
	_ = root
}

func TestHTTP_PublicationsAndRollback(t *testing.T) {
	db, _ := testutil.MustConnectTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = db.EnsureProductIndexes(ctx)
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
		HTMLLayout: httpLayout,
		Fields:     []models.TemplateField{{Name: "headline", Label: "H", Type: "text", Required: true}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	admin := generation.Actor{Role: "admin", ID: "a1", Email: "a@e.com", Authenticated: true, IsAdmin: true, Scopes: []string{}}
	// Publish v1 via service (needs idempotency ctx).
	raw1, _ := json.Marshal(map[string]any{"t": 1})
	pctx1 := generation.WithIdempotency(ctx, generation.IdempotencyParams{Owner: "a1", Method: "POST", Path: "/api/v1/page-generation", Key: "k-pub-list-1", Body: raw1})
	r1, err := gen.Generate(pctx1, admin, generation.GenerateRequest{
		Template: "financial-news", Title: "L", Slug: "list-page", FolderPath: "/n",
		Mode: "publish", ExpectedTemplateVersion: func() *int64 { v := int64(1); return &v }(), Data: map[string]any{"headline": "v1"},
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	h := &httpapi.Handlers{Gen: gen,
		ActorExtractor: func(r *http.Request) (generation.Actor, error) { return admin, nil },
		IdempotencyExtractor: func(r *http.Request) (string, bool) {
			if k := r.Header.Get("Idempotency-Key"); k != "" {
				return k, true
			}
			return "", false
		},
	}
	cid := r1.ID
	// list → 200
	rr := doRequest(h.HandleListPublications, "GET", "/api/v1/content/"+cid+"/publications", nil, nil, map[string]string{"id": cid})
	if rr.Code != 200 {
		t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
	}
	// detail → 200
	pid := *r1.PublicationID
	rr = doRequest(h.HandleGetPublication, "GET", "/x", nil, nil, map[string]string{"id": cid, "publication_id": pid})
	if rr.Code != 200 {
		t.Fatalf("detail: %d %s", rr.Code, rr.Body.String())
	}
	// rollback without key → 428
	rr = doRequest(h.HandleRollback, "POST", "/x", map[string]any{}, nil, map[string]string{"id": cid, "publication_id": pid})
	if rr.Code != 428 {
		t.Fatalf("rollback 428: %d %s", rr.Code, rr.Body.String())
	}
	// rollback with key → 200, new publication
	rr = doRequest(h.HandleRollback, "POST", "/x", map[string]any{}, map[string]string{"Idempotency-Key": "k-rb-http-1"}, map[string]string{"id": cid, "publication_id": pid})
	if rr.Code != 200 {
		t.Fatalf("rollback: %d %s", rr.Code, rr.Body.String())
	}
	var rb map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &rb)
	if rb["publication_id"] == pid {
		t.Fatal("rollback must mint new publication")
	}
	_ = root
}

func TestHTTP_MigrateSlugAndUpgrade(t *testing.T) {
	db, _ := testutil.MustConnectTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = db.EnsureProductIndexes(ctx)
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
	tplRec, _, err := func() (interface{}, interface{}, error) {
		tpl, ver, err := tpls.Create(ctx, templatecontract.TemplateInput{
			Slug: "financial-news", Name: "FN", Category: "news", Status: "active",
			HTMLLayout: httpLayout,
			Fields:     []models.TemplateField{{Name: "headline", Label: "H", Type: "text", Required: true}},
		})
		return tpl, ver, err
	}()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = tplRec
	admin := generation.Actor{Role: "admin", ID: "a1", Email: "a@e.com", Authenticated: true, IsAdmin: true, Scopes: []string{}}
	h := &httpapi.Handlers{Gen: gen,
		ActorExtractor: func(r *http.Request) (generation.Actor, error) { return admin, nil },
	}
	// migrate-slug collision: seed second template first.
	if _, _, err := tpls.Create(ctx, templatecontract.TemplateInput{
		Slug: "taken-slug", Name: "T", Category: "news", Status: "active",
		HTMLLayout: httpLayout,
		Fields:     []models.TemplateField{{Name: "headline", Label: "H", Type: "text", Required: true}},
	}); err != nil {
		t.Fatalf("seed2: %v", err)
	}
	tplRec2, _ := templatecontract.NewRepository(db).FindTemplateBySlug(ctx, "financial-news")
	rr := doRequest(h.HandleMigrateSlug, "POST", "/x",
		map[string]any{"new_slug": "taken-slug"}, nil, map[string]string{"id": tplRec2.ID.Hex()})
	if rr.Code != 409 {
		t.Fatalf("migrate collision: %d %s", rr.Code, rr.Body.String())
	}
	// upgrade preview is read-only.
	rr = doRequest(h.HandleUpgradePreview, "GET", "/x", nil, nil, map[string]string{"slug": "financial-news"})
	if rr.Code != 200 {
		t.Fatalf("preview: %d %s", rr.Code, rr.Body.String())
	}
	_ = primitive.ObjectID{}
	_ = root
}
