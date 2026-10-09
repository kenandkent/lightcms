package publication_test

import (
	"context"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"strings"
	"testing"
)

func TestCryptoRichtextMarkdownCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, slug, input string
		want, not         string
	}{
		{"crypto markdown", "crypto-analysis", "# Article title\n\nA **bold** observation.\n\n| Metric | Value |\n| --- | --- |\n| Volume | 10 |", "<table>", "**bold**"},
		{"crypto richtext preserved", "crypto-analysis", "<h2>Article title</h2><p>A <strong>bold</strong> observation.</p>", "<strong>bold</strong>", "&lt;h2"},
		{"other richtext unchanged", "another-template", "# A literal richtext heading\n\n**literal**", "**literal**", "<h1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := testSnapshot()
			snap.TemplateSlug = tc.slug
			snap.HTMLLayout = "<article>{{.body}}</article>"
			snap.Data["body"] = tc.input
			result, _, err := publication.Render(context.Background(), snap)
			if err != nil {
				t.Fatal(err)
			}
			got := string(result)
			if !strings.Contains(got, tc.want) || strings.Contains(got, tc.not) {
				t.Fatalf("unexpected body: %s", got)
			}
		})
	}
}
