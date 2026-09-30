// Recovery scanner (Task 10, integration owner).
//
// Scope boundary (plan Task 10; spec §17.5, §17.6, §35):
//   - ScanOnce(ctx) (ScanReport, error) + Run(ctx); startup scan then every
//     10 minutes, single-flight.
//   - Authority order: Mongo active Publication -> immutable object ->
//     canonical projection (rebuildable). Files NEVER elect an active record.
//   - Orphan quarantine gated on system_migrations.publication_model_v1 ==
//     completed; running/absent only reports.
//   - Restart repair consumes Task 8's ErrStopAfterRename fixture state
//     (new canonical + .previous-{oldID} on disk, OLD active in DB).
//   - Mismatched canonicals are quarantined BEFORE calling Task 6 Restore,
//     because Restore replaces bytes without preserving them.
//   - Task 6 Inspect hard failures (unreadable sidecars) are P0 alerts, not
//     clean bills of health.
//   - Publication.ContentHash uses the "sha256:<hex>" record form; the file
//     layer compares raw hex — the prefix is stripped for every comparison.
//   - legacy_unverified pages hold an active Publication, so they are never
//     orphans; the scanner keeps them serving.
//   - Retention/GC policy lives in gc.go; ScanOnce runs it as a phase.
//
// This file performs no Mongo writes outside the publication, outbox, and
// migration-flag collections, and no file writes outside the store-resolved
// canonical tree plus the quarantine directory. Server wiring (which passes
// the store root, intervals, and alert sinks) belongs to Task 16.
package publication

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/database"
	"github.com/jonradoff/lightcms/v7/internal/observe"
	"github.com/jonradoff/lightcms/v7/internal/product/storage"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Migration-flag contract (spec §35). Task 14 owns the writer
// (internal/product/migration/*); this scanner only reads. Accepted shapes
// are deliberately lenient so the writer can use {_id,key,name} x
// {status,state}: any document identifying publication_model_v1 whose
// status field reads "completed" opens the orphan-quarantine gate.
const (
	// CollectionMigrations tracks one-shot model migrations.
	CollectionMigrations = "system_migrations"
	// MigrationKeyPublicationModelV1 is the flag Task 14 flips
	// not_started -> running -> completed.
	MigrationKeyPublicationModelV1 = "publication_model_v1"
	// MigrationCompleted is the only state that permits orphan quarantine.
	MigrationCompleted = "completed"
	// MigrationRunning means the reconciler is still importing legacy pages:
	// canonical-without-active is reported, never quarantined.
	MigrationRunning = "running"
)

// Alert severities.
const (
	// AlertSeverityP0 needs a human now (data loss risk, divergence, poison).
	AlertSeverityP0 = "P0"
	// AlertSeverityWarn records intended repairs and reports.
	AlertSeverityWarn = "WARN"
)

// Scanner alert codes.
const (
	// CodeInspectFailed: Inspect errored (e.g. unreadable sidecar). The
	// content is skipped, never called clean.
	CodeInspectFailed = "RECOVERY_INSPECT_FAILED"
	// CodeMultipleActive: more than one active record; auto-repair stopped.
	CodeMultipleActive = "RECOVERY_MULTIPLE_ACTIVE"
	// CodeImmutableMissing: active bytes have no immutable object to rebuild
	// from. No guessing; site marked degraded.
	CodeImmutableMissing = "RECOVERY_IMMUTABLE_MISSING"
	// CodeImmutableCorrupt: immutable object present but hash-mismatched.
	CodeImmutableCorrupt = "RECOVERY_IMMUTABLE_CORRUPT"
	// CodeCanonicalMismatch: canonical bytes diverged from the active record;
	// bad bytes quarantined, canonical rebuilt.
	CodeCanonicalMismatch = "RECOVERY_CANONICAL_MISMATCH"
	// CodeOrphanQuarantined: serving file with no active record quarantined
	// (migration completed only).
	CodeOrphanQuarantined = "RECOVERY_ORPHAN_QUARANTINED"
	// CodeOrphanReported: same, but migration not completed — report only.
	CodeOrphanReported = "RECOVERY_ORPHAN_REPORTED"
	// CodeOutboxPoisoned: an outbox row exhausted the poisoning rule and was
	// moved to terminal failed (Task 10 owns this rule; Task 9 reserved the
	// state and owns delivery).
	CodeOutboxPoisoned = "RECOVERY_OUTBOX_POISONED"
	// CodeRepairError: transient repair failure (IO, CAS race); recorded and
	// retried on the next scan, never fatal to the whole pass.
	CodeRepairError = "RECOVERY_REPAIR_ERROR"
)

// Scanner audit actions emitted through Options.Audit.
const (
	AuditCanonicalRebuilt    = "recovery.canonical_rebuilt"
	AuditCanonicalRepaired   = "recovery.canonical_mismatch_repaired"
	AuditPreviousRestored    = "recovery.previous_restored"
	AuditPreviousCleaned     = "recovery.previous_cleaned"
	AuditBackupRestored      = "recovery.backup_restored"
	AuditBackupCleaned       = "recovery.backup_cleaned"
	AuditStaleNextCleaned    = "recovery.stale_next_cleaned"
	AuditStaleStagedFailed   = "recovery.stale_staged_failed"
	AuditOrphanQuarantined   = "recovery.orphan_quarantined"
	AuditQuarantinePreserved = "recovery.quarantine_preserved"
)

// Alert is one scanner finding. Severity P0 needs a human; WARN records an
// intended repair or a report-only observation.
type Alert struct {
	Severity      string
	Code          string
	Message       string
	Path          string
	ContentID     *primitive.ObjectID
	PublicationID *primitive.ObjectID
}

// QuarantineRecord tracks one preserved file: the original absolute path,
// its quarantine location, why it was moved, and when.
type QuarantineRecord struct {
	OriginalPath   string
	QuarantinePath string
	Reason         string
	At             time.Time
}

