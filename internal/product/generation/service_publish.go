package generation

import (
	"context"
	"fmt"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/idempotency"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/storage"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// publish implements mode=publish (§20.6): direct authorized command through
// PublicationService — replace Main Content + new Version + Publication, never
// a transient Fork. It owns Begin/Bind/Complete around the saga (Task 11
// handoff): callers must Complete (the saga never does), re-bind per attempt,
// never TakeOver a terminal attempt, and attest purity/secrets (Complete
// trusts the caller — this file only marks pure pre-mutation 422 as
// validationOnly and never stores secrets in the cached response).
func (s *Service) publish(ctx context.Context, actor Actor, tv templatecontract.TemplateVersion, live *models.Content, active *publication.Publication, published, targetExists bool, fullPath, slug, folderPath, title, canonical string, data map[string]any, warns []templatecontract.FieldWarning, req GenerateRequest, idemParams IdempotencyParams) (GenerateResponse, error) {
	op, berr := s.beginForPublish(ctx, actor, req, tv, idemParams)
	if berr != nil {
		var zero GenerateResponse
		return zero, berr
	}
	if op.Replay {
		return responseFromCache(op)
	}
	return s.publishWithOp(ctx, actor, tv, live, active, published, targetExists, fullPath, slug, folderPath, title, canonical, data, warns, req, idemParams, &op)
}

func (s *Service) publishWithOp(ctx context.Context, actor Actor, tv templatecontract.TemplateVersion, live *models.Content, active *publication.Publication, published, targetExists bool, fullPath, slug, folderPath, title, canonical string, data map[string]any, warns []templatecontract.FieldWarning, req GenerateRequest, idemParams IdempotencyParams, op *idempotency.Operation) (GenerateResponse, error) {
	var zero GenerateResponse
	if s.idem == nil {
		return zero, genErr(CodeInternal, "idempotency service is not wired", nil)
	}
	if s.pubs == nil {
		return zero, genErr(CodeInternal, "publication service is not wired", nil)
	}
	if op == nil {
		return zero, genErr(CodeInternal, "idempotency operation is required", nil)
	}
	// op is the early Begin from Generate (replay already handled there).

	// Content write + Bind in ONE transaction (§21.5). Abort leaves neither.
	var contentID primitive.ObjectID
	var contentVersion int64
	var isCreate bool
	if live == nil {
		isCreate = true
		contentID = primitive.NewObjectID()
		contentVersion = 1
		if err := s.createLiveForPublish(ctx, actor, tv, contentID, fullPath, slug, folderPath, title, canonical, data, op.ID); err != nil {
			s.completePublishError(ctx, *op, err)
			return zero, err
		}
	} else {
		isCreate = false
		next := live.CurrentVersion + 1
		if next < 1 {
			next = 1
		}
		// Full-replace may move the path (rename-and-publish): recompute the
		// canonical key and move it in the same transaction.
		newCanonical := live.CanonicalFullPath
		if fullPath != live.FullPath {
			newCanonical = lowerCanonical(fullPath)
		}
		contentID = live.ID
		contentVersion = next
		if err := s.replaceLiveForPublish(ctx, actor, tv, live, next, fullPath, slug, folderPath, title, newCanonical, data, op.ID); err != nil {
			s.completePublishError(ctx, *op, err)
			return zero, err
		}
	}

	// Saga publish with the frozen attempt (re-binds the same values —
	// idempotent — then freezes Publication ID + logical time before Render).
	var expectedActive *primitive.ObjectID
	if active != nil {
		id := active.ID
		expectedActive = &id
	}
	res, err := s.pubs.Publish(ctx, publication.PublishRequest{
		ContentID: contentID, ContentVersion: contentVersion,
		TemplateVersionID: tv.ID, ExpectedActiveID: expectedActive,
		Reason: "page_generation publish", IdempotencyRecord: &op.ID,
	})
	if err != nil {
		// Terminal pre-activation failures are already MarkedTerminal by the
		// saga; never Complete them as cacheable and never TakeOver here —
		// the next same-key Begin allocates a new attempt + Publication ID.
		// Non-terminal lease releases for retryable errors are handled by
		// completing as non-cacheable (releases the lease, never completes).
		s.completePublishError(ctx, *op, err)
		return zero, mapSagaErr(err)
	}

	action := "updated"
	status := 200
	if isCreate {
		action = "created"
		status = 201
	}
	pubHex := res.PublicationID.Hex()
	publicURL := res.PublicURL
	out := GenerateResponse{
		ID: contentID.Hex(), Action: action, Template: tv.Slug, FullPath: res.FullPath, Mode: ModePublish,
		Published: true, RequiresPublish: false,
		PublicURL: &publicURL, ContentVersion: res.ContentVersion, TemplateVersion: tv.Version,
		PublicationID: &pubHex, Warnings: warns,
	}
	// Cache the 2xx for replay (no secrets in the payload — enforced here).
	cache := map[string]any{
		"id": out.ID, "action": out.Action, "template": out.Template, "full_path": out.FullPath,
		"mode": out.Mode, "published": out.Published, "requires_publish": out.RequiresPublish,
		"public_url": publicURL, "content_version": float64(out.ContentVersion),
		"template_version": float64(out.TemplateVersion), "publication_id": pubHex,
	}
	if _, cerr := s.idem.Complete(ctx, op.ID, op.Attempt, status, cache, false); cerr != nil {
		// A completed publish that fails to cache is still a success — the
		// page is live. Surface the result; the retry will TakeOver/replay.
		s.auditf(ctx, "page_generation.idempotency_complete_failed", map[string]any{
			"content_id": contentID.Hex(), "error": cerr.Error(),
		})
	}
	s.auditf(ctx, "page_generation.publish", map[string]any{
		"template": tv.Slug, "template_version": tv.Version,
		"content_id": contentID.Hex(), "content_version": contentVersion,
		"publication_id": pubHex, "full_path": res.FullPath, "mode": ModePublish,
	})
	return out, nil
}

// createLiveForPublish inserts the live Main row + version 1 with Bind.
func (s *Service) createLiveForPublish(ctx context.Context, actor Actor, tv templatecontract.TemplateVersion, id primitive.ObjectID, fullPath, slug, folderPath, title, canonical string, data map[string]any, opID primitive.ObjectID) error {
	now := s.now().UTC()
	content := &models.Content{
		ID: id, TemplateID: tv.TemplateID, TemplateName: tv.Name,
		Title: title, Slug: slug, FolderPath: folderPath, FullPath: fullPath,
		CanonicalFullPath: canonical, PathScope: "live", PathActive: true,
		CurrentVersion: 1, Data: data,
		Published: false, CreatedAt: now, UpdatedAt: now,
	}
	ver := &models.ContentVersion{
		ContentID: id, Version: 1,
		TemplateID: tv.TemplateID, TemplateName: tv.Name,
		Title: title, Slug: slug, FolderPath: folderPath, FullPath: fullPath,
		Data: data, Actor: actorKind(actor), Via: actor.Via, AgentSession: actor.AgentSession,
		ModifiedByEmail: actor.Email, Comment: "page_generation publish create",
		CreatedAt: now,
	}
	err := s.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		if _, err := s.db.Collection("content").InsertOne(sc, content); err != nil {
			return mapDupKey(err, CodePathConflict, "page already exists at "+fullPath)
		}
		if _, err := s.db.Collection("content_versions").InsertOne(sc, ver); err != nil {
			return mapDupKey(err, CodeContentVersionConflict, "content version conflict")
		}
		if berr := s.idem.BindContentAndVersion(ctx, sc, opID, id, 1, canonical); berr != nil {
			return rwIdemErr(berr)
		}
		return nil
	})
	return err
}

