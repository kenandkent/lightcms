package templatecontract

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/jonradoff/lightcms/v7/internal/models"
)

func schemaVersion() TemplateVersion {
	return validationVersion()
}

func decodeSchema(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("ToJSONSchema output is not valid JSON: %v", err)
	}
	return out
}

func schemaProps(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	out := decodeSchema(t, raw)
	props, ok := out["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema missing properties object: %v", out)
	}
	return props
}

func TestJSONSchemaStable(t *testing.T) {
	v := schemaVersion()
	first, err := ToJSONSchema(v)
	if err != nil {
		t.Fatalf("ToJSONSchema: %v", err)
	}
	second, err := ToJSONSchema(v)
	if err != nil {
		t.Fatalf("ToJSONSchema: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("same version must produce byte-for-byte identical schema")
	}
	// An independently constructed equal version must produce identical bytes.
	clone := schemaVersion()
	third, err := ToJSONSchema(clone)
	if err != nil {
		t.Fatalf("ToJSONSchema: %v", err)
	}
	if !bytes.Equal(first, third) {
		t.Fatal("equal versions must produce byte-for-byte identical schema")
	}
}

func TestJSONSchemaShape(t *testing.T) {
	raw, err := ToJSONSchema(schemaVersion())
	if err != nil {
		t.Fatalf("ToJSONSchema: %v", err)
	}
	out := decodeSchema(t, raw)

	if out["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		t.Fatalf("missing $schema draft marker: %v", out["$schema"])
	}
	if out["type"] != "object" {
		t.Fatalf("top-level type must be object: %v", out["type"])
	}
	if addl, ok := out["additionalProperties"].(bool); !ok || addl {
		t.Fatalf("additionalProperties must be false: %v", out["additionalProperties"])
	}

	required := map[string]bool{}
	if req, ok := out["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				required[s] = true
			}
		}
	} else {
		t.Fatalf("schema missing required array: %v", out)
	}
	for _, f := range []string{"headline", "body", "pubdate", "hero", "category", "link", "breaking"} {
		if !required[f] {
			t.Errorf("required must contain %q, got %v", f, out["required"])
		}
	}
	// Fields with materializable defaults are fillable by the server and must
	// NOT be listed as required (they validate clean when missing).
	for _, f := range []string{"subtitle", "sponsored"} {
		if required[f] {
			t.Errorf("defaulted field %q must not be required, got %v", f, out["required"])
		}
	}

	props := schemaProps(t, raw)

	t.Run("text carries constraints", func(t *testing.T) {
		h := props["headline"].(map[string]any)
		if h["type"] != "string" {
			t.Fatalf("headline type: %v", h)
		}
		if h["minLength"] != json.Number("3") || h["maxLength"] != json.Number("10") {
			t.Fatalf("headline length constraints: %v", h)
		}
	})
	t.Run("pattern carried", func(t *testing.T) {
		s := props["slugline"].(map[string]any)
		if s["pattern"] != `^[a-z0-9-]+$` {
			t.Fatalf("slugline pattern: %v", s)
		}
	})
	t.Run("date has format date", func(t *testing.T) {
		d := props["pubdate"].(map[string]any)
		if d["type"] != "string" || d["format"] != "date" {
			t.Fatalf("pubdate: %v", d)
		}
	})
	t.Run("select has enum", func(t *testing.T) {
		c := props["category"].(map[string]any)
		if c["type"] != "string" {
			t.Fatalf("category type: %v", c)
		}
		enum, ok := c["enum"].([]any)
		if !ok || len(enum) != 3 || enum[0] != "news" || enum[1] != "sports" || enum[2] != "tech" {
			t.Fatalf("category enum: %v", c)
		}
	})
	t.Run("url has format uri", func(t *testing.T) {
		u := props["link"].(map[string]any)
		if u["type"] != "string" || u["format"] != "uri" {
			t.Fatalf("link: %v", u)
		}
	})
	t.Run("number carries min max", func(t *testing.T) {
		n := props["rating"].(map[string]any)
		if n["type"] != "number" {
			t.Fatalf("rating type: %v", n)
		}
		if n["minimum"] != json.Number("1") || n["maximum"] != json.Number("5") {
			t.Fatalf("rating range: %v", n)
		}
	})
	t.Run("boolean default true", func(t *testing.T) {
		b := props["sponsored"].(map[string]any)
		if b["type"] != "boolean" {
			t.Fatalf("sponsored type: %v", b)
		}
		if def, ok := b["default"].(bool); !ok || !def {
			t.Fatalf("sponsored default: %v", b)
		}
	})
	t.Run("string default carried", func(t *testing.T) {
		s := props["subtitle"].(map[string]any)
		if s["default"] != "Untitled" {
			t.Fatalf("subtitle default: %v", s)
		}
	})
	t.Run("descriptions and examples", func(t *testing.T) {
		v := TemplateVersion{Slug: "x", Fields: []models.TemplateField{{
			Name: "headline", Label: "Headline", Type: "text", Required: true,
			Description: "Primary headline displayed at the top of the article",
			Example:     "Bitcoin Rallies as Institutional Demand Returns",
		}}}
		props := schemaProps(t, mustSchema(t, v))
		h := props["headline"].(map[string]any)
		if h["description"] != "Primary headline displayed at the top of the article" {
			t.Fatalf("description: %v", h)
		}
		ex, ok := h["examples"].([]any)
		if !ok || len(ex) != 1 || ex[0] != "Bitcoin Rallies as Institutional Demand Returns" {
			t.Fatalf("examples: %v", h)
		}
	})
	t.Run("richtext markdown rawhtml textarea image are strings", func(t *testing.T) {
		for _, f := range []string{"summary", "body", "notes", "embed", "hero"} {
			p := props[f].(map[string]any)
			if p["type"] != "string" {
				t.Errorf("field %q type: %v, want string", f, p)
			}
		}
	})
}

