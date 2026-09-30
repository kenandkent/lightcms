package idempotency

import (
	"context"

	"github.com/jonradoff/lightcms/v7/internal/database"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Repository is the thin Mongo persistence layer for Operation records.
// All methods accept context.Context; callers inside a transaction pass the
// mongo.SessionContext as the context (per the plan Shared interface
// contract — no nested transactions are opened here). Indexes are owned by
// Task 2 (DB.EnsureProductIndexes); this repository defines none.
type Repository struct {
	db   *database.DB
	coll *mongo.Collection
}

// NewRepository binds the idempotency_records collection. The caller must
// have run EnsureProductIndexes (tests do this per-test because suite setup
// drops collections, which also drops indexes).
func NewRepository(db *database.DB) *Repository {
	return &Repository{db: db, coll: db.Collection(CollectionName)}
}

// DB exposes the underlying handle for transaction-scoped caller flows.
func (r *Repository) DB() *database.DB { return r.db }

// Insert persists a new operation. A duplicate (owner, method, path, key)
// surfaces as a duplicate-key error for the service to resolve via re-read.
func (r *Repository) Insert(ctx context.Context, op *Operation) error {
	_, err := r.coll.InsertOne(ctx, op)
	return err
}

// FindByKey loads the record for an operation identity tuple.
// Returns (nil, nil) when absent.
func (r *Repository) FindByKey(ctx context.Context, owner, method, path, key string) (*Operation, error) {
	var op Operation
	err := r.coll.FindOne(ctx, bson.M{
		"owner": owner, "method": method, "path": path, "key": key,
	}).Decode(&op)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &op, nil
}

// FindByID loads a record by operation ID. Returns (nil, nil) when absent.
func (r *Repository) FindByID(ctx context.Context, id primitive.ObjectID) (*Operation, error) {
	var op Operation
	err := r.coll.FindOne(ctx, bson.M{"_id": id}).Decode(&op)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &op, nil
}

// UpdateCAS runs a compare-and-swap update and reports matched documents.
// Callers map matched==0 to the specific lost-race error by re-reading.
func (r *Repository) UpdateCAS(ctx context.Context, filter, update bson.M, arrayFilters ...bson.M) (int64, error) {
	var opts []*options.UpdateOptions
	if len(arrayFilters) > 0 {
		filters := make([]interface{}, len(arrayFilters))
		for i, f := range arrayFilters {
			filters[i] = f
		}
		opts = append(opts, options.Update().SetArrayFilters(options.ArrayFilters{Filters: filters}))
	}
	res, err := r.coll.UpdateOne(ctx, filter, update, opts...)
	if err != nil {
		return 0, err
	}
	return res.MatchedCount, nil
}
