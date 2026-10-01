package handlers

// Lane 4A regression: RBAC gates on the seven Admin publication handlers and
// the restored form contracts (Wave 4A).
//
// Permission matrix (mirrors rbac_hardening_test.go style):
//   AdminProductPublications (GET)      -> content.view
//   AdminProductUpgradePreview (GET)    -> template.view
//   AdminProductPublish (POST)          -> content.edit + content.publish
//   AdminProductRestoreAndPublish       -> content.edit + content.publish
//   AdminProductRevertLive              -> content.edit + content.publish
//   AdminProductUpgradeStart (POST)     -> template.edit + content.publish
//   AdminProductUpgradeRun (POST)       -> template.edit + content.publish
//
// viewer holds content.view/template.view (GET gates pass) but no mutating
// permission; contributor holds neither content.edit/publish nor
// template.edit (all POSTs 403); editor passes the content POSTs but not the
// upgrade handlers (template.edit is admin-only — matching
// generation.StartUpgradeJob); admin passes everything. Allowed roles are
// exercised against NONEXISTENT content/template IDs so a passing test never
// runs a publish to completion (which would write files into the package
// dir) — 404 or the failure page is the expected outcome.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/jonradoff/lightcms/v7/internal/product/generation"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// rbacGet invokes a GET handler as the given role.
func rbacGet(t *testing.T, handler http.HandlerFunc, vars map[string]string, role string) *httptest.ResponseRecorder {
	t.Helper()
	req := rbacSessionReq(t, role, http.MethodGet, "/cm/x", nil, vars)
	rr := httptest.NewRecorder()
	handler(rr, req)
	return rr
}

// TestRBAC4APostGates: every mutating publication handler must 403 for
// viewer and contributor, and must NOT 403 for editor/admin (the allowed
// paths are stopped downstream by a nonexistent ID / missing idempotency
// key, never by RBAC).
func TestRBAC4APostGates(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	ghostContent := primitive.NewObjectID().Hex()
	ghostPub := primitive.NewObjectID().Hex()
	ghostTemplate := primitive.NewObjectID().Hex()
	ghostJob := primitive.NewObjectID().Hex()

	type call struct {
		name string
		fn   http.HandlerFunc
		vars map[string]string
		// allowedRoles are the roles expected to pass the RBAC gate (they
		// then fail downstream on the nonexistent ID instead). template.edit
		// is admin-only, so the upgrade handlers are admin-gated — matching
		// generation.StartUpgradeJob ("upgrade jobs require an admin with
		// template.edit").
		allowedRoles []string
	}
	calls := []call{
		{"Publish", h.AdminProductPublish, map[string]string{"id": ghostContent},
			[]string{"editor", "admin"}},
		{"RestoreAndPublish", h.AdminProductRestoreAndPublish, map[string]string{"id": ghostContent, "version": "1"},
			[]string{"editor", "admin"}},
		{"RevertLive", h.AdminProductRevertLive, map[string]string{"id": ghostContent, "publicationID": ghostPub},
			[]string{"editor", "admin"}},
		{"UpgradeStart", h.AdminProductUpgradeStart, map[string]string{"id": ghostTemplate},
			[]string{"admin"}},
		{"UpgradeRun", h.AdminProductUpgradeRun, map[string]string{"jobID": ghostJob},
			[]string{"admin"}},
	}

	for _, c := range calls {
		for _, role := range []string{"viewer", "contributor"} {
			rr := rbacPost(t, c.fn, nil, c.vars, role)
			if rr.Code != http.StatusForbidden {
				t.Errorf("%s as %s: got %d, want 403", c.name, role, rr.Code)
			}
		}
		for _, role := range c.allowedRoles {
			rr := rbacPost(t, c.fn, nil, c.vars, role)
			if rr.Code == http.StatusForbidden {
				t.Errorf("%s as %s: got 403, want allowed past RBAC (got %d: %s)",
					c.name, role, rr.Code, strings.TrimSpace(rr.Body.String()))
			}
		}
	}

	// Lock-in: editors hold content.edit + content.publish but NOT
	// template.edit, so they pass the content mutations and are forbidden
	// from starting/running template upgrade jobs.
	for _, c := range []call{
		{"UpgradeStart", h.AdminProductUpgradeStart, map[string]string{"id": ghostTemplate}, nil},
		{"UpgradeRun", h.AdminProductUpgradeRun, map[string]string{"jobID": ghostJob}, nil},
	} {
		if rr := rbacPost(t, c.fn, nil, c.vars, "editor"); rr.Code != http.StatusForbidden {
			t.Errorf("%s as editor: got %d, want 403 (template.edit is admin-only)", c.name, rr.Code)
		}
	}
}

