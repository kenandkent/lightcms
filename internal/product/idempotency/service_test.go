package idempotency

// Task 11 red test: replay matrix, lease takeover with stable business
// attempt, execution-snapshot crash reuse, terminal-retry new Publication ID,
// and transactional BindContentAndVersion coupling (spec §21, ADR-005).
//
// Expected red result: this package does not implement Service yet, so this
// file must fail to compile until model.go / repository.go / service.go land.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/database"
	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

func newTestService(t *testing.T) (*Service, *database.DB) {
	t.Helper()
	db, _ := testutil.MustConnectTestDB(t)
	ctx := context.Background()
	if err := db.EnsureProductIndexes(ctx); err != nil {
		t.Fatalf("EnsureProductIndexes: %v", err)
	}
	svc, err := NewService(db, Options{TTLHours: 24, LeaseMinutes: 5})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, db
}

func expireLease(t *testing.T, svc *Service, opID primitive.ObjectID) {
	t.Helper()
	ctx := context.Background()
	res, err := svc.repo.coll.UpdateOne(ctx,
		bson.M{"_id": opID},
		bson.M{"$set": bson.M{"processing_expires_at": time.Now().Add(-time.Minute)}})
	if err != nil {
		t.Fatalf("expireLease: %v", err)
	}
	if res.MatchedCount != 1 {
		t.Fatalf("expireLease: matched %d, want 1", res.MatchedCount)
	}
}

func mustBegin(t *testing.T, svc *Service, owner, key string, body []byte) Operation {
	t.Helper()
	op, err := svc.Begin(context.Background(), owner, "POST", "/api/v1/page-generation", key, body)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if op.Replay {
		t.Fatalf("Begin: unexpected replay on first call")
	}
	return op
}

