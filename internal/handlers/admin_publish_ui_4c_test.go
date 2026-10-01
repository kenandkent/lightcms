package handlers

import (
	"net/http"
	"strings"
	"testing"

	"github.com/jonradoff/lightcms/v7/internal/i18n"
)

// publishUIKeys are the i18n keys introduced by the Wave 4C edit-page publish
// controls (publish button, publications history link, checkbox help text).
var publishUIKeys = []string{
	"content_form.publish_now",
	"content_form.publication_history",
	"content_form.published_checkbox_help",
}

// formDepthAt returns how many <form> elements are still open in body
// immediately before offset. It is used to prove the publish form sits at
// top level: HTML forbids <form> inside <form>, and a nested form tag is
// silently dropped by browsers (its buttons then submit the outer form).
func formDepthAt(body string, offset int) int {
	if offset > len(body) {
		offset = len(body)
	}
	depth := 0
	for i := 0; i < offset; {
		switch {
		case strings.HasPrefix(body[i:], "<form"):
			depth++
			i += len("<form")
		case strings.HasPrefix(body[i:], "</form>"):
			depth--
			i += len("</form>")
		default:
			i++
		}
	}
	return depth
}

// TestEditContent_PublishUI guards the Wave 4C admin edit-page publish UI:
// a separate top-level publish form posting to the saga-backed endpoint, a
// publications history link, the published-checkbox help text, and a clean
// render when lane 4B's ActivePublicationID key is still absent.
func TestEditContent_PublishUI(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	tmplID := seedTemplate(t, h.db, "Page", "page")
	contentID := seedContent(t, h.db, tmplID, "Doc", "doc", "/doc")
	id := contentID.Hex()

	rr := getPage(t, h.EditContent, map[string]string{"id": id})
	if rr.Code != http.StatusOK {
		t.Fatalf("EditContent: status %d, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	// Publish button posts to the existing publish endpoint.
	publishAction := `action="/cm/content/` + id + `/publish"`
	publishIdx := strings.Index(body, publishAction)
	if publishIdx < 0 {
		t.Fatalf("edit page missing publish form %q", publishAction)
	}
	if !strings.Contains(body, "发布上线") {
		t.Error(`edit page missing publish button text "发布上线"`)
	}

	// The publish form must not nest inside another form (HTML forbids it).
	if depth := formDepthAt(body, publishIdx); depth != 1 {
		t.Errorf("publish form is open at depth %d, want 1 (top level, exactly one form open)", depth)
	}

	// Publications history link.
	historyHref := `href="/cm/content/` + id + `/publications"`
	if !strings.Contains(body, historyHref) {
		t.Errorf("edit page missing publications history link %q", historyHref)
	}
	if !strings.Contains(body, "发布历史") {
		t.Error(`edit page missing publications link text "发布历史"`)
	}

	// Published-checkbox help text.
	if !strings.Contains(body, "勾选后保存即通过发布流程上线") {
		t.Error("edit page missing published-checkbox help text")
	}

	// Render must continue past the new markup (guards template aborts).
	if !strings.Contains(body, `id="redirect-modal"`) {
		t.Error("render truncated before the redirect modal — template Execute aborted?")
	}

	// Lane 4B's ActivePublicationID key does not exist in this worktree: the
	// {{if}} guard must simply omit the hidden input rather than emitting
	// "<no value>" or breaking execution. Presence of the field is asserted by
	// post-merge verification once lane 4B lands.
	if strings.Contains(body, "expected_active_id") {
		t.Error("expected_active_id rendered while ActivePublicationID is absent from template data")
	}
	region := body[publishIdx:]
	if end := strings.Index(region, "</form>"); end >= 0 {
		region = region[:end]
	}
	if strings.Contains(region, "<no value>") {
		t.Errorf("publish form region rendered an unresolved value: %s", region)
	}

	// New-content form must not offer publishing (guarded by {{if not .IsNew}}).
	newRR := getPageQ(t, h.NewContentWithTemplate, "template_id="+tmplID.Hex(), nil)
	if newRR.Code == http.StatusOK {
		newBody := newRR.Body.String()
		if strings.Contains(newBody, publishAction) || strings.Contains(newBody, "/publications\"") {
			t.Error("new-content form must not render publish controls")
		}
	}
}

// TestEditContent_PublishUIKeyWiring checks the three new i18n calls in the
// content_form template source: root-scope $.Lang (range/scope regression) and
// the documented fallback text.
func TestEditContent_PublishUIKeyWiring(t *testing.T) {
	src, ok := adminTemplates["content_form"]
	if !ok {
		t.Fatal("content_form template missing from adminTemplates")
	}
	calls := []string{
		`{{i18n "content_form.publish_now" "发布上线" $.Lang}}`,
		`{{i18n "content_form.publication_history" "发布历史" $.Lang}}`,
		`{{i18n "content_form.published_checkbox_help" "勾选后保存即通过发布流程上线；线上版本可在发布历史中查看和回滚" $.Lang}}`,
	}
	for _, want := range calls {
		if !strings.Contains(src, want) {
			t.Errorf("content_form missing i18n call: %s", want)
		}
	}
	// Publish form fields.
	for _, want := range []string{
		`action="/cm/content/{{.Content.ID.Hex}}/publish"`,
		`{{if .ActivePublicationID}}<input type="hidden" name="expected_active_id" value="{{.ActivePublicationID}}">{{end}}`,
		`href="/cm/content/{{.Content.ID.Hex}}/publications"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("content_form missing publish UI markup: %s", want)
		}
	}
}

// TestEditContent_PublishUII18nParity cheaply asserts en/zh parity for the new
// keys (mirrors internal/i18n.TestDicts_Parity without running that suite) and
// that the dictionaries win over the fallback.
func TestEditContent_PublishUII18nParity(t *testing.T) {
	zh := i18n.Dict(i18n.LangZh)
	en := i18n.Dict(i18n.LangEn)
	for _, k := range publishUIKeys {
		if zh[k] == "" {
			t.Errorf("zh dict missing key %q", k)
		}
		if en[k] == "" {
			t.Errorf("en dict missing key %q", k)
		}
	}
	// Dict values must beat the (deliberately wrong) fallback argument.
	want := map[string]map[string]string{
		i18n.LangZh: {
			"content_form.publish_now":         "发布上线",
			"content_form.publication_history": "发布历史",
		},
		i18n.LangEn: {
			"content_form.publish_now":         "Publish",
			"content_form.publication_history": "Publications",
		},
	}
	for lang, kv := range want {
		for k, v := range kv {
			if got := i18n.T(k, "FALLBACK-MUST-NOT-RENDER", lang); got != v {
				t.Errorf("%s %s = %q, want %q", lang, k, got, v)
			}
		}
	}
}
