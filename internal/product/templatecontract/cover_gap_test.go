package templatecontract

// Task 17B coverage-gap tests for internal/product/templatecontract.

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

func TestCoverGapErrorHelpers(t *testing.T) {
	withCause := &Error{Code: CodeInternal, Message: "op", Err: errors.New("boom")}
	if s := withCause.Error(); !strings.Contains(s, "boom") {
		t.Fatalf("Error() = %q", s)
	}
	plain := &Error{Code: CodeNotFound, Message: "missing"}
	if s := plain.Error(); !strings.Contains(s, "missing") {
		t.Fatalf("Error() = %q", s)
	}
	if plain.Unwrap() != nil {
		t.Fatalf("Unwrap() without cause, want nil")
	}
	if CodeOf(nil) != "" {
		t.Fatalf("CodeOf(nil), want empty")
	}
	if CodeOf(withCause) != CodeInternal {
		t.Fatalf("CodeOf(typed) = %q", CodeOf(withCause))
	}
	if CodeOf(errors.New("plain")) != CodeInternal {
		t.Fatalf("CodeOf(plain) = %q, want INTERNAL_ERROR", CodeOf(errors.New("plain")))
	}

	if IsDuplicateKey(nil) {
		t.Fatalf("IsDuplicateKey(nil) = true")
	}
	dup := mongo.WriteException{WriteErrors: []mongo.WriteError{{Code: 11000, Message: "E11000 duplicate key"}}}
	if !IsDuplicateKey(dup) {
		t.Fatalf("IsDuplicateKey(11000) = false")
	}
	verDup := mongo.WriteException{WriteErrors: []mongo.WriteError{{Code: 11000, Message: "E11000 template_versions dup"}}}
	if !IsDuplicateKey(verDup) {
		t.Fatalf("IsDuplicateKey(version dup) = false")
	}
	if !IsDuplicateKey(errors.New("duplicate key error collection")) {
		t.Fatalf("IsDuplicateKey(string match) = false")
	}
	if IsDuplicateKey(errors.New("connection refused")) {
		t.Fatalf("IsDuplicateKey(other) = true")
	}

	if err := mapCreateDupKey(errors.New("connection refused")); CodeOf(err) != CodeInternal {
		t.Fatalf("mapCreateDupKey(non-dup) = %v", err)
	}
	if err := mapCreateDupKey(verDup); CodeOf(err) != CodeVersionConflict {
		t.Fatalf("mapCreateDupKey(version dup) = %v", err)
	}
	if err := mapCreateDupKey(dup); CodeOf(err) != CodeSlugConflict {
		t.Fatalf("mapCreateDupKey(slug dup) = %v", err)
	}

	v := TemplateVersion{Slug: "news", Name: "News", Category: "c", Status: StatusActive,
		HTMLLayout: "<b></b>", ScriptPolicy: "all",
		Fields:     []models.TemplateField{{Name: "t", Type: "text"}}}
	in := v.Input()
	if in.Slug != "news" || in.Name != "News" || in.ScriptPolicy != "all" || len(in.Fields) != 1 {
		t.Fatalf("Input() = %+v", in)
	}
	if h := hashJSON(func() {}); h == "" {
		t.Fatalf("hashJSON(unmarshallable) empty")
	}
}

func TestCoverGapStatusTransitions(t *testing.T) {
	if NormalizeStatus("") != StatusDraft {
		t.Fatalf("NormalizeStatus empty")
	}
	if NormalizeStatus(StatusActive) != StatusActive {
		t.Fatalf("NormalizeStatus passthrough")
	}
	cases := []struct {
		from, to string
		ok       bool
	}{
		{"", "", true},
		{"draft", "draft", true},
		{"draft", "active", true},
		{"draft", "deprecated", true},
		{"active", "deprecated", true},
		{"active", "active", true},
		{"active", "draft", false},
		{"deprecated", "deprecated", true},
		{"deprecated", "active", false},
		{"deprecated", "draft", false},
		{"bogus", "draft", false},
		{"draft", "bogus", false},
		{"", "active", true},
	}
	for _, c := range cases {
		err := ValidateStatusTransition(c.from, c.to)
		if (err == nil) != c.ok {
			t.Errorf("transition %q->%q err=%v, want ok=%v", c.from, c.to, err, c.ok)
		}
	}
}

