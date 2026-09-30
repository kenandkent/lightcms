// Package templatecontract implements the LightCMS V3 template slug, version,
// and field contract (plan Task 3, Lane A; spec §8–§9, ADR-002).
//
// Design summary (spec §9.6):
//   - Template slug is the unique, immutable machine identifier:
//     lowercase [a-z0-9][a-z0-9-_]{0,63}, unique, immutable on ordinary update.
//   - Each effective contract change creates an immutable TemplateVersion
//     document; readers (renderer, schema endpoint, validator) always read the
//     immutable version, never the mutable Template record.
//   - Versioning is decided by ContractHash only. Re-saving identical contract
//     content creates NO new version. RenderHash covers the rendering-affecting
//     subset (description-only changes keep it stable) and serves render
//     caching/comparison only.
//   - Version numbers come from CAS-incrementing templates.current_version
//     inside one Mongo transaction; concurrent losers get
//     TEMPLATE_VERSION_CONFLICT and no duplicate version document is created.
//
// File layout (plan Task 3): model.go (this file: TemplateVersion,
// TemplateInput, FieldError/FieldWarning, hashing, field/status validation),
// slug.go (slug validation), repository.go (Mongo finders, no nested
// transactions), service.go (Create/Update/GetCurrent/GetVersion).
// ValidateData/ToJSONSchema belong to Task 4 (validate.go/schema.go) and are
// intentionally NOT implemented here.
package templatecontract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/models"

	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// Error codes. Spec-defined codes (§27) are reused where they exist
// (TEMPLATE_NOT_FOUND, TEMPLATE_VERSION_NOT_FOUND, TEMPLATE_VERSION_CONFLICT);
// slug/input codes are package-level and map to 422/409 in Task 12.
const (
	CodeNotFound        = "TEMPLATE_NOT_FOUND"
	CodeVersionNotFound = "TEMPLATE_VERSION_NOT_FOUND"
	CodeVersionConflict = "TEMPLATE_VERSION_CONFLICT"
	CodeSlugInvalid     = "TEMPLATE_SLUG_INVALID"
	CodeSlugConflict    = "TEMPLATE_SLUG_CONFLICT"
	CodeSlugImmutable   = "TEMPLATE_SLUG_IMMUTABLE"
	CodeInputInvalid    = "TEMPLATE_INPUT_INVALID"
	CodeInternal        = "INTERNAL_ERROR"
)

// Error is a typed template-contract failure.
type Error struct {
	Code    string
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("templatecontract %s: %s: %v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("templatecontract %s: %s", e.Code, e.Message)
}

// Unwrap exposes the underlying cause.
func (e *Error) Unwrap() error { return e.Err }

// CodeOf maps any error to a template-contract code. Typed errors yield their
// code; nil yields ""; anything else yields INTERNAL_ERROR.
func CodeOf(err error) string {
	if err == nil {
		return ""
	}
	var te *Error
	if errors.As(err, &te) {
		return te.Code
	}
	return CodeInternal
}

func inputInvalid(format string, args ...any) *Error {
	return &Error{Code: CodeInputInvalid, Message: fmt.Sprintf(format, args...)}
}

func internalErr(op string, err error) *Error {
	return &Error{Code: CodeInternal, Message: op, Err: err}
}

func versionConflict(format string, args ...any) *Error {
	return &Error{Code: CodeVersionConflict, Message: fmt.Sprintf(format, args...)}
}

// IsDuplicateKey reports Mongo duplicate-key errors (code 11000).
func IsDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	if mongo.IsDuplicateKeyError(err) {
		return true
	}
	return strings.Contains(err.Error(), "duplicate key")
}

