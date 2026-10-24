package handlers

import (
	"strings"
	"testing"
)

func TestAuthoredJSONLDDoesNotSuppressIndependentSEOAdditions(t *testing.T) {
	source := `<html><head><title>Frozen</title><script type="application/ld+json">{"@type":"Article","headline":"Frozen"}</script><link rel="alternate" type="application/rss+xml" href="https://example.com/feed.xml"></head><body>Body</body></html>`
	extra := `<script type="application/ld+json">{"@type":"Article","headline":"Mutable"}</script><script type="application/ld+json">{"@type":"BreadcrumbList"}</script><link rel="canonical" href="https://example.com/article"><link rel="alternate" type="text/markdown" href="https://example.com/article.md"><link rel="alternate" type="application/rss+xml" href="https://example.com/feed.xml">`
	got := supplementStandaloneHead(source, "Mutable title", extra)
	for _, part := range []string{`"headline":"Frozen"`, `"@type":"BreadcrumbList"`, `rel="canonical"`, `type="text/markdown"`} {
		if !strings.Contains(got, part) {
			t.Errorf("missing independent SEO addition %s", part)
		}
	}
	if strings.Contains(got, "Mutable") || strings.Count(got, `type="application/rss+xml"`) != 1 || strings.Count(got, `"@type":"Article"`) != 1 {
		t.Fatal("authored metadata replaced or duplicate discovery tags added", got)
	}
}
