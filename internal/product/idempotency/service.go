package idempotency

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/database"
	"github.com/jonradoff/lightcms/v7/internal/observe"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// Options configures TTL and lease durations. Zero values select defaults;
// TTLHours outside 1–72 is rejected (spec §21.4).
type Options struct {
	// TTLHours is the idempotency record retention (default DefaultTTLHours).
	TTLHours int
	// LeaseMinutes is the processing lease (default DefaultLeaseMinutes).
	LeaseMinutes int
}

// Service implements the idempotency policy (spec §21, ADR-005) over a
// Repository. It opens no transactions itself; BindContentAndVersion accepts
// the caller's session so content writes and binding commit atomically.
type Service struct {
	repo  *Repository
	ttl   time.Duration
	lease time.Duration
}

// NewService validates options and binds the collection. Indexes remain
// owned by Task 2 (DB.EnsureProductIndexes).
func NewService(db *database.DB, opts Options) (*Service, error) {
	ttlHours := opts.TTLHours
	if ttlHours == 0 {
		ttlHours = DefaultTTLHours
	}
	if ttlHours < MinTTLHours || ttlHours > MaxTTLHours {
		return nil, idemErr(CodeInvalidRequest, "TTL hours must be 1-72")
	}
	leaseMin := opts.LeaseMinutes
	if leaseMin == 0 {
		leaseMin = DefaultLeaseMinutes
	}
	if leaseMin < 1 {
		return nil, idemErr(CodeInvalidRequest, "lease minutes must be >= 1")
	}
	return &Service{
		repo:  NewRepository(db),
		ttl:   time.Duration(ttlHours) * time.Hour,
		lease: time.Duration(leaseMin) * time.Minute,
	}, nil
}

// TTL returns the configured record retention. Lease returns the processing
// lease duration (Task 8/16 workers derive timeouts from it; every step must
// fit inside the remaining lease per spec §21.5).
func (s *Service) TTL() time.Duration   { return s.ttl }
func (s *Service) Lease() time.Duration { return s.lease }

// Begin starts or resumes the operation for (owner, method, path, key,
// canonicalBody):
//   - no record → inserts attempt 1 with a fresh lease; caller owns it.
//   - same key + changed body → 409 IDEMPOTENCY_CONFLICT (no side effects).
//   - completed + same body → (op with Replay=true, nil): return the cached
//     StatusCode/Response verbatim.
//   - processing + live lease + same body → 409 REQUEST_IN_PROGRESS.
//   - processing + expired lease + same body → IDEMPOTENCY_LEASE_EXPIRED:
//     the caller may TakeOver the same attempt.
//   - terminal attempt + same body → CAS attempt++ with a fresh lease and a
//     cleared execution snapshot (content binding is kept for recovery);
//     the caller allocates a NEW Publication ID and freezes it.
//
// Concurrent first-time beginners collide on the unique index; exactly one
// insert wins and the losers resolve through the same existing-record path.
func (s *Service) Begin(ctx context.Context, owner, method, path, key string, canonicalBody []byte) (Operation, error) {
	owner = strings.TrimSpace(owner)
	method = strings.ToUpper(strings.TrimSpace(method))
	path = strings.TrimSpace(path)
	if owner == "" || method == "" || path == "" || strings.TrimSpace(key) == "" {
		return Operation{}, idemErr(CodeInvalidRequest, "owner, method, path and key are all required (missing key maps to HTTP 428 upstream)")
	}
	hash := CanonicalHash(canonicalBody)
	now := time.Now().UTC()

	id := primitive.NewObjectID()
	fresh := &Operation{
		ID:              id,
		OperationIDHex:  id.Hex(),
		Owner:           owner,
		Method:          method,
		Path:            path,
		Key:             key,
		RequestHash:     hash,
		State:           StateProcessing,
		Attempt:         1,
		AttemptState:    AttemptProcessing,
		LeaseGeneration: 1,
		LeaseExpiresAt:  now.Add(s.lease),
		CreatedAt:       now,
		UpdatedAt:       now,
		ExpiresAt:       now.Add(s.ttl),
	}
	// Insert race loop: concurrent first-time beginners collide on the
	// unique index and exactly one insert wins. A duplicate whose record
	// then reads back missing fell into the TTL gap (expiry between our
	// insert conflict and re-read) — retry the insert rather than handing
	// the caller an unpersisted operation ID whose Bind/Freeze/Complete
	// would all dead-end on NotFound.
	for attempt := 0; ; attempt++ {
		if err := s.repo.Insert(ctx, fresh); err != nil {
			if !mongo.IsDuplicateKeyError(err) && !isDupKey(err) {
				return Operation{}, err
			}
			// Lost the insert race: resolve as an existing record.
		} else {
			return *fresh, nil
		}

		existing, err := s.repo.FindByKey(ctx, owner, method, path, key)
		if err != nil {
			return Operation{}, err
		}
		if existing == nil {
			// Index reports a duplicate but the document is gone (TTL expiry
			// between insert and re-read): safe to treat as a new operation.
			if attempt >= 2 {
				return Operation{}, idemErr(CodeNotFound, "idempotency record vanished repeatedly; retry the request")
			}
			fresh.ID = primitive.NewObjectID()
			fresh.OperationIDHex = fresh.ID.Hex()
			continue
		}
		return s.resolveExisting(ctx, existing, hash, now)
	}
}

