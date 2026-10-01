package handlers

import (
	"strings"
	"testing"
)

// TestAdminDashboardRangeLang is a regression test for the i18n range-scope
// bug: {{i18n ... .Lang}} calls inside {{range}} blocks resolved .Lang
// against the range item (which has no Lang field), aborting Execute and
// truncating the page. All i18n calls must use $.Lang (root template data).
func TestAdminDashboardRangeLang(t *testing.T) {
	initAdminTemplateCache()
	tmpl, ok := adminTemplateCache["dashboard"]
	if !ok {
		t.Fatal("dashboard template missing from adminTemplateCache")
	}
	items := []map[string]interface{}{
		{"Title": "Range Probe A", "FullPath": "/probe-a", "Slug": "probe-a", "Status": "published"},
		{"Title": "Range Probe B", "FullPath": "/probe-b", "Slug": "", "Status": "draft"},
	}
	for _, lang := range []string{"zh", "en"} {
		data := map[string]interface{}{
			"Lang":            lang,
			"AppVersion":      "7.3.0-test",
			"RecentContent":   items,
			"PendingApprovals": []map[string]interface{}{},
			"RecentComments":  []map[string]interface{}{},
		}
		var buf strings.Builder
		if err := tmpl.Execute(&buf, data); err != nil {
			t.Fatalf("dashboard Execute(lang=%s) error (range-scope i18n regression): %v", lang, err)
		}
		out := buf.String()
		// Range bodies must have executed (item titles present = no truncation).
		for _, want := range []string{"Range Probe A", "Range Probe B"} {
			if !strings.Contains(out, want) {
				t.Errorf("dashboard(lang=%s) missing range item %q — output truncated?", lang, want)
			}
		}
		// No unresolved template markers may leak into output.
		if strings.Contains(out, "<no value>") {
			t.Errorf("dashboard(lang=%s) contains <no value> markers", lang)
		}
	}
}
