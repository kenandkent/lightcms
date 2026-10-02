package publication

import (
	"context"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/database"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Webhook event types emitted transactionally with lifecycle changes
// (spec §28: content.publish only after activation; the failed and unpublish
// events likewise commit in the same transaction as the status flip).
const (
	// EventPublished is inserted in the activation transaction.
	EventPublished = "content.publish"
	// EventUnpublished is inserted in the unpublish transaction.
	EventUnpublished = "content.unpublish"
	// EventFailed is inserted in the mark-failed transaction.
	EventFailed = "publication.failed"
)

// OutboxInserter persists one idempotent webhook event inside the caller's
// Mongo transaction. ctx carries the transaction session (a
// mongo.SessionContext passed as context.Context); implementations must use
// it directly and must NOT open nested transactions.
//
// Identity is (eventType, publicationID), backed by Task 2's
// UNIQUE(event_type, aggregate_id) index: a retry after commit inserts no
// second row. Task 9 owns claiming, lease, HTTP delivery, and retry — this
// interface is injected so Task 9 can wire its worker without touching the
// lifecycle transactions defined here.
type OutboxInserter interface {
	InsertUnique(ctx context.Context, eventType string, publicationID primitive.ObjectID, payload map[string]any) error
}

// mongoOutbox is the minimal insert-only OutboxInserter used until Task 9
// wires its delivery worker. Insert-only is sufficient for Task 5: the
// exactly-once creation guarantee comes from the unique index + shared
// transaction, not from delivery.
//
// Idempotency (spec §28.1: "Idempotency retry ...不会创建第二个 publish
// event"): the UNIQUE(event_type, aggregate_id) index is the arbiter and a
// duplicate insert is a silent success (nil, no second row) — identical to
// Outbox.InsertUnique. Mechanism matters: a plain InsertOne that swallows the
// duplicate-key error is UNSAFE inside a multi-document transaction — the
// failed insert aborts the server-side transaction, so the later commit dies
// with NoSuchTransaction (observed) and the saga falls into failCommitted
// compensation, which can delete a just-cut live canonical whose bytes match
// the replayed attempt (deterministic render ⇒ identical SHA). The atomic
// upsert below never raises duplicate-key at all: concurrent upserts
// serialize on the unique index, exactly one wins the insert, the rest are
// no-op matches — the transaction always stays alive to commit.
type mongoOutbox struct {
	db *database.DB
}

func (m *mongoOutbox) InsertUnique(ctx context.Context, eventType string, publicationID primitive.ObjectID, payload map[string]any) error {
	now := time.Now()
	doc := bson.M{
		"event_type":      eventType,
		"aggregate_type":  "publication",
		"aggregate_id":    publicationID,
		"payload":         payload,
		"state":           OutboxStatePending,
		"attempt":         0,
		"next_attempt_at": now,
		"created_at":      now,
	}
	_, err := m.db.Collection(CollectionOutbox).UpdateOne(ctx,
		bson.M{"event_type": eventType, "aggregate_id": publicationID},
		bson.M{"$setOnInsert": doc},
		options.Update().SetUpsert(true),
	)
	return err
}

// Repository owns publication records and their transactional lifecycle
// moves. Single-document reads accept any context; the CAS moves run their
// lifecycle + Content projection + outbox writes in ONE Mongo transaction via
// DB.WithTransaction (replica set required) and accept a
// mongo.SessionContext as context.Context without opening nested
// transactions. File effects are explicitly out of scope (Task 8 saga).
type Repository struct {
	db     *database.DB
	outbox OutboxInserter
}

// NewRepository builds a publication repository. A nil outbox selects the
// default Mongo insert-only implementation; pass a fake in tests to prove
// single-transaction atomicity, and Task 9 replaces it with the delivery
// worker's inserter at wiring time.
func NewRepository(db *database.DB, outbox OutboxInserter) *Repository {
	if outbox == nil {
		outbox = &mongoOutbox{db: db}
	}
	return &Repository{db: db, outbox: outbox}
}

// eventPayload builds the webhook payload shared by all three lifecycle
// events. PublicURL is unknown at the records layer (Task 8/13 resolve it
// after activation); the payload carries the frozen render identity.
func eventPayload(p *Publication) map[string]any {
	payload := map[string]any{
		"content_id":           p.ContentID.Hex(),
		"publication_id":       p.ID.Hex(),
		"content_version":      p.ContentVersion,
		"template_version":     p.TemplateVersion,
		"full_path":            p.FullPath,
		"content_hash":         p.ContentHash,
		"logical_published_at": p.LogicalPublishedAt,
	}
	// Task 16E: resolved public URL travels with the activation event so
	// delivery never joins it back (enrichOutboxURL stays as backfill for
	// rows written before this field existed).
	if p.PublicURL != "" {
		payload["public_url"] = p.PublicURL
	}
	return payload
}

// InsertStaged persists a staged candidate. Required: ContentID, FullPath,
// ContentHash, LogicalPublishedAt (frozen before Render per the §16.1
// pipeline, so a staged record without them is a programming error). Status
// must be staged (empty defaults to staged); storage/verification default to
// pending and provider to filesystem. The generated ID and timestamps are
// written back onto pub.
func (r *Repository) InsertStaged(ctx context.Context, pub *Publication) error {
	if pub == nil {
		return pubErr(CodeValidation, "publication is required", nil)
	}
	if pub.ContentID.IsZero() {
		return pubErr(CodeValidation, "content_id is required", nil)
	}
	if pub.FullPath == "" {
		return pubErr(CodeValidation, "full_path is required", nil)
	}
	if pub.ContentHash == "" {
		return pubErr(CodeValidation, "content_hash is required", nil)
	}
	if pub.LogicalPublishedAt.IsZero() {
		return pubErr(CodeValidation, "logical_published_at is required", nil)
	}
	if pub.Status == "" {
		pub.Status = StatusStaged
	}
	if pub.Status != StatusStaged {
		return pubErr(CodeValidation, "only staged publications may be inserted, got "+string(pub.Status), nil)
	}
	if pub.StorageState == "" {
		pub.StorageState = StoragePending
	}
	if pub.VerificationStatus == "" {
		pub.VerificationStatus = VerificationPending
	}
	if pub.StorageProvider == "" {
		pub.StorageProvider = "filesystem"
	}
	if pub.ID.IsZero() {
		pub.ID = primitive.NewObjectID()
	}
	if pub.CreatedAt.IsZero() {
		pub.CreatedAt = time.Now()
	}
	if _, err := r.db.Collection(CollectionPublications).InsertOne(ctx, pub); err != nil {
		if isDupKey(err) {
			// R11 resume: the same attempt re-staging its frozen
			// publication after an uncertain commit must not fail —
			// tolerate a byte-identical re-insert, conflict on
			// divergence (a reused ID with different bytes is a bug).
			var existing Publication
			if ferr := r.db.Collection(CollectionPublications).FindOne(ctx,
				bson.M{"_id": pub.ID}).Decode(&existing); ferr == nil &&
				existing.Status == StatusStaged &&
				existing.ContentID == pub.ContentID &&
				existing.ContentVersion == pub.ContentVersion &&
				existing.ContentHash == pub.ContentHash {
				return nil
			}
			return pubErr(CodeConflict, "duplicate publication record", err)
		}
		return err
	}
	return nil
}

// GetActive returns the single active publication for content, or (nil, nil)
// when the page has no active publication (never published or unpublished).
// DTOs derive active_publication_id from this — Content stores no pointer.
func (r *Repository) GetActive(ctx context.Context, contentID primitive.ObjectID) (*Publication, error) {
	var p Publication
	err := r.db.Collection(CollectionPublications).FindOne(ctx, bson.M{
		"content_id": contentID,
		"status":     string(StatusActive),
	}).Decode(&p)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// GetByID loads one publication by ID, or a CodeNotFound error.
func (r *Repository) GetByID(ctx context.Context, id primitive.ObjectID) (*Publication, error) {
	var p Publication
	err := r.db.Collection(CollectionPublications).FindOne(ctx, bson.M{"_id": id}).Decode(&p)
	if err == mongo.ErrNoDocuments {
		return nil, pubErr(CodeNotFound, "publication "+id.Hex()+" not found", err)
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ListHistory returns every publication for content, newest first
// (created_at descending). Used by Admin history and rollback selection.
func (r *Repository) ListHistory(ctx context.Context, contentID primitive.ObjectID) ([]Publication, error) {
	cur, err := r.db.Collection(CollectionPublications).Find(ctx,
		bson.M{"content_id": contentID},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}),
	)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []Publication
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []Publication{}
	}
	return out, nil
}

// ActivateCAS flips staged -> active with compare-and-swap on the expected
// old active ID (nil = expect no active, i.e. first publish). In ONE
// transaction it: supersedes the old active, activates the new record,
// syncs Content.Published=true / PublishedAt=logical time /
// HasUnpublishedChanges=false, and inserts the content.publish outbox event.
//
// Conflict semantics: a stale expected ID (or an unexpected active when nil
// was passed) yields CodeConflict and changes nothing. A failed, superseded,
// or unpublished record can never become active (CodeInvalidTransition).
// Unverified output (pending/failed verification) cannot activate; legacy_unverified
// is permitted HERE for the Task 14 migration path — the ordinary business
// publish path (Task 8) must additionally reject it.
//
// Lost-response retry after a committed activation is idempotent: if the new
// record is already the current active, ActivateCAS returns nil.
func (r *Repository) ActivateCAS(ctx context.Context, contentID, newPublicationID primitive.ObjectID, expectedOldActiveID *primitive.ObjectID) error {
	return r.activateCAS(ctx, contentID, newPublicationID, expectedOldActiveID, "", "")
}

// ActivateCASWithRedirect commits the activation transaction with the
// rename redirect in the SAME transaction (R08, spec §18.3): oldPath →
// newPath is upserted atomically with the active-pointer flip, so a crash
// can never leave the new page live with the old link redirect-less.
// Empty paths skip the redirect write (non-rename activations).
func (r *Repository) ActivateCASWithRedirect(ctx context.Context, contentID, newPublicationID primitive.ObjectID, expectedOldActiveID *primitive.ObjectID, oldPath, newPath string) error {
	return r.activateCAS(ctx, contentID, newPublicationID, expectedOldActiveID, oldPath, newPath)
}

func (r *Repository) activateCAS(ctx context.Context, contentID, newPublicationID primitive.ObjectID, expectedOldActiveID *primitive.ObjectID, redirectFrom, redirectTo string) error {
	return r.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		pubs := r.db.Collection(CollectionPublications)
		content := r.db.Collection(CollectionContent)

		var next Publication
		if err := pubs.FindOne(sc, bson.M{"_id": newPublicationID}).Decode(&next); err != nil {
			if err == mongo.ErrNoDocuments {
				return pubErr(CodeNotFound, "publication "+newPublicationID.Hex()+" not found", err)
			}
			return err
		}
		if next.ContentID != contentID {
			return pubErr(CodeValidation, "publication "+newPublicationID.Hex()+" does not belong to this content", nil)
		}
		if next.Status == StatusActive {
			current, err := r.getActiveIn(sc, contentID)
			if err != nil {
				return err
			}
			if current != nil && current.ID == newPublicationID {
				return nil // idempotent replay of a committed activation
			}
			return pubErr(CodeInvalidTransition,
				"publication "+newPublicationID.Hex()+" is already active elsewhere and cannot be re-activated", nil)
		}
		if next.Status != StatusStaged {
			return pubErr(CodeInvalidTransition,
				"lifecycle transition "+string(next.Status)+" -> active is not allowed", nil)
		}
		if !ActivatableVerification(next.VerificationStatus, true) {
			return pubErr(CodeInvalidTransition,
				"cannot activate publication with verification "+string(next.VerificationStatus), nil)
		}

		current, err := r.getActiveIn(sc, contentID)
		if err != nil {
			return err
		}
		switch {
		case expectedOldActiveID == nil:
			if current != nil {
				return pubErr(CodeConflict, "expected no active publication but found "+current.ID.Hex(), nil)
			}
		default:
			if current == nil || current.ID != *expectedOldActiveID {
				return pubErr(CodeConflict, "stale expected active publication", nil)
			}
		}

		now := time.Now()
		if current != nil {
			res, err := pubs.UpdateOne(sc,
				bson.M{"_id": current.ID, "status": string(StatusActive)},
				bson.M{"$set": bson.M{"status": string(StatusSuperseded), "superseded_at": now}},
			)
			if err != nil {
				if isDupKey(err) {
					return pubErr(CodeConflict, "concurrent activation changed the active pointer", err)
				}
				return err
			}
			if res.MatchedCount == 0 {
				return pubErr(CodeConflict, "concurrent activation changed the active pointer", nil)
			}
		}
		res, err := pubs.UpdateOne(sc,
			bson.M{"_id": newPublicationID, "status": string(StatusStaged)},
			bson.M{"$set": bson.M{"status": string(StatusActive), "activated_at": now}},
		)
		if err != nil {
			if isDupKey(err) {
				return pubErr(CodeConflict, "duplicate active publication for content", err)
			}
			return err
		}
		if res.MatchedCount == 0 {
			return pubErr(CodeConflict, "concurrent activation changed publication "+newPublicationID.Hex(), nil)
		}

		// Compatibility projection: PublishedAt is the frozen LOGICAL time,
		// not the physical commit time (spec §15.3).
		cres, err := content.UpdateOne(sc,
			bson.M{"_id": contentID},
			bson.M{"$set": bson.M{
				"published":               true,
				"published_at":            next.LogicalPublishedAt,
				"has_unpublished_changes": false,
				"updated_at":              now,
			}},
		)
		if err != nil {
			return err
		}
		if cres.MatchedCount == 0 {
			return pubErr(CodeContentNotFound, "content "+contentID.Hex()+" not found", nil)
		}

		next.ID = newPublicationID
		if err := r.outbox.InsertUnique(sc, EventPublished, newPublicationID, eventPayload(&next)); err != nil {
			return err
		}
		if redirectFrom != "" && redirectTo != "" && redirectFrom != redirectTo {
			// R08: the rename redirect commits atomically with the
			// activation — same doc shape as the saga backfill (which
			// stays as the idempotent retry path).
			now := time.Now()
			if _, err := r.db.Collection("redirects").UpdateOne(sc,
				bson.M{"from_path": redirectFrom},
				bson.M{"$set": bson.M{
					"from_path": redirectFrom, "to_path": redirectTo, "status_code": 301,
					"description": "rename-and-publish redirect", "updated_at": now,
				}, "$setOnInsert": bson.M{"created_at": now}},
				options.Update().SetUpsert(true),
			); err != nil {
				return err
			}
		}
		return nil
	})
}

