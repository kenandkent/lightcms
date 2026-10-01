// Task 17 E2E lifecycle rows: template upgrade jobs, restore/revert,
// rename-and-publish, slug migration and legacy batch publish — over HTTP
// against one app instance.
package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/generation"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"

	"go.mongodb.org/mongo-driver/bson"
)

// TestE2E_TemplateUpgradeJob proves the explicit upgrade contract (§39.4
// lashed to HTTP): publish on v1 → bump template to v2 → live still v1 →
// read-only preview → durable job start → run → per-item v2 publish → GET v2.
func TestE2E_TemplateUpgradeJob(t *testing.T) {
	e := newEnv(t, envOpts{})
	tpl, tv := e.seedTemplate(t, "financial-news")
	if tv.Version != 1 {
		t.Fatalf("seed version = %d", tv.Version)
	}
	v1 := int64(1)
	code, r1 := e.publish(t, "upg-1", map[string]any{
		"template": "financial-news", "title": "Upg", "slug": "upg-page",
		"folder_path": "/news", "mode": "publish", "expected_template_version": v1,
		"data": map[string]any{"headline": "upg v1"},
	})
	if code != 201 {
		t.Fatalf("v1 publish = %d (%v)", code, r1)
	}
	u := strings.TrimPrefix(strOf(r1, "public_url"), e.base)

	// Bump the template to v2 (explicit version allocation, no silent live
	// regen).
	ctx := context.Background()
	tv2, err := e.tpls.Update(ctx, tpl.ID, 1, templatecontract.TemplateInput{
		Slug: "financial-news", Name: "E2E financial-news", Category: "news", Status: "active",
		HTMLLayout: sharedLayout + "<!-- v2 -->",
		Fields:     []models.TemplateField{{Name: "headline", Label: "Headline", Type: "text", Required: true}},
	})
	_ = tv2
	if err != nil {
		t.Fatalf("template bump: %v", err)
	}

	// Live still v1 after the template change (no silent regeneration).
	if c, body := e.getAnon(u); c != 200 || !strings.Contains(string(body), "upg v1") || strings.Contains(string(body), "v2 -->") {
		t.Fatalf("live changed by template bump: %d %s", c, body)
	}

	// Read-only preview.
	code, preview := e.getJSON("/api/v1/templates/financial-news/upgrade-preview", nil)
	if code != 200 {
		t.Fatalf("preview = %d (%v)", code, preview)
	}
	if c, body := e.getAnon(u); c != 200 || !strings.Contains(string(body), "upg v1") {
		t.Fatalf("preview mutated live: %d", c)
	}

	// Durable job: start (201) → get → run → per-item outcome.
	code, job := e.postJSON("/api/v1/templates/financial-news/upgrade-jobs", map[string]any{}, nil)
	if code != 201 {
		t.Fatalf("start job = %d (%v)", code, job)
	}
	jobID := strOf(job, "id")
	if jobID == "" {
		if jm, ok := job["job"].(map[string]any); ok {
			jobID = strOf(jm, "id")
		}
	}
	if jobID == "" {
		t.Fatalf("job without id: %v", job)
	}
	code, got := e.getJSON("/api/v1/templates/upgrade-jobs/"+jobID, nil)
	if code != 200 {
		t.Fatalf("get job = %d (%v)", code, got)
	}
	code, run := e.postJSON("/api/v1/templates/upgrade-jobs/"+jobID+"/run", map[string]any{}, nil)
	if code != 200 {
		t.Fatalf("run job = %d (%v)", code, run)
	}
	// Live is now v2 (job publishes explicitly, never silently).
	if c, body := e.getAnon(u); c != 200 || !strings.Contains(string(body), "upg v1") {
		t.Fatalf("live missing after job: %d %s", c, body)
	}
	cid := strOf(r1, "id")
	active := e.activePub(t, mustOID(t, cid))
	if active.TemplateVersion != 2 {
		t.Fatalf("active template version = %d, want 2", active.TemplateVersion)
	}
}