// resolveExisting applies the replay/conflict/lease/terminal matrix.
func (s *Service) resolveExisting(ctx context.Context, existing *Operation, hash string, now time.Time) (Operation, error) {
	if existing.RequestHash != hash {
		// Task 16F: same key + changed body.
		observe.Default().IncIdemConflict()
		return Operation{}, idemErr(CodeConflict, "same Idempotency-Key with a different request body: use a new key for a new operation")
	}
	if existing.State == StateCompleted {
		// Task 16F: cached-response replay (no duplicate side effect).
		observe.Default().IncIdemReplay()
		hit := *existing
		hit.Replay = true
		return hit, nil
	}
	if existing.AttemptState == AttemptTerminal {
		return s.advanceAttempt(ctx, existing, now)
	}
	if existing.LeaseExpiresAt.After(now) {
		// Task 16F: parallel worker holds the lease.
		observe.Default().IncIdemConflict()
		return Operation{}, idemErr(CodeInProgress, "operation is already being executed; retry after the lease lapses")
	}
	return Operation{}, idemErr(CodeLeaseExpired, "worker lease expired; take over the same attempt before proceeding")
}

// advanceAttempt CAS-moves a terminal attempt to attempt+1 with a fresh
// lease. The execution snapshot (publication/logical time/template) is
// cleared; the content binding is kept so the retry can recover — rather
// than recreate — already-committed Content. Per-attempt publication links
// stay in Attempts history.
func (s *Service) advanceAttempt(ctx context.Context, existing *Operation, now time.Time) (Operation, error) {
	matched, err := s.repo.UpdateCAS(ctx,
		bson.M{"_id": existing.ID, "attempt": existing.Attempt, "attempt_state": AttemptTerminal, "state": StateProcessing},
		bson.M{
			"$inc": bson.M{"attempt": 1, "lease_generation": 1},
			"$set": bson.M{
				"attempt_state":         AttemptProcessing,
				"processing_expires_at": now.Add(s.lease),
				"publication_id":        nil,
				"logical_published_at":  nil,
				"template_version_id":   nil,
				"last_error_code":       "",
				"updated_at":            now,
				"expires_at":            now.Add(s.ttl),
			},
		})
	if err != nil {
		return Operation{}, err
	}
	if matched == 0 {
		return Operation{}, idemErr(CodeConflict, "operation changed concurrently; retry Begin")
	}
	next, err := s.repo.FindByID(ctx, existing.ID)
	if err != nil {
		return Operation{}, err
	}
	if next == nil {
		return Operation{}, idemErr(CodeNotFound, "operation vanished (TTL expiry); retry as a new operation")
	}
	return *next, nil
}

// TakeOverByKey finds the operation for (owner, method, path, key) and
// takes over its expired lease (Task 16D: crashed-worker resume for
// background jobs with stable operation keys). It returns the live record:
// a concurrently completed operation reports StateCompleted (replay, do not
// republish). A live lease or a missing record is an error — retry later.
func (s *Service) TakeOverByKey(ctx context.Context, owner, method, path, key string) (Operation, error) {
	existing, err := s.repo.FindByKey(ctx,
		strings.TrimSpace(owner), strings.ToUpper(strings.TrimSpace(method)),
		strings.TrimSpace(path), strings.TrimSpace(key))
	if err != nil {
		return Operation{}, err
	}
	if existing == nil {
		return Operation{}, idemErr(CodeNotFound, "no such operation")
	}
	if existing.State == StateCompleted {
		hit := *existing
		hit.Replay = true
		return hit, nil
	}
	return s.TakeOver(ctx, existing.ID, existing.LeaseGeneration)
}