// TestReplayMatrix covers ADR-005 D1/D2: same key + same body replays a
// completed 2xx; changed body conflicts (409); 401/403/404/409/429/5xx never
// complete; pure 400/422 replays.
func TestReplayMatrix(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	body := []byte(`{"title":"Hello","slug":"hello"}`)

	// Empty key is rejected before any record exists.
	if _, err := svc.Begin(ctx, "owner-1", "POST", "/api/v1/page-generation", "", body); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("empty key: got %v, want %s", err, CodeInvalidRequest)
	}

	op := mustBegin(t, svc, "owner-1", "key-replay-1", body)
	if op.Attempt != 1 {
		t.Fatalf("first Begin attempt = %d, want 1", op.Attempt)
	}

	// Same key + same body while processing with a live lease: in progress.
	if _, err := svc.Begin(ctx, "owner-1", "POST", "/api/v1/page-generation", "key-replay-1", body); CodeOf(err) != CodeInProgress {
		t.Fatalf("processing repeat: got %v, want %s", err, CodeInProgress)
	}

	// Same key + changed body while processing: conflict, no side effect.
	changed := []byte(`{"title":"Hello changed","slug":"hello"}`)
	if _, err := svc.Begin(ctx, "owner-1", "POST", "/api/v1/page-generation", "key-replay-1", changed); CodeOf(err) != CodeConflict {
		t.Fatalf("changed body: got %v, want %s", err, CodeConflict)
	}

	// Semantically identical body (whitespace/key order) matches the hash.
	same := []byte(`{ "slug" : "hello" , "title" : "Hello" }`)
	if _, err := svc.Begin(ctx, "owner-1", "POST", "/api/v1/page-generation", "key-replay-1", same); CodeOf(err) != CodeInProgress {
		t.Fatalf("canonical-equal body: got %v, want %s", err, CodeInProgress)
	}

	// Complete with 2xx, then replay returns the same status/body.
	resp := map[string]any{"publication_id": "pub-1", "public_url": "https://example.com/news/hello"}
	if _, err := svc.Complete(ctx, op.ID, op.Attempt, 201, resp, false); err != nil {
		t.Fatalf("Complete 201: %v", err)
	}
	replayed, err := svc.Begin(ctx, "owner-1", "POST", "/api/v1/page-generation", "key-replay-1", body)
	if err != nil {
		t.Fatalf("replay Begin: %v", err)
	}
	if !replayed.Replay {
		t.Fatalf("completed repeat: Replay=false, want true")
	}
	if replayed.StatusCode != 201 {
		t.Fatalf("replay status = %d, want 201", replayed.StatusCode)
	}
	if replayed.Response["publication_id"] != "pub-1" {
		t.Fatalf("replay body = %v, want publication_id pub-1", replayed.Response)
	}

	// Changed body against a completed record still conflicts.
	if _, err := svc.Begin(ctx, "owner-1", "POST", "/api/v1/page-generation", "key-replay-1", changed); CodeOf(err) != CodeConflict {
		t.Fatalf("changed body after completion: got %v, want %s", err, CodeConflict)
	}

	// Different owner/method/path/key tuples are independent operations.
	op2, err := svc.Begin(ctx, "owner-2", "POST", "/api/v1/page-generation", "key-replay-1", body)
	if err != nil || op2.Replay {
		t.Fatalf("different owner: op=%v err=%v, want fresh operation", op2.ID.Hex(), err)
	}

	// Non-cacheable statuses never become completed; the lease is released
	// so the caller can fix state and retry the same attempt via takeover.
	for _, code := range []int{401, 403, 404, 409, 429, 500, 503} {
		key := "key-nocache-" + string(rune('0'+code%10)) + "-" + string(rune('0'+(code/10)%10)) + string(rune('0'+(code/100)%10))
		b := []byte(`{"title":"t"}`)
		o := mustBegin(t, svc, "owner-nc", key, b)
		if _, err := svc.Complete(ctx, o.ID, o.Attempt, code, map[string]any{"error": "x"}, false); err != nil {
			t.Fatalf("Complete %d: %v", code, err)
		}
		got, err := svc.Get(ctx, o.ID)
		if err != nil {
			t.Fatalf("Get after Complete %d: %v", code, err)
		}
		if got.State == StateCompleted {
			t.Fatalf("status %d became completed, want never-completed", code)
		}
		if _, err := svc.Begin(ctx, "owner-nc", "POST", "/api/v1/page-generation", key, b); CodeOf(err) != CodeLeaseExpired {
			t.Fatalf("status %d retry: got %v, want %s (lease released, same attempt resumable)", code, err, CodeLeaseExpired)
		}
		// Takeover resumes the SAME attempt.
		taken, err := svc.TakeOver(ctx, o.ID, got.LeaseGeneration)
		if err != nil {
			t.Fatalf("TakeOver after %d: %v", code, err)
		}
		if taken.Attempt != 1 {
			t.Fatalf("TakeOver after %d: attempt=%d, want 1", code, taken.Attempt)
		}
	}

	// Pure validation 422 completes and replays.
	vop := mustBegin(t, svc, "owner-v", "key-validation", []byte(`{"title":""}`))
	if _, err := svc.Complete(ctx, vop.ID, vop.Attempt, 422, map[string]any{"code": "FIELD_VALIDATION_FAILED"}, true); err != nil {
		t.Fatalf("CompleteValidation 422: %v", err)
	}
	vr, err := svc.Begin(ctx, "owner-v", "POST", "/api/v1/page-generation", "key-validation", []byte(`{"title":""}`))
	if err != nil || !vr.Replay || vr.StatusCode != 422 {
		t.Fatalf("422 replay: replay=%v status=%d err=%v", vr.Replay, vr.StatusCode, err)
	}

	// Non-pure 422 (validationOnly=false) must NOT complete.
	nop := mustBegin(t, svc, "owner-v", "key-nonpure", []byte(`{"title":""}`))
	if _, err := svc.Complete(ctx, nop.ID, nop.Attempt, 422, map[string]any{"code": "X"}, false); err != nil {
		t.Fatalf("Complete non-pure 422: %v", err)
	}
	if got, _ := svc.Get(ctx, nop.ID); got.State == StateCompleted {
		t.Fatalf("non-pure 422 became completed, want processing")
	}
}

// TestReplayConcurrentBegin: parallel same-key beginners yield exactly one
// executing owner; the rest see REQUEST_IN_PROGRESS.
func TestReplayConcurrentBegin(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	body := []byte(`{"title":"race"}`)
	const n = 8
	var wg sync.WaitGroup
	owners := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := svc.Begin(ctx, "owner-race", "POST", "/api/v1/page-generation", "key-race", body)
			owners[i] = err
		}(i)
	}
	wg.Wait()
	executed, inProgress := 0, 0
	for _, err := range owners {
		if err == nil {
			executed++
		} else if CodeOf(err) == CodeInProgress {
			inProgress++
		} else {
			t.Fatalf("unexpected Begin error: %v", err)
		}
	}
	if executed != 1 || inProgress != n-1 {
		t.Fatalf("executed=%d inProgress=%d, want 1/%d", executed, inProgress, n-1)
	}
}

