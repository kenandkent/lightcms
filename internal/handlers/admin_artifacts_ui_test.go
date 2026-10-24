package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/sessions"
	"github.com/metavert-io/lightcms/v7/internal/auth"
	"github.com/metavert-io/lightcms/v7/internal/product/artifacts"
	"go.mongodb.org/mongo-driver/mongo"
)

type artifactManagementFake struct {
	detail            artifacts.ObjectDetail
	err               error
	calls             []string
	plan              artifacts.CleanupPlan
	query             artifacts.ListQuery
	owner, digest     string
	confirmed, pinned bool
}

func (f *artifactManagementFake) ListObjects(_ context.Context, q artifacts.ListQuery) (artifacts.ObjectPage, error) {
	f.query = q
	return artifacts.ObjectPage{Items: []artifacts.ObjectSummary{}, Page: 1, Limit: 25}, f.err
}
func (f *artifactManagementFake) GetObject(_ context.Context, hash string) (artifacts.ObjectDetail, error) {
	f.calls = append(f.calls, "get:"+hash)
	return f.detail, f.err
}
func (f *artifactManagementFake) PreviewCleanup(_ context.Context, owner string) (artifacts.CleanupPlan, error) {
	f.owner = owner
	f.calls = append(f.calls, "preview")
	return f.plan, f.err
}
func (f *artifactManagementFake) ConfirmCleanup(_ context.Context, owner, digest string, confirmed bool) error {
	f.owner, f.digest, f.confirmed = owner, digest, confirmed
	f.calls = append(f.calls, "confirm:"+owner+":"+digest)
	return f.err
}
func (f *artifactManagementFake) ResumeCleanup(_ context.Context, owner, digest string, confirmed bool) error {
	f.owner, f.digest, f.confirmed = owner, digest, confirmed
	f.calls = append(f.calls, "resume:"+owner+":"+digest)
	return f.err
}
func (f *artifactManagementFake) PinObject(_ context.Context, hash string, pinned bool) error {
	f.pinned = pinned
	f.calls = append(f.calls, "pin:"+hash)
	return f.err
}

// Existing maintenance implementations need not adopt the browsing interface.
type legacyArtifactMaintenanceFake struct{ delegate *artifactManagementFake }

func (f legacyArtifactMaintenanceFake) PreviewCleanup(ctx context.Context, owner string) (artifacts.CleanupPlan, error) {
	return f.delegate.PreviewCleanup(ctx, owner)
}
func (f legacyArtifactMaintenanceFake) ConfirmCleanup(ctx context.Context, owner, digest string, confirmed bool) error {
	return f.delegate.ConfirmCleanup(ctx, owner, digest, confirmed)
}
func (f legacyArtifactMaintenanceFake) ResumeCleanup(ctx context.Context, owner, digest string, confirmed bool) error {
	return f.delegate.ResumeCleanup(ctx, owner, digest, confirmed)
}