// Get loads an operation by ID for takeover/resume reads.
func (s *Service) Get(ctx context.Context, opID primitive.ObjectID) (Operation, error) {
	op, err := s.repo.FindByID(ctx, opID)
	if err != nil {
		return Operation{}, err
	}
	if op == nil {
		return Operation{}, idemErr(CodeNotFound, "no such operation")
	}
	return *op, nil
}

// leaseGenerationContextKey carries the caller's owned lease generation
// for fencing (R07): mutating methods CAS on it when present, so a worker
// that lost its lease to a takeover cannot write onto the new owner's
// attempt. Callers stash it immediately after Begin/TakeOver (which return
// the owned generation) and re-stash after every attempt-advancing call.
// Absent = unchecked (backward compatible for unwired callers and tests).
type leaseGenerationContextKey struct{}

// WithLeaseGeneration returns a copy of ctx carrying the owned lease
// generation for fencing idempotency mutations.
func WithLeaseGeneration(ctx context.Context, generation int64) context.Context {
	return context.WithValue(ctx, leaseGenerationContextKey{}, generation)
}

// leaseGenerationFrom extracts the fenced generation, if stashed.
func leaseGenerationFrom(ctx context.Context) (int64, bool) {
	v, ok := ctx.Value(leaseGenerationContextKey{}).(int64)
	return v, ok
}

// fenceFilter augments a CAS filter with the stashed lease generation.
// Callers translate a zero match through the existing mismatch diagnosers,
// which report the attempt as superseded.
func fenceFilter(ctx context.Context, filter bson.M) bson.M {
	if gen, ok := leaseGenerationFrom(ctx); ok {
		filter["lease_generation"] = gen
	}
	return filter
}

// fencedOut reports CodeLeaseLost when the caller stashed a generation and
// the record moved past it (a newer worker took over). Nil means fencing is
// inactive or the record agrees — fall through to the existing diagnosers.
func (s *Service) fencedOut(ctx context.Context, opID primitive.ObjectID) error {
	gen, ok := leaseGenerationFrom(ctx)
	if !ok {
		return nil
	}
	cur, err := s.repo.FindByID(ctx, opID)
	if err != nil || cur == nil {
		return nil
	}
	if cur.LeaseGeneration != gen {
		return idemErr(CodeLeaseLost, "lease generation changed; a newer worker owns the attempt")
	}
	return nil
}

// BindContentAndVersion couples the operation to its Content/Version inside
// the caller's Mongo transaction: pass the session (which may be nil for
// non-transactional use, in which case ctx is used). No committed Content
// is left without a recoverable operation ID, and an aborted transaction
// leaves neither the content write nor the binding behind.
//
// Re-binding identical values is idempotent (crash-retry safe); binding a
// different content/version conflicts.
func (s *Service) BindContentAndVersion(ctx context.Context, sess mongo.SessionContext, opID, contentID primitive.ObjectID, version int64, canonicalPath string) error {
	if opID.IsZero() || contentID.IsZero() {
		return idemErr(CodeInvalidRequest, "operation ID and content ID are required")
	}
	if version < 1 {
		return idemErr(CodeInvalidRequest, "content version must be >= 1")
	}
	if strings.TrimSpace(canonicalPath) == "" {
		return idemErr(CodeInvalidRequest, "canonical path is required")
	}
	wctx := ctx
	if sess != nil {
		wctx = sess
	}
	matched, err := s.repo.UpdateCAS(wctx,
		fenceFilter(ctx, bson.M{
			"_id": opID, "state": StateProcessing, "attempt_state": AttemptProcessing,
			"$or": []bson.M{
				{"content_id": bson.M{"$exists": false}},
				{"content_id": nil},
				{"content_id": contentID},
			},
		}),
		bson.M{"$set": bson.M{
			"content_id":          contentID,
			"content_version":     version,
			"canonical_full_path": canonicalPath,
			"updated_at":          time.Now().UTC(),
		}})
	if err != nil {
		return err
	}
	if matched == 1 {
		return nil
	}
	if ferr := s.fencedOut(ctx, opID); ferr != nil {
		return ferr
	}
	current, rerr := s.repo.FindByID(wctx, opID)
	if rerr != nil {
		return rerr
	}
	if current == nil {
		return idemErr(CodeNotFound, "no such operation")
	}
	if current.State == StateCompleted {
		return idemErr(CodeAlreadyCompleted, "operation already completed")
	}
	if current.AttemptState != AttemptProcessing {
		return idemErr(CodeStaleAttempt, "operation attempt is not active")
	}
	if current.ContentID != nil && *current.ContentID != contentID {
		return idemErr(CodeConflict, "operation already bound to different content")
	}
	return idemErr(CodeConflict, "binding changed concurrently; re-read and retry")
}

