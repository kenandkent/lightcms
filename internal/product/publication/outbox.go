// Durable webhook outbox (Task 9, integration owner).
//
// Scope boundary (plan Task 9; spec §28.1):
//   - Outbox.InsertUnique: session-aware insert-only event creation with
//     identity (event_type, publication_id), backed by Task 2's
//     UNIQUE(event_type, aggregate_id) index. It is the delivery worker's
//     inserter: publication.Repository consumes it via the OutboxInserter
//     interface (mongo.SessionContext passed as context.Context, never nested
//     transactions); this worker consumes what it inserts.
//   - OutboxWorker: same-process polling worker with a Mongo claim lease so
//     only one app replica delivers a row at a time, at-least-once HTTP
//     delivery via the existing signed Webhook Engine (services.WebhookService
//     .DeliverRecordedEvent), and retry scheduling with a stable event ID.
//
// The plan's "InsertUnique(ctx, session, ...)" is realized as
// InsertUnique(ctx, ...) where ctx IS the session when called inside a
// transaction (mongo.SessionContext as context.Context), per the Shared
// interface contract ("Repository methods called inside a transaction accept
// mongo.SessionContext as context.Context; they must not open nested
// transactions"). There is no separate session parameter: passing the session
// as ctx is the session-aware form, and it is what lets Outbox implement
// OutboxInserter without an adapter.
//
// Delivery semantics: events are created once (unique index is the arbiter;
// a duplicate insert is an idempotent no-op success), HTTP delivery is
// at-least-once. Receivers dedupe with the stable event ID carried in both
// the X-LightCMS-Event-ID header and the JSON payload's event_id field (see
// EventID and RecordedPayloadShape for the Task 18 contract).
package publication

