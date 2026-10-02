// Package generation implements Task 12 (Lane E): one REST generation
// command and its supporting operations (plan Task 12; spec §8.1, §12, §20,
// §21; ADR-001/002/003/005).
//
// Shared interface contract (verbatim — must not be renamed):
//
//	type GenerateRequest struct {
//	    Template, Title, Slug, FolderPath, Mode string
//	    ExpectedTemplateVersion *int64
//	    Data map[string]any
//	    Upsert bool
//	}
//	type GenerateResponse struct {
//	    ID, Action, Template, FullPath, Mode string
//	    Published, RequiresPublish bool
//	    PublicURL *string
//	    ContentVersion, TemplateVersion int64
//	    PublicationID *string
//	    Warnings []templatecontract.FieldWarning
//	}
//
// Ownership: this package owns the 5 MiB data cap (Task 4 left it), the
// published-draft → Fork routing (§20.6 matrix), purity/secrets attestation
// for idempotency Complete, and all generation error mappings. It imports
// (never redefines): templatecontract.ValidateData/ToJSONSchema, pathkey,
// publication saga (Publish/Unpublish/Rollback + legacy_unverified rejection),
// idempotency Begin/Bind/Freeze/Complete, and publicurl.ValidateProductionBaseURL.
//
// Route registration lives in Task 16 (cmd/server/main.go) — this package
// builds the service + pure handler helpers only.
package generation

