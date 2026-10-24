package handlers

import (
	"bytes"
	"html/template"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func TestDeliveryMaintenanceHasExplicitPreviewConfirmation(t *testing.T) {
	h := &Handler{}
	w := httptest.NewRecorder()
	h.AdminDeliveryMaintenance(w, httptest.NewRequest("GET", "/cm/static-delivery/maintenance", nil))
	if w.Code != 403 {
		t.Fatalf("unauthenticated: %d", w.Code)
	}
	tmpl, err := template.New("maintenance").Funcs(adminTemplateFuncMap).Parse(adminTemplates["delivery_maintenance"])
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = tmpl.Execute(&out, map[string]any{"Lang": "zh", "CSRFToken": "csrf"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"rollback_pages", "rollback_site", "withdraw", "change_origin", "change_environment", "confirmation_digest", "respond-async"} {
		if !strings.Contains(out.String(), want) {
			t.Fatal("missing lifecycle action", want)
		}
	}
	for _, script := range regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllStringSubmatch(out.String(), -1) {
		cmd := exec.Command("node", "--check")
		cmd.Stdin = strings.NewReader(script[1])
		if result, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("invalid script: %v %s", err, result)
		}
	}
}
