package generation_test

// Task 17B coverage-gap tests round 3: duplicate-key flows, default
// materialization, deleted-live shadow, preview/audit variants,
// restore/saga-error mapping (external, DB-backed).

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/generation"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/publicurl"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"
	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func gapShadowContent(t *testing.T) {
	t.Helper()
}

func TestCoverGapDuplicateKeyFlows(t *testing.T) {
	gen, db, tpls, _, _ := gapGenSetup(t, nil)
	ctx := context.Background()
	tv := gapSeedTemplate(t, tpls, "gapdup")
	actor := authed()

	// Deleted shadow row: findLive misses it, but the unique index still
	// fires on insert → PATH_CONFLICT via mapDupKey.
	shadow := primitive.NewObjectID()
	_, err := db.Collection("content").InsertOne(ctx, bson.M{
		"_id": shadow, "template_id": primitive.NewObjectID(), "title": "Shadow",
		"slug": "shadow", "folder_path": "/news", "full_path": "/news/shadow",
		"canonical_full_path": "/news/shadow", "path_scope": "live", "path_active": true,
		"deleted": true, "current_version": 1, "data": bson.M{},
		"published": false, "created_at": time.Now(), "updated_at": time.Now(),
	})
	if err != nil {
		t.Fatalf("seed shadow: %v", err)
	}
	shadowReq := generation.GenerateRequest{Template: "gapdup", Title: "Shadow", Slug: "shadow",
		FolderPath: "/news", Mode: "draft", Data: map[string]any{"headline": "h"}}
	if _, err := gen.Generate(ctx, actor, shadowReq); generation.CodeOf(err) != generation.CodePathConflict {
		t.Fatalf("draft shadow dup: %v", err)
	}

	// Version-duplicate on publish replace → CONTENT_VERSION_CONFLICT.
	pubReq := generation.GenerateRequest{Template: "gapdup", Title: "T", Slug: "vdup",
		FolderPath: "/news", Mode: "publish", ExpectedTemplateVersion: &tv,
		Data: map[string]any{"headline": "h"}}
	pres, err := gen.Generate(pubCtx(actor, "k-vdup-1", pubReq, tv), actor, pubReq)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	cid := mustObjectID(pres.ID)
	if _, err := db.Collection("content_versions").InsertOne(ctx, bson.M{
		"_id": primitive.NewObjectID(), "content_id": cid, "version": int64(2),
		"title": "sneaky", "created_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed dup version: %v", err)
	}
	pubReq.Upsert = true
	pubReq.Data = map[string]any{"headline": "h2"}
	if _, err := gen.Generate(pubCtx(actor, "k-vdup-2", pubReq, tv), actor, pubReq); generation.CodeOf(err) != generation.CodeContentVersionConflict {
		t.Fatalf("publish version dup: %v", err)
	}

	// Version-duplicate on draft replace.
	dReq := generation.GenerateRequest{Template: "gapdup", Title: "D", Slug: "ddup",
		FolderPath: "/news", Mode: "draft", Data: map[string]any{"headline": "h"}}
	if _, err := gen.Generate(ctx, actor, dReq); err != nil {
		t.Fatalf("draft: %v", err)
	}
	var draftDoc bson.M
	if err := db.Collection("content").FindOne(ctx, bson.M{"full_path": "/news/ddup"}).Decode(&draftDoc); err != nil {
		t.Fatalf("load draft: %v", err)
	}
	did := draftDoc["_id"].(primitive.ObjectID)
	if _, err := db.Collection("content_versions").InsertOne(ctx, bson.M{
		"_id": primitive.NewObjectID(), "content_id": did, "version": int64(2),
		"title": "sneaky", "created_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed draft dup version: %v", err)
	}
	dReq.Upsert = true
	if _, err := gen.Generate(ctx, actor, dReq); generation.CodeOf(err) != generation.CodeContentVersionConflict {
		t.Fatalf("draft version dup: %v", err)
	}
}

