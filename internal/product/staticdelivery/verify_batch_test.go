package staticdelivery

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestVerificationBatchResumesWithoutRepeatingCompletedChecks(t *testing.T) {
	pages := []CapturedPage{}
	for i := 0; i < 7; i++ {
		pages = append(pages, releasePage(primitive.NewObjectID(), fmt.Sprintf("/page-%d", i)))
	}
	manifest, err := BuildRelease(context.Background(), BuildInput{ID: primitive.NewObjectID(), Target: releaseTarget(), Pages: pages})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	counts := map[string]int{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.__lc_delivery/health" {
			w.Header().Set("Content-Type", "application/json")
			w.Write(staticHealth(manifest.InstallationID, manifest.TargetID.Hex(), manifest.ID.Hex()))
			return
		}
		mu.Lock()
		counts[r.Method+" "+r.URL.Path]++
		mu.Unlock()
		f := manifest.Files[manifest.Pages[r.URL.Path].File]
		w.Header().Set("Content-Type", f.MIME)
		w.Header().Set("X-LC-Release", manifest.ID.Hex())
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", `"native"`)
		if r.Method != "HEAD" {
			w.Write(f.Data)
		}
	}))
	defer server.Close()
	v := NewReleaseVerifier()
	v.client = &http.Client{Transport: originTransport(func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		u := *r.URL
		u.Host = strings.TrimPrefix(server.URL, "https://")
		clone.URL = &u
		return server.Client().Transport.RoundTrip(clone)
	})}
	cursor := VerificationProgress{}
	done := false
	batches := 0
	for !done && batches < 30 {
		cursor, done, err = v.CheckBatch(context.Background(), manifest, cursor)
		if err != nil {
			t.Fatal(err)
		}
		batches++
	}
	if !done || batches != 12 {
		t.Fatalf("cursor=%+v batches=%d", cursor, batches)
	}
	for key, count := range counts {
		if count != 3 {
			t.Fatalf("%s checked %d times, want three rounds", key, count)
		}
	}
	if len(counts) != 14 {
		t.Fatalf("missing GET/HEAD checks: %v", counts)
	}
}
