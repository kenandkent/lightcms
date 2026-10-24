package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newSRRecorder serves a valid SearchReplaceResult for every request and
// records the Idempotency-Key header of each one.
func newSRRecorder(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(SearchReplaceResult{Success: true, TotalReplacements: 1})
	}))
	t.Cleanup(srv.Close)
	return srv, &keys
}

// Wave 3 lane 3A: search-replace execute endpoints answer 428
// IDEMPOTENCY_KEY_REQUIRED without an Idempotency-Key header, so every
// execute method must auto-mint one (mirrors PublishContentResult).
func TestSearchReplaceExecuteSendsIdempotencyKey(t *testing.T) {
	cases := []struct {
		name string
		call func(*Client) error
	}{
		{"SearchReplaceExecute", func(c *Client) error {
			_, err := c.SearchReplaceExecute(context.Background(), "foo", "bar", "replace foo", false, true)
			return err
		}},
		{"SearchReplaceExecutePairs", func(c *Client) error {
			_, err := c.SearchReplaceExecutePairs(context.Background(),
				[]map[string]interface{}{{"search": "a", "replace": "b"}}, "replace pair", false)
			return err
		}},
		{"ScopedSearchReplaceExecute", func(c *Client) error {
			_, err := c.ScopedSearchReplaceExecute(context.Background(), "foo", "bar", "scoped replace",
				false, true, ScopedSearchReplaceScope{Category: "blog"})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, keys := newSRRecorder(t)
			c := New(srv.URL, "tok")
			if err := tc.call(c); err != nil {
				t.Fatalf("call failed: %v", err)
			}
			if len(*keys) != 1 {
				t.Fatalf("expected exactly 1 request, got %d", len(*keys))
			}
			if got := (*keys)[0]; got == "" {
				t.Errorf("%s: expected non-empty Idempotency-Key header (server returns 428 without it)", tc.name)
			}
		})
	}
}

// Each execute call must mint its OWN key: distinct user actions are
// distinct idempotent operations (a constant key would replay stale results).
func TestSearchReplaceExecuteMintsDistinctKeys(t *testing.T) {
	srv, keys := newSRRecorder(t)
	c := New(srv.URL, "tok")
	for i := 0; i < 2; i++ {
		if _, err := c.SearchReplaceExecute(context.Background(), "foo", "bar", "c", false, false); err != nil {
			t.Fatalf("call %d failed: %v", i, err)
		}
	}
	if len(*keys) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(*keys))
	}
	if (*keys)[0] == "" || (*keys)[1] == "" {
		t.Errorf("expected both keys non-empty, got %q", *keys)
	}
	if (*keys)[0] == (*keys)[1] {
		t.Errorf("expected distinct keys per call, both %q", (*keys)[0])
	}
}

// Preview endpoints stay keyless (read-only, no 428 requirement).
func TestSearchReplacePreviewStaysKeyless(t *testing.T) {
	cases := []struct {
		name string
		call func(*Client) error
	}{
		{"SearchReplacePreview", func(c *Client) error {
			_, err := c.SearchReplacePreview(context.Background(), "foo", "bar", false)
			return err
		}},
		{"SearchReplacePreviewPairs", func(c *Client) error {
			_, err := c.SearchReplacePreviewPairs(context.Background(),
				[]map[string]interface{}{{"search": "a", "replace": "b"}})
			return err
		}},
		{"ScopedSearchReplacePreview", func(c *Client) error {
			_, err := c.ScopedSearchReplacePreview(context.Background(), "foo", "bar", false,
				ScopedSearchReplaceScope{Category: "blog"})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, keys := newSRRecorder(t)
			c := New(srv.URL, "tok")
			if err := tc.call(c); err != nil {
				t.Fatalf("call failed: %v", err)
			}
			if len(*keys) != 1 {
				t.Fatalf("expected exactly 1 request, got %d", len(*keys))
			}
			if got := (*keys)[0]; got != "" {
				t.Errorf("%s: preview must stay keyless, got Idempotency-Key %q", tc.name, got)
			}
		})
	}
}

// --- APIError envelope parsing (lane 3A contract unification) ---

func TestParseAPIError_LegacyFlat(t *testing.T) {
	e := parseAPIError(404, []byte(`{"error":"not found"}`))
	if e.Status != 404 {
		t.Errorf("Status: got %d, want 404", e.Status)
	}
	if e.Message != "not found" {
		t.Errorf("Message: got %q, want %q", e.Message, "not found")
	}
	if e.Code != "" || e.RequestID != "" || e.Retryable {
		t.Errorf("unexpected populated fields: %+v", e)
	}
	// Backward compat: legacy callers saw exactly the flat message.
	if e.Error() != "not found" {
		t.Errorf("Error(): got %q, want %q", e.Error(), "not found")
	}
}

