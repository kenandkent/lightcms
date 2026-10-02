package generation_test

// R04/R06 regression: lease takeover resume + replay across template
// upgrades through the public Generate path.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/generation"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// TestReplaySurvivesTemplateUpgrade (R06): a completed publish replays from
// cache after the template moves v1→v2 — same key + same body must NOT 409
// TEMPLATE_VERSION_CHANGED or IDEMPOTENCY_CONFLICT.
func TestReplaySurvivesTemplateUpgrade(t *testing.T) {
	s := newGenSetup(t, generation.Options{})
	ctx := context.Background()
	tplID, _, v1 := s.seedTemplate(t, "r06-news", "", nil)
	actor := adminActor()

	mkReq := func() generation.GenerateRequest {
		return generation.GenerateRequest{
			Template: "r06-news", Title: "R06", Slug: "r06-page", FolderPath: "/news",
			Mode: "publish", ExpectedTemplateVersion: &v1, Data: map[string]any{"headline": "r06"},
		}
	}
	// pubCtx-style explicit body (upgrade-stable: no resolved version in it).
	mkCtx := func(key string) context.Context {
		raw := []byte(fmt.Sprintf(`{"template":"r06-news","title":"R06","slug":"r06-page","folder_path":"/news","mode":"publish","upsert":false,"data":{"headline":"r06"},"expected_template_version":%d}`, v1))
		return generation.WithIdempotency(context.Background(), generation.IdempotencyParams{
			Owner: actor.Owner(), Method: "POST", Path: "/api/v1/page-generation", Key: key, Body: raw,
		})
	}
	r1, err := s.gen.Generate(mkCtx("k-r06"), actor, mkReq())
	if err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	if r1.PublicationID == nil {
		t.Fatal("initial publish missing publication id")
	}

	// Upgrade the template v1 → v2 (same slug/fields/layout).
	if _, err := s.tpls.Update(ctx, tplID, 1, templatecontract.TemplateInput{
		Slug: "r06-news", Name: "r06-news name", Category: "news", Status: "active",
		HTMLLayout: genLayout,
		Fields: []models.TemplateField{
			{Name: "headline", Label: "Headline", Type: "text", Required: true},
			{Name: "body", Label: "Body", Type: "textarea", Required: false},
		},
	}); err != nil {
		t.Fatalf("template upgrade: %v", err)
	}

	// Same key + same body after the upgrade: replay, not 409/412.
	r2, err := s.gen.Generate(mkCtx("k-r06"), actor, mkReq())
	if err != nil {
		t.Fatalf("post-upgrade retry: %v (want replay)", err)
	}
	if r2.PublicationID == nil || *r2.PublicationID != *r1.PublicationID {
		t.Fatalf("post-upgrade retry must replay %v, got %+v", *r1.PublicationID, r2)
	}
	if n := countDocs(t, s.db, "content_publications", bson.M{}); n != 1 {
		t.Fatalf("publications = %d, want 1 (no duplicate on replay)", n)
	}
}

// TestTakeoverResumeActiveCompletesCached (R04): a crashed attempt whose
// publication already went active must complete its cache on retry — never
// mint a second Publication.
func TestTakeoverResumeActiveCompletesCached(t *testing.T) {
	s := newGenSetup(t, generation.Options{})
	ctx := context.Background()
	_, _, v1 := s.seedTemplate(t, "r04a-news", "", nil)
	actor := adminActor()

	mkReq := func() generation.GenerateRequest {
		return generation.GenerateRequest{
			Template: "r04a-news", Title: "R04A", Slug: "r04a-page", FolderPath: "/news",
			Mode: "publish", ExpectedTemplateVersion: &v1, Data: map[string]any{"headline": "r04a"},
		}
	}
	raw := []byte(`{"k":"r04a"}`)
	mkCtx := func() context.Context {
		return generation.WithIdempotency(context.Background(), generation.IdempotencyParams{
			Owner: actor.Owner(), Method: "POST", Path: "/api/v1/page-generation", Key: "k-r04a", Body: raw,
		})
	}
	r1, err := s.gen.Generate(mkCtx(), actor, mkReq())
	if err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	// Simulate the crash: back to processing with an expired lease, keeping
	// the frozen publication ID + content binding.
	if _, err := s.db.Collection("idempotency_records").UpdateOne(ctx,
		bson.M{"owner": actor.Owner(), "key": "k-r04a"},
		bson.M{"$set": bson.M{
			"state": "processing", "attempt_state": "processing",
			"status_code": nil, "response": nil,
			"processing_expires_at": time.Now().Add(-time.Minute),
		}}); err != nil {
		t.Fatalf("revert op: %v", err)
	}
	r2, err := s.gen.Generate(mkCtx(), actor, mkReq())
	if err != nil {
		t.Fatalf("takeover retry: %v", err)
	}
	if r2.PublicationID == nil || *r2.PublicationID != *r1.PublicationID {
		t.Fatalf("takeover must resume %v, got %+v", *r1.PublicationID, r2)
	}
	if n := countDocs(t, s.db, "content_publications", bson.M{}); n != 1 {
		t.Fatalf("publications = %d, want 1 (no duplicate on resume)", n)
	}
}

