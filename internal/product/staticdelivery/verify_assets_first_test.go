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

func TestAssetsFirstVerifierAcceptsNativeETagButRejectsStaleBytes(t *testing.T) {
	target := releaseTarget()
	r, e := BuildRelease(context.Background(), BuildInput{ID: primitive.NewObjectID(), Target: target, Pages: []CapturedPage{releasePage(primitive.NewObjectID(), "/a")}})
	if e != nil {
		t.Fatal(e)
	}
	bad := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		w.Header().Set("X-LC-Release", r.ID.Hex())
		if q.URL.Path == "/.__lc_delivery/health" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"protocol":1,"installation_id":"` + target.InstallationID + `","target_id":"` + target.ID.Hex() + `","release_id":"` + r.ID.Hex() + `"}`))
			return
		}
		f := r.Files[r.Pages["/a"].File]
		w.Header().Set("Content-Type", f.MIME)
		w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")
		w.Header().Set("ETag", `"cloudflare-native-validator"`)
		if q.Method != "HEAD" {
			if bad {
				w.Write([]byte("stale report"))
			} else {
				w.Write(f.Data)
			}
		}
	}))
	defer server.Close()
	v := NewReleaseVerifier()
	v.client = &http.Client{Transport: originTransport(func(q *http.Request) (*http.Response, error) {
		clone := q.Clone(q.Context())
		u := *q.URL
		u.Host = strings.TrimPrefix(server.URL, "https://")
		clone.URL = &u
		return server.Client().Transport.RoundTrip(clone)
	})}
	v.wait = func(context.Context, time.Duration) error { return nil }
	if e = v.Check(context.Background(), r); e != nil {
		t.Fatal("native asset response rejected", e)
	}
	bad = true
	if e = v.Check(context.Background(), r); CodeOf(e) != CodeVerificationFailed {
		t.Fatal("native ETag was trusted instead of verifying bytes", e)
	}
}
