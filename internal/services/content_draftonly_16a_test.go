package services

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jonradoff/lightcms/v7/internal/models"
)

// Task 16A red-green: published PUT is draft-only — canonical HTML unchanged,
// version increments, has_unpublished_changes/requires_publish set.
func Test16A_PublishedPutLeavesCanonicalUnchanged(t *testing.T) {
	svc, cleanup := newTestContentService(t)
	defer cleanup()

	tmpDir := os.TempDir() + "/lightcms-test-16a-draftonly"
	os.MkdirAll(tmpDir+"/content/generated", 0755)
	origDir, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer func() {
		os.Chdir(origDir)
		os.RemoveAll(tmpDir)
	}()

	ctx := context.Background()
	tmplID := createTestTemplate(t, svc)

	content := &models.Content{
		TemplateID: tmplID, TemplateName: "Test Template", Title: "Live Page", Slug: "live-page",
		Data: map[string]interface{}{"content": "<p>v1 live</p>"},
	}
	if err := svc.CreateContent(ctx, content, "16A create"); err != nil {
		t.Fatalf("CreateContent: %v", err)
	}
	if content.CurrentVersion != 1 {
		t.Fatalf("expected CurrentVersion=1, got %d", content.CurrentVersion)
	}
	// Simulate a live publication's canonical file (saga owns this in prod;
	// here we stage the file explicitly to prove Update never rewrites it).
	if err := svc.GenerateStaticPage(ctx, content); err != nil {
		t.Fatalf("GenerateStaticPage: %v", err)
	}
	// Mark live via legacy publish path (draft-only: sets projection, no file write).
	if err := svc.PublishContent(ctx, content.ID); err != nil {
		t.Fatalf("PublishContent: %v", err)
	}
	before, err := os.ReadFile(tmpDir + "/content/generated/live-page.html")
	if err != nil {
		t.Fatalf("canonical missing after publish setup: %v", err)
	}

	got, _ := svc.GetContent(ctx, content.ID)
	got.Title = "Live Page v2"
	got.Data = map[string]interface{}{"content": "<p>v2 draft</p>"}
	if err := svc.UpdateContent(ctx, got, "16A edit published"); err != nil {
		t.Fatalf("UpdateContent: %v", err)
	}

	after, err := os.ReadFile(tmpDir + "/content/generated/live-page.html")
	if err != nil {
		t.Fatalf("canonical missing after draft edit: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("canonical HTML changed on draft edit")
	}
	if !strings.Contains(string(after), "v1 live") {
		t.Fatalf("canonical should still carry v1 bytes")
	}

	updated, _ := svc.GetContent(ctx, content.ID)
	if updated.CurrentVersion <= content.CurrentVersion && updated.CurrentVersion < 2 {
		t.Fatalf("expected CurrentVersion to advance, got %d", updated.CurrentVersion)
	}
	if !updated.HasUnpublishedChanges {
		t.Fatalf("expected has_unpublished_changes=true after editing a live page")
	}
	refreshRequiresPublish(updated)
	if !updated.RequiresPublish {
		t.Fatalf("expected requires_publish=true after editing a live page")
	}
	// Versions: create v1 + publish v2 + edit v3 (publish goes via UpdateContent legacy).
	vers, _ := svc.GetVersions(ctx, content.ID)
	if len(vers) < 3 {
		t.Fatalf("expected >=3 versions (create/publish/edit), got %d", len(vers))
	}
}

// Task 16A: CreateContent with Published=true must NOT write a live file.
func Test16A_CreatePublishedWritesNoFile(t *testing.T) {
	svc, cleanup := newTestContentService(t)
	defer cleanup()

	tmpDir := os.TempDir() + "/lightcms-test-16a-nofile"
	os.MkdirAll(tmpDir+"/content/generated", 0755)
	origDir, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer func() {
		os.Chdir(origDir)
		os.RemoveAll(tmpDir)
	}()

	ctx := context.Background()
	tmplID := createTestTemplate(t, svc)
	content := &models.Content{
		TemplateID: tmplID, TemplateName: "Test Template", Title: "No File", Slug: "no-file",
		Published: true, Data: map[string]interface{}{"content": "x"},
	}
	if err := svc.CreateContent(ctx, content); err != nil {
		t.Fatalf("CreateContent: %v", err)
	}
	if _, err := os.Stat(tmpDir + "/content/generated/no-file.html"); err == nil {
		t.Fatalf("draft-only CreateContent must not write a canonical file")
	}
	if content.CanonicalFullPath == "" || content.PathScope != "live" || !content.PathActive {
		t.Fatalf("canonical bookkeeping missing: %+v", content)
	}
}
