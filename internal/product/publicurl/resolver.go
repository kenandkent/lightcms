// Package publicurl builds canonical HTTPS public URLs for published pages.
//
// The resolver is a pure URL builder: it never touches MongoDB, the
// filesystem, or publications. Task 16 wires it to PUBLIC_BASE_URL/BASE_URL
// at startup; a Publication must only expose Resolve results after the
// activation transaction commits (spec §19.3).
package publicurl

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Resolver builds canonical public URLs from a validated base URL.
type Resolver struct {
	scheme   string
	host     string
	basePath string
}

// NewResolver validates baseURL and returns a Resolver.
//
// Base rules (spec §19.3):
//   - baseURL must be non-nil with http or https scheme and a host;
//   - must not contain userinfo, query, fragment, or an opaque part;
//   - plain HTTP is accepted only for loopback development hosts
//     (localhost / 127.0.0.0/8 / ::1); any other host must use HTTPS so a
//     production misconfiguration fails fast at startup.
//
// A base path prefix (e.g. https://example.com/blog) is allowed and is
// prepended to every resolved path.
func NewResolver(baseURL *url.URL) (*Resolver, error) {
	if baseURL == nil {
		return nil, fmt.Errorf("publicurl: base URL is required")
	}
	scheme := strings.ToLower(baseURL.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("publicurl: base URL scheme must be http or https")
	}
	if baseURL.Host == "" {
		return nil, fmt.Errorf("publicurl: base URL must have a host")
	}
	if baseURL.User != nil {
		return nil, fmt.Errorf("publicurl: base URL must not contain userinfo")
	}
	if baseURL.RawQuery != "" || baseURL.ForceQuery || baseURL.Fragment != "" || baseURL.RawFragment != "" {
		return nil, fmt.Errorf("publicurl: base URL must not contain query or fragment")
	}
	if baseURL.Opaque != "" {
		return nil, fmt.Errorf("publicurl: base URL must not be opaque")
	}
	if scheme == "http" && !isLocalHost(baseURL.Hostname()) {
		return nil, fmt.Errorf("publicurl: production public URL must use HTTPS")
	}
	basePath := baseURL.Path
	if basePath != "" && basePath != "/" {
		if !strings.HasPrefix(basePath, "/") {
			return nil, fmt.Errorf("publicurl: base URL path must start with /")
		}
		if strings.Contains(basePath, "//") || strings.Contains(basePath, "..") || strings.Contains(basePath, "\\") {
			return nil, fmt.Errorf("publicurl: base URL path is not canonical")
		}
		basePath = strings.TrimSuffix(basePath, "/")
	} else {
		basePath = ""
	}
	return &Resolver{scheme: scheme, host: baseURL.Host, basePath: basePath}, nil
}

// ValidateProductionBaseURL enforces the production startup rule for Task 16
// wiring: production base URLs must be absolute HTTPS URLs without userinfo,
// query, or fragment.
func ValidateProductionBaseURL(baseURL *url.URL) error {
	if baseURL == nil {
		return fmt.Errorf("publicurl: base URL is required")
	}
	if strings.ToLower(baseURL.Scheme) != "https" {
		return fmt.Errorf("publicurl: production public URL must use HTTPS")
	}
	if baseURL.Host == "" {
		return fmt.Errorf("publicurl: base URL must have a host")
	}
	if baseURL.User != nil {
		return fmt.Errorf("publicurl: base URL must not contain userinfo")
	}
	if baseURL.RawQuery != "" || baseURL.ForceQuery || baseURL.Fragment != "" || baseURL.RawFragment != "" {
		return fmt.Errorf("publicurl: base URL must not contain query or fragment")
	}
	return nil
}

// Base returns a copy of the validated base URL.
func (r *Resolver) Base() *url.URL {
	return &url.URL{Scheme: r.scheme, Host: r.host, Path: r.basePath}
}

