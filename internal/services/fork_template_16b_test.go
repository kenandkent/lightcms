package services

import (
	"context"
	"os"
	"testing"

	"github.com/jonradoff/lightcms/v7/internal/models"
)

// Task 16B red-green: template HTML edit creates a new TemplateVersion and
// leaves existing live canonical bytes unchanged (no auto-regen).
func Test16B_TemplateEditLeavesLiveUnchanged(t *testing.T) {
	tmplSvc, contentSvc, cleanup := newTestTemplateService(t)
	defer cleanup()

	tmpDir := os.TempDir() + "/lightcms-test-16b-tmpl"
	os.MkdirAll(tmpDir+"/content/generated", 0755)
	origDir, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer func() {
		os.Chdir(origDir)
		os.RemoveAll(tmpDir)
	}()

	ctx := context.Background()
	tmpl := &models.Template{
		Name: "Iso", Slug: "iso-tmpl",
		Fields:     []models.TemplateField{{Name: "body", Label: "Body", Type: "text"}},
		HTMLLayout: "<div>{{.body}}</div>",
	}
	if err := tmplSvc.CreateTemplate(ctx, tmpl); err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	if tmpl.CurrentVersion != 1 {
		t.Fatalf("expected CurrentVersion=1, got %d", tmpl.CurrentVersion)
	}

	content := &models.Content{
		TemplateID: tmpl.ID, TemplateName: tmpl.Name, Title: "Live", Slug: "live",
		Data: map[string]interface{}{"body": "v1"},
	}
	if err := contentSvc.CreateContent(ctx, content); err != nil {
		t.Fatalf("CreateContent: %v", err)
	}
	if err := contentSvc.GenerateStaticPage(ctx, content); err != nil {
		t.Fatalf("seed live file: %v", err)
	}
	before, _ := os.ReadFile(tmpDir + "/content/generated/live.html")

	tmpl.HTMLLayout = "<section>{{.body}}</section>"
	if err := tmplSvc.UpdateTemplate(ctx, tmpl); err != nil {
		t.Fatalf("UpdateTemplate: %v", err)
	}
	if tmpl.CurrentVersion != 2 {
		t.Fatalf("expected CurrentVersion=2 after HTML change, got %d", tmpl.CurrentVersion)
	}
	after, _ := os.ReadFile(tmpDir + "/content/generated/live.html")
	if string(before) != string(after) {
		t.Fatalf("live canonical changed on template edit")
	}
}

// Task 16B red-green: fork merge is draft-only — live bytes unchanged,
// RequiresPublish set, version created.
func Test16B_ForkMergeLeavesLiveUnchanged(t *testing.T) {
	svc, svcCleanup := newTestContentService(t)
	defer svcCleanup()
	cs := svc
	fs := NewForkService(cs.DB(), cs)
	ctx := context.Background()

	tmpDir := os.TempDir() + "/lightcms-test-16b-fork"
	os.MkdirAll(tmpDir+"/content/generated", 0755)
	origDir, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer func() {
		os.Chdir(origDir)
		os.RemoveAll(tmpDir)
	}()

	tmplID := createTestTemplate(t, cs)
	live := &models.Content{
		TemplateID: tmplID, TemplateName: "Test Template", Title: "Live", Slug: "mergelive",
		Published: true, Data: map[string]interface{}{"content": "v1 live"},
	}
	if err := cs.CreateContent(ctx, live); err != nil {
		t.Fatalf("CreateContent: %v", err)
	}
	if err := cs.GenerateStaticPage(ctx, live); err != nil {
		t.Fatalf("seed live: %v", err)
	}
	before, _ := os.ReadFile(tmpDir + "/content/generated/mergelive.html")

	uid := live.ID // any ObjectID for merged_by
	_ = uid
	fork, err := fs.Create(ctx, "f", "", live.ID, "e@x.com")
	if err != nil {
		t.Fatalf("Create fork: %v", err)
	}
	copyPage, err := fs.ForkPage(ctx, fork.ID, live.ID)
	if err != nil {
		t.Fatalf("ForkPage: %v", err)
	}
	_ = copyPage
	// Edit fork copy through the draft-only service (no live file write).
	fp, err := fs.GetForkPageByPath(ctx, fork.ID, "/mergelive")
	if err != nil {
		t.Fatalf("GetForkPage: %v", err)
	}
	fp.Title = "Fork Edit"
	if err := cs.UpdateContent(ctx, fp, "fork edit"); err != nil {
		t.Fatalf("edit fork: %v", err)
	}

	res, err := fs.Merge(ctx, fork.ID, live.ID, "m@x.com")
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(res.RequiresPublish) == 0 {
		t.Fatalf("expected requires_publish entries after fork merge")
	}
	after, _ := os.ReadFile(tmpDir + "/content/generated/mergelive.html")
	if string(before) != string(after) {
		t.Fatalf("live canonical changed on fork merge")
	}
	updated, _ := cs.GetContent(ctx, live.ID)
	if !updated.HasUnpublishedChanges {
		t.Fatalf("expected has_unpublished_changes after fork merge")
	}
}