// UnpublishCAS flips active -> unpublished in ONE transaction: lifecycle +
// Content.Published=false / PublishedAt=nil + the content.unpublish outbox
// event (spec §18.1). The immutable object is retained for exact rollback;
// physical deletion is retention GC (Task 10).
//
// Idempotency: when no active publication exists the call succeeds with
// didUnpublish=false and inserts NO new event, so a second Unpublish is a
// 200 with zero side effects. A non-nil expected ID that does not match the
// current active yields CodeConflict with nothing changed.
func (r *Repository) UnpublishCAS(ctx context.Context, contentID primitive.ObjectID, expectedActiveID *primitive.ObjectID) (didUnpublish bool, err error) {
	did := false
	err = r.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		pubs := r.db.Collection(CollectionPublications)
		content := r.db.Collection(CollectionContent)

		current, txErr := r.getActiveIn(sc, contentID)
		if txErr != nil {
			return txErr
		}
		if current == nil {
			return nil // idempotent: already unpublished
		}
		if expectedActiveID != nil && current.ID != *expectedActiveID {
			return pubErr(CodeConflict, "stale expected active publication", nil)
		}

		now := time.Now()
		res, txErr := pubs.UpdateOne(sc,
			bson.M{"_id": current.ID, "status": string(StatusActive)},
			bson.M{"$set": bson.M{"status": string(StatusUnpublished), "unpublished_at": now}},
		)
		if txErr != nil {
			return txErr
		}
		if res.MatchedCount == 0 {
			return pubErr(CodeConflict, "concurrent publication change during unpublish", nil)
		}

		cres, txErr := content.UpdateOne(sc,
			bson.M{"_id": contentID},
			bson.M{
				"$set":   bson.M{"published": false, "updated_at": now},
				"$unset": bson.M{"published_at": ""},
			},
		)
		if txErr != nil {
			return txErr
		}
		if cres.MatchedCount == 0 {
			return pubErr(CodeContentNotFound, "content "+contentID.Hex()+" not found", nil)
		}

		if txErr := r.outbox.InsertUnique(sc, EventUnpublished, current.ID, eventPayload(current)); txErr != nil {
			return txErr
		}
		did = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return did, nil
}

