// Retention GC and outbox poison/TTL sweep (Task 10, integration owner).
//
// Scope boundary (plan Task 10; spec §17.5 row "failed/superseded object 超过
// retention", §17.6, §18.1):
//   - SweepRetention deletes expired immutable objects and flips
//     storage_state=deleted ONLY after the storage delete succeeds; metadata
//     rows are retained for audit. Active and pinned objects are never
//     collected: superseded/unpublished default to 90 days (superseded_at /
//     unpublished_at, falling back to created_at), failed/staged to 7 days
//     (created_at), quarantine files to 30 days (file modtime).
//   - SweepOutbox owns the poisoning rule Task 9 deferred: a pending row at
//     MaxAttempts (default 10) or older than MaxAge (default 7d) is terminally
//     failed with a P0 instead of retried forever (Task 9 reserved the
//     `failed` state and owns delivery). Terminal (delivered/failed) rows
//     expire after the message TTL (default 90d). Receivers still dedupe on
//     the stable event ID if a late duplicate is ever recreated.
//   - ScanOnce (recovery.go) runs both sweeps as phases; server wiring that
//     chooses production intervals belongs to Task 16.
//
// This file performs no Mongo writes outside the publication and outbox
// collections, and no file writes outside immutable objects plus the
// quarantine directory.
package publication

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Retention/GC defaults (spec §17.6). ScannerOptions overrides each one;
// Task 16 wires production values from config.
const (
	// DefaultScanInterval is the spec §17.5 cadence: one startup scan, then
	// every 10 minutes, single-flight.
	DefaultScanInterval = 10 * time.Minute
	// DefaultRetentionDays keeps superseded/unpublished objects for 90 days.
	DefaultRetentionDays = 90
	// DefaultFailedRetentionDays keeps failed/staged objects for 7 days.
	DefaultFailedRetentionDays = 7
	// DefaultQuarantineRetentionDays keeps preserved files for 30 days.
	DefaultQuarantineRetentionDays = 30
	// DefaultOutboxMaxAttempts poisons a pending outbox row once its worker
	// claim count reaches 10.
	DefaultOutboxMaxAttempts = 10
	// DefaultOutboxMaxAge poisons a pending outbox row older than 7 days even
	// below the attempt ceiling.
	DefaultOutboxMaxAge = 7 * 24 * time.Hour
	// DefaultOutboxTerminalRetentionDays deletes delivered/failed outbox rows
	// past 90 days (message TTL owned by Task 10; delivery owned by Task 9).
	DefaultOutboxTerminalRetentionDays = 90
)

// GCReport accounts one retention pass. Counts are objects/files actually
// deleted, never promises about the next pass.
type GCReport struct {
	// ObjectsDeleted counts immutable objects removed with storage_state
	// flipped to deleted.
	ObjectsDeleted int
	// ObjectDeleteErrors counts eligible objects whose storage delete failed;
	// their metadata is untouched for retry.
	ObjectDeleteErrors int
	// QuarantineFilesDeleted counts expired preserved files removed.
	QuarantineFilesDeleted int
}

// OutboxSweepReport accounts one outbox poison/TTL pass.
type OutboxSweepReport struct {
	// Poisoned counts undeliverable pending rows moved to terminal failed.
	Poisoned int
	// ExpiredDeleted counts terminal rows removed past the message TTL.
	ExpiredDeleted int
}

func (s *Scanner) retentionDays() int {
	if s.opts.RetentionDays > 0 {
		return s.opts.RetentionDays
	}
	return DefaultRetentionDays
}

func (s *Scanner) failedRetentionDays() int {
	if s.opts.FailedRetentionDays > 0 {
		return s.opts.FailedRetentionDays
	}
	return DefaultFailedRetentionDays
}

func (s *Scanner) quarantineRetentionDays() int {
	if s.opts.QuarantineRetentionDays > 0 {
		return s.opts.QuarantineRetentionDays
	}
	return DefaultQuarantineRetentionDays
}

