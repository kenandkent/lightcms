package staticdelivery

import (
	"context"
	"encoding/json"
	ex "github.com/metavert-io/lightcms/v7/internal/product/staticexport"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"os"
	"strings"
	"testing"
)

func TestNativeRedirectEncodedLineBudget(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		rejected   bool
	}{
		{"boundary", "/" + strings.Repeat("a", 494), false},
		{"over-boundary", "/" + strings.Repeat("a", 495), true},
		{"encoded", "/" + strings.Repeat("中", 60), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := BuildRelease(context.Background(), BuildInput{ID: primitive.NewObjectID(), Target: releaseTarget(), Pages: []CapturedPage{releasePage(primitive.NewObjectID(), tc.path)}})
			if err == nil {
				_, err = nativeAssetConfig(r)
			}
			if (err != nil) != tc.rejected {
				t.Fatalf("rejected=%v error=%v", tc.rejected, err)
			}
		})
	}
}

func TestAssetsFirstReleaseCarriesNativeRoutingConfig(t *testing.T) {
	pages := []CapturedPage{}
	for _, p := range []string{"/", "/news/a", "/news/a.html", "/news/b/index", "/中文"} {
		pages = append(pages, releasePage(primitive.NewObjectID(), p))
	}
	pages[0].Document.HTML = []byte(strings.Replace(string(pages[0].Document.HTML), "</body>", `<img src="https://source.test/pixel.png"></body>`, 1))
	pages[0].Document.Resources = map[string]ex.Resource{"https://source.test/pixel.png": {URL: "https://source.test/pixel.png", MIME: "image/png", Data: []byte{137, 80, 78, 71}}}
	r, e := BuildRelease(context.Background(), BuildInput{ID: primitive.NewObjectID(), Target: releaseTarget(), Pages: pages})
	if e != nil {
		t.Fatal(e)
	}
	r.DeliveryMode = AssetsFirstV1 // Compatibility for already-frozen releases.
	modules, e := ReleaseModules(r)
	if e != nil {
		t.Fatal(e)
	}
	var config map[string]any
	for _, m := range modules {
		if m.Name == "delivery-assets-config.json" {
			if e = json.Unmarshal(m.Data, &config); e != nil {
				t.Fatal(e)
			}
		}
	}
	if config == nil {
		t.Fatal("native asset configuration is missing; all readers still invoke the Worker")
	}
	if config["html_handling"] != "none" || config["not_found_handling"] != "none" {
		t.Fatal("native URL handling would change canonical paths", config)
	}
	paths, ok := config["run_worker_first"].([]any)
	if !ok || len(paths) != 1 || paths[0] != "/.__lc_delivery/health" {
		t.Fatal("normal articles must not force Worker execution", config)
	}
	headers, _ := config["_headers"].(string)
	if !strings.Contains(headers, "X-LC-Release: "+r.ID.Hex()) || !strings.Contains(headers, "X-Robots-Tag: noindex, nofollow") || !strings.Contains(headers, "/_lc-assets/*") {
		t.Fatal("missing native response contract", headers)
	}
	if config["_redirects"] != "/ /.__lc_delivery/root.html 200\n" {
		t.Fatal("root must be a static internal rewrite", config)
	}
	files, e := ReleaseUploadFiles(r)
	if e != nil {
		t.Fatal(e)
	}
	want := map[string]bool{"/.__lc_delivery/root.html": false, "/news/a": false, "/news/a.html": false, "/news/b/index": false, "/中文": false}
	for _, f := range files {
		if _, ok := want[f.PublicPath]; ok {
			want[f.PublicPath] = true
			if f.MIME != "text/html; charset=utf-8" {
				t.Fatal("extensionless HTML lost MIME")
			}
		}
	}
	for p, found := range want {
		if !found {
			t.Fatal("missing direct static path", p)
		}
	}
	var routes struct {
		Pages map[string]struct{ File string }
	}
	if e = json.Unmarshal(modules[2].Data, &routes); e != nil {
		t.Fatal(e)
	}
	for _, p := range []string{"/news/a", "/news/a.html", "/news/b/index", "/中文"} {
		if routes.Pages[p].File != p {
			t.Fatalf("upload route %s still uses %s", p, routes.Pages[p].File)
		}
	}
	if file := os.Getenv("STATIC_DELIVERY_NATIVE_FIXTURE"); file != "" {
		data, e := json.Marshal(struct {
			Modules []Module
			Files   []UploadFile
		}{modules, files})
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(file, data, 0600); e != nil {
			t.Fatal(e)
		}
	}
	if file := os.Getenv("STATIC_DELIVERY_ASSETS_ONLY_FIXTURE"); file != "" {
		r.DeliveryMode = AssetsOnlyV1
		modules, err := ReleaseModules(r)
		if err != nil {
			t.Fatal(err)
		}
		files, err := ReleaseUploadFiles(r)
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(struct {
			Modules []Module
			Files   []UploadFile
		}{modules, files})
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(file, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLegacyReleaseStillUsesOriginalUploadLayout(t *testing.T) {
	r, e := BuildRelease(context.Background(), BuildInput{ID: primitive.NewObjectID(), Target: releaseTarget(), Pages: []CapturedPage{releasePage(primitive.NewObjectID(), "/a")}})
	if e != nil {
		t.Fatal(e)
	}
	r.DeliveryMode = "" // persisted before the asset-first upgrade
	files, e := ReleaseUploadFiles(r)
	if e != nil || len(files) != 1 || files[0].PublicPath != "/a/index.html" {
		t.Fatal("legacy frozen execution changed", e)
	}
	modules, e := ReleaseModules(r)
	if e != nil || len(modules) != 3 {
		t.Fatal("legacy version acquired new asset config", e)
	}
}

func TestAssetsFirstRejectsNativeControlFilePaths(t *testing.T) {
	for _, full := range []string{"/_headers", "/_redirects"} {
		_, e := BuildRelease(context.Background(), BuildInput{ID: primitive.NewObjectID(), Target: releaseTarget(), Pages: []CapturedPage{releasePage(primitive.NewObjectID(), full)}})
		if CodeOf(e) != CodePathConflict {
			t.Fatalf("native control path accepted: %s %v", full, e)
		}
	}
}
