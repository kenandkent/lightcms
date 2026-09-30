// Field validation for immutable template contracts (plan Task 4, Lane A;
// spec §8.4, §11). This file implements the shared validator used by Admin,
// REST/MCP generation, and publish paths:
//
//	func ValidateData(v TemplateVersion, data map[string]any) ([]FieldError, []FieldWarning)
//
// Contract notes (spec §11.3, MVP warnings policy):
//   - Only errors block create/publish. The publish gate is len(errors) > 0;
//     warnings NEVER block. Security, required, type, template-parse, and
//     resource-reference failures are always errors, never warnings.
//   - The validator reads the immutable TemplateVersion only — never the
//     mutable Template record (spec §9.6, §10.1).
//   - The validator is pure: no database, no network, no filesystem. Asset
//     existence/type checks happen at publish/render time; here an image value
//     gets a syntactic reference check only (no traversal, no control bytes).
//   - Request payload size (5 MiB data cap, spec §20.7) is enforced by the
//     generation layer (Task 12), not here.
//   - JSON null is rejected for every MVP type (spec §20.7: null only where a
//     field schema explicitly allows nullable; MVP has no such field).
//   - Empty string is an explicit value (spec §20.7): it is subject to
//     required validation, and for optional string-family fields it counts as
//     "not missing" (no further checks run on it).
//   - A missing field with a materializable Default does NOT error — not even
//     when required. The caller materializes the default (see fieldDefault)
//     and a FIELD_DEFAULT_APPLIED warning records what was applied. This is
//     how "optional boolean missing stays missing, default=true materializes
//     true" (spec §8.4) is communicated through a diagnostics-only signature.
package templatecontract

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jonradoff/lightcms/v7/internal/models"
)

// Field-level diagnostic codes. The top-level publish/generation error is
// FIELD_VALIDATION_FAILED (spec §27); these codes are the per-field details.
const (
	CodeFieldRequired       = "FIELD_REQUIRED"
	CodeFieldUnknown        = "FIELD_UNKNOWN"
	CodeFieldNull           = "FIELD_NULL_NOT_ALLOWED"
	CodeFieldType           = "FIELD_TYPE_MISMATCH"
	CodeFieldTooShort       = "FIELD_TOO_SHORT"
	CodeFieldTooLong        = "FIELD_TOO_LONG"
	CodeFieldPattern        = "FIELD_PATTERN_MISMATCH"
	CodeFieldPatternInvalid = "FIELD_PATTERN_INVALID"
	CodeFieldTooSmall       = "FIELD_TOO_SMALL"
	CodeFieldTooLarge       = "FIELD_TOO_LARGE"
	CodeFieldInvalidURL     = "FIELD_INVALID_URL"
	CodeFieldProtocol       = "FIELD_PROTOCOL_NOT_ALLOWED"
	CodeFieldEnum           = "FIELD_NOT_IN_ENUM"
	CodeFieldDate           = "FIELD_INVALID_DATE"
	CodeFieldAsset          = "FIELD_INVALID_ASSET"
	// CodeFieldDefaultApplied is the only warning code in MVP. It is
	// informational: the caller fills data[field] with the field default and
	// proceeds. It never blocks publish.
	CodeFieldDefaultApplied = "FIELD_DEFAULT_APPLIED"
)

// defaultURLProtocols applies when a url field sets no AllowedProtocols.
var defaultURLProtocols = []string{"http", "https"}

// dateLayout is the ISO 8601 calendar date (spec §8.4). It is NOT an RFC 3339
// datetime: time.Parse rejects nonexistent dates (2026-02-30), datetimes,
// and wrong shapes.
const dateLayout = "2006-01-02"

