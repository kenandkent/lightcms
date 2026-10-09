package handlers

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestStandalonePublicDocumentIsNotWrappedInSiteTheme(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()
	theme, err := h.db.GetThemeSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"<!doctype html>", "<!-- authored document -->\n<!DOCTYPE HTML>", "<html lang=\"en\">"} {
		content := prefix + `<head><title>Article title</title><meta property="og:title" content="Article title"></head><body><main><h1>Article title</h1></main></body></html>`
		rr := httptest.NewRecorder()
		h.renderPublicWithSEO(rr, httptest.NewRequest("GET", "/reports/example", nil), theme, content, true, true, "Outer title", "", "", "/reports/example")
		if rr.Body.String() != content {
			t.Errorf("standalone document changed or wrapped for prefix %q", prefix)
		}
		if strings.Contains(rr.Body.String(), "/static/css/main.css") {
			t.Fatal("site theme CSS contaminates standalone document")
		}
	}
}

func TestPublicHTMLFragmentStillGetsSiteLayout(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()
	theme, err := h.db.GetThemeSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	h.renderPublicWithSEO(rr, httptest.NewRequest("GET", "/fragment", nil), theme, `<article><pre>&lt;!doctype html&gt;</pre><p>Fragment</p></article>`, false, false, "Fragment title", "", "", "/fragment")
	if !strings.Contains(rr.Body.String(), "/static/css/main.css") || !strings.Contains(rr.Body.String(), "Fragment title") {
		t.Fatal("fragment lost its normal themed page shell")
	}
}

func TestStandaloneDocumentGetsMissingSEOWithoutThemeCSS(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()
	theme, err := h.db.GetThemeSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{`<html><body><p>Body</p></body></html>`, `<!doctype html><html><head></head><body><p>Body</p></body></html>`} {
		rr := httptest.NewRecorder()
		h.renderPublicWithSEO(rr, httptest.NewRequest("GET", "/seo", nil), theme, source, false, false, "Title <safe>", "", "", "", `<script type="application/ld+json">{"@type":"Article"}</script>`)
		got := rr.Body.String()
		if !strings.Contains(got, `<title>Title &lt;safe&gt;</title>`) || !strings.Contains(got, `"@type":"Article"`) || strings.Count(got, "<head>") != 1 || strings.Contains(got, "main.css") {
			t.Fatalf("missing standalone fallback metadata: %s", got)
		}
	}
}

func TestStandaloneAuthoredSEOIsNotReplacedByMutableMetadata(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()
	theme, err := h.db.GetThemeSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	source := `<!doctype html><html><head><title>Frozen title</title><script type="application/ld+json">{"@type":"Article","headline":"Frozen title"}</script></head><body>Body</body></html>`
	rr := httptest.NewRecorder()
	h.renderPublicWithSEO(rr, httptest.NewRequest("GET", "/seo", nil), theme, source, false, false, "Mutable draft title", "", "", "", `<script type="application/ld+json">{"headline":"Mutable draft title"}</script>`)
	if rr.Body.String() != source {
		t.Fatal("authored metadata was overwritten or duplicated")
	}
}

func TestStandaloneForkPreviewDoesNotReintroduceThemeWrapper(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()
	theme, err := h.db.GetThemeSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tmpl := models.Template{ID: primitive.NewObjectID(), Name: "独立模板", Slug: "standalone-fork", HTMLLayout: `<!doctype html><html><head><title>Fork page</title></head><body><main><p>Body</p></main></body></html>`}
	if _, err := h.db.InsertOne(context.Background(), "templates", tmpl); err != nil {
		t.Fatal(err)
	}
	content := models.Content{ID: primitive.NewObjectID(), TemplateID: tmpl.ID, Title: "Fork page", FullPath: "/fork-page"}
	rr := httptest.NewRecorder()
	h.servePageContent(rr, httptest.NewRequest("GET", "/fork-page", nil), &content, theme, &models.ContentFork{ID: primitive.NewObjectID(), Name: "Review"})
	got := rr.Body.String()
	if strings.Count(strings.ToLower(got), "<!doctype") != 1 || strings.Contains(got, "/static/css/main.css") || !strings.Contains(got, "lc-fork-preview-bar") {
		t.Fatalf("fork preview is wrapped or missing its toolbar: %.400s", got)
	}
}
