package services

// Lane 2B Fix 2 (red): bulk-create with Published:true must not persist a
// live flag without control-plane truth (no Publication row, no outbox, no
// idempotency). The endpoint rejects it with a clear 4xx (handlers test);
// the service backstop fails the item with a directing error.

import (
	"context"
	"strings"
	"testing"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/testutil"
)

func TestBulkCreateContent_RejectsPublishedTrue(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	svc := NewContentService(db)
	tmplID := createTestTemplate(t, svc)

	items := []*models.Content{
		{TemplateID: tmplID, TemplateName: "Test Template", Title: "Draft", Slug: "bulk-draft-ok",
			Data: map[string]interface{}{"content": "d"}},
		{TemplateID: tmplID, TemplateName: "Test Template", Title: "Pub", Slug: "bulk-pub-no",
			Published: true, Data: map[string]interface{}{"content": "p"}},
	}
	results := svc.BulkCreateContent(ctx, items, "bulk published guard")
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if !results[0].Success {
		t.Fatalf("draft item must succeed: %+v", results[0])
	}
	if results[1].Success {
		t.Fatal("Published:true item must NOT succeed without a publication record")
	}
	if !strings.Contains(strings.ToLower(results[1].Error), "publish") {
		t.Fatalf("rejection must direct to publish endpoints, got: %q", results[1].Error)
	}

	// No divergent live flag may persist for the rejected item.
	var probe models.Content
	if err := db.FindOne(ctx, "content", map[string]interface{}{"slug": "bulk-pub-no"}, &probe); err == nil {
		t.Fatalf("rejected Published:true item must not persist, found %+v", probe.FullPath)
	}

	// And no publication/outbox truth may exist for it either (belt and braces).
	n, err := db.Count(ctx, "content_publications", map[string]interface{}{})
	if err != nil {
		t.Fatalf("count publications: %v", err)
	}
	if n != 0 {
		t.Fatalf("bulk-create must mint zero publications, found %d", n)
	}
}
