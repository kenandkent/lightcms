package staticdelivery

import (
	"context"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// No TTL index is used: deleting an active operation would allow a repeated
// request to allocate a second remote deployment. Retention checks references.
func (r *Repository) EnsureIndexes(ctx context.Context) error {
	definitions := []struct {
		collection, name string
		keys             bson.D
		unique           bool
	}{
		{"accounts", "account_unique", bson.D{{Key: "account_id", Value: 1}}, true},
		{"credentials", "account_revision_unique", bson.D{{Key: "account_ref", Value: 1}, {Key: "revision", Value: 1}}, true},
		{"targets", "account_worker_unique", bson.D{{Key: "cf_account_id", Value: 1}, {Key: "worker_name", Value: 1}}, true},
		{"targets", "state_owner", bson.D{{Key: "status", Value: 1}, {Key: "active_job_id", Value: 1}}, false},
		{"exports", "operation_unique", bson.D{{Key: "operation_id", Value: 1}}, true},
		{"exports", "state_created", bson.D{{Key: "state", Value: 1}, {Key: "created_at", Value: 1}}, false},
		{"previews", "owner_expiry", bson.D{{Key: "owner", Value: 1}, {Key: "expires_at", Value: 1}}, false},
		{"releases", "job_unique", bson.D{{Key: "job_id", Value: 1}}, true},
		{"releases", "target_created", bson.D{{Key: "target_id", Value: 1}, {Key: "created_at", Value: -1}}, false},
		{"jobs", "operation_unique", bson.D{{Key: "operation_id", Value: 1}}, true},
		{"jobs", "state_due", bson.D{{Key: "state", Value: 1}, {Key: "next_run_at", Value: 1}}, false},
		{"jobs", "target_created", bson.D{{Key: "target_id", Value: 1}, {Key: "created_at", Value: -1}}, false},
		{"operations", "idempotency_unique", bson.D{{Key: "owner", Value: 1}, {Key: "action", Value: 1}, {Key: "target_scope", Value: 1}, {Key: "idempotency_key_digest", Value: 1}}, true},
		{"operations", "state_expiry", bson.D{{Key: "state", Value: 1}, {Key: "expire_at", Value: 1}}, false},
	}
	for _, d := range definitions {
		_, err := r.collection(d.collection).Indexes().CreateOne(ctx, mongo.IndexModel{Keys: d.keys, Options: options.Index().SetName(d.name).SetUnique(d.unique)})
		if err != nil {
			return failure(CodeModuleUnavailable, "cannot initialize static delivery indexes", err)
		}
	}
	return nil
}
