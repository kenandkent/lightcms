package publication_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/services"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// shortBackoff returns a test-friendly retry schedule.
func shortBackoff(attempt int) time.Duration {
	if attempt <= 1 {
		return 50 * time.Millisecond
	}
	return 100 * time.Millisecond
}

// TestOutboxActivationInsertsOneAndDedupes: activation commit inserts exactly
// one content.publish row; re-execution with the same Publication ID inserts
// no second row (UNIQUE(event_type, aggregate_id)).
func TestOutboxActivationInsertsOneAndDedupes(t *testing.T) {
	db, repo := testRepo(t)
	ctx := context.Background()
	contentID := seedContent(t, db)

	a := stagedPub(contentID)
	if err := repo.InsertStaged(ctx, a); err != nil {
		t.Fatalf("InsertStaged: %v", err)
	}
	if err := repo.ActivateCAS(ctx, contentID, a.ID, nil); err != nil {
		t.Fatalf("ActivateCAS: %v", err)
	}
	if n := outboxCount(t, db, "content.publish", a.ID); n != 1 {
		t.Fatalf("expected exactly 1 content.publish row, got %d", n)
	}

	// Idempotent replay of a committed activation must not insert a second row.
	if err := repo.ActivateCAS(ctx, contentID, a.ID, nil); err != nil {
		t.Fatalf("replay ActivateCAS: %v", err)
	}
	if n := outboxCount(t, db, "content.publish", a.ID); n != 1 {
		t.Fatalf("replay inserted a second row, count = %d, want 1", n)
	}

	// Direct re-insert with the same identity inserts none (idempotent no-op).
	ob := publication.NewOutbox(db)
	payload := map[string]any{"content_id": contentID.Hex()}
	if err := ob.InsertUnique(ctx, publication.EventPublished, a.ID, payload); err != nil {
		t.Fatalf("re-execution InsertUnique: %v", err)
	}
	if n := outboxCount(t, db, "content.publish", a.ID); n != 1 {
		t.Fatalf("direct re-insert created a second row, count = %d, want 1", n)
	}
}

// TestOutboxUnpublishAndFailedEvents: unpublish and failed transitions insert
// their matching event rows in the same transaction as the lifecycle update,
// and a second unpublish inserts nothing.
func TestOutboxUnpublishAndFailedEvents(t *testing.T) {
	db, repo := testRepo(t)
	ctx := context.Background()
	contentID := seedContent(t, db)

	a := stagedPub(contentID)
	if err := repo.InsertStaged(ctx, a); err != nil {
		t.Fatalf("InsertStaged: %v", err)
	}
	if err := repo.ActivateCAS(ctx, contentID, a.ID, nil); err != nil {
		t.Fatalf("ActivateCAS: %v", err)
	}
	did, err := repo.UnpublishCAS(ctx, contentID, nil)
	if err != nil || !did {
		t.Fatalf("UnpublishCAS = (%v, %v), want (true, nil)", did, err)
	}
	if n := outboxCount(t, db, "content.unpublish", a.ID); n != 1 {
		t.Fatalf("expected 1 content.unpublish row, got %d", n)
	}
	if did2, err := repo.UnpublishCAS(ctx, contentID, nil); err != nil || did2 {
		t.Fatalf("second UnpublishCAS = (%v, %v), want (false, nil)", did2, err)
	}
	if n := outboxCount(t, db, "content.unpublish", a.ID); n != 1 {
		t.Fatalf("second unpublish inserted a new row, count = %d", n)
	}

	// Failed transition inserts publication.failed in the same transaction.
	contentID2 := seedContent(t, db)
	b := stagedPub(contentID2)
	if err := repo.InsertStaged(ctx, b); err != nil {
		t.Fatalf("InsertStaged B: %v", err)
	}
	if err := repo.MarkFailed(ctx, b.ID, "verify hash mismatch"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if n := outboxCount(t, db, "publication.failed", b.ID); n != 1 {
		t.Fatalf("expected 1 publication.failed row, got %d", n)
	}
}

// recordedDelivery captures one HTTP delivery seen by the test receiver.
type recordedDelivery struct {
	eventID   string
	event     string
	signature string
	body      []byte
}

func verifyHMAC(t *testing.T, secret string, body []byte, sig string) {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(sig)) {
		t.Errorf("HMAC mismatch: got %q want %q", sig, want)
	}
}

