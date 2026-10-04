package templates_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestCryptoTemplateRendersRichTextAndShareMetadata(t *testing.T) {
	b, err := os.ReadFile("crypto-analysis.template.json")
	if err != nil {
		t.Fatal(err)
	}
	var in models.Template
	if err := json.Unmarshal(b, &in); err != nil {
		t.Fatal(err)
	}
	layout, err := os.ReadFile("crypto-analysis.html")
	if err != nil {
		t.Fatal(err)
	}
	if in.HTMLLayout != string(layout) {
		t.Fatal("crypto import layout differs from source")
	}
	v := templatecontract.TemplateVersion{ID: primitive.NewObjectID(), TemplateID: primitive.NewObjectID(), Version: 1, Slug: in.Slug, Fields: in.Fields, HTMLLayout: in.HTMLLayout}
	data := map[string]any{"headline": "Evidence <update>", "summary": "Verified observations", "author": "Research desk", "category": "Analysis", "body": "<h2>Context</h2><p><strong>Verified</strong> observations.</p>", "share_image_url": "https://publisher.example/cover.png", "share_image_alt": "Report cover", "favicon_url": "https://publisher.example/icon.svg"}
	if errs, _ := templatecontract.ValidateData(v, data); len(errs) > 0 {
		t.Fatalf("declared fields cannot accept article and share data: %+v", errs)
	}
	snap, err := publication.PlanSnapshot(models.Content{ID: primitive.NewObjectID(), FullPath: "/news/example", Data: data}, 1, v, primitive.NewObjectID(), time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC), "https://publisher.example/news/example", publication.PlanOptions{ScriptPolicy: "all"})
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := publication.Render(context.Background(), snap)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<strong>Verified</strong>", `property="og:title" content="Evidence &lt;update&gt;"`, `property="og:image" content="https://publisher.example/cover.png"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing rendered %s", want)
		}
	}
}
