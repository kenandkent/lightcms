package database

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// isDupKey reports duplicate-key errors (code 11000) without depending on
// exact driver error wrapping.
func isDupKey(err error) bool {
	if err == nil {
		return false
	}
	if mongo.IsDuplicateKeyError(err) {
		return true
	}
	return strings.Contains(err.Error(), "duplicate key")
}

// cleanProductCollections removes all docs from product collections so tests
// start from a clean slate on the shared test DB. It uses DeleteMany (not
// Drop) to preserve indexes created by Connect/EnsureProductIndexes across
// test-binary runs; otherwise a recreated legacy index with mismatched
// options would break the next run's Connect.
func cleanProductCollections(t *testing.T, db *DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, c := range []string{
		"content", "content_versions", "templates", "template_versions",
		"content_publications", "idempotency_records", "webhook_outbox",
	} {
		if _, err := db.Collection(c).DeleteMany(ctx, bson.M{}); err != nil {
			t.Fatalf("clean %s: %v", c, err)
		}
	}
}

// TestProductCanonicalPathIndex verifies EnsureProductIndexes creates the
// UNIQUE(canonical_full_path, path_scope) partial index and that concurrent
// casing-variant live inserts collide while fork scope shares the key.
func TestProductCanonicalPathIndex(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	cleanProductCollections(t, db)

	if err := db.EnsureProductIndexes(ctx); err != nil {
		t.Fatalf("EnsureProductIndexes: %v", err)
	}
	// Idempotent: second call must succeed.
	if err := db.EnsureProductIndexes(ctx); err != nil {
		t.Fatalf("EnsureProductIndexes (second call): %v", err)
	}

	// Legacy UNIQUE(full_path, fork_id) must still exist (Task 14 drops it
	// only after the new canonical index is verified).
	foundLegacy := false
	cur, err := db.Collection("content").Indexes().List(ctx)
	if err != nil {
		t.Fatalf("ListIndexes content: %v", err)
	}
	for cur.Next(ctx) {
		var spec bson.M
		if err := cur.Decode(&spec); err != nil {
			continue
		}
		if name, _ := spec["name"].(string); name == "full_path_1_fork_id_1" {
			foundLegacy = true
		}
		// Also confirm the new canonical index exists.
		if name, _ := spec["name"].(string); name == "content_canonical_path_scope_unique" {
			// ok
		}
	}
	_ = cur.Close(ctx)
	if !foundLegacy {
		t.Error("expected legacy index full_path_1_fork_id_1 to still exist (Task 14 owns its removal)")
	}

	// Concurrent live inserts with casing differences: same canonical key,
	// same scope "live" → exactly one must fail with duplicate key.
	coll := db.Collection("content")
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			full := "/News/Foo"
			if i == 1 {
				full = "/news/foo"
			}
			_, errs[i] = coll.InsertOne(ctx, bson.M{
				"full_path":           full,
				"canonical_full_path": "/news/foo",
				"path_scope":          "live",
				"path_active":         true,
				"title":               "Foo",
				"slug":                "foo",
				"created_at":          time.Now(),
				"updated_at":          time.Now(),
			})
		}(i)
	}
	wg.Wait()
	dups := 0
	for _, e := range errs {
		if isDupKey(e) {
			dups++
		} else if e != nil {
			t.Fatalf("unexpected insert error: %v", e)
		}
	}
	if dups != 1 {
		t.Errorf("expected exactly 1 duplicate-key error for casing-variant live inserts, got %d (errs=%v)", dups, errs)
	}

	// Fork scope may share the live canonical key.
	forkID := primitive.NewObjectID().Hex()
	if _, err := coll.InsertOne(ctx, bson.M{
		"full_path":           "/News/Foo",
		"canonical_full_path": "/news/foo",
		"path_scope":          forkID,
		"path_active":         true,
		"fork_id":             forkID,
		"title":               "Foo fork",
		"slug":                "foo",
		"created_at":          time.Now(),
		"updated_at":          time.Now(),
	}); err != nil {
		t.Errorf("fork scope sharing live key should succeed: %v", err)
	}

	// Inactive rows do not participate in the partial unique index. Use a
	// case-variant of the winning live FullPath so the legacy case-sensitive
	// UNIQUE(full_path, fork_id) index (still present until Task 14) does not
	// mask the canonical-index assertion: same canonical key, different
	// legacy key, inactive → must succeed.
	var winnerFull string
	{
		var doc bson.M
		if err := coll.FindOne(ctx, bson.M{"canonical_full_path": "/news/foo", "path_scope": "live", "path_active": true}).Decode(&doc); err == nil {
			winnerFull, _ = doc["full_path"].(string)
		}
	}
	inactiveFull := strings.ToUpper(winnerFull)
	if inactiveFull == "" || inactiveFull == winnerFull {
		inactiveFull = "/NEWS/FOO"
	}
	if _, err := coll.InsertOne(ctx, bson.M{
		"full_path":           inactiveFull,
		"canonical_full_path": "/news/foo",
		"path_scope":          "live",
		"path_active":         false,
		"title":               "Foo inactive",
		"slug":                "foo",
		"created_at":          time.Now(),
		"updated_at":          time.Now(),
	}); err != nil {
		t.Errorf("inactive row should bypass partial unique index: %v", err)
	}
}

