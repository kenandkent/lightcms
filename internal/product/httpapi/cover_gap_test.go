package httpapi_test

// Task 17B coverage-gap tests for internal/product/httpapi (external).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/jonradoff/lightcms/v7/internal/product/generation"
	"github.com/jonradoff/lightcms/v7/internal/product/httpapi"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"
	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func gapAdminHandlers(t *testing.T, gen *generation.Service, actor generation.Actor, idem func(r *http.Request) (string, bool)) *httpapi.Handlers {
	t.Helper()
	return &httpapi.Handlers{
		Gen: gen,
		ActorExtractor: func(r *http.Request) (generation.Actor, error) {
			return actor, nil
		},
		IdempotencyExtractor: idem,
	}
}

func TestCoverGapUpgradeJobHandlers(t *testing.T) {
	gen, h, admin := newExtraSetup(t)

	// Missing slug → 400.
	if rr := doExtra(h.HandleStartUpgradeJob, "POST", "/x", nil, nil, nil); rr.Code != 400 {
		t.Fatalf("start missing slug: %d", rr.Code)
	}
	if rr := doExtra(h.HandleUpgradePreview, "GET", "/x", nil, nil, nil); rr.Code != 400 {
		t.Fatalf("preview missing slug: %d", rr.Code)
	}
	// Unknown slug → 404.
	if rr := doExtra(h.HandleStartUpgradeJob, "POST", "/x", nil, nil, map[string]string{"slug": "nope"}); rr.Code != 404 {
		t.Fatalf("start unknown slug: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doExtra(h.HandleUpgradePreview, "GET", "/x", nil, nil, map[string]string{"slug": "nope"}); rr.Code != 404 {
		t.Fatalf("preview unknown slug: %d", rr.Code)
	}
	// Non-admin actor → 403 service error.
	plain := gapAdminHandlers(t, gen,
		generation.Actor{Role: "admin", ID: "u", Email: "u@e.com", Authenticated: true, Scopes: []string{"template.edit"}},
		nil)
	if rr := doExtra(plain.HandleStartUpgradeJob, "POST", "/x", nil, nil, map[string]string{"slug": "financial-news"}); rr.Code != 403 {
		t.Fatalf("start non-admin: %d %s", rr.Code, rr.Body.String())
	}
	// Admin start → 201.
	rr := doExtra(h.HandleStartUpgradeJob, "POST", "/x", nil, nil, map[string]string{"slug": "financial-news"})
	if rr.Code != 201 {
		t.Fatalf("start: %d %s", rr.Code, rr.Body.String())
	}
	var started map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &started); err != nil {
		t.Fatalf("start body: %v", err)
	}
	jobID, _ := started["id"].(string)
	if jobID == "" {
		if jid, ok := started["job_id"].(string); ok {
			jobID = jid
		}
	}
	if jobID == "" {
		t.Fatalf("start response has no job id: %v", started)
	}

	// Get: bad hex, unknown id, alias var, success.
	if rr := doExtra(h.HandleGetUpgradeJob, "GET", "/x", nil, nil, map[string]string{"job_id": "zzz"}); rr.Code != 400 {
		t.Fatalf("get bad hex: %d", rr.Code)
	}
	if rr := doExtra(h.HandleGetUpgradeJob, "GET", "/x", nil, nil, map[string]string{"job_id": primitive.NewObjectID().Hex()}); rr.Code != 404 {
		t.Fatalf("get unknown: %d", rr.Code)
	}
	if rr := doExtra(h.HandleGetUpgradeJob, "GET", "/x", nil, nil, map[string]string{"id": jobID}); rr.Code != 200 {
		t.Fatalf("get via id alias: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doExtra(h.HandleGetUpgradeJob, "GET", "/x", nil, nil, map[string]string{"job_id": jobID}); rr.Code != 200 {
		t.Fatalf("get: %d %s", rr.Code, rr.Body.String())
	}
	// Run: bad hex, unknown id, alias var, success (no pages → completes).
	if rr := doExtra(h.HandleRunUpgradeJob, "POST", "/x", nil, nil, map[string]string{"job_id": "zzz"}); rr.Code != 400 {
		t.Fatalf("run bad hex: %d", rr.Code)
	}
	if rr := doExtra(h.HandleRunUpgradeJob, "POST", "/x", nil, nil, map[string]string{"job_id": primitive.NewObjectID().Hex()}); rr.Code != 404 {
		t.Fatalf("run unknown: %d", rr.Code)
	}
	if rr := doExtra(h.HandleRunUpgradeJob, "POST", "/x", nil, nil, map[string]string{"id": jobID}); rr.Code != 200 {
		t.Fatalf("run via id alias: %d %s", rr.Code, rr.Body.String())
	}
	// Preview success.
	if rr := doExtra(h.HandleUpgradePreview, "GET", "/x", nil, nil, map[string]string{"slug": "financial-news"}); rr.Code != 200 {
		t.Fatalf("preview: %d %s", rr.Code, rr.Body.String())
	}
	_ = admin
}

func TestCoverGapActorBranches(t *testing.T) {
	gen, _, _ := newExtraSetup(t)
	// Extractor error → 401.
	badEx := gapAdminHandlers(t, gen, generation.Actor{Role: "admin"}, func(r *http.Request) (string, bool) {
		return "", false
	})
	badEx.ActorExtractor = func(r *http.Request) (generation.Actor, error) {
		return generation.Actor{Role: "admin"}, errors.New("no token")
	}
	if rr := doExtra(badEx.HandleGenerate, "POST", "/x", map[string]any{"template": "t"}, nil, nil); rr.Code != 401 {
		t.Fatalf("extractor error: %d", rr.Code)
	}
	// Unauthenticated actor → 401.
	anon := gapAdminHandlers(t, gen, generation.Actor{Role: "admin"}, nil)
	if rr := doExtra(anon.HandleListPublications, "GET", "/x", nil, nil,
		map[string]string{"id": primitive.NewObjectID().Hex()}); rr.Code != 401 {
		t.Fatalf("anonymous: %d", rr.Code)
	}
	// Nil extractor + context actor → proceeds (invalid content id → 400, not 401).
	var nilEx httpapi.Handlers
	nilEx.Gen = gen
	req := httptest.NewRequest("GET", "/x", nil)
	req = mux.SetURLVars(req, map[string]string{"id": "zzz"})
	ctx := generation.WithActor(req.Context(),
		generation.Actor{Role: "admin", ID: "u", Authenticated: true, Scopes: []string{}})
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()
	nilEx.HandleListPublications(rr, req)
	if rr.Code != 400 {
		t.Fatalf("ctx actor passthrough: %d", rr.Code)
	}
	// Nil extractor + no actor → 401.
	req2 := httptest.NewRequest("GET", "/x", nil)
	rr2 := httptest.NewRecorder()
	nilEx.HandleGetPublication(rr2, req2)
	if rr2.Code != 401 {
		t.Fatalf("nil extractor no actor: %d", rr2.Code)
	}
}

func rawGapRequest(t *testing.T, h http.HandlerFunc, method, path, rawBody string, vars map[string]string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if rawBody == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(rawBody)
	}
	req := httptest.NewRequest(method, path, reader)
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

func TestCoverGapGenerateBodyBranches(t *testing.T) {
	_, h, _ := newExtraSetup(t)
	// Empty body → 400.
	if rr := rawGapRequest(t, h.HandleGenerate, "POST", "/x", "", nil, nil); rr.Code != 400 {
		t.Fatalf("empty body: %d", rr.Code)
	}
	// Malformed JSON → 400.
	if rr := rawGapRequest(t, h.HandleGenerate, "POST", "/x", "{bad", nil, nil); rr.Code != 400 {
		t.Fatalf("malformed JSON: %d", rr.Code)
	}
	// Unknown top-level field → 422.
	if rr := doExtra(h.HandleGenerate, "POST", "/x", map[string]any{"template": "t", "bogus": 1}, nil, nil); rr.Code != 422 {
		t.Fatalf("unknown field: %d", rr.Code)
	}
	// Explicit null data → 422.
	if rr := rawGapRequest(t, h.HandleGenerate, "POST", "/x", `{"template":"t","data":null}`, nil, nil); rr.Code != 422 {
		t.Fatalf("null data: %d", rr.Code)
	}
	// Template not found → error log path.
	if rr := doExtra(h.HandleGenerate, "POST", "/x",
		map[string]any{"template": "missing", "title": "T", "slug": "s", "folder_path": "/n",
			"mode": "draft", "data": map[string]any{"headline": "h"}}, nil, nil); rr.Code != 404 {
		t.Fatalf("missing template: %d", rr.Code)
	}
	// Publish without key → 428.
	if rr := doExtra(h.HandleGenerate, "POST", "/x",
		map[string]any{"template": "financial-news", "title": "T", "slug": "gap-pub", "folder_path": "/n",
			"mode": "publish", "data": map[string]any{"headline": "h"}}, nil, nil); rr.Code != 428 {
		t.Fatalf("publish no key: %d", rr.Code)
	}
	// Publish with key → 201 created.
	if rr := doExtra(h.HandleGenerate, "POST", "/x",
		map[string]any{"template": "financial-news", "title": "T", "slug": "gap-pub", "folder_path": "/n",
			"mode": "publish", "expected_template_version": 1, "data": map[string]any{"headline": "h"}},
		map[string]string{"Idempotency-Key": "gap-pub-key-1"}, nil); rr.Code != 201 {
		t.Fatalf("publish with key: %d %s", rr.Code, rr.Body.String())
	}
}

func TestCoverGapSchemaAndMigrateBranches(t *testing.T) {
	gen, h, _ := newExtraSetup(t)
	// Scope-denied actor → 403.
	denied := gapAdminHandlers(t, gen,
		generation.Actor{Role: "admin", ID: "u", Email: "u@e.com", Authenticated: true, Scopes: []string{"content.view"}}, nil)
	if rr := doExtra(denied.HandleTemplateSchema, "GET", "/x", nil, nil, map[string]string{"slug": "financial-news"}); rr.Code != 403 {
		t.Fatalf("schema scope denied: %d", rr.Code)
	}
	if rr := doExtra(denied.HandleListPublications, "GET", "/x", nil, nil,
		map[string]string{"id": primitive.NewObjectID().Hex()}); rr.Code == 403 {
		// content.view present → proceeds; must not be 403.
		t.Fatalf("list with content.view scope: %d", rr.Code)
	}
	// Slug fallback from URL path (no mux vars).
	if rr := rawGapRequest(t, h.HandleTemplateSchema, "GET", "/api/v1/templates/financial-news/schema", "", nil, nil); rr.Code != 200 {
		t.Fatalf("schema path fallback: %d %s", rr.Code, rr.Body.String())
	}
	if rr := rawGapRequest(t, h.HandleTemplateSchema, "GET", "/api/v1/templates//schema", "", nil, nil); rr.Code == 200 {
		t.Fatalf("schema empty slug: want error, got 200")
	}
	if rr := doExtra(h.HandleTemplateSchema, "GET", "/x", nil, nil, map[string]string{"slug": "missing"}); rr.Code != 404 {
		t.Fatalf("schema unknown: %d", rr.Code)
	}
	// Migrate slug: missing id, bad hex, missing new_slug.
	if rr := doExtra(h.HandleMigrateSlug, "POST", "/x", map[string]any{"new_slug": "x"}, nil, nil); rr.Code != 400 {
		t.Fatalf("migrate missing id: %d", rr.Code)
	}
	if rr := doExtra(h.HandleMigrateSlug, "POST", "/x", map[string]any{"new_slug": "x"}, nil, map[string]string{"id": "zzz"}); rr.Code != 400 {
		t.Fatalf("migrate bad hex: %d", rr.Code)
	}
	tplID := seedGapTemplate(t)
	if rr := doExtra(h.HandleMigrateSlug, "POST", "/x", map[string]any{}, nil, map[string]string{"id": tplID}); rr.Code != 422 {
		t.Fatalf("migrate missing slug: %d", rr.Code)
	}
	if rr := doExtra(h.HandleMigrateSlug, "POST", "/x", map[string]any{"slug": "renamed-gap"},
		nil, map[string]string{"id": tplID}); rr.Code != 200 {
		t.Fatalf("migrate via slug alias: %d %s", rr.Code, rr.Body.String())
	}
}

func seedGapTemplate(t *testing.T) string {
	t.Helper()
	db, _ := testutil.MustConnectTestDB(t)
	tpls := templatecontract.NewService(db)
	tpl, _, err := tpls.Create(context.Background(), templatecontract.TemplateInput{
		Slug: "gap-migrate-src", Name: "G", Category: "n", Status: "active",
		HTMLLayout: extraLayout,
		Fields:     []models.TemplateField{{Name: "headline", Label: "H", Type: "text"}},
	})
	if err != nil {
		t.Fatalf("seedGapTemplate: %v", err)
	}
	return tpl.ID.Hex()
}

func TestCoverGapPublicationHandlerBranches(t *testing.T) {
	_, h, _ := newExtraSetup(t)
	cid := primitive.NewObjectID().Hex()
	pid := primitive.NewObjectID().Hex()

	// List: bad id; success empty; get: bad ids, alias, unknown pub.
	if rr := doExtra(h.HandleListPublications, "GET", "/x", nil, nil, map[string]string{"id": "zzz"}); rr.Code != 400 {
		t.Fatalf("list bad id: %d", rr.Code)
	}
	if rr := doExtra(h.HandleListPublications, "GET", "/x", nil, nil, map[string]string{"id": cid}); rr.Code != 200 {
		t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doExtra(h.HandleGetPublication, "GET", "/x", nil, nil, map[string]string{"id": "zzz", "publication_id": pid}); rr.Code != 400 {
		t.Fatalf("get bad cid: %d", rr.Code)
	}
	if rr := doExtra(h.HandleGetPublication, "GET", "/x", nil, nil, map[string]string{"id": cid, "publication_id": "zzz"}); rr.Code != 400 {
		t.Fatalf("get bad pid: %d", rr.Code)
	}
	if rr := doExtra(h.HandleGetPublication, "GET", "/x", nil, nil,
		map[string]string{"id": cid, "publicationId": pid}); rr.Code != 404 {
		t.Fatalf("get alias unknown: %d %s", rr.Code, rr.Body.String())
	}

	// Rollback: bad ids, bad expected hex, no key → 428, key + unknown → error.
	if rr := doExtra(h.HandleRollback, "POST", "/x", nil, nil, map[string]string{"id": "zzz", "publication_id": pid}); rr.Code != 400 {
		t.Fatalf("rollback bad cid: %d", rr.Code)
	}
	if rr := doExtra(h.HandleRollback, "POST", "/x", nil, nil, map[string]string{"id": cid, "publication_id": "zzz"}); rr.Code != 400 {
		t.Fatalf("rollback bad pid: %d", rr.Code)
	}
	if rr := doExtra(h.HandleRollback, "POST", "/x", map[string]any{"expected_active_id": "zzz"},
		nil, map[string]string{"id": cid, "publication_id": pid}); rr.Code != 400 {
		t.Fatalf("rollback bad expected: %d", rr.Code)
	}
	if rr := doExtra(h.HandleRollback, "POST", "/x", nil, nil,
		map[string]string{"id": cid, "publication_id": pid}); rr.Code != 428 {
		t.Fatalf("rollback no key: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doExtra(h.HandleRollback, "POST", "/x", nil,
		map[string]string{"Idempotency-Key": "gap-rb-1"},
		map[string]string{"id": cid, "publication_id": pid}); rr.Code == 200 {
		t.Fatalf("rollback unknown: want error, got 200")
	}

	// Restore: bad id, malformed JSON, missing version, bad expected hex.
	if rr := doExtra(h.HandleRestoreAndPublish, "POST", "/x", map[string]any{"version": 1}, nil, map[string]string{"id": "zzz"}); rr.Code != 400 {
		t.Fatalf("restore bad id: %d", rr.Code)
	}
	if rr := rawGapRequest(t, h.HandleRestoreAndPublish, "POST", "/x", "{bad", map[string]string{"id": cid}, nil); rr.Code != 400 {
		t.Fatalf("restore malformed: %d", rr.Code)
	}
	if rr := doExtra(h.HandleRestoreAndPublish, "POST", "/x", map[string]any{}, nil, map[string]string{"id": cid}); rr.Code != 400 {
		t.Fatalf("restore missing version: %d", rr.Code)
	}
	if rr := doExtra(h.HandleRestoreAndPublish, "POST", "/x",
		map[string]any{"version": 1, "expected_active_id": "zzz"}, nil, map[string]string{"id": cid}); rr.Code != 400 {
		t.Fatalf("restore bad expected: %d", rr.Code)
	}

	// Revert: bad id, malformed JSON, missing source, bad source hex, bad expected hex.
	if rr := doExtra(h.HandleRevertLive, "POST", "/x", map[string]any{"source_publication_id": pid}, nil, map[string]string{"id": "zzz"}); rr.Code != 400 {
		t.Fatalf("revert bad id: %d", rr.Code)
	}
	if rr := rawGapRequest(t, h.HandleRevertLive, "POST", "/x", "{bad", map[string]string{"id": cid}, nil); rr.Code != 400 {
		t.Fatalf("revert malformed: %d", rr.Code)
	}
	if rr := doExtra(h.HandleRevertLive, "POST", "/x", map[string]any{}, nil, map[string]string{"id": cid}); rr.Code != 400 {
		t.Fatalf("revert missing source: %d", rr.Code)
	}
	if rr := doExtra(h.HandleRevertLive, "POST", "/x", map[string]any{"source_publication_id": "zzz"}, nil, map[string]string{"id": cid}); rr.Code != 400 {
		t.Fatalf("revert bad source: %d", rr.Code)
	}
	if rr := doExtra(h.HandleRevertLive, "POST", "/x",
		map[string]any{"source_publication_id": pid, "expected_active_id": "zzz"}, nil, map[string]string{"id": cid}); rr.Code != 400 {
		t.Fatalf("revert bad expected: %d", rr.Code)
	}
	// Rollback via publicationId alias var.
	if rr := doExtra(h.HandleRollback, "POST", "/x", nil, nil,
		map[string]string{"id": cid, "publicationId": pid}); rr.Code != 428 {
		t.Fatalf("rollback alias no key: %d", rr.Code)
	}
}

func TestCoverGapIdemKeyAndWriteError(t *testing.T) {
	gen, h, _ := newExtraSetup(t)
	// Nil-extractor handlers: header fallback present/absent.
	var bare httpapi.Handlers
	bare.Gen = gen
	bare.ActorExtractor = h.ActorExtractor
	cid, pid := primitive.NewObjectID().Hex(), primitive.NewObjectID().Hex()
	if rr := doExtra(bare.HandleRollback, "POST", "/x", nil,
		map[string]string{"Idempotency-Key": "gap-bare-1"},
		map[string]string{"id": cid, "publication_id": pid}); rr.Code == 200 || rr.Code == 428 {
		t.Fatalf("bare extractor with header: %d (want service error past 428)", rr.Code)
	}
	if rr := doExtra(bare.HandleRollback, "POST", "/x", nil, nil,
		map[string]string{"id": cid, "publication_id": pid}); rr.Code != 428 {
		t.Fatalf("bare extractor no header: %d", rr.Code)
	}

	// WriteError with a plain (untyped) error and empty message.
	rr := httptest.NewRecorder()
	httpapi.WriteError(rr, httptest.NewRequest("GET", "/x", nil), errors.New("plain boom"))
	if rr.Code != 500 {
		t.Fatalf("plain error: %d", rr.Code)
	}
	rr = httptest.NewRecorder()
	httpapi.WriteError(rr, nil, &generation.Error{Code: generation.CodeInternal, Message: ""})
	if rr.Code != 500 {
		t.Fatalf("nil request: %d", rr.Code)
	}
	var env httpapi.ErrorEnvelope
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil || env.Error.Code != generation.CodeInternal || env.Error.Message == "" {
		t.Fatalf("nil-request envelope: %v %s", err, rr.Body.String())
	}
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("X-Request-ID", "req-1")
	rr = httptest.NewRecorder()
	httpapi.WriteError(rr, req, &generation.Error{Code: generation.CodeContentNotFound, Message: "gone"})
	var env2 httpapi.ErrorEnvelope
	_ = json.Unmarshal(rr.Body.Bytes(), &env2)
	if env2.Error.RequestID != "req-1" {
		t.Fatalf("request id envelope: %s", rr.Body.String())
	}
}
