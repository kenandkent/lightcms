package database

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

// Task 16E: Connect.createIndexes matches the post-migration index set —
// the legacy UNIQUE (full_path, fork_id) index is DROP-ONLY (Task 14 drops
// it after the canonical index is verified; a restart must NOT recreate
// it), and the canonical partial-unique index is ensured on every boot.
func Test16E_CreateIndexesPostMigrationSet(t *testing.T) {
	loadTestEnv(t)
	uri := lookupEnv("MONGODB_URI")
	if uri == "" {
		t.Skip("skipping: MONGODB_URI not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	wc := writeconcern.New(writeconcern.WMajority())
	db, err := Connect(ctx, uri, "lightcms-test-idx16e", options.Client().SetWriteConcern(wc))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() {
		_ = db.database.Drop(ctx)
		db.Disconnect(context.Background()) //nolint:errcheck
	}()

	names := map[string]bool{}
	cur, err := db.database.Collection("content").Indexes().List(ctx)
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	for cur.Next(ctx) {
		var spec bson.M
		if err := cur.Decode(&spec); err != nil {
			t.Fatalf("decode index: %v", err)
		}
		if name, ok := spec["name"].(string); ok {
			names[name] = true
		}
	}
	cur.Close(ctx) //nolint:errcheck

	if names["full_path_1_fork_id_1"] {
		t.Errorf("legacy (full_path, fork_id) index must NOT be recreated by Connect")
	}
	if !names["content_canonical_path_scope_unique"] {
		t.Errorf("canonical partial-unique index must be ensured by Connect")
	}

	// Replica-set topology is required for publication transactions.
	ok, err := db.IsReplicaSet(ctx)
	if err != nil {
		t.Fatalf("IsReplicaSet: %v", err)
	}
	if !ok {
		t.Errorf("test MongoDB must be a replica set (Task 0 fixture)")
	}

	// Legacy rows without canonical fields stay index-invisible: inserting
	// two of them must not conflict under the partial filter.
	for i := 0; i < 2; i++ {
		if _, err := db.database.Collection("content").InsertOne(ctx, bson.M{
			"full_path": "/legacy-no-canonical", "title": "legacy",
		}); err != nil {
			t.Fatalf("legacy-shaped insert %d: %v", i, err)
		}
	}
	var canonCount int64
	if canonCount, err = db.database.Collection("content").CountDocuments(ctx,
		bson.M{"full_path": "/legacy-no-canonical"}); err != nil || canonCount != 2 {
		t.Fatalf("expected 2 legacy rows, got %d (err=%v)", canonCount, err)
	}
}
