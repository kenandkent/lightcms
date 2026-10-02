package generation_test

// Task 17B coverage-gap tests round 2: schema/restore/upgrade/migrate ops,
// duplicate-key flows, saga-error mapping (external, DB-backed).

import (
	"context"
	"testing"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/generation"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"
	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestCoverGapSchemaOps(t *testing.T) {
	gen, db, tpls, _, _ := gapGenSetup(t, nil)
	ctx := context.Background()
	tv := gapSeedTemplate(t, tpls, "gapschema")

	// Unknown slug → not found.
	if _, _, err := gen.SchemaForSlug(ctx, "missing"); generation.CodeOf(err) != generation.CodeTemplateNotFound {
		t.Fatalf("SchemaForSlug unknown: %v", err)
	}
	// Success.
	ver, raw, err := gen.SchemaForSlug(ctx, "gapschema")
	if err != nil {
		t.Fatalf("SchemaForSlug: %v", err)
	}
	if ver.Version != tv || len(raw) == 0 {
		t.Fatalf("schema = %d %d", ver.Version, len(raw))
	}
	// Publish, then List/Get success.
	pubReq := generation.GenerateRequest{Template: "gapschema", Title: "T", Slug: "schema-1",
		FolderPath: "/news", Mode: "publish", ExpectedTemplateVersion: &tv,
		Data: map[string]any{"headline": "h"}}
	pres, err := gen.Generate(pubCtx(authed(), "k-schema-1", pubReq, tv), authed(), pubReq)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	cid := mustObjectID(pres.ID)
	pid := mustObjectID(*pres.PublicationID)
	list, err := gen.ListPublications(ctx, cid)
	if err != nil || len(list) == 0 {
		t.Fatalf("ListPublications = %d, %v", len(list), err)
	}
	got, err := gen.GetPublication(ctx, cid, pid)
	if err != nil || got.ID != pid {
		t.Fatalf("GetPublication: %v", err)
	}
	if _, err := gen.GetPublication(ctx, cid, primitive.NewObjectID()); generation.CodeOf(err) != generation.CodePublicationNotFound {
		t.Fatalf("GetPublication unknown: %v", err)
	}
	if _, err := gen.GetPublication(ctx, primitive.NewObjectID(), pid); generation.CodeOf(err) != generation.CodePublicationNotFound {
		t.Fatalf("GetPublication wrong content: %v", err)
	}
	// Nil-repo service → internal errors.
	nilSvc := generation.NewService(nil, generation.Options{})
	if _, err := nilSvc.ListPublications(ctx, cid); generation.CodeOf(err) != generation.CodeInternal {
		t.Fatalf("ListPublications unwired: %v", err)
	}
	if _, err := nilSvc.GetPublication(ctx, cid, pid); generation.CodeOf(err) != generation.CodeInternal {
		t.Fatalf("GetPublication unwired: %v", err)
	}
	// Broken-DB service → transport errors.
	bdb := testutil.MustConnectBrokenDB(t)
	brepo := publication.NewRepository(bdb, nil)
	bsvc := generation.NewService(bdb, generation.Options{PubRepo: brepo})
	if _, _, err := bsvc.SchemaForSlug(ctx, "gapschema"); generation.CodeOf(err) != generation.CodeInternal {
		t.Fatalf("SchemaForSlug broken: %v", err)
	}
	if _, err := bsvc.GetPublication(ctx, cid, pid); generation.CodeOf(err) != generation.CodeInternal {
		t.Fatalf("GetPublication broken: %v", err)
	}
	_ = db
}

func TestCoverGapRollbackPublicationMatrix(t *testing.T) {
	gen, _, tpls, idem, _ := gapGenSetup(t, nil)
	ctx := context.Background()
	tv := gapSeedTemplate(t, tpls, "gaprb")
	actor := authed()

	if _, err := gen.RollbackPublication(ctx, generation.Actor{Role: "admin"}, primitive.NewObjectID(), primitive.NewObjectID(), nil); generation.CodeOf(err) != generation.CodeUnauthenticated {
		t.Fatalf("rollback unauth: %v", err)
	}
	scoped := authed("content.view")
	idemCtx := generation.WithIdempotency(ctx, generation.IdempotencyParams{Owner: "o", Key: "k"})
	if _, err := gen.RollbackPublication(idemCtx, scoped, primitive.NewObjectID(), primitive.NewObjectID(), nil); generation.CodeOf(err) != generation.CodePermissionDenied {
		t.Fatalf("rollback scope: %v", err)
	}
	if _, err := gen.RollbackPublication(ctx, actor, primitive.NewObjectID(), primitive.NewObjectID(), nil); generation.CodeOf(err) != generation.CodeIdempotencyKeyRequired {
		t.Fatalf("rollback no key: %v", err)
	}
	nilSvc := generation.NewService(nil, generation.Options{})
	if _, err := nilSvc.RollbackPublication(idemCtx, actor, primitive.NewObjectID(), primitive.NewObjectID(), nil); generation.CodeOf(err) != generation.CodeInternal {
		t.Fatalf("rollback unwired: %v", err)
	}

	// Publish, then rollback success + replay + conflict.
	pubReq := generation.GenerateRequest{Template: "gaprb", Title: "T", Slug: "rb-1",
		FolderPath: "/news", Mode: "publish", ExpectedTemplateVersion: &tv,
		Data: map[string]any{"headline": "h"}}
	pres, err := gen.Generate(pubCtx(actor, "k-rb-1", pubReq, tv), actor, pubReq)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	cid := mustObjectID(pres.ID)
	pid := mustObjectID(*pres.PublicationID)
	rbCtx := generation.WithIdempotency(ctx, generation.IdempotencyParams{
		Owner: actor.Owner(), Method: "POST", Path: "/p", Key: "k-rb-op-1",
	})
	out, err := gen.RollbackPublication(rbCtx, actor, cid, pid, nil)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if out.Mode != "rollback" || out.PublicationID == "" {
		t.Fatalf("rollback out = %+v", out)
	}
	// Same key → replay.
	out2, err := gen.RollbackPublication(rbCtx, actor, cid, pid, nil)
	if err != nil || out2.PublicationID != out.PublicationID {
		t.Fatalf("rollback replay: %+v %v", out2, err)
	}
	// Same key + different source → Begin conflict.
	if _, err := gen.RollbackPublication(rbCtx, actor, cid, primitive.NewObjectID(), nil); generation.CodeOf(err) != generation.CodeIdempotencyConflict {
		t.Fatalf("rollback begin conflict: %v", err)
	}
	// Unknown source → saga error mapping.
	badCtx := generation.WithIdempotency(ctx, generation.IdempotencyParams{
		Owner: actor.Owner(), Method: "POST", Path: "/p", Key: "k-rb-op-2",
	})
	if _, err := gen.RollbackPublication(badCtx, actor, cid, primitive.NewObjectID(), nil); err == nil {
		t.Fatalf("rollback unknown source: want error")
	}
	// In-progress: pre-Begin the canonical rollback body with a live lease.
	sameBody := []byte(`{"source":"` + pid.Hex() + `","expected_active_id":null}`)
	if _, err := idem.Begin(ctx, actor.Owner(), "POST", "/p2", "k-rb-busy", sameBody); err != nil {
		t.Fatalf("pre-Begin: %v", err)
	}
	busyCtx := generation.WithIdempotency(ctx, generation.IdempotencyParams{
		Owner: actor.Owner(), Method: "POST", Path: "/p2", Key: "k-rb-busy",
	})
	if _, err := gen.RollbackPublication(busyCtx, actor, cid, pid, nil); generation.CodeOf(err) != generation.CodeRequestInProgress {
		t.Fatalf("rollback in-progress: %v", err)
	}
}

func TestCoverGapRestoreRevertMatrix(t *testing.T) {
	gen, _, tpls, _, _ := gapGenSetup(t, nil)
	ctx := context.Background()
	tv := gapSeedTemplate(t, tpls, "gaprestore")
	actor := adminActor()

	if _, err := gen.RestoreAndPublish(ctx, generation.Actor{Role: "admin"}, primitive.NewObjectID(), 1, nil); generation.CodeOf(err) != generation.CodeUnauthenticated {
		t.Fatalf("restore unauth: %v", err)
	}
	if _, err := gen.RevertLive(ctx, generation.Actor{Role: "admin"}, primitive.NewObjectID(), primitive.NewObjectID(), nil); generation.CodeOf(err) != generation.CodeUnauthenticated {
		t.Fatalf("revert unauth: %v", err)
	}
	scoped := authed("content.view")
	kctx := generation.WithIdempotency(ctx, generation.IdempotencyParams{Owner: "o", Key: "k"})
	if _, err := gen.RestoreAndPublish(kctx, scoped, primitive.NewObjectID(), 1, nil); generation.CodeOf(err) != generation.CodePermissionDenied {
		t.Fatalf("restore scope: %v", err)
	}
	if _, err := gen.RevertLive(kctx, scoped, primitive.NewObjectID(), primitive.NewObjectID(), nil); generation.CodeOf(err) != generation.CodePermissionDenied {
		t.Fatalf("revert scope: %v", err)
	}
	if _, err := gen.RestoreAndPublish(ctx, actor, primitive.NewObjectID(), 1, nil); generation.CodeOf(err) != generation.CodeIdempotencyKeyRequired {
		t.Fatalf("restore no key: %v", err)
	}
	if _, err := gen.RevertLive(ctx, actor, primitive.NewObjectID(), primitive.NewObjectID(), nil); generation.CodeOf(err) != generation.CodeIdempotencyKeyRequired {
		t.Fatalf("revert no key: %v", err)
	}
	nilSvc := generation.NewService(nil, generation.Options{})
	if _, err := nilSvc.RestoreAndPublish(kctx, actor, primitive.NewObjectID(), 1, nil); generation.CodeOf(err) != generation.CodeInternal {
		t.Fatalf("restore unwired: %v", err)
	}
	if _, err := nilSvc.RevertLive(kctx, actor, primitive.NewObjectID(), primitive.NewObjectID(), nil); generation.CodeOf(err) != generation.CodeInternal {
		t.Fatalf("revert unwired: %v", err)
	}

	// Publish v1, bump to v2 via draft+publish, restore v1, revert to v1 pub.
	pubReq := generation.GenerateRequest{Template: "gaprestore", Title: "T", Slug: "rr-1",
		FolderPath: "/news", Mode: "publish", ExpectedTemplateVersion: &tv,
		Data: map[string]any{"headline": "v1"}}
	r1, err := gen.Generate(pubCtx(actor, "k-rrestore-1", pubReq, tv), actor, pubReq)
	if err != nil {
		t.Fatalf("publish v1: %v", err)
	}
	cid := mustObjectID(r1.ID)
	pid1 := mustObjectID(*r1.PublicationID)
	pubReq.Data = map[string]any{"headline": "v2"}
	pubReq.Upsert = true
	r2, err := gen.Generate(pubCtx(actor, "k-rrestore-2", pubReq, tv), actor, pubReq)
	if err != nil {
		t.Fatalf("publish v2: %v", err)
	}
	_ = r2
	mkCtx := func(key string) context.Context {
		return generation.WithIdempotency(ctx, generation.IdempotencyParams{
			Owner: actor.Owner(), Method: "POST", Path: "/p", Key: key,
		})
	}
	// Unknown version → not found.
	if _, err := gen.RestoreAndPublish(mkCtx("k-rs-0"), actor, cid, 99, nil); generation.CodeOf(err) != generation.CodeContentNotFound {
		t.Fatalf("restore unknown version: %v", err)
	}
	// Stale expected active → conflict.
	ghost := primitive.NewObjectID()
	if _, err := gen.RestoreAndPublish(mkCtx("k-rs-1"), actor, cid, 1, &ghost); generation.CodeOf(err) != generation.CodePublicationConflict {
		t.Fatalf("restore stale expected: %v", err)
	}
	// Success → v3 from v1 data.
	rout, err := gen.RestoreAndPublish(mkCtx("k-rs-2"), actor, cid, 1, nil)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if rout.Mode != "restore_and_publish" || rout.ContentVersion != 3 {
		t.Fatalf("restore out = %+v", rout)
	}
	// Same key → replay.
	rout2, err := gen.RestoreAndPublish(mkCtx("k-rs-2"), actor, cid, 1, nil)
	if err != nil || rout2.PublicationID != rout.PublicationID {
		t.Fatalf("restore replay: %+v %v", rout2, err)
	}
	// Revert to the v1 publication → success + replay.
	vout, err := gen.RevertLive(mkCtx("k-rv-1"), actor, cid, pid1, nil)
	if err != nil {
		t.Fatalf("revert: %v", err)
	}
	if vout.Mode != "revert_live" {
		t.Fatalf("revert out = %+v", vout)
	}
	vout2, err := gen.RevertLive(mkCtx("k-rv-1"), actor, cid, pid1, nil)
	if err != nil || vout2.PublicationID != vout.PublicationID {
		t.Fatalf("revert replay: %+v %v", vout2, err)
	}
	// Revert unknown source → saga error.
	if _, err := gen.RevertLive(mkCtx("k-rv-2"), actor, cid, primitive.NewObjectID(), nil); err == nil {
		t.Fatalf("revert unknown source: want error")
	}
}

func TestCoverGapMigrateSlugMatrix(t *testing.T) {
	gen, _, tpls, _, _ := gapGenSetup(t, nil)
	ctx := context.Background()
	admin := adminActor()
	tplID, _, _ := mustCreateTpl(t, tpls, "gapmigrate")

	if _, err := gen.MigrateSlug(ctx, generation.Actor{Role: "admin"}, tplID, "x"); generation.CodeOf(err) != generation.CodeUnauthenticated {
		t.Fatalf("migrate unauth: %v", err)
	}
	if _, err := gen.MigrateSlug(ctx, authed(), tplID, "x"); generation.CodeOf(err) != generation.CodePermissionDenied {
		t.Fatalf("migrate non-admin: %v", err)
	}
	if _, err := gen.MigrateSlug(ctx, admin, tplID, "Bad Slug!"); generation.CodeOf(err) != generation.CodeFieldValidationFailed {
		t.Fatalf("migrate bad slug: %v", err)
	}
	if _, err := gen.MigrateSlug(ctx, admin, primitive.NewObjectID(), "elsewhere"); generation.CodeOf(err) != generation.CodeTemplateNotFound {
		t.Fatalf("migrate unknown: %v", err)
	}
	if _, err := gen.MigrateSlug(ctx, admin, tplID, "gapmigrate"); generation.CodeOf(err) != generation.CodeInvalidRequest {
		t.Fatalf("migrate same slug: %v", err)
	}
	mustCreateTpl(t, tpls, "taken-slug")
	if _, err := gen.MigrateSlug(ctx, admin, tplID, "taken-slug"); generation.CodeOf(err) != generation.CodePathConflict {
		t.Fatalf("migrate dup slug: %v", err)
	}
	res, err := gen.MigrateSlug(ctx, admin, tplID, "gapmigrated")
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if res.NewSlug != "gapmigrated" || res.OldSlug != "gapmigrate" {
		t.Fatalf("migrate out = %+v", res)
	}
}

func mustCreateTpl(t *testing.T, tpls *templatecontract.Service, slug string) (primitive.ObjectID, primitive.ObjectID, int64) {
	t.Helper()
	tpl, ver, err := tpls.Create(context.Background(), templatecontract.TemplateInput{
		Slug: slug, Name: slug, Category: "n", Status: "active",
		HTMLLayout: genLayout,
		Fields:     []models.TemplateField{{Name: "headline", Label: "H", Type: "text", Required: true}},
	})
	if err != nil {
		t.Fatalf("create %s: %v", slug, err)
	}
	return tpl.ID, ver.ID, ver.Version
}

func TestCoverGapUpgradeWithPages(t *testing.T) {
	gen, db, tpls, _, _ := gapGenSetup(t, nil)
	ctx := context.Background()
	actor := adminActor()
	tv1 := gapSeedTemplate(t, tpls, "gapup")

	// Publish a page on v1.
	pubReq := generation.GenerateRequest{Template: "gapup", Title: "T", Slug: "up-1",
		FolderPath: "/news", Mode: "publish", ExpectedTemplateVersion: &tv1,
		Data: map[string]any{"headline": "h"}}
	rp1, err := gen.Generate(pubCtx(actor, "k-up-1", pubReq, tv1), actor, pubReq)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	cid1 := mustObjectID(rp1.ID)
	// Bump the template to v2 (add optional field).
	cur, err := tpls.GetCurrent(ctx, "gapup")
	if err != nil {
		t.Fatalf("GetCurrent: %v", err)
	}
	upd := cur.Input()
	upd.Fields = append(upd.Fields, models.TemplateField{Name: "sub", Label: "S", Type: "text", Required: false})
	if _, err := tpls.Update(ctx, cur.TemplateID, cur.Version, upd); err != nil {
		t.Fatalf("Update template: %v", err)
	}

	// Preview with pages + scope-denied preview.
	if _, err := gen.PreviewUpgrade(ctx, authed("content.view"), "gapup"); generation.CodeOf(err) != generation.CodePermissionDenied {
		t.Fatalf("preview scope: %v", err)
	}
	pv, err := gen.PreviewUpgrade(ctx, actor, "gapup")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if pv.TotalPages != 1 || pv.WouldRepublish != 1 {
		t.Fatalf("preview = %+v", pv)
	}
	// Start + get + run to completion.
	job, err := gen.StartUpgradeJob(ctx, actor, "gapup")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if len(job.Items) != 1 {
		t.Fatalf("job items = %d", len(job.Items))
	}
	if _, err := gen.GetUpgradeJob(ctx, authed("template.view"), job.ID); err != nil {
		t.Fatalf("get with view scope: %v", err)
	}
	if _, err := gen.GetUpgradeJob(ctx, authed("content.view"), job.ID); generation.CodeOf(err) != generation.CodePermissionDenied {
		t.Fatalf("get scope denied: %v", err)
	}
	done, err := gen.RunUpgradeJob(ctx, actor, job.ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if done.Status != generation.UpgradeJobCompleted {
		t.Fatalf("job status = %q", done.Status)
	}
	// Failed-item job: bump to v3 so the v1 page is pending again, start a
	// fresh job, then delete its row so the run records a failure → partial.
	cur2, err := tpls.GetCurrent(ctx, "gapup")
	if err != nil {
		t.Fatalf("GetCurrent2: %v", err)
	}
	upd2 := cur2.Input()
	upd2.Fields = append(upd2.Fields, models.TemplateField{Name: "sub2", Label: "S2", Type: "text", Required: false})
	if _, err := tpls.Update(ctx, cur2.TemplateID, cur2.Version, upd2); err != nil {
		t.Fatalf("Update template2: %v", err)
	}
	job2, err := gen.StartUpgradeJob(ctx, actor, "gapup")
	if err != nil {
		t.Fatalf("start2: %v", err)
	}
	if _, err := db.Collection("content").DeleteOne(ctx, bson.M{"_id": cid1}); err != nil {
		t.Fatalf("delete page1: %v", err)
	}
	part, err := gen.RunUpgradeJob(ctx, actor, job2.ID)
	if err != nil {
		t.Fatalf("run2: %v", err)
	}
	if part.Status != generation.UpgradeJobPartial {
		t.Fatalf("job2 status = %q, want partial", part.Status)
	}
}
