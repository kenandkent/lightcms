package middleware

// B5 regression: middleware auth JSON errors carry the machine-readable
// sibling code (Wave-3B parity, API.md §12) — 401 maps to UNAUTHENTICATED
// instead of a bare {"error"} shape the typed clients cannot branch on.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAPIAuthErrorsCarryCodes(t *testing.T) {
	m := NewAPIAuth(func(ctx context.Context, rawKey string) (interface{}, error) {
		return nil, nil
	})
	handler := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Missing header → 401 + UNAUTHENTICATED.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
	var missing map[string]interface{}
	if err := json.NewDecoder(rr.Body).Decode(&missing); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := missing["error"].(string); !ok {
		t.Fatalf("error must stay a string: %v", missing)
	}
	if missing["code"] != "UNAUTHENTICATED" {
		t.Fatalf("code = %v, want UNAUTHENTICATED", missing["code"])
	}

	// Malformed header → same contract.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	req.Header.Set("Authorization", "nonsense")
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
	var malformed map[string]interface{}
	if err := json.NewDecoder(rr.Body).Decode(&malformed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if malformed["code"] != "UNAUTHENTICATED" {
		t.Fatalf("code = %v, want UNAUTHENTICATED", malformed["code"])
	}
}

// TestErrorCodeTableParity pins errorCodeForStatus to the APIHandler default
// table (defaultErrorCode): the two tracks must emit identical codes per
// status (the function is duplicated because handlers → middleware forbids
// sharing it — keep both in sync by hand).
func TestErrorCodeTableParity(t *testing.T) {
	for status, want := range map[int]string{
		http.StatusBadRequest:          "INVALID_REQUEST",
		http.StatusUnauthorized:        "UNAUTHENTICATED",
		http.StatusForbidden:           "PERMISSION_DENIED",
		http.StatusNotFound:            "NOT_FOUND",
		http.StatusConflict:            "CONFLICT",
		http.StatusUnprocessableEntity: "VALIDATION_FAILED",
		http.StatusTooManyRequests:     "RATE_LIMITED",
		http.StatusInternalServerError: "INTERNAL_ERROR",
		http.StatusServiceUnavailable:  "SERVICE_UNAVAILABLE",
		http.StatusTeapot:              "ERROR",
	} {
		if got := errorCodeForStatus(status); got != want {
			t.Errorf("errorCodeForStatus(%d) = %q, want %q (api.go defaultErrorCode)", status, got, want)
		}
	}
}
