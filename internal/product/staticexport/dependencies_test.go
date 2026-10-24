package staticexport

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRelativeURLUsesOriginalPageNotDirectory(t *testing.T) {
	got, err := ResolveReference("https://cms.example/news/a", "chart.png")
	if err != nil || got != "https://cms.example/news/chart.png" {
		t.Fatalf("%q %v", got, err)
	}
}
func TestDocumentRejectsFragmentAndKeepsRawScripts(t *testing.T) {
	for _, s := range []string{`<article><pre>&lt;!doctype html&gt;</pre></article>`, `<!doctype html><article>fragment</article>`} {
		if IsStandaloneDocument([]byte(s)) {
			t.Fatal("fragment accepted")
		}
	}
	body := `<!doctype html><html><head><script>const x = '<img src="not-real.png">'; if (a < b) { console.log("&"); }</script></head><body><p>Article</p></body></html>`
	doc, err := Collect(context.Background(), PageSource{PublicURL: "https://cms.example/news/a"}, []byte(body), memoryResources{})
	if err != nil || string(doc.HTML) != body || len(doc.Resources) != 0 {
		t.Fatalf("raw script changed or parsed as HTML: %v", err)
	}
}

type memoryResources map[string]Resource

func TestCaptureBudgetIncludesNestedStylesheets(t *testing.T) {
	body := []byte(`<!doctype html><html><head><link rel="stylesheet" href="/a.css"></head><body></body></html>`)
	resources := memoryResources{"https://cms.example/a.css": {MIME: "text/css", Data: []byte(`@import "b.css";`)}, "https://cms.example/b.css": {MIME: "text/css", Data: []byte(`body{color:red}`)}}
	if _, err := collectBudget(context.Background(), PageSource{PublicURL: "https://cms.example/a"}, body, resources, int64(len(body)+20)); err == nil || !strings.Contains(err.Error(), "STATIC_ASSET_LIMIT_EXCEEDED") {
		t.Fatalf("nested budget not enforced: %v", err)
	}
}

func (m memoryResources) Resolve(_ context.Context, u string) (Resource, error) {
	if r, ok := m[u]; ok {
		return r, nil
	}
	if strings.HasPrefix(u, "https://external.example/") {
		return Resource{}, ErrExternalResource
	}
	return Resource{}, errors.New("resource absent")
}
func TestDependencyClosureIncludesCSSSrcsetAndSocialImages(t *testing.T) {
	root := "https://cms.example"
	resources := memoryResources{
		root + "/styles/main.css":  {MIME: "text/css", Data: []byte(`@import "child.css"; .x{background:url('../img/chart.png')}`)},
		root + "/styles/child.css": {MIME: "text/css", Data: []byte(`.x{color:red}`)},
		root + "/img/chart.png":    {MIME: "image/png", Data: []byte("image")},
		root + "/img/chart2.png":   {MIME: "image/png", Data: []byte("image2")},
		root + "/img/share.png":    {MIME: "image/png", Data: []byte("share")},
	}
	body := `<!doctype html><html><head><link rel="stylesheet" href="/styles/main.css"><meta property="og:image" content="/img/share.png"></head><body><img srcset="/img/chart.png 1x, /img/chart2.png 2x"><a href="https://external.example/citation">Source</a></body></html>`
	doc, err := Collect(context.Background(), PageSource{PublicURL: root + "/news/a"}, []byte(body), resources)
	if err != nil || len(doc.Resources) != 5 || len(doc.External) != 0 {
		t.Fatalf("closure: resources=%d external=%v err=%v", len(doc.Resources), doc.External, err)
	}
}
func TestDependencyCSSCycleIsError(t *testing.T) {
	body := `<!doctype html><html><head><link rel="stylesheet" href="/a.css"></head><body></body></html>`
	r := memoryResources{"https://cms.example/a.css": {MIME: "text/css", Data: []byte(`@import "b.css";`)}, "https://cms.example/b.css": {MIME: "text/css", Data: []byte(`@import "a.css";`)}}
	if _, err := Collect(context.Background(), PageSource{PublicURL: "https://cms.example/news/a"}, []byte(body), r); err == nil {
		t.Fatal("cyclic CSS accepted")
	}
}
func TestDependencyExternalRecordedButNotDownloaded(t *testing.T) {
	body := `<!doctype html><html><head></head><body><img src="https://external.example/a.png"></body></html>`
	doc, err := Collect(context.Background(), PageSource{PublicURL: "https://cms.example/news/a"}, []byte(body), memoryResources{})
	if err != nil || len(doc.External) != 1 {
		t.Fatalf("external dependency not diagnosed: %+v %v", doc, err)
	}
}
func TestResolverReadsOwnedFilesAndRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "static", "images"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "static", "images", "图.svg"), []byte(`<svg></svg>`), 0600); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewFileResolver(root, "https://cms.example", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolver.Resolve(context.Background(), "https://cms.example/static/images/%E5%9B%BE.svg?v=1")
	if err != nil || string(got.Data) != "<svg></svg>" {
		t.Fatalf("read UTF8 resource: %v", err)
	}
	for _, p := range []string{"/static/../.env", "/static/%2e%2e/.env", "/static/%2Fetc/passwd", "/static/a\\b", "/.git/config"} {
		if _, err := resolver.Resolve(context.Background(), p); err == nil {
			t.Fatalf("unsafe reference accepted: %s", p)
		}
	}
	_, err = resolver.Resolve(context.Background(), "https://other.example/image.png")
	if !errors.Is(err, ErrExternalResource) {
		t.Fatal("unowned origin was read")
	}
}

func TestResolverCannotReadPrivateFileViaPublicSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "static"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "private-key"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../private-key", filepath.Join(root, "static", "key.txt")); err != nil {
		t.Fatal(err)
	}
	r, err := NewFileResolver(root, "https://cms.example", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Resolve(context.Background(), "/static/key.txt"); err == nil {
		t.Fatal("private project file escaped its public source root")
	}
}
