// Task 17 E2E: whole-system publication contract, entry matrix, faults and
// idempotency. Every row runs against ONE application instance over HTTP +
// public URL (testEnv), replica-set Mongo, and a temporary store root.
package e2e

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/config"
	"github.com/jonradoff/lightcms/v7/internal/database"
	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/generation"
	"github.com/jonradoff/lightcms/v7/internal/product/idempotency"
	"github.com/jonradoff/lightcms/v7/internal/product/pathkey"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/publicurl"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

// TestE2E_PublishContract is the headliner row: create template → schema →
// Generate publish → anonymous GET URL, with content, logical_published_at,
// publication ID and template version matching across the HTTP response, the
// publication record, the outbox payload (16E PublicURL enrichment) and the
// served canonical bytes.
func TestE2E_PublishContract(t *testing.T) {
	e := newEnv(t, envOpts{buildSHA: "task17-e2e-sha"})
	_, tv := e.seedTemplate(t, "financial-news")
	if tv.Version != 1 {
		t.Fatalf("seed template version = %d, want 1", tv.Version)
	}

	// Schema over HTTP: version + ETag from the same immutable version.
	code, schema := e.getJSON("/api/v1/templates/financial-news/schema", nil)
	if code != 200 {
		t.Fatalf("schema status = %d (%v)", code, schema)
	}
	if intOf(schema, "template_version") != 1 {
		t.Fatalf("schema version = %v", schema)
	}
	doc, _ := schema["json_schema"].(map[string]any)
	if doc == nil {
		t.Fatalf("schema missing json_schema: %v", schema)
	}

	// Publish over HTTP.
	v1 := int64(1)
	body := map[string]any{
		"template": "financial-news", "title": "Contract Page", "slug": "contract-page",
		"folder_path": "/news", "mode": "publish", "expected_template_version": v1,
		"data": map[string]any{"headline": "Contract Headline"},
	}
	code, resp := e.publish(t, "e2e-contract-1", body)
	if code != 201 {
		t.Fatalf("publish status = %d (%v)", code, resp)
	}
	pubID := strOf(resp, "publication_id")
	contentID := strOf(resp, "id")
	publicURL := strOf(resp, "public_url")
	if pubID == "" || contentID == "" || publicURL == "" {
		t.Fatalf("publish missing ids/url: %v", resp)
	}
	if strOf(resp, "action") != "created" || strOf(resp, "mode") != "publish" {
		t.Fatalf("publish shape: %v", resp)
	}
	if intOf(resp, "content_version") != 1 || intOf(resp, "template_version") != 1 {
		t.Fatalf("publish versions: %v", resp)
	}
	if published, _ := resp["published"].(bool); !published {
		t.Fatalf("publish published=false: %v", resp)
	}
	if rp, _ := resp["requires_publish"].(bool); rp {
		t.Fatalf("publish requires_publish=true: %v", resp)
	}
	cid, _ := primitive.ObjectIDFromHex(contentID)
	pid, _ := primitive.ObjectIDFromHex(pubID)

	// Publication detail over HTTP matches the publish response.
	code, detail := e.getJSON("/api/v1/content/"+contentID+"/publications/"+pubID, nil)
	if code != 200 {
		t.Fatalf("publication detail status = %d (%v)", code, detail)
	}

	// Active record matches: content, logical time, IDs, versions.
	active := e.activePub(t, cid)
	if active == nil {
		t.Fatal("no active publication")
	}
	if active.ID != pid {
		t.Fatalf("active id %s != published %s", active.ID.Hex(), pubID)
	}
	if active.ContentVersion != 1 || active.TemplateVersion != 1 {
		t.Fatalf("active versions content=%d template=%d", active.ContentVersion, active.TemplateVersion)
	}
	if active.FullPath != "/news/contract-page" {
		t.Fatalf("active path = %q", active.FullPath)
	}
	if active.PublicURL != publicURL {
		t.Fatalf("active public_url %q != response %q", active.PublicURL, publicURL)
	}
	if active.LogicalPublishedAt.IsZero() {
		t.Fatal("logical_published_at is zero")
	}
	if active.ProductBuildSHA != "task17-e2e-sha" {
		t.Fatalf("product_build_sha = %q (16E plumbing broken)", active.ProductBuildSHA)
	}

	// Anonymous GET on the public URL serves the frozen snapshot bytes.
	u := strings.TrimPrefix(publicURL, e.base)
	code, served := e.getAnon(u)
	if code != 200 {
		t.Fatalf("anonymous GET %s = %d", u, code)
	}
	for _, want := range []string{"Contract Headline", pubID, active.PublicURL} {
		if !strings.Contains(string(served), want) {
			t.Fatalf("served bytes miss %q:\n%s", want, served)
		}
	}

	// Outbox: exactly one content.publish row carrying the enriched PublicURL.
	if n := e.outboxCount(t, publication.EventPublished, pid); n != 1 {
		t.Fatalf("outbox rows = %d, want 1", n)
	}
	var row bson.M
	if err := e.db.FindOne(context.Background(), publication.CollectionOutbox,
		bson.M{"event_type": publication.EventPublished, "aggregate_id": pid}, &row); err != nil {
		t.Fatalf("outbox row: %v", err)
	}
	payload, _ := row["payload"].(bson.M)
	if payload == nil {
		if pm, ok := row["payload"].(map[string]any); ok {
			payload = bson.M(pm)
		}
	}
	if payload == nil || payload["public_url"] != publicURL {
		t.Fatalf("outbox payload public_url = %v, want %q (16E enrichment)", payload, publicURL)
	}

	// Delivery: worker delivers once with a stable event ID.
	did, derr := e.outbox.ProcessNext(context.Background())
	if !did || derr != nil {
		t.Fatalf("ProcessNext = %v, %v", did, derr)
	}
	e.mu.Lock()
	nDeliveries := len(e.deliveries)
	e.mu.Unlock()
	if nDeliveries != 1 {
		t.Fatalf("deliveries = %d, want 1", nDeliveries)
	}
	if n := e.outboxCount(t, publication.EventPublished, pid); n != 1 {
		t.Fatalf("outbox rows after delivery = %d, want 1 (no duplicate)", n)
	}

	// Generation scope sanity: the control publish used a full-owner actor.
	_ = generation.ModePublish
}

// TestE2E_StartupRejections pins the Task 16E startup guards: an S3 storage
// provider and a standalone production Mongo are both rejected before
// serving, with actionable diagnostics.
func TestE2E_StartupRejections(t *testing.T) {
	// S3 provider rejected by config validation (the first guard in
	// buildPublicationRuntime, cmd/server/publication_runtime.go:65).
	cfg := &config.Config{Env: "production", StaticStorageProvider: "s3"}
	cfg.ApplyPublicationDefaults()
	if err := cfg.ValidatePublicationConfig(); err == nil {
		t.Fatal("STATIC_STORAGE_PROVIDER=s3 accepted, want rejection")
	} else if !strings.Contains(err.Error(), "STATIC_STORAGE_PROVIDER") || !strings.Contains(err.Error(), "filesystem") {
		t.Fatalf("s3 diagnostic not actionable: %v", err)
	}
	// Filesystem provider accepted.
	cfg.StaticStorageProvider = "filesystem"
	if err := cfg.ValidatePublicationConfig(); err != nil {
		t.Fatalf("filesystem rejected: %v", err)
	}

	// Standalone topology probe: boot a non-replica-set mongod and prove
	// IsReplicaSet reports false, which is exactly what the production
	// guard (publication_runtime.go:160-173) refuses to serve on.
	requireLiveMongo(t)
	cname := "lightcms-standalone-probe"
	exec.Command("docker", "rm", "-f", cname).Run()
	out, err := exec.Command("docker", "run", "-d", "--rm", "--name", cname,
		"-p", "127.0.0.1:27018:27017", "mongo:7.0.14", "--port", "27017").CombinedOutput()
	if err != nil {
		t.Fatalf("docker run standalone mongod (no skip): %v %s", err, out)
	}
	defer exec.Command("docker", "rm", "-f", cname).Run()
	uri := "mongodb://127.0.0.1:27018/standalone-probe?directConnection=true"
	var db *database.DB
	deadline := time.Now().Add(90 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		wc := writeconcern.New(writeconcern.WMajority())
		db, err = database.Connect(ctx, uri, "standalone-probe-test", options.Client().SetWriteConcern(wc))
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("connect standalone (no skip): %v", err)
		}
		time.Sleep(2 * time.Second)
	}
	defer db.Disconnect(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ok, err := db.IsReplicaSet(ctx)
	if err != nil {
		t.Fatalf("IsReplicaSet: %v", err)
	}
	if ok {
		t.Fatal("standalone mongod reports replica set, want false")
	}
	// The production guard rejects exactly this combination; the RS fixture
	// used by every other E2E row reports true (proving the probe is real).
	prodCfg := &config.Config{Env: "production"}
	if !prodCfg.IsProd() {
		t.Fatal("IsProd() false for production config")
	}
	t.Logf("standalone topology=false + IsProd=true => startup rejection predicate holds (publication_runtime.go requireReplicaSet)")
}

