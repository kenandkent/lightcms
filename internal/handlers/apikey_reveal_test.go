package handlers

// Admin API-key copy/reveal flow: owner-or-admin permission gate, legacy
// keys fail closed with recreate guidance, and the list page renders an
// enabled copy button only for copyable keys.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/services"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

const revealTestSecret = "test-secret-min-32-chars-abcdef"

// rbacUserID mirrors rbacSessionReq's deterministic session identity.
func rbacUserID(role string) string {
	return "0000000000000000000000" + role[:1] + "1"
}

func TestRevealAPIKeyFlows(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()
	h.SetAPIKeyEncryptionKey(services.DeriveKeyEncryptionKey(revealTestSecret))
	ctx := context.Background()

	ownerID, _ := primitive.ObjectIDFromHex(rbacUserID("editor"))
	raw, wired, err := h.apiKeyService.CreateAPIKeyForUser(ctx, "owner-key", "d", &ownerID)
	if err != nil {
		t.Fatalf("create owner key: %v", err)
	}
	legacyID := primitive.NewObjectID()
	if _, err := h.db.Collection("api_keys").InsertOne(ctx, bson.M{
		"_id": legacyID, "name": "legacy-key", "prefix": "lc_legacy00",
		"key_hash": "deadbeef", "user_id": ownerID, "created_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed legacy key: %v", err)
	}

	decodeKey := func(rr *httptest.ResponseRecorder) string {
		t.Helper()
		var body map[string]string
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v (%s)", err, rr.Body.String())
		}
		return body["key"]
	}

	// Owner (non-admin editor) reveals the exact raw value.
	rr := rbacPost(t, h.RevealAPIKey, nil, map[string]string{"id": wired.ID.Hex()}, "editor")
	if rr.Code != http.StatusOK {
		t.Fatalf("owner reveal: got %d (%s)", rr.Code, rr.Body.String())
	}
	if got := decodeKey(rr); got != raw {
		t.Fatal("owner reveal returned wrong key material")
	}

	// Admin (non-owner) reveals.
	rr = rbacPost(t, h.RevealAPIKey, nil, map[string]string{"id": wired.ID.Hex()}, "admin")
	if rr.Code != http.StatusOK || decodeKey(rr) != raw {
		t.Fatalf("admin reveal: got %d (%s)", rr.Code, rr.Body.String())
	}

	// Stranger (viewer, different identity) is forbidden.
	rr = rbacPost(t, h.RevealAPIKey, nil, map[string]string{"id": wired.ID.Hex()}, "viewer")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("stranger reveal: got %d, want 403", rr.Code)
	}

	// Legacy key: 410 Gone with recreate guidance (owner or admin alike).
	rr = rbacPost(t, h.RevealAPIKey, nil, map[string]string{"id": legacyID.Hex()}, "editor")
	if rr.Code != http.StatusGone {
		t.Fatalf("legacy reveal: got %d, want 410", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "recreate") {
		t.Fatalf("legacy reveal must advise recreation: %s", rr.Body.String())
	}

	// Missing key: 404. Bad hex: 400.
	rr = rbacPost(t, h.RevealAPIKey, nil,
		map[string]string{"id": primitive.NewObjectID().Hex()}, "admin")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("missing reveal: got %d, want 404", rr.Code)
	}
	rr = rbacPost(t, h.RevealAPIKey, nil, map[string]string{"id": "zzz"}, "admin")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad hex reveal: got %d, want 400", rr.Code)
	}
}

func TestAPIKeysPageCopyButtons(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()
	h.SetAPIKeyEncryptionKey(services.DeriveKeyEncryptionKey(revealTestSecret))
	ctx := context.Background()

	ownerID, _ := primitive.ObjectIDFromHex(rbacUserID("admin"))
	if _, _, err := h.apiKeyService.CreateAPIKeyForUser(ctx, "wired-key", "d", &ownerID); err != nil {
		t.Fatalf("create wired key: %v", err)
	}
	if _, err := h.db.Collection("api_keys").InsertOne(ctx, bson.M{
		"_id": primitive.NewObjectID(), "name": "legacy-key", "prefix": "lc_legacy00",
		"key_hash": "deadbeef", "user_id": ownerID, "created_at": time.Now(),
	}); err != nil {
		t.Fatalf("seed legacy key: %v", err)
	}

	req := rbacSessionReq(t, "admin", http.MethodGet, "/cm/api-keys", nil, nil)
	rr := httptest.NewRecorder()
	h.APIKeysPage(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("keys page: got %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "copyApiKey(") {
		t.Fatal("keys page must wire the copy button handler")
	}
	if !strings.Contains(body, "function copyApiKey(id, btn)") {
		t.Fatal("keys page must define copyApiKey (not just call it)")
	}
	// Exactly one enabled copy button (wired) + one disabled (legacy).
	if n := strings.Count(body, "disabled title="); n != 1 {
		t.Fatalf("disabled legacy copy buttons = %d, want 1", n)
	}
}
