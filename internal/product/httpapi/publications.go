package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/observe"
	"github.com/jonradoff/lightcms/v7/internal/product/generation"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// HandleListPublications is GET /api/v1/content/{id}/publications.
func (h *Handlers) HandleListPublications(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actorOf(w, r)
	if !ok {
		return
	}
	if !actor.Can(generation.ScopeContentView) {
		WriteError(w, r, &generation.Error{Code: generation.CodePermissionDenied, Message: "missing required scope content.view"})
		return
	}
	idHex := vars(r, "id")
	cid, err := primitive.ObjectIDFromHex(idHex)
	if err != nil {
		WriteError(w, r, &generation.Error{Code: generation.CodeInvalidRequest, Message: "invalid content id"})
		return
	}
	list, err := h.Gen.ListPublications(r.Context(), cid)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"content_id": cid.Hex(), "publications": list})
}

// HandleGetPublication is GET /api/v1/content/{id}/publications/{publication_id}.
func (h *Handlers) HandleGetPublication(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actorOf(w, r)
	if !ok {
		return
	}
	if !actor.Can(generation.ScopeContentView) {
		WriteError(w, r, &generation.Error{Code: generation.CodePermissionDenied, Message: "missing required scope content.view"})
		return
	}
	idHex := vars(r, "id")
	pidHex := vars(r, "publication_id")
	if pidHex == "" {
		pidHex = vars(r, "publicationId")
	}
	cid, err := primitive.ObjectIDFromHex(idHex)
	if err != nil {
		WriteError(w, r, &generation.Error{Code: generation.CodeInvalidRequest, Message: "invalid content id"})
		return
	}
	pid, err := primitive.ObjectIDFromHex(pidHex)
	if err != nil {
		WriteError(w, r, &generation.Error{Code: generation.CodeInvalidRequest, Message: "invalid publication id"})
		return
	}
	pub, err := h.Gen.GetPublication(r.Context(), cid, pid)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, pub)
}

// HandleRollback is POST /api/v1/content/{id}/publications/{publication_id}/rollback.
func (h *Handlers) HandleRollback(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actorOf(w, r)
	if !ok {
		return
	}
	idHex := vars(r, "id")
	pidHex := vars(r, "publication_id")
	if pidHex == "" {
		pidHex = vars(r, "publicationId")
	}
	cid, err := primitive.ObjectIDFromHex(idHex)
	if err != nil {
		WriteError(w, r, &generation.Error{Code: generation.CodeInvalidRequest, Message: "invalid content id"})
		return
	}
	pid, err := primitive.ObjectIDFromHex(pidHex)
	if err != nil {
		WriteError(w, r, &generation.Error{Code: generation.CodeInvalidRequest, Message: "invalid publication id"})
		return
	}
	var body struct {
		ExpectedActiveID *string `json:"expected_active_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	var expected *primitive.ObjectID
	if body.ExpectedActiveID != nil && strings.TrimSpace(*body.ExpectedActiveID) != "" {
		eid, err := primitive.ObjectIDFromHex(strings.TrimSpace(*body.ExpectedActiveID))
		if err != nil {
			WriteError(w, r, &generation.Error{Code: generation.CodeInvalidRequest, Message: "invalid expected_active_id"})
			return
		}
		expected = &eid
	}
	ctx := generation.WithActor(r.Context(), actor)
	if key, has := h.idemKey(r); has {
		ctx = generation.WithIdempotency(ctx, generation.IdempotencyParams{
			Owner: actor.Owner(), Method: r.Method, Path: r.URL.Path, Key: key, Body: nil,
		})
	}
	// Task 16F: structured rollback request log with duration.
	t0 := time.Now()
	res, err := h.Gen.RollbackPublication(ctx, actor, cid, pid, expected)
	f := observe.Fields{
		RequestID: requestID(r), Actor: actor.ActorKind,
		UserID: actor.Owner(), AgentSession: actor.AgentSession,
		ContentID: cid.Hex(), PublicationID: pid.Hex(),
		Stage: "rollback", DurationMS: time.Since(t0).Milliseconds(),
	}
	if err != nil {
		f.ErrorCode = generation.CodeOf(err)
		f.StatusCode = generation.StatusForCode(f.ErrorCode)
		observe.LogPublication("rollback_failed", f)
		WriteError(w, r, err)
		return
	}
	f.StatusCode = 200
	observe.LogPublication("rollback", f)
	WriteJSON(w, http.StatusOK, res)
}

// HandleRestoreAndPublish is POST /api/v1/content/{id}/restore-and-publish.
func (h *Handlers) HandleRestoreAndPublish(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actorOf(w, r)
	if !ok {
		return
	}
	idHex := vars(r, "id")
	cid, err := primitive.ObjectIDFromHex(idHex)
	if err != nil {
		WriteError(w, r, &generation.Error{Code: generation.CodeInvalidRequest, Message: "invalid content id"})
		return
	}
	var body struct {
		Version          *int64  `json:"version"`
		ExpectedActiveID *string `json:"expected_active_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		WriteError(w, r, &generation.Error{Code: generation.CodeInvalidRequest, Message: "invalid JSON body"})
		return
	}
	if body.Version == nil {
		WriteError(w, r, &generation.Error{Code: generation.CodeInvalidRequest, Message: "version is required"})
		return
	}
	var expected *primitive.ObjectID
	if body.ExpectedActiveID != nil && strings.TrimSpace(*body.ExpectedActiveID) != "" {
		eid, err := primitive.ObjectIDFromHex(strings.TrimSpace(*body.ExpectedActiveID))
		if err != nil {
			WriteError(w, r, &generation.Error{Code: generation.CodeInvalidRequest, Message: "invalid expected_active_id"})
			return
		}
		expected = &eid
	}
	ctx := generation.WithActor(r.Context(), actor)
	if key, has := h.idemKey(r); has {
		ctx = generation.WithIdempotency(ctx, generation.IdempotencyParams{
			Owner: actor.Owner(), Method: r.Method, Path: r.URL.Path, Key: key, Body: nil,
		})
	}
	res, err := h.Gen.RestoreAndPublish(ctx, actor, cid, *body.Version, expected)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, res)
}

