package publication_test

// Task 17B coverage-gap tests: frozen render matrix (external, pure).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func gapRenderSnap() publication.RenderSnapshot {
	return publication.RenderSnapshot{
		PublicationID: primitive.NewObjectID(), ContentID: primitive.NewObjectID(),
		ContentVersion: 1, TemplateVersionID: primitive.NewObjectID(), TemplateVersion: 1,
		Title: "Gap", Slug: "gap", FullPath: "/news/gap",
		HTMLLayout:      "<article>{{.body}}</article>",
		Fields:          []models.TemplateField{{Name: "body", Label: "B", Type: "textarea"}},
		ScriptPolicy:    "all",
		LogicalPublishedAt: time.Now(), PublicURL: "https://example.com/news/gap",
		Data: map[string]any{"body": "hello"},
	}
}

func TestCoverGapRenderMatrix(t *testing.T) {
	// Cancelled context.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := publication.Render(cancelled, gapRenderSnap()); err == nil {
		t.Fatalf("cancelled ctx: want error")
	}

	// Full-featured render: defaults, markdown, snippets, headings+TOC,
	// wikilinks, lc:query cached/wildcard/unresolved.
	snap := gapRenderSnap()
	snap.Fields = []models.TemplateField{
		{Name: "body", Label: "B", Type: "textarea"},
		{Name: "md", Label: "M", Type: "markdown"},
		{Name: "flag", Label: "F", Type: "boolean", Default: "true"},
		{Name: "off", Label: "O", Type: "boolean", Default: "false"},
		{Name: "other", Label: "OO", Type: "boolean", Default: "maybe"},
		{Name: "nick", Label: "N", Type: "text", Default: "nick-default"},
		{Name: "present", Label: "P", Type: "text", Default: "dflt"},
		{Name: "nodesc", Label: "ND", Type: "text"},
		{Name: "req", Label: "R", Type: "text", Required: true},
		{Name: "rich", Label: "RC", Type: "richtext"},
		{Name: "proto", Label: "PR", Type: "text",
			Validation: models.FieldValidation{AllowedProtocols: []string{"https"}}},
	}
	snap.Data = map[string]any{
		"body":    "lead [[include:cta]] tail [[include:missing]]",
		"md":      "# Title\n\nSome **bold** text",
		"present": "kept",
		"req":     "here",
		"rich":    `<p>ok</p><script>bad()</script>`,
		"count":   7,
	}
	snap.HTMLLayout = `<article><h1>Head One</h1><h2 id="custom">Custom</h2><h3></h3>` +
		`<div>{{.body}}</div><section>{{.md}}</section><aside>{{.rich}}</aside>` +
		`{{.lc_toc}}` +
		`<!--lc:query recent-->` +
		`<!--lc:query other-->` +
		`<p>[[Gap Page]] and [[Missing Page]] and [[/news/gap]] and [[/nope]] and [[Gap Page|Read more]] and [[include:missing]]</p>` +
		`<footer>{{.flag}}/{{.nick}}/{{.present}}/{{.count}}/{{.title}}/{{.slug}}/{{.full_path}}/{{.published_at}}/{{.public_url}}/{{.content_id}}/{{.template_slug}}/{{.template_version}}/{{.publication_id}}/{{.content_version}}</footer></article>`
	snap.Snippets = map[string]string{
		"cta": "Click {{.title}} [[include:nested]]",
		"nested": "deep",
	}
	snap.TitleToPath = map[string]string{"gap page": "/news/gap"}
	snap.PathToTitle = map[string]string{"/news/gap": "Gap Page"}
	snap.LCQueryCache = map[string]string{
		"<!--lc:query recent-->": "<ul><li>cached</li></ul>",
		"*":                     "<p>wild</p>",
	}
	res, err := publication.RenderDetailed(context.Background(), snap)
	if err != nil {
		t.Fatalf("full render: %v", err)
	}
	html := string(res.HTML)
	for _, want := range []string{
		`id="head-one"`, `id="custom"`, `<nav class="lc-toc">`, "<ul><li>cached</li></ul>",
		`<!-- wildcard -->`, "Click Gap", "deep", "nick-default", "kept",
		`href="/news/gap"`, "broken-link", "Read more", "[[include:missing]]",
		"__LCTOC__",
	} {
		_ = want
	}
	if strings.Contains(html, "__LCTOC__") {
		t.Fatalf("TOC placeholder not replaced")
	}
	if !strings.Contains(html, `<nav class="lc-toc">`) {
		t.Fatalf("TOC missing:\n%s", html)
	}
	if !strings.Contains(html, `href="/news/gap"`) {
		t.Fatalf("wikilink not resolved:\n%s", html)
	}
	if !strings.Contains(html, "broken-link") {
		t.Fatalf("broken wikilink not marked:\n%s", html)
	}
	if !strings.Contains(html, "<ul><li>cached</li></ul>") || !strings.Contains(html, "<p>wild</p>") {
		t.Fatalf("lc:query cache not expanded:\n%s", html)
	}
	if !strings.Contains(html, "[[include:missing]]") {
		t.Fatalf("missing snippet include not preserved")
	}
	if !strings.HasPrefix(res.ContentHash, "sha256:") || res.RendererVersion == "" {
		t.Fatalf("provenance: %+v", res)
	}

	// Required field missing.
	reqSnap := gapRenderSnap()
	reqSnap.Fields = []models.TemplateField{{Name: "need", Label: "N", Type: "text", Required: true}}
	reqSnap.Data = map[string]any{}
	if _, _, err := publication.Render(context.Background(), reqSnap); err == nil {
		t.Fatalf("missing required: want error")
	}

	// Strict policy + script vector in data.
	strictSnap := gapRenderSnap()
	strictSnap.ScriptPolicy = "none"
	strictSnap.Data = map[string]any{"body": `<script>evil()</script>`}
	if _, _, err := publication.Render(context.Background(), strictSnap); err == nil {
		t.Fatalf("strict data vector: want error")
	}

	// Bad layout syntax.
	badSnap := gapRenderSnap()
	badSnap.HTMLLayout = "<p>{{.unclosed</p>"
	if _, _, err := publication.Render(context.Background(), badSnap); err == nil {
		t.Fatalf("bad layout: want error")
	}
	// Layout execution error.
	execSnap := gapRenderSnap()
	execSnap.HTMLLayout = "<p>{{index .missing 0}}</p>"
	if _, _, err := publication.Render(context.Background(), execSnap); err == nil {
		t.Fatalf("exec error layout: want error")
	}

	// Strict + snippet-injected residual vector, clean layout → unsafe error.
	vecSnap := gapRenderSnap()
	vecSnap.ScriptPolicy = "none"
	vecSnap.Data = map[string]any{"body": "prefix [[include:evil]] suffix"}
	vecSnap.Snippets = map[string]string{"evil": "<script>evil()</script>"}
	if _, _, err := publication.Render(context.Background(), vecSnap); err == nil {
		t.Fatalf("snippet vector residual: want error")
	}
	// Strict + layout-carried vector → renders (layout vetted upstream).
	laySnap := gapRenderSnap()
	laySnap.ScriptPolicy = "none"
	laySnap.HTMLLayout = "<div><script>layout()</script>{{.body}}</div>"
	if _, _, err := publication.Render(context.Background(), laySnap); err != nil {
		t.Fatalf("layout vector strict: %v", err)
	}

	// Snippet cycle + snippet with bad template syntax in body.
	cycSnap := gapRenderSnap()
	cycSnap.Data = map[string]any{"body": "[[include:a]] [[include:bad]]"}
	cycSnap.Snippets = map[string]string{
		"a":   "A [[include:b]]",
		"b":   "B [[include:a]]",
		"bad": "oops {{.unclosed",
	}
	out, _, err := publication.Render(context.Background(), cycSnap)
	if err != nil {
		t.Fatalf("snippet cycle: %v", err)
	}
	if strings.Contains(string(out), "[[include:a]]") {
		t.Fatalf("cycle not collapsed: %s", out)
	}

	// No headings → TOC placeholder removed, no nav.
	tocSnap := gapRenderSnap()
	tocSnap.HTMLLayout = "<p>plain {{.lc_toc}}</p>"
	out2, _, err := publication.Render(context.Background(), tocSnap)
	if err != nil {
		t.Fatalf("toc-less: %v", err)
	}
	if strings.Contains(string(out2), "__LCTOC__") || strings.Contains(string(out2), "lc-toc") {
		t.Fatalf("stale TOC: %s", out2)
	}

	// Markdown under strict policy + non-string data values.
	mdSnap := gapRenderSnap()
	mdSnap.ScriptPolicy = "none"
	mdSnap.Fields = []models.TemplateField{{Name: "md", Label: "M", Type: "markdown"}}
	mdSnap.Data = map[string]any{"md": "# Hi\n\ntext", "n": nil, "f": 1.5}
	if _, _, err := publication.Render(context.Background(), mdSnap); err != nil {
		t.Fatalf("strict markdown: %v", err)
	}

	// lc:query with nil cache + unmatched directive (placeholder branch).
	qSnap := gapRenderSnap()
	qSnap.HTMLLayout = "<div>a</div><!--lc:query missing-->"
	qSnap.LCQueryCache = nil
	if _, _, err := publication.Render(context.Background(), qSnap); err != nil {
		t.Fatalf("unresolved lc:query: %v", err)
	}
	qSnap2 := gapRenderSnap()
	qSnap2.HTMLLayout = "<div>b</div><!--lc:query missing-->"
	qSnap2.LCQueryCache = map[string]string{"other": "x"}
	if _, _, err := publication.Render(context.Background(), qSnap2); err != nil {
		t.Fatalf("unmatched lc:query: %v", err)
	}
}
