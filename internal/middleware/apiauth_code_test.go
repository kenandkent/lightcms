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
