package services

// Lane 2B Fix 2 (red, import alignment): the import update branch drops the
// auto-publish request on the floor — re-imported pages stay drafts even when
// autoPublish is on, while the create branch publishes. Both must converge.

import (
	"context"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/services/importer"
	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func waitPublished(t *testing.T, ctx context.Context, svc *ContentService, id primitive.ObjectID) models.Content {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var c models.Content
		if err := svc.DB().FindOne(ctx, "content", bson.M{"_id": id}, &c); err == nil && c.Published {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for import auto-publish of %s", id.Hex())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestImportMarkdown_UpdateBranchAutoPublishes(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	cs := NewContentService(db)
	is := NewImportService(db, cs)
	tmplID := createTestTemplate(t, cs)

	seed := &models.Content{
		TemplateID: tmplID, TemplateName: "Test Template",
		Title: "Hello", Slug: "hello", FolderPath: "/imports",
		Data: map[string]interface{}{"title": "Hello", "body": "old"},
	}
	if err := cs.CreateContent(ctx, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	pages := []importer.MarkdownPage{{
		Filename:    "hello.md",
		Frontmatter: map[string]string{"title": "Hello"},
		Body:        "updated body",
	}}
	if _, err := is.RunMarkdownImport(ctx, pages, "Imported", "/imports", true, "test"); err != nil {
		t.Fatalf("RunMarkdownImport: %v", err)
	}
	got := waitPublished(t, ctx, cs, seed.ID)
	if got.Data["body"] != "updated body" {
		t.Fatalf("update branch must apply new data, got %v", got.Data)
	}
}

func TestImportCSV_UpdateBranchAutoPublishes(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	cs := NewContentService(db)
	is := NewImportService(db, cs)
	tmplID := createTestTemplate(t, cs)

	seed := &models.Content{
		TemplateID: tmplID, TemplateName: "Test Template",
		Title: "Row One", Slug: "row-one", FolderPath: "/imports",
		Data: map[string]interface{}{"title": "Row One"},
	}
	if err := cs.CreateContent(ctx, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	records := []importer.CSVRecord{{Fields: map[string]string{"title": "Row One", "body": "new"}, Row: 2}}
	if _, err := is.RunCSVImport(ctx, records, map[string]string{"body": "body"}, "title", "", tmplID, "/imports", true, "test"); err != nil {
		t.Fatalf("RunCSVImport: %v", err)
	}
	waitPublished(t, ctx, cs, seed.ID)
}
