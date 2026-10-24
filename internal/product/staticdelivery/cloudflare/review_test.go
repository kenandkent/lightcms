package cloudflare

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestReviewReadsRetryTransportFailures(t *testing.T) {
	calls := 0
	c := newTestClient("secret", "https://api.test", &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls < 3 {
			return nil, errors.New("temporary network fault")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"success":true,"result":{"subdomain":"example"}}`))}, nil
	})}, func(context.Context, time.Duration) error { return nil })
	domain, e := c.Subdomain(context.Background(), account)
	if e != nil || domain != "example" || calls != 3 {
		t.Fatalf("transport retries missing: domain=%s error=%v calls=%d", domain, e, calls)
	}
}
func TestReviewDefaultTimeoutMatchesSpec(t *testing.T) {
	if NewClient("token", Options{}).http.Timeout != 30*time.Second {
		t.Fatal("HTTP timeout differs from documented30 seconds")
	}
}
