package generation_test

// R01 regression: the V3 production Actor dropped the caller's RBAC role and
// treated empty Scopes as allow-all, so a viewer (empty scopes) passed the
// generation scope check and could create/edit/publish through the new
// product API. Actor.Can now intersects role ∩ sandbox ∩ scopes-allowlist
// (mirroring auth.UserHasPermission), and every generation authorization
// check uses Can.

import (
	"context"
	"testing"

	"github.com/jonradoff/lightcms/v7/internal/product/generation"

	"go.mongodb.org/mongo-driver/bson"
)

func TestActorCanMatrix(t *testing.T) {
	admin := generation.Actor{Authenticated: true, IsAdmin: true, Role: "admin"}
	for _, p := range []string{"content.view", "content.create", "content.edit", "content.publish",
		"template.view", "template.edit", "settings.edit", "content.delete"} {
		if !admin.Can(p) {
			t.Errorf("admin Can(%q) = false, want true", p)
		}
	}
	viewer := generation.Actor{Authenticated: true, Role: "viewer"}
	if !viewer.Can("content.view") {
		t.Error("viewer Can(content.view) = false, want true")
	}
	for _, p := range []string{"content.create", "content.edit", "content.publish", "template.edit"} {
		if viewer.Can(p) {
			t.Errorf("viewer Can(%q) = true, want false (R01: empty scopes must not allow-all)", p)
		}
	}
	editor := generation.Actor{Authenticated: true, Role: "editor"}
	if !editor.Can("content.edit") || !editor.Can("content.publish") {
		t.Error("editor must hold content.edit + content.publish")
	}
	if editor.Can("template.edit") {
		t.Error("editor Can(template.edit) = true, want false")
	}
	// Sandbox-only narrows even admin to the sandbox allowlist.
	sandboxAdmin := generation.Actor{Authenticated: true, IsAdmin: true, Role: "admin", SandboxOnly: true}
	if !sandboxAdmin.Can("content.edit") {
		t.Error("sandbox admin Can(content.edit) = false, want true")
	}
	if sandboxAdmin.Can("content.publish") {
		t.Error("sandbox admin Can(content.publish) = true, want false (live publish excluded)")
	}
	// Key scope allowlist narrows the role set.
	scopedAdmin := generation.Actor{Authenticated: true, IsAdmin: true, Role: "admin", Scopes: []string{"content.view"}}
	if !scopedAdmin.Can("content.view") {
		t.Error("scoped admin Can(content.view) = false, want true")
	}
	if scopedAdmin.Can("content.edit") {
		t.Error("scoped admin Can(content.edit) = true, want false (allowlist)")
	}
	// Empty role grants nothing (extractors must populate it).
	if (generation.Actor{Authenticated: true}).Can("content.view") {
		t.Error("empty-role Can(content.view) = true, want false")
	}
}

// TestViewerGenerateDeniedZeroMutation is the R01 repro: a viewer calling
// the new product API the way the production extractor builds the Actor
// (IsAdmin=false, empty scopes) must be denied with zero business writes.
func TestViewerGenerateDeniedZeroMutation(t *testing.T) {
	s := newGenSetup(t, generation.Options{})
	ctx := context.Background()
	_, _, tv := s.seedTemplate(t, "r01-news", "", nil)

	viewer := generation.Actor{
		ID: "viewer-1", Email: "viewer@r01.test", Authenticated: true,
		IsAdmin: false, Role: "viewer", Scopes: []string{},
		ActorKind: "human", Via: "api",
	}
	req := generation.GenerateRequest{
		Template: "r01-news", Title: "R01 Page", Slug: "r01-page", FolderPath: "/news",
		Mode: "draft", Data: map[string]any{"headline": "r01"},
	}
	pctx := pubCtx(viewer, "k-r01-1", req, tv)
	before, err := s.db.Collection("content").CountDocuments(ctx, bson.M{})
	if err != nil {
		t.Fatalf("count before: %v", err)
	}
	_, err = s.gen.Generate(pctx, viewer, req)
	if err == nil || generation.CodeOf(err) != generation.CodePermissionDenied {
		t.Fatalf("viewer draft Generate = %v, want PERMISSION_DENIED", err)
	}
	after, err := s.db.Collection("content").CountDocuments(ctx, bson.M{})
	if err != nil {
		t.Fatalf("count after: %v", err)
	}
	if after != before {
		t.Fatalf("denied request wrote content rows: before=%d after=%d", before, after)
	}

	// Upgrade jobs stay admin-only: editor (no template.edit) is denied.
	editor := viewer
	editor.ID, editor.Email, editor.Role = "editor-1", "editor@r01.test", "editor"
	if _, err := s.gen.StartUpgradeJob(ctx, editor, "r01-news"); err == nil ||
		generation.CodeOf(err) != generation.CodePermissionDenied {
		t.Fatalf("editor StartUpgradeJob = %v, want PERMISSION_DENIED", err)
	}
	// Read paths stay open to viewers: a viewer preview of a missing
	// template fails downstream (not-found), never at RBAC.
	if _, err := s.gen.PreviewUpgrade(ctx, viewer, "no-such-template"); err == nil ||
		generation.CodeOf(err) == generation.CodePermissionDenied {
		t.Fatalf("viewer PreviewUpgrade missing template = %v, want non-RBAC error", err)
	}
}
