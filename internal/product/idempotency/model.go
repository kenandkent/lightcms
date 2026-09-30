// Package idempotency implements Task 11 (Lane D): durable request
// operations with the business attempt separated from the worker lease
// generation (spec §16.1, §21, ADR-005).
//
// Operation identity is (owner, method, path, key, canonicalBodyHash):
// same key + same canonical body replays a completed response, same key +
// changed body conflicts with 409 IDEMPOTENCY_CONFLICT.
//
// A business attempt is stable across worker crashes; the worker lease
// (lease_generation + processing_expires_at, heartbeat 60s) is CAS-guarded.
// Lease expiry lets another worker take over the SAME attempt (same
// Publication ID + logical time). Only a terminal pre-activation failure
// (MarkTerminal) lets a same-key retry allocate a new attempt with a new
// Publication ID via Begin.
//
// Mongo indexes (UNIQUE(owner, method, path, key) + TTL(expires_at)) are
// owned by Task 2 (DB.EnsureProductIndexes); this package reuses them and
// defines no indexes.
package idempotency

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// CollectionName is the Mongo collection for idempotency records (spec §21.2).
const CollectionName = "idempotency_records"

// TTL bounds (spec §21.4: default 24h, configurable 1–72h).
const (
	DefaultTTLHours = 24
	MinTTLHours     = 1
	MaxTTLHours     = 72
)

// DefaultLeaseMinutes is the default processing lease (spec §21.5: 5 min).
const DefaultLeaseMinutes = 5

// HeartbeatInterval is the worker heartbeat cadence (spec §21.5: 60s).
// Heartbeats CAS-extend processing_expires_at by the full lease duration.
const HeartbeatInterval = 60 * time.Second

// Record-level lifecycle states.
type State string

const (
	// StateProcessing means the operation has not completed; retries either
	// see REQUEST_IN_PROGRESS, take over an expired lease, or advance a
	// terminal attempt.
	StateProcessing State = "processing"
	// StateCompleted means a 2xx (or pure-validation 400/422) response is
	// cached and replayed for same-key + same-body retries.
	StateCompleted State = "completed"
)

// Business-attempt states.
type AttemptState string

const (
	// AttemptProcessing is the active attempt of a processing operation.
	AttemptProcessing AttemptState = "processing"
	// AttemptTerminal marks a proven pre-activation failure with no live
	// side effect (render/stage/verify failed). Same-key retry increments
	// the attempt and allocates a new Publication ID. Failed Publications
	// are never re-staged.
	AttemptTerminal AttemptState = "terminal_pre_activation_failure"
	// AttemptCompleted marks the attempt whose response is cached.
	AttemptCompleted AttemptState = "completed"
)

// Error codes. HTTP mapping is owned by Task 12; 409-class codes are noted.
const (
	// CodeConflict is same-key + changed-body (HTTP 409 IDEMPOTENCY_CONFLICT).
	CodeConflict = "IDEMPOTENCY_CONFLICT"
	// CodeInProgress is same-key + same-body with a live lease
	// (HTTP 409 REQUEST_IN_PROGRESS, retryable with Retry-After).
	CodeInProgress = "REQUEST_IN_PROGRESS"
	// CodeLeaseExpired means the lease lapsed; the caller may TakeOver the
	// same attempt (HTTP 409/503 + Retry-After, Task 12 maps it).
	CodeLeaseExpired = "IDEMPOTENCY_LEASE_EXPIRED"
	// CodeLeaseLost means the caller's lease generation is stale (a newer
	// worker owns the attempt); the caller must stop side effects.
	CodeLeaseLost = "IDEMPOTENCY_LEASE_LOST"
	// CodeLeaseActive means takeover was attempted before expiry.
	CodeLeaseActive = "IDEMPOTENCY_LEASE_ACTIVE"
	// CodeStaleAttempt means the attempt number no longer matches (a newer
	// attempt exists, or the attempt is terminal — Begin a retry instead).
	CodeStaleAttempt = "IDEMPOTENCY_STALE_ATTEMPT"
	// CodeAlreadyCompleted means a mutation was attempted on a completed op.
	CodeAlreadyCompleted = "IDEMPOTENCY_ALREADY_COMPLETED"
	// CodeInvalidRequest covers empty owner/method/path/key, bad IDs, etc.
	CodeInvalidRequest = "INVALID_REQUEST"
	// CodeNotFound means no record for the operation ID.
	CodeNotFound = "IDEMPOTENCY_NOT_FOUND"
)

// Error is a typed idempotency failure. Code is one of the Code* constants.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return "idempotency " + e.Code + ": " + e.Message }