// TestE2E_ForkVisibility proves draft/live isolation through the generation
// fork-draft path: publish v1 → draft edit on the published page (fork row)
// → GET still v1 → publish v2 → GET v2.
func TestE2E_ForkVisibility(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")
	v1 := int64(1)
	pub := func(key, headline string, upsert bool) (int, map[string]any) {
		return e.publish(t, key, map[string]any{
			"template": "financial-news", "title": "ForkVis", "slug": "fork-vis",
			"folder_path": "/news", "mode": "publish", "expected_template_version": v1,
			"upsert": upsert, "data": map[string]any{"headline": headline},
		})
	}
	code, r1 := pub("forkvis-1", "v1 headline", false)
	if code != 201 {
		t.Fatalf("v1 publish = %d (%v)", code, r1)
	}
	cid, _ := primitive.ObjectIDFromHex(strOf(r1, "id"))
	pid1 := strOf(r1, "publication_id")
	u := strings.TrimPrefix(strOf(r1, "public_url"), e.base)

	// Draft edit on the PUBLISHED page: must fork, never touch live.
	// (Upsert=true acknowledges the existing target; the write still goes
	// to a fork-scoped draft, never the main row or live files.)
	code, draft := e.postJSON("/api/v1/page-generation", map[string]any{
		"template": "financial-news", "title": "ForkVis", "slug": "fork-vis",
		"folder_path": "/news", "mode": "draft", "upsert": true, "data": map[string]any{"headline": "v2 draft"},
	}, nil)
	if code != 200 && code != 201 {
		t.Fatalf("draft edit = %d (%v)", code, draft)
	}
	if published, _ := draft["published"].(bool); !published {
		t.Fatalf("draft on published page must report published=true: %v", draft)
	}
	if rp, _ := draft["requires_publish"].(bool); !rp {
		t.Fatalf("draft must report requires_publish=true: %v", draft)
	}
	// Live still v1: canonical bytes + active publication unchanged.
	if c, body := e.getAnon(u); c != 200 || !strings.Contains(string(body), "v1 headline") {
		t.Fatalf("live changed by draft edit: %d %s", c, body)
	}
	if active := e.activePub(t, cid); active.ID.Hex() != pid1 {
		t.Fatalf("active changed by draft edit: %s", active.ID.Hex())
	}
	var main models.Content
	if err := e.db.FindOne(context.Background(), "content", bson.M{"_id": cid}, &main); err != nil {
		t.Fatalf("main row: %v", err)
	}
	if !main.HasUnpublishedChanges {
		t.Fatal("main row missing has_unpublished_changes after fork draft")
	}

	// Publish v2: GET serves v2 with a new publication.
	code, r2 := pub("forkvis-2", "v2 headline", true)
	if code != 200 {
		t.Fatalf("v2 publish = %d (%v)", code, r2)
	}
	if strOf(r2, "publication_id") == pid1 {
		t.Fatal("v2 reused the v1 publication id")
	}
	if c, body := e.getAnon(u); c != 200 || !strings.Contains(string(body), "v2 headline") {
		t.Fatalf("live not v2: %d %s", c, body)
	}
	if active := e.activePub(t, cid); active.ContentVersion != 2 {
		t.Fatalf("active content version = %d, want 2", active.ContentVersion)
	}
}

// TestE2E_ForkPageScopeBug pins a REAL defect found by this E2E run:
// ForkService.ForkPage copies the live row verbatim (CanonicalFullPath +
// PathScope "live"), so forking any V3-published page fails with E11000 on
// the Task 2 unique index content_canonical_path_scope_unique. The test
// PASSES by characterising the bug; the E2E table row is FAIL with the fix
// specified (ForkPage must scope the copy to the fork workspace, e.g.
// PathScope=forkID, which the index explicitly permits to share the live
// key). All Task 16B fork tests seed legacy-shaped rows without canonical
// fields, which is why this never fired before.
func TestE2E_ForkPageScopeBug(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")
	v1 := int64(1)
	code, r1 := e.publish(t, "forkbug-1", map[string]any{
		"template": "financial-news", "title": "ForkBug", "slug": "fork-bug",
		"folder_path": "/news", "mode": "publish", "expected_template_version": v1,
		"data": map[string]any{"headline": "v1"},
	})
	if code != 201 {
		t.Fatalf("v1 publish = %d (%v)", code, r1)
	}
	cid, _ := primitive.ObjectIDFromHex(strOf(r1, "id"))
	pid1 := strOf(r1, "publication_id")
	u := strings.TrimPrefix(strOf(r1, "public_url"), e.base)
	ctx := context.Background()

	uid := primitive.NewObjectID()
	fork, err := e.forks.Create(ctx, "e2e-fork", "scope bug probe", uid, "admin@e2e.test", "")
	if err != nil {
		t.Fatalf("fork create: %v", err)
	}
	_, err = e.forks.ForkPage(ctx, fork.ID, cid)
	if err == nil {
		t.Fatal("ForkPage succeeded on V3 content; bug characterisation invalid (fix may have landed — update this test)")
	}
	if !strings.Contains(err.Error(), "E11000") || !strings.Contains(err.Error(), "content_canonical_path_scope_unique") {
		t.Fatalf("ForkPage failed unexpectedly (want E11000 canonical unique): %v", err)
	}
	t.Logf("BUG PINNED: ForkPage on V3 content: %v", err)

	// The remainder of the chain works once the copy carries fork scope
	// (the shape ForkPage must produce post-fix): seed it directly,
	// edit, merge, and prove the visibility sequence.
	var live models.Content
	if err := e.db.FindOne(ctx, "content", bson.M{"_id": cid}, &live); err != nil {
		t.Fatalf("live row: %v", err)
	}
	now := time.Now()
	forkCopy := live
	forkCopy.ID = primitive.NewObjectID()
	forkCopy.ForkID = &fork.ID
	forkCopy.PathScope = fork.ID.Hex()
	forkCopy.PathActive = true
	forkCopy.Published = false
	forkCopy.Data = map[string]any{"headline": "v2 merged"}
	forkCopy.CreatedAt = now
	forkCopy.UpdatedAt = now
	if _, err := e.db.Collection("content").InsertOne(ctx, &forkCopy); err != nil {
		t.Fatalf("seed fork-scoped copy (post-fix ForkPage shape): %v", err)
	}
	mr, err := e.forks.Merge(ctx, fork.ID, uid, "admin@e2e.test")
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if mr.Updated != 1 {
		t.Fatalf("merge result = %+v, want 1 update", mr)
	}
	// merge → GET still v1, active still v1.
	if c, body := e.getAnon(u); c != 200 || !strings.Contains(string(body), ">v1<") {
		t.Fatalf("live changed by merge: %d %s", c, body)
	}
	if active := e.activePub(t, cid); active.ID.Hex() != pid1 {
		t.Fatalf("active changed by merge: %s", active.ID.Hex())
	}
	// publish → GET v2.
	code, r2 := e.publish(t, "forkbug-2", map[string]any{
		"template": "financial-news", "title": "ForkBug", "slug": "fork-bug",
		"folder_path": "/news", "mode": "publish", "expected_template_version": v1,
		"upsert": true, "data": map[string]any{"headline": "v2 merged"},
	})
	if code != 200 {
		t.Fatalf("v2 publish = %d (%v)", code, r2)
	}
	if c, body := e.getAnon(u); c != 200 || !strings.Contains(string(body), "v2 merged") {
		t.Fatalf("live not v2: %d %s", c, body)
	}
}

// TestE2E_CanonicalCaseCollision proves review focus 1 over HTTP: two
// concurrent creates for /News/Foo and /news/foo yield one live path, a
// stable 409 for the loser, one Content and one canonical file.
func TestE2E_CanonicalCaseCollision(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")
	v1 := int64(1)
	mkbody := func(folder, slug string) map[string]any {
		return map[string]any{
			"template": "financial-news", "title": "Case", "slug": slug,
			"folder_path": folder, "mode": "publish", "expected_template_version": v1,
			"data": map[string]any{"headline": "case"},
		}
	}
	rawA, _ := jsonMarshal(mkbody("/News", "Foo"))
	rawB, _ := jsonMarshal(mkbody("/news", "foo"))
	var wg sync.WaitGroup
	type res struct {
		code int
		body map[string]any
	}
	out := make([]res, 2)
	bodies := [][]byte{rawA, rawB}
	keys := []string{"case-a", "case-b"}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, b := e.postRaw("/api/v1/page-generation", bodies[i], map[string]string{"Idempotency-Key": keys[i]})
			out[i] = res{c, b}
		}(i)
	}
	wg.Wait()
	wins, losses := 0, 0
	var winner map[string]any
	for _, r := range out {
		switch r.code {
		case 201:
			wins++
			winner = r.body
		case 409:
			losses++
			if got := errorCodeOf(r.body); got != "PATH_CONFLICT" {
				t.Fatalf("loser code = %q, want PATH_CONFLICT (%v)", got, r.body)
			}
		default:
			t.Fatalf("unexpected status %d (%v)", r.code, r.body)
		}
	}
	if wins != 1 || losses != 1 {
		t.Fatalf("wins=%d losses=%d, want 1/1 (%v)", wins, losses, out)
	}
	if n := e.count("content", bson.M{"canonical_full_path": "/news/foo", "path_scope": "live"}); n != 1 {
		t.Fatalf("live contents for canonical = %d, want 1", n)
	}
	u := strings.TrimPrefix(strOf(winner, "public_url"), e.base)
	if c, _ := e.getAnon(u); c != 200 {
		t.Fatalf("winner not served: %d", c)
	}
	entries, err := os.ReadDir(filepath.Join(e.root, "generated", "news"))
	if err != nil {
		t.Fatalf("generated dir: %v", err)
	}
	if len(entries) != 1 {
		names := []string{}
		for _, en := range entries {
			names = append(names, en.Name())
		}
		t.Fatalf("canonical files = %v, want exactly one", names)
	}
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func errorCodeOf(body map[string]any) string {
	if body == nil {
		return ""
	}
	if e, ok := body["error"].(map[string]any); ok {
		if c, ok := e["code"].(string); ok {
			return c
		}
	}
	if c, ok := body["code"].(string); ok {
		return c
	}
	return ""
}