// TestOutboxWorkerDeliversPendingAfterRestart: a row committed before any
// worker ran (crash between commit and HTTP delivery) is delivered after
// restart with a stable event ID and a verifiable HMAC signature.
func TestOutboxWorkerDeliversPendingAfterRestart(t *testing.T) {
	db, repo := testRepo(t)
	ctx := context.Background()
	contentID := seedContent(t, db)

	// Commit the activation (and its outbox row) with no worker running.
	a := stagedPub(contentID)
	if err := repo.InsertStaged(ctx, a); err != nil {
		t.Fatalf("InsertStaged: %v", err)
	}
	if err := repo.ActivateCAS(ctx, contentID, a.ID, nil); err != nil {
		t.Fatalf("ActivateCAS: %v", err)
	}

	const secret = "test-secret-restart"
	var mu sync.Mutex
	var got []recordedDelivery
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_ = body
		var buf []byte
		tmp := make([]byte, 4096)
		for {
			n, err := r.Body.Read(tmp)
			if n > 0 {
				buf = append(buf, tmp[:n]...)
			}
			if err != nil {
				break
			}
		}
		mu.Lock()
		got = append(got, recordedDelivery{
			eventID:   r.Header.Get(publication.OutboxEventIDHeader),
			event:     r.Header.Get("X-LightCMS-Event"),
			signature: r.Header.Get("X-LightCMS-Signature"),
			body:      buf,
		})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Register a real webhook endpoint subscribed to content.publish.
	ws := services.NewWebhookService(db)
	wh, err := ws.Create(ctx, "restart-receiver", srv.URL, secret, []string{"content.publish"}, true)
	if err != nil {
		t.Fatalf("webhook Create: %v", err)
	}
	_ = wh

	// "Restart": construct the worker only now and drain the pending row.
	deliver := func(dctx context.Context, eventType, eventID string, payload map[string]any) error {
		return ws.DeliverRecordedEvent(dctx, eventType, eventID, payload)
	}
	worker := publication.NewOutboxWorker(db, deliver, publication.WorkerOptions{
		WorkerID:     "restart-worker-1",
		PollInterval: 10 * time.Millisecond,
		Lease:        time.Minute,
		Backoff:      shortBackoff,
	})
	didWork, err := worker.ProcessNext(ctx)
	if err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	if !didWork {
		t.Fatal("ProcessNext found no pending row, want 1 delivery")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("receiver hits = %d, want 1", len(got))
	}
	wantID := publication.EventID(publication.EventPublished, a.ID)
	if got[0].eventID != wantID {
		t.Errorf("event ID header = %q, want %q", got[0].eventID, wantID)
	}
	if got[0].event != publication.EventPublished {
		t.Errorf("event header = %q, want %q", got[0].event, publication.EventPublished)
	}
	if got[0].eventID == "" {
		t.Error("stable event ID header missing")
	}
	verifyHMAC(t, secret, got[0].body, got[0].signature)

	// Payload shape for Task 18: {event_id, event, timestamp, data}.
	var env struct {
		EventID   string         `json:"event_id"`
		Event     string         `json:"event"`
		Timestamp string         `json:"timestamp"`
		Data      map[string]any `json:"data"`
	}
	if err := json.Unmarshal(got[0].body, &env); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if env.EventID != wantID || env.Event != publication.EventPublished {
		t.Errorf("payload envelope = %+v, want event_id %q event %q", env, wantID, publication.EventPublished)
	}
	if env.Timestamp == "" || env.Data == nil {
		t.Errorf("payload missing timestamp/data: %+v", env)
	}

	// Row must now be delivered.
	var row bson.M
	if err := db.Collection(publication.CollectionOutbox).FindOne(ctx, bson.M{
		"event_type":   publication.EventPublished,
		"aggregate_id": a.ID,
	}).Decode(&row); err != nil {
		t.Fatalf("load outbox row: %v", err)
	}
	if row["state"] != "delivered" {
		t.Errorf("row state = %v, want delivered", row["state"])
	}
}