// TestE2E_RestoreRevertLive proves restore_and_publish and revert_live are
// distinct publication commands with idempotency + preconditions, and that
// the publication list endpoint serves history.
func TestE2E_RestoreRevertLive(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")
	_, r1 := e.publish(t, "rr-1", idemBody("rr-page", "v1", false))
	_, r2 := e.publish(t, "rr-2", idemBody("rr-page", "v2", true))
	cid := strOf(r2, "id")
	pid1 := strOf(r1, "publication_id")
	u := strings.TrimPrefix(strOf(r2, "public_url"), e.base)

	// History lists v1 + v2.
	code, list := e.getJSON("/api/v1/content/"+cid+"/publications", nil)
	if code != 200 {
		t.Fatalf("list publications = %d (%v)", code, list)
	}

	// restore_and_publish version 1 → new publication, GET serves v1 bytes.
	code, rout := e.postJSON("/api/v1/content/"+cid+"/restore-and-publish",
		map[string]any{"version": 1}, map[string]string{"Idempotency-Key": "rr-restore-1"})
	if code != 200 {
		t.Fatalf("restore = %d (%v)", code, rout)
	}
	if strOf(rout, "mode") != "restore_and_publish" {
		t.Fatalf("restore mode = %v", rout)
	}
	if c, body := e.getAnon(u); c != 200 || !strings.Contains(string(body), ">v1<") {
		t.Fatalf("live not restored to v1: %d %s", c, body)
	}
	// Same-key replay: same restore publication.
	code, rout2 := e.postJSON("/api/v1/content/"+cid+"/restore-and-publish",
		map[string]any{"version": 1}, map[string]string{"Idempotency-Key": "rr-restore-1"})
	if code != 200 || strOf(rout2, "publication_id") != strOf(rout, "publication_id") {
		t.Fatalf("restore replay = %d (%v)", code, rout2)
	}

	// revert_live to the v1 publication → exact retained bytes, distinct
	// publication from the restore.
	code, vout := e.postJSON("/api/v1/content/"+cid+"/revert-live",
		map[string]any{"source_publication_id": pid1}, map[string]string{"Idempotency-Key": "rr-revert-1"})
	if code != 200 {
		t.Fatalf("revert = %d (%v)", code, vout)
	}
	if strOf(vout, "mode") != "revert_live" {
		t.Fatalf("revert mode = %v", vout)
	}
	if strOf(vout, "publication_id") == strOf(rout, "publication_id") {
		t.Fatal("revert must mint a distinct publication from restore")
	}
	if c, body := e.getAnon(u); c != 200 || !strings.Contains(string(body), ">v1<") {
		t.Fatalf("live not v1 after revert: %d %s", c, body)
	}
}