// TestRBAC4AGetGates: viewer holds content.view + template.view, so the
// read-only publication/upgrade-preview pages must not 403 for any defined
// role (the gate still exists for unknown roles).
func TestRBAC4AGetGates(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	ghostContent := primitive.NewObjectID().Hex()
	ghostTemplate := primitive.NewObjectID().Hex()

	for _, role := range []string{"viewer", "contributor", "editor", "admin"} {
		rr := rbacGet(t, h.AdminProductPublications, map[string]string{"id": ghostContent}, role)
		if rr.Code == http.StatusForbidden {
			t.Errorf("AdminProductPublications as %s: got 403, want allowed (content.view)", role)
		}
		rr = rbacGet(t, h.AdminProductUpgradePreview, map[string]string{"id": ghostTemplate}, role)
		if rr.Code == http.StatusForbidden {
			t.Errorf("AdminProductUpgradePreview as %s: got 403, want allowed (template.view)", role)
		}
	}

	// An unknown role holds no permissions at all: GET gates must still 403.
	rr := rbacGet(t, h.AdminProductPublications, map[string]string{"id": ghostContent}, "ghost-role")
	if rr.Code != http.StatusForbidden {
		t.Errorf("AdminProductPublications as unknown role: got %d, want 403", rr.Code)
	}
	rr = rbacGet(t, h.AdminProductUpgradePreview, map[string]string{"id": ghostTemplate}, "ghost-role")
	if rr.Code != http.StatusForbidden {
		t.Errorf("AdminProductUpgradePreview as unknown role: got %d, want 403", rr.Code)
	}
}

var idempotencyKeyRe = regexp.MustCompile(`name="idempotency_key" value="([0-9a-f]+)"`)

// TestRestoreRevertActionsHTMLForms locks the three-form contract: the
// restore-as-draft form posts to the REGISTERED /revert route (the
// /restore-draft route never existed), and both mutating forms carry a
// render-time idempotency_key plus the expected_active_id precondition.
func TestRestoreRevertActionsHTMLForms(t *testing.T) {
	cid := primitive.NewObjectID().Hex()
	src := primitive.NewObjectID().Hex()
	active := primitive.NewObjectID().Hex()

	got := string(RestoreRevertActionsHTML(cid, 7, src, active))

	if !strings.Contains(got, "/versions/7/revert") {
		t.Errorf("restore-draft form must post to the registered /revert route in:\n%s", got)
	}
	if strings.Contains(got, "restore-draft") {
		t.Errorf("no /restore-draft route is registered; form must not reference it in:\n%s", got)
	}
	if !strings.Contains(got, "/versions/7/restore_and_publish") {
		t.Errorf("missing restore_and_publish action in:\n%s", got)
	}
	if !strings.Contains(got, "/publications/"+src+"/revert_live") {
		t.Errorf("missing revert_live action in:\n%s", got)
	}
	if n := strings.Count(got, `name="idempotency_key"`); n != 2 {
		t.Errorf("both mutating forms must emit idempotency_key, got %d occurrences in:\n%s", n, got)
	}
	for _, m := range idempotencyKeyRe.FindAllStringSubmatch(got, -1) {
		if len(m[1]) != 32 {
			t.Errorf("idempotency_key %q must be 16 random bytes hex-encoded", m[1])
		}
	}
	if !strings.Contains(got, `name="expected_active_id" value="`+active+`"`) {
		t.Errorf("expected_active_id %q missing from mutating forms in:\n%s", active, got)
	}

	// No active publication: the guard field is omitted, idempotency stays.
	got = string(RestoreRevertActionsHTML(cid, 7, src, ""))
	if strings.Contains(got, "expected_active_id") {
		t.Errorf("empty active ID must not emit expected_active_id in:\n%s", got)
	}
	if !strings.Contains(got, `name="idempotency_key"`) {
		t.Errorf("idempotency_key must still be emitted in:\n%s", got)
	}
}

