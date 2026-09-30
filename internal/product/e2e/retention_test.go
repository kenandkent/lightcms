// Task 17 E2E retention/repair rows (spec §17.6, §39.11): GC policy,
// stale-stage reaping, unpublish convergence and the outbox run loop.
package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/storage"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// TestE2E_RetentionGC proves the storage lifecycle (§17.6, §39.11): expired
// superseded/failed objects are deleted (storage before record), active and
// pinned objects are never GC'd.
func TestE2E_RetentionGC(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")
	_, r1 := e.publish(t, "gc-1", idemBody("gc-page", "v1", false))
	_, r2 := e.publish(t, "gc-2", idemBody("gc-page", "v2", true))
	cid := mustOID(t, strOf(r2, "id"))
	pid1 := mustOID(t, strOf(r1, "publication_id"))
	pid2 := mustOID(t, strOf(r2, "publication_id"))
	ctx := context.Background()

	immPath := func(pid primitive.ObjectID) string {
		return e.store.ImmutablePath(cid, pid)
	}
	if _, err := os.Stat(immPath(pid1)); err != nil {
		t.Fatalf("v1 immutable missing before GC: %v", err)
	}
	// Expire v1 (superseded 100d ago) and sweep.
	if _, err := e.db.Collection("content_publications").UpdateOne(ctx, bson.M{"_id": pid1},
		bson.M{"$set": bson.M{"superseded_at": time.Now().Add(-2400 * time.Hour)}}); err != nil {
		t.Fatalf("backdate v1: %v", err)
	}
	rep, err := e.scanner.SweepRetention(ctx)
	if err != nil {
		t.Fatalf("SweepRetention: %v", err)
	}
	if _, err := os.Stat(immPath(pid1)); !os.IsNotExist(err) {
		t.Fatal("expired superseded immutable object not deleted")
	}
	var v1rec publication.Publication
	if err := e.db.FindOne(ctx, "content_publications", bson.M{"_id": pid1}, &v1rec); err != nil {
		t.Fatalf("v1 record: %v", err)
	}
	if v1rec.StorageState != publication.StorageDeleted {
		t.Fatalf("v1 storage_state = %q, want deleted", v1rec.StorageState)
	}
	// Active v2 untouched and still served.
	if _, err := os.Stat(immPath(pid2)); err != nil {
		t.Fatalf("active immutable deleted by GC: %v", err)
	}
	u := strings.TrimPrefix(strOf(r2, "public_url"), e.base)
	if c, body := e.getAnon(u); c != 200 || !strings.Contains(string(body), "v2") {
		t.Fatalf("live damaged by GC: %d %s", c, body)
	}

	// Pinned v2 survives even when expired: publish v3, pin v2, expire it.
	_, r3 := e.publish(t, "gc-3", idemBody("gc-page", "v3", true))
	_ = r3
	if _, err := e.db.Collection("content_publications").UpdateOne(ctx, bson.M{"_id": pid2},
		bson.M{"$set": bson.M{"pinned": true, "superseded_at": time.Now().Add(-2400 * time.Hour)}}); err != nil {
		t.Fatalf("pin v2: %v", err)
	}
	if _, err := e.scanner.SweepRetention(ctx); err != nil {
		t.Fatalf("sweep2: %v", err)
	}
	if _, err := os.Stat(immPath(pid2)); err != nil {
		t.Fatalf("pinned object GC'd: %v", err)
	}

	// Failed objects expire after 7 days: fail one, backdate, sweep.
	e.refault(envOpts{maxWriteBytes: 1})
	_, _ = e.publish(t, "gc-fail", idemBody("gc-page", "v4", true))
	e.refault(envOpts{})
	var failed publication.Publication
	if err := e.db.FindOne(ctx, "content_publications",
		bson.M{"content_id": cid, "status": string(publication.StatusFailed)}, &failed); err != nil {
		t.Fatalf("failed record: %v", err)
	}
	if _, err := e.db.Collection("content_publications").UpdateOne(ctx, bson.M{"_id": failed.ID},
		bson.M{"$set": bson.M{"created_at": time.Now().Add(-240 * time.Hour)}}); err != nil {
		t.Fatalf("backdate failed: %v", err)
	}
	rep, err = e.scanner.SweepRetention(ctx)
	if err != nil {
		t.Fatalf("sweep3: %v", err)
	}
	_ = rep
	var failedAfter publication.Publication
	if err := e.db.FindOne(ctx, "content_publications", bson.M{"_id": failed.ID}, &failedAfter); err != nil {
		t.Fatalf("failed record after: %v", err)
	}
	if failedAfter.StorageState != publication.StorageDeleted {
		t.Fatalf("expired failed storage_state = %q, want deleted", failedAfter.StorageState)
	}
}

