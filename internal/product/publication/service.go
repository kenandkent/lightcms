// Package publication — Task 8: publish/unpublish saga (integration owner).
//
// This file owns the ONLY live-mutation service in the V3 program
// (spec §16.6):
//
//	type PublicationService interface {
//	    Publish(ctx context.Context, req PublishRequest) (*PublicationResult, error)
//	    Unpublish(ctx context.Context, req UnpublishRequest) error
//	    Rollback(ctx context.Context, req RollbackRequest) (*PublicationResult, error)
//	}
//
// (Pointer-vs-value result is the single deliberate deviation from the §16.6
// sketch: Publish/Rollback return (PublicationResult, error) with a zero
// value on failure, matching the plan Shared interface contract verbatim.
// PublishRequest/PublicationResult/UnpublishRequest/RollbackRequest are the
// verbatim contract shapes declared in model.go — never redefined here.)
//
// Pipeline (spec §16.1): page lock → load content → resolve + freeze content
// version, immutable template version and logical time → resolve canonical
// public URL → CAS-persist the idempotency execution snapshot → render →
// security check → hash → InsertStaged → Store.Stage → Store.Verify → atomic
// file cutover → Mongo activation transaction → old-publication supersede +
// Content projection + outbox in ONE transaction → post-commit cleanup →
// async-safe side effects → result. CDN purge and webhook delivery are
// post-commit retryable effects and never roll back a committed publication.
//
// What this saga is NOT (owned elsewhere, imported — never redefined):
//   - records + CAS transactions: Repository (Task 5, repository.go);
//   - immutable bytes + atomic cutover: storage.Store (Task 6, store.go);
//   - attempt snapshot + lease: idempotency.Service (Task 11) — the saga
//     re-binds per attempt (BindContentAndVersion), freezes before render
//     (FreezeExecution), marks terminal pre-activation failures
//     (MarkTerminal), NEVER calls TakeOver (a terminal attempt returns
//     STALE_ATTEMPT; only Begin advances attempt++), and NEVER calls Complete
//     (response caching is the Task 12 HTTP caller's job, which owns status
//     codes and the validationOnly attestation);
//   - canonical keys: pathkey.Canonical (Task 2);
//   - template versions: templatecontract.Service (Task 3);
//   - public URLs: publicurl.Resolver (Task 13).
//
// Critical handoffs enforced here:
//   - ActivateCAS permits legacy_unverified for the migration path; this saga
//     REJECTS legacy_unverified for ordinary business publish/rollback.
//   - Activate over an existing canonical returns CodeNeedsPreviousID; the
//     saga branches on the FROZEN old active ID instead — plain Activate for
//     first publish, ActivateWithPrevious for republish/rollback.
//   - Task 5's outbox payload lacks PublicURL; the saga enriches it at
//     activation time (best effort; delivery never depends on it).
//
// Crash semantics (spec §15.7, §16.4, §17.4): the file cutover and the Mongo
// transaction are NOT one atomic transaction — the saga compensates (restore
// previous / remove uncommitted canonical) when the transaction returns an
// error. ErrStopAfterRename + Faults.BeforeCommit simulates a crash BETWEEN
// cutover and commit with NO compensation so Task 10's scanner tests can
// prove restart repair against preserved on-disk files.
package publication