// replaceLiveForPublish full-replaces the live Main row (CAS bump) with Bind.
func (s *Service) replaceLiveForPublish(ctx context.Context, actor Actor, tv templatecontract.TemplateVersion, live *models.Content, next int64, fullPath, slug, folderPath, title, newCanonical string, data map[string]any, opID primitive.ObjectID) error {
	now := s.now().UTC()
	ver := &models.ContentVersion{
		ContentID: live.ID, Version: next,
		TemplateID: tv.TemplateID, TemplateName: tv.Name,
		Title: title, Slug: slug, FolderPath: folderPath, FullPath: fullPath,
		Data: data, Actor: actorKind(actor), Via: actor.Via, AgentSession: actor.AgentSession,
		ModifiedByEmail: actor.Email, Comment: "page_generation publish replace",
		CreatedAt: now,
	}
	err := s.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		res, err := s.db.Collection("content").UpdateOne(sc,
			bson.M{"_id": live.ID, "current_version": live.CurrentVersion},
			bson.M{"$set": bson.M{
				"template_id": tv.TemplateID, "template_name": tv.Name,
				"title": title, "slug": slug, "folder_path": folderPath, "full_path": fullPath,
				"canonical_full_path": newCanonical,
				"data": data, "current_version": next, "updated_at": now,
			}})
		if err != nil {
			return mapDupKey(err, CodePathConflict, "page path conflicts at "+fullPath)
		}
		if res.MatchedCount == 0 {
			return genErr(CodeContentVersionConflict, "content changed concurrently", nil)
		}
		if _, err := s.db.Collection("content_versions").InsertOne(sc, ver); err != nil {
			return mapDupKey(err, CodeContentVersionConflict, "content version conflict")
		}
		if berr := s.idem.BindContentAndVersion(ctx, sc, opID, live.ID, next, newCanonical); berr != nil {
			return rwIdemErr(berr)
		}
		return nil
	})
	return err
}