// HandleRevertLive is POST /api/v1/content/{id}/revert-live.
func (h *Handlers) HandleRevertLive(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actorOf(w, r)
	if !ok {
		return
	}
	idHex := vars(r, "id")
	cid, err := primitive.ObjectIDFromHex(idHex)
	if err != nil {
		WriteError(w, r, &generation.Error{Code: generation.CodeInvalidRequest, Message: "invalid content id"})
		return
	}
	var body struct {
		SourcePublicationID *string `json:"source_publication_id"`
		ExpectedActiveID    *string `json:"expected_active_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		WriteError(w, r, &generation.Error{Code: generation.CodeInvalidRequest, Message: "invalid JSON body"})
		return
	}
	if body.SourcePublicationID == nil || strings.TrimSpace(*body.SourcePublicationID) == "" {
		WriteError(w, r, &generation.Error{Code: generation.CodeInvalidRequest, Message: "source_publication_id is required"})
		return
	}
	sid, err := primitive.ObjectIDFromHex(strings.TrimSpace(*body.SourcePublicationID))
	if err != nil {
		WriteError(w, r, &generation.Error{Code: generation.CodeInvalidRequest, Message: "invalid source_publication_id"})
		return
	}
	var expected *primitive.ObjectID
	if body.ExpectedActiveID != nil && strings.TrimSpace(*body.ExpectedActiveID) != "" {
		eid, err := primitive.ObjectIDFromHex(strings.TrimSpace(*body.ExpectedActiveID))
		if err != nil {
			WriteError(w, r, &generation.Error{Code: generation.CodeInvalidRequest, Message: "invalid expected_active_id"})
			return
		}
		expected = &eid
	}
	ctx := generation.WithActor(r.Context(), actor)
	if key, has := h.idemKey(r); has {
		ctx = generation.WithIdempotency(ctx, generation.IdempotencyParams{
			Owner: actor.Owner(), Method: r.Method, Path: r.URL.Path, Key: key, Body: nil,
		})
	}
	res, err := h.Gen.RevertLive(ctx, actor, cid, sid, expected)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, res)
}

func (h *Handlers) idemKey(r *http.Request) (string, bool) {
	if h.IdempotencyExtractor != nil {
		return h.IdempotencyExtractor(r)
	}
	if v := strings.TrimSpace(r.Header.Get("Idempotency-Key")); v != "" {
		return v, true
	}
	return "", false
}