// TemplateVersion is the immutable template snapshot (spec §9.6). One document
// per effective contract change; never updated after insert.
type TemplateVersion struct {
	ID             primitive.ObjectID     `bson:"_id,omitempty" json:"id"`
	TemplateID     primitive.ObjectID     `bson:"template_id" json:"template_id"`
	Version        int64                  `bson:"version" json:"version"`
	Slug           string                 `bson:"slug" json:"slug"`
	Name           string                 `bson:"name" json:"name"`
	Category       string                 `bson:"category" json:"category"`
	Status         string                 `bson:"status" json:"status"`
	Fields         []models.TemplateField `bson:"fields" json:"fields"`
	HTMLLayout     string                 `bson:"html_layout" json:"html_layout"`
	ScriptPolicy   string                 `bson:"script_policy,omitempty" json:"script_policy,omitempty"`
	ContractHash   string                 `bson:"contract_hash" json:"contract_hash"`
	RenderHash     string                 `bson:"render_hash" json:"render_hash"`
	CreatedBy      *primitive.ObjectID    `bson:"created_by,omitempty" json:"created_by,omitempty"`
	CreatedByEmail string                 `bson:"created_by_email,omitempty" json:"created_by_email,omitempty"`
	CreatedAt      time.Time              `bson:"created_at" json:"created_at"`
}

// Input reconstructs the TemplateInput view of a version (e.g. for hash
// comparison or convergence checks after a conflict retry).
func (v TemplateVersion) Input() TemplateInput {
	return TemplateInput{
		Slug:         v.Slug,
		Name:         v.Name,
		Category:     v.Category,
		Status:       v.Status,
		HTMLLayout:   v.HTMLLayout,
		ScriptPolicy: v.ScriptPolicy,
		Fields:       v.Fields,
	}
}

// TemplateInput is the mutable write surface for Create/Update (plan Shared
// interface contract). Note there is no Description: display-name changes are
// display-only and never bump the version (spec §9.3). ScriptPolicy has no
// home on the mutable models.Template record, so it lives in version
// documents only; the mutable record stays a pointer plus display cache.
type TemplateInput struct {
	Slug, Name, Category, Status, HTMLLayout, ScriptPolicy string
	Fields                                                 []models.TemplateField
}

// FieldError and FieldWarning are the shared validation diagnostics (plan
// Shared interface contract). Produced by Task 4's ValidateData; declared
// here so both tasks share one definition.
type FieldError struct{ Code, Field, Message string }

// FieldWarning is a non-blocking validation diagnostic.
type FieldWarning struct{ Code, Field, Message string }

// SystemVars is the renderer-injected system-variable contract (spec §8.6).
// It is part of both hashes so a system-variable change is a contract change.
var SystemVars = []string{
	"title", "slug", "full_path", "published_at", "public_url",
	"content_id", "template_slug", "template_version", "publication_id",
}

// --- canonical hashing (spec §9.6) ---

// Both hashes use RFC 8785-style canonical JSON: UTF-8, fixed struct field
// order (no maps, so no key-sorting ambiguity), no insignificant whitespace,
// arrays keep definition order. Hash format is "sha256:<hex>" (§9.2 example).
// ContractHash decides versioning; RenderHash is render cache/comparison only.

type hashValidation struct {
	MinLength        *int     `json:"min_length,omitempty"`
	MaxLength        *int     `json:"max_length,omitempty"`
	Pattern          string   `json:"pattern,omitempty"`
	Min              *float64 `json:"min,omitempty"`
	Max              *float64 `json:"max,omitempty"`
	AllowedProtocols []string `json:"allowed_protocols,omitempty"`
	MaxItems         *int     `json:"max_items,omitempty"`
}

func isZeroValidation(v models.FieldValidation) bool {
	return v.MinLength == nil && v.MaxLength == nil && v.Pattern == "" &&
		v.Min == nil && v.Max == nil && len(v.AllowedProtocols) == 0 && v.MaxItems == nil
}

func toHashValidation(v models.FieldValidation) *hashValidation {
	if isZeroValidation(v) {
		return nil
	}
	return &hashValidation{
		MinLength:        v.MinLength,
		MaxLength:        v.MaxLength,
		Pattern:          v.Pattern,
		Min:              v.Min,
		Max:              v.Max,
		AllowedProtocols: v.AllowedProtocols,
		MaxItems:         v.MaxItems,
	}
}