func TestCoverGapValidateNumberValue(t *testing.T) {
	numField := models.TemplateField{Name: "n", Type: "number"}
	vals := []any{
		float64(1.5), float32(2), int(3), int8(4), int16(5), int32(6), int64(7),
		uint(8), uint8(9), uint16(10), uint32(11), uint64(12),
		json.Number("13.5"),
	}
	for _, v := range vals {
		if errs := validateNumberValue(numField, v); len(errs) != 0 {
			t.Errorf("validateNumberValue(%T %v) = %v, want clean", v, v, errs)
		}
	}
	if errs := validateNumberValue(numField, json.Number("nan-x")); len(errs) == 0 {
		t.Fatalf("bad json.Number: want error")
	}
	if errs := validateNumberValue(numField, "12"); len(errs) == 0 {
		t.Fatalf("string number: want error")
	}
	if errs := validateNumberValue(numField, true); len(errs) == 0 {
		t.Fatalf("bool number: want error")
	}
	if errs := validateNumberValue(numField, nil); len(errs) == 0 {
		t.Fatalf("nil number: want error")
	}
	if errs := validateNumberValue(numField, math.NaN()); len(errs) == 0 { // NaN
		t.Fatalf("NaN: want error")
	}
	min, max := 5.0, 10.0
	bounded := models.TemplateField{Name: "b", Type: "number", Validation: models.FieldValidation{Min: &min, Max: &max}}
	if errs := validateNumberValue(bounded, 3); len(errs) == 0 {
		t.Fatalf("below min: want error")
	}
	if errs := validateNumberValue(bounded, 30); len(errs) == 0 {
		t.Fatalf("above max: want error")
	}
	if errs := validateNumberValue(bounded, 7); len(errs) != 0 {
		t.Fatalf("in range: %v", errs)
	}
	// validateFieldValue dispatch: nil + unsupported type.
	if errs := validateFieldValue("s", models.TemplateField{Name: "x", Type: "text"}, nil); len(errs) == 0 {
		t.Fatalf("nil value: want FIELD_NULL_NOT_ALLOWED")
	}
	if errs := validateFieldValue("s", models.TemplateField{Name: "x", Type: "group"}, "v"); len(errs) == 0 {
		t.Fatalf("unsupported type: want error")
	}
	if errs := validateFieldValue("s", models.TemplateField{Name: "b", Type: "boolean"}, true); len(errs) != 0 {
		t.Fatalf("bool ok: %v", errs)
	}
}

func TestCoverGapFieldDefault(t *testing.T) {
	if _, ok := fieldDefault(models.TemplateField{Type: "boolean", Default: "true"}); !ok {
		t.Fatalf("bool true default not materialized")
	}
	if _, ok := fieldDefault(models.TemplateField{Type: "boolean", Default: "True"}); !ok {
		t.Fatalf("bool True default not materialized")
	}
	for _, d := range []string{"false", "", "maybe"} {
		if _, ok := fieldDefault(models.TemplateField{Type: "boolean", Default: d}); ok {
			t.Fatalf("bool default %q materialized, want missing", d)
		}
	}
	if _, ok := fieldDefault(models.TemplateField{Type: "number", Default: " 3.25 "}); !ok {
		t.Fatalf("number default not materialized")
	}
	for _, d := range []string{"", "  ", "abc"} {
		if _, ok := fieldDefault(models.TemplateField{Type: "number", Default: d}); ok {
			t.Fatalf("number default %q materialized, want missing", d)
		}
	}
	if _, ok := fieldDefault(models.TemplateField{Type: "text", Default: "hi"}); !ok {
		t.Fatalf("text default not materialized")
	}
	if _, ok := fieldDefault(models.TemplateField{Type: "text"}); ok {
		t.Fatalf("empty text default materialized")
	}
}

