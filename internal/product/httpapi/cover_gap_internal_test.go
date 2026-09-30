package httpapi

// Task 17B coverage-gap tests: unexported helpers (in-package).

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
)

func TestCoverGapUnexportedHelpers(t *testing.T) {
	if itoa(0) != "0" || itoa(7) != "7" || itoa(-42) != "-42" || itoa(500) != "500" {
		t.Fatalf("itoa branches")
	}
	if requestID(nil) != "" {
		t.Fatalf("requestID(nil)")
	}
	r1 := httptest.NewRequest("GET", "/x", nil)
	r1.Header.Set("X-Request-ID", "a")
	if requestID(r1) != "a" {
		t.Fatalf("requestID X-Request-ID")
	}
	r2 := httptest.NewRequest("GET", "/x", nil)
	r2.Header.Set("X-Request-Id", "b")
	if requestID(r2) != "b" {
		t.Fatalf("requestID X-Request-Id")
	}
	r3 := httptest.NewRequest("GET", "/x", nil)
	if requestID(r3) != "" {
		t.Fatalf("requestID absent")
	}
	if rawHasNullData(map[string]json.RawMessage{}) {
		t.Fatalf("rawHasNullData absent")
	}
	if !rawHasNullData(map[string]json.RawMessage{"data": json.RawMessage("null")}) {
		t.Fatalf("rawHasNullData null")
	}
	if !rawHasNullData(map[string]json.RawMessage{"data": json.RawMessage("  null  ")}) {
		t.Fatalf("rawHasNullData padded null")
	}
	if rawHasNullData(map[string]json.RawMessage{"data": json.RawMessage(`{"a":1}`)}) {
		t.Fatalf("rawHasNullData object")
	}
	vr := httptest.NewRequest("GET", "/x", nil)
	vr = mux.SetURLVars(vr, map[string]string{"slug": "s"})
	if vars(vr, "slug") != "s" || vars(vr, "missing") != "" {
		t.Fatalf("vars branches")
	}
	vr2 := httptest.NewRequest("GET", "/x", nil)
	if vars(vr2, "slug") != "" {
		t.Fatalf("vars no mux")
	}
}
