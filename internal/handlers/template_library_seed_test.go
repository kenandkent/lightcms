package handlers

import (
	"context"
	"testing"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// A curated library must stay curated after restarting; clearing system
// templates must not recreate them or legacy pages referencing them.
func TestSeedDefaultsHonorsCustomOnlyLibrary(t *testing.T) {
	for _, test := range []struct {
		name       string
		customOnly bool
		templates  int64
		content    int64
	}{
		{"custom_only_survives_restart", true, 1, 0},
		{"explicit_false_preserves_default_install", false, 8, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, cleanup := newTestHandler(t)
			defer cleanup()
			ctx := context.Background()
			_, err := h.db.InsertOne(ctx, "templates", models.Template{ID: primitive.NewObjectID(), Slug: "editorial-news", Name: "新闻媒体报道模板", HTMLLayout: "<h1>{{.headline}}</h1>"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = h.db.InsertOne(ctx, "settings", bson.M{"type": "template_library_policy", "custom_only": test.customOnly})
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if err := h.SeedDefaults(ctx); err != nil {
					t.Fatal(err)
				}
			}
			count, err := h.db.Count(ctx, "templates", bson.M{})
			if err != nil {
				t.Fatal(err)
			}
			if count != test.templates {
				t.Fatalf("template count=%d, want=%d", count, test.templates)
			}
			count, err = h.db.Count(ctx, "content", bson.M{})
			if err != nil {
				t.Fatal(err)
			}
			if count != test.content {
				t.Fatalf("content count=%d, want=%d", count, test.content)
			}
		})
	}
}
