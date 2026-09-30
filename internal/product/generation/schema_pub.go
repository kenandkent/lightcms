package generation

import (
	"context"
	"fmt"

	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"

	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// SchemaForSlug resolves the immutable TemplateVersion for slug and renders
// its deterministic JSON Schema. The returned version and bytes come from the
// SAME object (§10.1) — callers must use them together for ETag + body.
func (s *Service) SchemaForSlug(ctx context.Context, slug string) (templatecontract.TemplateVersion, []byte, error) {
	tv, err := s.templates.GetCurrent(ctx, slug)
	if err != nil {
		if templatecontract.CodeOf(err) == templatecontract.CodeNotFound {
			return templatecontract.TemplateVersion{}, nil, genErr(CodeTemplateNotFound, fmt.Sprintf("template %q not found", slug), err)
		}
		return templatecontract.TemplateVersion{}, nil, genErr(CodeInternal, "resolve template", err)
	}
	raw, err := templatecontract.ToJSONSchema(tv)
	if err != nil {
		return templatecontract.TemplateVersion{}, nil, genErr(CodeTemplateSchemaInvalid, "cannot render template schema", err)
	}
	return tv, raw, nil
}

// ListPublications returns every publication for content, newest first.
func (s *Service) ListPublications(ctx context.Context, contentID primitive.ObjectID) ([]publication.Publication, error) {
	if s.pubRepo == nil {
		return nil, genErr(CodeInternal, "publication repository is not wired", nil)
	}
	list, err := s.pubRepo.ListHistory(ctx, contentID)
	if err != nil {
		return nil, genErr(CodeInternal, "list publications", err)
	}
	return list, nil
}

// GetPublication loads one publication, scoping it to contentID.
func (s *Service) GetPublication(ctx context.Context, contentID, publicationID primitive.ObjectID) (publication.Publication, error) {
	if s.pubRepo == nil {
		return publication.Publication{}, genErr(CodeInternal, "publication repository is not wired", nil)
	}
	p, err := s.pubRepo.GetByID(ctx, publicationID)
	if err != nil {
		if publication.CodeOf(err) == publication.CodeNotFound || err == mongo.ErrNoDocuments {
			return publication.Publication{}, genErr(CodePublicationNotFound, "publication not found", err)
		}
		return publication.Publication{}, genErr(CodeInternal, "load publication", err)
	}
	if p.ContentID != contentID {
		return publication.Publication{}, genErr(CodePublicationNotFound, "publication does not belong to this content", nil)
	}
	return *p, nil
}

// RollbackPublicationResult is the HTTP shape for exact rollback.
type RollbackPublicationResult struct {
	ContentID      string `json:"content_id"`
	ContentVersion int64  `json:"content_version"`
	PublicationID  string `json:"publication_id"`
	FullPath       string `json:"full_path"`
	PublicURL      string `json:"public_url"`
	Mode           string `json:"mode"`
}

// RollbackPublication mints a NEW publication from the source's retained
// bytes (exact) or the weaker re-render past retention. It requires
// content.edit + content.publish and an Idempotency-Key when externally
// invoked (428 with zero mutation when absent).
func (s *Service) RollbackPublication(ctx context.Context, actor Actor, contentID, sourceID primitive.ObjectID, expectedActiveID *primitive.ObjectID) (RollbackPublicationResult, error) {
	var zero RollbackPublicationResult
	if !actor.Authenticated {
		return zero, genErr(CodeUnauthenticated, "authentication is required", nil)
	}
	if !actor.HasScope(ScopeContentEdit) || !actor.HasScope(ScopeContentPublish) {
		return zero, genErr(CodePermissionDenied, "rollback requires content.edit + content.publish", nil)
	}
	ifem, hasIdem := IdempotencyFrom(ctx)
	if !hasIdem {
		return zero, genErr(CodeIdempotencyKeyRequired, "rollback requires Idempotency-Key", nil)
	}
	if s.idem == nil || s.pubs == nil {
		return zero, genErr(CodeInternal, "services are not wired", nil)
	}
	owner := ifem.Owner
	if owner == "" {
		owner = actor.Owner()
	}
	method := ifem.Method
	if method == "" {
		method = "POST"
	}
	path := ifem.Path
	if path == "" {
		path = "/api/v1/content/" + contentID.Hex() + "/publications/" + sourceID.Hex() + "/rollback"
	}
	body := ifem.Body
	if len(body) == 0 {
		body = []byte(`{"source":"` + sourceID.Hex() + `"}`)
	}
	op, err := s.idem.Begin(ctx, owner, method, path, ifem.Key, body)
	if err != nil {
		return zero, mapIdemBeginErr(err)
	}
	if op.Replay {
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
		return RollbackPublicationResult{
			ContentID: str("content_id"), ContentVersion: cv,
			PublicationID: str("publication_id"), FullPath: str("full_path"),
			PublicURL: str("public_url"), Mode: "rollback",
		}, nil
	}
	res, err := s.pubs.Rollback(ctx, publication.RollbackRequest{
		ContentID: contentID, SourcePublicationID: sourceID,
		ExpectedActiveID: expectedActiveID, IdempotencyRecord: &op.ID,
	})
	if err != nil {
		s.completePublishError(ctx, op, err)
		return zero, mapSagaErr(err)
	}
	out := RollbackPublicationResult{
		ContentID: contentID.Hex(), ContentVersion: res.ContentVersion,
		PublicationID: res.PublicationID.Hex(), FullPath: res.FullPath,
		PublicURL: res.PublicURL, Mode: "rollback",
	}
	_, _ = s.idem.Complete(ctx, op.ID, op.Attempt, 200, map[string]any{
		"content_id": out.ContentID, "content_version": float64(out.ContentVersion),
		"publication_id": out.PublicationID, "full_path": out.FullPath,
		"public_url": out.PublicURL, "mode": out.Mode,
	}, false)
	s.auditf(ctx, "publication.rollback", map[string]any{
		"content_id": contentID.Hex(), "source_publication_id": sourceID.Hex(),
		"publication_id": out.PublicationID,
	})
	return out, nil
}
