package services

// M13 regression: fork copies share the live full_path and must never enter
// the embedding index — the single-row path short-circuits on ForkID before
// any provider call (no provider configured here by design).

import (
	"context"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestEmbeddingForkShortCircuit(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()

	forkID := primitive.NewObjectID()
	cid := primitive.NewObjectID()
	if _, err := db.InsertOne(ctx, "content", bson.M{
		"_id": cid, "title": "Forked", "slug": "forked", "full_path": "/forked",
		"fork_id": forkID, "published": true, "current_version": int64(1),
		"data": bson.M{"headline": "forked text"},
		"created_at": time.Now(), "updated_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed fork copy: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Collection("content").DeleteMany(context.Background(), bson.M{})
	})

	svc := NewSearchService(db, "")
	embedded, err := svc.UpdateContentEmbeddingForVersion(ctx, cid, 1)
	if err != nil {
		t.Fatalf("fork short-circuit must not error (no provider needed): %v", err)
	}
	if embedded {
		t.Fatal("fork copy must never be embedded")
	}
}