func mustSchema(t *testing.T, v TemplateVersion) []byte {
	t.Helper()
	raw, err := ToJSONSchema(v)
	if err != nil {
		t.Fatalf("ToJSONSchema: %v", err)
	}
	return raw
}

func TestJSONSchemaUnknownType(t *testing.T) {
	v := TemplateVersion{Slug: "x", Fields: []models.TemplateField{
		{Name: "g", Type: "group"},
	}}
	_, err := ToJSONSchema(v)
	if err == nil {
		t.Fatal("unsupported field type must fail schema generation")
	}
	if CodeOf(err) != CodeSchemaInvalid {
		t.Fatalf("want TEMPLATE_SCHEMA_INVALID, got %v (%v)", CodeOf(err), err)
	}
}

func TestJSONSchemaFromImmutableVersion(t *testing.T) {
	// The schema must come from the frozen TemplateVersion: two versions of
	// the same template with different fields yield different schemas.
	base := schemaVersion()
	altered := schemaVersion()
	altered.Fields = append([]models.TemplateField{}, base.Fields...)
	altered.Fields = altered.Fields[:len(altered.Fields)-1]
	a := mustSchema(t, base)
	b := mustSchema(t, altered)
	if bytes.Equal(a, b) {
		t.Fatal("different immutable versions must yield different schemas")
	}
}

func TestJSONSchemaValidatesValidPayload(t *testing.T) {
	// Cross-check: every field name in the valid payload exists in the schema
	// properties, and required names are present — the schema describes exactly
	// what ValidateData accepts (spec §10.1: pure deterministic transform of
	// the same immutable version).
	raw := mustSchema(t, schemaVersion())
	props := schemaProps(t, raw)
	for name := range validPayload() {
		if _, ok := props[name]; !ok {
			t.Errorf("payload field %q missing from schema properties", name)
		}
	}
	errs, _ := ValidateData(schemaVersion(), validPayload())
	if len(errs) != 0 {
		t.Fatalf("schema-covered payload must validate clean, got %+v", errs)
	}
}