// MarkFailed flips staged -> failed in ONE transaction with the
// publication.failed outbox event (spec §18.1: the failed event must be
// inserted in the same transaction as the status update, never after). The
// failure reason is recorded for operator triage; the bytes are left for the
// 7-day failed/staged retention GC. Repeating MarkFailed on an already-failed
// record is idempotent. Any other source state is CodeInvalidTransition.
func (r *Repository) MarkFailed(ctx context.Context, publicationID primitive.ObjectID, reason string) error {
	return r.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		pubs := r.db.Collection(CollectionPublications)

		var p Publication
		if err := pubs.FindOne(sc, bson.M{"_id": publicationID}).Decode(&p); err != nil {
			if err == mongo.ErrNoDocuments {
				return pubErr(CodeNotFound, "publication "+publicationID.Hex()+" not found", err)
			}
			return err
		}
		if p.Status == StatusFailed {
			return nil // idempotent
		}
		if p.Status != StatusStaged {
			return pubErr(CodeInvalidTransition,
				"lifecycle transition "+string(p.Status)+" -> failed is not allowed", nil)
		}

		res, err := pubs.UpdateOne(sc,
			bson.M{"_id": publicationID, "status": string(StatusStaged)},
			bson.M{"$set": bson.M{"status": string(StatusFailed), "failure_reason": reason}},
		)
		if err != nil {
			return err
		}
		if res.MatchedCount == 0 {
			again, err := r.getByIDIn(sc, publicationID)
			if err != nil {
				return err
			}
			if again.Status == StatusFailed {
				return nil
			}
			return pubErr(CodeConflict, "concurrent publication change during mark-failed", nil)
		}

		p.Status = StatusFailed
		p.FailureReason = reason
		payload := eventPayload(&p)
		payload["failure_reason"] = reason
		if err := r.outbox.InsertUnique(sc, EventFailed, publicationID, payload); err != nil {
			return err
		}
		return nil
	})
}

// getActiveIn loads the active publication inside a transaction session.
func (r *Repository) getActiveIn(sc mongo.SessionContext, contentID primitive.ObjectID) (*Publication, error) {
	var p Publication
	err := r.db.Collection(CollectionPublications).FindOne(sc, bson.M{
		"content_id": contentID,
		"status":     string(StatusActive),
	}).Decode(&p)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// getByIDIn loads one publication inside a transaction session.
func (r *Repository) getByIDIn(sc mongo.SessionContext, id primitive.ObjectID) (*Publication, error) {
	var p Publication
	err := r.db.Collection(CollectionPublications).FindOne(sc, bson.M{"_id": id}).Decode(&p)
	if err == mongo.ErrNoDocuments {
		return nil, pubErr(CodeNotFound, "publication "+id.Hex()+" not found", err)
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}
