package publication_test

import (
	"context"
	"github.com/metavert-io/lightcms/v7/internal/product/publication"
	"strings"
	"testing"
)

func TestLayoutHeadIncludesFrozenSnippet(t *testing.T) {
	snap := testSnapshot()
	snap.HTMLLayout = `<!doctype html><html><head>[[include:test]]</head><body>{{.title}}</body></html>`
	snap.Snippets = map[string]string{"test": `<meta name="test-title" content="{{.title}}"><script src="/test.js"></script>`}
	first, hash, e := publication.Render(context.Background(), snap)
	if e != nil {
		t.Fatal(e)
	}
	head := strings.Split(string(first), "</head>")[0]
	if !strings.Contains(head, `src="/test.js"`) || !strings.Contains(head, `content="Bitcoin Market Update"`) || strings.Contains(string(first), "[[include:") {
		t.Fatalf("snippet was not expanded in head: %s", first)
	}
	again, againHash, e := publication.Render(context.Background(), snap)
	if e != nil || string(again) != string(first) || againHash != hash {
		t.Fatal("snapshot is not reproducible")
	}
	next := snap
	next.Snippets = map[string]string{"test": `<script src="/updated.js"></script>`}
	changed, _, e := publication.Render(context.Background(), next)
	if e != nil || !strings.Contains(string(changed), "/updated.js") {
		t.Fatal("new snapshot did not use updated snippet")
	}
}

func TestLayoutSnippetErrorsAreNotPublishedAsLiteralMarkers(t *testing.T) {
	for _, snippets := range []map[string]string{{}, {"test": "[[include:test]]"}, {"test": "[[include:b]]", "b": "[[include:test]]"}} {
		snap := testSnapshot()
		snap.HTMLLayout = `<head>[[include:test]]</head>`
		snap.Snippets = snippets
		if _, _, e := publication.Render(context.Background(), snap); e == nil {
			t.Fatal("missing/cyclic layout snippet accepted")
		}
	}
}

func TestLayoutSnippetDoesNotReinterpretUserTemplateSyntax(t *testing.T) {
	snap := testSnapshot()
	snap.Title = `{{.publication_id}}`
	snap.HTMLLayout = `<head>[[include:test]]</head>`
	snap.Snippets = map[string]string{"test": `<meta name="test" content="{{.title}}">`}
	html, _, e := publication.Render(context.Background(), snap)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(html), `content="{{.publication_id}}"`) {
		t.Fatalf("user value was re-executed: %s", html)
	}
}

func TestLayoutSnippetRespectsScriptPolicy(t *testing.T) {
	snap := testSnapshot()
	snap.ScriptPolicy = "none"
	snap.HTMLLayout = `<head>[[include:test]]</head>`
	snap.Snippets = map[string]string{"test": `<script src="/test.js"></script>`}
	if _, _, e := publication.Render(context.Background(), snap); e == nil {
		t.Fatal("script snippet bypassed frozen policy")
	}
}
