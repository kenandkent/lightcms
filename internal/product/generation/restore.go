// restore_and_publish vs revert_live (§13, §18.4–§18.5).
//
//   - RestoreAndPublish: historical ContentVersion → new Main draft version →
//     new Publication (re-render through the current pipeline). Source is a
//     version number; the new bytes come from re-rendering retained data.
//   - RevertLive (exact rollback): historical Publication's retained immutable
//     bytes → new Publication (byte-identical within the Exact SLA). Source is
//     a publication ID; delegates to PublicationService.Rollback.
//
// Both mint a NEW publication ID, both require content.edit + content.publish,
// both honor ExpectedActiveID preconditions, and both require Idempotency-Key
// when externally invoked (428 with zero mutation when absent).
package generation

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/idempotency"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// RestoreAndPublishResult is the new-publication outcome.
type RestoreAndPublishResult struct {
	ContentID      string `json:"content_id"`
	ContentVersion int64  `json:"content_version"`
	PublicationID  string `json:"publication_id"`
	FullPath       string `json:"full_path"`
	PublicURL      string `json:"public_url"`
	Mode           string `json:"mode"` // always "restore_and_publish"
}

// RestoreAndPublish restores historical version data as a new draft version
// and publishes it. It never mutates the historical version row.
func (s *Service) RestoreAndPublish(ctx context.Context, actor Actor, contentID primitive.ObjectID, version int64, expectedActiveID *primitive.ObjectID) (RestoreAndPublishResult, error) {
	var zero RestoreAndPublishResult
	if !actor.Authenticated {
		return zero, genErr(CodeUnauthenticated, "authentication is required", nil)
	}
	if !actor.Can(ScopeContentEdit) || !actor.Can(ScopeContentPublish) {
		return zero, genErr(CodePermissionDenied, "restore_and_publish requires content.edit + content.publish", nil)
	}
	ifem, hasIdem := IdempotencyFrom(ctx)
	if !hasIdem {
		return zero, genErr(CodeIdempotencyKeyRequired, "restore_and_publish requires Idempotency-Key", nil)
	}
	if s.idem == nil || s.pubs == nil {
		return zero, genErr(CodeInternal, "services are not wired", nil)
	}
	// Load the historical version (precondition, read-only).
	var ver models.ContentVersion
	if err := s.db.FindOne(ctx, "content_versions",
		bson.M{"content_id": contentID, "version": version}, &ver); err != nil {
		if err == mongo.ErrNoDocuments {
			return zero, genErr(CodeContentNotFound, fmt.Sprintf("content version %d not found", version), err)
		}
		return zero, genErr(CodeInternal, "load content version", err)
	}
	// Load live for CAS + active check (read-only).
	var live models.Content
	if err := s.db.FindOne(ctx, "content", bson.M{"_id": contentID}, &live); err != nil {
		if err == mongo.ErrNoDocuments {
			return zero, genErr(CodeContentNotFound, "content not found", err)
		}
		return zero, genErr(CodeInternal, "load content", err)
	}
	active, err := s.pubRepo.GetActive(ctx, contentID)
	if err != nil {
		return zero, genErr(CodeInternal, "read active publication", err)
	}
	if expectedActiveID != nil {
		if active == nil || active.ID != *expectedActiveID {
			return zero, genErr(CodePublicationConflict, "stale expected active publication", nil)
		}
	}
	owner := ifem.Owner
	if owner == "" {
		owner = actor.Owner()
	}
	body, _ := json.Marshal(map[string]any{
		"op": "restore_and_publish", "content_id": contentID.Hex(), "version": version,
	})
	method := ifem.Method
	if method == "" {
		method = "POST"
	}
	path := ifem.Path
	if path == "" {
		path = "/api/v1/content/" + contentID.Hex() + "/restore-and-publish"
	}
	op, err := s.beginOrResume(ctx, owner, method, path, ifem.Key, body)
	if err != nil {
		return zero, mapIdemBeginErr(err)
	}
	if op.Replay {
		return restoreReplay(op)
	}
	// New Main draft version from the historical snapshot + Bind, one txn.
	next := live.CurrentVersion + 1
	if next < 1 {
		next = 1
	}
	newVer := &models.ContentVersion{
		ContentID: contentID, Version: next,
		TemplateID: ver.TemplateID, TemplateName: ver.TemplateName,
		Title: ver.Title, Slug: ver.Slug, FolderPath: ver.FolderPath, FullPath: ver.FullPath,
		Data: ver.Data, Actor: actorKind(actor), Via: actor.Via, AgentSession: actor.AgentSession,
		ModifiedByEmail: actor.Email, Comment: fmt.Sprintf("restore_and_publish from version %d", version),
		CreatedAt: s.now().UTC(),
	}
	canonical := live.CanonicalFullPath
	if canonical == "" {
		canonical = live.FullPath
	}
	err = s.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		res, err := s.db.Collection("content").UpdateOne(sc,
			bson.M{"_id": contentID, "current_version": live.CurrentVersion},
			bson.M{"$set": bson.M{
				"template_id": ver.TemplateID, "template_name": ver.TemplateName,
				"title": ver.Title, "slug": ver.Slug, "folder_path": ver.FolderPath,
				"data": ver.Data, "current_version": next, "updated_at": s.now().UTC(),
			}})
		if err != nil {
			return err
		}
		if res.MatchedCount == 0 {
			return genErr(CodeContentVersionConflict, "content changed concurrently", nil)
		}
		if _, err := s.db.Collection("content_versions").InsertOne(sc, newVer); err != nil {
			return mapDupKey(err, CodeContentVersionConflict, "content version conflict")
		}
		if berr := s.idem.BindContentAndVersion(ctx, sc, op.ID, contentID, next, canonical); berr != nil {
			return rwIdemErr(berr)
		}
		return nil
	})
	if err != nil {
		s.completePublishError(ctx, op, err)
		return zero, err
	}
	// Publish the restored draft. Template version: the live row's template
	// current version is resolved by the saga when TemplateVersionID is zero.
	res, err := s.pubs.Publish(ctx, publication.PublishRequest{
		ContentID: contentID, ContentVersion: next,
		ExpectedActiveID: expectedActiveID, Reason: "restore_and_publish",
		IdempotencyRecord: &op.ID,
		// Lane 2B: thread caller attribution into the minted record.
		Actor: actorKind(actor), Via: actor.Via, AgentSession: actor.AgentSession,
		AuthorIsAdmin: actor.IsAdmin,
	})
	if err != nil {
		s.completePublishError(ctx, op, err)
		return zero, mapSagaErr(err)
	}
	out := RestoreAndPublishResult{
		ContentID: contentID.Hex(), ContentVersion: next,
		PublicationID: res.PublicationID.Hex(), FullPath: res.FullPath,
		PublicURL: res.PublicURL, Mode: "restore_and_publish",
	}
	_, _ = s.idem.Complete(ctx, op.ID, op.Attempt, 200, map[string]any{
		"content_id": out.ContentID, "content_version": float64(next),
		"publication_id": out.PublicationID, "full_path": out.FullPath,
		"public_url": out.PublicURL, "mode": out.Mode,
	}, false)
	s.auditf(ctx, "publication.restore_and_publish", map[string]any{
		"content_id": contentID.Hex(), "from_version": version, "content_version": next,
		"publication_id": out.PublicationID,
	})
	return out, nil
}

