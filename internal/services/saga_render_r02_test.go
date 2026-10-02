package services

// R02 regression: production saga renders must go through the frozen
// snapshot pipeline (Markdown conversion, sanitizer policy, snippets,
// wikilinks, TOC) — never raw template.HTML. Matrix over script policy ×
// authorship through ContentService.SagaSnapshotRender.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"
	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func r02Input(admin bool) publication.RenderInput {
	return publication.RenderInput{
		Content: models.Content{
			ID:    primitive.NewObjectID(),
			Title: "R02", Slug: "r02", FullPath: "/r02", Data: map[string]any{
				"headline": `<img src=x onerror=alert(1)>`,
				"body":     `Hello **bold**`,
			},
		},
		Template: templatecontract.TemplateVersion{
			ID: primitive.NewObjectID(), TemplateID: primitive.NewObjectID(),
			Version: 1, Slug: "r02", Name: "R02", Status: "active",
			Fields: []models.TemplateField{
				{Name: "headline", Label: "Headline", Type: "text", Required: true},
				{Name: "body", Label: "Body", Type: "markdown"},
			},
			HTMLLayout: `<html><body><h1>{{.headline}}</h1><div>{{.body}}</div></body></html>`,
		},
		PublicationID:      primitive.NewObjectID(),
		ContentVersion:     1,
		TemplateVersionNum: 1,
		LogicalPublishedAt: time.Now().UTC().Truncate(time.Millisecond),
		PublicURL:          "http://localhost:8080/r02",
		AuthorIsAdmin:      admin,
	}
}

func TestSagaSnapshotRenderPolicyMatrix(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	svc := NewContentService(db)

	setPolicy := func(policy string) {
		t.Helper()
		_, _ = db.Collection("settings").DeleteMany(ctx, bson.M{"type": "site_config"})
		if policy == "" {
			return
		}
		if _, err := db.InsertOne(ctx, "settings", bson.M{
			"type": "site_config", "markdown_script_policy": policy,
			"title_template": "{{title}}", "max_upload_bytes": int64(1 << 20),
		}); err != nil {
			t.Fatalf("seed site_config: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = db.Collection("settings").DeleteMany(context.Background(), bson.M{"type": "site_config"})
	})

	// Policy "all" (default, no config doc): markdown converts, text passes
	// through like the legacy path (parity), hostile vectors in markdown
	// are sanitized by bluemonday.
	setPolicy("")
	res, err := svc.SagaSnapshotRender(ctx, r02Input(false))
	if err != nil {
		t.Fatalf("policy all render: %v", err)
	}
	if !strings.Contains(string(res.HTML), "<strong>bold</strong>") {
		t.Fatalf("markdown not converted under policy all:\n%s", res.HTML)
	}
	if res.RendererVersion == "" || res.RenderDependenciesHash == "" || res.DependencySnapshot == nil {
		t.Fatalf("render provenance missing: %+v", res)
	}

	// Policy "none": hostile text fails closed (rejected, never staged).
	setPolicy("none")
	if _, err := svc.SagaSnapshotRender(ctx, r02Input(false)); err == nil {
		t.Fatal("policy none + hostile text: expected RENDER_UNSAFE_CONTENT, got success")
	} else if !strings.Contains(err.Error(), "RENDER_UNSAFE_CONTENT") && !strings.Contains(err.Error(), "UNSAFE") {
		t.Fatalf("policy none + hostile text: wrong error: %v", err)
	}
	// ... but benign markdown still renders (sanitized, no error).
	benign := r02Input(false)
	benign.Content.Data = map[string]any{"headline": "plain", "body": `Hello **bold** <img src="pic.png">`}
	res, err = svc.SagaSnapshotRender(ctx, benign)
	if err != nil {
		t.Fatalf("policy none benign markdown: %v", err)
	}
	if !strings.Contains(string(res.HTML), "<strong>bold</strong>") {
		t.Fatalf("policy none: markdown lost:\n%s", res.HTML)
	}
	// Hostile vectors inside markdown also fail closed (pre-conversion
	// scan), even though the sanitizer could strip them.
	hostileMD := r02Input(false)
	hostileMD.Content.Data = map[string]any{"headline": "plain", "body": `click <a href="javascript:alert(1)">me</a>`}
	if _, err := svc.SagaSnapshotRender(ctx, hostileMD); err == nil {
		t.Fatal("policy none + javascript: URI: expected rejection, got success")
	}

	// Policy "admin_only": editors fail closed, admins render raw.
	setPolicy("admin_only")
	if _, err := svc.SagaSnapshotRender(ctx, r02Input(false)); err == nil {
		t.Fatal("admin_only + editor hostile text: expected rejection, got success")
	}
	res, err = svc.SagaSnapshotRender(ctx, r02Input(true))
	if err != nil {
		t.Fatalf("admin_only + admin: %v", err)
	}
	if !strings.Contains(string(res.HTML), "onerror") {
		t.Fatalf("admin_only + admin: raw passthrough lost:\n%s", res.HTML)
	}

	// Snippet includes + wikilinks resolve from frozen inputs.
	setPolicy("")
	if _, err := db.InsertOne(ctx, "snippets", bson.M{
		"name": "cta", "html": "<b>CTA</b>",
		"created_at": time.Now(), "updated_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed snippet: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Collection("snippets").DeleteMany(context.Background(), bson.M{})
	})
	if _, err := db.InsertOne(ctx, "content", bson.M{
		"title": "Other Page", "slug": "other", "full_path": "/other",
		"published": true, "current_version": int64(1),
		"created_at": time.Now(), "updated_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed wikilink target: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Collection("content").DeleteMany(context.Background(), bson.M{})
	})
	withRefs := r02Input(false)
	withRefs.Content.Data = map[string]any{
		"headline": "plain",
		"body":     `[[include:cta]] and [[Other Page]]`,
	}
	res, err = svc.SagaSnapshotRender(ctx, withRefs)
	if err != nil {
		t.Fatalf("snippet/wikilink render: %v", err)
	}
	// NOTE: snippet expansion happens on the markdown source before
	// conversion; the frozen wikilink pass runs on rendered HTML.
	if !strings.Contains(string(res.HTML), "CTA") {
		t.Fatalf("snippet include not expanded:\n%s", res.HTML)
	}
	if !strings.Contains(string(res.HTML), `href="/other"`) {
		t.Fatalf("wikilink not resolved:\n%s", res.HTML)
	}
}
