package templatecontract

import (
	"testing"

	"github.com/jonradoff/lightcms/v7/internal/models"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func floatPtr(f float64) *float64 { return &f }

// validationVersion builds an immutable TemplateVersion exercising every MVP
// field type (spec §8.4) plus the constraint surface of models.FieldValidation.
func validationVersion() TemplateVersion {
	tid := primitive.NewObjectID()
	return TemplateVersion{
		ID:         primitive.NewObjectID(),
		TemplateID: tid,
		Version:    3,
		Slug:       "field-validation-test",
		Name:       "Field Validation Test",
		Category:   "news",
		Status:     StatusActive,
		Fields: []models.TemplateField{
			{Name: "headline", Label: "Headline", Type: "text", Required: true,
				Validation: models.FieldValidation{MinLength: intPtr(3), MaxLength: intPtr(10)}},
			{Name: "slugline", Label: "Slugline", Type: "text",
				Validation: models.FieldValidation{Pattern: `^[a-z0-9-]+$`}},
			{Name: "summary", Label: "Summary", Type: "textarea",
				Validation: models.FieldValidation{MaxLength: intPtr(20)}},
			{Name: "body", Label: "Body", Type: "richtext", Required: true},
			{Name: "notes", Label: "Notes", Type: "markdown"},
			{Name: "embed", Label: "Embed", Type: "rawhtml"},
			{Name: "pubdate", Label: "Publish date", Type: "date", Required: true},
			{Name: "hero", Label: "Hero image", Type: "image", Required: true},
			{Name: "thumb", Label: "Thumbnail", Type: "image"},
			{Name: "category", Label: "Category", Type: "select", Required: true,
				Options: "news, sports, tech"},
			{Name: "link", Label: "Link", Type: "url", Required: true},
			{Name: "docs", Label: "Docs", Type: "url",
				Validation: models.FieldValidation{AllowedProtocols: []string{"https"}}},
			{Name: "rating", Label: "Rating", Type: "number",
				Validation: models.FieldValidation{Min: floatPtr(1), Max: floatPtr(5)}},
			{Name: "featured", Label: "Featured", Type: "boolean"},
			{Name: "breaking", Label: "Breaking", Type: "boolean", Required: true},
			{Name: "subtitle", Label: "Subtitle", Type: "text", Default: "Untitled"},
			{Name: "sponsored", Label: "Sponsored", Type: "boolean", Default: "true"},
		},
		HTMLLayout:   "<h1>{{.headline}}</h1>",
		ScriptPolicy: "all",
	}
}

// validPayload satisfies every required field of validationVersion.
func validPayload() map[string]any {
	return map[string]any{
		"headline":  "Hello",
		"slugline":  "hello-world",
		"summary":   "short",
		"body":      "<p>rich</p>",
		"notes":     "# md",
		"embed":     "<div>raw</div>",
		"pubdate":   "2026-09-30",
		"hero":      "/assets/hero.png",
		"thumb":     "",
		"category":  "news",
		"link":      "https://example.com/a",
		"docs":      "https://docs.example.com/x",
		"rating":    4.0,
		"featured":  false,
		"breaking":  true,
		"subtitle":  "Given",
		"sponsored": true,
	}
}

func errorCodes(errs []FieldError) map[string]string {
	out := make(map[string]string, len(errs))
	for _, e := range errs {
		out[e.Field] = e.Code
	}
	return out
}

func TestValidateDataValid(t *testing.T) {
	errs, warns := ValidateData(validationVersion(), validPayload())
	if len(errs) != 0 {
		t.Fatalf("valid payload: got %d errors: %+v", len(errs), errs)
	}
	if len(warns) != 0 {
		t.Fatalf("valid payload with all defaults supplied: got %d warnings: %+v", len(warns), warns)
	}
}

func TestValidateDataFieldTypes(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(map[string]any)
		field   string
		wantErr bool
	}{
		{"text ok", func(d map[string]any) { d["headline"] = "Hello" }, "headline", false},
		{"text wrong type", func(d map[string]any) { d["headline"] = 42.0 }, "headline", true},
		{"textarea wrong type", func(d map[string]any) { d["summary"] = true }, "summary", true},
		{"richtext wrong type", func(d map[string]any) { d["body"] = 1.0 }, "body", true},
		{"markdown wrong type", func(d map[string]any) { d["notes"] = false }, "notes", true},
		{"rawhtml wrong type", func(d map[string]any) { d["embed"] = 7.0 }, "embed", true},
		{"date wrong type", func(d map[string]any) { d["pubdate"] = 20260930.0 }, "pubdate", true},
		{"image wrong type", func(d map[string]any) { d["hero"] = true }, "hero", true},
		{"select wrong type", func(d map[string]any) { d["category"] = 1.0 }, "category", true},
		{"url wrong type", func(d map[string]any) { d["link"] = false }, "link", true},
		{"number ok int", func(d map[string]any) { d["rating"] = 3 }, "rating", false},
		{"number ok float", func(d map[string]any) { d["rating"] = 2.5 }, "rating", false},
		{"number string rejected", func(d map[string]any) { d["rating"] = "4" }, "rating", true},
		{"number bool rejected", func(d map[string]any) { d["rating"] = true }, "rating", true},
		{"boolean true ok", func(d map[string]any) { d["featured"] = true }, "featured", false},
		{"boolean false ok", func(d map[string]any) { d["featured"] = false }, "featured", false},
		{"boolean string rejected", func(d map[string]any) { d["featured"] = "false" }, "featured", true},
		{"boolean number rejected", func(d map[string]any) { d["featured"] = 1 }, "featured", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := validPayload()
			tc.mutate(d)
			errs, _ := ValidateData(validationVersion(), d)
			codes := errorCodes(errs)
			_, has := codes[tc.field]
			if tc.wantErr && !has {
				t.Fatalf("expected error on field %q, got %+v", tc.field, errs)
			}
			if !tc.wantErr && has {
				t.Fatalf("unexpected error on field %q: %+v", tc.field, errs)
			}
		})
	}
}

