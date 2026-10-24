package handlers

import (
	"context"
	"github.com/metavert-io/lightcms/v7/internal/models"
	"github.com/metavert-io/lightcms/v7/internal/testutil"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"sort"
	"testing"
	"time"
)

func TestContentVersionsSeparateDraftPublicationAndCurrentWorker(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	for _, name := range []string{"static_delivery_page_status", "static_delivery_targets", "content_publications"} {
		defer db.Collection(name).Drop(ctx)
	}
	cid, pid, target, release := primitive.NewObjectID(), primitive.NewObjectID(), primitive.NewObjectID(), primitive.NewObjectID()
	if _, err := db.InsertOne(ctx, "content_publications", bson.M{"_id": pid, "content_id": cid, "content_version": 2, "status": "active"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertOne(ctx, "static_delivery_targets", bson.M{"_id": target, "current_release_id": release}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertOne(ctx, "static_delivery_page_status", bson.M{"content_id": cid, "publication_id": pid, "target_id": target, "release_id": release, "url": "https://frozen.example/a", "target_name": "Reports"}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db}
	rows, err := h.contentVersionStates(ctx, []models.Content{{ID: cid, CurrentVersion: 3}})
	if err != nil {
		t.Fatal(err)
	}
	state := rows[cid.Hex()]
	if state.Draft != 3 || state.Published != 2 || !state.Changed || len(state.Workers) != 1 || state.Workers[0].URL != "https://frozen.example/a" {
		t.Fatalf("versions conflated: %+v", state)
	}
	if _, err := db.Collection("static_delivery_targets").UpdateOne(ctx, bson.M{"_id": target}, bson.M{"$set": bson.M{"current_release_id": primitive.NewObjectID()}}); err != nil {
		t.Fatal(err)
	}
	rows, err = h.contentVersionStates(ctx, []models.Content{{ID: cid, CurrentVersion: 3}})
	if err != nil || len(rows[cid.Hex()].Workers) != 0 {
		t.Fatalf("stale worker index presented as current: %+v %v", rows, err)
	}
}

func TestVersionReadModelLocalLatencySample(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	content := make([]models.Content, 50)
	for n := range content {
		content[n] = models.Content{ID: primitive.NewObjectID(), CurrentVersion: 3}
		if _, err := db.InsertOne(ctx, "content_publications", bson.M{"content_id": content[n].ID, "content_version": 2, "status": "active"}); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handler{db: db}
	samples := make([]time.Duration, 25)
	for n := range samples {
		started := time.Now()
		rows, err := h.contentVersionStates(ctx, content)
		if err != nil || len(rows) != 50 {
			t.Fatalf("bounded read failed: %d %v", len(rows), err)
		}
		samples[n] = time.Since(started)
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	t.Logf("local Mongo, 50 articles, 25 requests: p95=%s, max=%s (measurement, not a production SLO)", samples[23], samples[24])
}
