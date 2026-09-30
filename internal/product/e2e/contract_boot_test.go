// Task 17C: build-provenance, scheduler crash-lease, and outbox-enrichment
// verdicts. New symbols are Con*-prefixed: existing e2e files belong to
// Task 17A and must not be edited.
//
// Red-green note: pure-verdict locks (behavior already shipped by Task 16).
// Green-on-first-run is expected and stated per verdict; each assertion
// fails on regression (default SHA leaking into records, ldflag wiring
// drift, takeover converging silently, enrichment dropping public_url).
package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/product/idempotency"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// TestConBuildSHAProvenance locks verdict 3: ProductBuildSHA reports the
// ldflags git SHA in the built binary, not the default version string
// (Task 7/16 handoff). Two halves:
//  1. Record half: a publication minted by an instance wired with an
//     explicit SHA stamps exactly that SHA — never the publication-package
//     dev default ("7.2.2") and never empty.
//  2. Binary half: building ./cmd/server with
//     -ldflags "-X main.ProductBuildSHA=<sha>" embeds the marker in the
//     binary, and the Dockerfile release wiring still stamps the variable.
func TestConBuildSHAProvenance(t *testing.T) {
	e := newEnv(t, envOpts{buildSHA: "con-17c-sha"})
	e.seedTemplate(t, "financial-news")
	code, resp := e.publish(t, "con-sha-1", map[string]any{
		"template": "financial-news", "title": "Con SHA", "slug": "con-sha-page",
		"folder_path": "/news", "mode": "publish", "expected_template_version": int64(1),
		"data": map[string]any{"headline": "sha headline"},
	})
	if code != 201 {
		t.Fatalf("publish = %d (%v)", code, resp)
	}
	cid, _ := primitive.ObjectIDFromHex(strOf(resp, "id"))
	active := e.activePub(t, cid)
	if active == nil {
		t.Fatal("no active publication")
	}
	if active.ProductBuildSHA != "con-17c-sha" {
		t.Fatalf("record product_build_sha = %q, want wired instance SHA", active.ProductBuildSHA)
	}
	if active.ProductBuildSHA == publication.ProductBuildSHA {
		t.Fatalf("record carries the dev default %q (16E plumbing broken)", publication.ProductBuildSHA)
	}
	if active.RendererVersion == "" {
		t.Fatal("renderer_version not recorded alongside build SHA")
	}

	// Binary half: the exact release mechanism (Dockerfile ldflag) stamps
	// the marker into the built server binary.
	root := ConModuleRoot(t)
	out := filepath.Join(t.TempDir(), "lightcms-con-17c")
	cmd := exec.Command("go", "build", "-ldflags", "-X main.ProductBuildSHA=con-17c-ldflag-pin", "-o", out, "./cmd/server")
	cmd.Dir = root
	if bout, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build pinned server binary: %v %s", err, bout)
	}
	raw, rerr := os.ReadFile(out)
	if rerr != nil {
		t.Fatalf("read pinned binary: %v", rerr)
	}
	if !strings.Contains(string(raw), "con-17c-ldflag-pin") {
		t.Fatal("pinned ProductBuildSHA missing from server binary (ldflag wiring broken)")
	}
	df, derr := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if derr != nil {
		t.Fatalf("read Dockerfile: %v", derr)
	}
	if !strings.Contains(string(df), "-X main.ProductBuildSHA=") {
		t.Fatal("Dockerfile no longer stamps main.ProductBuildSHA (Task 7/16 release wiring drift)")
	}
	t.Logf("VERDICT 3 LOCKED: record stamps wired SHA (not %q); ldflag marker present in built binary; Dockerfile wiring intact.", publication.ProductBuildSHA)
}

