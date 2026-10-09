package templates_test

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

var reportSlugs = []string{"cybersecurity-report", "fund-research", "venture-research", "binance-announcement-style", "okx-announcement-style", "editorial-news", "financial-daily", "technology-report"}

// Exercise the production field validator and frozen renderer, not a substitute
// template engine. Missing metadata or an undeclared bilingual field breaks this.
func TestReportTemplatesRenderWithProductionContract(t *testing.T) {
	for _, slug := range reportSlugs {
		t.Run(slug, func(t *testing.T) {
			manifest, err := os.ReadFile(slug + ".template.json")
			if err != nil {
				t.Fatal(err)
			}
			var in struct {
				models.Template
				ScriptPolicy string `json:"script_policy"`
			}
			if err := json.Unmarshal(manifest, &in); err != nil {
				t.Fatal(err)
			}
			layout, err := os.ReadFile(slug + ".html")
			if err != nil {
				t.Fatal(err)
			}
			if in.HTMLLayout != string(layout) {
				t.Fatal("import payload differs from standalone layout")
			}
			if err := templatecontract.ValidateFields(in.Fields); err != nil {
				t.Fatal(err)
			}
			v := templatecontract.TemplateVersion{ID: primitive.NewObjectID(), TemplateID: primitive.NewObjectID(), Version: 1, Slug: in.Slug, Fields: in.Fields, HTMLLayout: in.HTMLLayout, ScriptPolicy: in.ScriptPolicy}
			fixture, err := os.ReadFile(slug + ".example.json")
			if err != nil {
				t.Fatal(err)
			}
			var req struct {
				Data map[string]any `json:"data"`
			}
			if err := json.Unmarshal(fixture, &req); err != nil {
				t.Fatal(err)
			}
			if errors, _ := templatecontract.ValidateData(v, req.Data); len(errors) > 0 {
				t.Fatalf("example does not validate: %+v", errors)
			}
			data := map[string]any{"headline": "Research <update> & outlook", "summary": "Evidence, not certainty.", "author": "Research desk", "category": "Analysis", "body": "## Evidence\n\n**Verified** observations.", "share_image_url": "https://publisher.example/cover.png", "share_image_alt": "Report cover", "favicon_url": "https://publisher.example/icon.svg", "touch_icon_url": "https://publisher.example/touch.png"}
			for _, bilingual := range []bool{false, true} {
				if bilingual {
					data["headline_zh"] = "研究更新"
					data["summary_zh"] = "以证据为基础"
					data["body_zh"] = "## 证据\n\n已核实的观察。"
				}
				if errors, _ := templatecontract.ValidateData(v, data); len(errors) > 0 {
					t.Fatalf("data does not validate: %+v", errors)
				}
				snap, err := publication.PlanSnapshot(models.Content{ID: primitive.NewObjectID(), FullPath: "/reports/example", Data: data}, 1, v, primitive.NewObjectID(), time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC), "https://publisher.example/reports/example", publication.PlanOptions{ScriptPolicy: "all"})
				if err != nil {
					t.Fatal(err)
				}
				out, _, err := publication.Render(context.Background(), snap)
				if err != nil {
					t.Fatal(err)
				}
				html := string(out)
				ld := regexp.MustCompile(`(?s)<script type="application/ld\+json">(.*?)</script>`).FindStringSubmatch(html)
				if len(ld) != 2 {
					t.Fatal("frozen structured metadata missing")
				}
				var metadata map[string]any
				if err := json.Unmarshal([]byte(ld[1]), &metadata); err != nil {
					t.Fatalf("invalid JSON-LD: %v", err)
				}
				if metadata["headline"] != "Research <update> & outlook" {
					t.Fatal("JSON-LD title differs from frozen article")
				}
				for _, want := range []string{`<html lang="en"`, `property="og:title" content="Research &lt;update&gt; &amp; outlook"`, `property="og:description" content="Evidence, not certainty."`, `property="og:url" content="https://publisher.example/reports/example"`, `property="og:image" content="https://publisher.example/cover.png"`, `<strong>Verified</strong>`, `name="twitter:card" content="summary_large_image"`} {
					if !strings.Contains(html, want) {
						t.Errorf("render missing %s", want)
					}
				}
				if strings.Contains(html, "ZgotmplZ") || strings.Contains(html, "<no value>") || strings.Contains(html, "{{.") {
					t.Fatal("unsafe or unresolved template output")
				}
				if bilingual && !strings.Contains(html, "研究更新") {
					t.Fatal("Chinese content not preserved")
				}
			}
		})
	}
}
