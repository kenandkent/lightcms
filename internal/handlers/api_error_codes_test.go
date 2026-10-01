package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/jonradoff/lightcms/v7/internal/database"
	"github.com/jonradoff/lightcms/v7/internal/product/idempotency"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/publicurl"
	"github.com/jonradoff/lightcms/v7/internal/product/storage"
)

// decodeLegacyErrBody decodes a legacy /api/v1 error body and enforces the
// additive contract: "error" STAYS a plain string (existing clients and admin
// JS parse it as a string — it must never nest) and a sibling "code" carries
// the machine-readable kind.
func decodeLegacyErrBody(t *testing.T, rr *httptest.ResponseRecorder) (message, code string) {
	t.Helper()
	var body map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rr.Body.String(), err)
	}
	raw, ok := body["error"]
	if !ok {
		t.Fatalf("missing error field in %s", rr.Body.String())
	}
	message, ok = raw.(string)
	if !ok {
		t.Fatalf("error must remain a string (got %T): %s", raw, rr.Body.String())
	}
	if message == "" {
		t.Fatalf("empty error message in %s", rr.Body.String())
	}
	code, _ = body["code"].(string)
	if code == "" {
		t.Fatalf("missing sibling code field in %s", rr.Body.String())
	}
	return message, code
}

// TestJsonError_StatusDerivedCode locks the status → default code table used
// when a caller does not pass an explicit code.
func TestJsonError_StatusDerivedCode(t *testing.T) {
	ah, _, cleanup := newTestAPIHandler(t)
	defer cleanup()

	cases := []struct {
		status int
		code   string
	}{
		{http.StatusBadRequest, "INVALID_REQUEST"},
		{http.StatusUnauthorized, "UNAUTHENTICATED"},
		{http.StatusForbidden, "PERMISSION_DENIED"},
		{http.StatusNotFound, "NOT_FOUND"},
		{http.StatusConflict, "CONFLICT"},
		{http.StatusUnprocessableEntity, "VALIDATION_FAILED"},
		{http.StatusTooManyRequests, "RATE_LIMITED"},
		{http.StatusInternalServerError, "INTERNAL_ERROR"},
		{http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE"},
	}
	for _, c := range cases {
		rr := httptest.NewRecorder()
		ah.jsonError(rr, c.status, "boom")
		if rr.Code != c.status {
			t.Errorf("status for %d: got %d", c.status, rr.Code)
		}
		msg, code := decodeLegacyErrBody(t, rr)
		if msg != "boom" {
			t.Errorf("status %d message: got %q", c.status, msg)
		}
		if code != c.code {
			t.Errorf("status %d default code: got %q, want %q", c.status, code, c.code)
		}
	}

	// A 428 must NOT be auto-mapped to IDEMPOTENCY_KEY_REQUIRED — only callers
	// that actually require the key say so.
	rr := httptest.NewRecorder()
	ah.jsonError(rr, 428, "precondition missing")
	if _, code := decodeLegacyErrBody(t, rr); code == "IDEMPOTENCY_KEY_REQUIRED" {
		t.Error("status-derived default must not assert IDEMPOTENCY_KEY_REQUIRED for an arbitrary 428")
	}
}

// TestJsonErrorCode_ExplicitCode covers the explicit-code helper: the caller's
// code wins over the status-derived default and the message stays a string.
func TestJsonErrorCode_ExplicitCode(t *testing.T) {
	ah, _, cleanup := newTestAPIHandler(t)
	defer cleanup()

	rr := httptest.NewRecorder()
	ah.jsonErrorCode(rr, http.StatusForbidden, "SCOPE_MISSING", "nope")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d", rr.Code)
	}
	msg, code := decodeLegacyErrBody(t, rr)
	if msg != "nope" || code != "SCOPE_MISSING" {
		t.Fatalf("got message=%q code=%q", msg, code)
	}

	// An empty code falls back to the status-derived default.
	rr = httptest.NewRecorder()
	ah.jsonErrorCode(rr, http.StatusNotFound, "", "gone")
	if _, code := decodeLegacyErrBody(t, rr); code != "NOT_FOUND" {
		t.Fatalf("empty code fallback: got %q", code)
	}
}

// wirePublicationRuntime mirrors the V3 wiring in cmd/server/main.go (Task 16C).
func wirePublicationRuntime(t *testing.T, ah *APIHandler, db *database.DB) {
	t.Helper()
	if err := db.EnsureProductIndexes(context.Background()); err != nil {
		t.Fatalf("EnsureProductIndexes: %v", err)
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
		Idem: idem, URLs: resolver, BuildSHA: "test-3b",
	})
	ah.SetPublicationRuntime(pubs, idem, nil)
}