func TestValidateDataUnknownFields(t *testing.T) {
	d := validPayload()
	d["mystery"] = "x"
	d["Title"] = "wrong case"
	errs, _ := ValidateData(validationVersion(), d)
	codes := errorCodes(errs)
	if codes["mystery"] != CodeFieldUnknown {
		t.Fatalf("unknown field: got codes %+v, want FIELD_UNKNOWN on mystery", codes)
	}
	if codes["Title"] != CodeFieldUnknown {
		t.Fatalf("case-mismatched field name must be unknown: %+v", codes)
	}
}

func TestValidateDataRequired(t *testing.T) {
	t.Run("missing required text", func(t *testing.T) {
		d := validPayload()
		delete(d, "headline")
		errs, _ := ValidateData(validationVersion(), d)
		if errorCodes(errs)["headline"] != CodeFieldRequired {
			t.Fatalf("got %+v, want FIELD_REQUIRED on headline", errs)
		}
	})
	t.Run("empty string fails required", func(t *testing.T) {
		d := validPayload()
		d["headline"] = ""
		errs, _ := ValidateData(validationVersion(), d)
		if errorCodes(errs)["headline"] != CodeFieldRequired {
			t.Fatalf("got %+v, want FIELD_REQUIRED on empty headline", errs)
		}
	})
	t.Run("missing required number", func(t *testing.T) {
		v := validationVersion()
		v.Fields = append(v.Fields, models.TemplateField{Name: "reqnum", Type: "number", Required: true})
		errs, _ := ValidateData(v, map[string]any{})
		if errorCodes(errs)["reqnum"] != CodeFieldRequired {
			t.Fatalf("got %+v, want FIELD_REQUIRED on reqnum", errs)
		}
	})
	t.Run("zero satisfies required number", func(t *testing.T) {
		v := validationVersion()
		v.Fields = append(v.Fields, models.TemplateField{Name: "reqnum", Type: "number", Required: true})
		d := validPayload()
		d["reqnum"] = 0.0
		errs, _ := ValidateData(v, d)
		if len(errs) != 0 {
			t.Fatalf("zero must satisfy required number, got %+v", errs)
		}
	})
	t.Run("missing required boolean", func(t *testing.T) {
		d := validPayload()
		delete(d, "breaking")
		errs, _ := ValidateData(validationVersion(), d)
		if errorCodes(errs)["breaking"] != CodeFieldRequired {
			t.Fatalf("got %+v, want FIELD_REQUIRED on breaking", errs)
		}
	})
	t.Run("explicit false satisfies required boolean", func(t *testing.T) {
		d := validPayload()
		d["breaking"] = false
		errs, _ := ValidateData(validationVersion(), d)
		if len(errs) != 0 {
			t.Fatalf("explicit false must satisfy required boolean, got %+v", errs)
		}
	})
	t.Run("required with default materializes instead of error", func(t *testing.T) {
		v := TemplateVersion{Slug: "x", Fields: []models.TemplateField{
			{Name: "nick", Type: "text", Required: true, Default: "anon"},
		}}
		errs, warns := ValidateData(v, map[string]any{})
		if len(errs) != 0 {
			t.Fatalf("required-with-default must not error, got %+v", errs)
		}
		if len(warns) != 1 || warns[0].Code != CodeFieldDefaultApplied || warns[0].Field != "nick" {
			t.Fatalf("want one FIELD_DEFAULT_APPLIED warning, got %+v", warns)
		}
	})
}

