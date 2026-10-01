package handlers

// Lane 1B fix 4: GenerateSitemap must exclude soft-deleted rows and fork
// copies (which share full_path with live pages), matching the live
// ServePage/llms filters.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestW1BSitemap_ExcludesDeletedAndFork(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	os.MkdirAll("static", 0755)
	defer os.Remove("static/sitemap.xml")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	now := time.Now()
	live := bson.M{
		"template_name": "Test", "title": "W1B Live", "slug": "w1b-live-ok",
		"full_path": "/w1b-live-ok", "published": true, "deleted": false,
		"created_at": now, "updated_at": now,
	}
	deleted := bson.M{
		"template_name": "Test", "title": "W1B Deleted", "slug": "w1b-deleted",
		"full_path": "/w1b-deleted", "published": true, "deleted": true,
		"created_at": now, "updated_at": now,
	}
	fork := bson.M{
		"template_name": "Test", "title": "W1B Fork", "slug": "w1b-forkpage",
		"full_path": "/w1b-forkpage", "published": true, "deleted": false,
		"fork_id":   primitive.NewObjectID(),
		"created_at": now, "updated_at": now,
	}
	for name, doc := range map[string]bson.M{"live": live, "deleted": deleted, "fork": fork} {
		if _, err := h.db.InsertOne(ctx, "content", doc); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	if err := h.GenerateSitemap(ctx, "https://example.com"); err != nil {
		t.Fatalf("GenerateSitemap: %v", err)
	}
	data, err := os.ReadFile("static/sitemap.xml")
	if err != nil {
		t.Fatalf("sitemap.xml not created: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "/w1b-live-ok") {
		t.Errorf("sitemap missing live page /w1b-live-ok\n%s", body)
	}
	for _, absent := range []string{"/w1b-deleted", "/w1b-forkpage"} {
		if strings.Contains(body, absent) {
			t.Errorf("sitemap leaks excluded path %s", absent)
		}
	}
}
