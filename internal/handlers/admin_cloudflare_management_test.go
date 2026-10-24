package handlers

import (
	"bytes"
	"context"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/metavert-io/lightcms/v7/internal/models"
	sd "github.com/metavert-io/lightcms/v7/internal/product/staticdelivery"
	"go.mongodb.org/mongo-driver/bson"
)

func TestCloudflareManagementListsAccountsAndScopedWorkerDialogs(t *testing.T) {
	a := sd.Account{Name: "<script>account</script>", AccountID: "0123456789abcdef0123456789abcdef", Status: "enabled"}
	a.ID = [12]byte{1}
	worker := sd.Target{Name: "Report site", WorkerName: "report-site", AccountRef: a.ID, Status: "draft", ConfigRevision: 1}
	worker.ID = [12]byte{2}
	other := a
	other.ID = [12]byte{3}
	other.Name = "Other account"
	otherWorker := worker
	otherWorker.ID = [12]byte{4}
	otherWorker.AccountRef = other.ID
	otherWorker.WorkerName = "other-worker"
	otherWorker.Status = "ready"
	otherWorker.CurrentRevision = 1
	for _, lang := range []string{"zh", "en"} {
		tmpl, err := template.New("cf").Funcs(adminTemplateFuncMap).Parse(adminTemplates["static_delivery"])
		if err != nil {
			t.Fatal(err)
		}
		data := map[string]any{"CF": cloudflareLabels(lang), "Accounts": []cloudflareAccountView{{Account: a, Workers: []sd.Target{worker}}, {Account: other, Workers: []sd.Target{otherWorker}}}, "Lang": lang, "CSRFField": template.HTML(`<input name="gorilla.csrf.Token" value="csrf-proof">`)}
		var b bytes.Buffer
		if err := tmpl.Execute(&b, data); err != nil {
			t.Fatal(err)
		}
		out := b.String()
		for _, want := range []string{`id="cloudflare-accounts"`, `id="cf-add-account"`, `id="cf-account-dialog"`, `id="cf-worker-form"`, `id="cf-worker-delete-form"`, `name="expected_config_revision"`, `name="account_ref"`, `name="confirm_delete"`, "report-site"} {
			if !strings.Contains(out, want) {
				t.Error("missing", want)
			}
		}
		if strings.Contains(out, a.Name) || strings.Contains(out, "private-token") {
			t.Fatal("account data not escaped")
		}
		for _, form := range regexp.MustCompile(`(?s)<form\b[^>]*method="post"[^>]*>.*?</form>`).FindAllString(out, -1) {
			if !strings.Contains(form, "csrf-proof") {
				t.Error("mutation form lacks CSRF", form[:min(180, len(form))])
			}
		}
		verify := regexp.MustCompile(`(?s)<form\b[^>]*id="cf-account-verify"[^>]*>(.*?)</form>`).FindStringSubmatch(out)
		if len(verify) != 2 || !strings.Contains(verify[1], `name="worker_name"`) || strings.Contains(verify[1], `name="token"`) {
			t.Fatal("existing scoped Token must be verifiable without replacing its credential")
		}
		node, err := exec.LookPath("node")
		if err != nil {
			t.Fatal(err)
		}
		for _, script := range regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllStringSubmatch(out, -1) {
			cmd := exec.Command(node, "--check")
			cmd.Stdin = strings.NewReader(script[1])
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("rendered JS invalid: %v %s", err, out)
			}
		}
		flow := exec.Command(node, "testdata/cloudflare_management_flow.cjs")
		flow.Stdin = strings.NewReader(out)
		if output, err := flow.CombinedOutput(); err != nil {
			t.Fatalf("management interactions: %v %s", err, output)
		}
	}
}

func TestCloudflareNavigationMovesOnlyRequestedEntries(t *testing.T) {
	tmpl, err := template.New("nav").Funcs(adminTemplateFuncMap).Parse(adminLayoutStart + adminLayoutEnd)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := tmpl.Execute(&b, map[string]any{"Lang": "zh", "CurrentUser": models.User{Role: models.RoleAdmin}}); err != nil {
		t.Fatal(err)
	}
	text := b.String()
	start, end := strings.Index(text, "<nav>"), strings.Index(text, "</nav>")
	if start < 0 || end < start {
		t.Fatal("missing sidebar nav")
	}
	nav := text[start:end]
	content, worker, artifact, templates, settings := strings.Index(nav, `href="/cm/content"`), strings.Index(nav, `href="/cm/static-delivery/maintenance"`), strings.Index(nav, `href="/cm/artifacts"`), strings.Index(nav, `href="/cm/templates"`), strings.Index(nav, `href="/cm/theme"`)
	if !(content < worker && worker < artifact && artifact < templates && templates < settings) {
		t.Fatal("requested content navigation order missing")
	}
	for _, path := range []string{"/cm/tasks", "/cm/operations", "/cm/static-delivery"} {
		if strings.Count(nav, `href="`+path+`"`) != 1 || strings.Index(nav, `href="`+path+`"`) < settings {
			t.Error("settings entry moved or duplicated", path)
		}
	}
	if !strings.Contains(nav, "Cloudflare 管理") || strings.Contains(nav, "Cloudflare 静态发布") {
		t.Fatal("Cloudflare menu not renamed")
	}
}