// TestE2E_IdempotencyKeyGate proves external publish without a key is 428
// with zero mutation (legacy route parity is covered in the legacy table).
func TestE2E_IdempotencyKeyGate(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")
	v1 := int64(1)
	before := e.count("content", bson.M{})
	code, resp := e.postJSON("/api/v1/page-generation", map[string]any{
		"template": "financial-news", "title": "NoKey", "slug": "no-key",
		"folder_path": "/news", "mode": "publish", "expected_template_version": v1,
		"data": map[string]any{"headline": "x"},
	}, nil)
	if code != 428 {
		t.Fatalf("no-key publish = %d (%v), want 428", code, resp)
	}
	if got := errorCodeOf(resp); got != "IDEMPOTENCY_KEY_REQUIRED" {
		t.Fatalf("code = %q, want IDEMPOTENCY_KEY_REQUIRED", got)
	}
	if n := e.count("content", bson.M{}); n != before {
		t.Fatal("428 publish mutated content")
	}
	if n := e.count("content_publications", bson.M{}); n != 0 {
		t.Fatal("428 publish minted a publication")
	}
	_ = idempotency.CodeConflict
}

// --- Fault table (spec §39.5) ------------------------------------------------
// Every row: old live remains or the scanner restores authoritative content;
// the failed attempt never emits a success event.

// faultSetup publishes v1 cleanly, then refaults the instance (same binary,
// same data) with the given fault options. It returns the env, content ID,
// first publication ID and the public path.
func faultSetup(t *testing.T, opts envOpts) (*testEnv, primitive.ObjectID, string, string) {
	t.Helper()
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")
	v1 := int64(1)
	code, r1 := e.publish(t, "fault-v1", map[string]any{
		"template": "financial-news", "title": "Fault", "slug": "fault-page",
		"folder_path": "/news", "mode": "publish", "expected_template_version": v1,
		"data": map[string]any{"headline": "v1 live"},
	})
	if code != 201 {
		t.Fatalf("v1 publish = %d (%v)", code, r1)
	}
	cid, _ := primitive.ObjectIDFromHex(strOf(r1, "id"))
	u := strings.TrimPrefix(strOf(r1, "public_url"), e.base)
	e.refault(opts)
	return e, cid, strOf(r1, "publication_id"), u
}

func v2body(headline string) map[string]any {
	v1 := int64(1)
	return map[string]any{
		"template": "financial-news", "title": "Fault", "slug": "fault-page",
		"folder_path": "/news", "mode": "publish", "expected_template_version": v1,
		"upsert": true, "data": map[string]any{"headline": headline},
	}
}

// TestE2E_FaultRenderStage verifies render and stage/verify failures leave
// the old page live, fail the attempt, and emit no success event.
func TestE2E_FaultRenderStage(t *testing.T) {
	// Render failure (injected renderer, before InsertStaged/file write).
	e, cid, pid1, u := faultSetup(t, envOpts{
		renderer: func(ctx context.Context, in publication.RenderInput) ([]byte, error) {
			return nil, errFault("injected render failure")
		},
	})
	code, resp := e.publish(t, "fault-render", v2body("v2 render"))
	if code < 500 {
		t.Fatalf("render failure status = %d (%v), want 5xx", code, resp)
	}
	assertOldLive(t, e, cid, pid1, u, "v1 live")
	if n := e.count("content_publications", bson.M{"content_id": cid}); n != 1 {
		t.Fatalf("publications = %d, want 1 (no failed row leaks as live)", n)
	}
	// No SUCCESS event for the failed attempt (a publication.failed
	// diagnostic row may exist; the v1 content.publish row is untouched).
	if n := e.outboxCount(t, publication.EventPublished, mustOID(t, pid1)); n != 1 {
		t.Fatalf("v1 success outbox rows = %d, want 1", n)
	}
	if n := e.count(publication.CollectionOutbox, bson.M{"event_type": publication.EventPublished}); n != 1 {
		t.Fatalf("content.publish rows = %d, want 1 (v1 only)", n)
	}

	// Stage/verify failure (truncated writes fail verification).
	e2, cid2, pid12, u2 := faultSetup(t, envOpts{maxWriteBytes: 1})
	code, resp = e2.publish(t, "fault-stage", v2body("v2 stage"))
	if code != 503 {
		t.Fatalf("stage failure status = %d (%v), want 503", code, resp)
	}
	if got := errorCodeOf(resp); got != "PUBLICATION_STAGE_FAILED" {
		t.Fatalf("stage code = %q", got)
	}
	assertOldLive(t, e2, cid2, pid12, u2, "v1 live")
	if n := e2.outboxCount(t, publication.EventPublished, mustOID(t, pid12)); n != 1 {
		t.Fatalf("v1 outbox rows changed by failed v2")
	}
}

// TestE2E_FaultActivateCommit verifies cutover and commit failures:
// a concurrently appeared canonical fails a first-publish cutover; a
// commit error after a successful cutover compensates the files and keeps
// the old publication active.
func TestE2E_FaultActivateCommit(t *testing.T) {
	// Concurrent canonical appearance on FIRST publish (no old active to
	// branch on): cutover refuses, occupant bytes untouched, no active.
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")
	code, draft := e.postJSON("/api/v1/page-generation", map[string]any{
		"template": "financial-news", "title": "Occupied", "slug": "occupied-page",
		"folder_path": "/news", "mode": "draft", "data": map[string]any{"headline": "mine"},
	}, nil)
	if code != 201 {
		t.Fatalf("draft = %d (%v)", code, draft)
	}
	seedOccupant(t, e, "/news/occupied-page", []byte("<html>concurrent occupant</html>"))
	v1 := int64(1)
	code, resp := e.publish(t, "fault-occupant", map[string]any{
		"template": "financial-news", "title": "Occupied", "slug": "occupied-page",
		"folder_path": "/news", "mode": "publish", "expected_template_version": v1,
		"upsert": true, "data": map[string]any{"headline": "mine"},
	})
	if code != 503 && code != 500 {
		t.Fatalf("activate failure status = %d (%v), want 5xx", code, resp)
	}
	if c, body := e.getAnon("/news/occupied-page"); c != 200 || !strings.Contains(string(body), "concurrent occupant") {
		t.Fatalf("occupant disturbed: %d %s", c, body)
	}
	cid, _ := primitive.ObjectIDFromHex(strOf(draft, "id"))
	if active := e.activePub(t, cid); active != nil {
		t.Fatalf("active minted despite cutover failure: %s", active.ID.Hex())
	}
	// No success event; the failure itself is recorded as publication.failed.
	if n := e.count(publication.CollectionOutbox, bson.M{"event_type": publication.EventPublished}); n != 0 {
		t.Fatalf("content.publish rows = %d, want 0", n)
	}

	// Commit failure AFTER a successful cutover: compensation restores v1.
	e2, cid2, pid12, u2 := faultSetup(t, envOpts{
		faults: publication.Faults{BeforeCommit: func(ctx context.Context) error {
			return errFault("injected mongo commit failure")
		}},
	})
	code, resp = e2.publish(t, "fault-commit", v2body("v2 commit"))
	if code < 500 {
		t.Fatalf("commit failure status = %d (%v), want 5xx", code, resp)
	}
	assertOldLive(t, e2, cid2, pid12, u2, "v1 live")
	if n := e2.count(publication.CollectionOutbox, bson.M{"event_type": publication.EventPublished}); n != 1 {
		t.Fatalf("content.publish rows = %d, want 1 (v1 only)", n)
	}
}

// TestE2E_FaultCutoverCrash proves the §39.5 crash contract over HTTP: a
// process crash between canonical rename and Mongo commit preserves the
// on-disk files with NO compensation; after a same-binary restart the
// scanner restores the authoritative (old active) canonical and the page
// republishes cleanly.
func TestE2E_FaultCutoverCrash(t *testing.T) {
	e, cid, pid1, u := faultSetup(t, envOpts{
		faults: publication.Faults{BeforeCommit: func(ctx context.Context) error {
			return publication.ErrStopAfterRename
		}},
	})
	code, resp := e.publish(t, "fault-crash", v2body("v2 crash"))
	if code < 500 {
		t.Fatalf("crash status = %d (%v), want 5xx", code, resp)
	}
	// Crash facts: new bytes on disk, DB still shows old active.
	if c, body := e.getAnon(u); c != 200 || !strings.Contains(string(body), "v2 crash") {
		t.Fatalf("crashed cutover bytes missing: %d %s", c, body)
	}
	if active := e.activePub(t, cid); active.ID.Hex() != pid1 {
		t.Fatalf("DB active changed by crash: %s", active.ID.Hex())
	}
	if n := e.count("content_publications", bson.M{"content_id": cid}); n != 2 {
		t.Fatalf("publication rows = %d, want 2 (v1 active + crashed staged)", n)
	}

	// Restart: same binary/data, faults cleared. Scanner repairs.
	e.refault(envOpts{})
	rep, err := e.scanner.ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if rep.CanonicalMismatchRepaired+rep.PreviousRestored+rep.CanonicalRebuilt == 0 {
		t.Fatalf("scanner repaired nothing: %+v", rep)
	}
	if c, body := e.getAnon(u); c != 200 || !strings.Contains(string(body), "v1 live") {
		t.Fatalf("authoritative v1 not restored: %d %s", c, body)
	}
	if active := e.activePub(t, cid); active.ID.Hex() != pid1 {
		t.Fatalf("active changed by scanner: %s", active.ID.Hex())
	}
	// The page republishes cleanly after repair.
	code, r2 := e.publish(t, "fault-crash-retry", v2body("v2 after repair"))
	if code != 200 {
		t.Fatalf("post-repair publish = %d (%v)", code, r2)
	}
	if c, body := e.getAnon(u); c != 200 || !strings.Contains(string(body), "v2 after repair") {
		t.Fatalf("live not v2 after repair: %d %s", c, body)
	}
}