// Resolve builds the canonical public URL for canonicalFullPath.
//
// Rules (spec §19.3):
//   - casing is preserved; the path is percent-escaped on output;
//   - user input can never change scheme/host and never introduces
//     query, fragment, or userinfo (a fresh url.URL is built from the base);
//   - query/fragment delimiters, backslashes, NUL bytes, traversal (".",
//     ".."), empty segments ("//"), trailing slashes (except root "/"), and
//     encoded "/", "\", "?", "#" ("%2F", "%5C", "%3F", "%23", "%00", encoded
//     dot segments) are rejected.
func (r *Resolver) Resolve(canonicalFullPath string) (*url.URL, error) {
	if r == nil {
		return nil, fmt.Errorf("publicurl: nil resolver")
	}
	if canonicalFullPath == "" {
		return nil, fmt.Errorf("publicurl: path is required")
	}
	if !strings.HasPrefix(canonicalFullPath, "/") {
		return nil, fmt.Errorf("publicurl: path must start with /")
	}
	if strings.Contains(canonicalFullPath, "\\") {
		return nil, fmt.Errorf("publicurl: path must not contain backslash")
	}
	if strings.Contains(canonicalFullPath, "\x00") {
		return nil, fmt.Errorf("publicurl: path must not contain NUL")
	}
	if strings.Contains(canonicalFullPath, "?") {
		return nil, fmt.Errorf("publicurl: path must not contain query")
	}
	if strings.Contains(canonicalFullPath, "#") {
		return nil, fmt.Errorf("publicurl: path must not contain fragment")
	}
	if canonicalFullPath != "/" && strings.Contains(canonicalFullPath, "//") {
		return nil, fmt.Errorf("publicurl: path must not contain empty segment")
	}
	if canonicalFullPath != "/" && strings.HasSuffix(canonicalFullPath, "/") {
		return nil, fmt.Errorf("publicurl: path must not have trailing slash")
	}

	parts := strings.Split(canonicalFullPath, "/")
	decoded := make([]string, 0, len(parts)-1)
	for i := 1; i < len(parts); i++ {
		seg := parts[i]
		// Root "/" splits to ["",""]; there are no segments to validate.
		if canonicalFullPath == "/" && seg == "" {
			continue
		}
		if seg == "" {
			return nil, fmt.Errorf("publicurl: path must not contain empty segment")
		}
		if seg == "." || seg == ".." {
			return nil, fmt.Errorf("publicurl: path must not contain dot segments")
		}
		for _, c := range seg {
			if c < 0x20 || c == 0x7f {
				return nil, fmt.Errorf("publicurl: path must not contain control characters")
			}
		}
		clean := seg
		if strings.Contains(seg, "%") {
			unescaped, err := url.PathUnescape(seg)
			if err != nil {
				return nil, fmt.Errorf("publicurl: bad percent-encoding: %w", err)
			}
			if strings.Contains(unescaped, "/") || strings.Contains(unescaped, "\\") ||
				strings.Contains(unescaped, "?") || strings.Contains(unescaped, "#") ||
				strings.Contains(unescaped, "\x00") {
				return nil, fmt.Errorf("publicurl: path must not contain encoded delimiters")
			}
			if unescaped == "." || unescaped == ".." || unescaped == "" {
				return nil, fmt.Errorf("publicurl: path must not contain encoded dot segments")
			}
			for _, c := range unescaped {
				if c < 0x20 || c == 0x7f {
					return nil, fmt.Errorf("publicurl: path must not contain control characters")
				}
			}
			clean = unescaped
		}
		decoded = append(decoded, clean)
	}

	cleanPath := "/"
	if len(decoded) > 0 {
		cleanPath += strings.Join(decoded, "/")
	}
	fullPath := cleanPath
	if r.basePath != "" {
		fullPath = r.basePath + cleanPath
	}
	return &url.URL{Scheme: r.scheme, Host: r.host, Path: fullPath}, nil
}

func isLocalHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "localhost" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