// ScanReport is the full account of one ScanOnce pass. Counts are repairs
// performed (or P0s found), never promises about the next pass.
type ScanReport struct {
	MigrationState            string
	ContentsScanned           int
	ContentsSkippedLocked     int
	CanonicalRebuilt          int
	CanonicalMismatchRepaired int
	ImmutableMissingP0        int
	ImmutableCorruptP0        int
	MultipleActiveP0          int
	InspectErrors             int
	PreviousRestored          int
	PreviousCleaned           int
	BackupRestored            int
	BackupCleaned             int
	StaleNextCleaned          int
	StaleStagedFailed         int
	OrphanReported            int
	OrphanQuarantined         int
	RepairErrors              int
	Quarantined               []QuarantineRecord
	Retention                 GCReport
	Outbox                    OutboxSweepReport
	Alerts                    []Alert
	Degraded                  bool
}

// ScannerOptions configures the recovery scanner. Zero values select spec
// defaults (§36 and §17.6); Task 16 wires production values from config.
type ScannerOptions struct {
	// StageTimeout overrides the stale-stage / stale-.next horizon.
	// Default storage.DefaultStageTimeout (15m, spec §36).
	StageTimeout time.Duration
	// RetentionDays for superseded/unpublished objects. Default 90 (§17.6).
	RetentionDays int
	// FailedRetentionDays for failed objects. Default 7 (§17.6).
	FailedRetentionDays int
	// QuarantineRetentionDays for preserved files. Default 30 (§17.6).
	QuarantineRetentionDays int
	// OutboxMaxAttempts poisons an undelivered row at this attempt count.
	// Default DefaultOutboxMaxAttempts. See gc.go for the rule.
	OutboxMaxAttempts int
	// OutboxMaxAge poisons an undelivered row older than this.
	// Default DefaultOutboxMaxAge.
	OutboxMaxAge time.Duration
	// OutboxTerminalRetentionDays deletes delivered/failed rows past this
	// age. Default DefaultOutboxTerminalRetentionDays.
	OutboxTerminalRetentionDays int
	// ScanInterval for Run. Default DefaultScanInterval (10m, §17.5).
	ScanInterval time.Duration
	// QuarantineDir holds preserved files outside the served tree. Defaults
	// to <filesystem-root>/quarantine when the store exposes its root.
	QuarantineDir string
	// Now injects time (tests backdate records/files instead of sleeping).
	Now func() time.Time
	// Audit receives one event per repair (may be nil).
	Audit func(ctx context.Context, action string, fields map[string]any)
	// Alert receives every alert in addition to report collection.
	Alert func(Alert)
}

// Scanner reconciles Mongo publication truth with filesystem truth on one
// instance. It shares the saga's in-process page locks: content under an
// active publish is skipped, never raced.
type Scanner struct {
	db       *database.DB
	repo     *Repository
	store    storage.Store
	opts     ScannerOptions
	running  atomic.Bool
	degraded atomic.Bool
}

// NewScanner builds a scanner. db/repo/store are required; opts zero values
// select spec defaults.
func NewScanner(db *database.DB, repo *Repository, store storage.Store, opts ScannerOptions) *Scanner {
	return &Scanner{db: db, repo: repo, store: store, opts: opts}
}

func (s *Scanner) now() time.Time {
	if s.opts.Now != nil {
		return s.opts.Now()
	}
	return time.Now()
}

func (s *Scanner) stageTimeout() time.Duration {
	if s.opts.StageTimeout > 0 {
		return s.opts.StageTimeout
	}
	return storage.DefaultStageTimeout
}

func (s *Scanner) scanInterval() time.Duration {
	if s.opts.ScanInterval > 0 {
		return s.opts.ScanInterval
	}
	return DefaultScanInterval
}

func (s *Scanner) quarantineDir() string {
	if s.opts.QuarantineDir != "" {
		return s.opts.QuarantineDir
	}
	if fs, ok := s.store.(*storage.FilesystemStore); ok && fs.Root != "" {
		return filepath.Join(fs.Root, "quarantine")
	}
	return ""
}

// Degraded reports whether the last scan found active bytes unrecoverable
// (missing/corrupt immutable object). Latched per scan; use ResetDegraded
// after human remediation verified a clean pass.
func (s *Scanner) Degraded() bool { return s.degraded.Load() }

// ResetDegraded clears the degraded latch (operator action after triage).
func (s *Scanner) ResetDegraded() { s.degraded.Store(false) }

// scanState threads per-pass mutable state through the phases.
type scanState struct {
	rpt       *ScanReport
	serving   map[string]bool // full paths owned by content or active records
	genRoot   string
	now       time.Time
	completed bool // migration completed: orphan quarantine open
}

func (s *Scanner) newScan() *scanState {
	return &scanState{
		rpt:     &ScanReport{Quarantined: []QuarantineRecord{}, Alerts: []Alert{}},
		serving: map[string]bool{},
		now:     s.now(),
	}
}

func (s *Scanner) emit(st *scanState, a Alert) {
	st.rpt.Alerts = append(st.rpt.Alerts, a)
	if s.opts.Alert != nil {
		s.opts.Alert(a)
	}
}

func (s *Scanner) auditf(ctx context.Context, action string, fields map[string]any) {
	if s.opts.Audit == nil {
		return
	}
	s.opts.Audit(ctx, action, fields)
}

// stripRecordHash converts the "sha256:<hex>" record form (spec §15.2) to
// the raw-hex file-layer form Task 8's handoff requires for comparison.
func stripRecordHash(h string) string {
	return strings.TrimPrefix(strings.TrimSpace(h), "sha256:")
}