// TestOutboxWorkerRetryOn500WithStableIDAndHMAC: receiver 500 schedules a
// retry; the retry carries the same stable event ID and a valid HMAC.
func TestOutboxWorkerRetryOn500WithStableIDAndHMAC(t *testing.T) {
	db, _ := testRepo(t)
	ctx := context.Background()

	pubID := primitive.NewObjectID()
	ob := publication.NewOutbox(db)
	if err := ob.InsertUnique(ctx, publication.EventPublished, pubID, map[string]any{
		"content_id":     primitive.NewObjectID().Hex(),
		"publication_id": pubID.Hex(),
	}); err != nil {
		t.Fatalf("InsertUnique: %v", err)
	}

	const secret = "test-secret-retry"
	var hits int64
	var mu sync.Mutex
	var seen []recordedDelivery
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&hits, 1)
		var buf []byte
		tmp := make([]byte, 4096)
		for {
			m, err := r.Body.Read(tmp)
			if m > 0 {
				buf = append(buf, tmp[:m]...)
			}
			if err != nil {
				break
			}
		}
		mu.Lock()
		seen = append(seen, recordedDelivery{
			eventID:   r.Header.Get(publication.OutboxEventIDHeader),
			event:     r.Header.Get("X-LightCMS-Event"),
			signature: r.Header.Get("X-LightCMS-Signature"),
			body:      buf,
		})
		mu.Unlock()
		if n == 1 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ws := services.NewWebhookService(db)
	if _, err := ws.Create(ctx, "retry-receiver", srv.URL, secret, []string{"content.publish"}, true); err != nil {
		t.Fatalf("webhook Create: %v", err)
	}
	deliver := func(dctx context.Context, eventType, eventID string, payload map[string]any) error {
		return ws.DeliverRecordedEvent(dctx, eventType, eventID, payload)
	}
	worker := publication.NewOutboxWorker(db, deliver, publication.WorkerOptions{
		WorkerID:     "retry-worker-1",
		PollInterval: 10 * time.Millisecond,
		Lease:        time.Minute,
		Backoff:      shortBackoff,
	})

	// First attempt hits receiver 500 → retry scheduled, not delivered.
	if _, err := worker.ProcessNext(ctx); err == nil {
		t.Fatal("first ProcessNext = nil error, want delivery failure")
	}
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Fatalf("receiver hits after first attempt = %d, want 1", got)
	}
	var row bson.M
	if err := db.Collection(publication.CollectionOutbox).FindOne(ctx, bson.M{"aggregate_id": pubID}).Decode(&row); err != nil {
		t.Fatalf("load row after failure: %v", err)
	}
	if row["state"] != "pending" {
		t.Errorf("row state after 500 = %v, want pending (retry scheduled)", row["state"])
	}
	nextAt, _ := row["next_attempt_at"].(primitive.DateTime)
	if !nextAt.Time().After(time.Now().Add(-time.Minute)) {
		t.Errorf("next_attempt_at not scheduled in the future: %v", nextAt.Time())
	}

	// Wait for the test backoff to elapse, then the retry must succeed with
	// the SAME stable event ID.
	time.Sleep(150 * time.Millisecond)
	didWork, err := worker.ProcessNext(ctx)
	if err != nil {
		t.Fatalf("retry ProcessNext: %v", err)
	}
	if !didWork {
		t.Fatal("retry found no row, want redelivery")
	}
	if got := atomic.LoadInt64(&hits); got != 2 {
		t.Fatalf("receiver hits after retry = %d, want 2", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("recorded deliveries = %d, want 2", len(seen))
	}
	if seen[0].eventID == "" || seen[0].eventID != seen[1].eventID {
		t.Errorf("event ID not stable across retry: %q vs %q", seen[0].eventID, seen[1].eventID)
	}
	wantID := publication.EventID(publication.EventPublished, pubID)
	if seen[0].eventID != wantID || seen[1].eventID != wantID {
		t.Errorf("event IDs = %q/%q, want stable %q", seen[0].eventID, seen[1].eventID, wantID)
	}
	verifyHMAC(t, secret, seen[0].body, seen[0].signature)
	verifyHMAC(t, secret, seen[1].body, seen[1].signature)
}

