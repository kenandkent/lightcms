// Package publication owns LightCMS V3 publication records and their state
// machine (Task 5, Lane C).
//
// Scope boundary (plan Task 5; spec §15, §18.1, §28.1):
//   - records (Publication model + shared Publish/Unpublish/Rollback request
//     contract), lifecycle/storage/verification transition rules, and the
//     transactional repository (GetActive, InsertStaged, ActivateCAS,
//     UnpublishCAS, MarkFailed, ListHistory) — WITHOUT file effects.
//   - The publish/unpublish saga orchestration is Task 8 (service.go), the
//     outbox delivery worker is Task 9 (outbox.go), and the recovery scanner
//     and retention GC are Task 10 (recovery.go, gc.go). This package must not
//     grow saga.go, recovery.go, or outbox.go; Tasks 8–10 add those files.
//
// Truth model (spec §15.7):
//
//	content_publications(status=active) = history, control-plane, expected-state truth
//	Content.Published / Content.PublishedAt = compatibility projections
//	filesystem canonical file = data-plane served-content truth
//	immutable publication object = verifiable, recoverable content truth
//
// Content carries NO active_publication_id: DTOs derive it via GetActive.
// Publication activation and unpublish each commit lifecycle + Content
// projection + webhook outbox event in ONE Mongo transaction (replica set
// required). File cutover before/after that transaction is the Task 8 saga;
// crash-window reconciliation is the Task 10 scanner.
package publication

