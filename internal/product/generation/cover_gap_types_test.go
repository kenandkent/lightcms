package generation_test

// Task 17B coverage-gap tests: generation types matrix (pure, no DB).

import (
	"context"
	"testing"

	"github.com/jonradoff/lightcms/v7/internal/product/generation"
)

func TestCoverGapStatusForCodeTable(t *testing.T) {
	cases := map[string]int{
		generation.CodeUnauthenticated: 401,
		generation.CodePermissionDenied: 403,
		generation.CodeTemplateNotFound: 404,
		generation.CodeContentNotFound: 404,
		generation.CodePublicationNotFound: 404,
		generation.CodeTemplateVersionNotFound: 404,
		generation.CodeUpgradeJobNotFound: 404,
		generation.CodeFieldValidationFailed: 422,
		generation.CodePathInvalid: 422,
		generation.CodeDataTooLarge: 422,
		generation.CodeTemplateSchemaInvalid: 422,
		generation.CodeTemplatePreconditionRequired: 428,
		generation.CodeIdempotencyKeyRequired: 428,
		generation.CodeRateLimited: 429,
		generation.CodeStoreUnavailable: 503,
		generation.CodePathConflict: 409,
		generation.CodeTemplateNotActive: 409,
		generation.CodeTemplateVersionChanged: 409,
		generation.CodeTemplateVersionConflict: 409,
		generation.CodeContentVersionConflict: 409,
		generation.CodePublicationConflict: 409,
		generation.CodePagePublishInProgress: 409,
		generation.CodeIdempotencyConflict: 409,
		generation.CodeRequestInProgress: 409,
		generation.CodeAgentSandboxRequired: 409,
		generation.CodeUpgradeJobConflict: 409,
		generation.CodeInvalidRequest: 400,
		generation.CodeInternal: 500,
		generation.CodeContentCreateFailed: 500,
		generation.CodeContentUpdateFailed: 500,
		"NOPE": 500,
		"": 500,
	}
	for code, want := range cases {
		if got := generation.StatusForCode(code); got != want {
			t.Errorf("StatusForCode(%q) = %d, want %d", code, got, want)
		}
	}
	for _, c := range []string{
		generation.CodeRateLimited, generation.CodeRequestInProgress,
		generation.CodeStoreUnavailable, generation.CodePagePublishInProgress,
	} {
		if !generation.Retryable(c) {
			t.Errorf("Retryable(%q) = false", c)
		}
	}
	if generation.Retryable(generation.CodeInternal) || generation.Retryable("") {
		t.Fatalf("Retryable negative")
	}
}

func TestCoverGapActorAndErrorHelpers(t *testing.T) {
	full := generation.Actor{Scopes: []string{}}
	if !full.HasScope("anything") || !full.HasScopes("a", "b") {
		t.Fatalf("empty scopes = full permissions")
	}
	lim := generation.Actor{Scopes: []string{"content.view"}}
	if !lim.HasScope("content.view") || lim.HasScope("content.edit") {
		t.Fatalf("HasScope allowlist")
	}
	if lim.HasScopes("content.view", "content.edit") {
		t.Fatalf("HasScopes partial")
	}
	if !lim.HasScopes("content.view") {
		t.Fatalf("HasScopes single")
	}
	ownerID := generation.Actor{ID: "i"}
	if ownerID.Owner() != "i" {
		t.Fatalf("Owner ID")
	}
	ownerEmail := generation.Actor{Email: "e"}
	if ownerEmail.Owner() != "e" {
		t.Fatalf("Owner email")
	}
	ownerAnon := generation.Actor{}
	if ownerAnon.Owner() != "anonymous" {
		t.Fatalf("Owner anonymous")
	}

	withCause := &generation.Error{Code: generation.CodeInternal, Message: "m", Err: errGapGenTest}
	if got := withCause.Error(); got == "" {
		t.Fatalf("Error() empty")
	}
	plain := &generation.Error{Code: generation.CodeInvalidRequest, Message: "bad"}
	if got := plain.Error(); got == "" {
		t.Fatalf("Error() plain empty")
	}
	if plain.Unwrap() != nil || withCause.Unwrap() == nil {
		t.Fatalf("Unwrap branches")
	}
	if generation.CodeOf(nil) != "" || generation.CodeOf(plain) != generation.CodeInvalidRequest ||
		generation.CodeOf(errGapGenTest) != "" {
		t.Fatalf("CodeOf branches")
	}

	ctx := generation.WithActor(context.Background(), lim)
	a, ok := generation.ActorFrom(ctx)
	if !ok || !a.HasScope("content.view") {
		t.Fatalf("ActorFrom present")
	}
	if _, ok := generation.ActorFrom(context.Background()); ok {
		t.Fatalf("ActorFrom absent")
	}
	idem := generation.IdempotencyParams{Owner: "o", Method: "POST", Path: "/p", Key: "k"}
	ctx2 := generation.WithIdempotency(context.Background(), idem)
	if p, ok := generation.IdempotencyFrom(ctx2); !ok || p.Key != "k" {
		t.Fatalf("IdempotencyFrom present")
	}
	if _, ok := generation.IdempotencyFrom(context.Background()); ok {
		t.Fatalf("IdempotencyFrom absent")
	}
	ctx3 := generation.WithIdempotency(context.Background(), generation.IdempotencyParams{})
	if _, ok := generation.IdempotencyFrom(ctx3); ok {
		t.Fatalf("IdempotencyFrom empty key")
	}
}

type errGapGen string

func (e errGapGen) Error() string { return string(e) }

var errGapGenTest error = errGapGen("gap")