// MigrationState reads system_migrations.publication_model_v1. Absent or
// unparseable reads as "" (not completed): the quarantine gate stays shut.
// A hard DB error is returned so ScanOnce can downgrade to report-only mode
// loudly instead of guessing the gate.
func (s *Scanner) MigrationState(ctx context.Context) (string, error) {
	var doc bson.M
	err := s.db.Collection(CollectionMigrations).FindOne(ctx, bson.M{
		"$or": []bson.M{
			{"_id": MigrationKeyPublicationModelV1},
			{"key": MigrationKeyPublicationModelV1},
			{"name": MigrationKeyPublicationModelV1},
		},
	}).Decode(&doc)
	if err == mongo.ErrNoDocuments {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for _, k := range []string{"status", "state", "value"} {
		if v, ok := doc[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v), nil
		}
	}
	return "", nil
}

// ScanOnce runs one full pass: per-content repair, orphan sweep, retention
// GC, and the outbox poison/TTL sweep. Per-item failures are recorded in the
// report (and alerted), never fatal to the pass; only content enumeration
// failure aborts with an error.
func (s *Scanner) ScanOnce(ctx context.Context) (ScanReport, error) {
	st := s.newScan()

	mig, err := s.MigrationState(ctx)
	if err != nil {
		s.emit(st, Alert{Severity: AlertSeverityWarn, Code: CodeRepairError,
			Message: "migration flag unreadable; orphan quarantine disabled this pass: " + err.Error()})
		mig = ""
	}
	st.rpt.MigrationState = mig
	st.completed = strings.EqualFold(mig, MigrationCompleted)

	if gr, err := s.generatedRoot(); err != nil {
		s.emit(st, Alert{Severity: AlertSeverityWarn, Code: CodeRepairError,
			Message: "generated root unresolvable; orphan sweep skipped: " + err.Error()})
	} else {
		st.genRoot = gr
	}

	type contentRef struct {
		id   primitive.ObjectID
		path string
	}
	var refs []contentRef
	cur, err := s.db.Collection(CollectionContent).Find(ctx, bson.M{},
		options.Find().SetProjection(bson.M{"_id": 1, "full_path": 1}))
	if err != nil {
		return *st.rpt, err
	}
	var docs []struct {
		ID       primitive.ObjectID `bson:"_id"`
		FullPath string             `bson:"full_path"`
	}
	if err := cur.All(ctx, &docs); err != nil {
		return *st.rpt, err
	}
	_ = cur.Close(ctx)
	for _, d := range docs {
		if d.FullPath == "" {
			continue
		}
		refs = append(refs, contentRef{id: d.ID, path: d.FullPath})
		st.serving[d.FullPath] = true
	}

	for _, r := range refs {
		s.reconcileContent(ctx, st, r.id, r.path)
	}
	s.sweepOrphans(ctx, st)

	if rep, err := s.runRetentionSweep(ctx, st); err != nil {
		s.emit(st, Alert{Severity: AlertSeverityWarn, Code: CodeRepairError,
			Message: "retention sweep failed: " + err.Error()})
	} else {
		st.rpt.Retention = rep
	}
	if rep, err := s.runOutboxSweep(ctx, st); err != nil {
		s.emit(st, Alert{Severity: AlertSeverityWarn, Code: CodeRepairError,
			Message: "outbox sweep failed: " + err.Error()})
	} else {
		st.rpt.Outbox = rep
	}

	s.degraded.Store(st.rpt.Degraded)
	return *st.rpt, nil
}

// Run performs the startup scan, then scans every ScanInterval until ctx is
// cancelled. Only one Run executes at a time; a second caller returns
// immediately. Scan errors are alerted, never fatal to the loop.
func (s *Scanner) Run(ctx context.Context) {
	if !s.running.CompareAndSwap(false, true) {
		return
	}
	defer s.running.Store(false)
	if rep, err := s.ScanOnce(ctx); err != nil {
		s.auditf(ctx, AuditCanonicalRebuilt, map[string]any{"startup_scan_error": err.Error()})
	} else {
		s.observeScan(rep)
	}
	t := time.NewTicker(s.scanInterval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if rep, err := s.ScanOnce(ctx); err != nil {
				s.auditf(ctx, AuditCanonicalRebuilt, map[string]any{"scan_error": err.Error()})
			} else {
				s.observeScan(rep)
			}
		}
	}
}

// observeScan feeds one scan report into the Task 16F counters and P0
// alerts: every repair increments scanner_repairs_total, every alert
// increments scanner_alerts_total, and any P0-coded finding pages the
// existing operational log surface immediately.
func (s *Scanner) observeScan(rep ScanReport) {
	repairs := int64(rep.CanonicalRebuilt + rep.CanonicalMismatchRepaired +
		rep.PreviousRestored + rep.PreviousCleaned + rep.BackupRestored +
		rep.BackupCleaned + rep.StaleNextCleaned + rep.StaleStagedFailed +
		rep.OrphanQuarantined)
	if repairs > 0 {
		observe.Default().AddScannerRepairs(repairs)
	}
	if len(rep.Alerts) > 0 {
		observe.Default().AddScannerAlerts(int64(len(rep.Alerts)))
	}
	p0 := int64(rep.ImmutableMissingP0 + rep.ImmutableCorruptP0 + rep.MultipleActiveP0)
	if p0 > 0 {
		observe.AlertP0("scanner_active_pointer_inconsistency",
			fmt.Sprintf("scanner found %d P0 inconsistencies (missing=%d corrupt=%d multi-active=%d)",
				p0, rep.ImmutableMissingP0, rep.ImmutableCorruptP0, rep.MultipleActiveP0),
			observe.Fields{Stage: "scanner"})
	}
}

