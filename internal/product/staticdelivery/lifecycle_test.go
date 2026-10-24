package staticdelivery

import (
	"context"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestLifecycleConfirmationRechecksAdminRole(t *testing.T) {
	s := NewCommandService(&Repository{}, CommandOptions{Authorize: func(context.Context, ActorSnapshot, bool, bool) error { return nil }})
	for _, op := range []string{"change_origin", "change_environment"} {
		_, err := s.SubmitDeployment(context.Background(), ActorSnapshot{UserID: "editor", Owner: "editor", Role: "editor"}, "key", primitive.NewObjectID(), DeployRequest{Operation: op})
		if CodeOf(err) != CodePermissionDenied {
			t.Fatalf("%s role downgrade accepted: %v", op, err)
		}
	}
}

func TestLifecycleWithdrawPreservesOtherPages(t *testing.T) {
	ctx := context.Background()
	target := releaseTarget()
	current, err := BuildRelease(ctx, BuildInput{ID: primitive.NewObjectID(), Target: target, Pages: []CapturedPage{releasePage(primitive.NewObjectID(), "/one"), releasePage(primitive.NewObjectID(), "/two")}})
	if err != nil {
		t.Fatal(err)
	}
	input, err := lifecycleBuildInput(target, DeployRequest{Operation: "withdraw", Paths: []string{"/one"}}, &current, nil)
	if err != nil {
		t.Fatal(err)
	}
	next, err := BuildRelease(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := next.Pages["/one"]; exists {
		t.Fatal("withdrawn page retained")
	}
	if next.Pages["/two"].SHA256 != current.Pages["/two"].SHA256 {
		t.Fatal("unselected page changed")
	}
}
func TestLifecycleRollbackUsesFrozenHistoricalPage(t *testing.T) {
	ctx := context.Background()
	target := releaseTarget()
	one := releasePage(primitive.NewObjectID(), "/one")
	historical, err := BuildRelease(ctx, BuildInput{ID: primitive.NewObjectID(), Target: target, Pages: []CapturedPage{one}})
	if err != nil {
		t.Fatal(err)
	}
	current, err := BuildRelease(ctx, BuildInput{ID: primitive.NewObjectID(), Target: target, Current: &historical, Pages: []CapturedPage{releasePage(primitive.NewObjectID(), "/two")}})
	if err != nil {
		t.Fatal(err)
	}
	input, err := lifecycleBuildInput(target, DeployRequest{Operation: "rollback_pages", Paths: []string{"/one"}}, &current, &historical)
	if err != nil {
		t.Fatal(err)
	}
	next, err := BuildRelease(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if next.Pages["/one"].PublicationID != historical.Pages["/one"].PublicationID || next.Pages["/two"].SHA256 != current.Pages["/two"].SHA256 {
		t.Fatal("rollback rebuilt history or removed unrelated page")
	}
	foreign := historical
	foreign.TargetID = primitive.NewObjectID()
	if _, err := lifecycleBuildInput(target, DeployRequest{Operation: "rollback_site"}, &current, &foreign); err == nil {
		t.Fatal("foreign target release accepted")
	}
}

func TestWithdrawVerificationRejectsStillVisiblePage(t *testing.T) {
	manifest, err := BuildRelease(context.Background(), BuildInput{ID: primitive.NewObjectID(), Target: releaseTarget()})
	if err != nil {
		t.Fatal(err)
	}
	manifest.RemovedPaths = []string{"/removed"}
	status := 200
	checked := map[string]bool{}
	v := NewReleaseVerifier()
	v.wait = func(context.Context, time.Duration) error { return nil }
	v.client = &http.Client{Transport: originTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/.__lc_delivery/health" {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(staticHealth(manifest.InstallationID, manifest.TargetID.Hex(), manifest.ID.Hex()))))}, nil
		}
		checked[r.Method+" "+r.URL.Path] = true
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("old page"))}, nil
	})}
	if err := v.Check(context.Background(), manifest); err == nil {
		t.Fatal("withdrawal succeeded while old page still returned 200")
	}
	status = 404
	if err := v.Check(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/removed", "/removed/", "/removed.html"} {
		for _, method := range []string{"GET", "HEAD"} {
			if !checked[method+" "+path] {
				t.Fatalf("missing removal verification %s %s", method, path)
			}
		}
	}
}