// TestOutboxClaimLeaseSingleDelivery: two replicas racing for one pending row
// result in exactly one HTTP delivery (Mongo claim lease).
func TestOutboxClaimLeaseSingleDelivery(t *testing.T) {
	db, _ := testRepo(t)
	ctx := context.Background()

	pubID := primitive.NewObjectID()
	ob := publication.NewOutbox(db)
	if err := ob.InsertUnique(ctx, publication.EventPublished, pubID, map[string]any{"k": "v"}); err != nil {
		t.Fatalf("InsertUnique: %v", err)
	}

	var deliveries int64
	deliver := func(dctx context.Context, eventType, eventID string, payload map[string]any) error {
		atomic.AddInt64(&deliveries, 1)
		time.Sleep(50 * time.Millisecond) // widen the race window
		return nil
	}
	workerA := publication.NewOutboxWorker(db, deliver, publication.WorkerOptions{
		WorkerID: "replica-a", PollInterval: 5 * time.Millisecond, Lease: time.Minute, Backoff: shortBackoff,
	})
	workerB := publication.NewOutboxWorker(db, deliver, publication.WorkerOptions{
		WorkerID: "replica-b", PollInterval: 5 * time.Millisecond, Lease: time.Minute, Backoff: shortBackoff,
	})

	var wg sync.WaitGroup
	var workedA, workedB bool
	var errA, errB error
	wg.Add(2)
	go func() {
		defer wg.Done()
		workedA, errA = workerA.ProcessNext(ctx)
	}()
	go func() {
		defer wg.Done()
		workedB, errB = workerB.ProcessNext(ctx)
	}()
	wg.Wait()
	if errA != nil || errB != nil {
		t.Fatalf("ProcessNext errors: A=%v B=%v", errA, errB)
	}
	if workedA == workedB {
		t.Fatalf("exactly one replica must win the claim, got A=%v B=%v", workedA, workedB)
	}
	if got := atomic.LoadInt64(&deliveries); got != 1 {
		t.Fatalf("deliveries = %d, want exactly 1", got)
	}
}

// TestOutboxLeaseExpiryTakeover: a delivering row with an expired lease becomes
// claimable again (crashed worker takeover); a row with a live lease does not.
func TestOutboxLeaseExpiryTakeover(t *testing.T) {
	db, _ := testRepo(t)
	ctx := context.Background()

	pubID := primitive.NewObjectID()
	ob := publication.NewOutbox(db)
	if err := ob.InsertUnique(ctx, publication.EventPublished, pubID, map[string]any{"k": "v"}); err != nil {
		t.Fatalf("InsertUnique: %v", err)
	}

	deliverOK := func(dctx context.Context, eventType, eventID string, payload map[string]any) error {
		return nil
	}
	holder := publication.NewOutboxWorker(db, deliverOK, publication.WorkerOptions{
		WorkerID: "holder", Lease: time.Minute, Backoff: shortBackoff,
	})
	challenger := publication.NewOutboxWorker(db, deliverOK, publication.WorkerOptions{
		WorkerID: "challenger", Lease: time.Minute, Backoff: shortBackoff,
	})

	// Simulate a holder that claimed but has not finished: force the row into
	// delivering with a live lease.
	liveLease := time.Now().Add(time.Minute)
	if _, err := db.Collection(publication.CollectionOutbox).UpdateOne(ctx,
		bson.M{"aggregate_id": pubID},
		bson.M{"$set": bson.M{
			"state": "delivering", "locked_by": "holder",
			"locked_at": time.Now(), "lease_expires_at": liveLease,
		}},
	); err != nil {
		t.Fatalf("force delivering: %v", err)
	}
	_ = holder
	if didWork, err := challenger.ProcessNext(ctx); err != nil || didWork {
		t.Fatalf("challenger claimed a live lease: didWork=%v err=%v, want false/nil", didWork, err)
	}

	// Expire the lease (crashed holder) → challenger takes over and delivers.
	if _, err := db.Collection(publication.CollectionOutbox).UpdateOne(ctx,
		bson.M{"aggregate_id": pubID},
		bson.M{"$set": bson.M{"lease_expires_at": time.Now().Add(-time.Second)}},
	); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if didWork, err := challenger.ProcessNext(ctx); err != nil || !didWork {
		t.Fatalf("takeover ProcessNext = (%v, %v), want (true, nil)", didWork, err)
	}
}
