package dependencies

import (
	"context"
	"github.com/metavert-io/lightcms/v7/internal/testutil"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"testing"
	"time"
)

func TestIndexNestedCycleAndImmutableReadSummary(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	for _, name := range []string{"dependency_rows", "dependency_state"} {
		defer db.Collection(name).Drop(ctx)
	}
	for name, html := range map[string]string{"a": "[[include:b]]", "b": "[[include:c]]", "c": "[[include:d]]", "d": "[[include:e]]", "e": "[[include:a]]"} {
		if _, err := db.InsertOne(ctx, "snippets", bson.M{"name": name, "html": html}); err != nil {
			t.Fatal(err)
		}
	}
	tid, cid := primitive.NewObjectID(), primitive.NewObjectID()
	if _, err := db.InsertOne(ctx, "templates", bson.M{"_id": tid, "html_layout": "<head>[[include:a]]</head>"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertOne(ctx, "content", bson.M{"_id": cid, "title": "Article", "template_id": tid, "full_path": "/a", "published": true, "data": bson.M{"body": "<lc:query snippet='card'>"}}); err != nil {
		t.Fatal(err)
	}
	index := New(db)
	if _, err := db.InsertOne(ctx, "assets", bson.M{"serve_path": "/images/chart.png"}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateOne(ctx, "snippets", bson.M{"name": "e"}, bson.M{"$set": bson.M{"html": "[[include:a]]<img src='/images/chart.png?version=1'>"}}); err != nil {
		t.Fatal(err)
	}
	pid := primitive.NewObjectID()
	if _, err := db.InsertOne(ctx, "content_publications", bson.M{"_id": pid, "content_id": cid, "template_id": tid, "content_version": 1, "render_dependencies_hash": "frozen-hash", "dependency_snapshot": bson.M{"snippets_hash": "original-snippets"}}); err != nil {
		t.Fatal(err)
	}
	if err := index.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	result, err := index.Query(ctx, primitive.NilObjectID, "e")
	if err != nil || result.Total != 1 || result.Published != 1 || len(result.Items) != 1 || result.IndexedAt.IsZero() {
		t.Fatalf("transitive dependencies missing: %+v %v", result, err)
	}
	result, err = index.Query(ctx, primitive.NilObjectID, "card")
	if err != nil || result.Total != 1 {
		t.Fatalf("inline query dependency missing: %+v %v", result, err)
	}
	// Indexed metadata is an advisory snapshot, not the current source or a
	asset, err := index.QueryAsset(ctx, "/images/chart.png")
	if err != nil || asset.Total != 1 {
		t.Fatalf("transitive asset not indexed: %+v %v", asset, err)
	}
	frozen, err := index.Frozen(ctx, cid)
	if err != nil || len(frozen) != 1 || frozen[0]["render_dependencies_hash"] != "frozen-hash" {
		t.Fatalf("frozen dependency provenance missing: %+v %v", frozen, err)
	}
	// Indexed metadata is an advisory snapshot, not the current source or a
	// publication authority. Content mutation cannot leak a body into the view.
	if _, err := db.Collection("content").UpdateOne(ctx, bson.M{"_id": cid}, bson.M{"$set": bson.M{"title": "Changed", "updated_at": time.Now()}}); err != nil {
		t.Fatal(err)
	}
	result, err = index.Query(ctx, tid, "")
	if err != nil || result.Items[0].Title != "Article" {
		t.Fatalf("query bypassed snapshot: %+v %v", result, err)
	}
	if err := index.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	result, err = index.Query(ctx, tid, "")
	if err != nil || result.Items[0].Title != "Changed" {
		t.Fatalf("rebuild failed: %+v %v", result, err)
	}
	frozen, err = index.Frozen(ctx, cid)
	if err != nil || frozen[0]["render_dependencies_hash"] != "frozen-hash" {
		t.Fatal("mutable source overwrote publication dependency", err)
	}
}