func (s *Scanner) outboxMaxAttempts() int {
	if s.opts.OutboxMaxAttempts > 0 {
		return s.opts.OutboxMaxAttempts
	}
	return DefaultOutboxMaxAttempts
}

func (s *Scanner) outboxMaxAge() time.Duration {
	if s.opts.OutboxMaxAge > 0 {
		return s.opts.OutboxMaxAge
	}
	return DefaultOutboxMaxAge
}

func (s *Scanner) outboxTerminalRetentionDays() int {
	if s.opts.OutboxTerminalRetentionDays > 0 {
		return s.opts.OutboxTerminalRetentionDays
	}
	return DefaultOutboxTerminalRetentionDays
}

// SweepRetention deletes expired immutable objects (storage deletion precedes
// the storage_state=deleted flip; failures leave metadata untouched) and
// expires old quarantine files. Publication metadata rows are always
// retained. Active and pinned objects are never collected.
func (s *Scanner) SweepRetention(ctx context.Context) (GCReport, error) {
	var rep GCReport
	now := s.now()
	if err := s.sweepExpiredObjects(ctx, now, &rep); err != nil {
		return rep, err
	}
	if err := s.sweepQuarantineDir(now, &rep); err != nil {
		return rep, err
	}
	return rep, nil
}

// SweepOutbox poisons undeliverable pending rows (P0 per row) and deletes
// terminal rows past the message TTL.
func (s *Scanner) SweepOutbox(ctx context.Context) (OutboxSweepReport, error) {
	rep, poisoned, err := s.sweepOutbox(ctx, s.now())
	if err != nil {
		return rep, err
	}
	for _, id := range poisoned {
		if s.opts.Alert != nil {
			s.opts.Alert(Alert{Severity: AlertSeverityP0, Code: CodeOutboxPoisoned,
				PublicationID: &id,
				Message:       "outbox row undeliverable; moved to terminal failed instead of retried forever"})
		}
	}
	return rep, nil
}

// runRetentionSweep is the ScanOnce phase wrapper: failures are recorded on
// the report, never fatal to the pass.
func (s *Scanner) runRetentionSweep(ctx context.Context, st *scanState) (GCReport, error) {
	return s.SweepRetention(ctx)
}

// runOutboxSweep is the ScanOnce phase wrapper: poison alerts join the pass
// report in addition to the Alert sink.
func (s *Scanner) runOutboxSweep(ctx context.Context, st *scanState) (OutboxSweepReport, error) {
	rep, poisoned, err := s.sweepOutbox(ctx, st.now)
	if err != nil {
		return rep, err
	}
	for _, id := range poisoned {
		pid := id
		s.emit(st, Alert{Severity: AlertSeverityP0, Code: CodeOutboxPoisoned,
			PublicationID: &pid,
			Message:       "outbox row undeliverable; moved to terminal failed instead of retried forever"})
	}
	return rep, nil
}

// gcCandidate is the retention projection of one publication record.
type gcCandidate struct {
	ID            primitive.ObjectID `bson:"_id"`
	ContentID     primitive.ObjectID `bson:"content_id"`
	Status        string             `bson:"status"`
	Pinned        bool               `bson:"pinned"`
	CreatedAt     time.Time          `bson:"created_at"`
	SupersededAt  *time.Time         `bson:"superseded_at"`
	UnpublishedAt *time.Time         `bson:"unpublished_at"`
}

