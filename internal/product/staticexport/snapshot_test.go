package staticexport

import (
	"bytes"
	"context"
	"github.com/metavert-io/lightcms/v7/internal/product/artifacts"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type testSourceObjects struct{ store *artifacts.Store }

func (s testSourceObjects) SaveExportObject(ctx context.Context, _ primitive.ObjectID, data []byte, mime string) (artifacts.Ref, error) {
	return s.store.Put(ctx, bytes.NewReader(data), mime, MaxResourceBytes)
}
func (s testSourceObjects) OpenExportObject(ctx context.Context, ref artifacts.Ref) (io.ReadCloser, error) {
	return s.store.Open(ctx, ref)
}
func TestObjectSnapshotDeduplicatesPrivateSourcesAndReadsLegacy(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	body := []byte(`<html><head><title>Object</title></head><body>Frozen</body></html>`)
	store, err := artifacts.NewStore(filepath.Join(root, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := NewService(archiveSource{body}, nil, Options{ProjectRoot: root, Root: filepath.Join(root, "private"), Objects: testSourceObjects{store}})
	for n := 0; n < 2; n++ {
		frozen := FrozenExport{ID: primitive.NewObjectID(), Pages: []PageSource{{FullPath: "/a", PublicURL: "https://source.test/a", SourceSHA256: bytesHash(body)}}}
		if _, err := svc.Build(ctx, frozen); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(root, "private", "exports", frozen.ID.Hex(), "source", "0.html")); !os.IsNotExist(err) {
			t.Fatal("private source still duplicated per export")
		}
		docs, err := svc.Documents(ctx, frozen.ID)
		if err != nil || len(docs) != 1 || !bytes.Equal(docs[0].HTML, body) {
			t.Fatalf("object snapshot reload: %v", err)
		}
	}
	files, err := filepath.Glob(filepath.Join(root, "objects", "objects", "[0-9a-f][0-9a-f]", "*"))
	if err != nil || len(files) != 1 {
		t.Fatalf("objects not deduplicated: %v %v", files, err)
	}
}

func TestSnapshotReloadNeverReadsMutableSource(t *testing.T) {
	root := t.TempDir()
	body := []byte(`<!doctype html><html><head><title>A</title><link rel="stylesheet" href="/main.css?v=1"></head><body><img src="/image.png"></body></html>`)
	frozen := FrozenExport{ID: primitive.NewObjectID(), Pages: []PageSource{{FullPath: "/a", PublicURL: "https://source.test/a", SourceSHA256: bytesHash(body)}}}
	resources := memoryResources{"https://source.test/main.css?v=1": {MIME: "text/css", Data: []byte(`body{background:url('/image.png')}`)}, "https://source.test/image.png": {MIME: "image/png", Data: []byte("old-image")}}
	svc := NewService(archiveSource{body}, resources, Options{ProjectRoot: root, Root: filepath.Join(root, "private")})
	if _, e := svc.Build(context.Background(), frozen); e != nil {
		t.Fatal(e)
	}
	svc.source = nil
	svc.resolver = nil
	docs, e := svc.Documents(context.Background(), frozen.ID)
	if e != nil || len(docs) != 1 {
		t.Fatalf("%v", e)
	}
	if !bytes.Equal(docs[0].HTML, body) || string(docs[0].Resources["https://source.test/image.png"].Data) != "old-image" {
		t.Fatal("snapshot changed")
	}
	if e = os.WriteFile(filepath.Join(root, "private", "exports", frozen.ID.Hex(), "source", bytesHash([]byte("old-image"))+".bin"), []byte("tampered"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = svc.Documents(context.Background(), frozen.ID); e == nil {
		t.Fatal("private source corruption was not detected")
	}
}
