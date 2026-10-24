package handlers

import (
	"bytes"
	"html/template"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// Exercise the rendered page, so target selection and retries use the same
// JavaScript and escaped selection that an authenticated user receives.
func TestStaticPublishStreamlinedSelectionAndRecovery(t *testing.T) {
	for _, lang := range []string{"zh", "en"} {
		t.Run(lang, func(t *testing.T) {
			tmpl, err := template.New("publish").Funcs(adminTemplateFuncMap).Parse(adminTemplates["static_delivery_publish"])
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := tmpl.Execute(&out, map[string]any{
				"Lang": lang, "CanPublish": true, "CSRFToken": "csrf-proof",
				"SelectedIDs": []string{"000000000000000000000001", "000000000000000000000002"},
				"Selection":   []map[string]string{{"Title": "First report"}, {"Title": `<script>untrusted title</script>`}},
			}); err != nil {
				t.Fatal(err)
			}
			node, err := exec.LookPath("node")
			if err != nil {
				t.Fatal(err)
			}
			flow := exec.Command(node, "testdata/static_publish_selection_flow.cjs")
			flow.Stdin = bytes.NewReader(out.Bytes())
			if output, err := flow.CombinedOutput(); err != nil {
				t.Fatalf("target selection and recovery: %v %s", err, output)
			}
			text := out.String()
			if strings.Contains(text, `<script>untrusted title</script>`) || !strings.Contains(text, "First report") {
				t.Fatal("selected article titles were lost or rendered without escaping")
			}
			count := regexp.MustCompile(`(?s)id="selection-summary"[^>]*>(.*?)</summary>`).FindStringSubmatch(text)
			if len(count) != 2 || !strings.Contains(count[1], "2") {
				t.Fatal("selected article count is not visible before expanding the article list")
			}
			if lang == "en" && (!strings.Contains(text, "Publish to Cloudflare") || !strings.Contains(text, "Preview publication")) {
				t.Fatal("publish controls do not follow the selected language")
			}
		})
	}
}

func TestStaticPublishEmptySelectionAndDownloadOnly(t *testing.T) {
	for _, mode := range []string{"empty", "download-only"} {
		t.Run(mode, func(t *testing.T) {
			tmpl, err := template.New("publish").Funcs(adminTemplateFuncMap).Parse(adminTemplates["static_delivery_publish"])
			if err != nil {
				t.Fatal(err)
			}
			ids := []string{}
			selection := []map[string]string{}
			if mode == "download-only" {
				ids = []string{"000000000000000000000001"}
				selection = []map[string]string{{"Title": "Downloadable article"}}
			}
			var out bytes.Buffer
			if err := tmpl.Execute(&out, map[string]any{"Lang": "en", "CanPublish": mode == "empty", "SelectedIDs": ids, "Selection": selection, "CSRFToken": "csrf-proof"}); err != nil {
				t.Fatal(err)
			}
			node, err := exec.LookPath("node")
			if err != nil {
				t.Fatal(err)
			}
			flow := exec.Command(node, "testdata/static_publish_selection_flow.cjs", mode)
			flow.Stdin = bytes.NewReader(out.Bytes())
			if output, err := flow.CombinedOutput(); err != nil {
				t.Fatalf("%s: %v %s", mode, err, output)
			}
		})
	}
}
