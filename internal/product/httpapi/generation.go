package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/jonradoff/lightcms/v7/internal/observe"
	"github.com/jonradoff/lightcms/v7/internal/product/generation"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Handlers wires the generation facade to net/http. ActorExtractor and
// IdempotencyExtractor adapt the existing /api/v1 middleware (Task 16 wiring);
// tests inject fakes. When nil, ActorExtractor rejects as 401.
type Handlers struct {
	Gen *generation.Service
	// ActorExtractor returns the authenticated actor for r.
	ActorExtractor func(r *http.Request) (generation.Actor, error)
	// IdempotencyExtractor returns (key, hasKey) for live-changing commands.
	IdempotencyExtractor func(r *http.Request) (string, bool)
}

// actorOf resolves the actor or writes 401.
func (h *Handlers) actorOf(w http.ResponseWriter, r *http.Request) (generation.Actor, bool) {
	if h.ActorExtractor != nil {
		a, err := h.ActorExtractor(r)
		if err != nil {
			WriteError(w, r, &generation.Error{Code: generation.CodeUnauthenticated, Message: "authentication is required"})
			return generation.Actor{}, false
		}
		if !a.Authenticated {
			WriteError(w, r, &generation.Error{Code: generation.CodeUnauthenticated, Message: "authentication is required"})
			return generation.Actor{}, false
		}
		return a, true
	}
	if a, ok := generation.ActorFrom(r.Context()); ok && a.Authenticated {
		return a, true
	}
	WriteError(w, r, &generation.Error{Code: generation.CodeUnauthenticated, Message: "authentication is required"})
	return generation.Actor{}, false
}

// Allowed top-level generation fields (§20.7: unknown top-level → 422).
var allowedGenerationFields = map[string]struct{}{
	"template": {}, "expected_template_version": {}, "title": {}, "slug": {},
	"folder_path": {}, "data": {}, "mode": {}, "upsert": {},
}

// GenerateRequestWire is the HTTP body shape (pointers detect omission).
type GenerateRequestWire struct {
	Template                *string        `json:"template"`
	ExpectedTemplateVersion *int64         `json:"expected_template_version"`
	Title                   *string        `json:"title"`
	Slug                    *string        `json:"slug"`
	FolderPath              *string        `json:"folder_path"`
	Data                    map[string]any `json:"data"`
	Mode                    *string        `json:"mode"`
	Upsert                  *bool          `json:"upsert"`
	// capture unknown top-levels via Raw map in the handler.
}

