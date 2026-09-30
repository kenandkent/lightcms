package pathkey_test

// Task 17B coverage-gap tests: InvalidPathError helpers + Canonical edge
// branches not exercised by pathkey_test.go.

import (
	"errors"
	"testing"

	"github.com/jonradoff/lightcms/v7/internal/product/pathkey"
)

func TestCoverGapInvalidPathErrorHelpers(t *testing.T) {
	e := pathkey.InvalidPathError{Path: "/bad//path", Reason: "empty segment not allowed"}
	if got := e.Error(); got == "" || len(got) < 10 {
		t.Fatalf("Error() = %q, want descriptive message", got)
	}
	var nilUnwrap error = (&pathkey.InvalidPathError{Path: "/x", Reason: "r"}).Unwrap()
	if nilUnwrap != nil {
		t.Fatalf("Unwrap() = %v, want nil", nilUnwrap)
	}

	if !pathkey.IsInvalidPath(e) {
		t.Fatalf("IsInvalidPath(value) = false, want true")
	}
	if !pathkey.IsInvalidPath(&e) {
		t.Fatalf("IsInvalidPath(pointer) = false, want true")
	}
	if pathkey.IsInvalidPath(nil) {
		t.Fatalf("IsInvalidPath(nil) = true, want false")
	}
	if pathkey.IsInvalidPath(errors.New("boom")) {
		t.Fatalf("IsInvalidPath(other) = true, want false")
	}

	// Canonical failures must surface as InvalidPathError.
	for _, bad := range []string{"", "no-slash", "/q?q=1", "/f#rag", "/a%2Fb", `/a\b`, "/trail/", "//", "/a/./b", "/a/../b"} {
		_, err := pathkey.Canonical(bad)
		if !pathkey.IsInvalidPath(err) {
			t.Fatalf("Canonical(%q) err = %v, want InvalidPathError", bad, err)
		}
	}

	// NFC convergence: decomposed + precomposed forms share a key.
	pre := "/caf\u00e9"
	decomp := "/café"
	k1, err1 := pathkey.Canonical(pre)
	k2, err2 := pathkey.Canonical(decomp)
	if err1 != nil || err2 != nil {
		t.Fatalf("Canonical unicode: %v %v", err1, err2)
	}
	if k1 != k2 {
		t.Fatalf("NFC keys differ: %q vs %q", k1, k2)
	}
	if k, err := pathkey.Canonical("/"); err != nil || k != "/" {
		t.Fatalf("Canonical(/) = %q, %v", k, err)
	}
}
