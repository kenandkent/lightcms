package pathkey

import (
	"strings"
	"testing"
)

// Task 2: /News/Foo and /news/foo yield the same key.
func TestCanonical_CaseInsensitive(t *testing.T) {
	a, err := Canonical("/News/Foo")
	if err != nil {
		t.Fatalf("Canonical(/News/Foo): %v", err)
	}
	b, err := Canonical("/news/foo")
	if err != nil {
		t.Fatalf("Canonical(/news/foo): %v", err)
	}
	if a != b {
		t.Errorf("case variants differ: %q vs %q", a, b)
	}
	if a != "/news/foo" {
		t.Errorf("expected /news/foo, got %q", a)
	}
}

// Valid authored FullPath casing stays unchanged (Canonical must not mutate input).
func TestCanonical_PreservesAuthoredCasing(t *testing.T) {
	in := "/News/Foo"
	orig := in
	key, err := Canonical(in)
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	if in != orig {
		t.Errorf("input mutated: was %q now %q", orig, in)
	}
	if key == in {
		t.Errorf("expected folded key to differ from authored casing %q", in)
	}
	if strings.ToLower(in) != key {
		t.Errorf("expected key %q, got %q", strings.ToLower(in), key)
	}
}

func TestCanonical_Rejects(t *testing.T) {
	cases := map[string]string{
		"/a/../b":          "traversal ..",
		"/a/./b":           "traversal .",
		"/../etc":          "leading traversal",
		"/a//b":            "empty segment",
		"/news/foo?bar=1":  "query",
		"/news/foo#sec":    "fragment",
		"/news%2ffoo":      "encoded slash lower",
		"/news%2Foo":       "encoded slash upper",
		"":                 "empty path",
		"news/foo":         "missing leading slash",
		"/news/foo/":       "trailing slash",
		"/a/./":            "dot with trailing slash",
		"/.":               "single dot",
		"/..":              "single dotdot",
		"/news/foo\\bar":   "backslash traversal",
		"/news/foo?":       "bare query marker",
		"/news/foo#":       "bare fragment marker",
		"/a/%2F/b":         "encoded slash in segment",
		"/a/%2f/b":         "encoded slash lower in segment",
	}
	for path, why := range cases {
		if got, err := Canonical(path); err == nil {
			t.Errorf("%s (%q): expected error, got key %q", why, path, got)
		continue
		} else if _, ok := err.(InvalidPathError); !ok {
			// Accept pointer form as well.
			if _, ok := err.(*InvalidPathError); !ok {
				t.Errorf("%s (%q): expected InvalidPathError, got %T: %v", why, path, err, err)
			}
		}
	}
}

func TestCanonical_Valid(t *testing.T) {
	valid := map[string]string{
		"/":                "/",
		"/news":            "/news",
		"/News":            "/news",
		"/news/foo-bar_baz": "/news/foo-bar_baz",
		"/a/b/c":           "/a/b/c",
		"/CLAUDE.md":       "/claude.md",
		"/news/2024/hello": "/news/2024/hello",
	}
	for in, want := range valid {
		got, err := Canonical(in)
		if err != nil {
			t.Errorf("Canonical(%q): unexpected error %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("Canonical(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCanonical_UnicodeNFCFold(t *testing.T) {
	// e + combining acute (U+0065 U+0301) must NFC-normalize to é (U+00E9)
	// and fold identically to the precomposed form in any casing.
	decomposed := "/News/Caf\u0065\u0301"
	composed := "/news/caf\u00e9"
	a, err := Canonical(decomposed)
	if err != nil {
		t.Fatalf("Canonical decomposed: %v", err)
	}
	b, err := Canonical(composed)
	if err != nil {
		t.Fatalf("Canonical composed: %v", err)
	}
	if a != b {
		t.Errorf("NFC fold mismatch: %q vs %q", a, b)
	}
}