import (
	"errors"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// Collection names owned by this package (indexes defined by Task 2 in
// DB.EnsureProductIndexes — reused here, never redefined).
const (
	// CollectionPublications is the content_publications collection (§15.2).
	CollectionPublications = "content_publications"
	// CollectionContent is the live Content collection (projection sync target).
	CollectionContent = "content"
	// CollectionOutbox is the webhook_outbox collection (spec §28.1).
	CollectionOutbox = "webhook_outbox"
)

// Publication lifecycle states (spec §15.3, §15.6).
type Status string

const (
	// StatusStaged is a rendered, verified candidate not yet serving traffic.
	StatusStaged Status = "staged"
	// StatusActive is the single serving publication per content.
	StatusActive Status = "active"
	// StatusSuperseded is a formerly active publication replaced by activation.
	StatusSuperseded Status = "superseded"
	// StatusFailed is a terminally failed staging attempt; never reactivated.
	StatusFailed Status = "failed"
	// StatusUnpublished is a formerly active publication taken offline.
	// History is retained; storage_state tracks the immutable object.
	StatusUnpublished Status = "unpublished"
)

// Storage states (spec §15.3, §15.6). Independent of lifecycle: retention GC
// only flips storage_state and never rewrites lifecycle history.
type StorageState string

const (
	// StoragePending: immutable object staging in progress.
	StoragePending StorageState = "pending"
	// StoragePresent: immutable object durably stored and readable.
	StoragePresent StorageState = "present"
	// StorageDeleting: retention GC has claimed the object for deletion.
	StorageDeleting StorageState = "deleting"
	// StorageDeleted: immutable object removed; metadata retained for audit.
	StorageDeleted StorageState = "deleted"
	// StorageMissing: expected object absent (stage never completed or lost).
	StorageMissing StorageState = "missing"
	// StorageCorrupt: object present but hash-mismatched; never served blindly.
	StorageCorrupt StorageState = "corrupt"
)

// Verification states (spec §15.3).
type VerificationStatus string

const (
	// VerificationPending: created, not yet stage/verify checked.
	VerificationPending VerificationStatus = "pending"
	// VerificationVerified: staged bytes passed SHA-256 verification.
	VerificationVerified VerificationStatus = "verified"
	// VerificationFailed: stage/verify rejected the candidate.
	VerificationFailed VerificationStatus = "failed"
	// VerificationLegacyUnverified: migration-imported bytes (§35.3) that
	// differ from a fresh re-render. Servable only via the explicit
	// migration path; ordinary business publish must not activate it
	// (enforced by the Task 8 saga, not by this repository).
	VerificationLegacyUnverified VerificationStatus = "legacy_unverified"
)

// Publication is one rendered, verified, activatable static deployment — not
// a second Page (spec §15.2). Created immutable except for the lifecycle,
// storage, verification, pin, and timestamp fields the state machine owns.
type Publication struct {
	ID                primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	ContentID         primitive.ObjectID `bson:"content_id" json:"content_id"`
	ContentVersion    int64              `bson:"content_version" json:"content_version"`
	TemplateID        primitive.ObjectID `bson:"template_id,omitempty" json:"template_id,omitempty"`
	TemplateVersionID primitive.ObjectID `bson:"template_version_id,omitempty" json:"template_version_id,omitempty"`
	TemplateVersion   int64              `bson:"template_version,omitempty" json:"template_version,omitempty"`
	FullPath          string             `bson:"full_path" json:"full_path"`
	ContentHash       string             `bson:"content_hash" json:"content_hash"`
	// PublicURL is the resolved canonical public URL frozen at plan time
	// (Task 16E): activation-transaction outbox inserts carry it so
	// delivery never joins it back.
	PublicURL string `bson:"public_url,omitempty" json:"public_url,omitempty"`

	StorageProvider string       `bson:"storage_provider,omitempty" json:"storage_provider,omitempty"`
	StoragePath     string       `bson:"storage_path,omitempty" json:"storage_path,omitempty"`
	StorageState    StorageState `bson:"storage_state" json:"storage_state"`

	Status             Status             `bson:"status" json:"status"`
	VerificationStatus VerificationStatus `bson:"verification_status" json:"verification_status"`
	Pinned             bool               `bson:"pinned,omitempty" json:"pinned,omitempty"`

	// LogicalPublishedAt is frozen before Render and is the business publish
	// time: it feeds the template published_at context, Content.PublishedAt,
	// and the content.publish webhook payload. ActivatedAt is only the
	// physical commit time for latency/audit debugging (spec §15.3).
	LogicalPublishedAt time.Time           `bson:"logical_published_at,omitempty" json:"logical_published_at,omitempty"`
	PublishedBy        *primitive.ObjectID `bson:"published_by,omitempty" json:"published_by,omitempty"`
	PublishedByEmail   string              `bson:"published_by_email,omitempty" json:"published_by_email,omitempty"`
	Actor              string              `bson:"actor,omitempty" json:"actor,omitempty"`
	Via                string              `bson:"via,omitempty" json:"via,omitempty"`
	AgentSession       string              `bson:"agent_session,omitempty" json:"agent_session,omitempty"`

	CreatedAt        time.Time  `bson:"created_at" json:"created_at"`
	ActivatedAt      *time.Time `bson:"activated_at,omitempty" json:"activated_at,omitempty"`
	SupersededAt     *time.Time `bson:"superseded_at,omitempty" json:"superseded_at,omitempty"`
	UnpublishedAt    *time.Time `bson:"unpublished_at,omitempty" json:"unpublished_at,omitempty"`
	StorageDeletedAt *time.Time `bson:"storage_deleted_at,omitempty" json:"storage_deleted_at,omitempty"`

	RendererVersion        string         `bson:"renderer_version,omitempty" json:"renderer_version,omitempty"`
	ProductBuildSHA        string         `bson:"product_build_sha,omitempty" json:"product_build_sha,omitempty"`
	RenderDependenciesHash string         `bson:"render_dependencies_hash,omitempty" json:"render_dependencies_hash,omitempty"`
	DependencySnapshot     map[string]any `bson:"dependency_snapshot,omitempty" json:"dependency_snapshot,omitempty"`

	FailureReason string `bson:"failure_reason,omitempty" json:"failure_reason,omitempty"`
}

// IsServable reports the control-plane serving decision: active with verified
// or migration-approved legacy_unverified output. Storage state never affects
// this decision — a missing/mismatched canonical is a data-plane repair job
// for the Task 10 scanner, not a reason to hide the control-plane truth.
// legacy_unverified pages therefore keep serving and are never quarantined.
func (p *Publication) IsServable() bool {
	if p == nil || p.Status != StatusActive {
		return false
	}
	return p.VerificationStatus == VerificationVerified ||
		p.VerificationStatus == VerificationLegacyUnverified
}

// Shared interface contract (plan §Shared interface contract). These request
// shapes are defined here so Tasks 8 (saga) and 12 (HTTP facade) build on
// the exact handoff types; the orchestration itself lives in Task 8.
type PublishRequest struct {
	ContentID         primitive.ObjectID
	ContentVersion    int64
	TemplateVersionID primitive.ObjectID
	ExpectedActiveID  *primitive.ObjectID
	Reason            string
	IdempotencyRecord *primitive.ObjectID
}

type PublicationResult struct {
	PublicationID      primitive.ObjectID
	ContentID          primitive.ObjectID
	ContentVersion     int64
	TemplateVersionID  primitive.ObjectID
	FullPath           string
	PublicURL          string
	ContentHash        string
	LogicalPublishedAt time.Time
}

type UnpublishRequest struct {
	ContentID        primitive.ObjectID
	ExpectedActiveID *primitive.ObjectID
}

type RollbackRequest struct {
	ContentID           primitive.ObjectID
	SourcePublicationID primitive.ObjectID
	ExpectedActiveID    *primitive.ObjectID
	IdempotencyRecord   *primitive.ObjectID
}

// Error codes. PUBLICATION_CONFLICT matches spec §27; the remaining codes
// are repository-local and never leak driver internals to callers.
const (
	// CodeConflict maps stale-expected-ID CAS failures and duplicate active
	// pointers to HTTP 409 PUBLICATION_CONFLICT.
	CodeConflict = "PUBLICATION_CONFLICT"
	// CodeInvalidTransition marks a disallowed state-machine edge (e.g.
	// failed -> active). Rollback creates a NEW record instead.
	CodeInvalidTransition = "PUBLICATION_INVALID_TRANSITION"
	// CodeNotFound marks a missing publication record.
	CodeNotFound = "PUBLICATION_NOT_FOUND"
	// CodeContentNotFound marks a missing Content row under a publication op.
	CodeContentNotFound = "CONTENT_NOT_FOUND"
	// CodeValidation marks a malformed record or request.
	CodeValidation = "PUBLICATION_VALIDATION_FAILED"
)

// Error is a typed publication failure.
type Error struct {
	Code    string
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Message != "" {
		return "publication " + e.Code + ": " + e.Message
	}
	return "publication " + e.Code
}

// Unwrap exposes the underlying cause (e.g. the driver duplicate-key error).
func (e *Error) Unwrap() error { return e.Err }

func pubErr(code, message string, err error) *Error {
	return &Error{Code: code, Message: message, Err: err}
}

// CodeOf maps any error to a publication code; unknown errors yield "".
func CodeOf(err error) string {
	if err == nil {
		return ""
	}
	var pe *Error
	if asErr(err, &pe) {
		return pe.Code
	}
	return ""
}

// IsConflict reports PUBLICATION_CONFLICT, including raw driver duplicate-key
// errors from the partial unique active index that bypass the typed path.
func IsConflict(err error) bool {
	if err == nil {
		return false
	}
	if CodeOf(err) == CodeConflict {
		return true
	}
	return isDupKey(err)
}

func asErr(err error, target **Error) bool {
	return errors.As(err, target)
}

// isDupKey reports Mongo duplicate-key errors (code 11000) without depending
// on exact driver error wrapping.
func isDupKey(err error) bool {
	if err == nil {
		return false
	}
	if mongo.IsDuplicateKeyError(err) {
		return true
	}
	return strings.Contains(err.Error(), "duplicate key")
}