func TestValidateDataNull(t *testing.T) {
	d := validPayload()
	for _, f := range []string{"headline", "pubdate", "rating", "featured", "category", "hero", "link"} {
		d[f] = nil
	}
	errs, _ := ValidateData(validationVersion(), d)
	codes := errorCodes(errs)
	for _, f := range []string{"headline", "pubdate", "rating", "featured", "category", "hero", "link"} {
		if codes[f] != CodeFieldNull {
			t.Errorf("field %q=null: got %q, want FIELD_NULL_NOT_ALLOWED", f, codes[f])
		}
	}
}

func TestValidateDataDefaults(t *testing.T) {
	t.Run("missing text default warns without error", func(t *testing.T) {
		d := validPayload()
		delete(d, "subtitle")
		errs, warns := ValidateData(validationVersion(), d)
		if len(errs) != 0 {
			t.Fatalf("got errors %+v", errs)
		}
		found := false
		for _, w := range warns {
			if w.Field == "subtitle" && w.Code == CodeFieldDefaultApplied {
				found = true
			}
		}
		if !found {
			t.Fatalf("want FIELD_DEFAULT_APPLIED warning for subtitle, got %+v", warns)
		}
	})
	t.Run("boolean default=true materializes true", func(t *testing.T) {
		d := validPayload()
		delete(d, "sponsored")
		errs, warns := ValidateData(validationVersion(), d)
		if len(errs) != 0 {
			t.Fatalf("got errors %+v", errs)
		}
		found := false
		for _, w := range warns {
			if w.Field == "sponsored" && w.Code == CodeFieldDefaultApplied {
				found = true
			}
		}
		if !found {
			t.Fatalf("want FIELD_DEFAULT_APPLIED warning for sponsored, got %+v", warns)
		}
	})
	t.Run("optional boolean missing stays missing", func(t *testing.T) {
		d := validPayload()
		delete(d, "featured")
		errs, warns := ValidateData(validationVersion(), d)
		if len(errs) != 0 {
			t.Fatalf("got errors %+v", errs)
		}
		for _, w := range warns {
			if w.Field == "featured" {
				t.Fatalf("no-default boolean must produce no warning, got %+v", warns)
			}
		}
	})
	t.Run("optional field without default stays silent", func(t *testing.T) {
		d := validPayload()
		delete(d, "summary")
		delete(d, "thumb")
		errs, warns := ValidateData(validationVersion(), d)
		if len(errs) != 0 {
			t.Fatalf("got errors %+v", errs)
		}
		for _, w := range warns {
			if w.Field == "summary" || w.Field == "thumb" {
				t.Fatalf("unexpected warning for defaultless optional, got %+v", warns)
			}
		}
	})
}

