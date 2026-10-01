package handlers

// Lane 2C fix 2 (red): APICreateComment must return 404 for comments on
// missing content (no orphans) and must surface provenance stamped from
// the request context on the created comment.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
	"github.com/jonradoff/lightcms/v7/internal/services"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestAPICreateComment_Orphan404(t *testing.T) {
	ah, _, cleanup := newTestAPIHandler(t)
	defer cleanup()

	ghost := primitive.NewObjectID().Hex()
	rr := doJSON(t, ah.APICreateComment, http.MethodPost,
		map[string]interface{}{"text": "orphan"}, map[string]string{"id": ghost})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("APICreateComment orphan: got %d (%s), want 404", rr.Code, rr.Body.String())
	}
}

func TestAPICreateComment_StampsProvenance(t *testing.T) {
	ah, db, cleanup := newTestAPIHandler(t)
	defer cleanup()

	tmpl := seedTemplate(t, db, "Page", "page")
	contentID := seedContent(t, db, tmpl, "Doc", "doc", "/doc")

	body, _ := json.Marshal(map[string]interface{}{"text": "agent note"})
	req := authReq(http.MethodPost, "/", bytes.NewReader(body))
	req = mux.SetURLVars(req, map[string]string{"id": contentID.Hex()})
	// Simulate the /api/v1 middleware in main.go: agent session header →
	// agent provenance on the request context.
	ctx := services.WithProvenance(req.Context(), services.Provenance{
		Actor: "agent", Via: "api", AgentSession: "agent-sess-9",
	})
	ctx = services.WithEditorEmail(ctx, "bot@x.com")
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	ah.APICreateComment(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("APICreateComment: got %d (%s), want 201", rr.Code, rr.Body.String())
	}
	var created map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if created["actor"] != "agent" {
		t.Errorf("actor = %v, want agent", created["actor"])
	}
	if created["agent_session"] != "agent-sess-9" {
		t.Errorf("agent_session = %v, want agent-sess-9", created["agent_session"])
	}
}
