package staticdelivery

import (
	"context"
	ex "github.com/metavert-io/lightcms/v7/internal/product/staticexport"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"strings"
	"testing"
	"time"
)

func releasePage(id primitive.ObjectID, full string) CapturedPage {
	return CapturedPage{ExportID: primitive.NewObjectID(), Document: ex.Document{Source: ex.PageSource{ContentID: id, PublicationID: primitive.NewObjectID(), FullPath: full, PublicURL: "https://source.test" + full}, HTML: []byte(`<!doctype html><html><head><title>Report</title><link rel="canonical" href="https://source.test/old"><meta property="og:url" content="https://source.test/old"></head><body><a href="/news/b">next</a><script>const untouched="https://source.test/old";</script></body></html>`), Resources: map[string]ex.Resource{}}}
}
func releaseTarget() Target {
	return Target{Record: newRecord(), InstallationID: "installation", Origin: "https://reports.example.com", Environment: "test", ConfigRevision: 1, AssetLimit: 20000}
}
func TestReleaseMergesAndRejectsOwnershipConflict(t *testing.T) {
	ctx := context.Background()
	target := releaseTarget()
	a, b := primitive.NewObjectID(), primitive.NewObjectID()
	first, e := BuildRelease(ctx, BuildInput{ID: primitive.NewObjectID(), Target: target, Pages: []CapturedPage{releasePage(a, "/news/a"), releasePage(b, "/news/b")}, Now: time.Now().UTC()})
	if e != nil {
		t.Fatal(e)
	}
	next, e := BuildRelease(ctx, BuildInput{ID: primitive.NewObjectID(), Target: target, Current: &first, Pages: []CapturedPage{releasePage(a, "/news/a")}, Now: time.Now().UTC()})
	if e != nil || len(next.Pages) != 2 {
		t.Fatalf("lost existing pages %+v %v", next, e)
	}
	page := next.Pages["/news/a"]
	html := string(next.Files[page.File].Data)
	if !strings.Contains(html, `href="https://reports.example.com/news/a"`) || !strings.Contains(html, `href="/news/b"`) || !strings.Contains(html, `const untouched="https://source.test/old";`) {
		t.Fatal(html)
	}
	if _, e = BuildRelease(ctx, BuildInput{ID: primitive.NewObjectID(), Target: target, Current: &first, Pages: []CapturedPage{releasePage(primitive.NewObjectID(), "/news/a")}}); CodeOf(e) != CodePathConflict {
		t.Fatalf("path takeover %v", e)
	}
	renamed, e := BuildRelease(ctx, BuildInput{ID: primitive.NewObjectID(), Target: target, Current: &first, Pages: []CapturedPage{releasePage(a, "/news/new")}})
	if e != nil || len(renamed.Pages) != 3 {
		t.Fatalf("rename silently deleted original %v", e)
	}
}
func TestReleaseCSSDependencyHashAndCycle(t *testing.T) {
	p := releasePage(primitive.NewObjectID(), "/news/a")
	p.Document.HTML = []byte(`<!doctype html><html><head><title>A</title><link rel="stylesheet" href="/main.css"></head><body><img src="/图.png"></body></html>`)
	p.Document.Resources = map[string]ex.Resource{"https://source.test/main.css": {URL: "https://source.test/main.css", MIME: "text/css", Data: []byte(`@import "nested.css";body{background:url('/图.png')}`)}, "https://source.test/nested.css": {URL: "https://source.test/nested.css", MIME: "text/css", Data: []byte(`p{color:red}`)}, "https://source.test/%E5%9B%BE.png": {URL: "https://source.test/%E5%9B%BE.png", MIME: "image/png", Data: []byte("picture")}}
	r, e := BuildRelease(context.Background(), BuildInput{ID: primitive.NewObjectID(), Target: releaseTarget(), Pages: []CapturedPage{p}})
	if e != nil {
		t.Fatal(e)
	}
	cssCount := 0
	for path, f := range r.Files {
		if !strings.Contains(f.MIME, "css") {
			continue
		}
		cssCount++
		if !strings.HasPrefix(path, "/_lc-assets/") || !strings.Contains(path, releaseHash(f.Data)) {
			t.Fatal("CSS hash not based on final bytes", path)
		}
		if strings.Contains(string(f.Data), "图.png") || strings.Contains(string(f.Data), "nested.css") {
			t.Fatal("unrewritten CSS", string(f.Data))
		}
	}
	if cssCount != 2 {
		t.Fatal(cssCount)
	}
	p.Document.Resources["https://source.test/nested.css"] = ex.Resource{URL: "https://source.test/nested.css", MIME: "text/css", Data: []byte(`@import "main.css";`)}
	if _, e = BuildRelease(context.Background(), BuildInput{ID: primitive.NewObjectID(), Target: releaseTarget(), Pages: []CapturedPage{p}}); CodeOf(e) != CodeDependencyUnresolved {
		t.Fatalf("cyclic CSS accepted %v", e)
	}
}
func TestRetainedAssetExpiryDoesNotRefresh(t *testing.T) {
	now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	p := releasePage(primitive.NewObjectID(), "/a")
	p.Document.HTML = []byte(`<!doctype html><html><head><title>A</title></head><body><img src="/a.png"></body></html>`)
	p.Document.Resources = map[string]ex.Resource{"https://source.test/a.png": {URL: "https://source.test/a.png", MIME: "image/png", Data: []byte("a")}}
	target := releaseTarget()
	r, e := BuildRelease(context.Background(), BuildInput{ID: primitive.NewObjectID(), Target: target, Pages: []CapturedPage{p}, Now: now})
	if e != nil {
		t.Fatal(e)
	}
	withdrawn, e := BuildRelease(context.Background(), BuildInput{ID: primitive.NewObjectID(), Target: target, Current: &r, RemovePaths: []string{"/a"}, Now: now.Add(time.Hour), RetentionDays: 90})
	if e != nil {
		t.Fatal(e)
	}
	later, e := BuildRelease(context.Background(), BuildInput{ID: primitive.NewObjectID(), Target: target, Current: &withdrawn, Now: now.Add(2 * time.Hour), RetentionDays: 90})
	if e != nil {
		t.Fatal(e)
	}
	for path, f := range withdrawn.Files {
		if f.RetainedUntil == nil {
			t.Fatal("old asset not retained")
		}
		if !later.Files[path].RetainedUntil.Equal(*f.RetainedUntil) {
			t.Fatal("retention refreshed")
		}
	}
	expired, e := BuildRelease(context.Background(), BuildInput{ID: primitive.NewObjectID(), Target: target, Current: &later, Now: now.Add(92 * 24 * time.Hour)})
	if e != nil || len(expired.Files) != 0 {
		t.Fatal("expired assets not removed", e)
	}
}
func TestPreviewDigestCoversIdentityAndNormalizesSets(t *testing.T) {
	p := Preview{TargetID: primitive.NewObjectID(), BaseRevision: 7, ConfigRevision: 2, Kind: "deploy", ExportHashes: []string{"b", "a"}, RemovePaths: []string{"/b", "/a"}}
	a := PreviewDigest(p)
	p.ExportHashes = []string{"a", "b"}
	p.RemovePaths = []string{"/a", "/b"}
	if PreviewDigest(p) != a {
		t.Fatal("set ordering changed digest")
	}
	p.BaseRevision++
	if PreviewDigest(p) == a {
		t.Fatal("base revision omitted")
	}
	p.BaseRevision--
	p.Kind = "rollback"
	if PreviewDigest(p) == a {
		t.Fatal("operation omitted")
	}
}