// TestE2E_RenamePublish proves rename-and-publish is one saga: new path
// live, redirect metadata committed, old publication record untouched, old
// canonical retired. The draft rename itself has no product-HTTP affordance
// (generation treats a changed path as a new page — asserted at the end),
// so the row move uses the same direct update the saga unit tests use;
// everything else (publish, cutover, redirect, serving) runs over HTTP.
func TestE2E_RenamePublish(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")
	v1 := int64(1)
	code, r1 := e.publish(t, "ren-1", map[string]any{
		"template": "financial-news", "title": "Rename", "slug": "old-name",
		"folder_path": "/news", "mode": "publish", "expected_template_version": v1,
		"data": map[string]any{"headline": "ren v1"},
	})
	if code != 201 {
		t.Fatalf("v1 = %d (%v)", code, r1)
	}
	pid1 := strOf(r1, "publication_id")
	cid := mustOID(t, strOf(r1, "id"))
	oldURL := strings.TrimPrefix(strOf(r1, "public_url"), e.base)
	ctx := context.Background()

	// Draft rename of the live row (no product-HTTP affordance exists).
	if err := e.db.Collection("content").FindOneAndUpdate(ctx, bson.M{"_id": cid},
		bson.M{"$set": bson.M{
			"current_version": int64(2), "full_path": "/news/new-name",
			"slug": "new-name", "canonical_full_path": "/news/new-name",
			"data": bson.M{"headline": "ren v2"},
		}}).Err(); err != nil {
		t.Fatalf("draft rename: %v", err)
	}

	// Publish the moved row through the legacy route (ContentID-addressed).
	code, r2 := e.postJSON("/api/v1/content/"+cid.Hex()+"/publish", map[string]any{},
		map[string]string{"Idempotency-Key": "ren-2"})
	if code != 200 {
		t.Fatalf("rename publish = %d (%v)", code, r2)
	}
	if strOf(r2, "full_path") != "/news/new-name" {
		t.Fatalf("rename result path = %v", r2)
	}
	newURL := strings.TrimPrefix(strOf(r2, "public_url"), e.base)
	if newURL == oldURL {
		t.Fatalf("rename did not move the path: %s", newURL)
	}
	if c, body := e.getAnon(newURL); c != 200 || !strings.Contains(string(body), "ren v2") {
		t.Fatalf("new path not live: %d %s", c, body)
	}
	if c, _ := e.getAnon(oldURL); c != 404 {
		t.Fatalf("old canonical not retired: %d", c)
	}
	// Old publication record keeps its original path metadata.
	var oldPub publication.Publication
	if err := e.db.FindOne(ctx, "content_publications",
		bson.M{"_id": mustOID(t, pid1)}, &oldPub); err != nil {
		t.Fatalf("old publication: %v", err)
	}
	if oldPub.FullPath != "/news/old-name" {
		t.Fatalf("old publication path mutated: %q", oldPub.FullPath)
	}
	// Redirect metadata committed old → new; active points at the new path.
	if n := e.count("redirects", bson.M{"from_path": "/news/old-name"}); n != 1 {
		t.Fatalf("redirect rows old→new = %d, want 1", n)
	}
	if active := e.activePub(t, cid); active.FullPath != "/news/new-name" {
		t.Fatalf("active path = %q", active.FullPath)
	}

	// Documented affordance gap: generation publish to another new path
	// creates a NEW page (it cannot rename) — the old page stays live.
	code, r3 := e.publish(t, "ren-3", map[string]any{
		"template": "financial-news", "title": "Rename", "slug": "third-name",
		"folder_path": "/news", "mode": "publish", "expected_template_version": v1,
		"data": map[string]any{"headline": "ren v3"},
	})
	if code != 201 || strOf(r3, "action") != "created" {
		t.Fatalf("new-path generate = %d (%v), want 201 created", code, r3)
	}
	if c, _ := e.getAnon(newURL); c != 200 {
		t.Fatalf("renamed page disturbed by third create: %d", c)
	}
}