// TestUpgradePreviewHTMLForm: the start control must post to the POST-only
// upgrade-start route for the template (the preview URL is GET-only — a
// self-posting form would 405).
func TestUpgradePreviewHTMLForm(t *testing.T) {
	tid := primitive.NewObjectID().Hex()
	prev := generation.UpgradePreview{
		Template: "financial-news", FromVersion: 2, ToVersion: 3,
		TotalPages: 1, WouldRepublish: 1,
	}
	got := string(UpgradePreviewHTML(prev, tid))
	want := `action="/cm/templates/` + tid + `/upgrade-start"`
	if !strings.Contains(got, want) {
		t.Errorf("start form must post to %s, got:\n%s", want, got)
	}
	if strings.Contains(got, `action=""`) {
		t.Errorf("start form must not self-post (GET-only preview URL) in:\n%s", got)
	}
}

// TestUpgradeJobHTMLForm: the retry/resume form must post to the explicit
// run URL — self-posting to upgrade-start would CREATE A NEW JOB.
func TestUpgradeJobHTMLForm(t *testing.T) {
	job := generation.UpgradeJob{
		ID: primitive.NewObjectID(), TemplateSlug: "financial-news",
		FromVersion: 2, ToVersion: 3, Status: generation.UpgradeJobPartial,
		Items: []generation.UpgradeJobItem{
			{ContentID: primitive.NewObjectID(), FullPath: "/news/a", Status: generation.UpgradeItemFailed, Attempts: 1, Error: "boom"},
		},
	}
	runURL := "/cm/upgrade-jobs/" + job.ID.Hex() + "/run"
	got := string(UpgradeJobHTML(job, runURL))
	if !strings.Contains(got, `action="`+runURL+`"`) {
		t.Errorf("retry form must post to %s, got:\n%s", runURL, got)
	}
	if strings.Contains(got, `action=""`) {
		t.Errorf("retry form must not self-post in:\n%s", got)
	}

	// A finished job renders no retry form at all (the description copy
	// mentions 重试, so assert on the form itself).
	job.Status = generation.UpgradeJobCompleted
	if got := string(UpgradeJobHTML(job, runURL)); strings.Contains(got, "<form") {
		t.Errorf("completed job must not offer a retry form in:\n%s", got)
	}
}