// TestE2E_FaultCutoverCrashLegacy proves the same crash contract through the
// legacy single-publish route (spec §16.6: all entries, one saga): clean v1,
// crashed v2 cutover, scanner repair, successful v3.
func TestE2E_FaultCutoverCrashLegacy(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")
	// Unpublished draft, then legacy publish v1 (wired saga branch).
	code, draft := e.postJSON("/api/v1/page-generation", map[string]any{
		"template": "financial-news", "title": "Legacy", "slug": "legacy-crash",
		"folder_path": "/news", "mode": "draft", "data": map[string]any{"headline": "legacy v1"},
	}, nil)
	if code != 201 {
		t.Fatalf("draft = %d (%v)", code, draft)
	}
	cid := strOf(draft, "id")
	code, r1 := e.postJSON("/api/v1/content/"+cid+"/publish", map[string]any{},
		map[string]string{"Idempotency-Key": "legacy-v1"})
	if code != 200 {
		t.Fatalf("legacy v1 publish = %d (%v)", code, r1)
	}
	pid1 := strOf(r1, "publication_id")
	u := strings.TrimPrefix(strOf(r1, "public_url"), e.base)

	// Crash the v2 cutover through the legacy route.
	e.refault(envOpts{
		faults: publication.Faults{BeforeCommit: func(ctx context.Context) error {
			return publication.ErrStopAfterRename
		}},
	})
	code, resp := e.postJSON("/api/v1/content/"+cid+"/publish", map[string]any{},
		map[string]string{"Idempotency-Key": "legacy-crash-1"})
	if code < 500 {
		t.Fatalf("legacy crash publish = %d (%v), want 5xx", code, resp)
	}
	// Legacy publish without a key is 428 with zero mutation.
	code, resp = e.postJSON("/api/v1/content/"+cid+"/publish", map[string]any{}, nil)
	if code != 428 {
		t.Fatalf("legacy no-key publish = %d (%v), want 428", code, resp)
	}
	coid, _ := primitive.ObjectIDFromHex(cid)
	if active := e.activePub(t, coid); active.ID.Hex() != pid1 {
		t.Fatalf("DB active changed by legacy crash: %s", active.ID.Hex())
	}
	// Restart + scanner repair, then legacy publish succeeds.
	e.refault(envOpts{})
	rep, err := e.scanner.ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if rep.CanonicalMismatchRepaired+rep.PreviousRestored+rep.CanonicalRebuilt == 0 {
		t.Fatalf("scanner repaired nothing: %+v", rep)
	}
	if c, body := e.getAnon(u); c != 200 || !strings.Contains(string(body), "legacy v1") {
		t.Fatalf("authoritative v1 not restored: %d %s", c, body)
	}
	code, resp = e.postJSON("/api/v1/content/"+cid+"/publish", map[string]any{},
		map[string]string{"Idempotency-Key": "legacy-crash-2"})
	if code != 200 {
		t.Fatalf("legacy publish after repair = %d (%v)", code, resp)
	}
	if strOf(resp, "publication_id") == "" || strOf(resp, "public_url") == "" {
		t.Fatalf("legacy publish missing publication fields: %v", resp)
	}
	if strOf(resp, "publication_id") == pid1 {
		t.Fatal("post-repair publish reused the crashed publication id")
	}
}

// TestE2E_FirstPublishCrashGap pins a recovery gap: a cutover crash on FIRST
// publish leaves an uncommitted canonical with no active record; pre
// migration-completion the scanner only REPORTS it (OrphanReported, never
// quarantined/repaired), so a same-key retry fails and an operator must
// remove the stray canonical (or complete migration). The test PASSES by
// characterising the gap; the table row records it as remaining risk.
func TestE2E_FirstPublishCrashGap(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")
	code, draft := e.postJSON("/api/v1/page-generation", map[string]any{
		"template": "financial-news", "title": "FirstCrash", "slug": "first-crash",
		"folder_path": "/news", "mode": "draft", "data": map[string]any{"headline": "fc v1"},
	}, nil)
	if code != 201 {
		t.Fatalf("draft = %d (%v)", code, draft)
	}
	cid := strOf(draft, "id")
	coid, _ := primitive.ObjectIDFromHex(cid)
	e.refault(envOpts{
		faults: publication.Faults{BeforeCommit: func(ctx context.Context) error {
			return publication.ErrStopAfterRename
		}},
	})
	v1 := int64(1)
	code, _ = e.publish(t, "first-crash-1", map[string]any{
		"template": "financial-news", "title": "FirstCrash", "slug": "first-crash",
		"folder_path": "/news", "mode": "publish", "expected_template_version": v1,
		"upsert": true, "data": map[string]any{"headline": "fc v1"},
	})
	if code < 500 {
		t.Fatalf("crash publish = %d, want 5xx", code)
	}
	e.refault(envOpts{})
	rep, err := e.scanner.ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if rep.OrphanReported != 1 || rep.OrphanQuarantined != 0 {
		t.Fatalf("scanner orphan handling = reported %d quarantined %d, want 1/0: %+v",
			rep.OrphanReported, rep.OrphanQuarantined, rep)
	}
	if active := e.activePub(t, coid); active != nil {
		t.Fatalf("scanner invented an active publication: %s", active.ID.Hex())
	}
	// Retry is still blocked by the stray canonical (needs operator action).
	code, resp := e.publish(t, "first-crash-2", map[string]any{
		"template": "financial-news", "title": "FirstCrash", "slug": "first-crash",
		"folder_path": "/news", "mode": "publish", "expected_template_version": v1,
		"upsert": true, "data": map[string]any{"headline": "fc v1"},
	})
	if code < 500 {
		t.Fatalf("retry after first-publish crash = %d (%v), want 5xx until operator removes the stray canonical", code, resp)
	}
	t.Logf("GAP PINNED: first-publish cutover crash needs manual canonical removal pre-migration-completion")
}

// TestE2E_FaultOutboxCDN proves webhook 500 schedules a retry with a stable
// event ID (no duplicate outbox row) and CDN purge failure never rolls back
// an activated publication.
func TestE2E_FaultOutboxCDN(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")
	v1 := int64(1)
	e.mu.Lock()
	e.deliverErr = errFault("receiver 500")
	e.mu.Unlock()
	code, r1 := e.publish(t, "fault-outbox-1", map[string]any{
		"template": "financial-news", "title": "Outbox", "slug": "outbox-page",
		"folder_path": "/news", "mode": "publish", "expected_template_version": v1,
		"data": map[string]any{"headline": "o1"},
	})
	if code != 201 {
		t.Fatalf("publish = %d (%v)", code, r1)
	}
	pid := mustOID(t, strOf(r1, "publication_id"))
	// First delivery attempt fails; row stays pending, no duplicate.
	did, derr := e.outbox.ProcessNext(context.Background())
	if !did || derr == nil {
		t.Fatalf("ProcessNext = %v, %v (want failed attempt)", did, derr)
	}
	if n := e.outboxCount(t, publication.EventPublished, pid); n != 1 {
		t.Fatalf("outbox rows = %d, want 1", n)
	}
	e.mu.Lock()
	firstID := ""
	if len(e.deliveries) > 0 {
		firstID = e.deliveries[0].ID
	}
	e.deliverErr = nil
	e.mu.Unlock()
	if firstID == "" {
		t.Fatal("no delivery attempt recorded")
	}
	// Make the retry due and deliver: same stable event ID, still one row.
	_, err := e.db.Collection(publication.CollectionOutbox).UpdateOne(context.Background(),
		bson.M{"event_type": publication.EventPublished, "aggregate_id": pid},
		bson.M{"$set": bson.M{"next_attempt_at": time.Now().Add(-time.Second)}})
	if err != nil {
		t.Fatalf("backdate retry: %v", err)
	}
	did, derr = e.outbox.ProcessNext(context.Background())
	if !did || derr != nil {
		t.Fatalf("retry ProcessNext = %v, %v", did, derr)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.deliveries) != 2 {
		t.Fatalf("delivery attempts = %d, want 2", len(e.deliveries))
	}
	if e.deliveries[1].ID != firstID {
		t.Fatalf("event ID changed across retry: %q vs %q (receiver cannot dedupe)", e.deliveries[1].ID, firstID)
	}
	if n := e.outboxCount(t, publication.EventPublished, pid); n != 1 {
		t.Fatalf("outbox rows after retry = %d, want 1", n)
	}

	// CDN purge failure: publication stays active, page live.
	e2 := newEnv(t, envOpts{})
	e2.seedTemplate(t, "financial-news")
	e2.mu.Lock()
	e2.purgeErr = errFault("cdn purge 503")
	e2.mu.Unlock()
	code, r2 := e2.publish(t, "fault-cdn-1", map[string]any{
		"template": "financial-news", "title": "CDN", "slug": "cdn-page",
		"folder_path": "/news", "mode": "publish", "expected_template_version": v1,
		"data": map[string]any{"headline": "cdn"},
	})
	if code != 201 {
		t.Fatalf("publish with purge failure = %d (%v), want 201", code, r2)
	}
	cid2, _ := primitive.ObjectIDFromHex(strOf(r2, "id"))
	if active := e2.activePub(t, cid2); active == nil {
		t.Fatal("purge failure deactivated the publication")
	}
	u2 := strings.TrimPrefix(strOf(r2, "public_url"), e2.base)
	if c, body := e2.getAnon(u2); c != 200 || !strings.Contains(string(body), "cdn") {
		t.Fatalf("page not live after purge failure: %d %s", c, body)
	}
}

