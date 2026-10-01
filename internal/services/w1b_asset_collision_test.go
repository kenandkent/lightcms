package services

// Lane 1B fix 5a: *.html asset uploads that collide with a live page
// canonical must be rejected (sentinel ErrAssetCanonicalCollision → HTTP
// 409); non-colliding *.html and non-html uploads keep working.

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jonradoff/lightcms/v7/internal/models"
)

func w1bHTMLData() []byte {
	return []byte("<html><head><title>w1b</title></head><body>w1b asset bytes</body></html>")
}

func TestW1BUploadAsset_RejectsCanonicalCollision(t *testing.T) {
	svc, cleanup := newTestContentService(t)
	defer cleanup()
	assets := NewAssetService(svc.db)
	ctx := context.Background()

	tmpDir := t.TempDir()
	origDir, _ := os.Getwd()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(origDir)

	tmplID := createTestTemplate(t, svc)
	live := &models.Content{
		TemplateID: tmplID, TemplateName: "Test Template",
		Title: "W1B Promo", Slug: "w1b-promo",
		Data: map[string]interface{}{"content": "<p>promo</p>"},
	}
	if err := svc.CreateContent(ctx, live); err != nil {
		t.Fatalf("CreateContent: %v", err)
	}

	_, err := assets.UploadAsset(ctx, w1bHTMLData(), "w1b-promo.html", "/assets/w1b-promo.html", "")
	if err == nil {
		t.Fatal("expected collision error for *.html asset matching live canonical, got nil")
	}
	if !errors.Is(err, ErrAssetCanonicalCollision) {
		t.Fatalf("expected ErrAssetCanonicalCollision, got %v", err)
	}
}

func TestW1BUploadAsset_NonCollidingHTMLAllowed(t *testing.T) {
	svc, cleanup := newTestContentService(t)
	defer cleanup()
	assets := NewAssetService(svc.db)
	ctx := context.Background()

	tmpDir := t.TempDir()
	origDir, _ := os.Getwd()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(origDir)

	tmplID := createTestTemplate(t, svc)
	live := &models.Content{
		TemplateID: tmplID, TemplateName: "Test Template",
		Title: "W1B Other", Slug: "w1b-other",
		Data: map[string]interface{}{"content": "<p>other</p>"},
	}
	if err := svc.CreateContent(ctx, live); err != nil {
		t.Fatalf("CreateContent: %v", err)
	}

	// Same *.html extension but a different path: must succeed.
	a, err := assets.UploadAsset(ctx, w1bHTMLData(), "w1b-standalone.html", "/assets/w1b-standalone.html", "")
	if err != nil {
		t.Fatalf("non-colliding *.html upload rejected: %v", err)
	}
	if a.ServePath != "/w1b-standalone.html" {
		t.Errorf("ServePath = %q, want stripped /w1b-standalone.html", a.ServePath)
	}

	// Non-html uploads are unaffected by the guard.
	png := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53, 0xde,
	}
	if _, err := assets.UploadAsset(ctx, png, "w1b-promo.png", "/assets/w1b-promo.png", ""); err != nil {
		t.Fatalf("non-html upload rejected: %v", err)
	}
}