// hashField is the full field contract: every attribute that changes what
// integrations accept or render, including display metadata (description,
// example, label) and validation constraints.
type hashField struct {
	Name        string          `json:"name"`
	Label       string          `json:"label,omitempty"`
	Type        string          `json:"type"`
	Required    bool            `json:"required,omitempty"`
	Placeholder string          `json:"placeholder,omitempty"`
	Options     string          `json:"options,omitempty"`
	Default     string          `json:"default,omitempty"`
	Description string          `json:"description,omitempty"`
	Example     string          `json:"example,omitempty"`
	Validation  *hashValidation `json:"validation,omitempty"`
}

// renderField is the rendering-affecting subset: field identity, value domain
// and defaults. Display metadata (label, placeholder, description, example)
// and validation constraints do not change HTML bytes for a given value and
// are excluded, so description-only edits keep RenderHash stable.
type renderField struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required,omitempty"`
	Options  string `json:"options,omitempty"`
	Default  string `json:"default,omitempty"`
}

type contractDoc struct {
	Slug         string      `json:"slug"`
	Category     string      `json:"category,omitempty"`
	Status       string      `json:"status"`
	Fields       []hashField `json:"fields"`
	HTMLLayout   string      `json:"html_layout,omitempty"`
	ScriptPolicy string      `json:"script_policy,omitempty"`
	SystemVars   []string    `json:"system_vars"`
}

type renderDoc struct {
	Fields       []renderField `json:"fields"`
	HTMLLayout   string        `json:"html_layout,omitempty"`
	ScriptPolicy string        `json:"script_policy,omitempty"`
	SystemVars   []string      `json:"system_vars"`
}

func hashJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		// Struct marshaling cannot fail for these shapes; guard anyway.
		raw = []byte("{}")
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ContractHash computes the version-deciding hash over slug, category,
// status, full field metadata/validation/default/options, HTML layout,
// script policy and the system-variable contract (spec §9.6). Name is display
// only and excluded, so name-only edits never bump the version (§9.3).
func ContractHash(in TemplateInput) string {
	fields := make([]hashField, 0, len(in.Fields))
	for _, f := range in.Fields {
		fields = append(fields, hashField{
			Name:        f.Name,
			Label:       f.Label,
			Type:        f.Type,
			Required:    f.Required,
			Placeholder: f.Placeholder,
			Options:     f.Options,
			Default:     f.Default,
			Description: f.Description,
			Example:     f.Example,
			Validation:  toHashValidation(f.Validation),
		})
	}
	return hashJSON(contractDoc{
		Slug:         in.Slug,
		Category:     in.Category,
		Status:       in.Status,
		Fields:       fields,
		HTMLLayout:   in.HTMLLayout,
		ScriptPolicy: in.ScriptPolicy,
		SystemVars:   SystemVars,
	})
}

// RenderHash computes the render-cache hash over the rendering-affecting
// field subset, HTML layout, script policy and render dependency declarations.
// It must NOT decide versioning.
func RenderHash(in TemplateInput) string {
	fields := make([]renderField, 0, len(in.Fields))
	for _, f := range in.Fields {
		fields = append(fields, renderField{
			Name:     f.Name,
			Type:     f.Type,
			Required: f.Required,
			Options:  f.Options,
			Default:  f.Default,
		})
	}
	return hashJSON(renderDoc{
		Fields:       fields,
		HTMLLayout:   in.HTMLLayout,
		ScriptPolicy: in.ScriptPolicy,
		SystemVars:   SystemVars,
	})
}

// --- template status (spec §8.3) ---

const (
	StatusDraft      = "draft"
	StatusActive     = "active"
	StatusDeprecated = "deprecated"
)

// NormalizeStatus maps "" (legacy records without a status) to draft.
func NormalizeStatus(s string) string {
	if s == "" {
		return StatusDraft
	}
	return s
}

