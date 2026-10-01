package handlers

// Lane 1B (publish-path safety) regression tests:
// read-path traversal guards on ServePage / ServeAsset / getStaticFilePath.
// Attack vectors (../, %2e%2e, absolute escapes) must yield 400/404 and never
// leak files outside content/generated; legit nested paths, dotted slugs and
// encoded sequences must keep serving.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorilla/mux"
)

func TestGetStaticFilePath_Safety(t *testing.T) {
	var h *Handler // pure function: receiver unused

	legit := map[string]string{
		"":              "content/generated/index.html",
		"/":             "content/generated/index.html",
		"/about":        "content/generated/about.html",
		"/blog/my-post": "content/generated/blog/my-post.html",
		// Dots in slugs and nested paths must be preserved.
		"/my.page/about":  "content/generated/my.page/about.html",
		"/a/b/c":          "content/generated/a/b/c.html",
		"/file.with.dots": "content/generated/file.with.dots.html",
		// Decoded space (from %20) is a legit encoded sequence.
		"/my page": "content/generated/my page.html",
	}
	for in, want := range legit {
		if got := h.getStaticFilePath(in); got != want {
			t.Errorf("getStaticFilePath(%q) = %q, want %q", in, got, want)
		}
	}

	attacks := []string{
		"/../x",
		"/../../etc/passwd",
		"/a/../../etc/passwd",
		"..\\windows\\x",
		"/a\x00b",
		"/%2e%2e/x",
		"/%2E%2E/x",
		"/a/%2e%2e/b",
		"/%00/x",
	}
	for _, in := range attacks {
		if got := h.getStaticFilePath(in); got != "" {
			t.Errorf("getStaticFilePath(%q) = %q, want empty (blocked)", in, got)
		}
	}
}

// servePageWithSlug calls ServePage directly with mux vars set, bypassing
// router path-cleaning so the handler's own guards are exercised.
func servePageWithSlug(h *Handler, slug string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req = mux.SetURLVars(req, map[string]string{"slug": slug})
	rr := httptest.NewRecorder()
	h.ServePage(rr, req)
	return rr
}

func TestServePage_TraversalBlocked(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	// Pre-fix, slug "../../handlers.go" resolved (via package cwd
	// internal/handlers) to the handler source itself and was served 200.
	// Post-fix it must be rejected and must not leak source.
	for _, slug := range []string{
		"../../handlers.go",
		"..%2f..%2fhandlers.go",
		"%2e%2e/%2e%2e/handlers.go",
	} {
		rr := servePageWithSlug(h, slug)
		if rr.Code != http.StatusBadRequest && rr.Code != http.StatusNotFound {
			t.Errorf("ServePage(%q) = %d, want 400/404", slug, rr.Code)
		}
		if strings.Contains(rr.Body.String(), "package handlers") {
			t.Errorf("ServePage(%q) leaked source file contents", slug)
		}
	}

	// Absolute-style path with no existing page must 404, never escape.
	rr := servePageWithSlug(h, "etc/passwd")
	if rr.Code != http.StatusBadRequest && rr.Code != http.StatusNotFound {
		t.Errorf("ServePage(abs) = %d, want 400/404", rr.Code)
	}
}

func TestServePage_DottedAndNestedAssetStillServes(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	// Dotted slug shortcut.
	if err := os.MkdirAll(filepath.Join("content", "generated"), 0755); err != nil {
		t.Fatalf("mkdir generated: %v", err)
	}
	dotted := filepath.Join("content", "generated", "w1b-legit.page.txt")
	if err := os.WriteFile(dotted, []byte("w1b-dotted-bytes"), 0644); err != nil {
		t.Fatalf("seed dotted asset: %v", err)
	}
	defer os.Remove(dotted)

	rr := servePageWithSlug(h, "w1b-legit.page.txt")
	if rr.Code != http.StatusOK {
		t.Fatalf("ServePage dotted slug = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "w1b-dotted-bytes") {
		t.Error("ServePage dotted slug did not serve file bytes")
	}

	// Legit nested path with dots.
	nested := filepath.Join("content", "generated", "w1b-nested", "dir", "asset.min.txt")
	if err := os.MkdirAll(filepath.Dir(nested), 0755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	defer os.RemoveAll(filepath.Join("content", "generated", "w1b-nested"))
	if err := os.WriteFile(nested, []byte("w1b-nested-bytes"), 0644); err != nil {
		t.Fatalf("seed nested asset: %v", err)
	}

	rr = servePageWithSlug(h, "w1b-nested/dir/asset.min.txt")
	if rr.Code != http.StatusOK {
		t.Fatalf("ServePage nested = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "w1b-nested-bytes") {
		t.Error("ServePage nested path did not serve file bytes")
	}
}

func TestServeAsset_TraversalBlocked(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	// Pre-fix, /assets/../../handlers.go served handler source (package cwd
	// is internal/handlers). Post-fix: 400/404 with no leak.
	for _, target := range []string{
		"/assets/../../handlers.go",
		"/assets/%2e%2e/%2e%2e/handlers.go",
		"/assets/..%2f..%2fhandlers.go",
	} {
		// Set URL.Path explicitly: httptest.NewRequest normalizes dot
		// segments during parsing, which would bypass the handler's own
		// guards. Explicit assignment exercises the handler logic itself.
		// The %2e targets cover the raw-escaped branch; the plain target
		// covers decoded ".." (what r.URL.Path carries after Go decodes
		// %2e in a real request).
		req := httptest.NewRequest(http.MethodGet, "/assets/", nil)
		req.URL.Path = target
		rr := httptest.NewRecorder()
		h.ServeAsset(rr, req)
		if rr.Code != http.StatusBadRequest && rr.Code != http.StatusNotFound {
			t.Errorf("ServeAsset(%q) = %d, want 400/404", target, rr.Code)
		}
		if strings.Contains(rr.Body.String(), "package handlers") {
			t.Errorf("ServeAsset(%q) leaked source file contents", target)
		}
	}
}

func TestServeAsset_LegitPathsStillServe(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	flat := filepath.Join("content", "generated", "w1b-asset.txt")
	if err := os.MkdirAll(filepath.Join("content", "generated"), 0755); err != nil {
		t.Fatalf("mkdir generated: %v", err)
	}
	if err := os.WriteFile(flat, []byte("w1b-asset-bytes"), 0644); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	defer os.Remove(flat)

	nested := filepath.Join("content", "generated", "w1b-assets", "dir", "file.txt")
	if err := os.MkdirAll(filepath.Dir(nested), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	defer os.RemoveAll(filepath.Join("content", "generated", "w1b-assets"))
	if err := os.WriteFile(nested, []byte("w1b-nested-asset-bytes"), 0644); err != nil {
		t.Fatalf("seed nested: %v", err)
	}

	for target, want := range map[string]string{
		"/assets/w1b-asset.txt":           "w1b-asset-bytes",
		"/assets/w1b-assets/dir/file.txt": "w1b-nested-asset-bytes",
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rr := httptest.NewRecorder()
		h.ServeAsset(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("ServeAsset(%q) = %d, want 200", target, rr.Code)
			continue
		}
		if !strings.Contains(rr.Body.String(), want) {
			t.Errorf("ServeAsset(%q) missing bytes %q", target, want)
		}
	}
}