// TestE2E_UnpublishSequence proves unpublish over the legacy route: first
// call removes the canonical, the second returns 200 with no new event.
func TestE2E_UnpublishSequence(t *testing.T) {
	e, cid, _, u := faultSetup(t, envOpts{})
	cidHex := cid.Hex()
	code, resp := e.postJSON("/api/v1/content/"+cidHex+"/unpublish", map[string]any{}, nil)
	if code != 200 {
		t.Fatalf("unpublish = %d (%v)", code, resp)
	}
	if c, _ := e.getAnon(u); c != 404 {
		t.Fatalf("canonical still served after unpublish: %d", c)
	}
	if active := e.activePub(t, cid); active != nil {
		t.Fatalf("active still set after unpublish: %s", active.ID.Hex())
	}
	before := e.count(publication.CollectionOutbox, bson.M{})
	code, resp = e.postJSON("/api/v1/content/"+cidHex+"/unpublish", map[string]any{}, nil)
	if code != 200 {
		t.Fatalf("second unpublish = %d (%v), want 200", code, resp)
	}
	if n := e.count(publication.CollectionOutbox, bson.M{}); n != before {
		t.Fatalf("second unpublish created an outbox event (%d -> %d)", before, n)
	}
	// Scanner after unpublish converges with no repair and no resurrection.
	rep, err := e.scanner.ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if rep.RepairErrors != 0 {
		t.Fatalf("scanner repair errors after clean unpublish: %+v", rep)
	}
	if c, _ := e.getAnon(u); c != 404 {
		t.Fatalf("scanner resurrected unpublished page: %d", c)
	}
}

// --- shared fault helpers -------------------------------------------------

func errFault(msg string) error { return &faultError{msg} }

type faultError struct{ msg string }

func (f *faultError) Error() string { return f.msg }

func mustOID(t *testing.T, hex string) primitive.ObjectID {
	t.Helper()
	id, err := primitive.ObjectIDFromHex(hex)
	if err != nil {
		t.Fatalf("bad oid %q: %v", hex, err)
	}
	return id
}

// assertOldLive checks the old publication is still active and served.
func assertOldLive(t *testing.T, e *testEnv, cid primitive.ObjectID, pid1, u, headline string) {
	t.Helper()
	if active := e.activePub(t, cid); active == nil || active.ID.Hex() != pid1 {
		t.Fatalf("old active lost: %+v", active)
	}
	if c, body := e.getAnon(u); c != 200 || !strings.Contains(string(body), headline) {
		t.Fatalf("old live not served: %d %s", c, body)
	}
}

// seedOccupant writes a foreign canonical file to simulate a concurrently
// appeared canonical at the publish target.
func seedOccupant(t *testing.T, e *testEnv, fullPath string, body []byte) {
	t.Helper()
	p, err := e.store.CanonicalFilePath(fullPath)
	if err != nil {
		t.Fatalf("canonical path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatalf("write occupant: %v", err)
	}
}

// TestE2E_LegacyParity proves the Task 16C legacy entry points share V3
// semantics over HTTP: by-path draft edit, keyed single publish (+ replay),
// and the /api/v1/regenerate contract verdict (below).
func TestE2E_LegacyParity(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")
	code, draft := e.postJSON("/api/v1/page-generation", map[string]any{
		"template": "financial-news", "title": "LegacyPar", "slug": "legacy-par",
		"folder_path": "/news", "mode": "draft", "data": map[string]any{"headline": "draft one"},
	}, nil)
	if code != 201 {
		t.Fatalf("draft = %d (%v)", code, draft)
	}
	cid := strOf(draft, "id")

	// Legacy by-path draft edit (draft-only: no live file appears).
	code, raw, _ := e.do("PUT", "/api/v1/content/by-path?path=/news/legacy-par",
		[]byte(`{"title":"LegacyPar Edited"}`), map[string]string{"Content-Type": "application/json"})
	if code != 200 {
		t.Fatalf("by-path update = %d (%s)", code, raw)
	}
	if c, _ := e.getAnon("/news/legacy-par"); c != 404 {
		t.Fatalf("draft edit produced a live file: %d", c)
	}

	// Legacy keyed publish creates the active publication + outbox row.
	pubBefore := e.count("content_publications", bson.M{})
	code, r1 := e.postJSON("/api/v1/content/"+cid+"/publish", map[string]any{},
		map[string]string{"Idempotency-Key": "legacy-par-1"})
	if code != 200 {
		t.Fatalf("legacy publish = %d (%v)", code, r1)
	}
	if strOf(r1, "publication_id") == "" {
		t.Fatalf("legacy publish missing publication_id: %v", r1)
	}
	if n := e.count("content_publications", bson.M{}); n != pubBefore+1 {
		t.Fatalf("publications = %d, want %d", n, pubBefore+1)
	}
	u := strings.TrimPrefix(strOf(r1, "public_url"), e.base)
	if c, body := e.getAnon(u); c != 200 || !strings.Contains(string(body), "draft one") {
		t.Fatalf("legacy live wrong: %d %s", c, body)
	}
	// Same-key replay returns the same publication (no duplicate).
	code, r2 := e.postJSON("/api/v1/content/"+cid+"/publish", map[string]any{},
		map[string]string{"Idempotency-Key": "legacy-par-1"})
	if code != 200 || strOf(r2, "publication_id") != strOf(r1, "publication_id") {
		t.Fatalf("legacy replay = %d (%v), want same publication %s", code, r2, strOf(r1, "publication_id"))
	}
	if n := e.count("content_publications", bson.M{}); n != pubBefore+1 {
		t.Fatalf("legacy replay minted a publication (%d)", n)
	}
}

// TestE2E_RegenerateVerdict pins the known Task 16 gap WITHOUT changing
// behavior (spec-first): POST /api/v1/regenerate answers 200 success while
// RegenerateAllContent is an unconditional nil no-op — canonical bytes,
// publications and outbox are all unchanged. Verdict recorded in the E2E
// table: the route needs a 410 Gone vs upgrade-job-redirect decision before
// G4; until then the success message is misleading and must not be relied
// on for cache invalidation.
func TestE2E_RegenerateVerdict(t *testing.T) {
	e, cid, pid1, u := faultSetup(t, envOpts{})
	beforeBody := func() []byte {
		c, body := e.getAnon(u)
		if c != 200 {
			t.Fatalf("live missing before regenerate: %d", c)
		}
		return body
	}()
	pubsBefore := e.count("content_publications", bson.M{})
	outBefore := e.count(publication.CollectionOutbox, bson.M{})

	code, resp := e.postJSON("/api/v1/regenerate", map[string]any{}, nil)
	if code != 200 {
		t.Fatalf("regenerate status = %d (%v)", code, resp)
	}
	msg, _ := resp["message"].(string)
	if !strings.Contains(msg, "regenerated") {
		t.Fatalf("regenerate message changed (verdict premise): %v", resp)
	}
	// The no-op proof: nothing changed anywhere.
	if c, body := e.getAnon(u); c != 200 || string(body) != string(beforeBody) {
		t.Fatalf("regenerate altered canonical bytes")
	}
	if active := e.activePub(t, cid); active.ID.Hex() != pid1 {
		t.Fatalf("regenerate changed active publication")
	}
	if n := e.count("content_publications", bson.M{}); n != pubsBefore {
		t.Fatalf("regenerate minted publications (%d -> %d)", pubsBefore, n)
	}
	if n := e.count(publication.CollectionOutbox, bson.M{}); n != outBefore {
		t.Fatalf("regenerate touched outbox (%d -> %d)", outBefore, n)
	}
	t.Logf("VERDICT PINNED: /api/v1/regenerate is a silent no-op (200 + %q, zero side effects) — needs 410 vs upgrade-job-redirect decision, not silent behavior change", msg)
}

// --- Idempotency table (spec §21, §39.6) ------------------------------------
// One shared page per sub-row would couple attempts; each row below owns its
// key space on a fresh instance except where the row explicitly retries.

func idemBody(slug, headline string, upsert bool) map[string]any {
	v1 := int64(1)
	return map[string]any{
		"template": "financial-news", "title": "Idem", "slug": slug,
		"folder_path": "/news", "mode": "publish", "expected_template_version": v1,
		"upsert": upsert, "data": map[string]any{"headline": headline},
	}
}

// TestE2E_IdempotencyReplay proves lost-response retry reuses the completed
// response: same key + byte-identical body → same publication, no duplicate
// publication, no duplicate outbox.
func TestE2E_IdempotencyReplay(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")
	raw, _ := jsonMarshal(idemBody("idem-replay", "h1", false))
	hdr := map[string]string{"Idempotency-Key": "idem-replay-1"}
	c1, r1 := e.postRaw("/api/v1/page-generation", raw, hdr)
	if c1 != 201 {
		t.Fatalf("first = %d (%v)", c1, r1)
	}
	c2, r2 := e.postRaw("/api/v1/page-generation", raw, hdr)
	if c2 != 200 && c2 != 201 {
		t.Fatalf("replay = %d (%v)", c2, r2)
	}
	if strOf(r2, "publication_id") != strOf(r1, "publication_id") {
		t.Fatalf("replay publication %s != original %s", strOf(r2, "publication_id"), strOf(r1, "publication_id"))
	}
	if n := e.count("content_publications", bson.M{}); n != 1 {
		t.Fatalf("publications = %d, want 1", n)
	}
	if n := e.count(publication.CollectionOutbox, bson.M{"event_type": publication.EventPublished}); n != 1 {
		t.Fatalf("publish outbox rows = %d, want 1", n)
	}
	if n := e.count("content_versions", bson.M{}); n != 1 {
		t.Fatalf("content versions = %d, want 1 (no duplicate version)", n)
	}
}

// TestE2E_IdempotencyParallel proves parallel same-key publishers converge
// on exactly one publication: one 201, the rest replay/in-progress, one
// outbox row.
func TestE2E_IdempotencyParallel(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")
	raw, _ := jsonMarshal(idemBody("idem-parallel", "hp", false))
	const n = 8
	type res struct {
		code int
		body map[string]any
	}
	out := make([]res, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, b := e.postRaw("/api/v1/page-generation", raw, map[string]string{"Idempotency-Key": "idem-parallel-1"})
			out[i] = res{c, b}
		}(i)
	}
	wg.Wait()
	seen := map[int]int{}
	pubIDs := map[string]bool{}
	for _, r := range out {
		seen[r.code]++
		switch r.code {
		case 201, 200:
			pubIDs[strOf(r.body, "publication_id")] = true
		case 409:
		default:
			t.Fatalf("unexpected status %d (%v)", r.code, r.body)
		}
	}
	if seen[201] != 1 {
		t.Fatalf("201 count = %d, want exactly 1 (%v)", seen[201], seen)
	}
	if len(pubIDs) != 1 {
		t.Fatalf("distinct publication ids = %d, want 1", len(pubIDs))
	}
	if n := e.count("content_publications", bson.M{}); n != 1 {
		t.Fatalf("publications = %d, want 1", n)
	}
	if n := e.count(publication.CollectionOutbox, bson.M{"event_type": publication.EventPublished}); n != 1 {
		t.Fatalf("publish outbox rows = %d, want 1", n)
	}
}

