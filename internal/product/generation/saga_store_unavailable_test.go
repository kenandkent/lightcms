package generation

// Lane 2C fix 1 (red): bare storage errors must map to the retryable 503
// CodeStoreUnavailable (with Retry-After), not INTERNAL_ERROR/500.
// mapSagaCode's templatecontract.CodeOf fallthrough (never "") swallows
// storage errors before the storage branch.

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jonradoff/lightcms/v7/internal/product/idempotency"
	"github.com/jonradoff/lightcms/v7/internal/product/storage"
)

func TestMapSagaErrBareStorageIsRetryable503(t *testing.T) {
	bare := &storage.Error{Code: storage.CodeIO, Message: "disk exploded"}
	if got := mapSagaCode(bare); got != CodeStoreUnavailable {
		t.Fatalf("mapSagaCode(bare storage) = %q, want %q", got, CodeStoreUnavailable)
	}
	mapped := mapSagaErr(bare)
	if CodeOf(mapped) != CodeStoreUnavailable {
		t.Fatalf("mapSagaErr(bare storage) code = %q, want %q", CodeOf(mapped), CodeStoreUnavailable)
	}
	if got := StatusForCode(CodeOf(mapped)); got != 503 {
		t.Fatalf("status = %d, want 503", got)
	}
	ge, ok := mapped.(*Error)
	if !ok {
		t.Fatalf("mapped type = %T, want *generation.Error", mapped)
	}
	if ge.RetryAfter != 30 {
		t.Fatalf("RetryAfter = %d, want 30", ge.RetryAfter)
	}
	if !Retryable(CodeOf(mapped)) {
		t.Fatalf("Retryable(%q) = false, want true", CodeOf(mapped))
	}
}

func TestMapSagaCodeWrappedStorageIsRetryable503(t *testing.T) {
	wrapped := fmt.Errorf("stage immutable object: %w",
		&storage.Error{Code: storage.CodeHashMismatch, Message: "sha mismatch", Path: "/x"})
	if got := mapSagaCode(wrapped); got != CodeStoreUnavailable {
		t.Fatalf("mapSagaCode(wrapped storage) = %q, want %q", got, CodeStoreUnavailable)
	}
	if got := CodeOf(mapSagaErr(wrapped)); got != CodeStoreUnavailable {
		t.Fatalf("mapSagaErr(wrapped storage) = %q, want %q", got, CodeStoreUnavailable)
	}
}

func TestMapSagaCodePassthroughPreserved(t *testing.T) {
	// Plain unknown errors must still collapse to INTERNAL_ERROR/500.
	if got := mapSagaCode(errors.New("plain boom")); got != CodeInternal {
		t.Fatalf("mapSagaCode(plain) = %q, want %q", got, CodeInternal)
	}
	if got := CodeOf(mapSagaErr(errors.New("plain boom"))); got != CodeInternal {
		t.Fatalf("mapSagaErr(plain) = %q, want %q", got, CodeInternal)
	}
	// Typed idempotency errors keep their own codes (no longer swallowed
	// by the templatecontract catch-all).
	if got := mapSagaCode(&idempotency.Error{Code: idempotency.CodeConflict, Message: "c"}); got != idempotency.CodeConflict {
		t.Fatalf("mapSagaCode(idem conflict) = %q, want %q", got, idempotency.CodeConflict)
	}
	// Generation codes pass through untouched.
	if got := mapSagaCode(&Error{Code: CodePathInvalid, Message: "p"}); got != CodePathInvalid {
		t.Fatalf("mapSagaCode(generation) = %q, want %q", got, CodePathInvalid)
	}
}
