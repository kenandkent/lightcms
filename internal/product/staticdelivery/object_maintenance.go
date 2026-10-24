package staticdelivery

import (
	"bytes"
	"context"
	"github.com/metavert-io/lightcms/v7/internal/product/artifacts"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"io"
	"time"
)

func (s *ObjectReleaseStore) EnsureIndexes(ctx context.Context) error {
	return artifacts.NewCatalog(s.db, nil, s.config.RetentionDays, s.config.FailedRetentionDays).EnsureIndexes(ctx)
}

func (s *ObjectReleaseStore) PinObject(ctx context.Context, hash string, pinned bool) error {
	root, store, catalog, err := s.open()
	if err != nil {
		return err
	}
	defer root.Close()
	defer store.Close()
	return catalog.Pin(ctx, hash, pinned)
}

func (s *ObjectReleaseStore) SaveExportObject(ctx context.Context, id primitive.ObjectID, data []byte, mime string) (artifacts.Ref, error) {
	root, store, catalog, err := s.open()
	if err != nil {
		return artifacts.Ref{}, err
	}
	defer root.Close()
	defer store.Close()
	ref, err := store.Put(ctx, bytes.NewReader(data), mime, 25<<20)
	if err != nil {
		return ref, err
	}
	if err := catalog.Register(ctx, ref, false); err != nil {
		return ref, err
	}
	expires := time.Now().Add(90 * 24 * time.Hour)
	err = catalog.Reference(ctx, "export-history:"+id.Hex(), ref, &expires)
	return ref, err
}
func (s *ObjectReleaseStore) OpenExportObject(ctx context.Context, ref artifacts.Ref) (io.ReadCloser, error) {
	root, store, _, err := s.open()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	defer store.Close()
	return store.Open(ctx, ref)
}

func (s *ObjectReleaseStore) PreviewCleanup(ctx context.Context, owner string) (artifacts.CleanupPlan, error) {
	root, store, catalog, err := s.open()
	if err != nil {
		return artifacts.CleanupPlan{}, err
	}
	defer root.Close()
	defer store.Close()
	return catalog.PreviewCleanup(ctx, owner)
}
func (s *ObjectReleaseStore) ConfirmCleanup(ctx context.Context, owner, digest string, confirmed bool) error {
	root, store, catalog, err := s.open()
	if err != nil {
		return err
	}
	defer root.Close()
	defer store.Close()
	return catalog.ConfirmCleanup(ctx, owner, digest, confirmed)
}
func (s *ObjectReleaseStore) ResumeCleanup(ctx context.Context, owner, digest string, confirmed bool) error {
	root, store, catalog, err := s.open()
	if err != nil {
		return err
	}
	defer root.Close()
	defer store.Close()
	return catalog.ResumeCleanup(ctx, owner, digest, confirmed)
}