// CodeOf maps any error to an idempotency code; unknown errors map to "".
func CodeOf(err error) string {
	if err == nil {
		return ""
	}
	if ie, ok := err.(*Error); ok {
		return ie.Code
	}
	return ""
}

func idemErr(code, message string) *Error { return &Error{Code: code, Message: message} }

// AttemptRecord keeps the per-attempt Publication ID association
// (spec §21.5: the record saves each attempt's publication linkage; the
// final completed response points at the successful attempt).
type AttemptRecord struct {
	Attempt       int64               `bson:"attempt" json:"attempt"`
	PublicationID *primitive.ObjectID `bson:"publication_id,omitempty" json:"publication_id,omitempty"`
	LogicalAt     *time.Time          `bson:"logical_published_at,omitempty" json:"logical_published_at,omitempty"`
	State         AttemptState        `bson:"state" json:"state"`
}

// Operation is the durable idempotency record (spec §21.2 + §21.5 fields).
// The Mongo _id IS the operation ID; OperationIDHex mirrors it as the
// spec's "operation_id" string for serialized shapes.
type Operation struct {
	ID               primitive.ObjectID  `bson:"_id" json:"id"`
	OperationIDHex   string              `bson:"operation_id" json:"operation_id"`
	Owner            string              `bson:"owner" json:"owner"`
	Method           string              `bson:"method" json:"method"`
	Path             string              `bson:"path" json:"path"`
	Key              string              `bson:"key" json:"key"`
	RequestHash      string              `bson:"request_hash" json:"request_hash"`
	State            State               `bson:"state" json:"state"`
	Attempt          int64               `bson:"attempt" json:"attempt"`
	AttemptState     AttemptState        `bson:"attempt_state" json:"attempt_state"`
	LeaseGeneration  int64               `bson:"lease_generation" json:"lease_generation"`
	LeaseExpiresAt   time.Time           `bson:"processing_expires_at" json:"processing_expires_at"`
	ContentID        *primitive.ObjectID `bson:"content_id,omitempty" json:"content_id,omitempty"`
	ContentVersion   int64               `bson:"content_version,omitempty" json:"content_version,omitempty"`
	CanonicalPath    string              `bson:"canonical_full_path,omitempty" json:"canonical_full_path,omitempty"`
	PublicationID    *primitive.ObjectID `bson:"publication_id,omitempty" json:"publication_id,omitempty"`
	LogicalAt        *time.Time          `bson:"logical_published_at,omitempty" json:"logical_published_at,omitempty"`
	TemplateVersion  *primitive.ObjectID `bson:"template_version_id,omitempty" json:"template_version_id,omitempty"`
	StatusCode       int                 `bson:"status_code,omitempty" json:"status_code,omitempty"`
	Response         map[string]any      `bson:"response,omitempty" json:"response,omitempty"`
	ValidationReplay bool                `bson:"validation_replay,omitempty" json:"validation_replay,omitempty"`
	Attempts         []AttemptRecord     `bson:"attempts,omitempty" json:"attempts,omitempty"`
	LastErrorCode    string              `bson:"last_error_code,omitempty" json:"last_error_code,omitempty"`
	TerminalError    string              `bson:"terminal_error_code,omitempty" json:"terminal_error_code,omitempty"`
	CreatedAt        time.Time           `bson:"created_at" json:"created_at"`
	UpdatedAt        time.Time           `bson:"updated_at" json:"updated_at"`
	CompletedAt      *time.Time          `bson:"completed_at,omitempty" json:"completed_at,omitempty"`
	ExpiresAt        time.Time           `bson:"expires_at" json:"expires_at"`

	// Replay is transient (never persisted): Begin sets it when the call
	// is a replay of a completed response. The caller must return
	// StatusCode/Response verbatim (with a fresh HTTP Date).
	Replay bool `bson:"-" json:"-"`
}

// CanonicalHash computes the spec §21.5 request hash: the SHA-256 over the
// canonical JSON form (insignificant whitespace removed, object keys sorted
// — encoding/json marshals maps with sorted keys), prefixed with "sha256:".
// Non-JSON bodies hash as raw bytes; empty bodies hash as empty input, so
// semantically identical retries always match while any byte-level change
// (after canonicalization) conflicts.
func CanonicalHash(canonicalBody []byte) string {
	trimmed := bytes.TrimSpace(canonicalBody)
	if len(trimmed) == 0 {
		sum := sha256.Sum256(nil)
		return "sha256:" + hex.EncodeToString(sum[:])
	}
	payload := trimmed
	var v any
	if err := json.Unmarshal(trimmed, &v); err == nil {
		if canonical, err := json.Marshal(v); err == nil {
			payload = canonical
		}
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}
