package handlers

// Lane 1A regression: destructive /cm POST handlers must enforce the same
// granular RBAC permissions as their /api/v1 equivalents. A viewer session
// must get 403 (not a silent success), while legit admin/editor flows keep
// working.
//
// Permission matrix (mirrors api_content.go / api_templates.go /
// api_snippets.go / api_forks.go):
//   DeleteContent  -> content.delete   (API: APIDeleteContent)
//   CreateTemplate -> template.create  (API: APICreateTemplate)
//   UpdateTemplate -> template.edit    (API: APIUpdateTemplate)
//   DeleteTemplate -> template.delete  (API: APIDeleteTemplate)
//   CreateSnippet  -> template.edit    (API: APICreateSnippet)
//   UpdateSnippet  -> template.edit    (API: APIUpdateSnippet)
//   DeleteSnippet  -> template.edit    (API: APIDeleteSnippet)
// Fork + user handlers were already granular (reviewed-OK) and are locked in
// here: merge/archive/delete -> fork.merge, create/fork-page/remove ->
// fork.create, toggle-disable/reset-password -> user.manage.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/sessions"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// rbacSessionReq forges an authenticated session cookie for the given role,
// mirroring sessionReq (admin-only) but parameterized so viewer/editor/low-
// priv sessions can exercise the admin UI handlers. Direct handler calls
// bypass CSRF middleware, matching the existing postForm/getPage pattern.
func rbacSessionReq(t *testing.T, role, method, target string, body io.Reader, vars map[string]string) *http.Request {
	t.Helper()
	store := sessions.NewCookieStore([]byte(testSessionSecret))
	req := httptest.NewRequest(method, target, body)
	rec := httptest.NewRecorder()
	sess, _ := store.New(req, "lightcms-session")
	sess.Values["authenticated"] = true
	sess.Values["user_id"] = "0000000000000000000000" + role[:1] + "1"
	sess.Values["user_email"] = role + "@test.local"
	sess.Values["user_role"] = role
	_ = sess.Save(req, rec)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	if vars != nil {
		req = mux.SetURLVars(req, vars)
	}
	return req
}

// rbacPost invokes a POST handler as the given role with form values.
func rbacPost(t *testing.T, handler http.HandlerFunc, form url.Values, vars map[string]string, role string) *httptest.ResponseRecorder {
	t.Helper()
	if form == nil {
		form = url.Values{}
	}
	req := rbacSessionReq(t, role, http.MethodPost, "/cm/x", strings.NewReader(form.Encode()), vars)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	handler(rr, req)
	return rr
}

func TestRBAC_DeleteContent_ViewerForbidden(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()
	db := testDB(t)
	tmpl := seedTemplate(t, db, "Page", "page")
	contentID := seedContent(t, db, tmpl, "Victim", "victim", "/victim")

	if rr := rbacPost(t, h.DeleteContent, nil, map[string]string{"id": contentID.Hex()}, "viewer"); rr.Code != http.StatusForbidden {
		t.Fatalf("viewer DeleteContent: got %d, want 403", rr.Code)
	}
	// Page must be untouched.
	var doc bson.M
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.Collection("content").FindOne(ctx, bson.M{"_id": mustObjID(t, contentID.Hex())}).Decode(&doc); err != nil {
		t.Fatalf("seeded content missing after blocked delete: %v", err)
	}
	if del, _ := doc["deleted"].(bool); del {
		t.Fatal("viewer DeleteContent mutated the page despite 403")
	}
}

func TestRBAC_DeleteContent_EditorAllowed_AdminAllowed(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()
	db := testDB(t)
	tmpl := seedTemplate(t, db, "Page", "page")

	// Editor carries content.delete — must NOT be forbidden.
	editorID := seedContent(t, db, tmpl, "Ed", "ed", "/ed")
	if rr := rbacPost(t, h.DeleteContent, nil, map[string]string{"id": editorID.Hex()}, "editor"); rr.Code == http.StatusForbidden {
		t.Fatalf("editor DeleteContent: got 403, want allowed (editor has content.delete)")
	}

	// Admin flow still works.
	adminID := seedContent(t, db, tmpl, "Ad", "ad", "/ad")
	if rr := rbacPost(t, h.DeleteContent, nil, map[string]string{"id": adminID.Hex()}, "admin"); rr.Code == http.StatusForbidden {
		t.Fatalf("admin DeleteContent: got 403, want allowed")
	}
}

