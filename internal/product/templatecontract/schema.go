// JSON Schema adapter for immutable template contracts (plan Task 4, Lane A;
// spec §10). ToJSONSchema is a pure, deterministic transform of one frozen
// TemplateVersion: it never reads the mutable Template record, never touches
// the database, and returns byte-for-byte identical output for equal versions
// (encoding/json sorts object keys; the required array follows field
// definition order).
//
// The schema describes the publish-time payload shape accepted by
// ValidateData in this same package: required lists fields that must be
// present (fields with a materializable default are fillable server-side and
// are therefore NOT required), additionalProperties is always false, date
// fields carry format:date, and select options become enum.
package templatecontract

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/jonradoff/lightcms/v7/internal/models"
)

// CodeSchemaInvalid (spec §27 TEMPLATE_SCHEMA_INVALID) is returned when a
// version contains a field type the schema adapter cannot express — e.g. a
// Phase 2 group/repeatable field, which Task 3's ValidateFields already
// rejects at write time.
const CodeSchemaInvalid = "TEMPLATE_SCHEMA_INVALID"

// schemaDialect is the JSON Schema dialect marker (spec §10.2 example).
const schemaDialect = "https://json-schema.org/draft/2020-12/schema"

// ToJSONSchema renders the deterministic JSON Schema document for v.
func ToJSONSchema(v TemplateVersion) ([]byte, error) {
	props := make(map[string]any, len(v.Fields))
	required := make([]string, 0, len(v.Fields))
	for _, f := range v.Fields {
		prop, err := fieldSchema(f)
		if err != nil {
			return nil, err
		}
		props[f.Name] = prop
		// A field with a materializable default validates clean when
		// missing (ValidateData emits FIELD_DEFAULT_APPLIED), so the
		// schema must not demand it.
		if f.Required {
			if _, ok := fieldDefault(f); !ok {
				required = append(required, f.Name)
			}
		}
	}
	doc := map[string]any{
		"$schema":              schemaDialect,
		"title":                v.Slug,
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, internalErr("marshal JSON schema", err)
	}
	return raw, nil
}

// applyStringConstraints copies MinLength/MaxLength/Pattern into schema
// keywords. They mirror what ValidateData enforces for the same field.
func applyStringConstraints(prop map[string]any, fv models.FieldValidation) {
	if fv.MinLength != nil {
		prop["minLength"] = *fv.MinLength
	}
	if fv.MaxLength != nil {
		prop["maxLength"] = *fv.MaxLength
	}
	if fv.Pattern != "" {
		prop["pattern"] = fv.Pattern
	}
}

// fieldSchema maps one template field to its JSON Schema property. Unknown
// types fail with TEMPLATE_SCHEMA_INVALID rather than emitting a vacuous
// permissive schema that would hide a contract bug.
func fieldSchema(f models.TemplateField) (map[string]any, error) {
	prop := map[string]any{}
	if f.Description != "" {
		prop["description"] = f.Description
	}
	if f.Example != "" {
		prop["examples"] = []string{f.Example}
	}
	switch f.Type {
	case "text", "textarea", "richtext", "markdown", "rawhtml", "date", "image":
		prop["type"] = "string"
		if f.Type == "date" {
			prop["format"] = "date"
		}
		applyStringConstraints(prop, f.Validation)
		if f.Default != "" {
			prop["default"] = f.Default
		}
	case "select":
		prop["type"] = "string"
		if opts := parseSelectOptions(f.Options); len(opts) > 0 {
			prop["enum"] = opts
		}
		applyStringConstraints(prop, f.Validation)
		if f.Default != "" {
			prop["default"] = f.Default
		}
	case "url":
		prop["type"] = "string"
		prop["format"] = "uri"
		applyStringConstraints(prop, f.Validation)
		if f.Default != "" {
			prop["default"] = f.Default
		}
	case "number":
		prop["type"] = "number"
		if f.Validation.Min != nil {
			prop["minimum"] = *f.Validation.Min
		}
		if f.Validation.Max != nil {
			prop["maximum"] = *f.Validation.Max
		}
		if s := strings.TrimSpace(f.Default); s != "" {
			if n, err := strconv.ParseFloat(s, 64); err == nil {
				prop["default"] = n
			}
		}
	case "boolean":
		prop["type"] = "boolean"
		if s := strings.TrimSpace(f.Default); s != "" {
			if b, err := strconv.ParseBool(s); err == nil {
				prop["default"] = b
			}
		}
	default:
		return nil, &Error{
			Code:    CodeSchemaInvalid,
			Message: fmt.Sprintf("field %q has unsupported type %q", f.Name, f.Type),
		}
	}
	return prop, nil
}