// HandleGenerate is POST /api/v1/page-generation.
func (h *Handlers) HandleGenerate(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actorOf(w, r)
	if !ok {
		return
	}
	// Task 16F: generation request counter + duration log.
	t0 := time.Now()
	observe.Default().IncGenerationRequests()
	logGen := func(template string, resp *generation.GenerateResponse, status int, code string) {
		f := observe.Fields{
			RequestID: requestID(r), Actor: actor.ActorKind,
			UserID: actor.Owner(), AgentSession: actor.AgentSession,
			TemplateSlug: template, DurationMS: time.Since(t0).Milliseconds(),
			StatusCode: status, ErrorCode: code, Stage: "generate",
		}
		if resp != nil {
			f.ContentID = resp.ID
			f.ContentVersion = resp.ContentVersion
			f.TemplateVersion = resp.TemplateVersion
			f.FullPath = resp.FullPath
			if resp.PublicationID != nil {
				f.PublicationID = *resp.PublicationID
			}
		}
		if code != "" {
			observe.Default().IncGenerationErrors()
		}
		observe.LogGeneration("request", f)
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, 10<<20) // existing 10 MiB body limit
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		WriteError(w, r, &generation.Error{Code: generation.CodeInvalidRequest, Message: "cannot read request body"})
		return
	}
	if len(raw) == 0 {
		WriteError(w, r, &generation.Error{Code: generation.CodeInvalidRequest, Message: "empty request body"})
		return
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		WriteError(w, r, &generation.Error{Code: generation.CodeInvalidRequest, Message: "invalid JSON body"})
		return
	}
	for k := range top {
		if _, ok := allowedGenerationFields[k]; !ok {
			WriteError(w, r, &generation.Error{
				Code: generation.CodeFieldValidationFailed, Message: "unknown request field",
				Details: []generation.FieldDetail{{Code: "FIELD_UNKNOWN", Field: k, Message: "unknown request field " + k}},
			})
			return
		}
	}
	var wire GenerateRequestWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		WriteError(w, r, &generation.Error{Code: generation.CodeInvalidRequest, Message: "invalid JSON body"})
		return
	}
	req := generation.GenerateRequest{Upsert: false}
	if wire.Template != nil {
		req.Template = *wire.Template
	}
	if wire.Title != nil {
		req.Title = *wire.Title
	}
	if wire.Slug != nil {
		req.Slug = *wire.Slug
	}
	if wire.FolderPath != nil {
		req.FolderPath = *wire.FolderPath
	}
	if wire.Mode != nil {
		req.Mode = *wire.Mode
	} else {
		req.Mode = generation.ModeDraft
	}
	if wire.Upsert != nil {
		req.Upsert = *wire.Upsert
	}
	req.ExpectedTemplateVersion = wire.ExpectedTemplateVersion
	if wire.Data != nil {
		req.Data = wire.Data
	} else {
		// Distinguish omitted vs explicit null: explicit null is a 422
		// (MVP has no nullable fields).
		if rawHasNullData(top) {
			WriteError(w, r, &generation.Error{
				Code: generation.CodeFieldValidationFailed, Message: "data must not be null",
				Details: []generation.FieldDetail{{Code: "FIELD_NULL_NOT_ALLOWED", Field: "data", Message: "data must not be null"}},
			})
			return
		}
		req.Data = map[string]any{}
	}

	// Idempotency plumbing for live-changing commands (preview ignores it).
	ctx := r.Context()
	ctx = generation.WithActor(ctx, actor)
	if strings.ToLower(strings.TrimSpace(req.Mode)) == generation.ModePublish {
		key := ""
		has := false
		if h.IdempotencyExtractor != nil {
			key, has = h.IdempotencyExtractor(r)
		} else if v, ok := generation.IdempotencyFrom(ctx); ok {
			key, has = v.Key, true
		} else {
			key = r.Header.Get("Idempotency-Key")
			has = strings.TrimSpace(key) != ""
		}
		if has {
			ctx = generation.WithIdempotency(ctx, generation.IdempotencyParams{
				Owner: actor.Owner(), Method: r.Method, Path: r.URL.Path, Key: key, Body: raw,
			})
		}
		// Absent key → service returns 428 (zero mutation). Do not fabricate.
	}
	resp, err := h.Gen.Generate(ctx, actor, req)
	if err != nil {
		logGen(req.Template, nil, generation.StatusForCode(generation.CodeOf(err)), generation.CodeOf(err))
		WriteError(w, r, err)
		return
	}
	status := 200
	switch {
	case resp.Mode == generation.ModePreview:
		status = 200
	case resp.Action == "created":
		status = 201
	default:
		status = 200
	}
	logGen(req.Template, &resp, status, "")
	WriteJSON(w, status, generationResponseWire(resp))
}

func rawHasNullData(top map[string]json.RawMessage) bool {
	raw, ok := top["data"]
	if !ok {
		return false
	}
	return strings.TrimSpace(string(raw)) == "null"
}

// generationResponseWire renders the exact §20.4 shape (requires_publish
// always present).
func generationResponseWire(resp generation.GenerateResponse) map[string]any {
	return map[string]any{
		"id": resp.ID, "action": resp.Action, "mode": resp.Mode,
		"published": resp.Published, "requires_publish": resp.RequiresPublish,
		"content_version": resp.ContentVersion, "template": resp.Template,
		"template_version": resp.TemplateVersion, "publication_id": resp.PublicationID,
		"full_path": resp.FullPath, "public_url": resp.PublicURL,
		"warnings": resp.Warnings,
	}
}

// HandleGenerateMethodNotAllowed is a helper for wiring (Task 16).
func vars(r *http.Request, key string) string {
	if v := mux.Vars(r)[key]; v != "" {
		return v
	}
	return ""
}

var _ = primitive.ObjectID{}
var _ = templatecontract.CodeNotFound