// FreezeExecution CAS-persists the execution snapshot before Render
// (spec §16.1/§21.5): operation ID, Publication ID, logical publish time,
// target content/version (via prior Bind), template version, canonical path.
// Persist failure must block Render — the caller must not proceed on error.
//
// Re-freezing identical values is idempotent; a divergent Publication ID
// for the same attempt conflicts (takeover must reuse, never reallocate);
// a stale attempt number is rejected.
func (s *Service) FreezeExecution(ctx context.Context, opID primitive.ObjectID, attempt int64, publicationID primitive.ObjectID, logicalAt time.Time, templateVersionID primitive.ObjectID) error {
	if opID.IsZero() || publicationID.IsZero() || templateVersionID.IsZero() {
		return idemErr(CodeInvalidRequest, "operation, publication and template version IDs are required")
	}
	if logicalAt.IsZero() {
		return idemErr(CodeInvalidRequest, "logical publish time is required")
	}
	now := time.Now().UTC()
	matched, err := s.repo.UpdateCAS(ctx,
		fenceFilter(ctx, bson.M{
			"_id": opID, "attempt": attempt, "state": StateProcessing, "attempt_state": AttemptProcessing,
			"$or": []bson.M{
				{"publication_id": bson.M{"$exists": false}},
				{"publication_id": nil},
				{"publication_id": publicationID},
			},
		}),
		bson.M{
			"$set": bson.M{
				"publication_id":       publicationID,
				"logical_published_at": logicalAt.UTC(),
				"template_version_id":  templateVersionID,
				"updated_at":           now,
			},
			"$addToSet": bson.M{
				"attempts": bson.M{
					"attempt": attempt, "publication_id": publicationID,
					"logical_published_at": logicalAt.UTC(), "state": AttemptProcessing,
				},
			},
		})
	if err != nil {
		return err
	}
	if matched == 1 {
		return nil
	}
	if ferr := s.fencedOut(ctx, opID); ferr != nil {
		return ferr
	}
	current, rerr := s.repo.FindByID(ctx, opID)
	if rerr != nil {
		return rerr
	}
	if current == nil {
		return idemErr(CodeNotFound, "no such operation")
	}
	if current.State == StateCompleted {
		return idemErr(CodeAlreadyCompleted, "operation already completed")
	}
	if current.Attempt != attempt || current.AttemptState != AttemptProcessing {
		return idemErr(CodeStaleAttempt, "attempt is not active (terminal or superseded)")
	}
	if current.PublicationID != nil && *current.PublicationID != publicationID {
		return idemErr(CodeConflict, "attempt already frozen to a different publication; reuse the durable snapshot")
	}
	return idemErr(CodeConflict, "snapshot changed concurrently; re-read and retry")
}