// rwIdemErr wraps an idempotency bind failure for the publish error path.
func rwIdemErr(err error) error {
	code := idempotency.CodeOf(err)
	switch code {
	case idempotency.CodeConflict:
		return genErr(CodeIdempotencyConflict, "idempotency binding conflict", err)
	case idempotency.CodeAlreadyCompleted:
		return genErr(CodeRequestInProgress, "operation already completed; replay the cached response", err)
	case idempotency.CodeStaleAttempt:
		return genErr(CodePublicationConflict, "idempotency attempt is not active", err)
	default:
		return genErr(CodeInternal, "bind idempotency execution", err)
	}
}

// completePublishError releases (never completes) the attempt for
// non-cacheable publish failures. Pure 422 already handled at validation
// time; saga terminal failures are already MarkedTerminal — this only
// releases the lease so the caller can fix state and TakeOver/retry.
func (s *Service) completePublishError(ctx context.Context, op idempotency.Operation, perr error) {
	code := CodeOf(perr)
	if code == "" {
		code = mapSagaCode(perr)
	}
	status := StatusForCode(code)
	validationOnly := false
	// Only pure pre-mutation validation may replay; publish content writes
	// already happened here, so never attest validationOnly post-mutation.
	_, _ = s.idem.Complete(ctx, op.ID, op.Attempt, status, map[string]any{"code": code}, validationOnly)
}

// mapIdemBeginErr translates Begin failures to generation codes.
func mapIdemBeginErr(err error) error {
	code := idempotency.CodeOf(err)
	switch code {
	case idempotency.CodeConflict:
		return genErr(CodeIdempotencyConflict, "same Idempotency-Key with a different request body", err)
	case idempotency.CodeInProgress:
		return &Error{Code: CodeRequestInProgress, Message: "operation is already being executed", RetryAfter: 5, Err: err}
	case idempotency.CodeLeaseExpired:
		return &Error{Code: CodeRequestInProgress, Message: "worker lease expired; retry to take over the same attempt", RetryAfter: 1, Err: err}
	default:
		return genErr(CodeInternal, "begin idempotency operation", err)
	}
}