// TestPublish428_CarriesIdempotencyCode: single + batch publish rejected with
// 428 must expose {"error": string, "code": "IDEMPOTENCY_KEY_REQUIRED"} so
// agents/MCP can branch on the kind instead of parsing prose.
func TestPublish428_CarriesIdempotencyCode(t *testing.T) {
	ah, db, cleanup := newTestAPIHandler(t)
	defer cleanup()
	wirePublicationRuntime(t, ah, db)

	// Single publish without Idempotency-Key.
	req := authReq(http.MethodPost, "/api/v1/content/000000000000000000000042/publish", nil)
	req = mux.SetURLVars(req, map[string]string{"id": "000000000000000000000042"})
	rr := httptest.NewRecorder()
	ah.APIPublishContent(rr, req)
	if rr.Code != 428 {
		t.Fatalf("single publish: expected 428, got %d (%s)", rr.Code, rr.Body.String())
	}
	msg, code := decodeLegacyErrBody(t, rr)
	if code != "IDEMPOTENCY_KEY_REQUIRED" {
		t.Errorf("single publish code: got %q", code)
	}
	if !strings.Contains(msg, "Idempotency-Key") {
		t.Errorf("single publish message lost its prose: %q", msg)
	}

	// Batch publish without Idempotency-Key.
	req = authReq(http.MethodPost, "/api/v1/content/batch-publish",
		strings.NewReader(`{"ids":["000000000000000000000042"]}`))
	rr = httptest.NewRecorder()
	ah.APIBatchPublishContent(rr, req)
	if rr.Code != 428 {
		t.Fatalf("batch publish: expected 428, got %d (%s)", rr.Code, rr.Body.String())
	}
	if _, code := decodeLegacyErrBody(t, rr); code != "IDEMPOTENCY_KEY_REQUIRED" {
		t.Errorf("batch publish code: got %q", code)
	}
}

// TestSearchReplace428_CarriesIdempotencyCode: the Wave 2 search-replace
// execute gates (global + scoped) expose the same machine-readable code.
func TestSearchReplace428_CarriesIdempotencyCode(t *testing.T) {
	ah, _, cleanup := newTestAPIHandler(t)
	defer cleanup()

	call := func(h http.HandlerFunc, target string) *httptest.ResponseRecorder {
		req := authReq(http.MethodPost, target, strings.NewReader(`{"search":"Go","replace":"Golang"}`))
		rr := httptest.NewRecorder()
		h(rr, req)
		return rr
	}

	rr := call(ah.APISearchReplaceExecute, "/api/v1/content/search-replace/execute")
	if rr.Code != 428 {
		t.Fatalf("global execute: expected 428, got %d (%s)", rr.Code, rr.Body.String())
	}
	if _, code := decodeLegacyErrBody(t, rr); code != "IDEMPOTENCY_KEY_REQUIRED" {
		t.Errorf("global execute code: got %q", code)
	}

	rr = call(ah.APIScopedSearchReplaceExecute, "/api/v1/content/scoped-search-replace/execute")
	if rr.Code != 428 {
		t.Fatalf("scoped execute: expected 428, got %d (%s)", rr.Code, rr.Body.String())
	}
	if _, code := decodeLegacyErrBody(t, rr); code != "IDEMPOTENCY_KEY_REQUIRED" {
		t.Errorf("scoped execute code: got %q", code)
	}
}

// TestSandboxOnly403_CarriesPermissionDeniedCode: both the requirePermission
// gate (publish) and the inline sandbox guards (create/update) report
// PERMISSION_DENIED, so clients never have to match on the prose.
func TestSandboxOnly403_CarriesPermissionDeniedCode(t *testing.T) {
	ah, db, cleanup := newTestAPIHandler(t)
	defer cleanup()

	tmplID := seedTemplate(t, db, "Page", "page-3b-code")

	// Inline sandbox guard on live create → 403 PERMISSION_DENIED.
	req := sandboxOnlyReq(http.MethodPost, "/api/v1/content", strings.NewReader(
		`{"template_id":"`+tmplID.Hex()+`","title":"Live","slug":"live-3b","data":{}}`))
	rr := httptest.NewRecorder()
	ah.APICreateContent(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("sandbox create: expected 403, got %d (%s)", rr.Code, rr.Body.String())
	}
	if _, code := decodeLegacyErrBody(t, rr); code != "PERMISSION_DENIED" {
		t.Errorf("sandbox create code: got %q", code)
	}

	// requirePermission gate on publish → 403 PERMISSION_DENIED.
	req = sandboxOnlyReq(http.MethodPost, "/api/v1/content/000000000000000000000042/publish", nil)
	req = mux.SetURLVars(req, map[string]string{"id": "000000000000000000000042"})
	rr = httptest.NewRecorder()
	ah.APIPublishContent(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("sandbox publish: expected 403, got %d (%s)", rr.Code, rr.Body.String())
	}
	msg, code := decodeLegacyErrBody(t, rr)
	if code != "PERMISSION_DENIED" {
		t.Errorf("sandbox publish code: got %q", code)
	}
	if !strings.Contains(msg, "sandbox-only") {
		t.Errorf("sandbox publish message: %q", msg)
	}
}

// TestUntouchedEndpoint404_StatusDerivedCode: an endpoint nobody opted into an
// explicit code for still gets a stable code from the status (404 → NOT_FOUND).
func TestUntouchedEndpoint404_StatusDerivedCode(t *testing.T) {
	ah, _, cleanup := newTestAPIHandler(t)
	defer cleanup()

	rr := doJSON(t, ah.APIGetAsset, http.MethodGet, nil,
		map[string]string{"id": "ffffffffffffffffffffffff"})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d (%s)", rr.Code, rr.Body.String())
	}
	msg, code := decodeLegacyErrBody(t, rr)
	if code != "NOT_FOUND" {
		t.Errorf("404 default code: got %q", code)
	}
	if msg == "" {
		t.Error("404 message empty")
	}
}