func TestParseAPIError_LegacyFlatWithSiblingCode(t *testing.T) {
	body := `{"error":"Idempotency-Key is required for search-replace execute","code":"IDEMPOTENCY_KEY_REQUIRED"}`
	e := parseAPIError(428, []byte(body))
	if e.Status != 428 {
		t.Errorf("Status: got %d, want 428", e.Status)
	}
	if e.Code != "IDEMPOTENCY_KEY_REQUIRED" {
		t.Errorf("Code: got %q, want %q", e.Code, "IDEMPOTENCY_KEY_REQUIRED")
	}
	if e.Message != "Idempotency-Key is required for search-replace execute" {
		t.Errorf("Message: got %q", e.Message)
	}
	if got := e.Error(); got == "" {
		t.Error("Error() must not be empty")
	}
}

func TestParseAPIError_V3Nested(t *testing.T) {
	body := `{"error":{"code":"REQUEST_IN_PROGRESS","message":"another request is running","request_id":"req-42","retryable":true}}`
	e := parseAPIError(503, []byte(body))
	if e.Status != 503 {
		t.Errorf("Status: got %d, want 503", e.Status)
	}
	if e.Code != "REQUEST_IN_PROGRESS" {
		t.Errorf("Code: got %q", e.Code)
	}
	if e.Message != "another request is running" {
		t.Errorf("Message: got %q", e.Message)
	}
	if e.RequestID != "req-42" {
		t.Errorf("RequestID: got %q", e.RequestID)
	}
	if !e.Retryable {
		t.Error("Retryable: got false, want true")
	}
	got := e.Error()
	if got == "" {
		t.Fatal("Error() must not be empty")
	}
	for _, want := range []string{"another request is running", "REQUEST_IN_PROGRESS"} {
		if !strings.Contains(got, want) {
			t.Errorf("Error() = %q, want it to contain %q", got, want)
		}
	}
}

func TestParseAPIError_GarbageBodyFallback(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"plain text", "internal server error"},
		{"invalid json", `{"error": `},
		{"empty error string", `{"error":""}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := parseAPIError(500, []byte(tc.body))
			if e.Status != 500 {
				t.Errorf("Status: got %d, want 500", e.Status)
			}
			got := e.Error()
			if !strings.Contains(got, "API error (status 500)") {
				t.Errorf("Error() = %q, want the generic fallback message", got)
			}
			if !strings.Contains(got, tc.body) {
				t.Errorf("Error() = %q, want it to include the raw body %q", got, tc.body)
			}
		})
	}
}

// do() must surface *APIError for both envelopes while remaining a plain
// error for callers that only check err != nil.
func TestDo_ReturnsTypedAPIError(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantMsg   string
		wantCode  string
		wantRetry bool
		wantReqID string
	}{
		{
			name:    "legacy flat",
			status:  404,
			body:    `{"error":"content not found"}`,
			wantMsg: "content not found",
		},
		{
			name:      "v3 nested",
			status:    429,
			body:      `{"error":{"code":"RATE_LIMITED","message":"slow down","request_id":"r1","retryable":true}}`,
			wantMsg:   "slow down",
			wantCode:  "RATE_LIMITED",
			wantRetry: true,
			wantReqID: "r1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)

			c := New(srv.URL, "tok")
			var err error = func() error {
				_, e := c.GetContent(context.Background(), "missing", false)
				return e
			}()
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("expected *APIError via errors.As, got %T: %v", err, err)
			}
			if apiErr.Status != tc.status {
				t.Errorf("Status: got %d, want %d", apiErr.Status, tc.status)
			}
			if apiErr.Message != tc.wantMsg {
				t.Errorf("Message: got %q, want %q", apiErr.Message, tc.wantMsg)
			}
			if apiErr.Code != tc.wantCode {
				t.Errorf("Code: got %q, want %q", apiErr.Code, tc.wantCode)
			}
			if apiErr.Retryable != tc.wantRetry {
				t.Errorf("Retryable: got %v, want %v", apiErr.Retryable, tc.wantRetry)
			}
			if apiErr.RequestID != tc.wantReqID {
				t.Errorf("RequestID: got %q, want %q", apiErr.RequestID, tc.wantReqID)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("Error() = %q, want it to contain %q", err.Error(), tc.wantMsg)
			}
		})
	}
}
