package staticdelivery

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

type originTransport func(*http.Request) (*http.Response, error)

func (f originTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestOriginCheckMatchesTargetMarkerAndDeniesPrivate(t *testing.T) {
	target := releaseTarget()
	target.Origin = "https://reports.example.com"
	state := RemoteState{Exists: true, InstallationID: target.InstallationID, TargetID: target.ID.Hex(), ReleaseID: ""}
	checker := NewOriginVerifier()
	checker.client = &http.Client{Transport: originTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/.__lc_delivery/health" {
			t.Error(r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"protocol":1,"installation_id":"installation","target_id":"` + target.ID.Hex() + `","release_id":""}`))}, nil
	})}
	if e := checker.Check(context.Background(), target, state); e != nil {
		t.Fatal(e)
	}
	state.InstallationID = "different"
	if e := checker.Check(context.Background(), target, state); CodeOf(e) != CodeOriginNotReady {
		t.Fatal("mismatched health accepted", e)
	}
	for _, address := range []string{"127.0.0.1", "10.0.0.1", "::1", "169.254.169.254", "100.64.0.1"} {
		if publicAddress(net.ParseIP(address)) {
			t.Fatal("private/special address accepted", address)
		}
	}
	if !publicAddress(net.ParseIP("1.1.1.1")) {
		t.Fatal("public address rejected")
	}
}