func TestValidateDataMinMaxLength(t *testing.T) {
	t.Run("too short", func(t *testing.T) {
		d := validPayload()
		d["headline"] = "Hi"
		errs, _ := ValidateData(validationVersion(), d)
		if errorCodes(errs)["headline"] != CodeFieldTooShort {
			t.Fatalf("got %+v, want FIELD_TOO_SHORT", errs)
		}
	})
	t.Run("too long", func(t *testing.T) {
		d := validPayload()
		d["headline"] = "01234567890"
		errs, _ := ValidateData(validationVersion(), d)
		if errorCodes(errs)["headline"] != CodeFieldTooLong {
			t.Fatalf("got %+v, want FIELD_TOO_LONG", errs)
		}
	})
	t.Run("boundary inclusive", func(t *testing.T) {
		d := validPayload()
		d["headline"] = "abc"
		if errs, _ := ValidateData(validationVersion(), d); len(errs) != 0 {
			t.Fatalf("min boundary must pass, got %+v", errs)
		}
		d["headline"] = "0123456789"
		if errs, _ := ValidateData(validationVersion(), d); len(errs) != 0 {
			t.Fatalf("max boundary must pass, got %+v", errs)
		}
	})
	t.Run("length counts runes not bytes", func(t *testing.T) {
		v := TemplateVersion{Slug: "x", Fields: []models.TemplateField{
			{Name: "t", Type: "text", Validation: models.FieldValidation{MaxLength: intPtr(3)}},
		}}
		// 3 runes, 9 bytes — must pass under rune counting.
		if errs, _ := ValidateData(v, map[string]any{"t": "日本語"}); len(errs) != 0 {
			t.Fatalf("rune counting: got %+v", errs)
		}
		if errs, _ := ValidateData(v, map[string]any{"t": "日本語X"}); errorCodes(errs)["t"] != CodeFieldTooLong {
			t.Fatalf("4 runes must fail maxLength=3, got %+v", errs)
		}
	})
}

func TestValidateDataPattern(t *testing.T) {
	t.Run("mismatch", func(t *testing.T) {
		d := validPayload()
		d["slugline"] = "Has Spaces!"
		errs, _ := ValidateData(validationVersion(), d)
		if errorCodes(errs)["slugline"] != CodeFieldPattern {
			t.Fatalf("got %+v, want FIELD_PATTERN_MISMATCH", errs)
		}
	})
	t.Run("match", func(t *testing.T) {
		d := validPayload()
		d["slugline"] = "ok-123"
		if errs, _ := ValidateData(validationVersion(), d); len(errs) != 0 {
			t.Fatalf("got %+v", errs)
		}
	})
	t.Run("invalid template pattern fails closed", func(t *testing.T) {
		v := TemplateVersion{Slug: "x", Fields: []models.TemplateField{
			{Name: "t", Type: "text", Validation: models.FieldValidation{Pattern: "([a-z"}},
		}}
		errs, _ := ValidateData(v, map[string]any{"t": "abc"})
		if errorCodes(errs)["t"] != CodeFieldPatternInvalid {
			t.Fatalf("got %+v, want FIELD_PATTERN_INVALID", errs)
		}
	})
}