// TestLeaseExpiryTakeoverStableAttempt: lease_generation CAS changes while the
// business attempt stays stable (ADR-005 D3).
func TestLeaseExpiryTakeoverStableAttempt(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	op := mustBegin(t, svc, "owner-l", "key-lease", []byte(`{"a":1}`))

	// Takeover while the lease is still valid is rejected.
	if _, err := svc.TakeOver(ctx, op.ID, op.LeaseGeneration); CodeOf(err) != CodeLeaseActive {
		t.Fatalf("early TakeOver: got %v, want %s", err, CodeLeaseActive)
	}
	// Wrong generation is rejected even after expiry.
	expireLease(t, svc, op.ID)
	if _, err := svc.TakeOver(ctx, op.ID, op.LeaseGeneration+99); CodeOf(err) != CodeLeaseLost {
		t.Fatalf("wrong-generation TakeOver: got %v, want %s", err, CodeLeaseLost)
	}

	taken, err := svc.TakeOver(ctx, op.ID, op.LeaseGeneration)
	if err != nil {
		t.Fatalf("TakeOver: %v", err)
	}
	if taken.LeaseGeneration != op.LeaseGeneration+1 {
		t.Fatalf("generation = %d, want %d", taken.LeaseGeneration, op.LeaseGeneration+1)
	}
	if taken.Attempt != op.Attempt {
		t.Fatalf("attempt changed on takeover: %d -> %d, want stable", op.Attempt, taken.Attempt)
	}

	// The stale worker's heartbeat is rejected; the new owner can renew.
	if _, err := svc.RenewLease(ctx, op.ID, op.Attempt, op.LeaseGeneration); CodeOf(err) != CodeLeaseLost {
		t.Fatalf("stale RenewLease: got %v, want %s", err, CodeLeaseLost)
	}
	renewed, err := svc.RenewLease(ctx, op.ID, taken.Attempt, taken.LeaseGeneration)
	if err != nil {
		t.Fatalf("RenewLease: %v", err)
	}
	if renewed.LeaseGeneration != taken.LeaseGeneration {
		t.Fatalf("RenewLease changed generation: %d -> %d", taken.LeaseGeneration, renewed.LeaseGeneration)
	}
	if !renewed.LeaseExpiresAt.After(taken.LeaseExpiresAt.Add(-time.Minute)) {
		t.Fatalf("RenewLease did not extend the lease")
	}
}

// TestLeaseHeartbeatPreventsTakeover: a 60s heartbeat keeps the lease alive.
func TestLeaseHeartbeatPreventsTakeover(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	op := mustBegin(t, svc, "owner-hb", "key-hb", []byte(`{"a":1}`))
	expireLease(t, svc, op.ID)
	// Heartbeat is only valid for the current generation; simulate the owner
	// renewing just before expiry by taking over first, then heartbeating.
	taken, err := svc.TakeOver(ctx, op.ID, op.LeaseGeneration)
	if err != nil {
		t.Fatalf("TakeOver: %v", err)
	}
	if _, err := svc.RenewLease(ctx, taken.ID, taken.Attempt, taken.LeaseGeneration); err != nil {
		t.Fatalf("RenewLease: %v", err)
	}
	if _, err := svc.TakeOver(ctx, taken.ID, taken.LeaseGeneration); CodeOf(err) != CodeLeaseActive {
		t.Fatalf("TakeOver after heartbeat: got %v, want %s", err, CodeLeaseActive)
	}
	if HeartbeatInterval != 60*time.Second {
		t.Fatalf("HeartbeatInterval = %v, want 60s (spec §21.5)", HeartbeatInterval)
	}
}