// TestTakeoverResumeSkipsContentWrite (R04): a taken-over op whose content
// write already committed must not re-execute it — no version bump, no
// duplicate version row; the saga publishes the bound version.
func TestTakeoverResumeSkipsContentWrite(t *testing.T) {
	s := newGenSetup(t, generation.Options{})
	ctx := context.Background()
	_, _, v1 := s.seedTemplate(t, "r04b-news", "", nil)
	actor := adminActor()

	// Draft the page first (v1, no publish).
	draftReq := generation.GenerateRequest{
		Template: "r04b-news", Title: "R04B", Slug: "r04b-page", FolderPath: "/news",
		Mode: "draft", Data: map[string]any{"headline": "r04b"},
	}
	dres, err := s.gen.Generate(ctx, actor, draftReq)
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	contentID, err := primitive.ObjectIDFromHex(dres.ID)
	if err != nil {
		t.Fatalf("draft id: %v", err)
	}

	// Manual op as the "crashed first attempt": bound to (content, v1),
	// lease already expired, publication never frozen.
	body := []byte(`{"k":"r04b"}`)
	op, err := s.idem.Begin(ctx, actor.Owner(), "POST", "/api/v1/page-generation", "k-r04b", body)
	if err != nil {
		t.Fatalf("manual Begin: %v", err)
	}
	if err := s.idem.BindContentAndVersion(ctx, nil, op.ID, contentID, 1, "/news/r04b-page"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if _, err := s.db.Collection("idempotency_records").UpdateOne(ctx,
		bson.M{"_id": op.ID},
		bson.M{"$set": bson.M{"processing_expires_at": time.Now().Add(-time.Minute)}}); err != nil {
		t.Fatalf("expire lease: %v", err)
	}

	pubReq := generation.GenerateRequest{
		Template: "r04b-news", Title: "R04B", Slug: "r04b-page", FolderPath: "/news",
		Mode: "publish", Upsert: true, ExpectedTemplateVersion: &v1,
		Data: map[string]any{"headline": "r04b"},
	}
	pubCtx := generation.WithIdempotency(context.Background(), generation.IdempotencyParams{
		Owner: actor.Owner(), Method: "POST", Path: "/api/v1/page-generation", Key: "k-r04b", Body: body,
	})
	res, err := s.gen.Generate(pubCtx, actor, pubReq)
	if err != nil {
		t.Fatalf("takeover publish: %v", err)
	}
	if res.PublicationID == nil {
		t.Fatal("takeover publish missing publication id")
	}
	var content models.Content
	if err := s.db.FindOne(ctx, "content", bson.M{"_id": contentID}, &content); err != nil {
		t.Fatalf("load content: %v", err)
	}
	if content.CurrentVersion != 1 {
		t.Fatalf("resumed publish bumped content to v%d, want v1 (no rewrite)", content.CurrentVersion)
	}
	if n := countDocs(t, s.db, "content_versions", bson.M{"content_id": contentID}); n != 1 {
		t.Fatalf("version rows = %d, want 1 (no duplicate write)", n)
	}
	active, err := s.repo.GetActive(ctx, contentID)
	if err != nil || active == nil || active.ContentVersion != 1 {
		t.Fatalf("active publication = %+v, err = %v (want v1 live)", active, err)
	}
}