// reconcileContent repairs one page under its saga page lock. A held publish
// lock means an active saga owns the files: skip, never race.
func (s *Scanner) reconcileContent(ctx context.Context, st *scanState, id primitive.ObjectID, contentPath string) {
	release, ok := acquireSagaLocks(id, lockPathsFor(contentPath))
	if !ok {
		st.rpt.ContentsSkippedLocked++
		return
	}
	defer release()
	st.rpt.ContentsScanned++

	actives, err := s.listActive(ctx, id)
	if err != nil {
		s.emit(st, Alert{Severity: AlertSeverityWarn, Code: CodeRepairError, Path: contentPath,
			ContentID: &id, Message: "active lookup failed; content skipped: " + err.Error()})
		st.rpt.RepairErrors++
		return
	}
	if len(actives) > 1 {
		st.rpt.MultipleActiveP0++
		s.emit(st, Alert{Severity: AlertSeverityP0, Code: CodeMultipleActive, Path: contentPath,
			ContentID: &id, Message: "multiple active publications; auto-repair stopped for this content"})
		return
	}
	if len(actives) == 1 {
		active := actives[0]
		if active.FullPath != "" {
			st.serving[active.FullPath] = true
		}
		s.reconcileActive(ctx, st, &active)
	} else {
		s.reconcileNoActive(ctx, st, id, contentPath)
	}
	// Stale staged candidates are reaped under the same page lock, so an
	// in-flight saga (which always holds its lock) is never reaped.
	s.reapStaleStaged(ctx, st, id)
}