// mapSagaErr translates saga failures to generation codes with HTTP mapping.
func mapSagaErr(err error) error {
	code := mapSagaCode(err)
	msg := err.Error()
	switch code {
	case publication.CodePagePublishInProgress:
		return &Error{Code: CodePagePublishInProgress, Message: msg, RetryAfter: 5, Err: err}
	case publication.CodeStageFailed, publication.CodeVerifyFailed, publication.CodeActivateFailed,
		publication.CodeUnpublishStageFailed:
		// Static-store / cutover failures are retryable 503s.
		if storage.CodeOf(err) != "" || isStoreErr(err) {
			return &Error{Code: CodeStoreUnavailable, Message: msg, RetryAfter: 30, Err: err}
		}
		return &Error{Code: CodeStoreUnavailable, Message: msg, RetryAfter: 30, Err: err}
	case publication.CodeValidationFailed:
		return genErr(CodeFieldValidationFailed, msg, err)
	case publication.CodePathInvalid:
		return genErr(CodePathInvalid, msg, err)
	case publication.CodePathConflict:
		return genErr(CodePathConflict, msg, err)
	case publication.CodeContentVersionConflict:
		return genErr(CodeContentVersionConflict, msg, err)
	case publication.CodeTemplateNotActive:
		return genErr(CodeTemplateNotActive, msg, err)
	case publication.CodePublicURLFailed:
		return genErr(CodePublicURLFailed, msg, err)
	case publication.CodeConflict:
		return genErr(CodePublicationConflict, msg, err)
	case publication.CodeInternal:
		return genErr(CodeInternal, msg, err)
	default:
		if code == "PUBLICATION_CONFLICT" {
			return genErr(CodePublicationConflict, msg, err)
		}
		return genErr(CodeInternal, msg, err)
	}
}

func mapSagaCode(err error) string {
	if c := publication.CodeOf(err); c != "" {
		return c
	}
	if c := templatecontract.CodeOf(err); c != "" {
		switch c {
		case templatecontract.CodeNotFound:
			return CodeTemplateNotFound
		case templatecontract.CodeVersionNotFound:
			return CodeTemplateVersionNotFound
		case templatecontract.CodeVersionConflict:
			return CodeTemplateVersionConflict
		}
		return c
	}
	if c := idempotency.CodeOf(err); c != "" {
		return c
	}
	if c := storage.CodeOf(err); c != "" {
		return CodeStoreUnavailable
	}
	if c := CodeOf(err); c != "" {
		return c
	}
	return CodeInternal
}

func isStoreErr(err error) bool { return storage.CodeOf(err) != "" }

// responseFromCache rebuilds a GenerateResponse from a replayed idempotency payload.
func responseFromCache(op idempotency.Operation) (GenerateResponse, error) {
	m := op.Response
	if m == nil {
		return GenerateResponse{}, fmt.Errorf("empty replay payload")
	}
	str := func(k string) string {
		if v, ok := m[k].(string); ok {
			return v
		}
		return ""
	}
	num := func(k string) int64 {
		switch v := m[k].(type) {
		case float64:
			return int64(v)
		case int64:
			return v
		case int:
			return int64(v)
		case int32:
			return int64(v)
		}
		return 0
	}
	boolean := func(k string) bool {
		if v, ok := m[k].(bool); ok {
			return v
		}
		return false
	}
	var pubURL *string
	if u := str("public_url"); u != "" {
		pubURL = &u
	}
	var pubID *string
	if p := str("publication_id"); p != "" {
		pubID = &p
	}
	return GenerateResponse{
		ID: str("id"), Action: str("action"), Template: str("template"),
		FullPath: str("full_path"), Mode: str("mode"),
		Published: boolean("published"), RequiresPublish: boolean("requires_publish"),
		PublicURL: pubURL, ContentVersion: num("content_version"),
		TemplateVersion: num("template_version"), PublicationID: pubID,
		Warnings: []templatecontract.FieldWarning{},
	}, nil
}

var _ = time.Now
