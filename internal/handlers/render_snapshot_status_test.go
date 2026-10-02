package handlers

import (
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"testing"
)

func TestFinalReviewLegacyRenderCapacityStatus(t *testing.T) {
	err := &publication.Error{Code: publication.CodeRenderValidation, Message: "frozen render snapshot exceeds 8 MiB"}
	if got := publicationHTTPStatus(err); got != 422 {
		t.Fatalf("render input capacity status=%d, want 422", got)
	}
}
