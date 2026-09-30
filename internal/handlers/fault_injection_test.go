package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// TestFaultInjection_APIHandlers exercises handler write-error branches: the
// handler reads an entity successfully, then the write fails. Seed with the
// hook cleared, then inject a failure for the relevant operation.
func TestFaultInjection_APIHandlers(t *testing.T) {
	ah, db := newFaultAPIHandler(t)
	tmpl := seedTemplate(t, db, "Page", "page")
	cid := seedContent(t, db, tmpl, "Doc", "doc", "/doc").Hex()
	idv := map[string]string{"id": cid}

	expectErr := func(name string, h http.HandlerFunc, method string, body interface{}, vars map[string]string) {
		rr := doJSON(t, h, method, body, vars)
		if rr.Code < 400 {
			t.Errorf("%s: expected error status when write fails, got %d", name, rr.Code)
		}
	}

	// Read ok, UpdateOne fails.
	db.SetFaultHook(testutil.FailOp("UpdateOne"))
	expectErr("APIUpdateContent", ah.APIUpdateContent, http.MethodPut, map[string]interface{}{"title": "New"}, idv)
	expectErr("APIPublishContent", ah.APIPublishContent, http.MethodPost, nil, idv)
	expectErr("APIDeleteContent", ah.APIDeleteContent, http.MethodDelete, nil, idv)

	// Template read ok, content InsertOne fails.
	db.SetFaultHook(testutil.FailOp("InsertOne"))
	expectErr("APICreateContent", ah.APICreateContent, http.MethodPost,
		map[string]interface{}{"template_id": tmpl.Hex(), "title": "T"}, nil)

	db.SetFaultHook(nil)
}

// TestFaultInjection_LegacyRoutes pins write-error branches of the Task 16
// legacy entry points (by-path update, unpublish, restore, version revert)
// plus the /api/v1/regenerate contract verdict: RegenerateAllContent is an
// unconditional nil no-op (Task 16B), so the route answers 200 success while
// changing nothing. Task 17 records the 410-vs-upgrade-redirect decision;
// this test pins current behavior so the decision cannot land silently.
func TestFaultInjection_LegacyRoutes(t *testing.T) {
	ah, db := newFaultAPIHandler(t)
	tmpl := seedTemplate(t, db, "Page", "page")
	cid := seedContent(t, db, tmpl, "Doc", "doc", "/doc").Hex()
	seedContentVersion(t, db, mustOID(t, cid), tmpl)
	idv := map[string]string{"id": cid}

	expectErr := func(name string, h http.HandlerFunc, method string, body interface{}, vars map[string]string, rawQuery string) {
		rr := doJSONQuery(t, h, method, body, vars, rawQuery)
		if rr.Code < 400 {
			t.Errorf("%s: expected error status when write fails, got %d", name, rr.Code)
		}
	}

	// Read ok, UpdateOne fails: by-path update, unpublish, restore.
	db.SetFaultHook(testutil.FailOp("UpdateOne"))
	expectErr("APIUpdateContentByPath", ah.APIUpdateContentByPath, http.MethodPut,
		map[string]interface{}{"title": "New"}, nil, "path=/doc")
	expectErr("APIUnpublishContent", ah.APIUnpublishContent, http.MethodPost, nil, idv, "")
	expectErr("APIRestoreContent", ah.APIRestoreContent, http.MethodPost, nil, idv, "")

	// Version read ok, version InsertOne fails on revert.
	db.SetFaultHook(testutil.FailOp("InsertOne"))
	expectErr("APIRevertContentVersion", ah.APIRevertContentVersion, http.MethodPost, nil,
		map[string]string{"id": cid, "version": "1"}, "")

	db.SetFaultHook(nil)

	// Regenerate verdict: 200 success message with zero side effects.
	rr := doJSON(t, ah.APIRegenerateAllContent, http.MethodPost, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("APIRegenerateAllContent: got %d (%s), want 200 (no-op verdict)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "regenerated") {
		t.Fatalf("APIRegenerateAllContent message changed (verdict premise): %s", rr.Body.String())
	}
}

// doJSONQuery is doJSON with an optional raw query string (for ?path= legacy
// routes).
func doJSONQuery(t *testing.T, h http.HandlerFunc, method string, body interface{}, vars map[string]string, rawQuery string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := authReq(method, "/", rdr)
	if rawQuery != "" {
		req.URL.RawQuery = rawQuery
	}
	if vars != nil {
		req = mux.SetURLVars(req, vars)
	}
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

func mustOID(t *testing.T, hex string) primitive.ObjectID {
	t.Helper()
	id, err := primitive.ObjectIDFromHex(hex)
	if err != nil {
		t.Fatalf("bad oid: %v", err)
	}
	return id
}