func TestRBAC_CreateTemplate_ViewerAndEditorForbidden(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	newForm := func() url.Values {
		f := url.Values{}
		f.Set("name", "RBAC Template")
		f.Set("html_layout", "<html><body>{{.Body}}</body></html>")
		return f
	}
	if rr := rbacPost(t, h.CreateTemplate, newForm(), nil, "viewer"); rr.Code != http.StatusForbidden {
		t.Fatalf("viewer CreateTemplate: got %d, want 403", rr.Code)
	}
	// Editors have no template.create — also forbidden (locks the matrix).
	if rr := rbacPost(t, h.CreateTemplate, newForm(), nil, "editor"); rr.Code != http.StatusForbidden {
		t.Fatalf("editor CreateTemplate: got %d, want 403", rr.Code)
	}
	if rr := rbacPost(t, h.CreateTemplate, newForm(), nil, "admin"); rr.Code == http.StatusForbidden {
		t.Fatalf("admin CreateTemplate: got 403, want allowed")
	}
}

func TestRBAC_UpdateTemplate_ViewerForbidden_AdminAllowed(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()
	db := testDB(t)
	tmplID := seedTemplate(t, db, "RBAC", "rbac")

	form := url.Values{}
	form.Set("name", "RBAC Renamed")
	form.Set("html_layout", "<html><body>{{.Body}}</body></html>")
	vars := map[string]string{"id": tmplID.Hex()}

	if rr := rbacPost(t, h.UpdateTemplate, form, vars, "viewer"); rr.Code != http.StatusForbidden {
		t.Fatalf("viewer UpdateTemplate: got %d, want 403", rr.Code)
	}
	if rr := rbacPost(t, h.UpdateTemplate, form, vars, "editor"); rr.Code != http.StatusForbidden {
		t.Fatalf("editor UpdateTemplate: got %d, want 403", rr.Code)
	}
	if rr := rbacPost(t, h.UpdateTemplate, form, vars, "admin"); rr.Code == http.StatusForbidden {
		t.Fatalf("admin UpdateTemplate: got 403, want allowed")
	}
}

func TestRBAC_DeleteTemplate_ViewerForbidden_AdminAllowed(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()
	db := testDB(t)

	victim := seedTemplate(t, db, "Victim", "victim")
	if rr := rbacPost(t, h.DeleteTemplate, nil, map[string]string{"id": victim.Hex()}, "viewer"); rr.Code != http.StatusForbidden {
		t.Fatalf("viewer DeleteTemplate: got %d, want 403", rr.Code)
	}
	// Victim must survive the blocked delete.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if n, _ := db.Collection("templates").CountDocuments(ctx, bson.M{"_id": victim}); n != 1 {
		t.Fatal("viewer DeleteTemplate removed the template despite 403")
	}

	other := seedTemplate(t, db, "Other", "other")
	if rr := rbacPost(t, h.DeleteTemplate, nil, map[string]string{"id": other.Hex()}, "admin"); rr.Code == http.StatusForbidden {
		t.Fatalf("admin DeleteTemplate: got 403, want allowed")
	}
}

func TestRBAC_CreateSnippet_ViewerForbidden_AdminAllowed(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	form := url.Values{}
	form.Set("name", "rbac-snippet")
	form.Set("html", "<div>hi</div>")

	if rr := rbacPost(t, h.CreateSnippet, form, nil, "viewer"); rr.Code != http.StatusForbidden {
		t.Fatalf("viewer CreateSnippet: got %d, want 403", rr.Code)
	}
	if rr := rbacPost(t, h.CreateSnippet, form, nil, "editor"); rr.Code != http.StatusForbidden {
		t.Fatalf("editor CreateSnippet: got %d, want 403 (snippets need template.edit, admin-only)", rr.Code)
	}
	if rr := rbacPost(t, h.CreateSnippet, form, nil, "admin"); rr.Code == http.StatusForbidden {
		t.Fatalf("admin CreateSnippet: got 403, want allowed")
	}
}

func TestRBAC_UpdateSnippet_ViewerForbidden_AdminAllowed(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	ctx := context.Background()
	snip, err := h.snippetService.CreateSnippet(ctx, "rbac-update", "<p>old</p>")
	if err != nil {
		t.Fatalf("seed snippet: %v", err)
	}
	form := url.Values{}
	form.Set("name", "rbac-update")
	form.Set("html", "<p>new</p>")
	vars := map[string]string{"id": snip.ID.Hex()}

	if rr := rbacPost(t, h.UpdateSnippet, form, vars, "viewer"); rr.Code != http.StatusForbidden {
		t.Fatalf("viewer UpdateSnippet: got %d, want 403", rr.Code)
	}
	after, _ := h.snippetService.GetSnippet(ctx, snip.ID)
	if after.HTML == "<p>new</p>" {
		t.Fatal("viewer UpdateSnippet mutated the snippet despite 403")
	}
	if rr := rbacPost(t, h.UpdateSnippet, form, vars, "admin"); rr.Code == http.StatusForbidden {
		t.Fatalf("admin UpdateSnippet: got 403, want allowed")
	}
}

