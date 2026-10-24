package staticdelivery

import (
	"encoding/json"
	"net/url"
	"sort"
	"strings"
)

const AssetsFirstV1 = "assets-first-v1"
const AssetsOnlyV1 = "assets-only-v1"
const rootAssetPath = "/.__lc_delivery/root.html"

func nativeDelivery(r ReleaseManifest) bool {
	return r.DeliveryMode == AssetsFirstV1 || r.DeliveryMode == AssetsOnlyV1
}

func staticHealth(installationID, targetID, releaseID string) []byte {
	b, _ := json.Marshal(map[string]any{"protocol": 1, "installation_id": installationID, "target_id": targetID, "release_id": releaseID})
	return b
}

// Private release layout stays unchanged; only Cloudflare's virtual manifest
// uses exact canonical URLs. No filename extension is required: upload MIME is
// explicit. HTML handling remains disabled so /a and /a.html are distinct pages.
func publicAssetPath(r ReleaseManifest, canonical, stored string) string {
	if !nativeDelivery(r) {
		return stored
	}
	if canonical == "/" {
		return rootAssetPath
	}
	return canonical
}

func ReleaseUploadFiles(r ReleaseManifest) ([]UploadFile, error) {
	if e := validateRelease(r); e != nil {
		return nil, e
	}
	mapping := map[string]string{}
	for canonical, p := range r.Pages {
		mapping[p.File] = publicAssetPath(r, canonical, p.File)
	}
	names := make([]string, 0, len(r.Files))
	for name := range r.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]UploadFile, 0, len(names))
	seen := map[string]bool{}
	for _, name := range names {
		f := r.Files[name]
		path := name
		if mapped, ok := mapping[name]; ok {
			path = mapped
		}
		if seen[path] {
			return nil, failure(CodePathConflict, "duplicate public asset path", nil)
		}
		seen[path] = true
		out = append(out, UploadFile{PublicPath: path, MIME: f.MIME, SHA256: f.SHA256, Data: f.Data})
	}
	if r.DeliveryMode == AssetsOnlyV1 {
		out = append(out, UploadFile{PublicPath: "/.__lc_delivery/health", MIME: "application/json", Data: staticHealth(r.InstallationID, r.TargetID.Hex(), r.ID.Hex())})
	}
	return out, nil
}

func nativeAssetConfig(r ReleaseManifest) ([]byte, error) {
	// Two native header rules, independent of article count; do not generate a
	// per-page _headers table (Cloudflare caps it at 100 rules).
	headers := "/*\n  X-LC-Release: " + r.ID.Hex() + "\n  X-Content-Type-Options: nosniff\n"
	if r.Environment == "test" {
		headers += "  X-Robots-Tag: noindex, nofollow\n"
	}
	headers += "/_lc-assets/*\n  Cache-Control: public,max-age=31536000,immutable\n"
	headers += "/.__lc_delivery/health\n  Cache-Control: no-store\n"
	redirects := ""
	if _, ok := r.Pages["/"]; ok {
		redirects = "/ " + rootAssetPath + " 200\n"
	}
	if r.DeliveryMode == AssetsOnlyV1 {
		// Native redirects replace the former JavaScript alias handling. Exact
		// article paths win, including an explicitly published /article.html.
		aliases := map[string]string{}
		for full := range r.Pages {
			if full == "/" {
				continue
			}
			for _, alias := range []string{full + "/", full + ".html"} {
				if _, exists := r.Pages[alias]; !exists {
					aliases[alias] = full
				}
			}
		}
		count := len(aliases)
		if redirects != "" {
			count++
		}
		if count > 2000 {
			return nil, failure(CodeAssetLimitExceeded, "native alias rules exceed the 2000 static redirect limit", nil)
		}
		names := make([]string, 0, len(aliases))
		for alias := range aliases {
			names = append(names, alias)
		}
		sort.Strings(names)
		for _, alias := range names {
			from := (&url.URL{Path: alias}).EscapedPath()
			to := (&url.URL{Path: aliases[alias]}).EscapedPath()
			line := from + " " + to + " 308"
			// Cloudflare's limit applies to the serialized rule, not the
			// decoded page path; non-ASCII paths expand during escaping.
			if len(line) > 1000 {
				return nil, failure(CodeAssetLimitExceeded, "encoded native redirect rule exceeds the 1000 character limit", nil)
			}
			redirects += line + "\n"
		}
	}
	config := map[string]any{"html_handling": "none", "not_found_handling": "none", "_headers": headers, "_redirects": redirects}
	if r.DeliveryMode == AssetsFirstV1 {
		config["run_worker_first"] = []string{"/.__lc_delivery/health"}
	}
	return json.Marshal(config)
}

func revalidatesHTML(cache string) bool {
	tokens := map[string]bool{}
	for _, p := range strings.Split(strings.ToLower(cache), ",") {
		tokens[strings.TrimSpace(p)] = true
	}
	return tokens["no-cache"] || tokens["max-age=0"] && tokens["must-revalidate"]
}