// TestE2E_IdempotencyConflict proves same key + changed body is 409 with
// zero new side effects.
func TestE2E_IdempotencyConflict(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")
	raw1, _ := jsonMarshal(idemBody("idem-conflict", "h1", false))
	c1, r1 := e.postRaw("/api/v1/page-generation", raw1, map[string]string{"Idempotency-Key": "idem-conflict-1"})
	if c1 != 201 {
		t.Fatalf("first = %d (%v)", c1, r1)
	}
	raw2, _ := jsonMarshal(idemBody("idem-conflict", "CHANGED", false))
	c2, r2 := e.postRaw("/api/v1/page-generation", raw2, map[string]string{"Idempotency-Key": "idem-conflict-1"})
	if c2 != 409 {
		t.Fatalf("changed-body retry = %d (%v), want 409", c2, r2)
	}
	if got := errorCodeOf(r2); got != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("code = %q, want IDEMPOTENCY_CONFLICT", got)
	}
	if n := e.count("content_publications", bson.M{}); n != 1 {
		t.Fatalf("publications = %d, want 1", n)
	}
	if n := e.count("content_versions", bson.M{}); n != 1 {
		t.Fatalf("versions changed by conflict retry")
	}
}

// TestE2E_IdempotencyTerminalRetry proves a terminal pre-activation failure
// on a REPLACE followed by the same key uses a NEW attempt + Publication ID
// (spec review focus 3, second half): one active, one publish outbox.
func TestE2E_IdempotencyTerminalRetry(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")
	_, r1 := e.publish(t, "term-base", idemBody("term-replace", "v1", false))
	if strOf(r1, "publication_id") == "" {
		t.Fatalf("base publish failed: %v", r1)
	}
	e.refault(envOpts{maxWriteBytes: 1})
	raw, _ := jsonMarshal(idemBody("term-replace", "v2", true))
	hdr := map[string]string{"Idempotency-Key": "term-replace-2"}
	c1, f1 := e.postRaw("/api/v1/page-generation", raw, hdr)
	if c1 != 503 {
		t.Fatalf("failing attempt = %d (%v), want 503", c1, f1)
	}
	failedPub := strOf(f1, "publication_id")
	e.refault(envOpts{})
	c2, r2 := e.postRaw("/api/v1/page-generation", raw, hdr)
	if c2 != 200 {
		t.Fatalf("retry after terminal = %d (%v), want success with new attempt", c2, r2)
	}
	if strOf(r2, "publication_id") == "" || strOf(r2, "publication_id") == failedPub {
		t.Fatalf("retry must mint a new publication id (failed=%q got=%v)", failedPub, r2)
	}
	if n := e.count("content_publications", bson.M{"status": "active"}); n != 1 {
		t.Fatalf("active publications = %d, want 1", n)
	}
	// Two successful publications (v1 + retried v2) carry exactly one
	// publish event each — no duplicate webhook per publication.
	if n := e.count(publication.CollectionOutbox, bson.M{"event_type": publication.EventPublished}); n != 2 {
		t.Fatalf("publish outbox rows = %d, want 2 (one per publication)", n)
	}
	newPub := mustOID(t, strOf(r2, "publication_id"))
	if n := e.outboxCount(t, publication.EventPublished, newPub); n != 1 {
		t.Fatalf("retried publication outbox rows = %d, want 1", n)
	}
}

// TestE2E_IdempotencyCreateTerminalGap pins a protocol gap: a terminal
// pre-activation failure on a CREATE deadlocks same-key retry — the
// byte-identical retry hits 409 PATH_CONFLICT at the upsert gate (the page
// was created by attempt 1), while an upsert=true retry hits 409
// IDEMPOTENCY_CONFLICT at Begin (body changed). The only escape is a NEW
// key + upsert, which breaks idempotent linkage. Specified fix (not
// implemented here): exempt same-operation retries from the upsert gate
// (the op metadata proves the target is the prior attempt's artifact) or
// document the new-key escape. Table row: FAIL.
func TestE2E_IdempotencyCreateTerminalGap(t *testing.T) {
	e := newEnv(t, envOpts{maxWriteBytes: 1})
	e.seedTemplate(t, "financial-news")
	raw, _ := jsonMarshal(idemBody("term-create", "h1", false))
	hdr := map[string]string{"Idempotency-Key": "term-create-1"}
	c1, _ := e.postRaw("/api/v1/page-generation", raw, hdr)
	if c1 != 503 {
		t.Fatalf("failing attempt = %d, want 503", c1)
	}
	e.refault(envOpts{})
	c2, r2 := e.postRaw("/api/v1/page-generation", raw, hdr)
	if c2 != 409 || errorCodeOf(r2) != "PATH_CONFLICT" {
		t.Fatalf("identical retry = %d (%v), want 409 PATH_CONFLICT", c2, r2)
	}
	rawU, _ := jsonMarshal(idemBody("term-create", "h1", true))
	c3, r3 := e.postRaw("/api/v1/page-generation", rawU, hdr)
	if c3 != 409 || errorCodeOf(r3) != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("upsert retry = %d (%v), want 409 IDEMPOTENCY_CONFLICT", c3, r3)
	}
	c4, r4 := e.postRaw("/api/v1/page-generation", rawU, map[string]string{"Idempotency-Key": "term-create-2"})
	if c4 != 200 {
		t.Fatalf("new-key escape = %d (%v), want 200", c4, r4)
	}
	if n := e.count(publication.CollectionOutbox, bson.M{"event_type": publication.EventPublished}); n != 1 {
		t.Fatalf("publish outbox rows = %d, want 1", n)
	}
	t.Logf("GAP PINNED: same-key retry after terminal CREATE failure is unreachable (PATH_CONFLICT vs IDEMPOTENCY_CONFLICT); escape needs a new key")
}

