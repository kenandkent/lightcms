package handlers

import (
	"bytes"
	sd "github.com/metavert-io/lightcms/v7/internal/product/staticdelivery"
	"html/template"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func TestAdminStaticDeliveryTemplateUsesSafeForms(t *testing.T) {
	a := sd.Account{Name: `<script>bad</script>`, AccountID: "0123456789abcdef0123456789abcdef", Status: "enabled"}
	a.ID = [12]byte{1}
	a.Verification.Readable = true
	tmpl, e := template.New("admin").Funcs(adminTemplateFuncMap).Parse(adminTemplates["static_delivery"])
	if e != nil {
		t.Fatal(e)
	}
	var b bytes.Buffer
	e = tmpl.Execute(&b, map[string]any{"CF": cloudflareLabels("zh"), "Accounts": []cloudflareAccountView{{Account: a, Workers: []sd.Target{}}}, "CSRFField": template.HTML(`<input name="gorilla.csrf.Token" value="csrf-proof">`), "Lang": "zh", "IsAuthenticated": true})
	if e != nil {
		t.Fatal(e)
	}
	html := b.String()
	for _, want := range []string{"Cloudflare 管理", "账号读取已验证", "部署写入尚未验证", "type=\"password\"", "csrf-proof", "id=\"account-token\"", "id=\"cf-worker-account\""} {
		if !strings.Contains(html, want) {
			t.Error("missing ", want)
		}
	}
	if strings.Contains(html, a.Name) || strings.Contains(html, "ciphertext") || strings.Contains(html, "value=\"private-token") {
		t.Fatal("unsafe template output")
	}
}
func TestAdminStaticDeliveryUnauthenticatedRejected(t *testing.T) {
	h := &Handler{}
	for _, post := range []bool{false, true} {
		r := httptest.NewRequest("GET", "/cm/static-delivery", nil)
		w := httptest.NewRecorder()
		if post {
			r.Method = "POST"
			h.AdminStaticDeliveryAction(w, r)
		} else {
			h.AdminStaticDeliveryPage(w, r)
		}
		if w.Code != 403 {
			t.Fatalf("unauthenticated %d", w.Code)
		}
	}
}

func TestAdminStaticPublishPageHasRealSelectionAndConfirmation(t *testing.T) {
	tmpl, e := template.New("publish").Funcs(adminTemplateFuncMap).Parse(adminTemplates["static_delivery_publish"])
	if e != nil {
		t.Fatal(e)
	}
	var b bytes.Buffer
	if e = tmpl.Execute(&b, map[string]any{"Lang": "zh", "CanPublish": true, "SelectedIDs": []string{"000000000000000000000001"}, "CSRFToken": "csrf-value", "Selection": []map[string]string{{"Title": "Published report"}}}); e != nil {
		t.Fatal(e)
	}
	for _, want := range []string{"Published report", "预览发布", "确认发布", "原站点", "static-exports", "deployment-preview"} {
		if !strings.Contains(b.String(), want) {
			t.Error("missing", want)
		}
	}
	if strings.Contains(b.String(), `id="export-start"`) {
		t.Fatal("manual export step still present")
	}
	// Check the rendered JavaScript, not the unexpanded Go template. In
	// particular html/template must serialize IDs and CSRF as JS strings.
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("Node is required for the static publish UI gate")
	}
	for _, script := range regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllStringSubmatch(b.String(), -1) {
		cmd := exec.Command(node, "--check")
		cmd.Stdin = strings.NewReader(script[1])
		if out, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("rendered script invalid: %v %s", e, out)
		}
	}
	flow := exec.Command(node, "testdata/static_publish_flow.cjs")
	flow.Stdin = bytes.NewReader(b.Bytes())
	if out, e := flow.CombinedOutput(); e != nil {
		t.Fatalf("automatic export/preview flow: %v %s", e, out)
	}
	if fixture := os.Getenv("STATIC_DELIVERY_BROWSER_FIXTURE"); fixture != "" {
		if e = os.WriteFile(fixture, b.Bytes(), 0600); e != nil {
			t.Fatal(e)
		}
	}
}