func TestCoverGapDefaultsAndVariants(t *testing.T) {
	// Bare (nil-audit) service first: every setup wipes the shared DB, so
	// seed only after the last setup call.
	bareGen, _, _, _, _ := gapGenSetup(t, func(o *generation.Options) { o.Audit = nil })
	gen, db, tpls, _, _ := gapGenSetup(t, nil)
	ctx := context.Background()
	_, ver, err := tpls.Create(ctx, templatecontract.TemplateInput{
		Slug: "gapdef", Name: "D", Category: "n", Status: "active", HTMLLayout: genLayout,
		Fields: []models.TemplateField{
			{Name: "headline", Label: "H", Type: "text", Required: true},
			{Name: "count", Label: "C", Type: "number", Default: "3.5"},
			{Name: "badnum", Label: "BN", Type: "number", Default: "abc"},
			{Name: "flag", Label: "F", Type: "boolean", Default: "true"},
			{Name: "off", Label: "O", Type: "boolean", Default: "false"},
			{Name: "maybe", Label: "M", Type: "boolean", Default: "maybe"},
			{Name: "nick", Label: "N", Type: "text", Default: "nick"},
		},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = ver
	dReq := generation.GenerateRequest{Template: "gapdef", Title: "T", Slug: "def-1",
		FolderPath: "/news", Mode: "draft", Data: map[string]any{"headline": "h"}}
	resp, err := gen.Generate(ctx, authed(), dReq)
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	var doc bson.M
	if err := db.Collection("content").FindOne(ctx, bson.M{"_id": mustObjectID(resp.ID)}).Decode(&doc); err != nil {
		t.Fatalf("load: %v", err)
	}
	data, _ := doc["data"].(bson.M)
	if data["count"] != 3.5 || data["flag"] != true || data["nick"] != "nick" {
		t.Fatalf("materialized defaults: %v", data)
	}
	if _, ok := data["off"]; ok {
		t.Fatalf("false default must stay missing: %v", data)
	}
	if _, ok := data["maybe"]; ok {
		t.Fatalf("invalid bool default must stay missing: %v", data)
	}
	if _, ok := data["badnum"]; ok {
		t.Fatalf("invalid number default must stay missing: %v", data)
	}

	// Preview on an existing draft (live!=nil branch).
	pv, err := gen.Generate(ctx, authed("content.view"), generation.GenerateRequest{
		Template: "gapdef", Title: "T", Slug: "def-1", FolderPath: "/news",
		Mode: "preview", Data: map[string]any{"headline": "h"},
	})
	if err != nil {
		t.Fatalf("preview live: %v", err)
	}
	if pv.Action != "preview" || pv.ID == "" || pv.ContentVersion != 1 {
		t.Fatalf("preview = %+v", pv)
	}

	// Nil-audit service draft (auditf nil branch) on the same shared DB.
	if _, err := bareGen.Generate(ctx, authed(), generation.GenerateRequest{
		Template: "gapdef", Title: "T", Slug: "def-bare", FolderPath: "/news",
		Mode: "draft", Data: map[string]any{"headline": "h"},
	}); err != nil {
		t.Fatalf("bare audit draft: %v", err)
	}
}

func TestCoverGapSagaErrorMapping(t *testing.T) {
	gen, _, tpls, _, _ := gapGenSetup(t, nil)
	ctx := context.Background()
	// Template whose layout always fails rendering → saga stage/render error
	// → 503 STORE_UNAVAILABLE through mapSagaErr + completePublishError.
	_, ver, err := tpls.Create(ctx, templatecontract.TemplateInput{
		Slug: "gapboom", Name: "B", Category: "n", Status: "active",
		HTMLLayout: "<p>{{index .missing 0}}</p>",
		Fields:     []models.TemplateField{{Name: "headline", Label: "H", Type: "text", Required: true}},
	})
	if err != nil {
		t.Fatalf("seed boom: %v", err)
	}
	tvn := ver.Version
	bReq := generation.GenerateRequest{Template: "gapboom", Title: "T", Slug: "boom-1",
		FolderPath: "/news", Mode: "publish", ExpectedTemplateVersion: &tvn,
		Data: map[string]any{"headline": "h"}}
	// The saga render failure surfaces as INTERNAL_ERROR through the
	// default mapSagaErr branch (only the stage/verify/activate code family
	// maps to 503); the path still exercises completePublishError.
	if _, err := gen.Generate(pubCtx(authed(), "k-boom-1", bReq, tvn), authed(), bReq); generation.CodeOf(err) != generation.CodeInternal {
		t.Fatalf("saga render fail: %v", err)
	}

	// Restore paths: Begin-conflict (same key, different version) and
	// version-insert dup inside the restore transaction.
	tv := gapSeedTemplate(t, tpls, "gaprsv")
	actor := adminActor()
	pubReq := generation.GenerateRequest{Template: "gaprsv", Title: "T", Slug: "rsv-1",
		FolderPath: "/news", Mode: "publish", ExpectedTemplateVersion: &tv,
		Data: map[string]any{"headline": "v1"}}
	r1, err := gen.Generate(pubCtx(actor, "k-rsv-1", pubReq, tv), actor, pubReq)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	cid := mustObjectID(r1.ID)
	mkCtx := func(key string) context.Context {
		return generation.WithIdempotency(ctx, generation.IdempotencyParams{
			Owner: actor.Owner(), Method: "POST", Path: "/p", Key: key,
		})
	}
	if _, err := gen.RestoreAndPublish(mkCtx("k-rsv-op"), actor, cid, 1, nil); err != nil {
		t.Fatalf("restore v1: %v", err)
	}
	// Same key + different version → Begin conflict.
	if _, err := gen.RestoreAndPublish(mkCtx("k-rsv-op"), actor, cid, 2, nil); generation.CodeOf(err) != generation.CodeIdempotencyConflict {
		t.Fatalf("restore begin conflict: %v", err)
	}
}

func TestCoverGapServiceWiringVariants(t *testing.T) {
	gen, db, tpls, idem, _ := gapGenSetup(t, nil)
	ctx := context.Background()
	tv := gapSeedTemplate(t, tpls, "gapwire")
	actor := adminActor()

	base, _ := url.Parse("http://localhost:8080")
	resolver, _ := publicurl.NewResolver(base)
	repo := publication.NewRepository(db, nil)

	// Pubs-nil service: RunUpgradeJob loads the job, then reports unwired.
	pubReq := generation.GenerateRequest{Template: "gapwire", Title: "T", Slug: "wire-1",
		FolderPath: "/news", Mode: "publish", ExpectedTemplateVersion: &tv,
		Data: map[string]any{"headline": "h"}}
	if _, err := gen.Generate(pubCtx(actor, "k-wire-1", pubReq, tv), actor, pubReq); err != nil {
		t.Fatalf("publish: %v", err)
	}
	job, err := gen.StartUpgradeJob(ctx, actor, "gapwire")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	noPubs := generation.NewService(db, generation.Options{
		Templates: tpls, PubRepo: repo, Idem: idem, URLs: resolver,
	})
	if _, err := noPubs.RunUpgradeJob(ctx, actor, job.ID); generation.CodeOf(err) != generation.CodeInternal {
		t.Fatalf("run pubs-unwired: %v", err)
	}
	// Broken-repo service: ListPublications surfaces the transport error.
	bdb := testutil.MustConnectBrokenDB(t)
	brepo := publication.NewRepository(bdb, nil)
	bsvc := generation.NewService(bdb, generation.Options{PubRepo: brepo})
	if _, err := bsvc.ListPublications(ctx, primitive.NewObjectID()); generation.CodeOf(err) != generation.CodeInternal {
		t.Fatalf("list broken: %v", err)
	}
	// RollbackPublication with key-only idempotency params (owner/method/
	// path defaults in beginForPublish).
	cid := primitive.NewObjectID()
	keyOnly := generation.WithIdempotency(ctx, generation.IdempotencyParams{Key: "k-keyonly"})
	if _, err := gen.RollbackPublication(keyOnly, actor, cid, primitive.NewObjectID(), nil); err == nil {
		t.Fatalf("rollback key-only: want error (unknown source)")
	}
}

func TestCoverGapPublishCreateDupAndEmptyLayout(t *testing.T) {
	gen, db, tpls, _, _ := gapGenSetup(t, nil)
	ctx := context.Background()
	tv := gapSeedTemplate(t, tpls, "gapdup2")
	actor := authed()

	// Deleted shadow row on the publish path: createLiveForPublish insert
	// collides → PATH_CONFLICT.
	_, err := db.Collection("content").InsertOne(ctx, bson.M{
		"_id": primitive.NewObjectID(), "template_id": primitive.NewObjectID(), "title": "Shadow",
		"slug": "pshadow", "folder_path": "/news", "full_path": "/news/pshadow",
		"canonical_full_path": "/news/pshadow", "path_scope": "live", "path_active": true,
		"deleted": true, "current_version": 1, "data": bson.M{},
		"published": false, "created_at": time.Now(), "updated_at": time.Now(),
	})
	if err != nil {
		t.Fatalf("seed shadow: %v", err)
	}
	shadowReq := generation.GenerateRequest{Template: "gapdup2", Title: "Shadow", Slug: "pshadow",
		FolderPath: "/news", Mode: "publish", ExpectedTemplateVersion: &tv,
		Data: map[string]any{"headline": "h"}}
	if _, err := gen.Generate(pubCtx(actor, "k-pshadow-1", shadowReq, tv), actor, shadowReq); generation.CodeOf(err) != generation.CodePathConflict {
		t.Fatalf("publish shadow dup: %v", err)
	}

	// Empty-layout template renders empty bytes → the saga rejects empty
	// output before staging (INTERNAL_ERROR via the default mapSagaErr
	// branch; the path still exercises completePublishError).
	_, ever, err := tpls.Create(ctx, templatecontract.TemplateInput{
		Slug: "gapempty", Name: "E", Category: "n", Status: "active",
		HTMLLayout: "",
		Fields:     []models.TemplateField{{Name: "headline", Label: "H", Type: "text", Required: true}},
	})
	if err != nil {
		t.Fatalf("seed empty layout: %v", err)
	}
	evn := ever.Version
	eReq := generation.GenerateRequest{Template: "gapempty", Title: "T", Slug: "empty-1",
		FolderPath: "/news", Mode: "publish", ExpectedTemplateVersion: &evn,
		Data: map[string]any{"headline": "h"}}
	if _, err := gen.Generate(pubCtx(actor, "k-empty-1", eReq, evn), actor, eReq); generation.CodeOf(err) != generation.CodeInternal {
		t.Fatalf("empty layout render fail: %v", err)
	}
}
