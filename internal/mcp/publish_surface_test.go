package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jonradoff/lightcms/v7/internal/apiclient"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// publishSurfaceServer returns an MCP Server whose backend answers the publish
// endpoints with the PublicationService response shape (publication_id,
// public_url, full_path, per-item publications), so tool reply surfacing can
// be asserted end to end through the real apiclient.
func publishSurfaceServer(t *testing.T, batchReply map[string]interface{}) *Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/batch-publish"):
			_ = json.NewEncoder(w).Encode(batchReply)
		case strings.HasSuffix(r.URL.Path, "/publish"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "publication_id": "pub-surface-1",
				"public_url": "https://example.com/page-one",
				"full_path":  "/page-one", "content_id": "c1",
				"content_version": 3,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return NewServer(apiclient.New(srv.URL, "test-key"))
}

// listToolDescriptions returns tool name -> description for every registered
// tool on the in-memory transport.
func listToolDescriptions(t *testing.T, s *Server) map[string]string {
	t.Helper()

	ctx := context.Background()
	serverTransport, clientTransport := mcpsdk.NewInMemoryTransports()
	go s.MCPServer().Run(ctx, serverTransport)

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()

	res, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	out := make(map[string]string, len(res.Tools))
	for _, tool := range res.Tools {
		out[tool.Name] = tool.Description
	}
	return out
}

// TestPublishContent_SurfacesPublicationResult: publish_content must return
// the publication_id / public_url / full_path produced by the publish call,
// not a plain "published" acknowledgement.
func TestPublishContent_SurfacesPublicationResult(t *testing.T) {
	s := publishSurfaceServer(t, nil)

	result := callTool(t, s, "publish_content", map[string]interface{}{"id": "c1"})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}

	var resp map[string]interface{}
	resultJSON(t, result, &resp)

	if resp["publication_id"] != "pub-surface-1" {
		t.Errorf("publication_id = %v, want pub-surface-1", resp["publication_id"])
	}
	if resp["public_url"] != "https://example.com/page-one" {
		t.Errorf("public_url = %v, want https://example.com/page-one", resp["public_url"])
	}
	if resp["full_path"] != "/page-one" {
		t.Errorf("full_path = %v, want /page-one", resp["full_path"])
	}
	ver, ok := resp["content_version"].(float64)
	if !ok || ver != 3 {
		t.Errorf("content_version = %v, want 3", resp["content_version"])
	}
	msg, _ := resp["message"].(string)
	if !strings.Contains(msg, "published successfully") {
		t.Errorf("message = %q, want it to contain %q", msg, "published successfully")
	}
}

// TestPublishMultiple_SurfacesPerItemPublicationInfo: the batch reply must
// carry published_count plus per-item publications (id/publication_id/public_url)
// and failed entries.
func TestPublishMultiple_SurfacesPerItemPublicationInfo(t *testing.T) {
	s := publishSurfaceServer(t, map[string]interface{}{
		"published": []string{"c1"},
		"publications": []map[string]string{
			{"id": "c1", "publication_id": "pub-surface-1", "public_url": "https://example.com/page-one"},
		},
		"failed": []map[string]string{{"id": "c2", "error": "boom"}},
	})

	result := callTool(t, s, "publish_multiple", map[string]interface{}{"ids": []string{"c1", "c2"}})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}

	text := resultText(t, result)
	for _, want := range []string{"publication_id", "public_url", "published_count"} {
		if !strings.Contains(text, want) {
			t.Errorf("reply missing %q:\n%s", want, text)
		}
	}

	var resp map[string]interface{}
	resultJSON(t, result, &resp)

	if n, ok := resp["published_count"].(float64); !ok || n != 1 {
		t.Errorf("published_count = %v, want 1", resp["published_count"])
	}

	pubs, ok := resp["publications"].([]interface{})
	if !ok || len(pubs) != 1 {
		t.Fatalf("publications = %v, want one entry", resp["publications"])
	}
	pub := pubs[0].(map[string]interface{})
	if pub["id"] != "c1" || pub["publication_id"] != "pub-surface-1" ||
		pub["public_url"] != "https://example.com/page-one" {
		t.Errorf("publication entry = %v", pub)
	}

	failed, ok := resp["failed"].([]interface{})
	if !ok || len(failed) != 1 {
		t.Fatalf("failed = %v, want one entry", resp["failed"])
	}
	f := failed[0].(map[string]interface{})
	if f["id"] != "c2" || f["error"] != "boom" {
		t.Errorf("failed entry = %v", f)
	}
}