import (
	"context"
	"fmt"

	"github.com/jonradoff/lightcms/v7/internal/auth"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// GenerateRequest is the verbatim shared contract (plan §Shared interface
// contract). Mode is one of draft|preview|sandbox|publish. ExpectedTemplateVersion
// is an optimistic precondition for mode=publish (not historical selection).
// Data is full-replace on upsert hits (no implicit merge). Upsert=false with
// an existing canonical path yields 409 PATH_CONFLICT.
type GenerateRequest struct {
	Template, Title, Slug, FolderPath, Mode string
	ExpectedTemplateVersion                 *int64
	Data                                    map[string]any
	Upsert                                  bool
}

// GenerateResponse is the verbatim shared contract. RequiresPublish is always
// present: draft/sandbox writes return true, preview and completed publish
// return false; published=true may coexist with requires_publish=true when a
// live page has unpublished changes (§20.4).
type GenerateResponse struct {
	ID, Action, Template, FullPath, Mode string
	Published, RequiresPublish           bool
	PublicURL                            *string
	ContentVersion, TemplateVersion      int64
	PublicationID                        *string
	Warnings                             []templatecontract.FieldWarning
}

// MaxDataBytes is the product-layer cap on canonical Data JSON (§20.7).
// The transport still enforces the existing 10 MiB body limit; this is the
// additional 5 MiB Data-only bound owned by Task 12.
const MaxDataBytes = 5 << 20 // 5 MiB

// Modes.
const (
	ModeDraft   = "draft"
	ModePreview = "preview"
	ModeSandbox = "sandbox"
	ModePublish = "publish"
)

// Actor carries the already-authenticated caller identity for scope/sandbox
// decisions. Handlers populate it from the existing /api/v1 auth middleware
// (Task 16 wiring); tests construct it directly.
//
// Role is the caller's RBAC role (viewer/contributor/editor/admin) and is
// the PRIMARY authorization input: Can intersects the role permission set
// with the sandbox-only restriction and the key scope allowlist. An empty
// Role grants nothing — extractors must always populate it from the
// authenticated session/API key.
type Actor struct {
	ID            string
	Email         string
	Authenticated bool
	IsAdmin       bool
	Role          string
	Scopes        []string
	SandboxOnly   bool
	SandboxForkID *primitive.ObjectID
	AgentSession  string
	Via           string
	ActorKind     string // "human" | "agent"
}

// Can reports whether the actor holds permission p: the role must grant it,
// sandbox-only keys are further narrowed to the sandbox allowlist, and a
// non-empty Scopes allowlist must contain it. This mirrors
// auth.UserHasPermission for the V3 actor shape (which cannot import session
// state); every generation authorization check must use Can, never the
// legacy scope-only HasScope.
func (a Actor) Can(p string) bool {
	if !auth.HasPermission(a.Role, p) {
		return false
	}
	if a.SandboxOnly && !auth.SandboxPermitted(p) {
		return false
	}
	if len(a.Scopes) > 0 {
		for _, s := range a.Scopes {
			if s == p {
				return true
			}
		}
		return false
	}
	return true
}

// HasScope reports whether the actor carries scope s. An empty Scopes
// allowlist means full owner permissions (existing API-key semantics).
func (a Actor) HasScope(s string) bool {
	if len(a.Scopes) == 0 {
		return true
	}
	for _, v := range a.Scopes {
		if v == s {
			return true
		}
	}
	return false
}

// HasScopes reports whether the actor carries every scope in ss.
func (a Actor) HasScopes(ss ...string) bool {
	for _, s := range ss {
		if !a.HasScope(s) {
			return false
		}
	}
	return true
}

// Owner returns the idempotency owner string for this actor (spec §21.5:
// database API key ID; OAuth uses client+subject). For generation the stable
// owner is ID when present, else Email, else "anonymous" (which never reaches
// publish — unauthenticated is 401 first).
func (a Actor) Owner() string {
	if a.ID != "" {
		return a.ID
	}
	if a.Email != "" {
		return a.Email
	}
	return "anonymous"
}

// Scope constants for generation (§22.2).
const (
	ScopeContentCreate  = "content.create"
	ScopeContentEdit    = "content.edit"
	ScopeContentPublish = "content.publish"
	ScopeContentView    = "content.view"
	ScopeTemplateView   = "template.view"
	ScopeTemplateEdit   = "template.edit"
	ScopeAssetUpload    = "asset.upload"
)

// Error codes (spec §27 + generation mappings). HTTP mapping lives in
// httpapi/errors.go via StatusForCode; service tests assert codes.
const (
	CodeUnauthenticated              = "UNAUTHENTICATED"
	CodePermissionDenied             = "PERMISSION_DENIED"
	CodeTemplateNotFound             = "TEMPLATE_NOT_FOUND"
	CodeTemplateNotActive            = "TEMPLATE_NOT_ACTIVE"
	CodeTemplateSchemaInvalid        = "TEMPLATE_SCHEMA_INVALID"
	CodeTemplateVersionNotFound      = "TEMPLATE_VERSION_NOT_FOUND"
	CodeTemplateVersionChanged       = "TEMPLATE_VERSION_CHANGED"
	CodeTemplatePreconditionRequired = "TEMPLATE_VERSION_PRECONDITION_REQUIRED"
	CodeTemplateVersionConflict      = "TEMPLATE_VERSION_CONFLICT"
	CodeFieldValidationFailed        = "FIELD_VALIDATION_FAILED"
	CodeDataTooLarge                 = "DATA_TOO_LARGE"
	CodePathInvalid                  = "PATH_INVALID"
	CodePathConflict                 = "PATH_CONFLICT"
	CodeContentCreateFailed          = "CONTENT_CREATE_FAILED"
	CodeContentUpdateFailed          = "CONTENT_UPDATE_FAILED"
	CodeContentVersionConflict       = "CONTENT_VERSION_CONFLICT"
	CodeContentNotFound              = "CONTENT_NOT_FOUND"
	CodePublicationNotFound          = "PUBLICATION_NOT_FOUND"
	CodePublicationConflict          = "PUBLICATION_CONFLICT"
	CodePagePublishInProgress        = "PAGE_PUBLISH_IN_PROGRESS"
	CodePublicURLFailed              = "PUBLIC_URL_RESOLUTION_FAILED"
	CodeIdempotencyConflict          = "IDEMPOTENCY_CONFLICT"
	CodeIdempotencyKeyRequired       = "IDEMPOTENCY_KEY_REQUIRED"
	CodeRequestInProgress            = "REQUEST_IN_PROGRESS"
	CodeAgentSandboxRequired         = "AGENT_SANDBOX_REQUIRED"
	CodeRateLimited                  = "RATE_LIMITED"
	CodeStoreUnavailable             = "PUBLICATION_STAGE_FAILED"
	CodeInternal                     = "INTERNAL_ERROR"
	CodeInvalidRequest               = "INVALID_REQUEST"
	CodeUpgradeJobNotFound           = "UPGRADE_JOB_NOT_FOUND"
	CodeUpgradeJobConflict           = "UPGRADE_JOB_CONFLICT"
	CodeRestorePreconditionFailed    = "RESTORE_PRECONDITION_FAILED"
)

// FieldDetail is one field-level diagnostic inside a 422 envelope.
type FieldDetail struct {
	Code    string `json:"code"`
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Error is a typed generation failure.
type Error struct {
	Code       string
	Message    string
	Details    []FieldDetail
	RetryAfter int // seconds; set for 429 / REQUEST_IN_PROGRESS / retryable 503
	Err        error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("generation %s: %s: %v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("generation %s: %s", e.Code, e.Message)
}

// Unwrap exposes the cause.
func (e *Error) Unwrap() error { return e.Err }

// CodeOf maps any error to a generation code; unknown errors yield "".
func CodeOf(err error) string {
	if err == nil {
		return ""
	}
	if ge, ok := err.(*Error); ok {
		return ge.Code
	}
	return ""
}

func genErr(code, message string, err error) *Error {
	return &Error{Code: code, Message: message, Err: err}
}

// StatusForCode maps a generation code to its HTTP status (§20.8).
func StatusForCode(code string) int {
	switch code {
	case CodeUnauthenticated:
		return 401
	case CodePermissionDenied:
		return 403
	case CodeTemplateNotFound, CodeContentNotFound, CodePublicationNotFound,
		CodeTemplateVersionNotFound, CodeUpgradeJobNotFound:
		return 404
	case CodeFieldValidationFailed, CodePathInvalid, CodeDataTooLarge,
		CodeTemplateSchemaInvalid:
		return 422
	case CodeTemplatePreconditionRequired, CodeIdempotencyKeyRequired:
		return 428
	case CodeRateLimited:
		return 429
	case CodeStoreUnavailable:
		return 503
	case CodePathConflict, CodeTemplateNotActive, CodeTemplateVersionChanged,
		CodeTemplateVersionConflict, CodeContentVersionConflict,
		CodePublicationConflict, CodePagePublishInProgress,
		CodeIdempotencyConflict, CodeRequestInProgress,
		CodeAgentSandboxRequired, CodeUpgradeJobConflict:
		return 409
	case CodeInvalidRequest:
		return 400
	default:
		return 500
	}
}

// Retryable reports whether the code is retryable (429, REQUEST_IN_PROGRESS,
// retryable 503 set Retry-After upstream).
func Retryable(code string) bool {
	switch code {
	case CodeRateLimited, CodeRequestInProgress, CodeStoreUnavailable,
		CodePagePublishInProgress:
		return true
	default:
		return false
	}
}

// --- request-scoped idempotency plumbing ---
//
// Generate's shared signature carries no idempotency fields, so the HTTP
// facade passes the external key + canonical body via context. Preview
// ignores it (no record); draft/sandbox ignore it (optional support —
// currently no record); publish requires it (428 when absent).

type ctxKey string

const (
	actorCtxKey ctxKey = "generation-actor"
	idemCtxKey  ctxKey = "generation-idempotency"
)

// IdempotencyParams is the external live-changing precondition (§21).
type IdempotencyParams struct {
	Owner, Method, Path, Key string
	Body                     []byte
}

// WithActor stores the actor in ctx for handler → service plumbing.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorCtxKey, a)
}

// ActorFrom reads the actor from ctx.
func ActorFrom(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(actorCtxKey).(Actor)
	return a, ok
}

// WithIdempotency stores the external idempotency params in ctx.
func WithIdempotency(ctx context.Context, p IdempotencyParams) context.Context {
	return context.WithValue(ctx, idemCtxKey, p)
}

// IdempotencyFrom reads the idempotency params from ctx.
func IdempotencyFrom(ctx context.Context) (IdempotencyParams, bool) {
	p, ok := ctx.Value(idemCtxKey).(IdempotencyParams)
	if !ok || p.Key == "" {
		return IdempotencyParams{}, false
	}
	return p, true
}