// ValidateData checks data against the immutable version v and returns
// blocking errors plus non-blocking warnings. Publish is blocked iff
// len(errors) > 0. Output order is deterministic: known fields follow the
// version's field definition order, unknown fields are sorted by name.
func ValidateData(v TemplateVersion, data map[string]any) ([]FieldError, []FieldWarning) {
	var errs []FieldError
	var warns []FieldWarning

	byName := make(map[string]models.TemplateField, len(v.Fields))
	for _, f := range v.Fields {
		byName[f.Name] = f
	}

	// Unknown data fields first (sorted for determinism). Unknown fields are
	// 422 errors (spec §20.7).
	var unknown []string
	for name := range data {
		if _, ok := byName[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(unknown)
	for _, name := range unknown {
		errs = append(errs, FieldError{
			Code:    CodeFieldUnknown,
			Field:   name,
			Message: fmt.Sprintf("field %q is not defined by template %q", name, v.Slug),
		})
	}

	// Known fields in definition order.
	for _, f := range v.Fields {
		val, present := data[f.Name]
		if !present {
			if def, ok := fieldDefault(f); ok {
				warns = append(warns, FieldWarning{
					Code:    CodeFieldDefaultApplied,
					Field:   f.Name,
					Message: fmt.Sprintf("field %q not provided; default %v will be applied", f.Name, def),
				})
				continue
			}
			if f.Required {
				errs = append(errs, FieldError{
					Code:    CodeFieldRequired,
					Field:   f.Name,
					Message: fmt.Sprintf("field %q is required", f.Name),
				})
			}
			continue
		}
		errs = append(errs, validateFieldValue(v.Slug, f, val)...)
	}
	return errs, warns
}

// fieldDefault reports the materializable default for a missing field.
// String-family types (text, textarea, richtext, markdown, rawhtml, date,
// image, select, url) materialize Default verbatim when non-empty. Number
// materializes Default when it parses as a float. Boolean materializes only
// Default "true" (spec §8.4): Default "false"/"" leaves the key unwritten so
// the store keeps distinguishing "missing" from "explicit false".
func fieldDefault(f models.TemplateField) (any, bool) {
	switch f.Type {
	case "boolean":
		if b, err := strconv.ParseBool(strings.TrimSpace(f.Default)); err == nil && b {
			return true, true
		}
		return nil, false
	case "number":
		if s := strings.TrimSpace(f.Default); s != "" {
			if n, err := strconv.ParseFloat(s, 64); err == nil {
				return n, true
			}
		}
		return nil, false
	default:
		if f.Default != "" {
			return f.Default, true
		}
		return nil, false
	}
}

// validateFieldValue checks one present value. A nil value is always a
// FIELD_NULL_NOT_ALLOWED error (MVP has no nullable fields).
func validateFieldValue(slug string, f models.TemplateField, val any) []FieldError {
	if val == nil {
		return []FieldError{{
			Code:    CodeFieldNull,
			Field:   f.Name,
			Message: fmt.Sprintf("field %q must not be null", f.Name),
		}}
	}
	switch f.Type {
	case "text", "textarea", "richtext", "markdown", "rawhtml",
		"date", "image", "select", "url":
		return validateStringValue(slug, f, val)
	case "number":
		return validateNumberValue(f, val)
	case "boolean":
		return validateBooleanValue(f, val)
	default:
		return []FieldError{{
			Code:    CodeFieldType,
			Field:   f.Name,
			Message: fmt.Sprintf("field %q has unsupported type %q", f.Name, f.Type),
		}}
	}
}

// validateStringValue enforces string type, required/empty, length/pattern
// constraints, and the per-type domain check (enum, date, url, asset ref).
func validateStringValue(_ string, f models.TemplateField, val any) []FieldError {
	s, ok := val.(string)
	if !ok {
		return []FieldError{{
			Code:    CodeFieldType,
			Field:   f.Name,
			Message: fmt.Sprintf("field %q must be a string, got %T", f.Name, val),
		}}
	}
	if s == "" {
		if f.Required {
			return []FieldError{{
				Code:    CodeFieldRequired,
				Field:   f.Name,
				Message: fmt.Sprintf("field %q is required", f.Name),
			}}
		}
		return nil
	}
	var errs []FieldError
	n := utf8.RuneCountInString(s) // user-visible length, not bytes
	if f.Validation.MinLength != nil && n < *f.Validation.MinLength {
		errs = append(errs, FieldError{
			Code:    CodeFieldTooShort,
			Field:   f.Name,
			Message: fmt.Sprintf("field %q is %d characters, minimum is %d", f.Name, n, *f.Validation.MinLength),
		})
	}
	if f.Validation.MaxLength != nil && n > *f.Validation.MaxLength {
		errs = append(errs, FieldError{
			Code:    CodeFieldTooLong,
			Field:   f.Name,
			Message: fmt.Sprintf("field %q is %d characters, maximum is %d", f.Name, n, *f.Validation.MaxLength),
		})
	}
	if f.Validation.Pattern != "" {
		re, err := regexp.Compile(f.Validation.Pattern)
		if err != nil {
			// Template authoring bug: fail closed so publish cannot proceed
			// on an unenforceable constraint.
			errs = append(errs, FieldError{
				Code:    CodeFieldPatternInvalid,
				Field:   f.Name,
				Message: fmt.Sprintf("field %q has an invalid pattern %q", f.Name, f.Validation.Pattern),
			})
		} else if !re.MatchString(s) {
			errs = append(errs, FieldError{
				Code:    CodeFieldPattern,
				Field:   f.Name,
				Message: fmt.Sprintf("field %q does not match pattern %q", f.Name, f.Validation.Pattern),
			})
		}
	}
	switch f.Type {
	case "select":
		if e := checkEnum(f, s); e != nil {
			errs = append(errs, *e)
		}
	case "date":
		if _, err := time.Parse(dateLayout, s); err != nil {
			errs = append(errs, FieldError{
				Code:    CodeFieldDate,
				Field:   f.Name,
				Message: fmt.Sprintf("field %q must be an ISO 8601 calendar date (YYYY-MM-DD), got %q", f.Name, s),
			})
		}
	case "url":
		if e := checkURL(f, s); e != nil {
			errs = append(errs, *e)
		}
	case "image":
		if e := checkAssetRef(f, s); e != nil {
			errs = append(errs, *e)
		}
	}
	return errs
}

// parseSelectOptions splits a select field's comma-separated Options, trims
// whitespace, and drops empties. Matching is exact and case-sensitive.
func parseSelectOptions(options string) []string {
	var out []string
	for _, o := range strings.Split(options, ",") {
		if o = strings.TrimSpace(o); o != "" {
			out = append(out, o)
		}
	}
	return out
}

func checkEnum(f models.TemplateField, s string) *FieldError {
	for _, o := range parseSelectOptions(f.Options) {
		if s == o {
			return nil
		}
	}
	return &FieldError{
		Code:    CodeFieldEnum,
		Field:   f.Name,
		Message: fmt.Sprintf("field %q must be one of [%s], got %q", f.Name, strings.Join(parseSelectOptions(f.Options), ", "), s),
	}
}

// normalizeProtocols lowercases schemes and strips trailing colons so authors
// may write "https" or "https:" interchangeably.
func normalizeProtocols(in []string) []string {
	var out []string
	for _, p := range in {
		p = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(p), ":")))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func checkURL(f models.TemplateField, s string) *FieldError {
	u, err := url.Parse(s)
	if err != nil {
		return &FieldError{
			Code:    CodeFieldInvalidURL,
			Field:   f.Name,
			Message: fmt.Sprintf("field %q is not a valid URL: %v", f.Name, err),
		}
	}
	allowed := normalizeProtocols(f.Validation.AllowedProtocols)
	if len(allowed) == 0 {
		allowed = defaultURLProtocols
	}
	// An empty scheme means the value is not an absolute URL at all
	// (relative path) — that is an invalid URL, not a protocol violation.
	if u.Scheme == "" {
		return &FieldError{
			Code:    CodeFieldInvalidURL,
			Field:   f.Name,
			Message: fmt.Sprintf("field %q must be an absolute URL with a host, got %q", f.Name, s),
		}
	}
	scheme := strings.ToLower(u.Scheme)
	ok := false
	for _, a := range allowed {
		if scheme == a {
			ok = true
			break
		}
	}
	if !ok {
		return &FieldError{
			Code:    CodeFieldProtocol,
			Field:   f.Name,
			Message: fmt.Sprintf("field %q uses protocol %q, allowed: [%s]", f.Name, u.Scheme, strings.Join(allowed, ", ")),
		}
	}
	// Scheme passed but the URL is not absolute (relative path, or a bare
	// "scheme:opaque" value like javascript:… without a host).
	if !u.IsAbs() || u.Host == "" {
		return &FieldError{
			Code:    CodeFieldInvalidURL,
			Field:   f.Name,
			Message: fmt.Sprintf("field %q must be an absolute URL with a host, got %q", f.Name, s),
		}
	}
	return nil
}

// checkAssetRef is the syntactic half of image validation. Existence and
// MIME/type checks need storage access, which this pure validator does not
// have; those run at publish/render time.
func checkAssetRef(f models.TemplateField, s string) *FieldError {
	invalid := func(reason string) *FieldError {
		return &FieldError{
			Code:    CodeFieldAsset,
			Field:   f.Name,
			Message: fmt.Sprintf("field %q is not a valid asset reference: %s", f.Name, reason),
		}
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == ".." {
			return invalid("path traversal is not allowed")
		}
	}
	for _, r := range s {
		if r == 0 || unicode.IsControl(r) {
			return invalid("control characters are not allowed")
		}
	}
	if s != strings.TrimSpace(s) {
		return invalid("leading/trailing whitespace is not allowed")
	}
	return nil
}

// validateNumberValue accepts every JSON-plausible numeric Go type
// (encoding/json decodes to float64; BSON/apiclients may hand int variants or
// json.Number). Strings and bools are type errors — no silent coercion.
func validateNumberValue(f models.TemplateField, val any) []FieldError {
	var n float64
	switch t := val.(type) {
	case float64:
		n = t
	case float32:
		n = float64(t)
	case int:
		n = float64(t)
	case int8:
		n = float64(t)
	case int16:
		n = float64(t)
	case int32:
		n = float64(t)
	case int64:
		n = float64(t)
	case uint:
		n = float64(t)
	case uint8:
		n = float64(t)
	case uint16:
		n = float64(t)
	case uint32:
		n = float64(t)
	case uint64:
		n = float64(t)
	case json.Number:
		parsed, err := t.Float64()
		if err != nil {
			return []FieldError{{
				Code:    CodeFieldType,
				Field:   f.Name,
				Message: fmt.Sprintf("field %q must be a number, got %q", f.Name, t.String()),
			}}
		}
		n = parsed
	default:
		return []FieldError{{
			Code:    CodeFieldType,
			Field:   f.Name,
			Message: fmt.Sprintf("field %q must be a number, got %T", f.Name, val),
		}}
	}
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return []FieldError{{
			Code:    CodeFieldType,
			Field:   f.Name,
			Message: fmt.Sprintf("field %q must be a finite number", f.Name),
		}}
	}
	var errs []FieldError
	if f.Validation.Min != nil && n < *f.Validation.Min {
		errs = append(errs, FieldError{
			Code:    CodeFieldTooSmall,
			Field:   f.Name,
			Message: fmt.Sprintf("field %q is %v, minimum is %v", f.Name, n, *f.Validation.Min),
		})
	}
	if f.Validation.Max != nil && n > *f.Validation.Max {
		errs = append(errs, FieldError{
			Code:    CodeFieldTooLarge,
			Field:   f.Name,
			Message: fmt.Sprintf("field %q is %v, maximum is %v", f.Name, n, *f.Validation.Max),
		})
	}
	return errs
}

func validateBooleanValue(f models.TemplateField, val any) []FieldError {
	if _, ok := val.(bool); !ok {
		return []FieldError{{
			Code:    CodeFieldType,
			Field:   f.Name,
			Message: fmt.Sprintf("field %q must be a boolean, got %T", f.Name, val),
		}}
	}
	return nil
}