func TestRBAC_DeleteSnippet_ViewerForbidden_AdminAllowed(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	ctx := context.Background()
	victim, err := h.snippetService.CreateSnippet(ctx, "rbac-del", "<p>bye</p>")
	if err != nil {
		t.Fatalf("seed snippet: %v", err)
	}
	vars := map[string]string{"id": victim.ID.Hex()}
	if rr := rbacPost(t, h.DeleteSnippet, nil, vars, "viewer"); rr.Code != http.StatusForbidden {
		t.Fatalf("viewer DeleteSnippet: got %d, want 403", rr.Code)
	}
	if _, err := h.snippetService.GetSnippet(ctx, victim.ID); err != nil {
		t.Fatal("viewer DeleteSnippet removed the snippet despite 403")
	}

	other, err := h.snippetService.CreateSnippet(ctx, "rbac-del2", "<p>bye</p>")
	if err != nil {
		t.Fatalf("seed snippet: %v", err)
	}
	if rr := rbacPost(t, h.DeleteSnippet, nil, map[string]string{"id": other.ID.Hex()}, "admin"); rr.Code == http.StatusForbidden {
		t.Fatalf("admin DeleteSnippet: got 403, want allowed")
	}
}

// Reviewed-OK lock-in: handlers_fork.go already enforces fork.create for
// create/fork-page/remove and fork.merge for merge/archive/delete. Viewers
// (who hold neither) must get 403.
func TestRBAC_ForkWrite_ViewerForbidden(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fork, err := h.forkService.Create(ctx, "RBAC WS", "d", primitive.NewObjectID(), "admin@test.local")
	if err != nil {
		t.Fatalf("seed fork: %v", err)
	}
	fv := map[string]string{"id": fork.ID.Hex()}

	if rr := rbacPost(t, h.CreateFork, url.Values{"name": {"nope"}}, nil, "viewer"); rr.Code != http.StatusForbidden {
		t.Fatalf("viewer CreateFork: got %d, want 403", rr.Code)
	}
	if rr := rbacPost(t, h.MergeFork, nil, fv, "viewer"); rr.Code != http.StatusForbidden {
		t.Fatalf("viewer MergeFork: got %d, want 403", rr.Code)
	}
	if rr := rbacPost(t, h.ArchiveFork, nil, fv, "viewer"); rr.Code != http.StatusForbidden {
		t.Fatalf("viewer ArchiveFork: got %d, want 403", rr.Code)
	}
	if rr := rbacPost(t, h.DeleteForkHandler, nil, fv, "viewer"); rr.Code != http.StatusForbidden {
		t.Fatalf("viewer DeleteFork: got %d, want 403", rr.Code)
	}
	// Editor may create but must not merge (fork.merge is admin-only).
	if rr := rbacPost(t, h.MergeFork, nil, fv, "editor"); rr.Code != http.StatusForbidden {
		t.Fatalf("editor MergeFork: got %d, want 403", rr.Code)
	}
}

// Reviewed-OK lock-in: ToggleUserDisabled / ResetUserPassword already require
// user.manage. A viewer is redirected without effect (never succeeds).
func TestRBAC_UserMutations_ViewerBlocked(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	adminUser, _ := h.auth.ValidateCredentials(ctx, "admin@localhost", "admin123")
	var createdBy primitive.ObjectID
	if adminUser != nil {
		createdBy = adminUser.ID
	}
	target, _, err := h.userService.CreateUser(ctx, "rbac-target@example.com", "Target", "editor", createdBy)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	vars := map[string]string{"id": target.ID.Hex()}

	if rr := rbacPost(t, h.ToggleUserDisabled, nil, vars, "viewer"); rr.Code == http.StatusOK {
		t.Fatalf("viewer ToggleUserDisabled: got 200 success, want blocked (redirect/403)")
	}
	after, _ := h.userService.GetByID(ctx, target.ID)
	if after == nil || after.Disabled {
		t.Fatal("viewer ToggleUserDisabled changed account state")
	}
	if rr := rbacPost(t, h.ResetUserPassword, nil, vars, "viewer"); rr.Code == http.StatusOK {
		t.Fatalf("viewer ResetUserPassword: got 200 success, want blocked (redirect/403)")
	}
}
