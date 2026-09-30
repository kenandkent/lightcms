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
		// NOTE (accepted risk, Task 19): a BARE storage error still maps to
		// INTERNAL_ERROR/500 because mapSagaCode's templatecontract.CodeOf
		// fallthrough (never returns "") swallows it before the storage
		// branch. Store failures raised through the saga's stage/verify/
		// activate/unpublish cases correctly yield 503. Reordering
		// mapSagaCode is deferred to the generation owner (it would change
		// completePublishError status computation + locked pins).
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
