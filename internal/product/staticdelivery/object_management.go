package staticdelivery

import (
	"context"

	"github.com/metavert-io/lightcms/v7/internal/product/artifacts"
)

// Metadata management does not open/create the private filesystem root. It is
// available even when reading object bytes would fail, and has no remote calls.
func (s *ObjectReleaseStore) ListObjects(ctx context.Context, query artifacts.ListQuery) (artifacts.ObjectPage, error) {
	return artifacts.NewCatalog(s.db, nil, s.config.RetentionDays, s.config.FailedRetentionDays).ListObjects(ctx, query)
}

func (s *ObjectReleaseStore) GetObject(ctx context.Context, hash string) (artifacts.ObjectDetail, error) {
	return artifacts.NewCatalog(s.db, nil, s.config.RetentionDays, s.config.FailedRetentionDays).GetObject(ctx, hash)
}