// TestPublishMultiple_SurfacesCountOnLegacyShape: the legacy batch response
// (no publications key — PublicationService unwired) must still surface a
// published count and the published/failed lists.
func TestPublishMultiple_SurfacesCountOnLegacyShape(t *testing.T) {
	s := publishSurfaceServer(t, map[string]interface{}{
		"published": []string{"c1", "c2"},
		"failed":    []interface{}{},
	})

	result := callTool(t, s, "publish_multiple", map[string]interface{}{"ids": []string{"c1", "c2"}})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}

	var resp map[string]interface{}
	resultJSON(t, result, &resp)

	if n, ok := resp["published_count"].(float64); !ok || n != 2 {
		t.Errorf("published_count = %v, want 2", resp["published_count"])
	}
	if _, ok := resp["publications"]; !ok {
		t.Errorf("publications key missing from reply: %v", resp)
	}
	if _, ok := resp["failed"]; !ok {
		t.Errorf("failed key missing from reply: %v", resp)
	}
	if pubs, ok := resp["published"].([]interface{}); !ok || len(pubs) != 2 {
		t.Errorf("published = %v, want 2 ids", resp["published"])
	}
}

// TestPublishToolDescriptions_StateRequiredPermissions: both publish tools must
// document the generation scope matrix (content.edit + content.publish for
// existing pages; content.create + content.publish for new ones).
func TestPublishToolDescriptions_StateRequiredPermissions(t *testing.T) {
	s := NewServer(apiclient.New("http://localhost:0", "test"))
	descs := listToolDescriptions(t, s)

	for _, name := range []string{"publish_content", "publish_multiple"} {
		desc, ok := descs[name]
		if !ok {
			t.Fatalf("tool %q not registered", name)
		}
		for _, want := range []string{"content.edit", "content.publish", "content.create"} {
			if !strings.Contains(desc, want) {
				t.Errorf("%s description missing %q:\n%s", name, want, desc)
			}
		}
	}
}

// TestSearchReplaceExecute_SurfacesResultCounts documents that the execute
// tools already pass the whole SearchReplaceResult through (total_replacements,
// pages_updated) — reviewed-OK for wave 3 fix 3.
func TestSearchReplaceExecute_SurfacesResultCounts(t *testing.T) {
	s, _, cleanup := testAPI(t)
	defer cleanup()

	result := callTool(t, s, "search_replace_execute", map[string]interface{}{
		"search": "old", "replace": "new",
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
	var resp map[string]interface{}
	resultJSON(t, result, &resp)
	if v, ok := resp["total_replacements"].(float64); !ok || v != 5 {
		t.Errorf("total_replacements = %v, want 5", resp["total_replacements"])
	}
	if v, ok := resp["pages_updated"].(float64); !ok || v != 2 {
		t.Errorf("pages_updated = %v, want 2", resp["pages_updated"])
	}

	result = callTool(t, s, "scoped_search_replace_execute", map[string]interface{}{
		"search": "old", "replace": "new", "folder_path": "/blog",
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
	resp = nil
	resultJSON(t, result, &resp)
	if v, ok := resp["total_replacements"].(float64); !ok || v != 3 {
		t.Errorf("scoped total_replacements = %v, want 3", resp["total_replacements"])
	}
	if v, ok := resp["pages_updated"].(float64); !ok || v != 1 {
		t.Errorf("scoped pages_updated = %v, want 1", resp["pages_updated"])
	}
}
