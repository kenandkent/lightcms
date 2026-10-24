package staticexport

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

type archiveSource struct{ body []byte }

func (a archiveSource) Resolve(context.Context, Item) (PageSource, error) {
	return PageSource{}, io.EOF
}
func (a archiveSource) Open(context.Context, PageSource) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(a.body)), nil
}

func TestArchiveBuildsStandaloneZIPWithoutPrivateFiles(t *testing.T) {
	root := t.TempDir()
	body := []byte(`<!doctype html><html><head><title>News</title><link rel="icon" href="https://cms.example/static/icon.svg"></head><body><img src="chart.png"><script>const untouched = "cms.example";</script></body></html>`)
	sum := sha256.Sum256(body)
	frozen := FrozenExport{ID: primitive.NewObjectID(), OperationID: primitive.NewObjectID(), CapturedAt: time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC), Pages: []PageSource{{ContentID: primitive.NewObjectID(), PublicationID: primitive.NewObjectID(), FullPath: "/news/a", PublicURL: "https://cms.example/news/a", SourceSHA256: hex.EncodeToString(sum[:]), StoragePath: "/private/original-path"}}}
	resources := memoryResources{"https://cms.example/static/icon.svg": {Data: []byte(`<svg/>`), MIME: "image/svg+xml"}, "https://cms.example/news/chart.png": {Data: []byte("chart"), MIME: "image/png"}}
	svc := NewService(archiveSource{body}, resources, Options{ProjectRoot: root, Root: filepath.Join(root, "exports")})
	artifact, err := svc.Build(context.Background(), frozen)
	if err != nil {
		t.Fatal(err)
	}
	var zipBytes bytes.Buffer
	if err = svc.WriteZIP(context.Background(), artifact.ID, &zipBytes); err != nil {
		t.Fatal(err)
	}
	z, err := zip.NewReader(bytes.NewReader(zipBytes.Bytes()), int64(zipBytes.Len()))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, file := range z.File {
		r, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		files[file.Name] = string(data)
	}
	for _, p := range []string{"manifest.json", "README.txt", "public/news/a/index.html", "public/news/chart.png", "public/static/icon.svg"} {
		if _, ok := files[p]; !ok {
			t.Fatalf("missing %s", p)
		}
	}
	page := files["public/news/a/index.html"]
	if !strings.Contains(page, `src="/news/chart.png"`) || !strings.Contains(page, `href="/static/icon.svg"`) || !strings.Contains(page, `const untouched = "cms.example";`) {
		t.Fatalf("incorrect export rewrite: %s", page)
	}
	if strings.Contains(files["manifest.json"], "/private/original-path") {
		t.Fatal("source storage path leaked")
	}
	for name := range files {
		if strings.Contains(name, "source/") || strings.Contains(name, "private/") {
			t.Fatalf("private file leaked: %s", name)
		}
	}
	if err = svc.WriteHTML(context.Background(), artifact.ID, &bytes.Buffer{}); err == nil {
		t.Fatal("dependent page exported as single HTML")
	}
	if _, err = svc.Build(context.Background(), frozen); err != nil {
		t.Fatalf("same export cannot be recovered: %v", err)
	}
}

func TestArchiveRejectsPrivateEntries(t *testing.T) {
	for _, p := range []string{"../token", "private/provenance.json", "/etc/passwd", "public/../secret", "public/a\\b", "public//a"} {
		if IsPublicArchivePath(p) {
			t.Fatalf("unsafe path %q", p)
		}
	}
}

func TestArchiveFailureDoesNotExposeHalfPackage(t *testing.T) {
	root := t.TempDir()
	body := []byte(`<!doctype html><html><head></head><body><img src="missing.png"></body></html>`)
	sum := sha256.Sum256(body)
	frozen := FrozenExport{ID: primitive.NewObjectID(), Pages: []PageSource{{FullPath: "/a", PublicURL: "https://cms.example/a", SourceSHA256: hex.EncodeToString(sum[:])}}}
	svc := NewService(archiveSource{body}, memoryResources{}, Options{ProjectRoot: root, Root: filepath.Join(root, "exports")})
	if _, err := svc.Build(context.Background(), frozen); err == nil {
		t.Fatal("missing resource accepted")
	}
	if err := svc.WriteZIP(context.Background(), frozen.ID, &bytes.Buffer{}); err == nil {
		t.Fatal("partial package downloadable")
	}
	if _, err := os.Stat(filepath.Join(root, "exports", "exports", frozen.ID.Hex(), "complete.json")); !os.IsNotExist(err) {
		t.Fatal("failed package has completion marker")
	}
}

func TestArchiveAllowsSingleHTMLWhenNoResourceFiles(t *testing.T) {
	root := t.TempDir()
	body := []byte(`<!doctype html><html><head><style>body{color:red}</style></head><body>News</body></html>`)
	sum := sha256.Sum256(body)
	frozen := FrozenExport{ID: primitive.NewObjectID(), Pages: []PageSource{{FullPath: "/", PublicURL: "https://cms.example/", SourceSHA256: hex.EncodeToString(sum[:])}}}
	svc := NewService(archiveSource{body}, memoryResources{}, Options{ProjectRoot: root, Root: filepath.Join(root, "exports")})
	if _, err := svc.Build(context.Background(), frozen); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := svc.WriteHTML(context.Background(), frozen.ID, &out); err != nil || out.String() != string(body) {
		t.Fatalf("single HTML: %v", err)
	}
}