func TestArtifactManagementLegacyMaintenanceAndPostArgumentsRemainCompatible(t *testing.T) {
	hash, digest := strings.Repeat("a", 64), strings.Repeat("b", 64)
	fake := &artifactManagementFake{plan: artifacts.CleanupPlan{Digest: digest, Items: []artifacts.Object{{Hash: hash, Size: 42}}, ExpiresAt: time.Now().Add(time.Minute)}}
	h := artifactUIHandler(legacyArtifactMaintenanceFake{fake})
	h.db = testDB(t)
	w := httptest.NewRecorder()
	h.AdminArtifactsPage(w, sessionReq(http.MethodGet, "/cm/artifacts?lang=en", nil, nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "does not support artifact browsing") || !strings.Contains(w.Body.String(), "Resume interrupted cleanup") {
		t.Fatalf("legacy maintenance page unavailable: %d %s", w.Code, w.Body.String())
	}
	for _, action := range []string{"preview", "confirm", "resume"} {
		t.Run(action, func(t *testing.T) {
			body := url.Values{"action": {action}, "owner": {"attacker"}, "digest": {digest}, "confirmed": {"yes"}}
			r := sessionReq(http.MethodPost, "/cm/artifacts?lang=en&q=aaa&state=available", strings.NewReader(body.Encode()), nil)
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			h.AdminArtifactsPage(w, r)
			if w.Code != http.StatusOK || fake.owner != "000000000000000000000001" {
				t.Fatalf("legacy %s used submitted owner or failed: %d %q", action, w.Code, fake.owner)
			}
			if action == "preview" {
				if !strings.Contains(w.Body.String(), digest) || !strings.Contains(w.Body.String(), hash) || !strings.Contains(w.Body.String(), `name="confirmed"`) {
					t.Fatal("preview did not render the original candidates and explicit confirmation")
				}
			} else if fake.digest != digest || !fake.confirmed {
				t.Fatalf("%s altered the plan or confirmation: %q %v", action, fake.digest, fake.confirmed)
			}
		})
	}
	fake.err = errors.New("token:/Users/ken/private-artifacts")
	body := url.Values{"action": {"resume"}, "digest": {digest}, "confirmed": {"yes"}}
	r := sessionReq(http.MethodPost, "/cm/artifacts?lang=en", strings.NewReader(body.Encode()), nil)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	h.AdminArtifactsPage(w, r)
	if strings.Contains(w.Body.String(), "private-artifacts") || !strings.Contains(w.Body.String(), `value="`+digest+`"`) || !strings.Contains(w.Body.String(), "data-open-on-load") {
		t.Fatal("interrupted cleanup lost its original plan or exposed a private error")
	}
}

func TestArtifactManagementForwardsBoundedFiltersAndPinIntent(t *testing.T) {
	fake := &artifactManagementFake{}
	h := artifactUIHandler(fake)
	h.db = testDB(t)
	w := httptest.NewRecorder()
	h.AdminArtifactsPage(w, sessionReq(http.MethodGet, "/cm/artifacts?q=abc&state=available&pin=pinned&page=2&limit=9999", nil, nil))
	if w.Code != http.StatusOK || fake.query.Search != "abc" || fake.query.State != "available" || fake.query.Pin != "pinned" || fake.query.Page != 2 || fake.query.Limit != 25 {
		t.Fatalf("request bypassed list bounds/filters: %d %+v", w.Code, fake.query)
	}
	for _, action := range []string{"pin", "unpin"} {
		body := url.Values{"action": {action}, "hash": {strings.Repeat("a", 64)}, "confirmed": {"yes"}}
		r := sessionReq(http.MethodPost, "/cm/artifacts", strings.NewReader(body.Encode()), nil)
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.AdminArtifactsPage(w, r)
		if w.Code != http.StatusOK || fake.pinned != (action == "pin") || fake.calls[len(fake.calls)-1] != "pin:"+strings.Repeat("a", 64) {
			t.Fatalf("pin intent changed for %s: %d %+v", action, w.Code, fake)
		}
	}
	fake.calls = nil
	w = httptest.NewRecorder()
	h.AdminArtifactsPage(w, sessionReq(http.MethodGet, "/cm/artifacts?format=json&hash=../../secret", nil, nil))
	if w.Code != http.StatusBadRequest || len(fake.calls) != 0 {
		t.Fatal("unsafe object lookup reached the browser service")
	}
	fake.err = mongo.ErrNoDocuments
	w = httptest.NewRecorder()
	h.AdminArtifactsPage(w, sessionReq(http.MethodGet, "/cm/artifacts?format=json&hash="+strings.Repeat("a", 64), nil, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing object returned %d", w.Code)
	}
}

func artifactUIHandler(f artifactMaintenance) *Handler {
	return &Handler{auth: auth.NewManager(sessions.NewCookieStore([]byte(testSessionSecret)), nil, nil), staticDelivery: &StaticDeliveryAdmin{Artifacts: f}}
}

func TestArtifactManagementDetailsRequireAdminAndHideServiceErrors(t *testing.T) {
	hash := strings.Repeat("a", 64)
	fake := &artifactManagementFake{detail: artifacts.ObjectDetail{ObjectSummary: artifacts.ObjectSummary{Object: artifacts.Object{Hash: hash, Size: 42, MIME: "text/html", State: "available"}}}}
	h := artifactUIHandler(fake)
	h.db = testDB(t)
	request := sessionReq(http.MethodGet, "/cm/artifacts?format=json&hash="+hash, nil, nil)
	w := httptest.NewRecorder()
	h.AdminArtifactsPage(w, request)
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("metadata response: %d %s", w.Code, w.Body.String())
	}
	var detail artifacts.ObjectDetail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil || detail.Hash != hash {
		t.Fatalf("detail endpoint did not return metadata JSON: %+v %v", detail, err)
	}
	fake.err = errors.New("private-token /Users/ken/private-path upstream body")
	w = httptest.NewRecorder()
	h.AdminArtifactsPage(w, request)
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "private-token") || strings.Contains(w.Body.String(), "/Users") {
		t.Fatalf("unsafe error response: %d %s", w.Code, w.Body.String())
	}
	fake.calls = nil
	w = httptest.NewRecorder()
	h.AdminArtifactsPage(w, httptest.NewRequest(http.MethodGet, "/cm/artifacts?format=json&hash="+hash, nil))
	if w.Code != http.StatusForbidden || len(fake.calls) != 0 {
		t.Fatalf("anonymous detail access: %d %+v", w.Code, fake.calls)
	}
	store := sessions.NewCookieStore([]byte(testSessionSecret))
	request = httptest.NewRequest(http.MethodGet, "/cm/artifacts?format=json&hash="+hash, nil)
	session, _ := store.New(request, "lightcms-session")
	session.Values["authenticated"], session.Values["user_id"], session.Values["user_email"], session.Values["user_role"] = true, "000000000000000000000002", "editor@localhost", "editor"
	response := httptest.NewRecorder()
	if err := session.Save(request, response); err != nil {
		t.Fatal(err)
	}
	request.AddCookie(response.Result().Cookies()[0])
	w = httptest.NewRecorder()
	h.AdminArtifactsPage(w, request)
	if w.Code != http.StatusForbidden || len(fake.calls) != 0 {
		t.Fatalf("editor detail access: %d %+v", w.Code, fake.calls)
	}
}

