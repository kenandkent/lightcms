package staticexport

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"

	nethtml "golang.org/x/net/html"
)

type Reference struct{ Value, Kind, Tag, Attribute string }

func IsStandaloneDocument(body []byte) bool {
	z := nethtml.NewTokenizer(bytes.NewReader(bytes.TrimPrefix(body, []byte("\xef\xbb\xbf"))))
	leading := true
	doc, head, html, bodyTag := false, false, false, false
	for {
		tt := z.Next()
		if tt == nethtml.ErrorToken {
			return doc && head && html && bodyTag && z.Err() == io.EOF
		}
		t := z.Token()
		if leading {
			if tt == nethtml.CommentToken || (tt == nethtml.TextToken && strings.TrimSpace(t.Data) == "") {
				continue
			}
			leading = false
			doc = (tt == nethtml.DoctypeToken && strings.EqualFold(t.Data, "html")) || (tt == nethtml.StartTagToken && t.Data == "html")
			if !doc {
				return false
			}
		}
		if tt == nethtml.StartTagToken {
			switch t.Data {
			case "html":
				html = true
			case "head":
				head = true
			case "body":
				bodyTag = true
			}
		}
	}
}

// TransformHTML changes only recognized URL-bearing tokens. Raw text,
// whitespace, inline script bytes and unrelated attributes remain untouched.
func TransformHTML(input []byte, visit func(Reference) (string, error)) ([]byte, error) {
	z := nethtml.NewTokenizer(bytes.NewReader(input))
	var out bytes.Buffer
	textTag, scriptType := "", ""
	for {
		tt := z.Next()
		raw := append([]byte(nil), z.Raw()...)
		if tt == nethtml.ErrorToken {
			if z.Err() != io.EOF {
				return nil, z.Err()
			}
			break
		}
		t := z.Token()
		changed := false
		if tt == nethtml.StartTagToken || tt == nethtml.SelfClosingTagToken {
			if t.Data == "base" {
				return nil, exportError("STATIC_DEPENDENCY_UNRESOLVED", "base elements need explicit URL migration and are not supported", nil)
			}
			if t.Data == "script" {
				textTag = "script"
				scriptType = attribute(t, "type")
			}
			if t.Data == "style" {
				textTag = "style"
			}
			for i, a := range t.Attr {
				kind := ""
				switch a.Key {
				case "src", "poster", "srcset":
					kind = "resource"
				case "href":
					if t.Data == "a" {
						kind = "link"
					}
					if t.Data == "link" {
						rel := strings.Fields(strings.ToLower(attribute(t, "rel")))
						for _, r := range rel {
							switch r {
							case "stylesheet", "icon", "apple-touch-icon", "preload", "modulepreload", "mask-icon":
								kind = "resource"
							case "canonical":
								kind = "page"
							}
						}
					}
				case "content":
					if t.Data == "meta" {
						key := strings.ToLower(attribute(t, "property"))
						if key == "" {
							key = strings.ToLower(attribute(t, "name"))
						}
						switch key {
						case "og:image", "og:image:secure_url", "twitter:image", "twitter:image:src":
							kind = "resource"
						case "og:url":
							kind = "page"
						}
					}
				case "style":
					v, err := TransformCSS(a.Val, func(v string) (string, error) {
						return visit(Reference{Value: v, Kind: "resource", Tag: t.Data, Attribute: "style"})
					})
					if err != nil {
						return nil, err
					}
					if v != a.Val {
						t.Attr[i].Val = v
						changed = true
					}
					continue
				}
				if kind != "" {
					call := func(v string) (string, error) {
						return visit(Reference{Value: v, Kind: kind, Tag: t.Data, Attribute: a.Key})
					}
					var v string
					var err error
					if a.Key == "srcset" {
						v, err = transformSrcset(a.Val, call)
					} else {
						v, err = call(a.Val)
					}
					if err != nil {
						return nil, err
					}
					if v != a.Val {
						t.Attr[i].Val = v
						changed = true
					}
				}
			}
		}
		if tt == nethtml.TextToken && textTag == "style" {
			v, err := TransformCSS(string(raw), func(v string) (string, error) { return visit(Reference{Value: v, Kind: "resource", Tag: "style"}) })
			if err != nil {
				return nil, err
			}
			raw = []byte(v)
		}
		if tt == nethtml.TextToken && textTag == "script" && strings.EqualFold(scriptType, "application/ld+json") {
			v, err := transformJSONLD(raw, visit)
			if err != nil {
				return nil, err
			}
			raw = v
		}
		if tt == nethtml.EndTagToken && (t.Data == "style" || t.Data == "script") {
			textTag = ""
			scriptType = ""
		}
		if changed {
			out.WriteString(t.String())
		} else {
			out.Write(raw)
		}
	}
	return out.Bytes(), nil
}
func attribute(t nethtml.Token, key string) string {
	for _, a := range t.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func transformSrcset(s string, visit func(string) (string, error)) (string, error) {
	var out []string
	remaining := strings.TrimSpace(s)
	changed := false
	for len(remaining) > 0 {
		remaining = strings.TrimLeft(remaining, " \t\r\n,")
		if remaining == "" {
			break
		}
		i := strings.IndexAny(remaining, " \t\r\n")
		if i < 0 {
			i = len(remaining)
		}
		ref := remaining[:i]
		remaining = remaining[i:]
		descriptor := ""
		if strings.HasSuffix(ref, ",") {
			ref = strings.TrimRight(ref, ",")
		} else {
			j := strings.IndexByte(remaining, ',')
			if j < 0 {
				descriptor = strings.TrimSpace(remaining)
				remaining = ""
			} else {
				descriptor = strings.TrimSpace(remaining[:j])
				remaining = remaining[j+1:]
			}
		}
		v, err := visit(ref)
		if err != nil {
			return "", err
		}
		changed = changed || v != ref
		if descriptor != "" {
			v += " " + descriptor
		}
		out = append(out, v)
	}
	if !changed {
		return s, nil
	}
	return strings.Join(out, ", "), nil
}

func transformJSONLD(input []byte, visit func(Reference) (string, error)) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, exportError("STATIC_METADATA_INVALID", "invalid JSON-LD", nil)
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, exportError("STATIC_METADATA_INVALID", "JSON-LD must contain one JSON value", nil)
	}
	changed := false
	var walk func(any, string, bool, bool) (any, error)
	walk = func(v any, key string, inImage, identity bool) (any, error) {
		switch key {
		case "publisher", "author", "creator", "provider", "sourceOrganization", "copyrightHolder", "funder", "sponsor", "citation":
			identity = true
		}
		resource := inImage || key == "image" || key == "logo" || key == "thumbnailUrl" || key == "contentUrl"
		switch x := v.(type) {
		case map[string]any:
			for k, item := range x {
				next, e := walk(item, k, resource, identity)
				if e != nil {
					return nil, e
				}
				x[k] = next
			}
		case []any:
			for i, item := range x {
				next, e := walk(item, key, resource, identity)
				if e != nil {
					return nil, e
				}
				x[i] = next
			}
		case string:
			kind := ""
			if resource && (key == "url" || key == "@id" || key == "image" || key == "logo" || key == "thumbnailUrl" || key == "contentUrl") {
				kind = "resource"
			} else if !resource && !identity && (key == "url" || key == "@id" || key == "mainEntityOfPage") {
				kind = "page"
			}
			if kind != "" {
				next, e := visit(Reference{Value: x, Kind: kind, Tag: "jsonld", Attribute: key})
				if e != nil {
					return nil, e
				}
				changed = changed || next != x
				return next, nil
			}
		}
		return v, nil
	}
	next, err := walk(value, "", false, false)
	if err != nil {
		return nil, err
	}
	if !changed {
		return input, nil
	}
	return json.Marshal(next)
}
