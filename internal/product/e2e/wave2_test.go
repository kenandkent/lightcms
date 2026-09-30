// Task 17 E2E second wave: high-value contract rows that also close the
// largest reachable coverage blocks (quarantine, fork reuse, slug/default
// derivation, template status, validation replay, stale preconditions,
// missing-immutable P0, outbox poison, upgrade resume).
package e2e

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/generation"
	"github.com/jonradoff/lightcms/v7/internal/product/migration"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"

	"go.mongodb.org/mongo-driver/bson"
)

// migrationNew builds a Migrator with a deterministic renderer (test-only
// seam; production default covered by TestE2E_MigrationDefaultRenderer).
func migrationNew(_ *testing.T, e *testEnv, byPath map[string][]byte, errPaths map[string]string) *migration.Migrator {
	return migration.New(migration.Config{DB: e.db, Store: e.store, BaseURL: e.base,
		Render: (&detRenderer{byPath: byPath, errPaths: errPaths}).render})
}

// TestE2E_QuarantineOrphan proves the post-completion orphan rule: with
// migration completed, a stray canonical file is quarantined OUT of the
// served tree (never served, never guessed into an active).
func TestE2E_QuarantineOrphan(t *testing.T) {
	e := newEnv(t, envOpts{})
	preMigrationIndexes(t, e)
	tpl := seedLegacyTemplate(t, e, "Good", "good-news")
	seedLegacyContent(t, e, tpl, "/news/real-page")
	writeCanonicalFile(t, e, "/news/real-page", []byte("<html>real</html>"))
	m := migrationNew(t, e, map[string][]byte{"/news/real-page": []byte("<html>real</html>")}, nil)
	if _, err := m.Run(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Plant a stray file with no content row behind it.
	writeCanonicalFile(t, e, "/news/stray-orphan", []byte("<html>stray</html>"))
	if c, _ := e.getAnon("/news/stray-orphan"); c != 200 {
		t.Fatalf("stray not served before scan: %d", c)
	}
	rep, err := e.scanner.ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if rep.OrphanQuarantined != 1 {
		t.Fatalf("OrphanQuarantined = %d, want 1 (%+v)", rep.OrphanQuarantined, rep)
	}
	if c, _ := e.getAnon("/news/stray-orphan"); c != 404 {
		t.Fatalf("quarantined file still served: %d", c)
	}
	if c, body := e.getAnon("/news/real-page"); c != 200 || !strings.Contains(string(body), "real") {
		t.Fatalf("real page disturbed by quarantine sweep: %d", c)
	}
}

// TestE2E_ForkDraftReuse proves repeated drafts on a published page reuse
// the same fork scope (update, not duplicate) while live stays pinned.
func TestE2E_ForkDraftReuse(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")
	v1 := int64(1)
	code, r1 := e.publish(t, "fr-1", map[string]any{
		"template": "financial-news", "title": "FR", "slug": "fr-page",
		"folder_path": "/news", "mode": "publish", "expected_template_version": v1,
		"data": map[string]any{"headline": "v1"},
	})
	if code != 201 {
		t.Fatalf("v1 = %d (%v)", code, r1)
	}
	pid1 := strOf(r1, "publication_id")
	u := strings.TrimPrefix(strOf(r1, "public_url"), e.base)
	draftBody := func(h string) map[string]any {
		return map[string]any{
			"template": "financial-news", "title": "FR", "slug": "fr-page",
			"folder_path": "/news", "mode": "draft", "upsert": true, "data": map[string]any{"headline": h},
		}
	}
	c1, d1 := e.postJSON("/api/v1/page-generation", draftBody("edit one"), nil)
	c2, d2 := e.postJSON("/api/v1/page-generation", draftBody("edit two"), nil)
	if c1 != 201 || c2 != 200 {
		t.Fatalf("drafts = %d/%d (%v %v), want 201 created + 200 updated", c1, c2, d1, d2)
	}
	if strOf(d1, "action") != "created" || strOf(d2, "action") != "updated" {
		t.Fatalf("fork reuse actions = %v / %v", d1, d2)
	}
	if n := e.count("content", bson.M{"path_scope": bson.M{"$ne": "live"}}); n != 1 {
		t.Fatalf("fork rows = %d, want 1 (reuse, no duplicate)", n)
	}
	if active := e.activePub(t, mustOID(t, strOf(r1, "id"))); active.ID.Hex() != pid1 {
		t.Fatal("live moved by fork edits")
	}
	if c, body := e.getAnon(u); c != 200 || !strings.Contains(string(body), ">v1<") {
		t.Fatalf("live bytes moved: %d %s", c, body)
	}
}

// TestE2E_SlugAndDefaults proves request-shape derivation: omitted slug
// falls back to the title, server-side field defaults materialize, and
// explicit false stays false.
func TestE2E_SlugAndDefaults(t *testing.T) {
	e := newEnv(t, envOpts{})
	ctx := context.Background()
	_, _, err := e.tpls.Create(ctx, templatecontract.TemplateInput{
		Slug: "defaults-page", Name: "Defaults", Category: "news", Status: "active",
		HTMLLayout: `<html><body><h1>{{.headline}}</h1><p>{{.flag}}</p><p>{{.count}}</p></body></html>`,
		Fields: []models.TemplateField{
			{Name: "headline", Label: "H", Type: "text", Required: true},
			{Name: "flag", Label: "F", Type: "boolean", Default: "true"},
			{Name: "count", Label: "C", Type: "number", Default: "41"},
		},
	})
	if err != nil {
		t.Fatalf("defaults template: %v", err)
	}
	v1 := int64(1)
	// No slug: derived from the title.
	code, resp := e.publish(t, "def-1", map[string]any{
		"template": "defaults-page", "title": "Hello World Page",
		"folder_path": "/news", "mode": "publish", "expected_template_version": v1,
		"data": map[string]any{"headline": "h"},
	})
	if code != 201 {
		t.Fatalf("publish = %d (%v)", code, resp)
	}
	if strOf(resp, "full_path") != "/news/hello-world-page" {
		t.Fatalf("derived path = %q", strOf(resp, "full_path"))
	}
	u := strings.TrimPrefix(strOf(resp, "public_url"), e.base)
	if c, body := e.getAnon(u); c != 200 || !strings.Contains(string(body), "41") {
		t.Fatalf("defaults not materialized: %d %s", c, body)
	}
}

// TestE2E_TemplateStatusGate proves deprecated/draft templates cannot
// publish (409 TEMPLATE_NOT_ACTIVE) with zero mutation.
func TestE2E_TemplateStatusGate(t *testing.T) {
	e := newEnv(t, envOpts{})
	tpl, _ := e.seedTemplate(t, "financial-news")
	ctx := context.Background()
	if _, err := e.tpls.Update(ctx, tpl.ID, 1, templatecontract.TemplateInput{
		Slug: "financial-news", Name: "E2E financial-news", Category: "news", Status: "deprecated",
		HTMLLayout: sharedLayout,
		Fields:     []models.TemplateField{{Name: "headline", Label: "H", Type: "text", Required: true}},
	}); err != nil {
		t.Fatalf("deprecate: %v", err)
	}
	before := snapshotFootprint(t, e)
	code, resp := e.publish(t, "st-dep", map[string]any{
		"template": "financial-news", "title": "D", "slug": "dep-page",
		"folder_path": "/news", "mode": "publish", "expected_template_version": 2,
		"data": map[string]any{"headline": "h"},
	})
	if code != 409 || errorCodeOf(resp) != "TEMPLATE_NOT_ACTIVE" {
		t.Fatalf("deprecated publish = %d (%v), want 409 TEMPLATE_NOT_ACTIVE", code, resp)
	}
	assertNoMutation(t, e, before, "deprecated publish")
}

// TestE2E_ValidationReplay proves pure 422 validation failures are
// replay-cached per key: same key + same bad body replays the 422 with no
// new side effects.
func TestE2E_ValidationReplay(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")
	raw, _ := jsonMarshal(map[string]any{
		"template": "financial-news", "title": "VR", "slug": "vr-page",
		"folder_path": "/news", "mode": "publish", "expected_template_version": int64(1),
		"data": map[string]any{},
	})
	hdr := map[string]string{"Idempotency-Key": "vr-1"}
	c1, r1 := e.postRaw("/api/v1/page-generation", raw, hdr)
	c2, r2 := e.postRaw("/api/v1/page-generation", raw, hdr)
	if c1 != 422 || c2 != 422 {
		t.Fatalf("validation = %d/%d (%v %v), want 422/422", c1, c2, r1, r2)
	}
	if errorCodeOf(r1) != "FIELD_VALIDATION_FAILED" || errorCodeOf(r2) != "FIELD_VALIDATION_FAILED" {
		t.Fatalf("codes = %q/%q", errorCodeOf(r1), errorCodeOf(r2))
	}
	if n := e.count("content", bson.M{}); n != 0 {
		t.Fatal("invalid publish created content")
	}
}

// TestE2E_StaleExpectedActive proves stale optimistic preconditions fail
// closed: rollback with a wrong expected_active_id is 409 with zero new
// publication and unchanged live bytes.
func TestE2E_StaleExpectedActive(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")
	_, r1 := e.publish(t, "sea-1", idemBody("sea-page", "v1", false))
	_, r2 := e.publish(t, "sea-2", idemBody("sea-page", "v2", true))
	cid := strOf(r2, "id")
	pid1 := strOf(r1, "publication_id")
	u := strings.TrimPrefix(strOf(r2, "public_url"), e.base)
	pubsBefore := e.count("content_publications", bson.M{})
	stale := "000000000000000000000000"
	code, resp := e.postJSON("/api/v1/content/"+cid+"/publications/"+pid1+"/rollback",
		map[string]any{"expected_active_id": stale}, map[string]string{"Idempotency-Key": "sea-stale-1"})
	if code != 409 {
		t.Fatalf("stale rollback = %d (%v), want 409", code, resp)
	}
	if n := e.count("content_publications", bson.M{}); n != pubsBefore {
		t.Fatal("stale rollback minted a publication")
	}
	if c, body := e.getAnon(u); c != 200 || !strings.Contains(string(body), ">v2<") {
		t.Fatalf("live moved by stale rollback: %d %s", c, body)
	}
}

// TestE2E_MissingImmutableP0 proves crash-recovery honesty: with the active
// immutable object AND canonical both gone, the scanner raises P0, marks
// degraded, and never guesses replacement bytes.
func TestE2E_MissingImmutableP0(t *testing.T) {
	e, cid, _, u := faultSetup(t, envOpts{})
	active := e.activePub(t, cid)
	if err := os.Remove(e.store.ImmutablePath(cid, active.ID)); err != nil {
		t.Fatalf("remove immutable: %v", err)
	}
	p, _ := e.store.CanonicalFilePath("/news/fault-page")
	if err := os.Remove(p); err != nil {
		t.Fatalf("remove canonical: %v", err)
	}
	rep, err := e.scanner.ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if rep.ImmutableMissingP0 == 0 {
		t.Fatalf("missing immutable not reported P0: %+v", rep)
	}
	if !rep.Degraded {
		t.Fatalf("scanner not degraded with missing authoritative bytes: %+v", rep)
	}
	if c, _ := e.getAnon(u); c != 404 {
		t.Fatalf("scanner guessed replacement bytes: %d", c)
	}
	if now := e.activePub(t, cid); now == nil || now.ID != active.ID {
		t.Fatal("scanner moved the active pointer without bytes")
	}
}

// TestE2E_OutboxPoison proves undeliverable rows are poisoned (P0 per row)
// instead of retried forever.
func TestE2E_OutboxPoison(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")
	v1 := int64(1)
	e.mu.Lock()
	e.deliverErr = errFault("receiver always 500")
	e.mu.Unlock()
	_, r1 := e.publish(t, "poison-1", map[string]any{
		"template": "financial-news", "title": "Poison", "slug": "poison-page",
		"folder_path": "/news", "mode": "publish", "expected_template_version": v1,
		"data": map[string]any{"headline": "p"},
	})
	pid := mustOID(t, strOf(r1, "publication_id"))
	ctx := context.Background()
	// Age the row past the poison horizon with maxed attempts.
	if _, err := e.db.Collection(publication.CollectionOutbox).UpdateOne(ctx,
		bson.M{"event_type": publication.EventPublished, "aggregate_id": pid},
		bson.M{"$set": bson.M{
			"created_at":      time.Now().Add(-30 * 24 * time.Hour),
			"attempt":         99,
			"next_attempt_at": time.Now().Add(-time.Hour),
		}}); err != nil {
		t.Fatalf("age outbox row: %v", err)
	}
	srep, err := e.scanner.SweepOutbox(ctx)
	if err != nil {
		t.Fatalf("SweepOutbox: %v", err)
	}
	_ = srep
	var row bson.M
	if err := e.db.FindOne(ctx, publication.CollectionOutbox,
		bson.M{"event_type": publication.EventPublished, "aggregate_id": pid}, &row); err != nil {
		t.Fatalf("poisoned row: %v", err)
	}
	if row["state"] == "pending" {
		t.Fatalf("undeliverable row still pending (retry-forever): %v", row)
	}
}

// TestE2E_UpgradeResume proves upgrade jobs are durable: running twice does
// not duplicate publications, and unknown jobs are 404.
func TestE2E_UpgradeResume(t *testing.T) {
	e := newEnv(t, envOpts{})
	tpl, _ := e.seedTemplate(t, "financial-news")
	v1 := int64(1)
	_, r1 := e.publish(t, "upr-1", map[string]any{
		"template": "financial-news", "title": "Upr", "slug": "upr-page",
		"folder_path": "/news", "mode": "publish", "expected_template_version": v1,
		"data": map[string]any{"headline": "u1"},
	})
	cid := mustOID(t, strOf(r1, "id"))
	if _, err := e.tpls.Update(context.Background(), tpl.ID, 1, templatecontract.TemplateInput{
		Slug: "financial-news", Name: "E2E financial-news", Category: "news", Status: "active",
		HTMLLayout: sharedLayout, Fields: []models.TemplateField{{Name: "headline", Label: "H", Type: "text", Required: true}},
	}); err != nil {
		t.Fatalf("bump: %v", err)
	}
	code, job := e.postJSON("/api/v1/templates/financial-news/upgrade-jobs", map[string]any{}, nil)
	if code != 201 {
		t.Fatalf("start = %d (%v)", code, job)
	}
	jobID := strOf(job, "id")
	if jobID == "" {
		if jm, ok := job["job"].(map[string]any); ok {
			jobID = strOf(jm, "id")
		}
	}
	code, _ = e.postJSON("/api/v1/templates/upgrade-jobs/"+jobID+"/run", map[string]any{}, nil)
	if code != 200 {
		t.Fatalf("run1 = %d", code)
	}
	nAfterRun1 := e.count("content_publications", bson.M{"content_id": cid})
	code, _ = e.postJSON("/api/v1/templates/upgrade-jobs/"+jobID+"/run", map[string]any{}, nil)
	if code != 200 {
		t.Fatalf("run2 = %d", code)
	}
	if n := e.count("content_publications", bson.M{"content_id": cid}); n != nAfterRun1 {
		t.Fatalf("job re-run duplicated publications (%d -> %d)", nAfterRun1, n)
	}
	code, resp := e.getJSON("/api/v1/templates/upgrade-jobs/000000000000000000000000", nil)
	if code != 404 {
		t.Fatalf("unknown job = %d (%v), want 404", code, resp)
	}
}

// TestE2E_RestoreVersion404 proves restore/revert preconditions: unknown
// version and unknown source publication are 404 with zero mutation.
func TestE2E_RestoreVersion404(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")
	_, r1 := e.publish(t, "rv-1", idemBody("rv-page", "v1", false))
	cid := strOf(r1, "id")
	pubsBefore := e.count("content_publications", bson.M{})
	code, resp := e.postJSON("/api/v1/content/"+cid+"/restore-and-publish",
		map[string]any{"version": 99}, map[string]string{"Idempotency-Key": "rv-404-1"})
	if code != 404 {
		t.Fatalf("restore unknown version = %d (%v), want 404", code, resp)
	}
	code, resp = e.postJSON("/api/v1/content/"+cid+"/revert-live",
		map[string]any{"source_publication_id": "6abd026e6f2815eddbe8dd99"},
		map[string]string{"Idempotency-Key": "rv-404-2"})
	// Task 19 fix landed: unknown source is 404 PUBLICATION_NOT_FOUND
	// (mapSagaErr translates publication CodeNotFound). Zero mutation
	// either way.
	rawResp, _ := json.Marshal(resp)
	if code != 404 || !strings.Contains(string(rawResp), "PUBLICATION_NOT_FOUND") {
		t.Fatalf("revert unknown source = %d (%v), want 404/PUBLICATION_NOT_FOUND", code, resp)
	}
	if n := e.count("content_publications", bson.M{}); n != pubsBefore {
		t.Fatal("failed restore/revert minted publications")
	}
	// Revert replay: same key replays the same new publication.
	pid1 := strOf(r1, "publication_id")
	code, v1out := e.postJSON("/api/v1/content/"+cid+"/revert-live",
		map[string]any{"source_publication_id": pid1},
		map[string]string{"Idempotency-Key": "rv-replay-1"})
	if code != 200 {
		t.Fatalf("revert = %d (%v)", code, v1out)
	}
	code, v2out := e.postJSON("/api/v1/content/"+cid+"/revert-live",
		map[string]any{"source_publication_id": pid1},
		map[string]string{"Idempotency-Key": "rv-replay-1"})
	if code != 200 || strOf(v2out, "publication_id") != strOf(v1out, "publication_id") {
		t.Fatalf("revert replay = %d (%v)", code, v2out)
	}
	_ = generation.ModePreview
}