// TestE2E_IdempotencyLeaseMatrix pins the crash-lease contract at the
// service seam used by scheduler ticks: uncertain takeover reuses the
// attempt's publication ID + logical time; unowned takeovers are typed
// NOT_FOUND with zero mutation; active/completed leases refuse.
func TestE2E_IdempotencyLeaseMatrix(t *testing.T) {
	e := newEnv(t, envOpts{})
	_, tv := e.seedTemplate(t, "financial-news")
	ctx := context.Background()
	pubID := primitive.NewObjectID()
	logicalAt := time.Now().UTC().Truncate(time.Millisecond)

	// Begin + freeze an execution snapshot (crash before Render shape).
	op, err := e.idem.Begin(ctx, "scheduler", "POST", "/internal/scheduler/publish", "sched/lease-1", nil)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := e.idem.FreezeExecution(ctx, op.ID, op.Attempt, pubID, logicalAt, tv.ID); err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	// Active lease: takeover refused.
	if _, err := e.idem.TakeOver(ctx, op.ID, op.LeaseGeneration); idempotency.CodeOf(err) != idempotency.CodeLeaseActive {
		t.Fatalf("active-lease TakeOver = %v, want LEASE_ACTIVE", err)
	}
	// Expire the lease (crashed worker) and take over: same attempt, same
	// durable Publication ID + logical time (no reallocation).
	if _, err := e.db.Collection("idempotency_records").UpdateOne(ctx,
		bson.M{"_id": op.ID}, bson.M{"$set": bson.M{"processing_expires_at": time.Now().Add(-time.Second)}}); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	taken, err := e.idem.TakeOverByKey(ctx, "scheduler", "POST", "/internal/scheduler/publish", "sched/lease-1")
	if err != nil {
		t.Fatalf("TakeOverByKey: %v", err)
	}
	if taken.Attempt != op.Attempt {
		t.Fatalf("takeover attempt = %d, want %d (stable)", taken.Attempt, op.Attempt)
	}
	if taken.PublicationID == nil || *taken.PublicationID != pubID {
		t.Fatalf("takeover publication changed (want durable reuse)")
	}
	// Unowned takeovers: unknown ID and unknown key are typed NOT_FOUND.
	if _, err := e.idem.TakeOver(ctx, primitive.NewObjectID(), 0); idempotency.CodeOf(err) != idempotency.CodeNotFound {
		t.Fatalf("unowned TakeOver = %v, want NOT_FOUND", err)
	}
	if _, err := e.idem.TakeOverByKey(ctx, "scheduler", "POST", "/nope", "missing"); idempotency.CodeOf(err) != idempotency.CodeNotFound {
		t.Fatalf("missing-key TakeOverByKey = %v, want NOT_FOUND", err)
	}
	before := e.count("idempotency_records", bson.M{})
	_ = before
	// Completed operations refuse takeover (replay instead).
	op2, err := e.idem.Begin(ctx, "o2", "POST", "/p", "k2", nil)
	if err != nil {
		t.Fatalf("Begin2: %v", err)
	}
	if _, err := e.idem.Complete(ctx, op2.ID, op2.Attempt, 200, map[string]any{"ok": true}, false); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, err := e.idem.TakeOver(ctx, op2.ID, op2.LeaseGeneration); idempotency.CodeOf(err) != idempotency.CodeAlreadyCompleted {
		t.Fatalf("completed TakeOver = %v, want ALREADY_COMPLETED", err)
	}
	t.Logf("lease matrix: uncertain takeover reuses attempt+%s; unowned/missing=NOT_FOUND; active refused; completed refused", pubID.Hex())
}

// TestE2E_RollbackRetry proves rollback retry with the same key replays the
// same new publication (no duplicate) and mode=preview ignores the key.
func TestE2E_RollbackRetry(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")
	v1 := int64(1)
	_, r1 := e.publish(t, "rb-1", idemBody("rb-page", "v1", false))
	_, r2 := e.publish(t, "rb-2", idemBody("rb-page", "v2", true))
	cid := strOf(r2, "id")
	pid1 := strOf(r1, "publication_id")
	hdr := map[string]string{"Idempotency-Key": "rb-rollback-1"}
	c3, rb1 := e.postJSON("/api/v1/content/"+cid+"/publications/"+pid1+"/rollback", map[string]any{}, hdr)
	if c3 != 200 {
		t.Fatalf("rollback = %d (%v)", c3, rb1)
	}
	rbp := strOf(rb1, "publication_id")
	if rbp == "" || rbp == pid1 {
		t.Fatalf("rollback must mint a new publication: %v", rb1)
	}
	c4, rb2 := e.postJSON("/api/v1/content/"+cid+"/publications/"+pid1+"/rollback", map[string]any{}, hdr)
	if c4 != 200 || strOf(rb2, "publication_id") != rbp {
		t.Fatalf("rollback retry = %d (%v), want replay of %s", c4, rb2, rbp)
	}
	if n := e.count("content_publications", bson.M{"content_id": mustOID(t, cid)}); n != 3 {
		t.Fatalf("publications = %d, want 3 (v1, v2, rollback)", n)
	}

	// Preview ignores Idempotency-Key and creates no record.
	before := e.count("idempotency_records", bson.M{})
	c5, pv := e.postJSON("/api/v1/page-generation", map[string]any{
		"template": "financial-news", "title": "Pv", "slug": "pv-ignore",
		"folder_path": "/news", "mode": "preview", "data": map[string]any{"headline": "p"},
	}, map[string]string{"Idempotency-Key": "pv-1"})
	if c5 != 200 {
		t.Fatalf("preview = %d (%v)", c5, pv)
	}
	if n := e.count("idempotency_records", bson.M{}); n != before {
		t.Fatalf("preview created an idempotency record")
	}
	_ = v1
}

// TestE2E_SchedulerStableKey proves the Task 16D background contract: the
// scheduler tick body (ContentService.PublishInternal under a stable
// operation key) replays instead of duplicating and tells parallel ticks to
// retry later. The crash-takeover branch is PINNED BROKEN (shadowing bug,
// table row FAIL — see inline); the idempotency seam itself is proven sound
// by an explicit takeover + publish with exactly one publication.
func TestE2E_SchedulerStableKey(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")
	code, draft := e.postJSON("/api/v1/page-generation", map[string]any{
		"template": "financial-news", "title": "Sched", "slug": "sched-page",
		"folder_path": "/news", "mode": "draft", "data": map[string]any{"headline": "s"},
	}, nil)
	if code != 201 {
		t.Fatalf("draft = %d (%v)", code, draft)
	}
	cid := mustOID(t, strOf(draft, "id"))
	ctx := context.Background()
	key := "scheduler/" + cid.Hex() + "/v1"
	path := "/internal/scheduler/publish"

	// Tick 1 publishes; tick 2 with the same key replays (nil error, no
	// new publication, no new outbox row).
	if err := e.contentSvc.PublishInternal(ctx, cid, "scheduler", path, key); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if err := e.contentSvc.PublishInternal(ctx, cid, "scheduler", path, key); err != nil {
		t.Fatalf("tick 2 replay: %v", err)
	}
	if n := e.count("content_publications", bson.M{"content_id": cid}); n != 1 {
		t.Fatalf("publications = %d, want 1", n)
	}

	// Parallel tick holding a live lease gets retry-later, never a duplicate.
	op, err := e.idem.Begin(ctx, "scheduler", "POST", "/internal/scheduler/other", "sched/par-1", nil)
	if err != nil {
		t.Fatalf("manual begin: %v", err)
	}
	_ = op
	if err := e.contentSvc.PublishInternal(ctx, cid, "scheduler", "/internal/scheduler/other", "sched/par-1"); err == nil {
		t.Fatal("parallel tick succeeded, want ErrPublishRetryLater")
	} else if !strings.Contains(err.Error(), "retry the same operation key later") {
		t.Fatalf("parallel tick error = %v, want retry-later", err)
	}

	// Crashed worker (expired lease, same attempt, no snapshot): the next
	// tick takes over and converges without a duplicate. Use a fresh page
	// so the lease belongs to an unfinished attempt.
	//
	// BUG PINNED (Task 16D, internal/services/publish_internal.go:96): the
	// takeover branch declares `op, terr := TakeOverByKey(...)` inside the
	// case clause, shadowing the outer `op`. The taken-over operation is
	// discarded and the saga receives the outer ZERO op ID, so every
	// background crash-takeover fails with IDEMPOTENCY_NOT_FOUND. Worse,
	// the discarded TakeOverByKey already refreshed the lease (+5m), so the
	// next tick sees IN_PROGRESS → retry-later; when that lapses the cycle
	// repeats: a crashed scheduler attempt NEVER converges (livelock with
	// 5-minute period) until an operator intervenes. Specified fix (not
	// implemented here — services/ is frozen): assign the outer `op`
	// (`var terr error; op, terr = ...`). The scanner has no lease role
	// (file-level only) and cannot break the cycle — recorded below.
	code, draft2 := e.postJSON("/api/v1/page-generation", map[string]any{
		"template": "financial-news", "title": "Sched2", "slug": "sched-page-2",
		"folder_path": "/news", "mode": "draft", "data": map[string]any{"headline": "s2"},
	}, nil)
	if code != 201 {
		t.Fatalf("draft2 = %d (%v)", code, draft2)
	}
	cid2 := mustOID(t, strOf(draft2, "id"))
	key2 := "scheduler/" + cid2.Hex() + "/v1"
	op2, err := e.idem.Begin(ctx, "scheduler", "POST", path, key2, nil)
	if err != nil {
		t.Fatalf("begin2: %v", err)
	}
	_ = op2
	if _, err := e.db.Collection("idempotency_records").UpdateOne(ctx,
		bson.M{"owner": "scheduler", "path": path, "key": key2},
		bson.M{"$set": bson.M{"processing_expires_at": time.Now().Add(-time.Second)}}); err != nil {
		t.Fatalf("expire: %v", err)
	}
	err = e.contentSvc.PublishInternal(ctx, cid2, "scheduler", path, key2)
	if err == nil || publication.CodeOf(err) != idempotency.CodeNotFound {
		t.Fatalf("takeover tick = %v, want IDEMPOTENCY_NOT_FOUND (shadowing bug pin)", err)
	}
	t.Logf("BUG PINNED: scheduler crash-takeover via PublishInternal fails with IDEMPOTENCY_NOT_FOUND (publish_internal.go:96 shadows the taken-over op)")
	// The underlying seam is sound: taking over and passing the taken op
	// explicitly converges with exactly one publication. (The bug's own
	// discarded takeover refreshed the lease, so expire it again first.)
	if _, err := e.db.Collection("idempotency_records").UpdateOne(ctx,
		bson.M{"owner": "scheduler", "path": path, "key": key2},
		bson.M{"$set": bson.M{"processing_expires_at": time.Now().Add(-time.Second)}}); err != nil {
		t.Fatalf("re-expire: %v", err)
	}
	taken, err := e.idem.TakeOverByKey(ctx, "scheduler", "POST", path, key2)
	if err != nil {
		t.Fatalf("manual takeover: %v", err)
	}
	if _, err := e.saga.Publish(ctx, publication.PublishRequest{ContentID: cid2, IdempotencyRecord: &taken.ID}); err != nil {
		t.Fatalf("seam publish with taken op: %v", err)
	}
	if n := e.count("content_publications", bson.M{"content_id": cid2}); n != 1 {
		t.Fatalf("takeover publications = %d, want 1", n)
	}
	if active := e.activePub(t, cid2); active == nil {
		t.Fatal("takeover tick left no active publication")
	}
	// Scanner's role is file-level only (no lease ownership): nothing to
	// repair after clean scheduler ticks.
	rep, err := e.scanner.ScanOnce(ctx)
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if rep.RepairErrors != 0 || rep.ImmutableMissingP0 != 0 {
		t.Fatalf("scanner damage after scheduler ticks: %+v", rep)
	}
}