// sweepExpiredObjects deletes eligible immutable objects. Only records whose
// storage_state is not yet deleted are examined; the storage delete runs
// first and the metadata flip follows on success.
func (s *Scanner) sweepExpiredObjects(ctx context.Context, now time.Time, rep *GCReport) error {
	retentionCutoff := now.AddDate(0, 0, -s.retentionDays())
	failedCutoff := now.AddDate(0, 0, -s.failedRetentionDays())
	cur, err := s.db.Collection(CollectionPublications).Find(ctx,
		bson.M{"storage_state": bson.M{"$ne": string(StorageDeleted)}},
		options.Find().SetProjection(bson.M{
			"_id": 1, "content_id": 1, "status": 1, "pinned": 1,
			"created_at": 1, "superseded_at": 1, "unpublished_at": 1,
		}))
	if err != nil {
		return err
	}
	var rows []gcCandidate
	if err := cur.All(ctx, &rows); err != nil {
		_ = cur.Close(ctx)
		return err
	}
	_ = cur.Close(ctx)
	for _, r := range rows {
		if r.Status == string(StatusActive) || r.Pinned {
			continue // active and pinned objects are never GC'd
		}
		var cutoff time.Time
		switch r.Status {
		case string(StatusSuperseded):
			cutoff = retentionCutoff
			if r.SupersededAt != nil && !r.SupersededAt.IsZero() {
				if r.SupersededAt.After(cutoff) {
					continue
				}
			} else if r.CreatedAt.After(cutoff) {
				continue
			}
		case string(StatusUnpublished):
			cutoff = retentionCutoff
			if r.UnpublishedAt != nil && !r.UnpublishedAt.IsZero() {
				if r.UnpublishedAt.After(cutoff) {
					continue
				}
			} else if r.CreatedAt.After(cutoff) {
				continue
			}
		case string(StatusFailed), string(StatusStaged):
			if r.CreatedAt.After(failedCutoff) {
				continue
			}
		default:
			continue
		}
		if err := s.store.DeleteImmutable(ctx, r.ContentID, r.ID); err != nil {
			rep.ObjectDeleteErrors++
			s.auditf(ctx, "gc.object_delete_failed", map[string]any{
				"publication_id": r.ID.Hex(), "error": err.Error()})
			continue
		}
		if _, err := s.db.Collection(CollectionPublications).UpdateOne(ctx,
			bson.M{"_id": r.ID},
			bson.M{"$set": bson.M{
				"storage_state":      string(StorageDeleted),
				"storage_deleted_at": now,
			}}); err != nil {
			rep.ObjectDeleteErrors++
			s.auditf(ctx, "gc.object_delete_failed", map[string]any{
				"publication_id": r.ID.Hex(), "error": err.Error()})
			continue
		}
		rep.ObjectsDeleted++
		s.auditf(ctx, "gc.object_deleted", map[string]any{
			"publication_id": r.ID.Hex(), "status": r.Status})
	}
	return nil
}

// sweepQuarantineDir deletes preserved files older than the quarantine
// retention. The quarantine tree may hold subdirectories mirroring served
// paths, so it walks recursively and only removes regular files.
func (s *Scanner) sweepQuarantineDir(now time.Time, rep *GCReport) error {
	qdir := s.quarantineDir()
	if qdir == "" {
		return nil
	}
	if st, err := os.Stat(qdir); err != nil || !st.IsDir() {
		return nil
	}
	cutoff := now.AddDate(0, 0, -s.quarantineRetentionDays())
	var firstErr error
	_ = filepath.WalkDir(qdir, func(abs string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().After(cutoff) {
			return nil // fresh forensics are kept
		}
		if rerr := os.Remove(abs); rerr != nil && !os.IsNotExist(rerr) {
			if firstErr == nil {
				firstErr = rerr
			}
			return nil
		}
		rep.QuarantineFilesDeleted++
		return nil
	})
	return firstErr
}

// sweepOutbox poisons undeliverable pending rows and expires terminal rows,
// returning the poisoned aggregate IDs so callers can alert on each one.
func (s *Scanner) sweepOutbox(ctx context.Context, now time.Time) (OutboxSweepReport, []primitive.ObjectID, error) {
	var rep OutboxSweepReport
	poisoned, err := s.poisonOutbox(ctx, now, &rep)
	if err != nil {
		return rep, nil, err
	}
	if err := s.expireOutboxTerminal(ctx, now, &rep); err != nil {
		return rep, poisoned, err
	}
	return rep, poisoned, nil
}