func TestArtifactManagementRejectsMissingCleanupOrUnpinConfirmation(t *testing.T) {
	fake := &artifactManagementFake{}
	h := artifactUIHandler(fake)
	h.db = testDB(t)
	for _, action := range []string{"confirm", "resume", "unpin"} {
		body := url.Values{"action": {action}, "hash": {strings.Repeat("a", 64)}, "digest": {strings.Repeat("b", 64)}}
		request := sessionReq(http.MethodPost, "/cm/artifacts", strings.NewReader(body.Encode()), nil)
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.AdminArtifactsPage(w, request)
		if w.Code != http.StatusBadRequest || len(fake.calls) != 0 {
			t.Fatalf("unconfirmed %s reached mutation: %d %+v", action, w.Code, fake.calls)
		}
	}
}

func TestArtifactManagementDoesNotPresentReadFailuresAsAnEmptyCatalog(t *testing.T) {
	tmpl, err := template.New("artifacts").Funcs(adminTemplateFuncMap).Parse(adminTemplates["artifacts"])
	if err != nil {
		t.Fatal(err)
	}
	for _, failed := range []bool{false, true} {
		data := map[string]any{"Lang": "en", "CanBrowse": true, "Listing": artifacts.ObjectPage{Items: []artifacts.ObjectSummary{}, Page: 1}}
		if failed {
			data["ListError"] = "Could not read the catalog"
		}
		var out bytes.Buffer
		if err := tmpl.Execute(&out, data); err != nil {
			t.Fatal(err)
		}
		empty := strings.Contains(out.String(), "<strong>No matching artifacts</strong>")
		if empty == failed {
			t.Fatalf("catalog empty state failed=%v empty=%v", failed, empty)
		}
	}
}

func TestArtifactManagementRendersListDetailsAndCleanupDialogs(t *testing.T) {
	tmpl, err := template.New("artifacts").Funcs(adminTemplateFuncMap).Parse(adminTemplates["artifacts"])
	if err != nil {
		t.Fatal(err)
	}
	for _, lang := range []string{"zh", "en"} {
		t.Run(lang, func(t *testing.T) {
			var out bytes.Buffer
			err := tmpl.Execute(&out, map[string]any{
				"Lang": lang, "CSRFToken": "csrf-proof", "CanPin": true, "CanBrowse": true,
				"Listing": map[string]any{"Page": 1, "HasNext": true, "Items": []map[string]any{{
					"Hash": strings.Repeat("a", 64), "MIME": "text/html", "State": "available", "Size": 123,
					"CreatedAt": time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC), "Pinned": true, "ReferenceCount": 2,
				}, {"Hash": strings.Repeat("d", 64), "MIME": "image/png", "State": "available", "Size": 50, "CreatedAt": time.Now()}}},
				"Plan": artifacts.CleanupPlan{Digest: strings.Repeat("b", 64), ExpiresAt: time.Now(), Items: []artifacts.Object{{Hash: strings.Repeat("c", 64), Size: 456}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			html := out.String()
			for _, want := range []string{`id="artifact-list"`, `data-artifact-detail="` + strings.Repeat("a", 64), `<dialog id="artifact-detail-dialog"`, `<dialog id="artifact-cleanup-dialog"`, `aria-labelledby="artifact-detail-title"`, `name="confirmed"`, `name="gorilla.csrf.Token"`, `csrf-proof`, `text/html`, `name="state"`, `name="q"`} {
				if !strings.Contains(html, want) {
					t.Errorf("rendered management UI missing %s", want)
				}
			}
			if lang == "en" && (!strings.Contains(html, "Artifact management") || !strings.Contains(html, "Preview cleanup")) {
				t.Fatal("artifact controls do not use the selected language")
			}
			node, err := exec.LookPath("node")
			if err != nil {
				t.Fatal(err)
			}
			for _, script := range regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllStringSubmatch(html, -1) {
				cmd := exec.Command(node, "--check")
				cmd.Stdin = strings.NewReader(script[1])
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("rendered script invalid: %v %s", err, output)
				}
			}
			flow := exec.Command(node, "testdata/artifact_management_flow.cjs")
			flow.Stdin = bytes.NewReader(out.Bytes())
			if output, err := flow.CombinedOutput(); err != nil {
				t.Fatalf("artifact dialog flow: %v %s", err, output)
			}
		})
	}
}
