package services

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonradoff/lightcms/v7/internal/product/idempotency"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Task 16D: stable operation keys for background publication callers
// (spec §16.6: scheduler, import auto-publish, copilot, search-replace
// auto-republish, template upgrade). Interactive callers carry an external
// Idempotency-Key; internal jobs have none, so each job family builds a
// deterministic key for its logical operation:
//
//   - scheduler:  scheduler/<contentID>/v<currentVersion>
//   - import:      import/<jobID>/<contentID>      (create-only call sites)
//   - copilot:     copilot/<session>/<contentID>/v<currentVersion>
//   - search-replace: sr/<requestKey>/<contentID>  (requestKey = header or UUID)
//   - upgrade job: upgrade/<jobID>/<contentID>     (generation package)
//
// Same key + completed → replay with NO new Publication/outbox row.
// Terminal pre-activation failure → next same-key retry allocates a new
// attempt (new Publication ID, per spec review focus §3). A live lease
// (parallel worker) or a lapsed lease (crashed worker) surfaces
// ErrPublishRetryLater / takeover instead of a duplicate.

// internalIdem is the shared idempotency service for background publish.
// Wired once by the Task 16 server construction; nil preserves the unkeyed
// legacy delegation (tests + unwired binaries).
var internalIdem *idempotency.Service

// SetInternalIdempotency wires the shared idempotency service used by
// PublishInternal. Called once at server startup (Task 16).
func SetInternalIdempotency(idem *idempotency.Service) {
	internalIdem = idem
}

// ErrPublishRetryLater reports a transient idempotency state (a parallel
// worker holds the lease, or a takeover race): the caller must retry the
// same stable key later, never mint a fresh key.
var ErrPublishRetryLater = fmt.Errorf("publication already in progress; retry the same operation key later")

// SchedulerOpKey is stable across ticks for one due version of one item.
// A content edit bumps CurrentVersion → a new key (new intent); an
// unpublished-again item also carries a new version (Unpublish versions).
func SchedulerOpKey(contentID primitive.ObjectID, version int64) string {
	return fmt.Sprintf("scheduler/%s/v%d", contentID.Hex(), version)
}

// ImportOpKey is stable for one created page within one import job.
func ImportOpKey(jobID, contentID primitive.ObjectID) string {
	return fmt.Sprintf("import/%s/%s", jobID.Hex(), contentID.Hex())
}

// CopilotOpKey is stable for one session publishing one content version.
func CopilotOpKey(session string, contentID primitive.ObjectID, version int64) string {
	session = strings.TrimSpace(session)
	if session == "" {
		session = "no-session"
	}
	return fmt.Sprintf("copilot/%s/%s/v%d", session, contentID.Hex(), version)
}

// SearchReplaceOpKey is stable for one page within one execute request.
func SearchReplaceOpKey(requestKey string, contentID primitive.ObjectID) string {
	requestKey = strings.TrimSpace(requestKey)
	if requestKey == "" {
		requestKey = "no-key"
	}
	return fmt.Sprintf("sr/%s/%s", requestKey, contentID.Hex())
}

// PublishInternal publishes one content item through the saga under a stable
// idempotency key. It is the ONLY path background callers may use:
// scheduler, import auto-publish, copilot publish and search-replace
// auto-republish. owner/path identify the job family for the idempotency
// record; key is one of the OpKey builders above.
func (s *ContentService) PublishInternal(ctx context.Context, contentID primitive.ObjectID, owner, path, key string) error {
	if internalIdem == nil {
		// Unwired (unit tests): legacy delegation — the saga when 16C wired
		// it via SetPublicationPublisher, else the legacy publish path.
		return s.PublishContent(ctx, contentID)
	}
	op, err := internalIdem.Begin(ctx, owner, "POST", path, key, nil)
	if err != nil {
		code := idempotency.CodeOf(err)
		switch code {
		case idempotency.CodeLeaseExpired:
			// Crashed worker: take over the same attempt (reuses the frozen
			// execution snapshot — no duplicate publication).
			op, terr := internalIdem.TakeOverByKey(ctx, owner, "POST", path, key)
			if terr != nil {
				return fmt.Errorf("%w: %v", ErrPublishRetryLater, terr)
			}
			if op.State == idempotency.StateCompleted {
				return nil
			}
		case idempotency.CodeInProgress:
			return fmt.Errorf("%w: %v", ErrPublishRetryLater, err)
		default:
			return err
		}
	}
	if op.Replay {
		// Same key already completed: no new Publication, no new outbox row.
		return nil
	}
	if legacyPublicationSaga == nil {
		return fmt.Errorf("publication saga is not wired")
	}
	// Lane 2B: thread caller attribution (when the caller's ctx carries
	// middleware-stamped provenance, e.g. copilot/API-triggered jobs) into
	// the minted Publication record.
	actor, via, session := publishAttributionFromContext(ctx)
	_, perr := legacyPublicationSaga.Publish(ctx, publication.PublishRequest{
		ContentID: contentID, IdempotencyRecord: &op.ID,
		Actor: actor, Via: via, AgentSession: session,
		AuthorIsAdmin: AuthorIsAdminFromContext(ctx),
	})
	if perr != nil {
		// Pre-activation failure: mark terminal so the next same-key Begin
		// allocates a new attempt (a retry after a terminal staging failure
		// uses a NEW Publication ID). Post-commit paths return success, so
		// this never marks a live page's operation terminal.
		code := publication.CodeOf(perr)
		if code == "" {
			code = "PUBLISH_FAILED"
		}
		_, _ = internalIdem.MarkTerminal(ctx, op.ID, op.Attempt, code)
		return perr
	}
	_, _ = internalIdem.Complete(ctx, op.ID, op.Attempt, 200,
		map[string]any{"content_id": contentID.Hex()}, false)
	return nil
}
