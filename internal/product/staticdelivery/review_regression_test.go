package staticdelivery

import (
	"context"
	"errors"
	ex "github.com/metavert-io/lightcms/v7/internal/product/staticexport"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"strings"
	"testing"
	"time"
)

func TestReviewExportErrorsPreserveHTTPCode(t *testing.T) {
	err := &ex.Error{Code: CodeArtifactUnavailable, Message: "missing"}
	if StatusForError(err) != 410 {
		t.Fatalf("export 410 was remapped to %d", StatusForError(err))
	}
}
func TestReviewRetiredAssetsDefaultToSevenDays(t *testing.T) {
	now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	target := releaseTarget()
	old := ReleaseManifest{TargetID: target.ID, Pages: map[string]ReleasePage{}, Files: map[string]ReleaseFile{"/_lc-assets/a.png": {Path: "/_lc-assets/a.png", Data: []byte("a"), SHA256: releaseHash([]byte("a")), MIME: "image/png", Size: 1}}}
	r, e := BuildRelease(context.Background(), BuildInput{ID: primitive.NewObjectID(), Target: target, Current: &old, Now: now})
	if e != nil {
		t.Fatal(e)
	}
	until := r.Files["/_lc-assets/a.png"].RetainedUntil
	if until == nil || !until.Equal(now.Add(7*24*time.Hour)) {
		t.Fatalf("wrong retention %v", until)
	}
}
func TestReviewRecoveryCredentialFailureStaysUncertain(t *testing.T) {
	r, ctx := connectDeliveryTestDB(t)
	target := releaseTarget()
	target.Status = "ready"
	now := time.Now().UTC()
	job := Job{Record: newRecord(), OperationID: primitive.NewObjectID(), State: JobVerifying, TargetID: target.ID, ReleaseID: primitive.NewObjectID(), Checkpoint: Checkpoint{ActivationIntentAt: &now}}
	target.ActiveJobID = job.ID
	r.collection("targets").InsertOne(ctx, target)
	r.collection("jobs").InsertOne(ctx, job)
	runner := NewRunner(r, RunnerDependencies{Owner: "review", Authorize: func(context.Context, Job, bool) error { return nil }, Remote: func(context.Context, Job, Target) (Remote, error) {
		return nil, failure(CodeCredentialUnavailable, "revoked", errors.New("private"))
	}})
	if e := runner.RunOnce(ctx); e != nil {
		t.Fatalf("recovery entered invalid transition: %v", e)
	}
	got, _ := r.GetTarget(ctx, target.ID)
	if got.ActiveJobID != job.ID {
		t.Fatal("unknown target owner released")
	}
}
func TestReviewDataSocialImageRejected(t *testing.T) {
	p := releasePage(primitive.NewObjectID(), "/a")
	p.Document.HTML = []byte(`<!doctype html><html><head><title>A</title><meta property="og:image" content="data:image/png;base64,abc"><link rel="icon" href="data:image/svg+xml,icon"></head><body><h1>A</h1></body></html>`)
	target := releaseTarget()
	target.Environment = "production"
	if _, e := BuildRelease(context.Background(), BuildInput{ID: primitive.NewObjectID(), Target: target, Pages: []CapturedPage{p}}); CodeOf(e) != CodeMetadataInvalid {
		t.Fatalf("invalid social image accepted: %v", e)
	}
}

func TestReviewMergeKeepsUnchangedPageBytes(t *testing.T) {
	target := releaseTarget()
	p := releasePage(primitive.NewObjectID(), "/a")
	first, e := BuildRelease(context.Background(), BuildInput{ID: primitive.NewObjectID(), Target: target, Pages: []CapturedPage{p}})
	if e != nil {
		t.Fatal(e)
	}
	calls := 0
	next, e := BuildRelease(context.Background(), BuildInput{ID: primitive.NewObjectID(), Target: target, Current: &first, Pages: []CapturedPage{releasePage(primitive.NewObjectID(), "/b")}, Defaults: func(context.Context, string) (ex.Resource, ex.Resource, error) {
		calls++
		return ex.Resource{}, ex.Resource{}, nil
	}})
	if e != nil {
		t.Fatal(e)
	}
	if calls != 1 {
		t.Fatalf("unchanged page was rendered against mutable defaults; calls=%d", calls)
	}
	if first.Pages["/a"].SHA256 != next.Pages["/a"].SHA256 {
		t.Fatal("unchanged page mutated")
	}
}

func TestReviewMissingCurrentFileCannotBeRebuiltAsEmpty(t *testing.T) {
	target := releaseTarget()
	p := releasePage(primitive.NewObjectID(), "/a")
	first, e := BuildRelease(context.Background(), BuildInput{ID: primitive.NewObjectID(), Target: target, Pages: []CapturedPage{p}})
	if e != nil {
		t.Fatal(e)
	}
	delete(first.Files, first.Pages["/a"].File)
	if _, e = BuildRelease(context.Background(), BuildInput{ID: primitive.NewObjectID(), Target: target, Current: &first}); CodeOf(e) != CodeArtifactUnavailable {
		t.Fatalf("missing current artifact was silently reconstructed: %v", e)
	}
}

func TestReviewMetadataUsesExistingOGTitle(t *testing.T) {
	p := releasePage(primitive.NewObjectID(), "/a")
	p.Document.HTML = []byte(`<!doctype html><html><head><meta property="og:title" content="Original publication"></head><body>Frozen article text</body></html>`)
	r, e := BuildRelease(context.Background(), BuildInput{ID: primitive.NewObjectID(), Target: releaseTarget(), Pages: []CapturedPage{p}})
	if e != nil {
		t.Fatal("OG title fallback rejected", e)
	}
	if !strings.Contains(string(r.Files[r.Pages["/a"].File].Data), "<title>Original publication</title>") {
		t.Fatal("missing original title fallback")
	}
}
