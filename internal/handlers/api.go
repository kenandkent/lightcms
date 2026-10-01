package handlers

import (
	"encoding/json"
	"io"
	"log"
	"net/http"

	"github.com/jonradoff/lightcms/v7/internal/auth"
	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/generation"
	"github.com/jonradoff/lightcms/v7/internal/product/idempotency"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/services"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// APIHandler handles REST API endpoints (JSON-only, no sessions/templates)
type APIHandler struct {
	contentService      *services.ContentService
	templateService     *services.TemplateService
	assetService        *services.AssetService
	settingsService     *services.SettingsService
	apiKeyService       *services.APIKeyService
	searchService       *services.SearchService
	auditService        *services.AuditService
	snippetService      *services.SnippetService
	forkService         *services.ForkService
	importService       *services.ImportService
	webhookService      *services.WebhookService
	lockService         *services.LockService
	linkCheckerService  *services.LinkCheckerService
	commentService      *services.CommentService
	approvalService     *services.ApprovalService
	userService         *services.UserService
	agentSessionService *services.AgentSessionService
	maintenanceService  *services.MaintenanceService
	// Task 16C: shared publication runtime (wired in main.go; nil in unit
	// tests preserves legacy behavior). When set, single/batch publish,
	// rollback and unpublish route through PublicationService with
	// Idempotency-Key handling; old URLs return Publication IDs and no raw
	// GenerateStaticPage call occurs.
	publicationService *publication.Service
	idempotencyService *idempotency.Service
	generationService  *generation.Service
}

// SetPublicationRuntime wires the shared V3 publication runtime (Task 16C/E).
func (a *APIHandler) SetPublicationRuntime(pubs *publication.Service, idem *idempotency.Service, gen *generation.Service) {
	a.publicationService = pubs
	a.idempotencyService = idem
	a.generationService = gen
}

// SetCommentService wires in the comment service.
func (a *APIHandler) SetCommentService(cs *services.CommentService) { a.commentService = cs }

// SetApprovalService wires in the approval service.
func (a *APIHandler) SetApprovalService(as *services.ApprovalService) { a.approvalService = as }

// SetUserService wires in the user service (used for approver ID validation).
func (a *APIHandler) SetUserService(us *services.UserService) { a.userService = us }

// NewAPIHandler creates a new API handler
func NewAPIHandler(
	contentService *services.ContentService,
	templateService *services.TemplateService,
	assetService *services.AssetService,
	settingsService *services.SettingsService,
	apiKeyService *services.APIKeyService,
	auditService *services.AuditService,
	snippetService *services.SnippetService,
) *APIHandler {
	return &APIHandler{
		contentService:  contentService,
		templateService: templateService,
		assetService:    assetService,
		settingsService: settingsService,
		apiKeyService:   apiKeyService,
		auditService:    auditService,
		snippetService:  snippetService,
	}
}

// getAPIUser extracts the authenticated user from an API request context.
// Returns nil if no user is set (e.g., legacy/system key without user association).
func (a *APIHandler) getAPIUser(r *http.Request) *auth.SessionUser {
	user, _ := auth.UserFromAPIContext(r.Context())
	return user
}

// requirePermission checks if the authenticated API user has the required permission.
// Returns true if allowed, false if denied (and writes a 403 response).
// API keys without an associated user are rejected — legacy system keys must be
// re-created with a user owner via the admin UI or API.
func (a *APIHandler) requirePermission(w http.ResponseWriter, r *http.Request, perm string) bool {
	user := a.getAPIUser(r)
	if user == nil {
		log.Printf("[security] API request denied: key has no user context (perm=%s, path=%s)", perm, r.URL.Path)
		a.jsonErrorCode(w, http.StatusForbidden, "PERMISSION_DENIED", "API key must be associated with a user — legacy keys without user context are no longer supported")
		return false
	}
	if !auth.UserHasPermission(user, perm) {
		msg := "insufficient permissions"
		if user.SandboxOnly && auth.HasPermission(user.Role, perm) {
			msg = "this API key is sandbox-only: live mutations are not permitted — work inside a fork and submit it for human review"
		}
		a.jsonErrorCode(w, http.StatusForbidden, "PERMISSION_DENIED", msg)
		return false
	}
	return true
}

// auditLog fires an async audit log entry for the current API user
func (a *APIHandler) auditLog(r *http.Request, action, resource, resourceID string, details map[string]interface{}) {
	user := a.getAPIUser(r)
	entry := models.AuditLog{
		Action:       action,
		Resource:     resource,
		ResourceID:   resourceID,
		Details:      details,
		AgentSession: r.Header.Get("X-Agent-Session"),
	}
	if user != nil {
		if oid, err := primitive.ObjectIDFromHex(user.ID); err == nil {
			entry.UserID = oid
		}
		entry.UserEmail = user.Email
		entry.ViaAPI = user.ViaAPIKey
	}
	a.auditService.LogAsync(entry)
}

// jsonResponse writes a JSON response with the given status code
func (a *APIHandler) jsonResponse(w http.ResponseWriter, statusCode int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(data)
}

// jsonError writes a JSON error response. The body is the legacy additive
// envelope: {"error": "<message>", "code": "<CODE>"} — "error" stays a string
// for existing clients; the sibling "code" is derived from the status when the
// caller has nothing more specific to say.
func (a *APIHandler) jsonError(w http.ResponseWriter, statusCode int, message string) {
	a.jsonErrorCode(w, statusCode, defaultErrorCode(statusCode), message)
}

// jsonErrorCode writes a JSON error response with an explicit machine-readable
// code alongside the string message, e.g.
// {"error": "Idempotency-Key is required for publish",
//  "code": "IDEMPOTENCY_KEY_REQUIRED"}.
// Additive only: no field was removed or re-nested, so legacy parsers that read
// "error" as a string keep working, while agents/MCP can branch on "code".
func (a *APIHandler) jsonErrorCode(w http.ResponseWriter, statusCode int, code, message string) {
	if code == "" {
		code = defaultErrorCode(statusCode)
	}
	a.jsonResponse(w, statusCode, map[string]interface{}{
		"error": message,
		"code":  code,
	})
}

// defaultErrorCode maps an HTTP status to the machine-readable code emitted by
// jsonError when the caller does not pass an explicit one. 428 is deliberately
// absent: only the caller knows whether the missing precondition is the
// Idempotency-Key (IDEMPOTENCY_KEY_REQUIRED) or something else, so those sites
// pass the code explicitly. Unmapped statuses fall back to "ERROR".
func defaultErrorCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "INVALID_REQUEST"
	case http.StatusUnauthorized:
		return "UNAUTHENTICATED"
	case http.StatusForbidden:
		return "PERMISSION_DENIED"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusConflict:
		return "CONFLICT"
	case http.StatusUnprocessableEntity:
		return "VALIDATION_FAILED"
	case http.StatusTooManyRequests:
		return "RATE_LIMITED"
	case http.StatusInternalServerError:
		return "INTERNAL_ERROR"
	case http.StatusServiceUnavailable:
		return "SERVICE_UNAVAILABLE"
	default:
		return "ERROR"
	}
}

// decodeJSON reads and decodes the request body into the given target
func (a *APIHandler) decodeJSON(r *http.Request, target interface{}) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20)) // 10MB limit
	if err != nil {
		return err
	}
	return json.Unmarshal(body, target)
}