// TestE2E_ScannerRunLoop proves the durable scanner entry point scans on
// start and stops with its context (covers Run/scanInterval/observeScan).
func TestE2E_ScannerRunLoop(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	e.scanner.Run(ctx)
	e.scanner.ResetDegraded()
	if e.scanner.Degraded() {
		t.Fatal("degraded after clean run")
	}
}

// TestE2E_StaleStageReap proves the scanner reaps staged objects past the
// stage timeout (crashed-before-activation leftovers): record failed,
// staged bytes aborted.
func TestE2E_StaleStageReap(t *testing.T) {
	e := newEnv(t, envOpts{scanTimeout: time.Millisecond})
	e.seedTemplate(t, "financial-news")
	code, draft := e.postJSON("/api/v1/page-generation", map[string]any{
		"template": "financial-news", "title": "Stale", "slug": "stale-stage",
		"folder_path": "/news", "mode": "draft", "data": map[string]any{"headline": "s"},
	}, nil)
	if code != 201 {
		t.Fatalf("draft = %d (%v)", code, draft)
	}
	cid := mustOID(t, strOf(draft, "id"))
	ctx := context.Background()

	// Plant a staged record + staged bytes (crashed-saga shape).
	staleID := primitive.NewObjectID()
	html := []byte("<html>stale stage</html>")
	sum := sha256.Sum256(html)
	if err := e.repo.InsertStaged(ctx, &publication.Publication{
		ID: staleID, ContentID: cid, ContentVersion: 1,
		FullPath: "/news/stale-stage", ContentHash: "sha256:" + hex.EncodeToString(sum[:]),
		LogicalPublishedAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("InsertStaged: %v", err)
	}
	staged, err := e.store.Stage(ctx, storage.StageRequest{
		ContentID: cid, PublicationID: staleID, CanonicalPath: "/news/stale-stage",
		HTML: html, ExpectedSHA256: hex.EncodeToString(sum[:]),
	})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	_ = staged
	if _, err := e.db.Collection("content_publications").UpdateOne(ctx, bson.M{"_id": staleID},
		bson.M{"$set": bson.M{"created_at": time.Now().Add(-time.Hour)}}); err != nil {
		t.Fatalf("backdate staged: %v", err)
	}
	rep, err := e.scanner.ScanOnce(ctx)
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if rep.StaleStagedFailed != 1 {
		t.Fatalf("StaleStagedFailed = %d, want 1 (%+v)", rep.StaleStagedFailed, rep)
	}
	if _, err := os.Stat(e.store.ImmutablePath(cid, staleID)); !os.IsNotExist(err) {
		t.Fatal("stale staged bytes not aborted")
	}
	var rec publication.Publication
	if err := e.db.FindOne(ctx, "content_publications", bson.M{"_id": staleID}, &rec); err != nil {
		t.Fatalf("staged record: %v", err)
	}
	if rec.Status != publication.StatusFailed {
		t.Fatalf("staged record status = %q, want failed", rec.Status)
	}
}

// TestE2E_UnpublishMissingCanonical proves unpublish converges when the
// canonical is already absent (backup-missing branch): control plane flips,
// no resurrection, scanner stays clean.
func TestE2E_UnpublishMissingCanonical(t *testing.T) {
	e, cid, _, u := faultSetup(t, envOpts{})
	p, err := e.store.CanonicalFilePath("/news/fault-page")
	if err != nil {
		t.Fatalf("canonical path: %v", err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatalf("remove canonical: %v", err)
	}
	code, resp := e.postJSON("/api/v1/content/"+cid.Hex()+"/unpublish", map[string]any{}, nil)
	if code != 200 {
		t.Fatalf("unpublish without canonical = %d (%v)", code, resp)
	}
	if active := e.activePub(t, cid); active != nil {
		t.Fatal("active remains after unpublish")
	}
	if c, _ := e.getAnon(u); c != 404 {
		t.Fatalf("page resurrected: %d", c)
	}
	if rep, err := e.scanner.ScanOnce(context.Background()); err != nil || rep.RepairErrors != 0 {
		t.Fatalf("scan = %+v, %v", rep, err)
	}
}

// TestE2E_OutboxWorkerRun proves the in-process worker drains the backlog
// (Run loop + Backlog gauge) with at-least-once delivery.
func TestE2E_OutboxWorkerRun(t *testing.T) {
	e, cid, _, _ := faultSetup(t, envOpts{})
	ctx := context.Background()
	if n, err := e.outbox.Backlog(ctx); err != nil || n != 1 {
		t.Fatalf("Backlog = %d, %v (want 1)", n, err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.outbox.Run(runCtx); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		e.mu.Lock()
		n := len(e.deliveries)
		e.mu.Unlock()
		if n >= 1 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("worker did not deliver within 10s")
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done
	if n, err := e.outbox.Backlog(ctx); err != nil || n != 0 {
		t.Fatalf("Backlog after run = %d, %v (want 0)", n, err)
	}
	_ = cid
}
