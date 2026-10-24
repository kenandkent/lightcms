package staticexport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"

	"github.com/metavert-io/lightcms/v7/internal/product/publication"
	"github.com/metavert-io/lightcms/v7/internal/product/storage"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

type sourceRecords struct {
	pubs    map[primitive.ObjectID]publication.Publication
	active  primitive.ObjectID
	content ContentState
}

func (s *sourceRecords) GetActive(_ context.Context, _ primitive.ObjectID) (*publication.Publication, error) {
	p := s.pubs[s.active]
	return &p, nil
}
func (s *sourceRecords) GetByID(_ context.Context, id primitive.ObjectID) (*publication.Publication, error) {
	p, ok := s.pubs[id]
	if !ok {
		return nil, io.EOF
	}
	return &p, nil
}
func (s *sourceRecords) Content(_ context.Context, _ primitive.ObjectID) (ContentState, error) {
	return s.content, nil
}
func (s *sourceRecords) TemplateSlug(_ context.Context, _ primitive.ObjectID) (string, error) {
	return "crypto-analysis", nil
}

func TestSourceFrozenPublicationSurvivesNewPublish(t *testing.T) {
	ctx := context.Background()
	cid, pid, newID := primitive.NewObjectID(), primitive.NewObjectID(), primitive.NewObjectID()
	store := storage.NewFilesystemStore(t.TempDir())
	body := []byte("<!doctype html><html><head><title>Original</title></head><body>First</body></html>")
	sum := sha256.Sum256(body)
	obj, err := store.Stage(ctx, storage.StageRequest{ContentID: cid, PublicationID: pid, CanonicalPath: "/news/a", HTML: body})
	if err != nil {
		t.Fatal(err)
	}
	records := &sourceRecords{active: pid, pubs: map[primitive.ObjectID]publication.Publication{pid: {ID: pid, ContentID: cid, FullPath: "/news/a", PublicURL: "https://cms.example/news/a", ContentHash: "sha256:" + hex.EncodeToString(sum[:]), Status: publication.StatusActive, StorageState: publication.StoragePresent, VerificationStatus: publication.VerificationVerified, TemplateVersionID: primitive.NewObjectID()}}}
	source := NewPublicationSource(records, records, store)
	frozen, err := source.Resolve(ctx, Item{ContentID: cid})
	if err != nil {
		t.Fatal(err)
	}
	if frozen.SourceSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("storage digest must be raw SHA-256, got %q", frozen.SourceSHA256)
	}
	records.active = newID
	records.pubs[newID] = publication.Publication{ID: newID, ContentID: cid, ContentHash: strings.Repeat("f", 64)}
	frozen.StoragePath = "/outside-project/must-not-be-opened"
	r, err := source.Open(ctx, frozen)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil || string(got) != string(body) || frozen.PublicationID != pid {
		t.Fatalf("lost frozen snapshot: %s %v", got, err)
	}
	if err = store.Verify(ctx, obj); err != nil {
		t.Fatalf("export modified original: %v", err)
	}
}

func TestSourceRejectsUnservableVersionsAndWrongOwnership(t *testing.T) {
	ctx := context.Background()
	cid, pid := primitive.NewObjectID(), primitive.NewObjectID()
	base := publication.Publication{ID: pid, ContentID: cid, FullPath: "/news/a", ContentHash: strings.Repeat("a", 64), Status: publication.StatusActive, StorageState: publication.StoragePresent, VerificationStatus: publication.VerificationVerified, TemplateVersionID: primitive.NewObjectID()}
	for _, tc := range []struct {
		name   string
		mutate func(*publication.Publication, *ContentState)
	}{
		{"failed", func(p *publication.Publication, _ *ContentState) { p.Status = publication.StatusFailed }},
		{"staged", func(p *publication.Publication, _ *ContentState) { p.Status = publication.StatusStaged }},
		{"unpublished", func(p *publication.Publication, _ *ContentState) { p.Status = publication.StatusUnpublished }},
		{"legacy", func(p *publication.Publication, _ *ContentState) {
			p.VerificationStatus = publication.VerificationLegacyUnverified
		}},
		{"deleted-object", func(p *publication.Publication, _ *ContentState) { p.StorageState = publication.StorageDeleted }},
		{"other-content", func(p *publication.Publication, _ *ContentState) { p.ContentID = primitive.NewObjectID() }},
		{"deleted-content", func(_ *publication.Publication, c *ContentState) { c.Deleted = true }},
		{"fork", func(_ *publication.Publication, c *ContentState) { c.Fork = true }},
		{"malformed-record-hash", func(p *publication.Publication, _ *ContentState) { p.ContentHash = "sha256:" + strings.Repeat("g", 64) }},
		{"short-record-hash", func(p *publication.Publication, _ *ContentState) { p.ContentHash = "sha256:abc" }},
		{"wrong-hash-algorithm", func(p *publication.Publication, _ *ContentState) { p.ContentHash = "sha512:" + strings.Repeat("a", 64) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, c := base, ContentState{}
			tc.mutate(&p, &c)
			records := &sourceRecords{pubs: map[primitive.ObjectID]publication.Publication{pid: p}, active: pid, content: c}
			source := NewPublicationSource(records, records, storage.NewFilesystemStore(t.TempDir()))
			if _, err := source.Resolve(ctx, Item{ContentID: cid, PublicationID: pid}); err == nil {
				t.Fatal("unexportable publication accepted")
			}
		})
	}
}