// TestFailedPublishHTMLRetryHidden: a retryable failure must re-emit the
// hidden fields it was given, otherwise a retry of restore/revert drops
// idempotency_key and fails again with IDEMPOTENCY_KEY_REQUIRED.
func TestFailedPublishHTMLRetryHidden(t *testing.T) {
	hidden := url.Values{
		"idempotency_key":    {"deadbeefdeadbeefdeadbeefdeadbeef"},
		"expected_active_id": {primitive.NewObjectID().Hex()},
	}
	got := string(FailedPublishHTML("https://pages.example.com/news/old",
		"IDEMPOTENCY_KEY_REQUIRED", true, hidden))

	for _, want := range []string{
		`name="idempotency_key" value="deadbeefdeadbeefdeadbeefdeadbeef"`,
		`name="expected_active_id" value="` + hidden.Get("expected_active_id") + `"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("retry form must re-emit %s in:\n%s", want, got)
		}
	}
	// Hidden inputs belong INSIDE the retry form, before the button.
	formAt := strings.Index(got, `<form method="POST" action="">`)
	keyAt := strings.Index(got, `name="idempotency_key"`)
	btnAt := strings.Index(got, "重试发布")
	if !(formAt >= 0 && formAt < keyAt && keyAt < btnAt) {
		t.Errorf("hidden fields must sit inside the retry form before the button in:\n%s", got)
	}

	// No hidden values passed: the retry form still renders, just empty.
	bare := string(FailedPublishHTML("", "PUBLICATION_STAGE_FAILED", true))
	if !strings.Contains(bare, `<form method="POST" action="">`) {
		t.Errorf("retryable failure must still offer a retry in:\n%s", bare)
	}
	if strings.Contains(bare, `name="idempotency_key"`) {
		t.Errorf("no hidden fields were passed, none should render in:\n%s", bare)
	}

	// Non-retryable failures render no form, so no hidden fields leak.
	nonRetry := string(FailedPublishHTML("", "IDEMPOTENCY_KEY_REQUIRED", false, hidden))
	if strings.Contains(nonRetry, "<form") {
		t.Errorf("non-retryable failure must not render a form in:\n%s", nonRetry)
	}
}

// TestMergeResultHTMLPublishLinks: requires_publish entries are the IDs whose
// edit page owns the Publish action (lane 4C) — each must link there.
func TestMergeResultHTMLPublishLinks(t *testing.T) {
	id := primitive.NewObjectID().Hex()
	got := string(MergeResultHTML(ForkMergeDisplay{
		Updated: 1, Created: 1,
		ContentIDs:      []string{primitive.NewObjectID().Hex()},
		RequiresPublish: []string{id},
	}))
	want := `href="/cm/content/` + id + `"`
	if !strings.Contains(got, want) {
		t.Errorf("requires_publish entry must link with %s, got:\n%s", want, got)
	}
}

// TestAdminProductPageLayoutRender: writeAdminProductPage must EXECUTE the
// shared Admin layout (it is template text — {{i18n}}, {{.CurrentUser.Email}},
// {{.CSRFField}}). Writing it verbatim shipped the placeholders as literal
// page content and stripped the layout's CSRF field.
func TestAdminProductPageLayoutRender(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	rr := rbacGet(t, h.AdminProductPublications,
		map[string]string{"id": primitive.NewObjectID().Hex()}, "admin")
	if rr.Code != http.StatusOK {
		t.Fatalf("AdminProductPublications: got %d, want 200", rr.Code)
	}
	page := rr.Body.String()
	if !strings.Contains(page, "发布历史") {
		t.Fatalf("page body missing in rendered output:\n%.500s", page)
	}
	for _, literal := range []string{"{{i18n", "{{.CurrentUser.Email}}", "{{.CSRFField}}", "{{if .AppVersion"} {
		if strings.Contains(page, literal) {
			t.Errorf("layout placeholder %q leaked as literal text — layout was not executed:\n%.500s", literal, page)
		}
	}
	// Only present when the layout ran with real template data (the forged
	// admin session email flows through {{.CurrentUser.Email}}).
	if !strings.Contains(page, "admin@test.local") {
		t.Errorf("layout did not execute with CurrentUser data:\n%.500s", page)
	}
}

// TestStampCSRFTokens: every server-rendered POST form gets the gorilla/csrf
// hidden field (the /cm router 403s token-less POSTs); GET forms and an empty
// token (no middleware, e.g. direct handler tests) stay untouched.
func TestStampCSRFTokens(t *testing.T) {
	body := `<form method="POST" action="/cm/x"><button>a</button></form>` +
		`<form method="GET" action="/cm/y"><button>b</button></form>` +
		`<form method="POST" action=""><button>c</button></form>`

	if got := stampCSRFTokens(body, ""); got != body {
		t.Errorf("empty token must leave the body unchanged, got:\n%s", got)
	}

	got := stampCSRFTokens(body, "tok123")
	if n := strings.Count(got, `name="gorilla.csrf.Token" value="tok123"`); n != 2 {
		t.Errorf("both POST forms must carry the CSRF field, got %d in:\n%s", n, got)
	}
	getStart := strings.Index(got, `<form method="GET"`)
	if getStart < 0 {
		t.Fatalf("GET form missing in:\n%s", got)
	}
	getEnd := strings.Index(got[getStart:], "</form>")
	if seg := got[getStart : getStart+getEnd+len("</form>")]; strings.Contains(seg, "gorilla.csrf.Token") {
		t.Errorf("GET form must not receive a CSRF field in:\n%s", got)
	}
}
