package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/jonradoff/lightcms/v7/internal/product/generation"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// schemaResponse is the GET /api/v1/templates/{slug}/schema shape (§10.2).
type schemaResponse struct {
	Template        string         `json:"template"`
	TemplateVersion int64          `json:"template_version"`
	Fields          []schemaField  `json:"fields"`
	JSONSchema      map[string]any `json:"json_schema"`
}

type schemaField struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Required    bool   `json:"required"`
	Description string `json:"description,omitempty"`
	Example     string `json:"example,omitempty"`
}

// HandleTemplateSchema is GET /api/v1/templates/{slug}/schema.
// Template version, ETag and JSON Schema come from the SAME immutable
// TemplateVersion object (§10.1).
func (h *Handlers) HandleTemplateSchema(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actorOf(w, r)
	if !ok {
		return
	}
	if !actor.Can(generation.ScopeTemplateView) {
		WriteError(w, r, &generation.Error{Code: generation.CodePermissionDenied, Message: "missing required scope template.view"})
		return
	}
	slug := vars(r, "slug")
	if slug == "" {
		slug = strings.TrimPrefix(r.URL.Path, "/api/v1/templates/")
		slug = strings.TrimSuffix(slug, "/schema")
	}
	if slug == "" {
		WriteError(w, r, &generation.Error{Code: generation.CodeTemplateNotFound, Message: "template slug is required"})
		return
	}
	// Templates service is reached via Gen's repo helper; expose it through
	// the generation service to keep httpapi free of extra wiring.
	tv, rawSchema, err := h.Gen.SchemaForSlug(r.Context(), slug)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var doc map[string]any
	if err := json.Unmarshal(rawSchema, &doc); err != nil {
		WriteError(w, r, &generation.Error{Code: generation.CodeTemplateSchemaInvalid, Message: "cannot render template schema"})
		return
	}
	fields := make([]schemaField, 0, len(tv.Fields))
	for _, f := range tv.Fields {
		fields = append(fields, schemaField{
			Name: f.Name, Type: f.Type, Required: f.Required,
			Description: f.Description, Example: f.Example,
		})
	}
	w.Header().Set("ETag", fmt.Sprintf(`"template-version-%d"`, tv.Version))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(schemaResponse{
		Template: tv.Slug, TemplateVersion: tv.Version, Fields: fields, JSONSchema: doc,
	})
}

// HandleMigrateSlug is POST /api/v1/templates/{id}/migrate-slug (admin-only).
func (h *Handlers) HandleMigrateSlug(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actorOf(w, r)
	if !ok {
		return
	}
	idHex := vars(r, "id")
	if idHex == "" {
		WriteError(w, r, &generation.Error{Code: generation.CodeInvalidRequest, Message: "template id is required"})
		return
	}
	id, err := primitive.ObjectIDFromHex(idHex)
	if err != nil {
		WriteError(w, r, &generation.Error{Code: generation.CodeInvalidRequest, Message: "invalid template id"})
		return
	}
	var body struct {
		NewSlug *string `json:"new_slug"`
		Slug    *string `json:"slug"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	newSlug := ""
	if body.NewSlug != nil {
		newSlug = *body.NewSlug
	} else if body.Slug != nil {
		newSlug = *body.Slug
	}
	if strings.TrimSpace(newSlug) == "" {
		WriteError(w, r, &generation.Error{Code: generation.CodeFieldValidationFailed, Message: "new_slug is required",
			Details: []generation.FieldDetail{{Code: "FIELD_REQUIRED", Field: "new_slug", Message: "new_slug is required"}}})
		return
	}
	res, err := h.Gen.MigrateSlug(r.Context(), actor, id, strings.TrimSpace(newSlug))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, res)
}

// HandleUpgradePreview is GET /api/v1/templates/{slug}/upgrade-preview.
func (h *Handlers) HandleUpgradePreview(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actorOf(w, r)
	if !ok {
		return
	}
	slug := vars(r, "slug")
	if slug == "" {
		WriteError(w, r, &generation.Error{Code: generation.CodeInvalidRequest, Message: "template slug is required"})
		return
	}
	preview, err := h.Gen.PreviewUpgrade(r.Context(), actor, slug)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, preview)
}

// HandleStartUpgradeJob is POST /api/v1/templates/{slug}/upgrade-jobs.
func (h *Handlers) HandleStartUpgradeJob(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actorOf(w, r)
	if !ok {
		return
	}
	slug := vars(r, "slug")
	if slug == "" {
		WriteError(w, r, &generation.Error{Code: generation.CodeInvalidRequest, Message: "template slug is required"})
		return
	}
	job, err := h.Gen.StartUpgradeJob(r.Context(), actor, slug)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, job)
}

// HandleGetUpgradeJob is GET /api/v1/templates/upgrade-jobs/{job_id}.
func (h *Handlers) HandleGetUpgradeJob(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actorOf(w, r)
	if !ok {
		return
	}
	idHex := vars(r, "job_id")
	if idHex == "" {
		idHex = vars(r, "id")
	}
	id, err := primitive.ObjectIDFromHex(idHex)
	if err != nil {
		WriteError(w, r, &generation.Error{Code: generation.CodeInvalidRequest, Message: "invalid job id"})
		return
	}
	job, err := h.Gen.GetUpgradeJob(r.Context(), actor, id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, job)
}

// HandleRunUpgradeJob is POST /api/v1/templates/upgrade-jobs/{job_id}/run.
func (h *Handlers) HandleRunUpgradeJob(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actorOf(w, r)
	if !ok {
		return
	}
	idHex := vars(r, "job_id")
	if idHex == "" {
		idHex = vars(r, "id")
	}
	id, err := primitive.ObjectIDFromHex(idHex)
	if err != nil {
		WriteError(w, r, &generation.Error{Code: generation.CodeInvalidRequest, Message: "invalid job id"})
		return
	}
	job, err := h.Gen.RunUpgradeJob(r.Context(), actor, id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, job)
}

var _ = templatecontract.CodeNotFound
