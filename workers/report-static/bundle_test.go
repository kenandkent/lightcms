package reportstatic

import (
	"strings"
	"testing"
)

func TestBundleModuleNamesAndMIME(t *testing.T) {
	m, e := Modules([]byte(`{"pages":{},"aliases":{},"assets":{},"release_id":"r1"}`))
	if e != nil {
		t.Fatal(e)
	}
	if len(m) != 3 || m[0].Name != "index.js" || m[1].Name != "handler.mjs" || m[2].Name != "routes.json" || m[0].MIME != "application/javascript+module" || m[2].MIME != "text/plain" || !strings.Contains(string(m[0].Data), "JSON.parse(routes)") {
		t.Fatalf("invalid module bundle %+v", m)
	}
	if _, e = Modules([]byte(`not-json`)); e == nil {
		t.Fatal("invalid routes accepted")
	}
}