// TestProductContentVersionUnique verifies UNIQUE(content_id, version).
func TestProductContentVersionUnique(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	cleanProductCollections(t, db)
	if err := db.EnsureProductIndexes(ctx); err != nil {
		t.Fatalf("EnsureProductIndexes: %v", err)
	}

	cid := primitive.NewObjectID()
	doc := bson.M{"content_id": cid, "version": int64(1), "title": "v1"}
	if _, err := db.Collection("content_versions").InsertOne(ctx, doc); err != nil {
		t.Fatalf("first version insert: %v", err)
	}
	if _, err := db.Collection("content_versions").InsertOne(ctx, doc); !isDupKey(err) {
		t.Errorf("expected duplicate-key for (content_id, version), got %v", err)
	}
	// Different version succeeds.
	if _, err := db.Collection("content_versions").InsertOne(ctx, bson.M{"content_id": cid, "version": int64(2)}); err != nil {
		t.Errorf("different version should succeed: %v", err)
	}
}

// TestWithTransaction_SuccessAndRollback exercises commit and abort paths
// against the test replica set (transactions require replica-set mode).
func TestWithTransaction_SuccessAndRollback(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	cleanProductCollections(t, db)

	coll := db.Collection("content")

	// Success: insert inside txn is visible after commit.
	marker := primitive.NewObjectID().Hex()
	if err := db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		_, err := coll.InsertOne(sc, bson.M{"full_path": "/txn-ok-" + marker, "title": "txn ok"})
		return err
	}); err != nil {
		t.Fatalf("WithTransaction commit: %v", err)
	}
	if n, err := coll.CountDocuments(ctx, bson.M{"full_path": "/txn-ok-" + marker}); err != nil || n != 1 {
		t.Errorf("expected committed doc visible (n=%d, err=%v)", n, err)
	}

	// Forced failure: returned error aborts, doc must not be visible.
	marker2 := primitive.NewObjectID().Hex()
	txnErr := context.DeadlineExceeded // sentinel-ish; any non-nil aborts
	_ = txnErr
	forced := func(sc mongo.SessionContext) error {
		if _, err := coll.InsertOne(sc, bson.M{"full_path": "/txn-fail-" + marker2}); err != nil {
			return err
		}
		return context.DeadlineExceeded
	}
	if err := db.WithTransaction(ctx, forced); err == nil {
		t.Fatal("expected forced transaction error")
	}
	if n, err := coll.CountDocuments(ctx, bson.M{"full_path": "/txn-fail-" + marker2}); err != nil || n != 0 {
		t.Errorf("expected rolled-back doc invisible (n=%d, err=%v)", n, err)
	}
}

// TestEnsureProductIndexes_TemplateSlugUnique verifies unique template slug.
func TestEnsureProductIndexes_TemplateSlugUnique(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	cleanProductCollections(t, db)
	if err := db.EnsureProductIndexes(ctx); err != nil {
		t.Fatalf("EnsureProductIndexes: %v", err)
	}
	if _, err := db.Collection("templates").InsertOne(ctx, bson.M{"name": "A", "slug": "financial-news"}); err != nil {
		t.Fatalf("first template insert: %v", err)
	}
	if _, err := db.Collection("templates").InsertOne(ctx, bson.M{"name": "B", "slug": "financial-news"}); !isDupKey(err) {
		t.Errorf("expected duplicate-key for template slug, got %v", err)
	}
}
