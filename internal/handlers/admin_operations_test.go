package handlers

import (
	"encoding/json"
	"github.com/metavert-io/lightcms/v7/internal/testutil"
	"net/http/httptest"
	"testing"
)

func TestLivenessDoesNotDependOnMongoOrCloudflare(t *testing.T) {
	h := &Handler{}
	w := httptest.NewRecorder()
	h.Livez(w, httptest.NewRequest("GET", "/livez", nil))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	w = httptest.NewRecorder()
	h.Readyz(w, httptest.NewRequest("GET", "/readyz", nil))
	if w.Code != 503 {
		t.Fatal("missing core database must not be ready")
	}
	w = httptest.NewRecorder()
	h.AdminOperations(w, httptest.NewRequest("GET", "/cm/operations", nil))
	if w.Code != 403 {
		t.Fatal("operations requires admin")
	}
}

func TestOptionalDeliveryFailureDegradesHealthWithoutFailingReadiness(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	h := &Handler{db: db, staticDelivery: &StaticDeliveryAdmin{Unavailable: "private diagnostic"}}
	w := httptest.NewRecorder()
	h.Healthz(w, httptest.NewRequest("GET", "/healthz", nil))
	var result healthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || result.Status != healthStatusDegraded {
		t.Fatalf("optional failure must degrade, not fail core: %d %+v", w.Code, result)
	}
	w = httptest.NewRecorder()
	h.Readyz(w, httptest.NewRequest("GET", "/readyz", nil))
	if w.Code != 200 {
		t.Fatal("optional failure blocked readiness")
	}
}
