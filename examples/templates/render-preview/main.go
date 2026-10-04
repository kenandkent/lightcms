// Render illustrative pages with the real LightCMS publication renderer.
// No database or live publication is changed. Output must be inside the repo.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func main() {
	root, err := os.Getwd()
	must(err)
	out := filepath.Join(root, "bin/template-previews")
	must(os.MkdirAll(out, 0755))
	files, err := filepath.Glob(filepath.Join(root, "examples/templates/*.template.json"))
	must(err)
	for _, path := range files {
		var manifest struct {
			models.Template
			ScriptPolicy string `json:"script_policy"`
		}
		b, err := os.ReadFile(path)
		must(err)
		must(json.Unmarshal(b, &manifest))
		var request struct {
			Data  map[string]any `json:"data"`
			Title string         `json:"title"`
		}
		b, err = os.ReadFile(strings.TrimSuffix(path, ".template.json") + ".example.json")
		must(err)
		must(json.Unmarshal(b, &request))
		// Development URLs only. Never use these fixture URLs in production.
		request.Data["share_image_url"] = "http://127.0.0.1:18083/static/images/report-templates/" + manifest.Slug + "-share.png"
		request.Data["favicon_url"] = "/static/images/report-templates/" + manifest.Slug + "-icon.svg"
		request.Data["touch_icon_url"] = "/static/images/report-templates/" + manifest.Slug + "-touch.png"
		if manifest.Slug == "crypto-analysis" {
			request.Data["share_image_url"] = "http://127.0.0.1:18083/static/images/chain-lens-share.png"
			request.Data["favicon_url"] = "/static/images/chain-lens-icon.svg"
			delete(request.Data, "touch_icon_url")
		}
		version := templatecontract.TemplateVersion{ID: primitive.NewObjectID(), TemplateID: primitive.NewObjectID(), Version: 1, Slug: manifest.Slug, Fields: manifest.Fields, HTMLLayout: manifest.HTMLLayout}
		content := models.Content{ID: primitive.NewObjectID(), FullPath: "/reports/" + manifest.Slug, Title: request.Title, Data: request.Data}
		snap, err := publication.PlanSnapshot(content, 1, version, primitive.NewObjectID(), time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC), "http://127.0.0.1:18083/bin/template-previews/"+manifest.Slug+".html", publication.PlanOptions{ScriptPolicy: "all"})
		must(err)
		html, _, err := publication.Render(context.Background(), snap)
		must(err)
		must(os.WriteFile(filepath.Join(out, manifest.Slug+".html"), html, 0644))
		fmt.Println(manifest.Slug + ".html")
	}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