// Heartbeat starts lease renewal for (opID, attempt, generation) until
// stop is called (R07: spec §21.5 heartbeat). Renewals run every
// lease/3 (clamped to 1s–60s). The FIRST failed renewal cancels the
// returned context: a generation mismatch (or any loss) means a newer
// worker owns the attempt — the caller must stop all side effects
// immediately. Callers scope the child context to the long side-effect
// section only (never the completion call itself); Completion gates on
// ownership separately. Stopping is idempotent.
func (s *Service) Heartbeat(ctx context.Context, opID primitive.ObjectID, attempt, generation int64) (context.Context, context.CancelFunc) {
	hctx, cancel := context.WithCancel(ctx)
	interval := s.lease / 3
	if interval < time.Second {
		interval = time.Second
	}
	if interval > time.Minute {
		interval = time.Minute
	}
	done := make(chan struct{})
	var once sync.Once
	stop := func() {
		once.Do(func() { close(done) })
		cancel()
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-hctx.Done():
				return
			case <-t.C:
				if _, err := s.RenewLease(hctx, opID, attempt, generation); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	return hctx, stop
}

// RenewLease is the worker heartbeat (every HeartbeatInterval): it CAS-extends
// processing_expires_at without touching the business attempt or the lease
// generation. A generation mismatch means a newer worker owns the attempt —
// the caller must stop all side effects immediately (spec §21.5).
func (s *Service) RenewLease(ctx context.Context, opID primitive.ObjectID, attempt, generation int64) (Operation, error) {
	now := time.Now().UTC()
	matched, err := s.repo.UpdateCAS(ctx,
		bson.M{"_id": opID, "attempt": attempt, "lease_generation": generation, "state": StateProcessing, "attempt_state": AttemptProcessing},
		bson.M{"$set": bson.M{"processing_expires_at": now.Add(s.lease), "updated_at": now}})
	if err != nil {
		return Operation{}, err
	}
	if matched == 1 {
		return s.Get(ctx, opID)
	}
	return Operation{}, s.leaseLossReason(ctx, opID, attempt, generation)
}

// TakeOver CAS-increments lease_generation after the lease has expired,
// keeping the business attempt (and its durable Publication ID + logical
// time) stable. The new owner resumes from the execution snapshot and must
// not re-execute completed side effects.
func (s *Service) TakeOver(ctx context.Context, opID primitive.ObjectID, expectedGeneration int64) (Operation, error) {
	now := time.Now().UTC()
	matched, err := s.repo.UpdateCAS(ctx,
		bson.M{
			"_id": opID, "lease_generation": expectedGeneration,
			"state": StateProcessing, "attempt_state": AttemptProcessing,
			"processing_expires_at": bson.M{"$lte": now},
		},
		bson.M{
			"$inc": bson.M{"lease_generation": 1},
			"$set": bson.M{"processing_expires_at": now.Add(s.lease), "updated_at": now},
		})
	if err != nil {
		return Operation{}, err
	}
	if matched == 1 {
		return s.Get(ctx, opID)
	}
	current, rerr := s.repo.FindByID(ctx, opID)
	if rerr != nil {
		return Operation{}, rerr
	}
	if current == nil {
		return Operation{}, idemErr(CodeNotFound, "no such operation")
	}
	if current.State == StateCompleted {
		return Operation{}, idemErr(CodeAlreadyCompleted, "operation already completed")
	}
	if current.AttemptState != AttemptProcessing {
		return Operation{}, idemErr(CodeStaleAttempt, "attempt is terminal; Begin a retry for a new attempt")
	}
	if current.LeaseGeneration != expectedGeneration {
		return Operation{}, idemErr(CodeLeaseLost, "lease generation changed; a newer worker owns the attempt")
	}
	return Operation{}, idemErr(CodeLeaseActive, "lease still valid; takeover refused")
}

// leaseLossReason distinguishes heartbeat CAS failures.
func (s *Service) leaseLossReason(ctx context.Context, opID primitive.ObjectID, attempt, generation int64) error {
	current, err := s.repo.FindByID(ctx, opID)
	if err != nil {
		return err
	}
	if current == nil {
		return idemErr(CodeNotFound, "no such operation")
	}
	if current.State == StateCompleted {
		return idemErr(CodeAlreadyCompleted, "operation already completed")
	}
	if current.Attempt != attempt || current.AttemptState != AttemptProcessing {
		return idemErr(CodeStaleAttempt, "attempt is not active")
	}
	if current.LeaseGeneration != generation {
		return idemErr(CodeLeaseLost, "lease generation changed; stop all side effects")
	}
	return idemErr(CodeLeaseLost, "lease no longer renewable")
}

// Complete records the attempt outcome and applies the replay matrix
// (spec §21.5, ADR-005 D2):
//   - 2xx → completed; Begin replays the response for same-key + same-body.
//   - 400/422 with validationOnly=true (caller attests pure request
//     validation, deterministic on the request bytes) → completed and
//     replayable for 24h.
//   - anything else (401/403/404/409/429/5xx, or non-attested 400/422) is
//     NEVER completed: the lease is released so the caller can fix state
//     and retry the same attempt via TakeOver.
//
// Completion refreshes expires_at so the replay window runs TTL from
// completion. Response must not contain secrets (Authorization, cookies,
// API keys) — enforced by the caller, not inspected here.
func (s *Service) Complete(ctx context.Context, opID primitive.ObjectID, attempt int64, statusCode int, response map[string]any, validationOnly bool) (Operation, error) {
	now := time.Now().UTC()
	cacheable := (statusCode >= 200 && statusCode < 300) ||
		((statusCode == 400 || statusCode == 422) && validationOnly)
	if !cacheable {
		matched, err := s.repo.UpdateCAS(ctx,
			fenceFilter(ctx, bson.M{"_id": opID, "attempt": attempt, "state": StateProcessing}),
			bson.M{"$set": bson.M{
				"processing_expires_at": now, // release: immediate takeover allowed
				"last_error_code":       httpLikeCode(statusCode),
				"updated_at":            now,
			}})
		if err != nil {
			return Operation{}, err
		}
		if matched == 0 {
			if ferr := s.fencedOut(ctx, opID); ferr != nil {
				return Operation{}, ferr
			}
			return Operation{}, s.completionBlocker(ctx, opID, attempt)
		}
		return s.Get(ctx, opID)
	}
	matched, err := s.repo.UpdateCAS(ctx,
		fenceFilter(ctx, bson.M{"_id": opID, "attempt": attempt, "state": StateProcessing}),
		bson.M{"$set": bson.M{
			"state":                 StateCompleted,
			"attempt_state":         AttemptCompleted,
			"status_code":           statusCode,
			"response":              response,
			"validation_replay":     statusCode == 400 || statusCode == 422,
			"completed_at":          now,
			"updated_at":            now,
			"expires_at":            now.Add(s.ttl),
			"processing_expires_at": now,
		}})
	if err != nil {
		return Operation{}, err
	}
	if matched == 0 {
		if ferr := s.fencedOut(ctx, opID); ferr != nil {
			return Operation{}, ferr
		}
		return Operation{}, s.completionBlocker(ctx, opID, attempt)
	}
	// Mark the matching attempt-history entry completed (best effort: the
	// entry may not exist when validation failed before Freeze).
	_, _ = s.repo.UpdateCAS(ctx,
		bson.M{"_id": opID},
		bson.M{"$set": bson.M{"attempts.$[elem].state": AttemptCompleted}},
		bson.M{"elem.attempt": attempt})
	return s.Get(ctx, opID)
}

// completionBlocker explains why a completion CAS matched nothing.
func (s *Service) completionBlocker(ctx context.Context, opID primitive.ObjectID, attempt int64) error {
	current, err := s.repo.FindByID(ctx, opID)
	if err != nil {
		return err
	}
	if current == nil {
		return idemErr(CodeNotFound, "no such operation")
	}
	if current.State == StateCompleted {
		return idemErr(CodeAlreadyCompleted, "operation already completed")
	}
	if current.Attempt != attempt {
		return idemErr(CodeStaleAttempt, "attempt superseded")
	}
	return idemErr(CodeStaleAttempt, "attempt is not active")
}

// MarkTerminal records a proven terminal pre-activation failure for the
// attempt (render/stage/verify failed with no live side effect) and releases
// the lease. The failed Publication must never be re-staged: the next
// same-key Begin allocates a new attempt + new Publication ID. Crash or
// uncertain takeover before this mark reuses the current attempt instead —
// the two paths must never be mixed (spec §16.1).
func (s *Service) MarkTerminal(ctx context.Context, opID primitive.ObjectID, attempt int64, errCode string) (Operation, error) {
	if strings.TrimSpace(errCode) == "" {
		return Operation{}, idemErr(CodeInvalidRequest, "terminal error code is required")
	}
	now := time.Now().UTC()
	matched, err := s.repo.UpdateCAS(ctx,
		fenceFilter(ctx, bson.M{"_id": opID, "attempt": attempt, "state": StateProcessing, "attempt_state": AttemptProcessing}),
		bson.M{"$set": bson.M{
			"attempt_state":         AttemptTerminal,
			"terminal_error_code":   errCode,
			"processing_expires_at": now, // release the worker lease
			"updated_at":            now,
		}})
	if err != nil {
		return Operation{}, err
	}
	if matched == 0 {
		if ferr := s.fencedOut(ctx, opID); ferr != nil {
			return Operation{}, ferr
		}
		return Operation{}, s.completionBlocker(ctx, opID, attempt)
	}
	_, _ = s.repo.UpdateCAS(ctx,
		bson.M{"_id": opID},
		bson.M{"$set": bson.M{"attempts.$[elem].state": AttemptTerminal}},
		bson.M{"elem.attempt": attempt})
	return s.Get(ctx, opID)
}

// isDupKey reports duplicate-key errors without depending on exact driver
// error wrapping (mirrors internal/database product tests).
func isDupKey(err error) bool {
	if err == nil {
		return false
	}
	if mongo.IsDuplicateKeyError(err) {
		return true
	}
	return strings.Contains(err.Error(), "E11000")
}

// httpLikeCode renders a status for last_error_code diagnostics.
func httpLikeCode(statusCode int) string {
	return "HTTP_" + itoa(statusCode)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