func TestCoverGapCheckURLAndAssetRef(t *testing.T) {
	urlField := models.TemplateField{Name: "u", Type: "url"}
	if err := checkURL(urlField, "https://example.com/x"); err != nil {
		t.Fatalf("valid url: %v", err)
	}
	for _, bad := range []string{
		"http://foo.com/\x7f", // url.Parse failure (control char)
		"just-a-relative-path",
		"",
		"ftp://example.com/x", // disallowed protocol
		"https:opaque-without-host",
		"https:///no-host",
	} {
		if err := checkURL(urlField, bad); err == nil {
			t.Errorf("checkURL(%q): want error", bad)
		}
	}
	custom := models.TemplateField{Name: "u", Type: "url",
		Validation: models.FieldValidation{AllowedProtocols: []string{"ftp"}}}
	if err := checkURL(custom, "ftp://example.com/x"); err != nil {
		t.Fatalf("custom protocol: %v", err)
	}

	assetField := models.TemplateField{Name: "img", Type: "image"}
	if err := checkAssetRef(assetField, "uploads/a.png"); err != nil {
		t.Fatalf("valid asset: %v", err)
	}
	for _, bad := range []string{"../secret", "a\x00b", " padded ", "a/b\x01c"} {
		if err := checkAssetRef(assetField, bad); err == nil {
			t.Errorf("checkAssetRef(%q): want error", bad)
		}
	}
}

