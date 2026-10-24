package staticdelivery

import (
	"context"
	"github.com/metavert-io/lightcms/v7/internal/product/artifacts"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"os"
)

// Reserve parsing/copy overhead as well as both snapshots used by rollback or
// queue rebasing. Never load the bodies in order to decide admission.
func releaseReservation(ctx context.Context, store ReleaseStore, ids ...primitive.ObjectID) (int64, error) {
	total := int64(512 << 20)
	seen := map[primitive.ObjectID]bool{}
	for _, id := range ids {
		if id.IsZero() || seen[id] {
			continue
		}
		seen[id] = true
		size := int64(512 << 20)
		if sized, ok := store.(interface {
			LoadBytes(context.Context, primitive.ObjectID) (int64, error)
		}); ok {
			var err error
			size, err = sized.LoadBytes(ctx, id)
			if err != nil {
				return 0, err
			}
		}
		if size < 0 || size > 512<<20 {
			return 0, artifacts.ErrLimit
		}
		total += 3 * size
	}
	return total, nil
}
func (s *FileReleaseStore) LoadBytes(ctx context.Context, id primitive.ObjectID) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if id.IsZero() {
		return 0, failure(CodeInvalidRequest, "release ID required", nil)
	}
	root, err := s.root()
	if err != nil {
		return 0, err
	}
	defer root.Close()
	info, err := root.Stat("releases/" + id.Hex() + "/private/release.gob")
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || info.Size() > 512<<20 {
		return 0, artifacts.ErrLimit
	}
	return info.Size(), nil
}
func (s *ObjectReleaseStore) LoadBytes(ctx context.Context, id primitive.ObjectID) (int64, error) {
	root, objects, _, err := s.open()
	if err != nil {
		return 0, err
	}
	defer root.Close()
	defer objects.Close()
	if _, err := root.Lstat("object-releases/" + id.Hex()); os.IsNotExist(err) {
		return s.legacy.LoadBytes(ctx, id)
	} else if err != nil {
		return 0, err
	}
	pointer, _, err := s.readPointer(root, id)
	if err != nil {
		return 0, err
	}
	total := int64(0)
	seen := map[string]bool{}
	for _, ref := range pointer.Objects {
		if seen[ref.Hash] {
			continue
		}
		seen[ref.Hash] = true
		if ref.Size < 0 || ref.Size > 512<<20 {
			return 0, artifacts.ErrLimit
		}
		total += ref.Size
		if total > 512<<20 {
			return 0, artifacts.ErrLimit
		}
	}
	return total, nil
}
