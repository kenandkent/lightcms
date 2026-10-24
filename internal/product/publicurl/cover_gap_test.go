package publicurl

// Task 17B coverage-gap tests: ValidateProductionBaseURL, Base, and the
// NewResolver/Resolve/isLocalHost branches not hit by resolver_test.go.

import (
	"net/url"
	"strings"
	"testing"
)

func mustParseGap(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return u
}

func TestCoverGapValidateProductionBaseURL(t *testing.T) {
	if err := ValidateProductionBaseURL(nil); err == nil {
		t.Fatalf("nil: want error")
	}
	if err := ValidateProductionBaseURL(mustParseGap(t, "http://example.com")); err == nil {
		t.Fatalf("http: want HTTPS error")
	}
	if err := ValidateProductionBaseURL(mustParseGap(t, "https://")); err == nil {
		t.Fatalf("no host: want error")
	}
	if err := ValidateProductionBaseURL(mustParseGap(t, "https://user@example.com")); err == nil {
		t.Fatalf("userinfo: want error")
	}
	if err := ValidateProductionBaseURL(mustParseGap(t, "https://example.com/?x=1")); err == nil {
		t.Fatalf("query: want error")
	}
	if err := ValidateProductionBaseURL(mustParseGap(t, "https://example.com/#f")); err == nil {
		t.Fatalf("fragment: want error")
	}
	u := mustParseGap(t, "https://example.com")
	u.ForceQuery = true
	if err := ValidateProductionBaseURL(u); err == nil {
		t.Fatalf("force query: want error")
	}
	if err := ValidateProductionBaseURL(mustParseGap(t, "HTTPS://example.com/news")); err != nil {
		t.Fatalf("valid production URL: %v", err)
	}
}

func TestCoverGapBaseAndConstructorBranches(t *testing.T) {
	if _, err := NewResolver(nil); err == nil {
		t.Fatalf("nil base: want error")
	}
	if _, err := NewResolver(mustParseGap(t, "ftp://example.com")); err == nil {
		t.Fatalf("ftp scheme: want error")
	}
	if _, err := NewResolver(mustParseGap(t, "https://")); err == nil {
		t.Fatalf("no host: want error")
	}
	if _, err := NewResolver(mustParseGap(t, "https://user@example.com")); err == nil {
		t.Fatalf("userinfo: want error")
	}
	if _, err := NewResolver(mustParseGap(t, "https://example.com/?x=1")); err == nil {
		t.Fatalf("query: want error")
	}
	if _, err := NewResolver(mustParseGap(t, "https://example.com/#f")); err == nil {
		t.Fatalf("fragment: want error")
	}
	if _, err := NewResolver(mustParseGap(t, "mailto:foo@example.com")); err == nil {
		t.Fatalf("opaque: want error")
	}
	if _, err := NewResolver(mustParseGap(t, "http://127.0.0.1:8080")); err != nil {
		t.Fatalf("http loopback 127.0.0.1: %v", err)
	}
	if _, err := NewResolver(mustParseGap(t, "http://[::1]:8080")); err != nil {
		t.Fatalf("http loopback ::1: %v", err)
	}
	if _, err := NewResolver(mustParseGap(t, "http://127.5.6.7/")); err != nil {
		t.Fatalf("http 127/8: %v", err)
	}
	if _, err := NewResolver(mustParseGap(t, "http://8.8.8.8/")); err == nil {
		t.Fatalf("http public IP: want error")
	}
	if _, err := NewResolver(mustParseGap(t, "http://LOCALHOST:8080/")); err != nil {
		t.Fatalf("http LOCALHOST: %v", err)
	}
	if _, err := NewResolver(mustParseGap(t, "https://example.com/a//b")); err == nil {
		t.Fatalf("double slash base path: want error")
	}
	if _, err := NewResolver(mustParseGap(t, "https://example.com/a/../b")); err == nil {
		t.Fatalf("dotdot base path: want error")
	}
	if _, err := NewResolver(mustParseGap(t, "https://example.com/blog/")); err != nil {
		t.Fatalf("trailing slash base path: %v", err)
	} else {
		got := mustResolveGap(t, mustNew(t, "https://example.com/blog/"), "/News/1")
		if got != "https://example.com/blog/News/1" {
			t.Fatalf("base prefix resolve = %q", got)
		}
		if base := mustNew(t, "https://example.com/blog/").Base(); base.Path != "/blog" {
			t.Fatalf("Base().Path = %q, want /blog", base.Path)
		}
	}
	r := mustNew(t, "https://example.com/")
	if base := r.Base(); base.Scheme != "https" || base.Host != "example.com" || base.Path != "" {
		t.Fatalf("Base() = %v", base)
	}
	if _, err := NewResolver(mustParseGap(t, "https://example.com/")); err != nil {
		t.Fatalf("root base path: %v", err)
	}
}

func mustNew(t *testing.T, s string) *Resolver {
	t.Helper()
	r, err := NewResolver(mustParseGap(t, s))
	if err != nil {
		t.Fatalf("NewResolver(%q): %v", s, err)
	}
	return r
}

func mustResolveGap(t *testing.T, r *Resolver, p string) string {
	t.Helper()
	u, err := r.Resolve(p)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", p, err)
	}
	return u.String()
}

func TestCoverGapResolveEdgeBranches(t *testing.T) {
	var nilR *Resolver
	if _, err := nilR.Resolve("/x"); err == nil {
		t.Fatalf("nil resolver: want error")
	}
	r := mustNew(t, "https://example.com")
	for _, bad := range []string{
		"", "no-slash", "/a\\b", "/a\x00b", "/a?b", "/a#b",
		"/a//b", "/a/", "/.", "/..", "/a/./b", "/a/../b",
		"/a/%2Fb", "/a/%5Cb", "/a/%3Fb", "/a/%23b", "/a/%00b",
		"/%2e", "/%2e%2e", "/%", "/a/%zz",
		"/a/\x01b", "/a/%01b",
	} {
		if _, err := r.Resolve(bad); err == nil {
			t.Fatalf("Resolve(%q): want error", bad)
		}
	}
	if got := mustResolveGap(t, r, "/"); got != "https://example.com/" {
		t.Fatalf("Resolve(/) = %q", got)
	}
	if got := mustResolveGap(t, r, "/caf\u00e9"); !strings.HasPrefix(got, "https://example.com/") {
		t.Fatalf("Resolve(unicode) = %q", got)
	}
	if got := mustResolveGap(t, r, "/a%20b"); got != "https://example.com/a%20b" {
		t.Fatalf("Resolve(space) = %q", got)
	}
}
