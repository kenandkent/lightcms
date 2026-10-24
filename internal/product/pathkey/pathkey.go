package pathkey

import (
	"fmt"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// InvalidPathError is a typed invalid-path error returned by Canonical.
// Callers map it to 409 PATH_CONFLICT / 422 depending on context; the
// publication and generation layers treat duplicate-key as the final arbiter.
type InvalidPathError struct {
	Path   string
	Reason string
}

func (e InvalidPathError) Error() string {
	return fmt.Sprintf("invalid path %q: %s", e.Path, e.Reason)
}

func (e *InvalidPathError) Unwrap() error { return nil }

// IsInvalidPath reports whether err is an InvalidPathError (value or pointer).
func IsInvalidPath(err error) bool {
	if err == nil {
		return false
	}
	switch err.(type) {
	case InvalidPathError, *InvalidPathError:
		return true
	}
	return false
}

// Canonical returns the collision-safe business key for an authored FullPath.
//
// Rules (Task 2 / spec §12.8):
//   - FullPath keeps the author's canonical casing; this function returns a
//     separate lowercased, Unicode NFC-normalized key used by the
//     UNIQUE(canonical_full_path, path_scope) index.
//   - "/News/Foo" and "/news/foo" yield the same key.
//   - Traversal ("." / ".." segments, backslash), empty segments ("//",
//     trailing "/" except root "/"), query ("?"), fragment ("#"), and
//     encoded slash ("%2F" case-insensitive) are rejected with InvalidPathError.
//   - The input string is never mutated.
func Canonical(fullPath string) (string, error) {
	orig := fullPath
	fail := func(reason string) (string, error) {
		return "", InvalidPathError{Path: orig, Reason: reason}
	}

	if fullPath == "" {
		return fail("empty path")
	}
	if !strings.HasPrefix(fullPath, "/") {
		return fail("must start with /")
	}
	if strings.Contains(fullPath, "?") {
		return fail("query not allowed in canonical path")
	}
	if strings.Contains(fullPath, "#") {
		return fail("fragment not allowed in canonical path")
	}
	if strings.Contains(strings.ToLower(fullPath), "%2f") {
		return fail("encoded slash not allowed in canonical path")
	}
	if strings.Contains(fullPath, "\\") {
		return fail("backslash not allowed in canonical path")
	}
	if fullPath == "/" {
		return "/", nil
	}
	if strings.HasSuffix(fullPath, "/") {
		return fail("trailing slash not allowed (use canonical form without trailing slash)")
	}
	if strings.Contains(fullPath, "//") {
		return fail("empty segment not allowed")
	}

	parts := strings.Split(fullPath, "/")
	// parts[0] == "" (leading slash). Every other segment must be non-empty
	// and must not be "." or "..".
	for i := 1; i < len(parts); i++ {
		seg := parts[i]
		if seg == "" {
			return fail("empty segment not allowed")
		}
		if seg == "." || seg == ".." {
			return fail("path traversal not allowed")
		}
	}

	// Unicode NFC normalization + case folding. NFC first so decomposed and
	// precomposed forms converge, then simple case folding via ToLower, then
	// NFC again (lowercasing can introduce decomposed forms).
	folded := norm.NFC.String(fullPath)
	folded = strings.ToLower(folded)
	folded = norm.NFC.String(folded)
	return folded, nil
}