import (
	"context"
	"fmt"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/database"
	"github.com/jonradoff/lightcms/v7/internal/observe"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Outbox delivery states (spec §28.1).
const (
	// OutboxStatePending is a committed event awaiting (re)delivery.
	OutboxStatePending = "pending"
	// OutboxStateDelivering is a row claimed by one worker under lease.
	OutboxStateDelivering = "delivering"
	// OutboxStateDelivered is a row whose HTTP delivery succeeded.
	OutboxStateDelivered = "delivered"
	// OutboxStateFailed is a terminally poisoned row (reserved; the worker
	// never marks transient HTTP failures as failed — it requeues them as
	// pending with a future next_attempt_at).
	OutboxStateFailed = "failed"
)

const (
	// OutboxEventIDHeader is the stable at-least-once dedupe header sent on
	// every outbox HTTP delivery. Receivers MUST dedupe on this value.
	// Contract for Task 18: exact header name is "X-LightCMS-Event-ID".
	// It must stay in sync with the literal in
	// internal/services/webhook.go DeliverRecordedEvent.
	OutboxEventIDHeader = "X-LightCMS-Event-ID"
	// OutboxAggregateType is the aggregate_type stored on every publication
	// outbox row (spec §28.1).
	OutboxAggregateType = "publication"
)

// EventID returns the stable at-least-once identity for an outbox event:
// "<event_type>:<publication_id hex>". It is identical across retries of the
// same row (and would be identical even if the row were recreated), so
// receivers dedupe on it. Contract for Task 18.
func EventID(eventType string, publicationID primitive.ObjectID) string {
	return eventType + ":" + publicationID.Hex()
}

// RecordedPayloadShape documents the exact JSON body sent by
// WebhookService.DeliverRecordedEvent for Task 18. The HMAC in
// X-LightCMS-Signature signs these exact bytes:
//
//	{
//	  "event_id": "<event_type>:<publication_id hex>" (== X-LightCMS-Event-ID),
//	  "event":    "<event_type, e.g. content.publish>",
//	  "timestamp": "<RFC3339 delivery time; changes per attempt>",
///	  "data":     {<stored outbox payload: content_id, publication_id,
//	                 content_version, template_version, full_path,
//	                 content_hash, logical_published_at, ...>}
//	}
//
// The event ID is stable across retries; timestamp (and therefore the HMAC)
// changes per attempt but always verifies with the endpoint secret.
const RecordedPayloadShape = `{"event_id":string,"event":string,"timestamp":RFC3339,"data":object}`

// Outbox persists idempotent webhook events. It implements OutboxInserter so
// publication.Repository can inject it; the worker consumes what it inserts.
type Outbox struct {
	db *database.DB
}

// NewOutbox builds the delivery worker's inserter. Pass it to NewRepository
// at wiring time to replace the default insert-only mongoOutbox.
func NewOutbox(db *database.DB) *Outbox { return &Outbox{db: db} }

// InsertUnique inserts one outbox row with identity
// (event_type, aggregate_id=publicationID). ctx carries the caller's Mongo
// session when invoked inside a lifecycle transaction (mongo.SessionContext
// as context.Context); it must not open a nested transaction.
//
// Idempotency: the UNIQUE(event_type, aggregate_id) index is the arbiter. A
// duplicate insert returns nil with no second row (once-created event), so a
// lost-response retry after commit is safe. Implemented as an atomic upsert
// ($setOnInsert): unlike insert-and-swallow-duplicate-key, it never raises
// duplicate-key inside the caller's multi-document transaction — the failed
// insert would abort the server-side transaction and kill the later commit
// with NoSuchTransaction (see mongoOutbox.InsertUnique). Concurrent upserts
// serialize on the unique index; exactly one wins the insert.
func (o *Outbox) InsertUnique(ctx context.Context, eventType string, publicationID primitive.ObjectID, payload map[string]any) error {
	if eventType == "" {
		return pubErr(CodeValidation, "event_type is required", nil)
	}
	if publicationID.IsZero() {
		return pubErr(CodeValidation, "publication_id is required", nil)
	}
	if payload == nil {
		payload = map[string]any{}
	}
	now := time.Now()
	doc := bson.M{
		"event_type":      eventType,
		"aggregate_type":  OutboxAggregateType,
		"aggregate_id":    publicationID,
		"payload":         payload,
		"state":           OutboxStatePending,
		"attempt":         0,
		"next_attempt_at": now,
		"created_at":      now,
	}
	_, err := o.db.Collection(CollectionOutbox).UpdateOne(ctx,
		bson.M{"event_type": eventType, "aggregate_id": publicationID},
		bson.M{"$setOnInsert": doc},
		options.Update().SetUpsert(true),
	)
	return err
}

// OutboxRecord is one webhook_outbox row.
type OutboxRecord struct {
	ID             primitive.ObjectID `bson:"_id,omitempty"`
	EventType      string             `bson:"event_type"`
	AggregateType  string             `bson:"aggregate_type"`
	AggregateID    primitive.ObjectID `bson:"aggregate_id"`
	Payload        map[string]any     `bson:"payload"`
	State          string             `bson:"state"`
	Attempt        int                `bson:"attempt"`
	NextAttemptAt  time.Time          `bson:"next_attempt_at"`
	CreatedAt      time.Time          `bson:"created_at"`
	DeliveredAt    *time.Time         `bson:"delivered_at,omitempty"`
	LockedBy       string             `bson:"locked_by,omitempty"`
	LockedAt       *time.Time         `bson:"locked_at,omitempty"`
	LeaseExpiresAt *time.Time         `bson:"lease_expires_at,omitempty"`
}

// DeliverFunc delivers one claimed event. Implementations MUST send the
// stable eventID in the X-LightCMS-Event-ID header and HMAC-sign the body
// with the endpoint secret (services.WebhookService.DeliverRecordedEvent
// does this); the worker retries (at-least-once) on any returned error.
type DeliverFunc func(ctx context.Context, eventType, eventID string, payload map[string]any) error

// WorkerOptions configures an OutboxWorker. Zero values select production
// defaults; tests override PollInterval/Lease/Backoff for determinism.
type WorkerOptions struct {
	// WorkerID identifies this replica in locked_by. Defaults to a random ID.
	WorkerID string
	// PollInterval is the idle sleep when no row is due. Default 5s.
	PollInterval time.Duration
	// Lease is the claim-lease duration. A row stuck in delivering past its
	// lease becomes claimable by another replica (crash takeover).
	// Default 5 minutes.
	Lease time.Duration
	// Backoff maps the post-claim attempt count (>=1) to the next retry
	// delay. Default mirrors the existing webhook retry schedule for the
	// first two attempts (30s, 5m), then 30m, capped at 1h.
	Backoff func(attempt int) time.Duration
}

// OutboxWorker polls webhook_outbox in-process and delivers pending events
// through DeliverFunc (wired to WebhookService.DeliverRecordedEvent).
type OutboxWorker struct {
	db           *database.DB
	deliver      DeliverFunc
	workerID     string
	pollInterval time.Duration
	lease        time.Duration
	backoff      func(attempt int) time.Duration
}

// defaultBackoff mirrors the existing WebhookService retry delays (30s, 5m)
// for the first two attempts so outbox retries reuse the established retry
// policy without sleeping the worker.
func defaultBackoff(attempt int) time.Duration {
	switch {
	case attempt <= 1:
		return 30 * time.Second
	case attempt == 2:
		return 5 * time.Minute
	case attempt == 3:
		return 30 * time.Minute
	default:
		return time.Hour
	}
}

// NewOutboxWorker builds a worker. deliver must be non-nil at ProcessNext/Run
// time (wiring injects WebhookService.DeliverRecordedEvent).
func NewOutboxWorker(db *database.DB, deliver DeliverFunc, opts WorkerOptions) *OutboxWorker {
	id := opts.WorkerID
	if id == "" {
		id = "outbox-worker-" + primitive.NewObjectID().Hex()
	}
	interval := opts.PollInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	lease := opts.Lease
	if lease <= 0 {
		lease = 5 * time.Minute
	}
	backoff := opts.Backoff
	if backoff == nil {
		backoff = defaultBackoff
	}
	return &OutboxWorker{
		db: db, deliver: deliver,
		workerID: id, pollInterval: interval, lease: lease, backoff: backoff,
	}
}

// Run polls until ctx is cancelled: claim one due row at a time, deliver it,
// mark delivered or schedule a retry. Delivery errors are requeued, never
// fatal. At most one replica holds a row's lease at a time (see claim).
func (w *OutboxWorker) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		didWork, _ := w.ProcessNext(ctx) //nolint:errcheck // retry already scheduled
		if didWork {
			continue // drain backlog without sleeping
		}
		// Task 16F: publish the backlog gauge even when idle (one indexed
		// count per poll interval).
		if n, err := w.Backlog(ctx); err == nil {
			observe.Default().SetOutboxBacklog(n)
		}
		t := time.NewTimer(w.pollInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// Backlog counts pending (undelivered, due or scheduled) outbox rows: the
// spec §37.2 outbox backlog gauge. Delivery workers and P0 monitors read
// it; delivery itself never blocks on it.
func (w *OutboxWorker) Backlog(ctx context.Context) (int64, error) {
	return w.db.Collection(CollectionOutbox).CountDocuments(ctx, bson.M{
		"state": bson.M{"$in": []string{OutboxStatePending, OutboxStateDelivering}},
	})
}

// ProcessNext claims a single due row, delivers it, and marks it delivered
// or requeues it with a future next_attempt_at. It reports (false, nil) when
// no row is due. On delivery failure it returns (true, deliverErr) AFTER
// scheduling the retry, so callers observe the failure while the row stays
// pending. Exposed for deterministic tests; Run loops over it.
func (w *OutboxWorker) ProcessNext(ctx context.Context) (bool, error) {
	if w.deliver == nil {
		return false, fmt.Errorf("outbox worker: deliver func is nil")
	}
	rec, err := w.claim(ctx)
	if err != nil {
		return false, err
	}
	if rec == nil {
		return false, nil
	}
	eventID := EventID(rec.EventType, rec.AggregateID)
	payload := rec.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	if err := w.deliver(ctx, rec.EventType, eventID, payload); err != nil {
		// Task 16F: delivery failure counter (retry already scheduled).
		observe.Default().IncOutboxFailed()
		if rerr := w.scheduleRetry(ctx, rec); rerr != nil {
			return true, err
		}
		return true, err
	}
	if err := w.markDelivered(ctx, rec); err != nil {
		return true, err
	}
	// Task 16F: successful delivery counter.
	observe.Default().IncOutboxDelivered()
	return true, nil
}

// claim atomically moves one due row to delivering under this worker's lease.
// Only one replica wins per row: the findOneAndUpdate filter/sort is atomic
// in MongoDB. Due means pending with next_attempt_at <= now, or delivering
// with an expired (or missing) lease — the crashed-holder takeover path.
func (w *OutboxWorker) claim(ctx context.Context) (*OutboxRecord, error) {
	now := time.Now()
	filter := bson.M{
		"$or": []bson.M{
			{"state": OutboxStatePending, "next_attempt_at": bson.M{"$lte": now}},
			{"state": OutboxStateDelivering, "lease_expires_at": bson.M{"$lte": now}},
			{"state": OutboxStateDelivering, "lease_expires_at": bson.M{"$exists": false}},
		},
	}
	update := bson.M{
		"$set": bson.M{
			"state":            OutboxStateDelivering,
			"locked_by":        w.workerID,
			"locked_at":        now,
			"lease_expires_at": now.Add(w.lease),
		},
		"$inc": bson.M{"attempt": 1},
	}
	opts := options.FindOneAndUpdate().
		SetReturnDocument(options.After).
		SetSort(bson.D{{Key: "next_attempt_at", Value: 1}, {Key: "created_at", Value: 1}})
	var rec OutboxRecord
	if err := w.db.Collection(CollectionOutbox).FindOneAndUpdate(ctx, filter, update, opts).Decode(&rec); err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, nil
		}
		return nil, err
	}
	return &rec, nil
}