func TestValidateDataNumberRange(t *testing.T) {
	t.Run("below min", func(t *testing.T) {
		d := validPayload()
		d["rating"] = 0.5
		errs, _ := ValidateData(validationVersion(), d)
		if errorCodes(errs)["rating"] != CodeFieldTooSmall {
			t.Fatalf("got %+v, want FIELD_TOO_SMALL", errs)
		}
	})
	t.Run("above max", func(t *testing.T) {
		d := validPayload()
		d["rating"] = 6.0
		errs, _ := ValidateData(validationVersion(), d)
		if errorCodes(errs)["rating"] != CodeFieldTooLarge {
			t.Fatalf("got %+v, want FIELD_TOO_LARGE", errs)
		}
	})
	t.Run("boundaries inclusive", func(t *testing.T) {
		d := validPayload()
		d["rating"] = 1.0
		if errs, _ := ValidateData(validationVersion(), d); len(errs) != 0 {
			t.Fatalf("min boundary must pass, got %+v", errs)
		}
		d["rating"] = 5.0
		if errs, _ := ValidateData(validationVersion(), d); len(errs) != 0 {
			t.Fatalf("max boundary must pass, got %+v", errs)
		}
	})
	t.Run("optional number missing is fine", func(t *testing.T) {
		d := validPayload()
		delete(d, "rating")
		if errs, _ := ValidateData(validationVersion(), d); len(errs) != 0 {
			t.Fatalf("got %+v", errs)
		}
	})
}

func TestValidateDataURLProtocol(t *testing.T) {
	t.Run("default allows http and https", func(t *testing.T) {
		d := validPayload()
		d["link"] = "http://example.com/a"
		if errs, _ := ValidateData(validationVersion(), d); len(errs) != 0 {
			t.Fatalf("http must pass by default, got %+v", errs)
		}
	})
	t.Run("javascript rejected", func(t *testing.T) {
		d := validPayload()
		d["link"] = "javascript:alert(1)"
		errs, _ := ValidateData(validationVersion(), d)
		if errorCodes(errs)["link"] != CodeFieldProtocol {
			t.Fatalf("got %+v, want FIELD_PROTOCOL_NOT_ALLOWED", errs)
		}
	})
	t.Run("ftp rejected by default", func(t *testing.T) {
		d := validPayload()
		d["link"] = "ftp://example.com/a"
		errs, _ := ValidateData(validationVersion(), d)
		if errorCodes(errs)["link"] != CodeFieldProtocol {
			t.Fatalf("got %+v, want FIELD_PROTOCOL_NOT_ALLOWED", errs)
		}
	})
	t.Run("relative url rejected", func(t *testing.T) {
		d := validPayload()
		d["link"] = "/news/foo"
		errs, _ := ValidateData(validationVersion(), d)
		if errorCodes(errs)["link"] != CodeFieldInvalidURL {
			t.Fatalf("got %+v, want FIELD_INVALID_URL", errs)
		}
	})
	t.Run("custom allowed protocols", func(t *testing.T) {
		d := validPayload()
		d["docs"] = "http://docs.example.com/x"
		errs, _ := ValidateData(validationVersion(), d)
		if errorCodes(errs)["docs"] != CodeFieldProtocol {
			t.Fatalf("http must fail https-only field, got %+v", errs)
		}
	})
	t.Run("optional url empty ok", func(t *testing.T) {
		v := TemplateVersion{Slug: "x", Fields: []models.TemplateField{{Name: "u", Type: "url"}}}
		if errs, _ := ValidateData(v, map[string]any{"u": ""}); len(errs) != 0 {
			t.Fatalf("got %+v", errs)
		}
		if errs, _ := ValidateData(v, map[string]any{}); len(errs) != 0 {
			t.Fatalf("got %+v", errs)
		}
	})
}