// RevertLiveResult is the exact-rollback outcome.
type RevertLiveResult struct {
	ContentID      string `json:"content_id"`
	ContentVersion int64  `json:"content_version"`
	PublicationID  string `json:"publication_id"`
	FullPath       string `json:"full_path"`
	PublicURL      string `json:"public_url"`
	Mode           string `json:"mode"` // always "revert_live"
}

// RevertLive performs the exact rollback: a NEW publication restoring the
// source publication's retained immutable bytes (or the weaker re-render past
// retention — decided by the saga). Distinct from RestoreAndPublish, which
// re-renders from a ContentVersion snapshot.
func (s *Service) RevertLive(ctx context.Context, actor Actor, contentID, sourcePublicationID primitive.ObjectID, expectedActiveID *primitive.ObjectID) (RevertLiveResult, error) {
	var zero RevertLiveResult
	if !actor.Authenticated {
		return zero, genErr(CodeUnauthenticated, "authentication is required", nil)
	}
	if !actor.Can(ScopeContentEdit) || !actor.Can(ScopeContentPublish) {
		return zero, genErr(CodePermissionDenied, "revert_live requires content.edit + content.publish", nil)
	}
	ifem, hasIdem := IdempotencyFrom(ctx)
	if !hasIdem {
		return zero, genErr(CodeIdempotencyKeyRequired, "revert_live requires Idempotency-Key", nil)
	}
	if s.idem == nil || s.pubs == nil {
		return zero, genErr(CodeInternal, "services are not wired", nil)
	}
	owner := ifem.Owner
	if owner == "" {
		owner = actor.Owner()
	}
	body, _ := json.Marshal(map[string]any{
		"op": "revert_live", "content_id": contentID.Hex(),
		"source_publication_id": sourcePublicationID.Hex(),
	})
	method := ifem.Method
	if method == "" {
		method = "POST"
	}
	path := ifem.Path
	if path == "" {
		path = "/api/v1/content/" + contentID.Hex() + "/revert-live"
	}
	op, err := s.beginOrResume(ctx, owner, method, path, ifem.Key, body)
	if err != nil {
		return zero, mapIdemBeginErr(err)
	}
	if op.Replay {
		return revertReplay(op)
	}
	res, err := s.pubs.Rollback(ctx, publication.RollbackRequest{
		ContentID: contentID, SourcePublicationID: sourcePublicationID,
		ExpectedActiveID: expectedActiveID, IdempotencyRecord: &op.ID,
		// Lane 2B: thread caller attribution into the minted record.
		Actor: actorKind(actor), Via: actor.Via, AgentSession: actor.AgentSession,
		AuthorIsAdmin: actor.IsAdmin,
	})
	if err != nil {
		s.completePublishError(ctx, op, err)
		return zero, mapSagaErr(err)
	}
	out := RevertLiveResult{
		ContentID: contentID.Hex(), ContentVersion: res.ContentVersion,
		PublicationID: res.PublicationID.Hex(), FullPath: res.FullPath,
		PublicURL: res.PublicURL, Mode: "revert_live",
	}
	_, _ = s.idem.Complete(ctx, op.ID, op.Attempt, 200, map[string]any{
		"content_id": out.ContentID, "content_version": float64(out.ContentVersion),
		"publication_id": out.PublicationID, "full_path": out.FullPath,
		"public_url": out.PublicURL, "mode": out.Mode,
	}, false)
	s.auditf(ctx, "publication.revert_live", map[string]any{
		"content_id": contentID.Hex(), "source_publication_id": sourcePublicationID.Hex(),
		"publication_id": out.PublicationID,
	})
	return out, nil
}

func restoreReplay(op idempotency.Operation) (RestoreAndPublishResult, error) {
	m := op.Response
	str := func(k string) string {
		if v, ok := m[k].(string); ok {
			return v
		}
		return ""
	}
	var cv int64
	switch v := m["content_version"].(type) {
	case float64:
		cv = int64(v)
	case int64:
		cv = v
	}
	return RestoreAndPublishResult{
		ContentID: str("content_id"), ContentVersion: cv,
		PublicationID: str("publication_id"), FullPath: str("full_path"),
		PublicURL: str("public_url"), Mode: "restore_and_publish",
	}, nil
}

func revertReplay(op idempotency.Operation) (RevertLiveResult, error) {
	m := op.Response
	str := func(k string) string {
		if v, ok := m[k].(string); ok {
			return v
		}
		return ""
	}
	var cv int64
	switch v := m["content_version"].(type) {
	case float64:
		cv = int64(v)
	case int64:
		cv = v
	}
	return RevertLiveResult{
		ContentID: str("content_id"), ContentVersion: cv,
		PublicationID: str("publication_id"), FullPath: str("full_path"),
		PublicURL: str("public_url"), Mode: "revert_live",
	}, nil
}
