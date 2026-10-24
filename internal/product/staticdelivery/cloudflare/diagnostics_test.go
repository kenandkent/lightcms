package cloudflare

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestAPIErrorPreservesSafeDiagnostics(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("CF-Ray", "abc123-YYZ")
		w.WriteHeader(400)
		io.WriteString(w, `{"success":false,"errors":[{"code":10021,"message":"Invalid metadata api-secret completion-secret"}]}`)
	})
	err := c.request(context.Background(), "POST", "/test", "completion-secret", "application/json", nil, nil, false, true)
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatal(err)
	}
	s := err.Error()
	if !strings.Contains(s, "10021") || !strings.Contains(s, "Invalid metadata") || !strings.Contains(s, "abc123-YYZ") {
		t.Fatalf("missing diagnostics: %s", s)
	}
	if strings.Contains(s, "api-secret") || strings.Contains(s, "completion-secret") {
		t.Fatalf("credential leaked: %s", s)
	}
}