func TestValidateDataEnum(t *testing.T) {
	t.Run("valid option", func(t *testing.T) {
		d := validPayload()
		d["category"] = "tech"
		if errs, _ := ValidateData(validationVersion(), d); len(errs) != 0 {
			t.Fatalf("got %+v", errs)
		}
	})
	t.Run("option whitespace trimmed", func(t *testing.T) {
		d := validPayload()
		d["category"] = "sports"
		if errs, _ := ValidateData(validationVersion(), d); len(errs) != 0 {
			t.Fatalf("got %+v", errs)
		}
	})
	t.Run("invalid option", func(t *testing.T) {
		d := validPayload()
		d["category"] = "politics"
		errs, _ := ValidateData(validationVersion(), d)
		if errorCodes(errs)["category"] != CodeFieldEnum {
			t.Fatalf("got %+v, want FIELD_NOT_IN_ENUM", errs)
		}
	})
	t.Run("case sensitive", func(t *testing.T) {
		d := validPayload()
		d["category"] = "News"
		errs, _ := ValidateData(validationVersion(), d)
		if errorCodes(errs)["category"] != CodeFieldEnum {
			t.Fatalf("got %+v, want FIELD_NOT_IN_ENUM", errs)
		}
	})
	t.Run("optional select empty ok", func(t *testing.T) {
		v := TemplateVersion{Slug: "x", Fields: []models.TemplateField{
			{Name: "c", Type: "select", Options: "a, b"},
		}}
		if errs, _ := ValidateData(v, map[string]any{"c": ""}); len(errs) != 0 {
			t.Fatalf("got %+v", errs)
		}
	})
	t.Run("required select empty fails required", func(t *testing.T) {
		d := validPayload()
		d["category"] = ""
		errs, _ := ValidateData(validationVersion(), d)
		if errorCodes(errs)["category"] != CodeFieldRequired {
			t.Fatalf("got %+v, want FIELD_REQUIRED", errs)
		}
	})
}

func TestValidateDataAssetRef(t *testing.T) {
	t.Run("valid path", func(t *testing.T) {
		d := validPayload()
		d["hero"] = "/assets/2026/09/hero.png"
		if errs, _ := ValidateData(validationVersion(), d); len(errs) != 0 {
			t.Fatalf("got %+v", errs)
		}
	})
	t.Run("traversal rejected", func(t *testing.T) {
		d := validPayload()
		d["hero"] = "/assets/../etc/passwd"
		errs, _ := ValidateData(validationVersion(), d)
		if errorCodes(errs)["hero"] != CodeFieldAsset {
			t.Fatalf("got %+v, want FIELD_INVALID_ASSET", errs)
		}
	})
	t.Run("control characters rejected", func(t *testing.T) {
		d := validPayload()
		d["hero"] = "/assets/a\x00b.png"
		errs, _ := ValidateData(validationVersion(), d)
		if errorCodes(errs)["hero"] != CodeFieldAsset {
			t.Fatalf("got %+v, want FIELD_INVALID_ASSET", errs)
		}
	})
	t.Run("required empty fails", func(t *testing.T) {
		d := validPayload()
		d["hero"] = ""
		errs, _ := ValidateData(validationVersion(), d)
		if errorCodes(errs)["hero"] != CodeFieldRequired {
			t.Fatalf("got %+v, want FIELD_REQUIRED", errs)
		}
	})
	t.Run("optional empty ok", func(t *testing.T) {
		d := validPayload()
		d["thumb"] = ""
		if errs, _ := ValidateData(validationVersion(), d); len(errs) != 0 {
			t.Fatalf("got %+v", errs)
		}
	})
}

