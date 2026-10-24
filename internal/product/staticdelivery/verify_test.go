package staticdelivery

import (
	"context"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReviewVerifierChecksThreeRoundsAndRejectsWrongBytes(t *testing.T) {
	target := releaseTarget()
	releaseID := primitive.NewObjectID()
	bad := false
	requests, waits := 0, 0
	var manifest ReleaseManifest
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("X-LC-Release", releaseID.Hex())
		if r.URL.Path == "/.__lc_delivery/health" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"protocol":1,"installation_id":"` + target.InstallationID + `","target_id":"` + target.ID.Hex() + `","release_id":"` + releaseID.Hex() + `"}`))
			return
		}
		p, ok := manifest.Pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		f := manifest.Files[p.File]
		w.Header().Set("Content-Type", f.MIME)
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", `"`+f.SHA256+`"`)
		if r.Method == "HEAD" {
			return
		}
		if bad {
			w.Write([]byte("changed"))
			return
		}
		w.Write(f.Data)
	}))
	defer server.Close()
	var err error
	manifest, err = BuildRelease(context.Background(), BuildInput{ID: releaseID, Target: target, Pages: []CapturedPage{releasePage(primitive.NewObjectID(), "/a")}})
	if err != nil {
		t.Fatal(err)
	}
	verifier := NewReleaseVerifier()
	verifier.client = &http.Client{Transport: originTransport(func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		u := *r.URL
		u.Host = strings.TrimPrefix(server.URL, "https://")
		clone.URL = &u
		return server.Client().Transport.RoundTrip(clone)
	})}
	verifier.wait = func(context.Context, time.Duration) error { waits++; return nil }
	if err = verifier.Check(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	if waits != 2 || requests < 9 {
		t.Fatalf("not three independent rounds: requests=%d waits=%d", requests, waits)
	}
	bad = true
	if err = verifier.Check(context.Background(), manifest); CodeOf(err) != CodeVerificationFailed || strings.Contains(err.Error(), server.URL) {
		t.Fatal("wrong content was accepted or leaked URL", err)
	}
}