// IsValidStatus reports whether s is a known template status.
func IsValidStatus(s string) bool {
	switch NormalizeStatus(s) {
	case StatusDraft, StatusActive, StatusDeprecated:
		return true
	}
	return false
}

// ValidateStatusTransition enforces the status lifecycle: draft → active →
// deprecated, with draft → deprecated allowed as a shortcut. Deprecated is
// terminal (history stays traceable; reactivation needs a new template),
// and there is no path back to draft. Empty (legacy) counts as draft.
func ValidateStatusTransition(from, to string) error {
	f, t := NormalizeStatus(from), NormalizeStatus(to)
	if !IsValidStatus(f) || !IsValidStatus(t) {
		return inputInvalid("unknown template status transition %q -> %q", from, to)
	}
	if f == t {
		return nil
	}
	switch f {
	case StatusDraft:
		if t == StatusActive || t == StatusDeprecated {
			return nil
		}
	case StatusActive:
		if t == StatusDeprecated {
			return nil
		}
	case StatusDeprecated:
		// Terminal: no exits.
	}
	return inputInvalid("template status transition %q -> %q is not allowed", f, t)
}

// --- fields (spec §8.4–§8.5) ---

// SupportedFieldTypes is the MVP field-type set (spec §8.4). group/repeatable
// are Phase 2 and rejected here so contracts cannot promise them.
var SupportedFieldTypes = map[string]struct{}{
	"text": {}, "textarea": {}, "richtext": {}, "markdown": {},
	"rawhtml": {}, "date": {}, "image": {}, "select": {},
	"url": {}, "number": {}, "boolean": {},
}

// IsSupportedFieldType reports whether t is an MVP field type.
func IsSupportedFieldType(t string) bool {
	_, ok := SupportedFieldTypes[t]
	return ok
}

// ReservedFieldNames are system-injected names user fields must not claim
// (spec §8.5). The list is exactly the spec list — no more, no less.
var ReservedFieldNames = map[string]struct{}{
	"id": {}, "template_id": {}, "template_version": {}, "full_path": {},
	"published": {}, "published_at": {}, "created_at": {}, "updated_at": {},
	"publication_id": {}, "public_url": {},
}

// IsReservedFieldName reports whether name collides with a system field.
func IsReservedFieldName(name string) bool {
	_, ok := ReservedFieldNames[name]
	return ok
}

var fieldNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// ValidateFieldName enforces [a-z][a-z0-9_]{0,63} plus the reserved list.
func ValidateFieldName(name string) error {
	if !fieldNamePattern.MatchString(name) {
		return inputInvalid("invalid field name %q: must match [a-z][a-z0-9_]{0,63}", name)
	}
	if IsReservedFieldName(name) {
		return inputInvalid("field name %q collides with a reserved system field", name)
	}
	return nil
}

// ValidateFields checks field names (pattern, reserved, duplicate) and types.
func ValidateFields(fields []models.TemplateField) error {
	seen := make(map[string]struct{}, len(fields))
	for i, f := range fields {
		if err := ValidateFieldName(f.Name); err != nil {
			return fmt.Errorf("fields[%d]: %w", i, err)
		}
		if _, dup := seen[f.Name]; dup {
			return inputInvalid("duplicate field name %q", f.Name)
		}
		seen[f.Name] = struct{}{}
		if !IsSupportedFieldType(f.Type) {
			return inputInvalid("field %q has unsupported type %q", f.Name, f.Type)
		}
	}
	return nil
}

// SupportedScriptPolicies: "" inherits the site default; otherwise one of the
// site markdown_script_policy values (all | admin_only | none).
var SupportedScriptPolicies = map[string]struct{}{
	"": {}, "all": {}, "admin_only": {}, "none": {},
}

// ValidateScriptPolicy rejects unknown script policies.
func ValidateScriptPolicy(p string) error {
	if _, ok := SupportedScriptPolicies[p]; !ok {
		return inputInvalid("unsupported script policy %q", p)
	}
	return nil
}