func TestCloudflareManagementFormsKeepScopeConfirmationAndContext(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()
	repo := sd.NewRepository(h.db)
	ctx := context.Background()
	if err := repo.EnsureIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	checkedWorker := ""
	accounts := sd.NewAccountService(repo, nil, func(_ context.Context, _ string, _ string, worker string) (sd.ConnectionCheck, error) {
		checkedWorker = worker
		return sd.ConnectionCheck{Readable: true}, nil
	})
	targets := sd.NewTargetService(repo, accounts, "form-installation", nil, nil, nil)
	a, err := accounts.Create(ctx, "Form account", "0123456789abcdef0123456789abcdef", "private-token", "")
	if err != nil {
		t.Fatal(err)
	}
	defer accounts.Delete(ctx, a.ID)
	target, err := targets.Create(ctx, a.ID, "Reports", "form-worker", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	h.SetStaticDeliveryAdmin(&StaticDeliveryAdmin{Accounts: accounts, Targets: targets})
	post := func(kind, id, action string, body url.Values) *httptest.ResponseRecorder {
		r := sessionReq(http.MethodPost, "/cm/static-delivery/"+kind+"/"+id+"/"+action, strings.NewReader(body.Encode()), map[string]string{"kind": kind, "id": id, "action": action})
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.AdminStaticDeliveryAction(w, r)
		return w
	}
	body := url.Values{"account_ref": {a.ID.Hex()}}
	if w := post("accounts", a.ID.Hex(), "verify", url.Values{"worker_name": {"scoped-worker"}}); w.Code != 303 || checkedWorker != "scoped-worker" {
		t.Fatalf("existing scoped Token not verified: %d %s", w.Code, checkedWorker)
	}
	if w := post("targets", target.ID.Hex(), "delete", body); w.Code != 428 {
		t.Fatalf("delete without consent: %d", w.Code)
	}
	body.Set("confirm_delete", "yes")
	body.Set("account_ref", "000000000000000000000001")
	if w := post("targets", target.ID.Hex(), "delete", body); w.Code != 404 {
		t.Fatalf("cross account action: %d", w.Code)
	}
	body = url.Values{"account_ref": {a.ID.Hex()}, "name": {"Updated name"}, "worker_name": {target.WorkerName}, "environment": {"test"}, "public_origin": {target.Origin}, "expected_config_revision": {"bad"}, "expected_management_revision": {"0"}}
	if w := post("targets", target.ID.Hex(), "update", body); w.Code != 400 {
		t.Fatalf("bad version: %d", w.Code)
	}
	body.Set("expected_config_revision", "1")
	if w := post("targets", target.ID.Hex(), "update", body); w.Code != 303 || !strings.Contains(w.Header().Get("Location"), "account="+a.ID.Hex()) {
		t.Fatalf("dialog context lost after update: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := post("targets", target.ID.Hex(), "update", body); w.Code != 409 || !strings.Contains(w.Body.String(), sd.CodeRevisionConflict) || strings.Contains(w.Body.String(), "private-token") {
		t.Fatalf("stale update not explained safely: %d", w.Code)
	}
	w := httptest.NewRecorder()
	h.AdminStaticDeliveryPage(w, sessionReq(http.MethodGet, "/cm/static-delivery?account="+a.ID.Hex(), nil, nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Updated name") || strings.Contains(w.Body.String(), "private-token") {
		t.Fatal("updated configuration not listed safely")
	}
	body = url.Values{"account_ref": {a.ID.Hex()}, "confirm_delete": {"yes"}}
	if w := post("targets", target.ID.Hex(), "delete", body); w.Code != 303 {
		t.Fatalf("Worker local delete: %d", w.Code)
	}
	if n, _ := h.db.Collection("static_delivery_targets").CountDocuments(ctx, bson.M{"_id": target.ID}); n != 0 {
		t.Fatal("target not removed")
	}
	if w := post("accounts", a.ID.Hex(), "delete", url.Values{}); w.Code != 428 {
		t.Fatalf("account consent not required: %d", w.Code)
	}
	if w := post("accounts", a.ID.Hex(), "delete", url.Values{"confirm_delete": {"yes"}}); w.Code != 303 {
		t.Fatalf("account delete: %d", w.Code)
	}
	if _, err := accounts.Get(ctx, a.ID); sd.CodeOf(err) != sd.CodeNotFound {
		t.Fatal("account not removed", err)
	}
}
