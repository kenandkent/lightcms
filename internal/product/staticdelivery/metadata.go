package staticdelivery

import (
	"bytes"
	"context"
	ex "github.com/metavert-io/lightcms/v7/internal/product/staticexport"
	nethtml "golang.org/x/net/html"
	"html"
	"io"
	"strings"
)

func completeMetadata(ctx context.Context, body []byte, canonical, slug, environment string, defaults func(context.Context, string) (ex.Resource, ex.Resource, error), add func(ex.Resource) (string, error)) ([]byte, []ex.Diagnostic, error) {
	tree, e := nethtml.Parse(bytes.NewReader(body))
	if e != nil {
		return nil, nil, e
	}
	title, heading, description, bodyText := "", "", "", ""
	ogTitle := ""
	titleExists := false
	has := map[string]bool{}
	var walk func(*nethtml.Node)
	var text func(*nethtml.Node) string
	text = func(n *nethtml.Node) string {
		if n.Type == nethtml.ElementNode && (n.Data == "script" || n.Data == "style" || n.Data == "noscript") {
			return ""
		}
		if n.Type == nethtml.TextNode {
			return n.Data + " "
		}
		var b strings.Builder
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			b.WriteString(text(c))
		}
		return b.String()
	}
	walk = func(n *nethtml.Node) {
		if n.Type == nethtml.ElementNode {
			attrs := map[string]string{}
			for _, a := range n.Attr {
				attrs[a.Key] = a.Val
			}
			switch n.Data {
			case "title":
				titleExists = true
				title = strings.Join(strings.Fields(text(n)), " ")
			case "h1":
				if heading == "" {
					heading = strings.Join(strings.Fields(text(n)), " ")
				}
			case "body":
				bodyText = strings.Join(strings.Fields(text(n)), " ")
			case "meta":
				key := attrs["property"]
				if key == "" {
					key = attrs["name"]
				}
				if attrs["content"] != "" {
					has[key] = true
				}
				if key == "og:title" {
					ogTitle = attrs["content"]
				}
				if key == "description" {
					description = attrs["content"]
				}
				if description == "" && key == "og:description" {
					description = attrs["content"]
				}
			case "link":
				for _, rel := range strings.Fields(attrs["rel"]) {
					if attrs["href"] != "" {
						has[rel] = true
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(tree)
	if title == "" {
		title = ogTitle
	}
	if title == "" {
		title = heading
	}
	if title == "" {
		return nil, nil, failure(CodeMetadataInvalid, "published report has no title", nil)
	}
	if description == "" {
		runes := []rune(bodyText)
		if len(runes) > 200 {
			runes = runes[:200]
		}
		description = string(runes)
	}
	var additions strings.Builder
	diagnostics := []ex.Diagnostic{}
	meta := func(key, value string, property bool) {
		if has[key] {
			return
		}
		attr := "name"
		if property {
			attr = "property"
		}
		additions.WriteString(`<meta ` + attr + `="` + html.EscapeString(key) + `" content="` + html.EscapeString(value) + `">`)
		has[key] = true
	}
	if !has["canonical"] {
		additions.WriteString(`<link rel="canonical" href="` + html.EscapeString(canonical) + `">`)
	}
	if !titleExists {
		additions.WriteString("<title>" + html.EscapeString(title) + "</title>")
	}
	meta("description", description, false)
	meta("og:title", title, true)
	meta("og:description", description, true)
	meta("og:url", canonical, true)
	meta("og:type", "article", true)
	meta("twitter:card", "summary_large_image", false)
	meta("twitter:title", title, false)
	meta("twitter:description", description, false)
	if !has["og:image"] || !has["icon"] {
		var share, icon ex.Resource
		var defaultErr error
		if defaults != nil {
			share, icon, defaultErr = defaults(ctx, slug)
		}
		if defaultErr != nil {
			return nil, nil, defaultErr
		}
		if !has["og:image"] && len(share.Data) > 0 {
			u, e := add(share)
			if e != nil {
				return nil, nil, e
			}
			meta("og:image", u, true)
			meta("twitter:image", u, false)
		}
		if !has["icon"] && len(icon.Data) > 0 {
			u, e := add(icon)
			if e != nil {
				return nil, nil, e
			}
			additions.WriteString(`<link rel="icon" href="` + html.EscapeString(u) + `">`)
			has["icon"] = true
		}
	}
	if !has["og:image"] || !has["icon"] {
		if environment == "production" {
			return nil, nil, failure(CodeMetadataInvalid, "production requires a share image and icon in the frozen template or explicit defaults", nil)
		}
		diagnostics = append(diagnostics, ex.Diagnostic{Code: "MISSING_SHARE_METADATA", Message: "share image or icon is missing"})
	}
	if environment == "test" {
		meta("robots", "noindex, nofollow", false)
	}
	z := nethtml.NewTokenizer(bytes.NewReader(body))
	var out bytes.Buffer
	inserted := false
	for {
		tt := z.Next()
		if tt == nethtml.ErrorToken {
			if z.Err() != io.EOF {
				return nil, nil, z.Err()
			}
			break
		}
		raw := append([]byte(nil), z.Raw()...)
		if tt == nethtml.EndTagToken && z.Token().Data == "head" {
			out.WriteString(additions.String())
			inserted = true
		}
		out.Write(raw)
	}
	if !inserted {
		return nil, nil, failure(CodeNotStandalone, "report head must have a closing element", nil)
	}
	return out.Bytes(), diagnostics, nil
}
