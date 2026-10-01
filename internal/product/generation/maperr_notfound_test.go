// Task 19 gap 17B/18A: mapSagaErr must preserve spec §27 not-found codes
// (and the store-unavailable passthrough) instead of collapsing them to
// INTERNAL_ERROR. StatusForCode already promises 404/409/503 for these
// codes; the saga translator has to deliver them.
package generation

import (
	"testing"

	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"
)

func TestMapSagaErrPreservesNotFound(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		code   string
		status int
	}{
		{"rollback unknown source",
			&publication.Error{Code: publication.CodeNotFound, Message: "publication abcd not found"},
			CodePublicationNotFound, 404},
		{"publish unknown template",
			&templatecontract.Error{Code: templatecontract.CodeNotFound, Message: "no such template"},
			CodeTemplateNotFound, 404},
		{"publish unknown template version",
			&templatecontract.Error{Code: templatecontract.CodeVersionNotFound, Message: "no such version"},
			CodeTemplateVersionNotFound, 404},
		{"publish template version conflict",
			&templatecontract.Error{Code: templatecontract.CodeVersionConflict, Message: "stale version"},
			CodeTemplateVersionConflict, 409},
		// Lane 2C fix 1 (was: accepted risk, Task 19): a BARE storage error
		// now maps to CodeStoreUnavailable/503 with Retry-After: 30.
		// mapSagaCode checks ""-for-unknown codes before the
		// templatecontract catch-all and matches *storage.Error explicitly.
		// (CodeStoreUnavailable == publication.CodeStageFailed, so the
		// existing stage-failure branch delivers the 503 — a duplicate
		// case value would not compile.)
	}
	for _, c := range cases {
		mapped := mapSagaErr(c.err)
		if CodeOf(mapped) != c.code {
			t.Errorf("%s: code = %q, want %q", c.name, CodeOf(mapped), c.code)
		}
		if got := StatusForCode(CodeOf(mapped)); got != c.status {
			t.Errorf("%s: status = %d, want %d", c.name, got, c.status)
		}
		if mapped.Error() == "" {
			t.Errorf("%s: empty message", c.name)
		}
	}
}