// poisonOutbox moves pending rows at MaxAttempts or older than MaxAge to
// terminal failed. Only pending rows are eligible: delivering rows belong to
// the Task 9 worker lease/takeover path and must not be poisoned from under
// it. next_attempt_at is ignored — a row at the attempt ceiling is
// undeliverable whenever it would next run.
func (s *Scanner) poisonOutbox(ctx context.Context, now time.Time, rep *OutboxSweepReport) ([]primitive.ObjectID, error) {
	maxAttempts := s.outboxMaxAttempts()
	ageCutoff := now.Add(-s.outboxMaxAge())
	cur, err := s.db.Collection(CollectionOutbox).Find(ctx,
		bson.M{
			"state": OutboxStatePending,
			"$or": []bson.M{
				{"attempt": bson.M{"$gte": maxAttempts}},
				{"created_at": bson.M{"$lte": ageCutoff}},
			},
		},
		options.Find().SetProjection(bson.M{"_id": 1, "aggregate_id": 1, "attempt": 1}))
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID          primitive.ObjectID `bson:"_id"`
		AggregateID primitive.ObjectID `bson:"aggregate_id"`
		Attempt     int                `bson:"attempt"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		_ = cur.Close(ctx)
		return nil, err
	}
	_ = cur.Close(ctx)
	var poisoned []primitive.ObjectID
	for _, r := range rows {
		reason := fmt.Sprintf("outbox poisoned: undeliverable after %d attempts (max %d) (recovery scanner)", r.Attempt, maxAttempts)
		res, err := s.db.Collection(CollectionOutbox).UpdateOne(ctx,
			bson.M{"_id": r.ID, "state": OutboxStatePending},
			bson.M{"$set": bson.M{
				"state":          OutboxStateFailed,
				"failed_at":      now,
				"failure_reason": reason,
			}})
		if err != nil {
			return poisoned, err
		}
		if res.MatchedCount == 0 {
			continue // claimed/delivered concurrently; leave it alone
		}
		rep.Poisoned++
		poisoned = append(poisoned, r.AggregateID)
		s.auditf(ctx, "outbox.poisoned", map[string]any{
			"aggregate_id": r.AggregateID.Hex(), "attempt": r.Attempt})
	}
	return poisoned, nil
}

// expireOutboxTerminal deletes delivered/failed rows whose terminal time
// (delivered_at, else failed_at, else created_at) is past the message TTL.
func (s *Scanner) expireOutboxTerminal(ctx context.Context, now time.Time, rep *OutboxSweepReport) error {
	cutoff := now.AddDate(0, 0, -s.outboxTerminalRetentionDays())
	cur, err := s.db.Collection(CollectionOutbox).Find(ctx,
		bson.M{"state": bson.M{"$in": []string{OutboxStateDelivered, OutboxStateFailed}}},
		options.Find().SetProjection(bson.M{
			"_id": 1, "created_at": 1, "delivered_at": 1, "failed_at": 1,
		}))
	if err != nil {
		return err
	}
	var rows []struct {
		ID          primitive.ObjectID `bson:"_id"`
		CreatedAt   time.Time          `bson:"created_at"`
		DeliveredAt *time.Time         `bson:"delivered_at"`
		FailedAt    *time.Time         `bson:"failed_at"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		_ = cur.Close(ctx)
		return err
	}
	_ = cur.Close(ctx)
	var ids []primitive.ObjectID
	for _, r := range rows {
		terminal := r.CreatedAt
		if r.DeliveredAt != nil && !r.DeliveredAt.IsZero() {
			terminal = *r.DeliveredAt
		} else if r.FailedAt != nil && !r.FailedAt.IsZero() {
			terminal = *r.FailedAt
		}
		if terminal.After(cutoff) {
			continue
		}
		ids = append(ids, r.ID)
	}
	if len(ids) == 0 {
		return nil
	}
	res, err := s.db.Collection(CollectionOutbox).DeleteMany(ctx, bson.M{"_id": bson.M{"$in": ids}})
	if err != nil {
		return err
	}
	rep.ExpiredDeleted = int(res.DeletedCount)
	return nil
}
