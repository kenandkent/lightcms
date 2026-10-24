package publicurl

import (
	"net/url"
	"strings"
	"testing"
)

func mustBase(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse base %q: %v", raw, err)
	}
	return u
}

func TestPublicURL_HTTPSRequired(t *testing.T) {
	// Production-like public host over plain HTTP must be rejected.
	if _, err := NewResolver(mustBase(t, "http://example.com")); err == nil {
		t.Fatal("expected error for http://example.com (production must be HTTPS)")
	}
	// HTTPS public host is accepted.
	if _, err := NewResolver(mustBase(t, "https://example.com")); err != nil {
		t.Fatalf("expected https base to validate, got %v", err)
	}
	// Local development over HTTP on loopback is allowed.
	if _, err := NewResolver(mustBase(t, "http://localhost:8082")); err != nil {
		t.Fatalf("expected http localhost to validate for dev, got %v", err)
	}
}

func TestPublicURL_CasingPreserved(t *testing.T) {
	r, err := NewResolver(mustBase(t, "https://pages.example.com"))
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	u, err := r.Resolve("/News/Bitcoin-Market-Update")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := u.String(); got != "https://pages.example.com/News/Bitcoin-Market-Update" {
		t.Fatalf("casing not preserved, got %q", got)
	}
}

func TestPublicURL_PathEscaped(t *testing.T) {
	r, err := NewResolver(mustBase(t, "https://pages.example.com"))
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	u, err := r.Resolve("/news/hello world")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if u.Path != "/news/hello world" {
		t.Fatalf("unexpected Path %q", u.Path)
	}
	if got := u.String(); got != "https://pages.example.com/news/hello%20world" {
		t.Fatalf("expected escaped URL, got %q", got)
	}
	if strings.Contains(u.String(), " ") {
		t.Fatal("URL string must not contain raw space")
	}
}

func TestPublicURL_QueryFragmentRejected(t *testing.T) {
	r, err := NewResolver(mustBase(t, "https://pages.example.com"))
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	for _, p := range []string{
		"/news/foo?x=1",
		"/news/foo#frag",
		"/news/foo?x=1#y",
		"/news/foo%3Fbar",
		"/news/foo%23bar",
	} {
		if _, err := r.Resolve(p); err == nil {
			t.Fatalf("expected error for %q (query/fragment must not be in canonical URL)", p)
		}
	}
}

func TestPublicURL_SchemeHostConfusion(t *testing.T) {
	r, err := NewResolver(mustBase(t, "https://pages.example.com"))
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	for _, p := range []string{
		"https://evil.com/phish",
		"http://evil.com/phish",
		"//evil.com/phish",
		"news/foo",
		"",
		"/news/foo?evil=1",
		"/news/foo#evil",
	} {
		if _, err := r.Resolve(p); err == nil {
			t.Fatalf("expected error for %q", p)
		}
	}
	// Valid resolve must never change scheme/host and must carry no query/fragment/userinfo.
	u, err := r.Resolve("/news/bitcoin-market-update")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if u.Scheme != "https" || u.Host != "pages.example.com" {
		t.Fatalf("scheme/host changed: %q", u.String())
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		t.Fatalf("query/fragment/userinfo must be empty: %q", u.String())
	}
}

func TestPublicURL_PathConfusion(t *testing.T) {
	r, err := NewResolver(mustBase(t, "https://pages.example.com"))
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	invalid := []string{
		"",
		"news/foo",
		"/news/../admin",
		"/news/./foo",
		"/news//foo",
		"/news/foo/",
		"/news/foo%2fbar",
		"/news/foo%2Fbar",
		"/news/%2e%2e/admin",
		`/news/foo\bar`,
		"/news/foo%5cbar",
		"/news/foo%5Cbar",
		"/news/foo\x00bar",
		"/../admin",
		"/.",
		"/..",
	}
	for _, p := range invalid {
		if _, err := r.Resolve(p); err == nil {
			t.Fatalf("expected error for path-confusion %q", p)
		}
	}
	valid := []string{
		"/",
		"/news",
		"/news/foo",
		"/News/Foo-Bar_123",
		"/a/b/c",
	}
	for _, p := range valid {
		if _, err := r.Resolve(p); err != nil {
			t.Fatalf("expected valid for %q, got %v", p, err)
		}
	}
}

func TestPublicURL_BaseValidation(t *testing.T) {
	if _, err := NewResolver(nil); err == nil {
		t.Fatal("expected error for nil base URL")
	}
	for _, raw := range []string{
		"ftp://example.com",
		"https://",
		"https://user@example.com",
		"https://user:pass@example.com/",
		"https://example.com/?x=1",
		"https://example.com/#frag",
		"http://example.com",
	} {
		u, _ := url.Parse(raw)
		if _, err := NewResolver(u); err == nil {
			t.Fatalf("expected error for base %q", raw)
		}
	}
}
