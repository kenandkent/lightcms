package publication_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func testSnapshot() publication.RenderSnapshot {
	pubID := primitive.NewObjectID()
	contentID := primitive.NewObjectID()
	tmplVerID := primitive.NewObjectID()
	tmplID := primitive.NewObjectID()
	logicalAt := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	return publication.RenderSnapshot{
		PublicationID:     pubID,
		ContentID:         contentID,
		ContentVersion:    8,
		TemplateID:        tmplID,
		TemplateVersionID: tmplVerID,
		TemplateVersion:   3,
		TemplateSlug:      "financial-news",
		FullPath:          "/news/bitcoin-market-update",
		Title:             "Bitcoin Market Update",
		Slug:              "bitcoin-market-update",
		Data: map[string]any{
			"headline": "Bitcoin Rallies",
			"body":     "Markets move higher",
		},
		HTMLLayout: `<article><h1>{{.headline}}</h1><div>{{.body}}</div><p class="meta">{{.publication_id}}|{{.published_at}}|{{.public_url}}|{{.template_version}}</p></article>`,
		Fields: []models.TemplateField{
			{Name: "headline", Label: "Headline", Type: "text", Required: true},
			{Name: "body", Label: "Body", Type: "richtext", Required: true},
		},
		ScriptPolicy:       "all",
		LogicalPublishedAt: logicalAt,
		PublicURL:          "https://pages.example.com/news/bitcoin-market-update",
	}
}

// TestRenderSnapshot is the Task 7 red test: frozen snapshot renders system
// vars deterministically with identical bytes/hash on re-render.
func TestRenderSnapshot(t *testing.T) {
	ctx := context.Background()
	snap := testSnapshot()

	html1, hash1, err := publication.Render(ctx, snap)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(html1) == 0 {
		t.Fatal("Render returned empty HTML")
	}
	if !strings.Contains(string(html1), snap.PublicationID.Hex()) {
		t.Errorf("rendered HTML missing frozen {{.publication_id}} %s", snap.PublicationID.Hex())
	}
	if !strings.Contains(string(html1), snap.PublicURL) {
		t.Errorf("rendered HTML missing frozen {{.public_url}} %s", snap.PublicURL)
	}
	if !strings.Contains(string(html1), "3") {
		t.Errorf("rendered HTML missing frozen {{.template_version}}")
	}
	// published_at must derive from the frozen logical time, not wall clock.
	if !strings.Contains(string(html1), snap.LogicalPublishedAt.UTC().Format(time.RFC3339)) {
		t.Errorf("rendered HTML missing frozen {{.published_at}} %s; got %s",
			snap.LogicalPublishedAt.UTC().Format(time.RFC3339), html1)
	}

	html2, hash2, err := publication.Render(ctx, snap)
	if err != nil {
		t.Fatalf("second Render: %v", err)
	}
	if string(html1) != string(html2) {
		t.Errorf("re-render of same snapshot yielded different bytes:\n%s\n---\n%s", html1, html2)
	}
	if hash1 != hash2 {
		t.Errorf("re-render of same snapshot yielded different hash %s vs %s", hash1, hash2)
	}
	if err := publication.VerifyContentHash(html1, hash1); err != nil {
		t.Errorf("VerifyContentHash: %v", err)
	}
}

// TestRenderSnapshotFrozenLogicalTime proves the renderer never reads the wall
// clock: two snapshots identical except for LogicalPublishedAt render different
// bytes, while re-rendering either snapshot is byte-identical.
func TestRenderSnapshotFrozenLogicalTime(t *testing.T) {
	ctx := context.Background()
	a := testSnapshot()
	b := testSnapshot()
	b.PublicationID = a.PublicationID
	b.ContentID = a.ContentID
	b.TemplateVersionID = a.TemplateVersionID
	b.LogicalPublishedAt = a.LogicalPublishedAt.Add(time.Hour)

	htmlA, hashA, err := publication.Render(ctx, a)
	if err != nil {
		t.Fatalf("Render A: %v", err)
	}
	htmlB, hashB, err := publication.Render(ctx, b)
	if err != nil {
		t.Fatalf("Render B: %v", err)
	}
	if string(htmlA) == string(htmlB) {
		t.Error("different frozen logical times must render different bytes")
	}
	if hashA == hashB {
		t.Error("different frozen logical times must yield different hashes")
	}
	// Re-render A: must be identical (no wall-clock drift).
	htmlA2, hashA2, err := publication.Render(ctx, a)
	if err != nil {
		t.Fatalf("re-render A: %v", err)
	}
	if string(htmlA) != string(htmlA2) || hashA != hashA2 {
		t.Error("re-render of frozen snapshot must be byte/hash identical")
	}
}

// TestRenderSnapshotMissingRequiredField fails before any staging or file
// write when a required template field has no value.
func TestRenderSnapshotMissingRequiredField(t *testing.T) {
	ctx := context.Background()
	snap := testSnapshot()
	delete(snap.Data, "headline")
	if _, _, err := publication.Render(ctx, snap); err == nil {
		t.Fatal("Render with missing required field must fail")
	} else if publication.CodeOfRender(err) != publication.CodeRenderValidation {
		t.Fatalf("missing required field code = %q, want %q (%v)",
			publication.CodeOfRender(err), publication.CodeRenderValidation, err)
	}
}

