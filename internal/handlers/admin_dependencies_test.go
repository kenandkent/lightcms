package handlers

import (
	"bytes"
	"go.mongodb.org/mongo-driver/bson"
	"html/template"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDependencyInventoryIsReadOnlyBoundedAndEscaped(t *testing.T) {
	h := &Handler{}
	w := httptest.NewRecorder()
	h.AdminDependencies(w, httptest.NewRequest("GET", "/cm/dependencies", nil))
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	tmpl, err := template.New("dependencies").Funcs(adminTemplateFuncMap).Parse(adminTemplates["dependencies"])
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, map[string]any{"Lang": "zh", "Kind": "publication", "Value": "<script>bad</script>", "Frozen": []bson.M{{"publication_id": "id", "dependency_snapshot": "<script>bad</script>"}}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "<script>bad</script>") || !strings.Contains(out.String(), "50 条") || !strings.Contains(out.String(), "不伪造历史") {
		t.Fatal("inventory escaped content or bounds missing")
	}
}
