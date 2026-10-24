package staticexport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"sort"
	"strings"
)

var ErrExternalResource = errors.New("resource belongs to an external origin")

// Budget includes source HTML and resources before transformed copies exist.
// Limits are explicit failures, never partial exports.
const MaxCaptureBytes int64 = 128 << 20

func DocumentBytes(doc Document) int64 {
	size := int64(len(doc.HTML))
	for _, r := range doc.Resources {
		size += int64(len(r.Data))
	}
	return size
}

func ResolveReference(base, ref string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return "", exportError("STATIC_DEPENDENCY_UNRESOLVED", "invalid resource URL", nil)
	}
	b, err := url.Parse(base)
	if err != nil || !b.IsAbs() {
		return "", exportError("STATIC_DEPENDENCY_UNRESOLVED", "publication has no absolute source URL", nil)
	}
	return b.ResolveReference(u).String(), nil
}

// Collect walks only actual resource references. It does not execute scripts
// or download third-party resources, and it never treats citation links as files.
func Collect(ctx context.Context, source PageSource, body []byte, resolver Resolver) (Document, error) {
	return collectBudget(ctx, source, body, resolver, MaxCaptureBytes)
}

func collectBudget(ctx context.Context, source PageSource, body []byte, resolver Resolver, budget int64) (Document, error) {
	if int64(len(body)) > budget {
		return Document{}, exportError("STATIC_ASSET_LIMIT_EXCEEDED", "export source budget exceeded", nil)
	}
	doc := Document{Source: source, HTML: append([]byte(nil), body...), Resources: make(map[string]Resource)}
	used := int64(len(body))
	if !IsStandaloneDocument(body) {
		return doc, exportError("STATIC_DOCUMENT_NOT_STANDALONE", "a complete html/head/body document is required", nil)
	}
	active := make(map[string]bool)
	external := make(map[string]bool)
	var add func(string, string) error
	add = func(base, ref string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		ref = strings.TrimSpace(ref)
		if ref == "" || strings.HasPrefix(ref, "#") || strings.HasPrefix(strings.ToLower(ref), "data:") {
			return nil
		}
		u, err := ResolveReference(base, ref)
		if err != nil {
			return err
		}
		parsed, _ := url.Parse(u)
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return exportError("STATIC_DEPENDENCY_UNRESOLVED", "unsupported resource protocol", nil)
		}
		parsed.Fragment = ""
		u = parsed.String()
		if active[u] {
			return exportError("STATIC_DEPENDENCY_UNRESOLVED", "cyclic stylesheet dependency", nil)
		}
		if _, ok := doc.Resources[u]; ok {
			return nil
		}
		if external[u] {
			return nil
		}
		if resolver == nil {
			return exportError("STATIC_DEPENDENCY_UNRESOLVED", "resource resolver is unavailable", nil)
		}
		resource, err := resolver.Resolve(ctx, u)
		if errors.Is(err, ErrExternalResource) {
			external[u] = true
			return nil
		}
		if err != nil {
			return exportError("STATIC_DEPENDENCY_UNRESOLVED", "a required local resource is unavailable", err)
		}
		resource.URL = u
		used += int64(len(resource.Data))
		if len(resource.Data) > MaxResourceBytes || used > budget {
			return exportError("STATIC_ASSET_LIMIT_EXCEEDED", "export resource budget exceeded", nil)
		}
		sum := sha256.Sum256(resource.Data)
		resource.SHA256 = hex.EncodeToString(sum[:])
		active[u] = true
		if strings.EqualFold(strings.Split(resource.MIME, ";")[0], "text/css") {
			_, err = TransformCSS(string(resource.Data), func(ref string) (string, error) { return ref, add(u, ref) })
			if err != nil {
				return err
			}
		}
		delete(active, u)
		doc.Resources[u] = resource
		return nil
	}
	_, err := TransformHTML(body, func(ref Reference) (string, error) {
		if ref.Kind == "resource" {
			return ref.Value, add(source.PublicURL, ref.Value)
		}
		return ref.Value, nil
	})
	if err != nil {
		return doc, err
	}
	for u := range external {
		doc.External = append(doc.External, u)
	}
	sort.Strings(doc.External)
	for _, u := range doc.External {
		doc.Diagnostics = append(doc.Diagnostics, Diagnostic{Code: "EXTERNAL_RENDER_DEPENDENCY", URL: u, Message: "Import or inline this dependency before independent deployment"})
	}
	return doc, nil
}
