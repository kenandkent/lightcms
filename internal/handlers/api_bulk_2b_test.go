package handlers

// Lane 2B Fix 2 (red, HTTP layer): bulk-create must reject Published:true
// with a clear 4xx directing to the publish endpoints — persisting a live
// flag without a Publication row, outbox event, or idempotency record is a
// control-plane divergence.
//
// Lane 2B Fix 1 (health surfacing): a degraded boot (migration-required
// latch) must surface on /healthz as a degraded migration dependency while
// the endpoint stays HTTP 200 (liveness preserved for the dry-run path).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPIBulkCreateContent_RejectsPublishedTrue(t *testing.T) {
	ah, db, cleanup := newTestAPIHandler(t)
	defer cleanup()
	tmpl := seedTemplate(t, db, "Page", "page")

	rr := doJSON(t, ah.APIBulkCreateContent, http.MethodPost, map[string]interface{}{
		"items": []map[string]interface{}{
			{"template_id": tmpl.Hex(), "title": "Bulk Pub", "slug": "bulk-pub-reject", "published": true},
		},
	}, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bulk-create Published:true: got %d, want 400 (%s)", rr.Code, rr.Body.String())
	}
	body := strings.ToLower(rr.Body.String())
	if !strings.Contains(body, "publish") {
		t.Fatalf("400 must direct to publish endpoints, got: %s", rr.Body.String())
	}
}

func TestHealthz_MigrationRequiredDegraded(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	h.SetMigrationRequired("product indexes unavailable (run `lightcms migrate-publications --dry-run` for blockers)")

	rr := httptest.NewRecorder()
	req := authReq(http.MethodGet, "/healthz", nil)
	h.Healthz(rr, req)

	// Liveness preserved: degraded is HTTP 200 (only unhealthy is 503) so
	// the migration dry-run diagnostic path stays reachable.
	if rr.Code != http.StatusOK {
		t.Fatalf("degraded healthz must stay 200, got %d", rr.Code)
	}
	var decoded struct {
		Status       string `json:"status"`
		Dependencies []struct {
			Name    string `json:"name"`
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"dependencies"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Status != "degraded" {
		t.Fatalf("expected status=degraded, got %q", decoded.Status)
	}
	found := false
	for _, d := range decoded.Dependencies {
		if d.Name == "migration" && d.Status == "degraded" &&
			strings.Contains(d.Message, "migrate-publications --dry-run") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected degraded migration dependency, got %+v", decoded.Dependencies)
	}
}