// TestExecutionSnapshotCrashReuse: crash before Render reuses the durable
// Publication ID + logical time, never allocating a second one (spec §16.1).
func TestExecutionSnapshotCrashReuse(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	op := mustBegin(t, svc, "owner-s", "key-snap", []byte(`{"title":"snap"}`))

	contentID := primitive.NewObjectID()
	tmplV := primitive.NewObjectID()
	pubID := primitive.NewObjectID()
	logicalAt := time.Now().UTC().Truncate(time.Millisecond)

	// Freeze before Render must fail when nothing is bound yet? No — Freeze
	// requires no prior bind; it CAS-persists the snapshot. Bind first anyway
	// to mirror the saga order (bind in content txn, freeze before render).
	if err := svc.BindContentAndVersion(ctx, nil, op.ID, contentID, 3, "/news/snap"); err != nil {
		t.Fatalf("BindContentAndVersion: %v", err)
	}
	if err := svc.FreezeExecution(ctx, op.ID, 1, pubID, logicalAt, tmplV); err != nil {
		t.Fatalf("FreezeExecution: %v", err)
	}

	// Simulate a crash: lease expires, a new worker takes over the SAME
	// attempt and must observe the identical snapshot.
	expireLease(t, svc, op.ID)
	got, err := svc.Get(ctx, op.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	taken, err := svc.TakeOver(ctx, op.ID, got.LeaseGeneration)
	if err != nil {
		t.Fatalf("TakeOver: %v", err)
	}
	if taken.Attempt != 1 {
		t.Fatalf("takeover attempt = %d, want 1", taken.Attempt)
	}
	if taken.PublicationID == nil || *taken.PublicationID != pubID {
		t.Fatalf("takeover publication = %v, want %s", taken.PublicationID, pubID.Hex())
	}
	if taken.LogicalAt == nil || !taken.LogicalAt.Equal(logicalAt) {
		t.Fatalf("takeover logicalAt = %v, want %v", taken.LogicalAt, logicalAt)
	}
	if taken.ContentID == nil || *taken.ContentID != contentID || taken.ContentVersion != 3 {
		t.Fatalf("takeover content binding lost: %+v", taken)
	}

	// Re-freezing the same values is idempotent; freezing a different
	// Publication ID for the same attempt conflicts.
	if err := svc.FreezeExecution(ctx, op.ID, 1, pubID, logicalAt, tmplV); err != nil {
		t.Fatalf("idempotent re-freeze: %v", err)
	}
	if err := svc.FreezeExecution(ctx, op.ID, 1, primitive.NewObjectID(), logicalAt, tmplV); CodeOf(err) != CodeConflict {
		t.Fatalf("divergent re-freeze: got %v, want %s", err, CodeConflict)
	}

	// Complete and replay keep pointing at the frozen Publication ID.
	if _, err := svc.Complete(ctx, op.ID, 1, 201, map[string]any{"publication_id": pubID.Hex()}, false); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	rp, err := svc.Begin(ctx, "owner-s", "POST", "/api/v1/page-generation", "key-snap", []byte(`{"title":"snap"}`))
	if err != nil || !rp.Replay || rp.Response["publication_id"] != pubID.Hex() {
		t.Fatalf("post-crash replay: replay=%v resp=%v err=%v", rp.Replay, rp.Response, err)
	}
}

// TestExecutionSnapshotTerminalRetryNewPublication: a terminal
// pre-activation failure marks the attempt terminal; the same-key retry
// increments the attempt and allocates a NEW Publication ID (ADR-005 D5).
func TestExecutionSnapshotTerminalRetryNewPublication(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	body := []byte(`{"title":"term"}`)
	op := mustBegin(t, svc, "owner-t", "key-term", body)

	pub1 := primitive.NewObjectID()
	logical1 := time.Now().UTC().Truncate(time.Millisecond)
	if err := svc.FreezeExecution(ctx, op.ID, 1, pub1, logical1, primitive.NewObjectID()); err != nil {
		t.Fatalf("FreezeExecution: %v", err)
	}
	if _, err := svc.MarkTerminal(ctx, op.ID, 1, "RENDER_FAILED"); err != nil {
		t.Fatalf("MarkTerminal: %v", err)
	}

	// Takeover of a terminal attempt is rejected; only Begin-with-retry
	// allocates the next attempt.
	got, _ := svc.Get(ctx, op.ID)
	if _, err := svc.TakeOver(ctx, op.ID, got.LeaseGeneration); CodeOf(err) != CodeStaleAttempt {
		t.Fatalf("TakeOver on terminal: got %v, want %s", err, CodeStaleAttempt)
	}

	retry, err := svc.Begin(ctx, "owner-t", "POST", "/api/v1/page-generation", "key-term", body)
	if err != nil {
		t.Fatalf("terminal retry Begin: %v", err)
	}
	if retry.Replay {
		t.Fatalf("terminal retry must not replay")
	}
	if retry.Attempt != 2 {
		t.Fatalf("retry attempt = %d, want 2", retry.Attempt)
	}
	if retry.PublicationID != nil {
		t.Fatalf("new attempt must start without a frozen publication, got %v", retry.PublicationID)
	}

	pub2 := primitive.NewObjectID()
	logical2 := logical1.Add(time.Second)
	if pub2 == pub1 {
		t.Fatal("test setup: pub IDs collided")
	}
	if err := svc.FreezeExecution(ctx, op.ID, 2, pub2, logical2, primitive.NewObjectID()); err != nil {
		t.Fatalf("FreezeExecution attempt 2: %v", err)
	}
	// Stale attempt freeze is rejected.
	if err := svc.FreezeExecution(ctx, op.ID, 1, pub1, logical1, primitive.NewObjectID()); CodeOf(err) != CodeStaleAttempt {
		t.Fatalf("stale freeze: got %v, want %s", err, CodeStaleAttempt)
	}

	final, err := svc.Get(ctx, op.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(final.Attempts) != 2 {
		t.Fatalf("attempt history = %d entries, want 2", len(final.Attempts))
	}
	if final.Attempts[0].PublicationID == nil || *final.Attempts[0].PublicationID != pub1 {
		t.Fatalf("attempt 1 history lost pub1: %+v", final.Attempts[0])
	}
}

// TestExecutionSnapshotBindCoupling: BindContentAndVersion commits atomically
// with Content/Version writes — no committed Content without a recoverable
// operation ID, and abort leaves neither.
func TestExecutionSnapshotBindCoupling(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	op := mustBegin(t, svc, "owner-b", "key-bind", []byte(`{"title":"bind"}`))
	contentID := primitive.NewObjectID()

	// Commit path: content insert + bind in one transaction.
	err := db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		if _, err := db.Collection("content").InsertOne(sc, bson.M{
			"_id": contentID, "full_path": "/news/bind-coupled", "title": "bind",
		}); err != nil {
			return err
		}
		return svc.BindContentAndVersion(ctx, sc, op.ID, contentID, 1, "/news/bind-coupled")
	})
	if err != nil {
		t.Fatalf("transactional bind: %v", err)
	}
	got, err := svc.Get(ctx, op.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ContentID == nil || *got.ContentID != contentID || got.ContentVersion != 1 || got.CanonicalPath != "/news/bind-coupled" {
		t.Fatalf("binding not persisted: %+v", got)
	}

	// Abort path: bind rolls back together with the content write.
	op2 := mustBegin(t, svc, "owner-b", "key-bind-abort", []byte(`{"title":"abort"}`))
	contentID2 := primitive.NewObjectID()
	forced := func(sc mongo.SessionContext) error {
		if _, err := db.Collection("content").InsertOne(sc, bson.M{
			"_id": contentID2, "full_path": "/news/bind-abort", "title": "abort",
		}); err != nil {
			return err
		}
		if err := svc.BindContentAndVersion(ctx, sc, op2.ID, contentID2, 1, "/news/bind-abort"); err != nil {
			return err
		}
		return context.DeadlineExceeded // force abort
	}
	if err := db.WithTransaction(ctx, forced); err == nil {
		t.Fatal("expected forced transaction error")
	}
	if n, _ := db.Collection("content").CountDocuments(ctx, bson.M{"_id": contentID2}); n != 0 {
		t.Fatalf("aborted content insert visible (n=%d)", n)
	}
	got2, err := svc.Get(ctx, op2.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got2.ContentID != nil {
		t.Fatalf("aborted bind persisted: %+v", got2)
	}

	// Re-binding the same values is idempotent; a different content conflicts.
	if err := svc.BindContentAndVersion(ctx, nil, op.ID, contentID, 1, "/news/bind-coupled"); err != nil {
		t.Fatalf("idempotent re-bind: %v", err)
	}
	if err := svc.BindContentAndVersion(ctx, nil, op.ID, primitive.NewObjectID(), 2, "/news/other"); CodeOf(err) != CodeConflict {
		t.Fatalf("divergent bind: got %v, want %s", err, CodeConflict)
	}
}
