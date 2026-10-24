package cloudflare

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestProbeScopedWorkerWhenListForbidden(t *testing.T) {
	calls := 0
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.URL.Path {
		case "/accounts/" + account + "/workers/scripts":
			w.WriteHeader(403)
			io.WriteString(w, `{"success":false}`)
		case "/accounts/" + account + "/workers/scripts/reports/settings":
			io.WriteString(w, `{"success":true,"result":{"bindings":[]}}`)
		case "/accounts/" + account + "/workers/subdomain":
			io.WriteString(w, `{"success":true,"result":{"subdomain":"example"}}`)
		default:
			t.Error(r.URL.Path)
		}
	})
	p, e := c.Probe(context.Background(), account, "reports")
	if e != nil || !p.Readable || p.ListReadable || p.Subdomain != "example" || calls != 3 {
		t.Fatalf("%+v %v calls=%d", p, e, calls)
	}
}
func TestRateLimitHonorsBoundedRetryAfter(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(429)
		io.WriteString(w, `{"success":false}`)
	})
	waits := 0
	c.wait = func(_ context.Context, d time.Duration) error {
		waits++
		if d != 30*time.Second {
			t.Errorf("wrong bounded delay %v", d)
		}
		return nil
	}
	c.Subdomain(context.Background(), account)
	if waits != 4 {
		t.Fatal(waits)
	}
}
func TestUnrequestedHashesAndMissingCompletionRejected(t *testing.T) {
	for _, unknown := range []bool{true, false} {
		t.Run(map[bool]string{true: "unknown hash", false: "missing completion"}[unknown], func(t *testing.T) {
			calls := 0
			hash := AssetHash([]byte("x"), "png")
			if unknown {
				hash = "unknown"
			}
			c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls == 1 {
					io.WriteString(w, `{"success":true,"result":{"jwt":"upload","buckets":[["`+hash+`"]]}}`)
				} else {
					io.WriteString(w, `{"success":true,"result":{}}`)
				}
			})
			if _, e := c.UploadAssets(context.Background(), account, "reports", []File{{Path: "/x.png", MIME: "image/png", Data: []byte("x")}}); e == nil {
				t.Fatal("invalid upload succeeded")
			}
		})
	}
}