import (
	"context"
	"errors"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/database"
	"github.com/jonradoff/lightcms/v7/internal/product/idempotency"
	"github.com/jonradoff/lightcms/v7/internal/product/pathkey"
	"github.com/jonradoff/lightcms/v7/internal/product/publicurl"
	"github.com/jonradoff/lightcms/v7/internal/product/storage"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Saga error codes. Spec §27 codes are reused verbatim where they exist;
// the redirect code is saga-local and documented for the Task 12 mapper.
const (
	// CodePagePublishInProgress maps contended saga locks to 409 PAGE_PUBLISH_IN_PROGRESS.
	CodePagePublishInProgress = "PAGE_PUBLISH_IN_PROGRESS"
	// CodeStageFailed maps Store.Stage failures to PUBLICATION_STAGE_FAILED.
	CodeStageFailed = "PUBLICATION_STAGE_FAILED"
	// CodeVerifyFailed maps Store.Verify failures to PUBLICATION_VERIFY_FAILED.
	CodeVerifyFailed = "PUBLICATION_VERIFY_FAILED"
	// CodeActivateFailed maps cutover/commit failures to PUBLICATION_ACTIVATE_FAILED.
	CodeActivateFailed = "PUBLICATION_ACTIVATE_FAILED"
	// CodeUnpublishStageFailed maps backup-rename failures to PUBLICATION_UNPUBLISH_STAGE_FAILED.
	CodeUnpublishStageFailed = "PUBLICATION_UNPUBLISH_STAGE_FAILED"
	// CodeRenderFailed maps renderer failures to CONTENT_PUBLISH_FAILED.
	CodeRenderFailed = "CONTENT_PUBLISH_FAILED"
	// CodeValidationFailed maps strict field rejection to FIELD_VALIDATION_FAILED.
	CodeValidationFailed = "FIELD_VALIDATION_FAILED"
	// CodePathInvalid maps bad paths to PATH_INVALID.
	CodePathInvalid = "PATH_INVALID"
	// CodePathConflict maps canonical collisions to PATH_CONFLICT.
	CodePathConflict = "PATH_CONFLICT"
	// CodeContentVersionConflict maps stale version pins to CONTENT_VERSION_CONFLICT.
	CodeContentVersionConflict = "CONTENT_VERSION_CONFLICT"
	// CodeTemplateNotActive maps draft/deprecated template publish to TEMPLATE_NOT_ACTIVE.
	CodeTemplateNotActive = "TEMPLATE_NOT_ACTIVE"
	// CodePublicURLFailed maps resolver failures to PUBLIC_URL_RESOLUTION_FAILED.
	CodePublicURLFailed = "PUBLIC_URL_RESOLUTION_FAILED"
	// CodeRedirectFailed is saga-local: the activation committed but the
	// rename redirect row did not. The page is live at the new path; the
	// redirect must be retried and the old canonical is retained (never
	// deleted) until the redirect exists.
	CodeRedirectFailed = "REDIRECT_CREATE_FAILED"
	// CodeCrashStop is the synthetic code on the crash-fixture error
	// (errors.Is(err, ErrStopAfterRename)): no compensation ran and the
	// on-disk files are preserved for Task 10 restart repair.
	CodeCrashStop = "CRASH_STOP_AFTER_RENAME"
	// CodeActivationUnknown marks an indeterminate commit (R11): the
	// activation transaction errored AND the read-back failed too, so the
	// commit may or may not have landed. Nothing is compensated and the
	// staged record is kept — the scanner or a same-key retry converges.
	// Callers must retry the same key, never mint a new operation.
	CodeActivationUnknown = "ACTIVATION_UNKNOWN"
	// CodeInternal marks unexpected saga failures (never leaks driver detail).
	CodeInternal = "INTERNAL_ERROR"
)

// sagaErr builds a saga *Error (the shared publication.Error type —
// CodeOf/IsConflict in model.go keep working for saga errors).
func sagaErr(code, message string, err error) *Error {
	return &Error{Code: code, Message: message, Err: err}
}

// ErrStopAfterRename is returned by a Faults.BeforeCommit hook to simulate a
// process crash between canonical rename and Mongo commit: the saga returns
// WITHOUT compensation and WITHOUT marking the attempt failed, preserving
// the new canonical + .previous-{oldID} files for the Task 10 scanner.
// Tests assert errors.Is(err, ErrStopAfterRename).
var ErrStopAfterRename = errors.New("publication saga: stop after canonical rename before Mongo commit (crash fixture)")

// Faults carries test-only injection points. Production constructors leave
// it zero: no hook runs. BeforeCommit runs after a successful file cutover
// and before the Mongo activation transaction.
type Faults struct {
	BeforeCommit func(ctx context.Context) error
	// CommitReadError, when non-nil, replaces the post-commit read-back
	// error (R11 unknown-path test seam): combined with a failing commit
	// it deterministically exercises the indeterminate-commit branch.
	CommitReadError error
}

// Options wires the saga. DB, Repo and Store are required; everything else
// has a safe default (nil Templates builds from DB, nil Renderer selects the
// deterministic in-process renderer, nil Idem/URLs/Purge/Audit disables that
// integration — Task 16 wires the full set).
type Options struct {
	Templates *templatecontract.Service
	Idem      *idempotency.Service
	URLs      *publicurl.Resolver
	Renderer  Renderer
	// SnapshotRender, when non-nil, replaces Renderer for plan builds with
	// the frozen snapshot pipeline (PlanSnapshot + RenderDetailed: Markdown,
	// sanitizer policy, snippets, wikilinks, TOC). R02: production MUST set
	// this — the minimal DefaultRenderer marks every string trusted HTML
	// with no sanitization. Tests keep passing explicit Renderer fakes.
	SnapshotRender SnapshotRenderFunc
	Purge          func(ctx context.Context, urls []string) error
	Audit          func(ctx context.Context, action string, fields map[string]any)
	Faults         Faults
	Now            func() time.Time

	RendererVersion string
	BuildSHA        string
}

// Service is the only live-mutation service (spec §16.6). Single-instance
// locking is in-process (per-content + per-path try-locks, stable order);
// multi-instance Mongo lease locks arrive with the scale-out Storage ADR.
type Service struct {
	db        *database.DB
	repo      *Repository
	store     storage.Store
	templates *templatecontract.Service
	idem      *idempotency.Service
	urls      *publicurl.Resolver
	renderer  Renderer
	snapshot  SnapshotRenderFunc
	purge     func(ctx context.Context, urls []string) error
	audit     func(ctx context.Context, action string, fields map[string]any)
	faults    Faults
	now       func() time.Time

	rendererVersion string
	buildSHA        string
}

// NewService builds the saga. A nil outbox inside repo is the repository's
// own default; a nil store is rejected by construction contract (callers must
// pass the Task 6 filesystem store).
func NewService(db *database.DB, repo *Repository, store storage.Store, opts Options) *Service {
	templates := opts.Templates
	if templates == nil && db != nil {
		templates = templatecontract.NewService(db)
	}
	render := opts.Renderer
	if render == nil {
		render = DefaultRenderer
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	rendererVersion := opts.RendererVersion
	if rendererVersion == "" {
		rendererVersion = "lightcms-renderer-v1"
	}
	buildSHA := opts.BuildSHA
	if buildSHA == "" {
		buildSHA = "dev"
	}
	return &Service{
		db: db, repo: repo, store: store,
		templates: templates, idem: opts.Idem, urls: opts.URLs,
		renderer: render, snapshot: opts.SnapshotRender, purge: opts.Purge, audit: opts.Audit,
		faults: opts.Faults, now: now,
		rendererVersion: rendererVersion, buildSHA: buildSHA,
	}
}

// Publish runs the §16 atomic publish pipeline and returns the result only
// after the activation transaction commits. Zero-value request versions
// resolve under the page lock (content → CurrentVersion, template → current
// immutable version) and are frozen for this attempt.
func (s *Service) Publish(ctx context.Context, req PublishRequest) (PublicationResult, error) {
	if req.ContentID.IsZero() {
		return PublicationResult{}, sagaErr(CodeValidationFailed, "content_id is required", nil)
	}
	ctx, stop, err := s.ownExecution(ctx, req.IdempotencyRecord)
	if err != nil {
		return PublicationResult{}, err
	}
	defer stop()
	if result, ok, err := s.committedExecution(ctx, req.IdempotencyRecord, req.ContentID); err != nil || ok {
		return result, err
	}
	content, err := s.loadContent(ctx, req.ContentID)
	if err != nil {
		return PublicationResult{}, err
	}
	canonical, err := pathkey.Canonical(content.FullPath)
	if err != nil {
		return PublicationResult{}, sagaErr(CodePathInvalid, "invalid content path "+content.FullPath, err)
	}
	oldActive, err := s.repo.GetActive(ctx, req.ContentID)
	if err != nil {
		return PublicationResult{}, sagaErr(CodeInternal, "read active publication", err)
	}

	release, ok := acquireSagaLocks(req.ContentID, lockPathsFor(content.FullPath, activePath(oldActive)))
	if !ok {
		return PublicationResult{}, sagaErr(CodePagePublishInProgress, "another publish holds this page", nil)
	}
	defer release()

	plan, err := s.buildPublishPlan(ctx, req, content, canonical)
	if err != nil {
		return PublicationResult{}, err
	}
	return s.executeCutoverPlan(ctx, plan)
}

// Unpublish runs the §18.1 flow: stage the canonical away to
// .unpublish-backup-{publicationID}, flip active → unpublished + Content
// projection + outbox in ONE transaction, then clean the backup. A page with
// no active publication returns success with no new event (naturally
// idempotent second call).
func (s *Service) Unpublish(ctx context.Context, req UnpublishRequest) error {
	if req.ContentID.IsZero() {
		return sagaErr(CodeValidationFailed, "content_id is required", nil)
	}
	content, err := s.loadContent(ctx, req.ContentID)
	if err != nil {
		return err
	}
	if _, err := pathkey.Canonical(content.FullPath); err != nil {
		return sagaErr(CodePathInvalid, "invalid content path "+content.FullPath, err)
	}
	preActive, err := s.repo.GetActive(ctx, req.ContentID)
	if err != nil {
		return sagaErr(CodeInternal, "read active publication", err)
	}

	release, ok := acquireSagaLocks(req.ContentID, lockPathsFor(content.FullPath, activePath(preActive)))
	if !ok {
		return sagaErr(CodePagePublishInProgress, "another publish holds this page", nil)
	}
	defer release()

	if err := s.dbReloadContent(ctx, &content); err != nil {
		return err
	}
	active, err := s.repo.GetActive(ctx, req.ContentID)
	if err != nil {
		return sagaErr(CodeInternal, "read active publication", err)
	}
	if active == nil {
		return nil // idempotent: already unpublished, no new event.
	}
	if req.ExpectedActiveID != nil && active.ID != *req.ExpectedActiveID {
		return sagaErr(CodeConflict, "stale expected active publication", nil)
	}

	targetPath := active.FullPath
	if targetPath == "" {
		targetPath = content.FullPath
	}
	backedUp := true
	if _, err := s.store.StageUnpublishBackup(ctx, targetPath, active.ID); err != nil {
		if storage.CodeOf(err) == storage.CodeNotFound {
			backedUp = false // canonical already absent: goal state holds; converge the control plane.
		} else {
			return sagaErr(CodeUnpublishStageFailed, "stage canonical away for unpublish", err)
		}
	}

	did, err := s.repo.UnpublishCAS(ctx, req.ContentID, req.ExpectedActiveID,
		Attribution{Actor: req.Actor, Via: req.Via, AgentSession: req.AgentSession})
	if err != nil {
		// R11 symmetric with publish: determine whether the transaction
		// took effect before touching files. Still-active ⇒ the txn did
		// not commit and restoring the backup is safe. Read failure ⇒
		// UNKNOWN: never resurrect (the txn may have committed) — leave
		// the backup for the scanner and report retryable unknown.
		// No active record ⇒ the unpublish took effect despite the error
		// (or a concurrent one did): converge by dropping the backup.
		cur, rerr := s.repo.GetActive(ctx, req.ContentID)
		if rerr != nil {
			s.auditf(ctx, "publication.unpublish_unknown", map[string]any{
				"content_id": req.ContentID.Hex(), "full_path": targetPath,
			})
			return sagaErr(CodeActivationUnknown,
				"unpublish transaction result unknown; backup retained for recovery — retry", err)
		}
		if cur == nil {
			_ = s.store.RemoveUnpublishBackup(ctx, targetPath, active.ID)
			s.bestEffortPurge(ctx, []string{targetPath})
			s.auditf(ctx, "publication.unpublish", map[string]any{
				"content_id": content.ID.Hex(), "publication_id": active.ID.Hex(),
				"full_path": targetPath, "converged_after_error": true,
			})
			return nil
		}
		if backedUp {
			if rerr := s.store.RestoreUnpublishBackup(ctx, targetPath, active.ID); rerr != nil {
				return sagaErr(CodeUnpublishStageFailed,
					"unpublish transaction failed AND backup restore failed (P0: scanner repair required)", errors.Join(err, rerr))
			}
		}
		return sagaErr(CodeUnpublishStageFailed, "unpublish transaction failed; live page restored", err)
	}
	if !did {
		// Lost a race with a concurrent unpublish: converge the file state.
		if backedUp {
			_ = s.store.RestoreUnpublishBackup(ctx, targetPath, active.ID)
		}
		return nil
	}

	// Post-commit: backup cleanup, CDN purge and audit are retryable effects
	// that never resurrect the unpublished state.
	_ = s.store.RemoveUnpublishBackup(ctx, targetPath, active.ID)
	s.bestEffortPurge(ctx, []string{targetPath})
	s.auditf(ctx, "publication.unpublish", map[string]any{
		"content_id": content.ID.Hex(), "publication_id": active.ID.Hex(), "full_path": targetPath,
	})
	return nil
}

// Rollback mints a NEW publication from a historical source: exact retained
// immutable bytes when present, otherwise the explicitly weaker re-render
// from the retained content-version + template-version snapshots (never
// byte-identical by promise). Every rollback mints a new record even when the
// source is current (spec §15.6: same hash still keeps a new audit record).
func (s *Service) Rollback(ctx context.Context, req RollbackRequest) (PublicationResult, error) {
	if req.ContentID.IsZero() || req.SourcePublicationID.IsZero() {
		return PublicationResult{}, sagaErr(CodeValidationFailed, "content_id and source_publication_id are required", nil)
	}
	ctx, stop, err := s.ownExecution(ctx, req.IdempotencyRecord)
	if err != nil {
		return PublicationResult{}, err
	}
	defer stop()
	if result, ok, err := s.committedExecution(ctx, req.IdempotencyRecord, req.ContentID); err != nil || ok {
		return result, err
	}
	content, err := s.loadContent(ctx, req.ContentID)
	if err != nil {
		return PublicationResult{}, err
	}
	canonical, err := pathkey.Canonical(content.FullPath)
	if err != nil {
		return PublicationResult{}, sagaErr(CodePathInvalid, "invalid content path "+content.FullPath, err)
	}
	source, err := s.repo.GetByID(ctx, req.SourcePublicationID)
	if err != nil {
		return PublicationResult{}, err
	}
	if source.ContentID != req.ContentID {
		return PublicationResult{}, sagaErr(CodeValidationFailed, "source publication does not belong to this content", nil)
	}
	if source.Status == StatusFailed {
		return PublicationResult{}, sagaErr(CodeValidationFailed, "cannot roll back to a failed publication", nil)
	}
	oldActive, err := s.repo.GetActive(ctx, req.ContentID)
	if err != nil {
		return PublicationResult{}, sagaErr(CodeInternal, "read active publication", err)
	}

	release, ok := acquireSagaLocks(req.ContentID, lockPathsFor(content.FullPath, activePath(oldActive)))
	if !ok {
		return PublicationResult{}, sagaErr(CodePagePublishInProgress, "another publish holds this page", nil)
	}
	defer release()

	plan, err := s.buildRollbackPlan(ctx, req, content, canonical, source)
	if err != nil {
		return PublicationResult{}, err
	}
	return s.executeCutoverPlan(ctx, plan)
}

// bestEffortPurge runs the injected CDN purge without ever failing the saga:
// purge is post-commit and retryable by definition.
func (s *Service) bestEffortPurge(ctx context.Context, urls []string) {
	if s.purge == nil {
		return
	}
	if err := s.purge(ctx, urls); err != nil {
		s.auditf(ctx, "publication.purge_retry", map[string]any{"urls": urls, "error": err.Error()})
	}
}

// auditf emits a structured saga audit event when Task 16 wires a sink.
func (s *Service) auditf(ctx context.Context, action string, fields map[string]any) {
	if s.audit == nil {
		return
	}
	s.audit(ctx, action, fields)
}

// idemAttempt loads the bound operation for saga idempotency bookkeeping. It
// returns ok=false when the saga runs without an idempotency record.
func (s *Service) idemAttempt(ctx context.Context, opID *primitive.ObjectID) (op idempotency.Operation, attempt int64, ok bool, err error) {
	if s.idem == nil || opID == nil {
		return idempotency.Operation{}, 0, false, nil
	}
	loaded, gerr := s.idem.Get(ctx, *opID)
	if gerr != nil {
		return idempotency.Operation{}, 0, false, sagaErr(string(idempotency.CodeNotFound), "idempotency record not found", gerr)
	}
	if loaded.State == idempotency.StateCompleted {
		return idempotency.Operation{}, 0, false, sagaErr(string(idempotency.CodeAlreadyCompleted),
			"operation already completed; replay the cached response instead of republishing", nil)
	}
	if loaded.AttemptState == idempotency.AttemptTerminal {
		// NEVER TakeOver a terminal attempt: only Begin advances attempt++.
		return idempotency.Operation{}, 0, false, sagaErr(string(idempotency.CodeStaleAttempt),
			"attempt is terminal; Begin a retry for a new attempt", nil)
	}
	gen := loaded.LeaseGeneration
	if owned, ok := idempotency.LeaseGenerationFrom(ctx); ok {
		gen = owned
	}
	if err := s.idem.AssertOwned(ctx, loaded.ID, loaded.Attempt, gen); err != nil {
		return op, 0, false, err
	}
	return loaded, loaded.Attempt, true, nil
}

func (s *Service) ownExecution(ctx context.Context, opID *primitive.ObjectID) (context.Context, context.CancelFunc, error) {
	if opID == nil || s.idem == nil {
		return ctx, func() {}, nil
	}
	op, _, _, err := s.idemAttempt(ctx, opID)
	if err != nil {
		return ctx, func() {}, err
	}
	if _, err = s.idem.RenewLease(ctx, op.ID, op.Attempt, op.LeaseGeneration); err != nil {
		return ctx, func() {}, err
	}
	ctx = idempotency.WithExecutionLease(ctx, op.ID, op.Attempt, op.LeaseGeneration)
	hctx, stop := s.idem.Heartbeat(ctx, op.ID, op.Attempt, op.LeaseGeneration)
	bounded, cancel := context.WithTimeout(hctx, s.idem.Lease()/2)
	bounded = storage.WithWriteGuard(bounded, func() error { return s.idem.AssertOwned(bounded, op.ID, op.Attempt, op.LeaseGeneration) })
	return bounded, func() { cancel(); stop() }, nil
}

func (s *Service) committedExecution(ctx context.Context, opID *primitive.ObjectID, contentID primitive.ObjectID) (PublicationResult, bool, error) {
	if opID == nil || s.idem == nil {
		return PublicationResult{}, false, nil
	}
	op, err := s.idem.Get(ctx, *opID)
	if err != nil {
		return PublicationResult{}, false, err
	}
	if op.PublicationID == nil {
		return PublicationResult{}, false, nil
	}
	p, err := s.repo.GetByID(ctx, *op.PublicationID)
	if CodeOf(err) == CodeNotFound {
		return PublicationResult{}, false, nil
	}
	if err != nil {
		return PublicationResult{}, false, err
	}
	if p.ActivatedAt == nil {
		return PublicationResult{}, false, nil
	}
	if p.ContentID != contentID {
		return PublicationResult{}, false, sagaErr(CodeConflict, "bound publication belongs to different content", nil)
	}
	return PublicationResult{PublicationID: p.ID, ContentID: p.ContentID, ContentVersion: p.ContentVersion, TemplateVersionID: p.TemplateVersionID, FullPath: p.FullPath, PublicURL: p.PublicURL, ContentHash: p.ContentHash, LogicalPublishedAt: p.LogicalPublishedAt}, true, nil
}