func TestValidateDataDate(t *testing.T) {
	t.Run("nonexistent calendar date fails", func(t *testing.T) {
		d := validPayload()
		d["pubdate"] = "2026-02-30"
		errs, _ := ValidateData(validationVersion(), d)
		if errorCodes(errs)["pubdate"] != CodeFieldDate {
			t.Fatalf("got %+v, want FIELD_INVALID_DATE", errs)
		}
	})
	t.Run("datetime rejected", func(t *testing.T) {
		d := validPayload()
		d["pubdate"] = "2026-09-30T00:00:00Z"
		errs, _ := ValidateData(validationVersion(), d)
		if errorCodes(errs)["pubdate"] != CodeFieldDate {
			t.Fatalf("got %+v, want FIELD_INVALID_DATE", errs)
		}
	})
	t.Run("bad month rejected", func(t *testing.T) {
		d := validPayload()
		d["pubdate"] = "2026-13-01"
		errs, _ := ValidateData(validationVersion(), d)
		if errorCodes(errs)["pubdate"] != CodeFieldDate {
			t.Fatalf("got %+v, want FIELD_INVALID_DATE", errs)
		}
	})
	t.Run("leap day accepted", func(t *testing.T) {
		d := validPayload()
		d["pubdate"] = "2024-02-29"
		if errs, _ := ValidateData(validationVersion(), d); len(errs) != 0 {
			t.Fatalf("got %+v", errs)
		}
	})
	t.Run("optional date empty means missing", func(t *testing.T) {
		v := TemplateVersion{Slug: "x", Fields: []models.TemplateField{{Name: "d", Type: "date"}}}
		if errs, _ := ValidateData(v, map[string]any{"d": ""}); len(errs) != 0 {
			t.Fatalf("got %+v", errs)
		}
		if errs, _ := ValidateData(v, map[string]any{}); len(errs) != 0 {
			t.Fatalf("got %+v", errs)
		}
	})
	t.Run("required date empty fails", func(t *testing.T) {
		d := validPayload()
		d["pubdate"] = ""
		errs, _ := ValidateData(validationVersion(), d)
		if errorCodes(errs)["pubdate"] != CodeFieldRequired {
			t.Fatalf("got %+v, want FIELD_REQUIRED", errs)
		}
	})
}

func TestValidateDataMissingVsExplicitFalse(t *testing.T) {
	v := TemplateVersion{Slug: "x", Fields: []models.TemplateField{
		{Name: "opt", Type: "boolean"},
		{Name: "req", Type: "boolean", Required: true},
	}}
	t.Run("both missing: only required errors", func(t *testing.T) {
		errs, warns := ValidateData(v, map[string]any{})
		codes := errorCodes(errs)
		if codes["req"] != CodeFieldRequired {
			t.Fatalf("got %+v, want FIELD_REQUIRED on req", errs)
		}
		if _, has := codes["opt"]; has {
			t.Fatalf("missing optional boolean must not error, got %+v", errs)
		}
		if len(warns) != 0 {
			t.Fatalf("no defaults: want zero warnings, got %+v", warns)
		}
	})
	t.Run("explicit false on both: no errors", func(t *testing.T) {
		errs, warns := ValidateData(v, map[string]any{"opt": false, "req": false})
		if len(errs) != 0 {
			t.Fatalf("explicit false is a value, got %+v", errs)
		}
		if len(warns) != 0 {
			t.Fatalf("got %+v", warns)
		}
	})
	t.Run("nil data map behaves as empty", func(t *testing.T) {
		errs, _ := ValidateData(v, nil)
		if errorCodes(errs)["req"] != CodeFieldRequired {
			t.Fatalf("got %+v, want FIELD_REQUIRED on req", errs)
		}
	})
}

// TestValidateDataWarningsNeverBlock is the MVP warnings policy (spec §11.3):
// only errors block publish. A warnings-only result must be publishable.
func TestValidateDataWarningsNeverBlock(t *testing.T) {
	v := TemplateVersion{Slug: "x", Fields: []models.TemplateField{
		{Name: "req", Type: "text", Required: true},
		{Name: "nick", Type: "text", Default: "anon"},
		{Name: "flag", Type: "boolean", Default: "true"},
	}}
	errs, warns := ValidateData(v, map[string]any{"req": "ok"})
	if len(errs) != 0 {
		t.Fatalf("warnings-only payload must have zero errors, got %+v", errs)
	}
	if len(warns) != 2 {
		t.Fatalf("want 2 default-applied warnings, got %+v", warns)
	}
	// The publish gate from spec §11.3: blocked iff len(errors) > 0.
	blocked := len(errs) > 0
	if blocked {
		t.Fatal("warnings must never block publish")
	}
	for _, w := range warns {
		if w.Code == "" || w.Field == "" || w.Message == "" {
			t.Fatalf("warnings must carry code/field/message, got %+v", warns)
		}
	}
}