func TestCoverGapSchemas(t *testing.T) {
	v := TemplateVersion{Slug: "gap", Fields: []models.TemplateField{
		{Name: "d", Type: "date", Description: "D", Example: "2024-01-01", Default: "2024-01-01",
			Validation: models.FieldValidation{MinLength: intPtrGap(2), MaxLength: intPtrGap(10), Pattern: "^[0-9-]+$"}},
		{Name: "sel", Type: "select", Options: "a,b", Required: true, Default: "a"},
		{Name: "req", Type: "text", Required: true},
		{Name: "u", Type: "url"},
		{Name: "n", Type: "number", Validation: models.FieldValidation{Min: floatPtrGap(1), Max: floatPtrGap(5)}, Default: "oops"},
		{Name: "n2", Type: "number", Default: "3"},
		{Name: "b", Type: "boolean", Default: "notabool"},
		{Name: "b2", Type: "boolean", Default: "true"},
	}}
	raw, err := ToJSONSchema(v)
	if err != nil {
		t.Fatalf("ToJSONSchema: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	req, _ := doc["required"].([]any)
	for _, r := range req {
		if r == "sel" {
			t.Fatalf("field with materializable default must not be required")
		}
	}
	bad := TemplateVersion{Slug: "bad", Fields: []models.TemplateField{{Name: "g", Type: "group"}}}
	if _, err := ToJSONSchema(bad); CodeOf(err) != CodeSchemaInvalid {
		t.Fatalf("unknown type schema: %v, want TEMPLATE_SCHEMA_INVALID", err)
	}
	if _, err := fieldSchema(models.TemplateField{Name: "g", Type: "group"}); CodeOf(err) != CodeSchemaInvalid {
		t.Fatalf("fieldSchema unknown: %v", err)
	}
}

func intPtrGap(i int) *int             { return &i }
func floatPtrGap(f float64) *float64  { return &f }

func TestCoverGapRepositoryFaults(t *testing.T) {
	// Direct collection calls bypass the fault hook, so exercise the
	// transport-error (internalErr) branches with a disconnected client.
	bdb := testutil.MustConnectBrokenDB(t)
	brepo := NewRepository(bdb)
	ctx := context.Background()
	id := primitive.NewObjectID()

	if _, err := brepo.FindTemplateByID(ctx, id); CodeOf(err) != CodeInternal {
		t.Fatalf("FindTemplateByID broken: %v", err)
	}
	if _, err := brepo.FindTemplateBySlug(ctx, "x"); CodeOf(err) != CodeInternal {
		t.Fatalf("FindTemplateBySlug broken: %v", err)
	}
	if _, err := brepo.FindVersion(ctx, id, 1); CodeOf(err) != CodeInternal {
		t.Fatalf("FindVersion broken: %v", err)
	}
	if _, err := brepo.FindVersionByID(ctx, id); CodeOf(err) != CodeInternal {
		t.Fatalf("FindVersionByID broken: %v", err)
	}
	// Not-found branches on the live connection.
	db, _ := testutil.MustConnectTestDB(t)
	repo := NewRepository(db)
	if _, err := repo.FindTemplateByID(ctx, id); CodeOf(err) != CodeNotFound {
		t.Fatalf("FindTemplateByID missing: %v", err)
	}
	if _, err := repo.FindTemplateBySlug(ctx, "nope"); CodeOf(err) != CodeNotFound {
		t.Fatalf("FindTemplateBySlug missing: %v", err)
	}
	if _, err := repo.FindVersion(ctx, id, 1); CodeOf(err) != CodeVersionNotFound {
		t.Fatalf("FindVersion missing: %v", err)
	}
	if _, err := repo.FindVersionByID(ctx, id); CodeOf(err) != CodeVersionNotFound {
		t.Fatalf("FindVersionByID missing: %v", err)
	}
}

func TestCoverGapServiceUpdateBranches(t *testing.T) {
	db, _ := testutil.MustConnectTestDB(t)
	ctx := context.Background()
	svc := NewService(db)
	base := TemplateInput{Slug: "gap-update", Name: "Gap", Category: "n", Status: StatusDraft,
		HTMLLayout: "<p>x</p>", Fields: []models.TemplateField{{Name: "t", Label: "T", Type: "text"}}}

	if _, _, err := svc.Create(ctx, TemplateInput{Slug: "Bad Slug!", Name: "x"}); err == nil {
		t.Fatalf("Create bad slug: want error")
	}
	tpl, _, err := svc.Create(ctx, base)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, _, err := svc.Create(ctx, base); CodeOf(err) != CodeSlugConflict {
		t.Fatalf("Create duplicate fast path: %v", err)
	}

	if _, err := svc.Update(ctx, primitive.NewObjectID(), 1, base); CodeOf(err) != CodeNotFound {
		t.Fatalf("Update missing: %v", err)
	}
	if _, err := svc.Update(ctx, tpl.ID, 99, base); CodeOf(err) != CodeVersionConflict {
		t.Fatalf("Update stale version: %v", err)
	}
	renamed := base
	renamed.Slug = "other"
	if _, err := svc.Update(ctx, tpl.ID, 1, renamed); CodeOf(err) != CodeSlugImmutable {
		t.Fatalf("Update slug change: %v", err)
	}
	// Same contract + display-name follow.
	same := base
	same.Name = "Gap Renamed"
	cur, err := svc.Update(ctx, tpl.ID, 1, same)
	if err != nil {
		t.Fatalf("Update same-contract: %v", err)
	}
	if cur.Version != 1 {
		t.Fatalf("same-contract version = %d, want 1", cur.Version)
	}
	// Deprecated is terminal: draft->active ok, then active->deprecated ok,
	// deprecated->active rejected.
	toActive := base
	toActive.Name = "Gap Renamed"
	toActive.Status = StatusActive
	toActive.Fields = append(append([]models.TemplateField{}, base.Fields...),
		models.TemplateField{Name: "extra", Label: "E", Type: "text"})
	if _, err := svc.Update(ctx, tpl.ID, 1, toActive); err != nil {
		t.Fatalf("Update draft->active: %v", err)
	}
	back := toActive
	back.Status = StatusDraft
	if _, err := svc.Update(ctx, tpl.ID, 2, back); err == nil {
		t.Fatalf("Update active->draft: want error")
	}
}
