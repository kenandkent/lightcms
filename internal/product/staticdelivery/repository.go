package staticdelivery

import (
	"context"
	"errors"
	"time"

	"github.com/metavert-io/lightcms/v7/internal/database"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

type releaseReferences interface {
	ReferenceRelease(context.Context, string, primitive.ObjectID, *time.Time) error
	ReleaseOwner(context.Context, string) error
}
type Repository struct {
	summaries interface {
		Summary(context.Context, primitive.ObjectID) (ReleaseManifest, error)
	}
	db         *database.DB
	references releaseReferences
}

func NewRepository(db *database.DB) *Repository { return &Repository{db: db} }
func (r *Repository) collection(name string) *mongo.Collection {
	return r.db.Collection(collectionPrefix + name)
}

func (r *Repository) ClaimTarget(ctx context.Context, targetID, jobID primitive.ObjectID, revision int64) error {
	if targetID.IsZero() || jobID.IsZero() || revision < 0 {
		return failure(CodeInvalidRequest, "invalid target claim", nil)
	}
	res, err := r.collection("targets").UpdateOne(ctx, bson.M{"_id": targetID, "status": "ready", "current_revision": revision, "$or": bson.A{
		bson.M{"active_job_id": nil}, bson.M{"active_job_id": primitive.NilObjectID}, bson.M{"active_job_id": jobID},
	}}, bson.M{"$set": bson.M{"active_job_id": jobID, "updated_at": time.Now().UTC()}})
	if err != nil {
		return err
	}
	if res.MatchedCount != 1 {
		return failure(CodeTargetBusy, "target is busy, unavailable or its revision changed", nil)
	}
	return nil
}

// CommitTarget is the commit authority. Recording the successful job and
// release can be repaired later; no transient write may clear the owner early.
func (r *Repository) CommitTarget(ctx context.Context, targetID, jobID, releaseID primitive.ObjectID, revision int64) error {
	if targetID.IsZero() || jobID.IsZero() || releaseID.IsZero() || revision < 0 {
		return failure(CodeInvalidRequest, "invalid release commit", nil)
	}
	res, err := r.collection("targets").UpdateOne(ctx, bson.M{"_id": targetID, "active_job_id": jobID, "current_revision": revision}, bson.M{
		"$set": bson.M{"current_release_id": releaseID, "active_job_id": primitive.NilObjectID, "updated_at": time.Now().UTC()},
		"$inc": bson.M{"current_revision": int64(1)},
	})
	if err != nil {
		return err
	}
	if res.MatchedCount == 1 {
		return nil
	}
	// An already completed commit is replayable only for this exact release.
	err = r.collection("targets").FindOne(ctx, bson.M{"_id": targetID, "current_revision": revision + 1, "current_release_id": releaseID}).Err()
	if err == nil {
		return nil
	}
	if !errors.Is(err, mongo.ErrNoDocuments) {
		return err
	}
	return failure(CodeRevisionConflict, "target owner or revision changed", nil)
}

func (r *Repository) GetTarget(ctx context.Context, id primitive.ObjectID) (Target, error) {
	var target Target
	err := r.collection("targets").FindOne(ctx, bson.M{"_id": id}).Decode(&target)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return target, failure(CodeNotFound, "target not found", nil)
	}
	return target, err
}