// listActive returns up to 3 active records so callers can detect the
// multiple-active P0 (the partial unique index normally prevents it, but it
// may be absent before Task 14 finishes remediation).
func (s *Scanner) listActive(ctx context.Context, contentID primitive.ObjectID) ([]Publication, error) {
	cur, err := s.db.Collection(CollectionPublications).Find(ctx,
		bson.M{"content_id": contentID, "status": string(StatusActive)},
		options.Find().SetLimit(3))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []Publication
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// sidecarsOf partitions an Inspect result by kind.
func sidecarsOf(info storage.CanonicalInfo) (previous, next, backups []storage.Sidecar) {
	for _, sc := range info.Sidecars {
		switch sc.Kind {
		case storage.SidecarPrevious:
			previous = append(previous, sc)
		case storage.SidecarNext:
			next = append(next, sc)
		case storage.SidecarUnpublishBackup:
			backups = append(backups, sc)
		}
	}
	return previous, next, backups
}

// reinspect reloads canonical metadata after a mutation; hard errors become
// P0 skips, never silent clean states.
func (s *Scanner) reinspect(ctx context.Context, st *scanState, active *Publication, path string) (storage.CanonicalInfo, bool) {
	info, err := s.store.Inspect(ctx, path)
	if err != nil {
		st.rpt.InspectErrors++
		s.emit(st, Alert{Severity: AlertSeverityP0, Code: CodeInspectFailed, Path: path,
			ContentID: &active.ContentID, PublicationID: &active.ID,
			Message: "inspect failed after repair step; content skipped: " + err.Error()})
		st.rpt.RepairErrors++
		return storage.CanonicalInfo{}, false
	}
	return info, true
}

// reconcileActive enforces the §17.5 active-path rules in dependency order:
// unpublish backups, crash-window previous files, canonical health, then
// post-repair sidecar cleanup.
func (s *Scanner) reconcileActive(ctx context.Context, st *scanState, active *Publication) {
	path := active.FullPath
	want := stripRecordHash(active.ContentHash)
	info, err := s.store.Inspect(ctx, path)
	if err != nil {
		st.rpt.InspectErrors++
		s.emit(st, Alert{Severity: AlertSeverityP0, Code: CodeInspectFailed, Path: path,
			ContentID: &active.ContentID, PublicationID: &active.ID,
			Message: "inspect failed (unreadable sidecar?); content skipped, not clean: " + err.Error()})
		return
	}

	// 1. Unpublish backups: DB active + canonical absent means the unpublish
	// saga crashed between the backup rename and its transaction — restore.
	// A healthy canonical means a stale backup: remove it.
	mutated := false
	_, _, backups := sidecarsOf(info)
	for _, sc := range backups {
		if !info.Exists {
			if rerr := s.store.RestoreUnpublishBackup(ctx, path, sc.PublicationID); rerr != nil {
				st.rpt.RepairErrors++
				s.emit(st, Alert{Severity: AlertSeverityWarn, Code: CodeRepairError, Path: path,
					ContentID: &active.ContentID, PublicationID: &active.ID,
					Message: "backup restore failed: " + rerr.Error()})
				continue
			}
			st.rpt.BackupRestored++
			s.auditf(ctx, AuditBackupRestored, map[string]any{
				"content_id": active.ContentID.Hex(), "publication_id": active.ID.Hex(), "full_path": path})
			mutated = true
			break
		}
		if strings.EqualFold(info.SHA256, want) {
			if rerr := s.store.RemoveUnpublishBackup(ctx, path, sc.PublicationID); rerr != nil {
				st.rpt.RepairErrors++
				s.emit(st, Alert{Severity: AlertSeverityWarn, Code: CodeRepairError, Path: path,
					ContentID: &active.ContentID, Message: "stale backup cleanup failed: " + rerr.Error()})
				continue
			}
			st.rpt.BackupCleaned++
			s.auditf(ctx, AuditBackupCleaned, map[string]any{
				"content_id": active.ContentID.Hex(), "full_path": path})
		}
	}
	if mutated {
		var ok bool
		if info, ok = s.reinspect(ctx, st, active, path); !ok {
			return
		}
	}

	// 2. Previous files. .previous-{activeID} + DB old-active is the Task 8
	// crash window (commit never landed): restore previous, preserving the
	// displaced uncommitted bytes in quarantine (they remain recoverable
	// from the staged immutable object too).
	previous, _, _ := sidecarsOf(info)
	for _, sc := range previous {
		if sc.PublicationID != active.ID {
			continue
		}
		if info.Exists && strings.EqualFold(info.SHA256, want) {
			// Same-bytes edge: canonical already serves the active bytes.
			if cerr := s.store.ConfirmActivate(ctx, path, sc.PublicationID); cerr != nil {
				st.rpt.RepairErrors++
				s.emit(st, Alert{Severity: AlertSeverityWarn, Code: CodeRepairError, Path: path,
					ContentID: &active.ContentID, Message: "previous cleanup failed: " + cerr.Error()})
				continue
			}
			st.rpt.PreviousCleaned++
			s.auditf(ctx, AuditPreviousCleaned, map[string]any{
				"content_id": active.ContentID.Hex(), "full_path": path})
			continue
		}
		if info.Exists {
			if _, qerr := s.quarantineFile(st, info.Path, "crash-window-displaced-canonical"); qerr != nil {
				st.rpt.RepairErrors++
				s.emit(st, Alert{Severity: AlertSeverityP0, Code: CodeRepairError, Path: path,
					ContentID: &active.ContentID, PublicationID: &active.ID,
					Message: "cannot preserve displaced canonical; refusing blind restore: " + qerr.Error()})
				return
			}
		}
		if rerr := renameFile(sc.Path, info.Path); rerr != nil {
			st.rpt.RepairErrors++
			s.emit(st, Alert{Severity: AlertSeverityP0, Code: CodeRepairError, Path: path,
				ContentID: &active.ContentID, PublicationID: &active.ID,
				Message: "previous restore failed: " + rerr.Error()})
			return
		}
		st.rpt.PreviousRestored++
		s.emit(st, Alert{Severity: AlertSeverityWarn, Code: AuditPreviousRestored, Path: path,
			ContentID: &active.ContentID, PublicationID: &active.ID,
			Message: "crash-window repair: previous restored over uncommitted canonical"})
		s.auditf(ctx, AuditPreviousRestored, map[string]any{
			"content_id": active.ContentID.Hex(), "publication_id": active.ID.Hex(), "full_path": path})
		var ok bool
		if info, ok = s.reinspect(ctx, st, active, path); !ok {
			return
		}
		if info.Exists && !strings.EqualFold(info.SHA256, want) {
			// The previous file itself disagrees with the active record:
			// fall through to authoritative immutable repair below.
			break
		}
	}

	// 3. Canonical health against the active record.
	if !info.Exists {
		obj := storage.StoredObject{ContentID: active.ContentID, PublicationID: active.ID,
			Path: s.store.ImmutablePath(active.ContentID, active.ID), SHA256: want}
		if rerr := s.store.Restore(ctx, obj, path); rerr != nil {
			s.classifyRestoreError(st, active, path, rerr)
			return
		}
		st.rpt.CanonicalRebuilt++
		s.emit(st, Alert{Severity: AlertSeverityWarn, Code: AuditCanonicalRebuilt, Path: path,
			ContentID: &active.ContentID, PublicationID: &active.ID,
			Message: "canonical rebuilt from immutable bytes"})
		s.auditf(ctx, AuditCanonicalRebuilt, map[string]any{
			"content_id": active.ContentID.Hex(), "publication_id": active.ID.Hex(), "full_path": path})
	} else if !strings.EqualFold(info.SHA256, want) {
		// Preserve the bad bytes FIRST: Task 6 Restore replaces a
		// mismatched canonical without quarantining it.
		if _, qerr := s.quarantineFile(st, info.Path, "canonical-hash-mismatch"); qerr != nil {
			st.rpt.RepairErrors++
			s.emit(st, Alert{Severity: AlertSeverityP0, Code: CodeRepairError, Path: path,
				ContentID: &active.ContentID, PublicationID: &active.ID,
				Message: "cannot preserve mismatched canonical; refusing blind rebuild: " + qerr.Error()})
			return
		}
		obj := storage.StoredObject{ContentID: active.ContentID, PublicationID: active.ID,
			Path: s.store.ImmutablePath(active.ContentID, active.ID), SHA256: want}
		if rerr := s.store.Restore(ctx, obj, path); rerr != nil {
			s.classifyRestoreError(st, active, path, rerr)
			return
		}
		st.rpt.CanonicalMismatchRepaired++
		s.emit(st, Alert{Severity: AlertSeverityP0, Code: CodeCanonicalMismatch, Path: path,
			ContentID: &active.ContentID, PublicationID: &active.ID,
			Message: "canonical hash mismatch: bad bytes quarantined, canonical rebuilt from immutable object"})
		s.auditf(ctx, AuditCanonicalRepaired, map[string]any{
			"content_id": active.ContentID.Hex(), "publication_id": active.ID.Hex(), "full_path": path})
	}

	// 4. Post-repair cleanup, gated on a verified-healthy canonical so no
	// evidence is deleted before the page provably serves again.
	healthy, ok := s.reinspect(ctx, st, active, path)
	if !ok {
		return
	}
	if !healthy.Exists || !strings.EqualFold(healthy.SHA256, want) {
		return
	}
	previous, next, backups := sidecarsOf(healthy)
	for _, sc := range previous {
		if sc.PublicationID == active.ID {
			continue // restored above; re-inspect would have shown it gone
		}
		if cerr := s.store.ConfirmActivate(ctx, path, sc.PublicationID); cerr != nil {
			st.rpt.RepairErrors++
			s.emit(st, Alert{Severity: AlertSeverityWarn, Code: CodeRepairError, Path: path,
				ContentID: &active.ContentID, Message: "previous cleanup failed: " + cerr.Error()})
			continue
		}
		st.rpt.PreviousCleaned++
		s.auditf(ctx, AuditPreviousCleaned, map[string]any{
			"content_id": active.ContentID.Hex(), "full_path": path})
	}
	for _, sc := range backups {
		if rerr := s.store.RemoveUnpublishBackup(ctx, path, sc.PublicationID); rerr != nil {
			st.rpt.RepairErrors++
			s.emit(st, Alert{Severity: AlertSeverityWarn, Code: CodeRepairError, Path: path,
				ContentID: &active.ContentID, Message: "backup cleanup failed: " + rerr.Error()})
			continue
		}
		st.rpt.BackupCleaned++
		s.auditf(ctx, AuditBackupCleaned, map[string]any{
			"content_id": active.ContentID.Hex(), "full_path": path})
	}
	for _, sc := range next {
		s.cleanStaleNext(st, sc)
	}
}

// reconcileNoActive handles pages with no active publication: backups follow
// the unpublished rule, serving files without an active record follow the
// migration-gated orphan rule (§35: running only reports).
func (s *Scanner) reconcileNoActive(ctx context.Context, st *scanState, id primitive.ObjectID, contentPath string) {
	info, err := s.store.Inspect(ctx, contentPath)
	if err != nil {
		st.rpt.InspectErrors++
		s.emit(st, Alert{Severity: AlertSeverityP0, Code: CodeInspectFailed, Path: contentPath,
			ContentID: &id, Message: "inspect failed; content skipped, not clean: " + err.Error()})
		return
	}
	_, next, backups := sidecarsOf(info)
	if !info.Exists {
		// DB unpublished + canonical absent: drop leftover backups.
		for _, sc := range backups {
			if rerr := s.store.RemoveUnpublishBackup(ctx, contentPath, sc.PublicationID); rerr != nil {
				st.rpt.RepairErrors++
				s.emit(st, Alert{Severity: AlertSeverityWarn, Code: CodeRepairError, Path: contentPath,
					ContentID: &id, Message: "backup cleanup failed: " + rerr.Error()})
				continue
			}
			st.rpt.BackupCleaned++
			s.auditf(ctx, AuditBackupCleaned, map[string]any{
				"content_id": id.Hex(), "full_path": contentPath})
		}
		for _, sc := range next {
			s.cleanStaleNext(st, sc)
		}
		return
	}
	// Canonical exists with no active record: the orphan rule.
	if st.completed {
		s.quarantineOrphanFamily(ctx, st, contentPath, info, nil)
		return
	}
	st.rpt.OrphanReported++
	s.emit(st, Alert{Severity: AlertSeverityWarn, Code: CodeOrphanReported, Path: contentPath,
		ContentID: &id, Message: "canonical exists with no active publication; migration not completed — reported, not quarantined"})
}

// classifyRestoreError maps a failed authoritative rebuild to P0s. Missing
// or corrupt immutable bytes mark the site degraded and never guess content.
func (s *Scanner) classifyRestoreError(st *scanState, active *Publication, path string, rerr error) {
	switch storage.CodeOf(rerr) {
	case storage.CodeNotFound:
		st.rpt.ImmutableMissingP0++
		st.rpt.Degraded = true
		s.emit(st, Alert{Severity: AlertSeverityP0, Code: CodeImmutableMissing, Path: path,
			ContentID: &active.ContentID, PublicationID: &active.ID,
			Message: "active publication has no immutable object; no replacement guessed, site degraded"})
	case storage.CodeHashMismatch:
		st.rpt.ImmutableCorruptP0++
		st.rpt.Degraded = true
		s.emit(st, Alert{Severity: AlertSeverityP0, Code: CodeImmutableCorrupt, Path: path,
			ContentID: &active.ContentID, PublicationID: &active.ID,
			Message: "immutable object failed verification; refusing rebuild, site degraded"})
	default:
		st.rpt.RepairErrors++
		s.emit(st, Alert{Severity: AlertSeverityWarn, Code: CodeRepairError, Path: path,
			ContentID: &active.ContentID, PublicationID: &active.ID,
			Message: "canonical rebuild failed: " + rerr.Error()})
	}
}

// cleanStaleNext deletes one .next sidecar past the stage timeout. Fresh
// in-flight cutovers are always left alone.
func (s *Scanner) cleanStaleNext(st *scanState, sc storage.Sidecar) {
	if !s.store.IsStale(sc.ModTime, st.now) {
		return
	}
	if err := os.Remove(sc.Path); err != nil && !os.IsNotExist(err) {
		st.rpt.RepairErrors++
		s.emit(st, Alert{Severity: AlertSeverityWarn, Code: CodeRepairError, Path: sc.Path,
			Message: "stale .next cleanup failed: " + err.Error()})
		return
	}
	st.rpt.StaleNextCleaned++
}

// reapStaleStaged marks staged records past the stage timeout failed and
// aborts their staged objects (§17.5). It runs under the page lock, so an
// in-flight saga (which always holds its lock) is never reaped.
func (s *Scanner) reapStaleStaged(ctx context.Context, st *scanState, contentID primitive.ObjectID) {
	cutoff := st.now.Add(-s.stageTimeout())
	cur, err := s.db.Collection(CollectionPublications).Find(ctx,
		bson.M{"content_id": contentID, "status": string(StatusStaged)},
		options.Find().SetProjection(bson.M{"_id": 1, "created_at": 1}))
	if err != nil {
		st.rpt.RepairErrors++
		s.emit(st, Alert{Severity: AlertSeverityWarn, Code: CodeRepairError,
			ContentID: &contentID, Message: "stale-stage lookup failed: " + err.Error()})
		return
	}
	var rows []struct {
		ID        primitive.ObjectID `bson:"_id"`
		CreatedAt time.Time          `bson:"created_at"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		_ = cur.Close(ctx)
		st.rpt.RepairErrors++
		return
	}
	_ = cur.Close(ctx)
	for _, r := range rows {
		if r.CreatedAt.After(cutoff) {
			continue
		}
		if err := s.repo.MarkFailed(ctx, r.ID,
			"stale stage: no activation within "+s.stageTimeout().String()+" (recovery scanner)"); err != nil {
			st.rpt.RepairErrors++
			s.emit(st, Alert{Severity: AlertSeverityWarn, Code: CodeRepairError,
				ContentID: &contentID, PublicationID: &r.ID,
				Message: "stale staged mark-failed failed: " + err.Error()})
			continue
		}
		obj := storage.StagedObject{ContentID: contentID, PublicationID: r.ID,
			Path: s.store.ImmutablePath(contentID, r.ID)}
		if err := s.store.Abort(ctx, obj); err != nil {
			st.rpt.RepairErrors++
			s.emit(st, Alert{Severity: AlertSeverityWarn, Code: CodeRepairError,
				ContentID: &contentID, PublicationID: &r.ID,
				Message: "stale staged abort failed: " + err.Error()})
			continue
		}
		_, _ = s.db.Collection(CollectionPublications).UpdateOne(ctx,
			bson.M{"_id": r.ID},
			bson.M{"$set": bson.M{
				"storage_state":      string(StorageDeleted),
				"storage_deleted_at": st.now,
			}})
		st.rpt.StaleStagedFailed++
		s.auditf(ctx, AuditStaleStagedFailed, map[string]any{
			"content_id": contentID.Hex(), "publication_id": r.ID.Hex()})
	}
}

// quarantineFile moves one absolute file path into the quarantine directory,
// preserving bytes outside the served tree. It never overwrites: the
// destination carries a nanotime suffix.
func (s *Scanner) quarantineFile(st *scanState, absPath, reason string) (QuarantineRecord, error) {
	qdir := s.quarantineDir()
	if qdir == "" {
		return QuarantineRecord{}, errNoQuarantine("no quarantine directory configured and store root unknown")
	}
	rel := filepath.Base(absPath)
	if st.genRoot != "" {
		if r, err := filepath.Rel(st.genRoot, absPath); err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			rel = r
		}
	}
	dest := filepath.Join(qdir, rel) + ".q-" + time.Now().Format("20060102T150405.000000000")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return QuarantineRecord{}, err
	}
	if err := os.Rename(absPath, dest); err != nil {
		return QuarantineRecord{}, err
	}
	_ = syncDir(filepath.Dir(dest))
	rec := QuarantineRecord{OriginalPath: absPath, QuarantinePath: dest, Reason: reason, At: st.now}
	st.rpt.Quarantined = append(st.rpt.Quarantined, rec)
	s.auditf(context.Background(), AuditQuarantinePreserved, map[string]any{
		"original": absPath, "quarantine": dest, "reason": reason})
	return rec, nil
}

// quarantineOrphanFamily takes a serving path offline: the canonical plus
// its previous/backup sidecars move to quarantine (never deleted — they may
// be the only copy of historically served bytes). Stale .next files are
// deleted; fresh ones are left for their owner.
func (s *Scanner) quarantineOrphanFamily(ctx context.Context, st *scanState, fullPath string, info storage.CanonicalInfo, _ *primitive.ObjectID) {
	previous, next, backups := sidecarsOf(info)
	moved := 0
	if info.Exists {
		if _, err := s.quarantineFile(st, info.Path, "orphan-canonical-no-active-publication"); err != nil {
			st.rpt.RepairErrors++
			s.emit(st, Alert{Severity: AlertSeverityP0, Code: CodeRepairError, Path: fullPath,
				Message: "orphan quarantine failed; file left serving: " + err.Error()})
			return
		}
		moved++
	}
	for _, sc := range append(append([]storage.Sidecar{}, previous...), backups...) {
		if _, err := os.Stat(sc.Path); err != nil {
			continue
		}
		if _, err := s.quarantineFile(st, sc.Path, "orphan-sidecar-no-active-publication"); err != nil {
			st.rpt.RepairErrors++
			s.emit(st, Alert{Severity: AlertSeverityWarn, Code: CodeRepairError, Path: fullPath,
				Message: "orphan sidecar quarantine failed: " + err.Error()})
			continue
		}
		moved++
	}
	for _, sc := range next {
		s.cleanStaleNext(st, sc)
	}
	if moved > 0 {
		st.rpt.OrphanQuarantined++
		s.emit(st, Alert{Severity: AlertSeverityWarn, Code: CodeOrphanQuarantined, Path: fullPath,
			Message: "canonical with no active publication quarantined (migration completed); no Publication inferred from files"})
		s.auditf(ctx, AuditOrphanQuarantined, map[string]any{"full_path": fullPath, "files_moved": moved})
	}
}

// generatedRoot derives the served canonical directory from the store so the
// orphan sweep walks exactly what static serving reads.
func (s *Scanner) generatedRoot() (string, error) {
	p, err := s.store.CanonicalFilePath("/")
	if err != nil {
		return "", err
	}
	return filepath.Dir(p), nil
}

// orphanFamily groups one canonical path with its sidecar files found on disk.
type orphanFamily struct {
	fullPath string
	canonAbs string
	hasCanon bool
	sidecars []orphanSidecar
}

type orphanSidecar struct {
	abs  string
	kind storage.SidecarKind
}

// splitSidecarName reverses the store's sidecar naming: {base}.previous-…,
// {base}.next-…, {base}.unpublish-backup-… with a 24-hex publication ID.
func splitSidecarName(name string) (base, kind string, ok bool) {
	for _, m := range []struct {
		suffix string
		kind   string
	}{
		{".html.previous-", string(storage.SidecarPrevious)},
		{".html.next-", string(storage.SidecarNext)},
		{".html.unpublish-backup-", string(storage.SidecarUnpublishBackup)},
	} {
		if i := strings.Index(name, m.suffix); i >= 0 {
			tail := name[i+len(m.suffix):]
			if len(tail) != 24 {
				return "", "", false
			}
			if _, err := primitive.ObjectIDFromHex(tail); err != nil {
				return "", "", false
			}
			return name[:i] + ".html", m.kind, true
		}
	}
	return "", "", false
}

// sweepOrphans walks the generated tree for canonical files no content or
// active record owns. Migration-completed quarantines them; otherwise they
// are reported. Families whose path lock is held by a publisher are skipped.
func (s *Scanner) sweepOrphans(ctx context.Context, st *scanState) {
	if st.genRoot == "" {
		return
	}
	qdir := s.quarantineDir()
	families := map[string]*orphanFamily{}
	_ = filepath.WalkDir(st.genRoot, func(abs string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // best effort; per-content passes already repaired owned paths
		}
		if d.IsDir() {
			if qdir != "" {
				if rel, rerr := filepath.Rel(st.genRoot, abs); rerr == nil && (rel == "." || !strings.HasPrefix(rel, "..")) {
					if qr, qerr := filepath.Rel(st.genRoot, qdir); qerr == nil && (rel == qr || strings.HasPrefix(qr, rel+string(filepath.Separator))) {
						if rel == qr {
							return filepath.SkipDir
						}
					}
				}
			}
			return nil
		}
		name := d.Name()
		dir := filepath.Dir(abs)
		if strings.HasSuffix(name, ".html") {
			rel, rerr := filepath.Rel(st.genRoot, abs)
			if rerr != nil {
				return nil
			}
			fp := "/" + strings.TrimSuffix(filepath.ToSlash(rel), ".html")
			if strings.HasSuffix(fp, "/index") && fp != "/index" {
				fp = strings.TrimSuffix(fp, "/index")
			}
			if fp == "/index" {
				fp = "/"
			}
			fam := families[abs]
			if fam == nil {
				fam = &orphanFamily{fullPath: fp, canonAbs: abs}
				families[abs] = fam
			}
			fam.hasCanon = true
			fam.fullPath = fp
			return nil
		}
		if base, kind, ok := splitSidecarName(name); ok {
			canonAbs := filepath.Join(dir, base)
			fam := families[canonAbs]
			if fam == nil {
				rel, rerr := filepath.Rel(st.genRoot, canonAbs)
				if rerr != nil {
					return nil
				}
				fp := "/" + strings.TrimSuffix(filepath.ToSlash(rel), ".html")
				if fp == "/index" {
					fp = "/"
				}
				fam = &orphanFamily{fullPath: fp, canonAbs: canonAbs}
				families[canonAbs] = fam
			}
			fam.sidecars = append(fam.sidecars, orphanSidecar{abs: abs, kind: storage.SidecarKind(kind)})
		}
		return nil
	})

	keys := make([]string, 0, len(families))
	for k := range families {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fam := families[k]
		if st.serving[fam.fullPath] {
			continue
		}
		release, ok := acquireSagaLocks(primitive.NilObjectID, lockPathsFor(fam.fullPath))
		if !ok {
			st.rpt.ContentsSkippedLocked++
			continue
		}
		func() {
			defer release()
			// Re-check under lock: the owner may have published concurrently.
			if st.serving[fam.fullPath] {
				return
			}
			if fam.hasCanon {
				if st.completed {
					info, err := s.store.Inspect(ctx, fam.fullPath)
					if err != nil {
						st.rpt.InspectErrors++
						s.emit(st, Alert{Severity: AlertSeverityP0, Code: CodeInspectFailed, Path: fam.fullPath,
							Message: "orphan inspect failed; left in place: " + err.Error()})
						return
					}
					s.quarantineOrphanFamily(ctx, st, fam.fullPath, info, nil)
					return
				}
				st.rpt.OrphanReported++
				s.emit(st, Alert{Severity: AlertSeverityWarn, Code: CodeOrphanReported, Path: fam.fullPath,
					Message: "canonical with no owning content/active; migration not completed — reported, not quarantined"})
				return
			}
			// Sidecar-only debris on an unowned path.
			if st.completed {
				moved := false
				for _, sc := range fam.sidecars {
					if sc.kind == storage.SidecarNext {
						if stt, serr := os.Stat(sc.abs); serr == nil && s.store.IsStale(stt.ModTime(), st.now) {
							if rerr := os.Remove(sc.abs); rerr == nil {
								st.rpt.StaleNextCleaned++
							}
						}
						continue
					}
					if _, qerr := s.quarantineFile(st, sc.abs, "orphan-sidecar-no-owner"); qerr == nil {
						moved = true
					}
				}
				if moved {
					st.rpt.OrphanQuarantined++
					s.emit(st, Alert{Severity: AlertSeverityWarn, Code: CodeOrphanQuarantined, Path: fam.fullPath,
						Message: "orphan sidecar debris quarantined (migration completed)"})
				}
				return
			}
			for _, sc := range fam.sidecars {
				if sc.kind != storage.SidecarNext {
					continue
				}
				if stt, serr := os.Stat(sc.abs); serr == nil && s.store.IsStale(stt.ModTime(), st.now) {
					if rerr := os.Remove(sc.abs); rerr == nil {
						st.rpt.StaleNextCleaned++
					}
				}
			}
		}()
	}
}

// renameFile moves a sidecar back to its canonical name and fsyncs the
// directory so crash-window restores are durable.
func renameFile(from, to string) error {
	if from == "" || to == "" {
		return errNoQuarantine("rename requires source and destination")
	}
	if _, err := os.Stat(to); err == nil {
		return errNoQuarantine("refusing to restore over an existing canonical: " + to)
	}
	if err := os.Rename(from, to); err != nil {
		return err
	}
	return syncDir(filepath.Dir(to))
}

// syncDir fsyncs a directory so renames inside it survive a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	serr := d.Sync()
	cerr := d.Close()
	if serr != nil {
		return serr
	}
	return cerr
}

// scannerError is a local typed error for refused operations.
type scannerError struct{ msg string }

func (e *scannerError) Error() string { return "recovery scanner: " + e.msg }

func errNoQuarantine(msg string) error { return &scannerError{msg: msg} }
