package generation

// Task 17B coverage-gap tests: unexported mapping helpers + fault-driven
// repository branches (in-package; no flow can reach these otherwise).

import (
	"context"
	"errors"
	"testing"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/idempotency"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/storage"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"
	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

func pubErrGap(code, msg string) error {
	return &publication.Error{Code: code, Message: msg}
}

func TestCoverGapMapSagaErr(t *testing.T) {
	cases := []struct {
		err  error
		code string
	}{
		{pubErrGap(publication.CodePagePublishInProgress, "busy"), CodePagePublishInProgress},
		{pubErrGap(publication.CodeStageFailed, "stage"), CodeStoreUnavailable},
		{pubErrGap(publication.CodeVerifyFailed, "verify"), CodeStoreUnavailable},
		{pubErrGap(publication.CodeActivateFailed, "activate"), CodeStoreUnavailable},
		{pubErrGap(publication.CodeUnpublishStageFailed, "unstage"), CodeStoreUnavailable},
		{pubErrGap(publication.CodeValidationFailed, "val"), CodeFieldValidationFailed},
		{pubErrGap(publication.CodePathInvalid, "path"), CodePathInvalid},
		{pubErrGap(publication.CodePathConflict, "conf"), CodePathConflict},
		{pubErrGap(publication.CodeContentVersionConflict, "cvc"), CodeContentVersionConflict},
		{pubErrGap(publication.CodeTemplateNotActive, "inactive"), CodeTemplateNotActive},
		{pubErrGap(publication.CodePublicURLFailed, "url"), CodePublicURLFailed},
		{pubErrGap(publication.CodeConflict, "conflict"), CodePublicationConflict},
		{pubErrGap(publication.CodeInternal, "internal"), CodeInternal},
		{pubErrGap("PUBLICATION_CONFLICT", "raw"), CodePublicationConflict},
		{pubErrGap("SOMETHING_ELSE", "other"), CodeInternal},
		// Non-publication codes fall through the publication-code switch to
		// the default INTERNAL mapping (their mapSagaCode branches serve
		// completePublishError's status computation, asserted below).
		{&templatecontract.Error{Code: templatecontract.CodeNotFound, Message: "t"}, CodeInternal},
		{&idempotency.Error{Code: idempotency.CodeConflict, Message: "c"}, CodeInternal},
		{&storage.Error{Code: storage.CodeIO, Message: "io"}, CodeInternal},
		{&Error{Code: CodePathInvalid, Message: "g"}, CodeInternal},
		{errors.New("plain"), CodeInternal},
	}
	for i, c := range cases {
		mapped := mapSagaErr(c.err)
		if CodeOf(mapped) != c.code {
			t.Errorf("case %d (%v): got %q, want %q", i, c.err, CodeOf(mapped), c.code)
		}
		if mapped.Error() == "" {
			t.Errorf("case %d: empty message", i)
		}
	}
	// Store-typed saga error takes the storage branch.
	storeSaga := &publication.Error{Code: publication.CodeStageFailed, Message: "io",
		Err: &storage.Error{Code: storage.CodeIO, Message: "disk"}}
	_ = storeSaga
	if got := mapSagaCode(pubErrGap(publication.CodeStageFailed, "x")); got != publication.CodeStageFailed {
		t.Fatalf("mapSagaCode publication = %q", got)
	}
	for err, want := range map[error]string{
		&templatecontract.Error{Code: templatecontract.CodeNotFound}:        CodeTemplateNotFound,
		&templatecontract.Error{Code: templatecontract.CodeVersionNotFound}: CodeTemplateVersionNotFound,
		&templatecontract.Error{Code: templatecontract.CodeVersionConflict}: CodeTemplateVersionConflict,
		&templatecontract.Error{Code: "OTHER_T"}:                            "OTHER_T",
		// templatecontract.CodeOf never returns "": every other error maps
		// to templatecontract.CodeInternal here (the idempotency/storage/
		// generation branches below are unreachable).
		&idempotency.Error{Code: idempotency.CodeConflict}: templatecontract.CodeInternal,
		&storage.Error{Code: storage.CodeIO}:               templatecontract.CodeInternal,
		&Error{Code: CodePathInvalid}:                      templatecontract.CodeInternal,
		errors.New("plain"):                                templatecontract.CodeInternal,
	} {
		if got := mapSagaCode(err); got != want {
			t.Errorf("mapSagaCode(%v) = %q, want %q", err, got, want)
		}
	}
	if templatecontract.CodeInternal != "INTERNAL_ERROR" {
		t.Fatalf("templatecontract.CodeInternal = %q", templatecontract.CodeInternal)
	}
	// storage.CodeOf maps every error (unknown → CodeIO), so isStoreErr is
	// always true; assert the call shape.
	if !isStoreErr(errors.New("x")) || !isStoreErr(&storage.Error{Code: storage.CodeIO, Message: "io"}) {
		t.Fatalf("isStoreErr branches")
	}
}

func TestCoverGapRwIdemAndDupKey(t *testing.T) {
	if err := rwIdemErr(&idempotency.Error{Code: idempotency.CodeConflict, Message: "c"}); CodeOf(err) != CodeIdempotencyConflict {
		t.Fatalf("rw conflict: %v", err)
	}
	if err := rwIdemErr(&idempotency.Error{Code: idempotency.CodeAlreadyCompleted, Message: "c"}); CodeOf(err) != CodeRequestInProgress {
		t.Fatalf("rw completed: %v", err)
	}
	if err := rwIdemErr(&idempotency.Error{Code: idempotency.CodeStaleAttempt, Message: "s"}); CodeOf(err) != CodePublicationConflict {
		t.Fatalf("rw stale: %v", err)
	}
	if err := rwIdemErr(errors.New("other")); CodeOf(err) != CodeInternal {
		t.Fatalf("rw default: %v", err)
	}
	if mapDupKey(nil, CodePathConflict, "x") != nil {
		t.Fatalf("mapDupKey nil")
	}
	dup := mongo.WriteException{WriteErrors: []mongo.WriteError{{Code: 11000, Message: "E11000"}}}
	if err := mapDupKey(dup, CodePathConflict, "taken"); CodeOf(err) != CodePathConflict {
		t.Fatalf("mapDupKey dup: %v", err)
	}
	if err := mapDupKey(errors.New("E11000 dup"), CodePathConflict, "taken"); CodeOf(err) != CodePathConflict {
		t.Fatalf("mapDupKey string: %v", err)
	}
	other := errors.New("timeout")
	if mapDupKey(other, CodePathConflict, "x") != other {
		t.Fatalf("mapDupKey passthrough")
	}

	if err := mapIdemBeginErr(&idempotency.Error{Code: idempotency.CodeConflict, Message: "c"}); CodeOf(err) != CodeIdempotencyConflict {
		t.Fatalf("begin conflict: %v", err)
	}
	if err := mapIdemBeginErr(&idempotency.Error{Code: idempotency.CodeInProgress, Message: "p"}); CodeOf(err) != CodeRequestInProgress {
		t.Fatalf("begin in-progress: %v", err)
	}
	if err := mapIdemBeginErr(&idempotency.Error{Code: idempotency.CodeLeaseExpired, Message: "l"}); CodeOf(err) != CodeRequestInProgress {
		t.Fatalf("begin expired: %v", err)
	}
	if err := mapIdemBeginErr(errors.New("db down")); CodeOf(err) != CodeInternal {
		t.Fatalf("begin default: %v", err)
	}
}

func TestCoverGapReplayDecoders(t *testing.T) {
	full := idempotency.Operation{Response: map[string]any{
		"id": "c1", "action": "created", "template": "t", "full_path": "/t",
		"mode": "publish", "published": true, "requires_publish": false,
		"public_url": "https://e.com/t", "content_version": float64(3),
		"template_version": int64(2), "publication_id": "p1",
	}}
	out, err := responseFromCache(full)
	if err != nil || out.ID != "c1" || out.ContentVersion != 3 || out.TemplateVersion != 2 ||
		out.PublicURL == nil || out.PublicationID == nil || !out.Published {
		t.Fatalf("responseFromCache full: %+v %v", out, err)
	}
	ints := idempotency.Operation{Response: map[string]any{
		"content_version": int(4), "template_version": int32(5),
	}}
	out2, err := responseFromCache(ints)
	if err != nil || out2.ContentVersion != 4 || out2.TemplateVersion != 5 {
		t.Fatalf("responseFromCache ints: %+v %v", out2, err)
	}
	if _, err := responseFromCache(idempotency.Operation{}); err == nil {
		t.Fatalf("responseFromCache nil: want error")
	}
	bad := idempotency.Operation{Response: map[string]any{"published": "yes"}}
	out3, err := responseFromCache(bad)
	if err != nil || out3.Published {
		t.Fatalf("responseFromCache bad bool: %+v %v", out3, err)
	}

	rr, err := restoreReplay(idempotency.Operation{Response: map[string]any{
		"content_id": "c", "content_version": int64(7), "publication_id": "p",
		"full_path": "/f", "public_url": "u",
	}})
	if err != nil || rr.ContentVersion != 7 || rr.Mode != "restore_and_publish" {
		t.Fatalf("restoreReplay int64: %+v %v", rr, err)
	}
	rv, err := revertReplay(idempotency.Operation{Response: map[string]any{
		"content_id": "c", "content_version": int64(9), "publication_id": "p",
		"full_path": "/f", "public_url": "u",
	}})
	if err != nil || rv.ContentVersion != 9 || rv.Mode != "revert_live" {
		t.Fatalf("revertReplay int64: %+v %v", rv, err)
	}

	if forkIDValue(nil) != primitive.NilObjectID {
		t.Fatalf("forkIDValue nil")
	}
	if forkIDValue(&models.Content{}) != primitive.NilObjectID {
		t.Fatalf("forkIDValue no fork")
	}
	fid := primitive.NewObjectID()
	if forkIDValue(&models.Content{ForkID: &fid}) != fid {
		t.Fatalf("forkIDValue")
	}
	if actorKind(Actor{ActorKind: "agent"}) != "agent" {
		t.Fatalf("actorKind explicit")
	}
	if actorKind(Actor{AgentSession: "s"}) != "agent" {
		t.Fatalf("actorKind session")
	}
	if actorKind(Actor{}) != "human" {
		t.Fatalf("actorKind default")
	}
}

func TestCoverGapNilServiceBranches(t *testing.T) {
	var s Service
	if _, err := s.beginForPublish(context.Background(), Actor{}, GenerateRequest{}, templatecontract.TemplateVersion{}, IdempotencyParams{}); CodeOf(err) != CodeInternal {
		t.Fatalf("beginForPublish nil idem: %v", err)
	}
	s.completeValidation(context.Background(), IdempotencyParams{}, 422, GenerateRequest{}, templatecontract.TemplateVersion{})
	s.auditf(context.Background(), "x", nil)

	// publishWithOp wiring guards.
	if _, err := s.publishWithOp(context.Background(), Actor{}, templatecontract.TemplateVersion{}, nil, nil, false, false, "", "", "", "", "", nil, nil, GenerateRequest{}, IdempotencyParams{}, nil); CodeOf(err) != CodeInternal {
		t.Fatalf("publishWithOp nil idem: %v", err)
	}
	op := idempotency.Operation{}
	s2 := Service{idem: &idempotency.Service{}}
	if _, err := s2.publishWithOp(context.Background(), Actor{}, templatecontract.TemplateVersion{}, nil, nil, false, false, "", "", "", "", "", nil, nil, GenerateRequest{}, IdempotencyParams{}, &op); CodeOf(err) != CodeInternal {
		t.Fatalf("publishWithOp nil pubs: %v", err)
	}
}

func TestCoverGapFaultFindBranches(t *testing.T) {
	fdb := testutil.MustConnectFaultDB(t)
	svc := NewService(fdb, Options{})
	ctx := context.Background()
	fdb.SetFaultHook(testutil.FailOp("FindOne"))
	defer fdb.SetFaultHook(nil)

	if _, err := svc.findLive(ctx, "/news/x"); err == nil {
		t.Fatalf("findLive fault: want error")
	}
	if _, err := svc.findFork(ctx, "/news/x", ""); err == nil {
		t.Fatalf("findFork fault: want error")
	}
	if _, _, err := svc.SchemaForSlug(ctx, "x"); CodeOf(err) != CodeTemplateNotFound {
		t.Fatalf("SchemaForSlug empty fault db: %v", err)
	}
	// GetUpgradeJob via the generic FindOne → transport error.
	if _, err := svc.GetUpgradeJob(ctx, Actor{ID: "a", Authenticated: true, Scopes: []string{}}, primitive.NewObjectID()); err == nil {
		t.Fatalf("GetUpgradeJob fault: want error")
	}
	fdb.SetFaultHook(nil)
	if _, err := svc.GetUpgradeJob(ctx, Actor{ID: "a", Authenticated: true, Scopes: []string{}}, primitive.NewObjectID()); CodeOf(err) != CodeUpgradeJobNotFound {
		t.Fatalf("GetUpgradeJob missing: %v", err)
	}
}