// TestRenderSnapshotUnsafeContent fails before staging when raw script vectors
// appear under a restrictive script policy.
func TestRenderSnapshotUnsafeContent(t *testing.T) {
	ctx := context.Background()
	snap := testSnapshot()
	snap.ScriptPolicy = "none"
	snap.Data["body"] = `<p>hi</p><script>alert(1)</script>`
	if _, _, err := publication.Render(ctx, snap); err == nil {
		t.Fatal("Render with unsafe content under policy=none must fail")
	} else if publication.CodeOfRender(err) != publication.CodeRenderUnsafe {
		t.Fatalf("unsafe content code = %q, want %q (%v)",
			publication.CodeOfRender(err), publication.CodeRenderUnsafe, err)
	}

	// Same payload passes under policy=all (default allow).
	snap2 := testSnapshot()
	snap2.Data["body"] = `<p>hi</p><script>alert(1)</script>`
	if _, _, err := publication.Render(ctx, snap2); err != nil {
		t.Fatalf("policy=all must allow raw HTML, got %v", err)
	}
}

// TestRenderSnapshotProvenance records renderer version, build SHA and
// dependency snapshot without reading a newer mutable template.
func TestRenderSnapshotProvenance(t *testing.T) {
	ctx := context.Background()
	snap := testSnapshot()
	snap.DependencySnapshot = map[string]any{
		"theme_hash":      "sha256:theme1",
		"snippet_hash":    "sha256:snip1",
		"sanitizer":       "all",
		"wikilink_version": "v1",
		"toc_version":      "v1",
	}
	res, err := publication.RenderDetailed(ctx, snap)
	if err != nil {
		t.Fatalf("RenderDetailed: %v", err)
	}
	if res.RendererVersion == "" {
		t.Error("RendererVersion must be recorded")
	}
	if res.ProductBuildSHA == "" {
		t.Error("ProductBuildSHA must be recorded")
	}
	if res.RenderDependenciesHash == "" {
		t.Error("RenderDependenciesHash must be recorded")
	}
	if res.DependencySnapshot == nil {
		t.Error("DependencySnapshot must be recorded")
	}
	if res.ContentHash == "" {
		t.Error("ContentHash must be recorded")
	}
	if err := publication.VerifyContentHash(res.HTML, res.ContentHash); err != nil {
		t.Errorf("VerifyContentHash(detailed): %v", err)
	}
	// Frozen layout is authoritative: mutating a would-be mutable template
	// must not change output unless the snapshot layout changes.
	mutated := snap
	mutated.HTMLLayout = `<article>CHANGED {{.headline}}</article>`
	htmlMut, _, err := publication.Render(ctx, mutated)
	if err != nil {
		t.Fatalf("Render mutated layout: %v", err)
	}
	if string(htmlMut) == string(res.HTML) {
		t.Error("render must follow the frozen snapshot layout, not a mutable template")
	}
}

// TestRenderSnapshotMarkdownSnippetTOC exercises the extracted in-memory
// pipeline: markdown conversion, snippet includes, heading IDs, TOC and
// wikilinks — all from frozen snapshot data, with no DB or file writes.
func TestRenderSnapshotMarkdownSnippetTOC(t *testing.T) {
	ctx := context.Background()
	snap := testSnapshot()
	snap.Fields = []models.TemplateField{
		{Name: "body", Label: "Body", Type: "markdown", Required: true},
	}
	snap.Data = map[string]any{
		"body": "# Hello\n\nSee [[Other Page]] and [[include:cta]].",
	}
	snap.HTMLLayout = `<article>{{.body}}{{.lc_toc}}</article>`
	snap.Snippets = map[string]string{"cta": `<a href="/signup">Sign up</a>`}
	snap.TitleToPath = map[string]string{"other page": "/news/other-page"}
	snap.PathToTitle = map[string]string{"/news/other-page": "Other Page"}
	html, _, err := publication.Render(ctx, snap)
	if err != nil {
		t.Fatalf("Render markdown pipeline: %v", err)
	}
	out := string(html)
	if !strings.Contains(out, "<h1") || !strings.Contains(out, "Hello") {
		t.Errorf("markdown heading not rendered; got %s", out)
	}
	if !strings.Contains(out, `<a href="/signup">Sign up</a>`) {
		t.Errorf("snippet include not expanded; got %s", out)
	}
	if !strings.Contains(out, `<a href="/news/other-page">`) {
		t.Errorf("wikilink not resolved; got %s", out)
	}
	if !strings.Contains(out, `class="lc-toc"`) {
		t.Errorf("TOC not injected; got %s", out)
	}
}

// TestBytesHashVerify covers the BytesHash/render-hash verify contract: the
// hash identifies the exact bytes, and any byte flip fails verification.
func TestBytesHashVerify(t *testing.T) {
	html := []byte("<article><h1>hi</h1></article>")
	hash := publication.BytesHash(html)
	if hash == "" {
		t.Fatal("BytesHash must not be empty")
	}
	if err := publication.VerifyContentHash(html, hash); err != nil {
		t.Fatalf("verify own hash: %v", err)
	}
	tampered := []byte("<article><h1>hi!</h1></article>")
	if err := publication.VerifyContentHash(tampered, hash); err == nil {
		t.Fatal("tampered bytes must fail hash verification")
	}
}
