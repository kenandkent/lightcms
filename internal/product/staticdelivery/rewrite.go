package staticdelivery

import (
	"context"
	"mime"
	"net/url"
	"path"
	"sort"
	"strings"

	ex "github.com/metavert-io/lightcms/v7/internal/product/staticexport"
)

func rewritePage(ctx context.Context, c CapturedPage, origin string, site map[string]CapturedPage, environment string, defaults func(context.Context, string) (ex.Resource, ex.Resource, error)) (ReleasePage, map[string]ReleaseFile, []ex.Diagnostic, error) {
	doc := c.Document
	source := doc.Source
	result := ReleasePage{ExportID: c.ExportID, ContentID: source.ContentID, PublicationID: source.PublicationID, Path: source.FullPath, Captured: c}
	files := map[string]ReleaseFile{}
	diagnostics := append([]ex.Diagnostic{}, doc.Diagnostics...)
	if len(doc.External) > 0 {
		return result, nil, nil, failure(CodeDependencyUnresolved, "external rendering dependencies must be imported before deployment", nil)
	}
	resources := map[string]ex.Resource{}
	for u, r := range doc.Resources {
		resources[u] = r
	}
	rewritten := map[string]string{}
	visiting := map[string]bool{}
	var buildAsset func(string) (string, error)
	var resourceURL func(string, string, bool) (string, error)
	resourceURL = func(base, ref string, absolute bool) (string, error) {
		if ref == "" || strings.HasPrefix(ref, "#") || strings.HasPrefix(strings.ToLower(ref), "data:") {
			return ref, nil
		}
		u, e := ex.ResolveReference(base, ref)
		if e != nil {
			return "", e
		}
		parsed, e := url.Parse(u)
		if e != nil {
			return "", e
		}
		fragment := parsed.Fragment
		parsed.Fragment = ""
		name, e := buildAsset(parsed.String())
		if e != nil {
			return "", e
		}
		out := (&url.URL{Path: name, Fragment: fragment}).String()
		if absolute {
			out = origin + out
		}
		return out, nil
	}
	buildAsset = func(u string) (string, error) {
		if name, ok := rewritten[u]; ok {
			return name, nil
		}
		if visiting[u] {
			return "", failure(CodeDependencyUnresolved, "cyclic CSS imports cannot be content-addressed", nil)
		}
		visiting[u] = true
		defer delete(visiting, u)
		resource, ok := resources[u]
		if !ok {
			return "", failure(CodeDependencyUnresolved, "rendering resource is not in the captured export", nil)
		}
		data := resource.Data
		kind, _, e := mime.ParseMediaType(resource.MIME)
		if e != nil || kind == "text/html" {
			return "", failure(CodeDependencyUnresolved, "invalid asset MIME", nil)
		}
		if kind == "text/css" {
			css, e := ex.TransformCSS(string(data), func(ref string) (string, error) { return resourceURL(u, ref, false) })
			if e != nil {
				return "", e
			}
			data = []byte(css)
		}
		parsed, e := url.Parse(u)
		if e != nil {
			return "", e
		}
		ext := strings.ToLower(path.Ext(parsed.Path))
		if len(ext) > 12 || strings.ContainsAny(ext, "/\\%") {
			ext = ""
		}
		if ext == "" {
			extensions, _ := mime.ExtensionsByType(kind)
			if len(extensions) > 0 {
				sort.Strings(extensions)
				ext = extensions[0]
			} else {
				ext = ".bin"
			}
		}
		hash := releaseHash(data)
		name := "/_lc-assets/" + hash + ext
		if old, ok := files[name]; ok && old.MIME != resource.MIME {
			return "", failure(CodePathConflict, "same asset path has conflicting MIME", nil)
		}
		files[name] = ReleaseFile{Path: name, MIME: resource.MIME, SHA256: hash, Size: int64(len(data)), Data: append([]byte(nil), data...)}
		rewritten[u] = name
		return name, nil
	}
	canonical := origin + (&url.URL{Path: source.FullPath}).EscapedPath()
	transformed, e := ex.TransformHTML(doc.HTML, func(ref ex.Reference) (string, error) {
		switch ref.Kind {
		case "page":
			return canonical, nil
		case "resource":
			if ref.Tag == "meta" && strings.HasPrefix(strings.ToLower(strings.TrimSpace(ref.Value)), "data:") {
				return "", failure(CodeMetadataInvalid, "social preview images require public HTTP resources, not data URLs", nil)
			}
			return resourceURL(source.PublicURL, ref.Value, ref.Tag == "meta" || ref.Tag == "jsonld" || ref.Tag == "link" && ref.Attribute == "href")
		case "link":
			if ref.Value == "" || strings.HasPrefix(ref.Value, "#") {
				return ref.Value, nil
			}
			resolved, e := ex.ResolveReference(source.PublicURL, ref.Value)
			if e != nil {
				return ref.Value, nil
			}
			src, _ := url.Parse(source.PublicURL)
			u, e := url.Parse(resolved)
			if e != nil {
				return ref.Value, nil
			}
			if u.Host == src.Host && u.Scheme == src.Scheme {
				if _, ok := site[u.Path]; ok {
					return (&url.URL{Path: u.Path, RawQuery: u.RawQuery, Fragment: u.Fragment}).String(), nil
				}
				diagnostics = append(diagnostics, ex.Diagnostic{Code: "UNDEPLOYED_ARTICLE_LINK", Message: "link retains its original source address"})
			}
			return resolved, nil
		}
		return ref.Value, nil
	})
	if e != nil {
		return result, nil, nil, e
	}
	if !ex.IsStandaloneDocument(transformed) {
		return result, nil, nil, failure(CodeNotStandalone, "captured report must be a complete document", nil)
	}
	transformed, extra, e := completeMetadata(ctx, transformed, canonical, source.TemplateSlug, environment, defaults, func(r ex.Resource) (string, error) {
		if r.URL == "" {
			return "", failure(CodeMetadataInvalid, "default metadata resource URL required", nil)
		}
		resources[r.URL] = r
		name, e := buildAsset(r.URL)
		return origin + name, e
	})
	if e != nil {
		return result, nil, nil, e
	}
	diagnostics = append(diagnostics, extra...)
	file := strings.Trim(source.FullPath, "/") + "/index.html"
	if source.FullPath == "/" {
		file = "index.html"
	}
	result.File = "/" + file
	result.SHA256 = releaseHash(transformed)
	for name := range files {
		result.Assets = append(result.Assets, name)
	}
	sort.Strings(result.Assets)
	files[result.File] = ReleaseFile{Path: result.File, MIME: "text/html; charset=utf-8", SHA256: result.SHA256, Size: int64(len(transformed)), Data: transformed}
	return result, files, diagnostics, nil
}
