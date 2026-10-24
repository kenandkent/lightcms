package handlers

import (
	"bytes"
	"github.com/metavert-io/lightcms/v7/internal/product/artifacts"
	"html/template"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestArtifactMaintenanceRequiresAdminAndExplicitConfirmation(t *testing.T) {
	h := &Handler{}
	for _, method := range []string{"GET", "POST"} {
		w := httptest.NewRecorder()
		h.AdminArtifactsPage(w, httptest.NewRequest(method, "/cm/artifacts", nil))
		if w.Code != 403 {
			t.Fatalf("unauthenticated status %d", w.Code)
		}
	}
	tmpl, err := template.New("artifacts").Funcs(adminTemplateFuncMap).Parse(adminTemplates["artifacts"])
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = tmpl.Execute(&out, map[string]any{"Lang": "zh", "CSRFToken": "csrf", "Plan": artifacts.CleanupPlan{Digest: "confirmed-digest", Items: []artifacts.Object{{Hash: "hash", Size: 3}}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`name="gorilla.csrf.Token"`, `name="confirmed"`, `confirmed-digest`, "预览"} {
		if !strings.Contains(out.String(), want) {
			t.Fatal("missing confirmation control", want)
		}
	}
}