// markDelivered flips a worker-owned delivering row to delivered. The
// locked_by CAS prevents a stale holder (lease lost to a takeover) from
// clobbering the new owner's row.
func (w *OutboxWorker) markDelivered(ctx context.Context, rec *OutboxRecord) error {
	now := time.Now()
	res, err := w.db.Collection(CollectionOutbox).UpdateOne(ctx,
		bson.M{"_id": rec.ID, "locked_by": w.workerID, "state": OutboxStateDelivering},
		bson.M{
			"$set":   bson.M{"state": OutboxStateDelivered, "delivered_at": now},
			"$unset": bson.M{"lease_expires_at": "", "locked_at": ""},
		},
	)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return fmt.Errorf("outbox worker: lease lost for event %s", EventID(rec.EventType, rec.AggregateID))
	}
	return nil
}

// scheduleRetry requeues a worker-owned row as pending with a future
// next_attempt_at derived from the post-claim attempt count. Transient HTTP
// failures (e.g. receiver 500) always land here — never in terminal failed.
func (w *OutboxWorker) scheduleRetry(ctx context.Context, rec *OutboxRecord) error {
	delay := w.backoff(rec.Attempt)
	if delay < 0 {
		delay = 0
	}
	next := time.Now().Add(delay)
	res, err := w.db.Collection(CollectionOutbox).UpdateOne(ctx,
		bson.M{"_id": rec.ID, "locked_by": w.workerID},
		bson.M{
			"$set":   bson.M{"state": OutboxStatePending, "next_attempt_at": next},
			"$unset": bson.M{"lease_expires_at": "", "locked_at": ""},
		},
	)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return fmt.Errorf("outbox worker: lease lost for event %s", EventID(rec.EventType, rec.AggregateID))
	}
	return nil
}