// TestConSchedulerCrashTakeover locks verdict 5: what the scheduler-tick
// crash-lease TakeOver path does TODAY. Observed behavior (Task 16, still
// present at internal/services/publish_internal.go `op, terr :=
// TakeOverByKey(...)` inside the lease-expired case, shadowing the outer
// op): a scheduler tick that takes over a crashed (expired-lease) attempt
// via PublishInternal FAILS with IDEMPOTENCY_NOT_FOUND — the taken-over
// operation is discarded and the saga receives the zero op ID. The failed
// tick mints nothing, and the recovery scanner takes NO lease role
// (file-level repair only): ScanOnce reports a clean pass with nothing to
// repair.
//
// Recorded gap (not fixed here — services/ is frozen for 17C): the crashed
// scheduler attempt never converges via PublishInternal (the discarded
// takeover already refreshed the lease, so the next tick sees IN_PROGRESS
// → retry-later until it lapses again). Specified fix: assign the outer op
// (`op, terr = ...`). Task 19 must decide: fix-and-retry vs. scanner-owned
// lease repair. This test fails if the behavior changes either way
// (silent convergence OR a new error code), forcing an explicit decision.
func TestConSchedulerCrashTakeover(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")
	ctx := context.Background()

	// Draft page over HTTP; scheduler ticks use a stable key per version.
	code, draft := e.postJSON("/api/v1/page-generation", map[string]any{
		"template": "financial-news", "title": "Con Takeover", "slug": "con-takeover-page",
		"folder_path": "/news", "mode": "draft", "data": map[string]any{"headline": "takeover"},
	}, nil)
	if code != 201 {
		t.Fatalf("draft = %d (%v)", code, draft)
	}
	cid, _ := primitive.ObjectIDFromHex(strOf(draft, "id"))
	path := "/internal/scheduler/publish"
	key := "scheduler/" + cid.Hex() + "/v1"

	// Tick 1 begins the attempt, then the worker "crashes": force the lease
	// into the past so the next tick hits the takeover branch.
	op, err := e.idem.Begin(ctx, "scheduler", "POST", path, key, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	_ = op
	if _, err := e.db.Collection("idempotency_records").UpdateOne(ctx,
		bson.M{"owner": "scheduler", "path": path, "key": key},
		bson.M{"$set": bson.M{"processing_expires_at": time.Now().Add(-time.Second)}}); err != nil {
		t.Fatalf("expire lease: %v", err)
	}

	// Tick 2 (takeover): lock the CURRENT observed failure.
	err = e.contentSvc.PublishInternal(ctx, cid, "scheduler", path, key)
	if err == nil {
		t.Fatal("takeover tick converged — behavior changed (shadowing fix landed without a verdict update); revise this lock and record the Task 19 decision")
	}
	if got := publication.CodeOf(err); got != idempotency.CodeNotFound {
		t.Fatalf("takeover tick = %v (code %q), want IDEMPOTENCY_NOT_FOUND (observed behavior changed)", err, got)
	}

	// Zero side effects: the failed tick minted no publication.
	if n := e.count("content_publications", bson.M{"content_id": cid}); n != 0 {
		t.Fatalf("takeover tick minted %d publications, want 0", n)
	}
	if active := e.activePub(t, cid); active != nil {
		t.Fatalf("takeover tick activated %s, want no active publication", active.ID.Hex())
	}

	// Scanner side: no lease ownership — ScanOnce neither repairs nor
	// reports anything for the crashed scheduler attempt (file tree is
	// untouched: draft-only page, no canonical, no active record).
	rep, serr := e.scanner.ScanOnce(ctx)
	if serr != nil {
		t.Fatalf("ScanOnce: %v", serr)
	}
	if rep.RepairErrors != 0 || rep.InspectErrors != 0 {
		t.Fatalf("scanner errors after crashed tick: %+v", rep)
	}
	if rep.ImmutableMissingP0 != 0 || rep.ImmutableCorruptP0 != 0 || rep.MultipleActiveP0 != 0 {
		t.Fatalf("scanner P0 after crashed tick: %+v", rep)
	}
	if rep.ContentsSkippedLocked != 0 {
		t.Fatalf("scanner skipped locked content (no saga holds a lock): %+v", rep)
	}
	t.Logf("VERDICT 5 LOCKED: PublishInternal crash-takeover fails IDEMPOTENCY_NOT_FOUND with zero side effects; scanner is file-level only (clean pass, no lease role). Gap recorded: attempt never converges — Task 19 decision required.")
}

// TestConOutboxPublicURLDelivered locks verdict 6 end to end: publish via
// HTTP, drain the outbox through the worker, and assert the DELIVERED
// webhook payload carries public_url present and correct — equal to the
// publish-response URL and the active record URL, with the activation IDs
// joining it to the publication.
func TestConOutboxPublicURLDelivered(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")
	ctx := context.Background()

	code, resp := e.publish(t, "con-outbox-1", map[string]any{
		"template": "financial-news", "title": "Con Outbox", "slug": "con-outbox-page",
		"folder_path": "/news", "mode": "publish", "expected_template_version": int64(1),
		"data": map[string]any{"headline": "outbox headline"},
	})
	if code != 201 {
		t.Fatalf("publish = %d (%v)", code, resp)
	}
	pubID := strOf(resp, "publication_id")
	contentID := strOf(resp, "id")
	publicURL := strOf(resp, "public_url")
	if pubID == "" || contentID == "" || publicURL == "" {
		t.Fatalf("publish missing ids/url: %v", resp)
	}
	pid, _ := primitive.ObjectIDFromHex(pubID)
	cid, _ := primitive.ObjectIDFromHex(contentID)

	// Drain through the worker (the production delivery path), not by
	// reading the row: this proves what downstream webhooks receive.
	drained := 0
	for i := 0; i < 25; i++ {
		did, derr := e.outbox.ProcessNext(ctx)
		if derr != nil {
			t.Fatalf("ProcessNext: %v", derr)
		}
		if did {
			drained++
			continue
		}
		break
	}
	if drained == 0 {
		t.Fatal("worker delivered nothing")
	}

	// Find the delivered content.publish event by its stable event ID.
	wantEventID := publication.EventID(publication.EventPublished, pid)
	e.mu.Lock()
	deliveries := append([]deliveredEvent{}, e.deliveries...)
	e.mu.Unlock()
	var payload map[string]any
	for _, d := range deliveries {
		if d.Type == publication.EventPublished && d.ID == wantEventID {
			payload = d.Payload
			break
		}
	}
	if payload == nil {
		t.Fatalf("no delivered %s event with id %s (%d deliveries)", publication.EventPublished, wantEventID, len(deliveries))
	}
	gotURL, _ := payload["public_url"].(string)
	if gotURL == "" {
		t.Fatalf("delivered payload lacks public_url: %v", payload)
	}
	if gotURL != publicURL {
		t.Fatalf("delivered public_url %q != publish-response %q (enrichment wrong)", gotURL, publicURL)
	}
	if active := e.activePub(t, cid); active == nil || active.PublicURL != gotURL {
		t.Fatalf("delivered public_url %q != active record (enrichment diverged)", gotURL)
	}
	if payload["content_id"] != contentID || payload["publication_id"] != pubID {
		t.Fatalf("delivered payload IDs do not join the activation: %v", payload)
	}
	if fp, _ := payload["full_path"].(string); fp != "/news/con-outbox-page" {
		t.Fatalf("delivered full_path = %q", fp)
	}
	t.Logf("VERDICT 6 LOCKED: delivered %s carries public_url %q == response == active record.", publication.EventPublished, gotURL)
}