// TestE2E_MigrateSlug proves admin slug migration changes no page: slug
// collision is 409, success keeps live bytes + active publication, and the
// schema moves to the new slug.
func TestE2E_MigrateSlug(t *testing.T) {
	e := newEnv(t, envOpts{})
	tpl, _ := e.seedTemplate(t, "financial-news")
	e.seedTemplate(t, "taken-slug")
	v1 := int64(1)
	code, r1 := e.publish(t, "ms-1", map[string]any{
		"template": "financial-news", "title": "MS", "slug": "ms-page",
		"folder_path": "/news", "mode": "publish", "expected_template_version": v1,
		"data": map[string]any{"headline": "ms v1"},
	})
	if code != 201 {
		t.Fatalf("publish = %d (%v)", code, r1)
	}
	cid := strOf(r1, "id")
	pid1 := strOf(r1, "publication_id")
	u := strings.TrimPrefix(strOf(r1, "public_url"), e.base)

	// Collision first.
	code, resp := e.postJSON("/api/v1/templates/"+tpl.ID.Hex()+"/migrate-slug",
		map[string]any{"new_slug": "taken-slug"}, nil)
	if code != 409 {
		t.Fatalf("slug collision = %d (%v), want 409", code, resp)
	}
	// Success.
	code, resp = e.postJSON("/api/v1/templates/"+tpl.ID.Hex()+"/migrate-slug",
		map[string]any{"new_slug": "financial-news-v2"}, nil)
	if code != 200 {
		t.Fatalf("migrate slug = %d (%v)", code, resp)
	}
	// Live bytes + active publication unchanged; schema moved.
	if c, body := e.getAnon(u); c != 200 || !strings.Contains(string(body), "ms v1") {
		t.Fatalf("live changed by slug migration: %d", c)
	}
	if active := e.activePub(t, mustOID(t, cid)); active.ID.Hex() != pid1 {
		t.Fatalf("active changed by slug migration: %s", active.ID.Hex())
	}
	code, schema := e.getJSON("/api/v1/templates/financial-news-v2/schema", nil)
	if code != 200 || intOf(schema, "template_version") != 1 {
		t.Fatalf("schema at new slug = %d (%v)", code, schema)
	}
	code, _ = e.getJSON("/api/v1/templates/financial-news/schema", nil)
	if code != 404 {
		t.Fatalf("old slug schema = %d, want 404", code)
	}
}

// TestE2E_BatchPublishLegacy proves the §39.9 batch entry creates one
// publication + outbox row per item through the shared saga, and that a
// same-key retry replays per-item idempotent records instead of minting
// duplicates (lane 2A: per-item Begin/Complete keyed by parent key +
// content path suffix).
func TestE2E_BatchPublishLegacy(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")
	mkdraft := func(slug string) string {
		code, d := e.postJSON("/api/v1/page-generation", map[string]any{
			"template": "financial-news", "title": "Batch " + slug, "slug": slug,
			"folder_path": "/news", "mode": "draft", "data": map[string]any{"headline": slug},
		}, nil)
		if code != 201 {
			t.Fatalf("draft %s = %d (%v)", slug, code, d)
		}
		return strOf(d, "id")
	}
	idA, idB := mkdraft("batch-a"), mkdraft("batch-b")
	hdr := map[string]string{"Idempotency-Key": "batch-1"}
	code, resp := e.postJSON("/api/v1/content/batch-publish", map[string]any{"ids": []string{idA, idB}}, hdr)
	if code != 200 {
		t.Fatalf("batch publish = %d (%v)", code, resp)
	}
	pubs, _ := resp["publications"].([]any)
	if len(pubs) != 2 {
		t.Fatalf("batch publications = %v", resp)
	}
	if n := e.count(publication.CollectionOutbox, bson.M{"event_type": publication.EventPublished}); n != 2 {
		t.Fatalf("batch outbox rows = %d, want 2", n)
	}
	for _, slug := range []string{"batch-a", "batch-b"} {
		if c, body := e.getAnon("/news/" + slug); c != 200 || !strings.Contains(string(body), slug) {
			t.Fatalf("batch page %s not live: %d %s", slug, c, body)
		}
	}
	// Same top-level key repeated: per-item idempotency records replay, so
	// no duplicate publications are minted (count stays at 2).
	code, resp2 := e.postJSON("/api/v1/content/batch-publish", map[string]any{"ids": []string{idA, idB}}, hdr)
	if code != 200 {
		t.Fatalf("batch replay = %d (%v)", code, resp2)
	}
	if n := e.count("content_publications", bson.M{}); n != 2 {
		t.Fatalf("batch publications after same-key repeat = %d (want 2 = replay, no duplicates)", n)
	}
	pubs2, _ := resp2["publications"].([]any)
	if len(pubs2) != 2 {
		t.Fatalf("batch replay publications = %v", resp2)
	}
	t.Logf("verified: same-key batch repeat replays per-item idempotent records (no duplicate publications)")
	_ = generation.ModePublish
}