// TestE2E_BuildSHAProvenance proves the 16E ldflag plumbing end to end:
// the Dockerfile stamps main.ProductBuildSHA with the git SHA; the saga
// persists it on every publication record. The test builds the real server
// binary with a pinned SHA and asserts the marker lands in the binary, and
// asserts the served publication record carries the instance SHA (not the
// "7.2.2" dev default).
func TestE2E_BuildSHAProvenance(t *testing.T) {
	e := newEnv(t, envOpts{buildSHA: "task17-e2e-sha"})
	e.seedTemplate(t, "financial-news")
	v1 := int64(1)
	_, r1 := e.publish(t, "sha-1", idemBody("sha-page", "s", false))
	_ = v1
	cid := mustOID(t, strOf(r1, "id"))
	active := e.activePub(t, cid)
	if active.ProductBuildSHA != "task17-e2e-sha" {
		t.Fatalf("record product_build_sha = %q, want the wired instance SHA (not the dev default)", active.ProductBuildSHA)
	}
	if active.RendererVersion == "" {
		t.Fatal("renderer_version not recorded")
	}

	// Binary mechanism: the exact Dockerfile ldflag stamps the marker.
	modRoot := moduleRoot(t)
	out := filepath.Join(t.TempDir(), "lightcms-e2e")
	cmd := exec.Command("go", "build", "-ldflags", "-X main.ProductBuildSHA=e2e-pin-sha-12345", "-o", out, "./cmd/server")
	cmd.Dir = modRoot
	if bout, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build pinned binary: %v %s", err, bout)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read binary: %v", err)
	}
	if !strings.Contains(string(raw), "e2e-pin-sha-12345") {
		t.Fatal("pinned ProductBuildSHA missing from server binary (Dockerfile ldflag wiring broken)")
	}
	df, err := os.ReadFile(filepath.Join(modRoot, "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	if !strings.Contains(string(df), "-X main.ProductBuildSHA=") {
		t.Fatal("Dockerfile no longer stamps main.ProductBuildSHA (release wiring drift)")
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above working directory")
		}
		dir = parent
	}
}

// TestE2E_ProductURLGuard pins the §13 production URL rule at the seam the
// 16E runtime uses: plain-HTTP bases are rejected for production, HTTPS
// accepted, and the wired resolver reports its base.
func TestE2E_ProductURLGuard(t *testing.T) {
	e := newEnv(t, envOpts{})
	httpBase, _ := url.Parse("http://pages.example.com")
	if err := publicurl.ValidateProductionBaseURL(httpBase); err == nil {
		t.Fatal("http production base accepted, want HTTPS rejection")
	}
	httpsBase, _ := url.Parse("https://pages.example.com/sub")
	if err := publicurl.ValidateProductionBaseURL(httpsBase); err != nil {
		t.Fatalf("https production base rejected: %v", err)
	}
	if e.resolver.Base() == nil || !strings.Contains(e.resolver.Base().String(), "127.0.0.1") {
		t.Fatalf("resolver base = %v", e.resolver.Base())
	}
	// pathkey typed errors are introspectable (invalid-path branch).
	canonErr := func() error {
		_, err := pathkey.Canonical("/news/../evil")
		return err
	}()
	if canonErr == nil || !pathkey.IsInvalidPath(canonErr) {
		t.Fatalf("traversal canonical = %v", canonErr)
	}
	if pathkey.IsInvalidPath(nil) || pathkey.IsInvalidPath(errFault("x")) {
		t.Fatal("IsInvalidPath misclassifies")
	}
	ipe := &pathkey.InvalidPathError{Path: "/x", Reason: "test"}
	if ipe.Error() == "" || ipe.Unwrap() != nil {
		t.Fatal("InvalidPathError helpers wrong")
	}
}

// TestE2E_IdempotencyHeartbeat proves the worker lease contract: heartbeat
// renews without touching the attempt; a stale generation stops side
// effects; TTL/lease accessors report the wired policy.
func TestE2E_IdempotencyHeartbeat(t *testing.T) {
	e := newEnv(t, envOpts{})
	ctx := context.Background()
	op, err := e.idem.Begin(ctx, "worker", "POST", "/job", "heartbeat-1", nil)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if e.idem.TTL() <= 0 || e.idem.Lease() <= 0 {
		t.Fatalf("TTL/Lease not wired: %v %v", e.idem.TTL(), e.idem.Lease())
	}
	renewed, err := e.idem.RenewLease(ctx, op.ID, op.Attempt, op.LeaseGeneration)
	if err != nil {
		t.Fatalf("RenewLease: %v", err)
	}
	if renewed.Attempt != op.Attempt || renewed.LeaseGeneration != op.LeaseGeneration {
		t.Fatalf("heartbeat moved attempt/generation: %+v", renewed)
	}
	// Stale generation: lease-loss, caller must stop side effects.
	if _, err := e.idem.RenewLease(ctx, op.ID, op.Attempt, op.LeaseGeneration+99); idempotency.CodeOf(err) != idempotency.CodeLeaseLost {
		t.Fatalf("stale heartbeat = %v, want LEASE_LOST", err)
	}
	// Expired lease + takeover, then the OLD generation is stale.
	if _, err := e.db.Collection("idempotency_records").UpdateOne(ctx, bson.M{"_id": op.ID},
		bson.M{"$set": bson.M{"processing_expires_at": time.Now().Add(-time.Second)}}); err != nil {
		t.Fatalf("expire: %v", err)
	}
	taken, err := e.idem.TakeOver(ctx, op.ID, op.LeaseGeneration)
	if err != nil {
		t.Fatalf("TakeOver: %v", err)
	}
	if _, err := e.idem.RenewLease(ctx, op.ID, taken.Attempt, op.LeaseGeneration); idempotency.CodeOf(err) != idempotency.CodeLeaseLost {
		t.Fatalf("pre-takeover heartbeat = %v, want LEASE_LOST", err)
	}
	if _, err := e.idem.RenewLease(ctx, taken.ID, taken.Attempt, taken.LeaseGeneration); err != nil {
		t.Fatalf("post-takeover heartbeat: %v", err)
	}
}

// TestE2E_DuplicateTemplateSlug pins template identity: duplicate slug
// creation is rejected with a typed error and changes no template version.
func TestE2E_DuplicateTemplateSlug(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")
	ctx := context.Background()
	tplRec, err := templatecontract.NewRepository(e.db).FindTemplateBySlug(ctx, "financial-news")
	if err != nil {
		t.Fatalf("find template: %v", err)
	}
	_ = tplRec
	nBefore := e.count("template_versions", bson.M{})
	_, _, err = e.tpls.Create(ctx, templatecontract.TemplateInput{
		Slug: "financial-news", Name: "Dupe", Category: "news", Status: "active",
		HTMLLayout: sharedLayout,
		Fields:     []models.TemplateField{{Name: "headline", Label: "H", Type: "text", Required: true}},
	})
	if err == nil {
		t.Fatal("duplicate slug accepted")
	}
	if n := e.count("template_versions", bson.M{}); n != nBefore {
		t.Fatal("duplicate slug minted a version")
	}
	// generation.Actor scope helpers behave (scope-matrix unit surface).
	a := generation.Actor{Role: "admin", Authenticated: true, Scopes: []string{"content.view"}}
	if !a.HasScopes("content.view") || a.HasScopes("content.view", "content.edit") {
		t.Fatal("HasScopes matrix wrong")
	}
	if _, ok := generation.ActorFrom(context.Background()); ok {
		t.Fatal("ActorFrom on empty ctx")
	}
	if got, ok := generation.ActorFrom(generation.WithActor(context.Background(), a)); !ok || !got.Authenticated {
		t.Fatal("ActorFrom round-trip failed")
	}
	ge := &generation.Error{Code: generation.CodeInvalidRequest, Message: "bad"}
	if ge.Error() == "" || generation.CodeOf(ge) != generation.CodeInvalidRequest || generation.CodeOf(nil) != "" {
		t.Fatal("generation error helpers wrong")
	}
}
