package generation

import (
	"context"
	"strings"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/idempotency"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// draft implements mode=draft (§20.6): absent/unpublished → Main draft;
// published → Fork draft (never touches the main Content row or live files).
func (s *Service) draft(ctx context.Context, actor Actor, tv templatecontract.TemplateVersion, live *models.Content, active *publication.Publication, published, targetExists bool, fullPath, slug, folderPath, title, canonical string, data map[string]any, warns []templatecontract.FieldWarning) (GenerateResponse, error) {
	var zero GenerateResponse
	if !published {
		if live == nil {
			id, cv, err := s.createMainDraft(ctx, actor, tv, fullPath, slug, folderPath, title, canonical, data)
			if err != nil {
				return zero, err
			}
			s.auditf(ctx, "page_generation.create", map[string]any{
				"template": tv.Slug, "template_version": tv.Version,
				"content_id": id.Hex(), "full_path": fullPath, "mode": ModeDraft,
			})
			return GenerateResponse{
				ID: id.Hex(), Action: "created", Template: tv.Slug, FullPath: fullPath, Mode: ModeDraft,
				Published: false, RequiresPublish: true,
				ContentVersion: cv, TemplateVersion: tv.Version, Warnings: warns,
			}, nil
		}
		cv, err := s.replaceMainDraft(ctx, actor, tv, live, fullPath, slug, folderPath, title, data)
		if err != nil {
			return zero, err
		}
		s.auditf(ctx, "page_generation.update", map[string]any{
			"template": tv.Slug, "template_version": tv.Version,
			"content_id": live.ID.Hex(), "full_path": fullPath, "mode": ModeDraft,
		})
		return GenerateResponse{
			ID: live.ID.Hex(), Action: "updated", Template: tv.Slug, FullPath: fullPath, Mode: ModeDraft,
			Published: false, RequiresPublish: true,
			ContentVersion: cv, TemplateVersion: tv.Version, Warnings: warns,
		}, nil
	}
	// Published target → Fork draft.
	forkID, cv, created, err := s.upsertForkDraft(ctx, actor, tv, live, fullPath, slug, folderPath, title, canonical, data, "")
	if err != nil {
		return zero, err
	}
	action := "updated"
	if created {
		action = "created"
	}
	s.auditf(ctx, "page_generation.update", map[string]any{
		"template": tv.Slug, "template_version": tv.Version,
		"content_id": live.ID.Hex(), "fork_id": forkID.Hex(), "full_path": fullPath, "mode": ModeDraft,
	})
	_ = forkID
	return GenerateResponse{
		ID: live.ID.Hex(), Action: action, Template: tv.Slug, FullPath: fullPath, Mode: ModeDraft,
		Published: true, RequiresPublish: true,
		ContentVersion: cv, TemplateVersion: tv.Version, Warnings: warns,
	}, nil
}

// sandbox implements mode=sandbox: always the actor's sandbox Fork.
func (s *Service) sandbox(ctx context.Context, actor Actor, tv templatecontract.TemplateVersion, live *models.Content, active *publication.Publication, published bool, fullPath, slug, folderPath, title, canonical string, data map[string]any, warns []templatecontract.FieldWarning) (GenerateResponse, error) {
	var zero GenerateResponse
	scope := actor.SandboxForkID.Hex()
	forkID, cv, created, err := s.upsertForkDraft(ctx, actor, tv, live, fullPath, slug, folderPath, title, canonical, data, scope)
	if err != nil {
		return zero, err
	}
	action := "updated"
	if created {
		action = "created"
	}
	publishedOut := active != nil
	s.auditf(ctx, "page_generation.update", map[string]any{
		"template": tv.Slug, "template_version": tv.Version,
		"fork_id": forkID.Hex(), "full_path": fullPath, "mode": ModeSandbox,
	})
	_ = forkID
	return GenerateResponse{
		ID: func() string {
			if live != nil && publishedOut {
				return live.ID.Hex()
			}
			return forkID.Hex()
		}(), Action: action, Template: tv.Slug, FullPath: fullPath, Mode: ModeSandbox,
		Published: publishedOut, RequiresPublish: true,
		ContentVersion: cv, TemplateVersion: tv.Version, Warnings: warns,
	}, nil
}

// createMainDraft inserts a new unpublished Main content + version 1.
func (s *Service) createMainDraft(ctx context.Context, actor Actor, tv templatecontract.TemplateVersion, fullPath, slug, folderPath, title, canonical string, data map[string]any) (primitive.ObjectID, int64, error) {
	now := s.now().UTC()
	id := primitive.NewObjectID()
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
		ModifiedByEmail: actor.Email, Comment: "page_generation draft create",
		CreatedAt: now,
	}
	err := s.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		if _, err := s.db.Collection("content").InsertOne(sc, content); err != nil {
			return mapDupKey(err, CodePathConflict, "page already exists at "+fullPath)
		}
		if _, err := s.db.Collection("content_versions").InsertOne(sc, ver); err != nil {
			return mapDupKey(err, CodeContentVersionConflict, "content version conflict")
		}
		return nil
	})
	if err != nil {
		return primitive.NilObjectID, 0, err
	}
	return id, 1, nil
}

// replaceMainDraft full-replaces an unpublished Main draft (CAS version bump).
func (s *Service) replaceMainDraft(ctx context.Context, actor Actor, tv templatecontract.TemplateVersion, live *models.Content, fullPath, slug, folderPath, title string, data map[string]any) (int64, error) {
	now := s.now().UTC()
	next := live.CurrentVersion + 1
	if next < 1 {
		next = 1
	}
	// Detect a path change (rename-as-draft): recompute canonical for the new
	// path and move the uniqueness key in the same transaction.
	newCanonical := live.CanonicalFullPath
	moved := false
	if fullPath != live.FullPath {
		moved = true
		// Caller already validated fullPath via pathkey.Canonical.
		newCanonical = lowerCanonical(fullPath)
	}
	ver := &models.ContentVersion{
		ContentID: live.ID, Version: next,
		TemplateID: tv.TemplateID, TemplateName: tv.Name,
		Title: title, Slug: slug, FolderPath: folderPath, FullPath: fullPath,
		Data: data, Actor: actorKind(actor), Via: actor.Via, AgentSession: actor.AgentSession,
		ModifiedByEmail: actor.Email, Comment: "page_generation draft replace",
		CreatedAt: now,
	}
	err := s.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		set := bson.M{
			"template_id": tv.TemplateID, "template_name": tv.Name,
			"title": title, "slug": slug, "folder_path": folderPath, "full_path": fullPath,
			"data": data, "current_version": next, "updated_at": now,
		}
		if moved {
			set["canonical_full_path"] = newCanonical
		}
		res, err := s.db.Collection("content").UpdateOne(sc,
			bson.M{"_id": live.ID, "current_version": live.CurrentVersion},
			bson.M{"$set": set})
		if err != nil {
			return mapDupKey(err, CodePathConflict, "page path conflicts at "+fullPath)
		}
		if res.MatchedCount == 0 {
			return genErr(CodeContentVersionConflict, "content changed concurrently", nil)
		}
		if _, err := s.db.Collection("content_versions").InsertOne(sc, ver); err != nil {
			return mapDupKey(err, CodeContentVersionConflict, "content version conflict")
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return next, nil
}

// upsertForkDraft creates or replaces the Fork draft for a canonical path.
// scope=="" means auto Fork (draft on published: find any live Fork draft for
// reuse, else mint a new fork workspace); scope!= "" pins to that sandbox scope.
func (s *Service) upsertForkDraft(ctx context.Context, actor Actor, tv templatecontract.TemplateVersion, live *models.Content, fullPath, slug, folderPath, title, canonical string, data map[string]any, scope string) (primitive.ObjectID, int64, bool, error) {
	now := s.now().UTC()
	// Reuse check.
	existing, err := s.findFork(ctx, canonical, scope)
	if err != nil {
		return primitive.NilObjectID, 0, false, genErr(CodeInternal, "lookup fork draft", err)
	}
	if existing != nil {
		next := existing.CurrentVersion + 1
		if next < 1 {
			next = 1
		}
		ver := &models.ContentVersion{
			ContentID: existing.ID, Version: next,
			TemplateID: tv.TemplateID, TemplateName: tv.Name,
			Title: title, Slug: slug, FolderPath: folderPath, FullPath: fullPath,
			Data: data, Actor: actorKind(actor), Via: actor.Via, AgentSession: actor.AgentSession,
			ModifiedByEmail: actor.Email, Comment: "page_generation fork replace",
			CreatedAt: now,
		}
		err := s.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
			res, err := s.db.Collection("content").UpdateOne(sc,
				bson.M{"_id": existing.ID, "current_version": existing.CurrentVersion},
				bson.M{"$set": bson.M{
					"template_id": tv.TemplateID, "template_name": tv.Name,
					"title": title, "slug": slug, "folder_path": folderPath, "full_path": fullPath,
					"data": data, "current_version": next, "updated_at": now,
				}})
			if err != nil {
				return err
			}
			if res.MatchedCount == 0 {
				return genErr(CodeContentVersionConflict, "fork draft changed concurrently", nil)
			}
			if _, err := s.db.Collection("content_versions").InsertOne(sc, ver); err != nil {
				return mapDupKey(err, CodeContentVersionConflict, "content version conflict")
			}
			// Mark the live row as having unpublished changes (same txn).
			if live != nil {
				_, _ = s.db.Collection("content").UpdateOne(sc,
					bson.M{"_id": live.ID},
					bson.M{"$set": bson.M{"has_unpublished_changes": true, "updated_at": now}})
			}
			return nil
		})
		if err != nil {
			return primitive.NilObjectID, 0, false, err
		}
		return forkIDValue(existing), next, false, nil
	}
	// Create a new Fork draft.
	var forkID primitive.ObjectID
	var forkScope string
	if scope != "" {
		forkScope = scope
		if actor.SandboxForkID != nil {
			forkID = *actor.SandboxForkID
		} else {
			var err error
			forkID, err = primitive.ObjectIDFromHex(scope)
			if err != nil {
				forkID = primitive.NewObjectID()
				forkScope = forkID.Hex()
			}
		}
	} else {
		forkID = primitive.NewObjectID()
		forkScope = forkID.Hex()
	}
	id := primitive.NewObjectID()
	fork := &models.Content{
		ID: id, TemplateID: tv.TemplateID, TemplateName: tv.Name,
		Title: title, Slug: slug, FolderPath: folderPath, FullPath: fullPath,
		CanonicalFullPath: canonical, PathScope: forkScope, PathActive: true,
		CurrentVersion: 1, Data: data,
		Published: false, ForkID: &forkID,
		CreatedAt: now, UpdatedAt: now,
	}
	ver := &models.ContentVersion{
		ContentID: id, Version: 1,
		TemplateID: tv.TemplateID, TemplateName: tv.Name,
		Title: title, Slug: slug, FolderPath: folderPath, FullPath: fullPath,
		Data: data, Actor: actorKind(actor), Via: actor.Via, AgentSession: actor.AgentSession,
		ModifiedByEmail: actor.Email, Comment: "page_generation fork create",
		CreatedAt: now,
	}
	err = s.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		if _, err := s.db.Collection("content").InsertOne(sc, fork); err != nil {
			return mapDupKey(err, CodePathConflict, "fork draft conflicts at "+fullPath)
		}
		if _, err := s.db.Collection("content_versions").InsertOne(sc, ver); err != nil {
			return mapDupKey(err, CodeContentVersionConflict, "content version conflict")
		}
		if live != nil {
			_, _ = s.db.Collection("content").UpdateOne(sc,
				bson.M{"_id": live.ID},
				bson.M{"$set": bson.M{"has_unpublished_changes": true, "updated_at": now}})
		}
		return nil
	})
	if err != nil {
		return primitive.NilObjectID, 0, false, err
	}
	return forkID, 1, true, nil
}

// findFork locates a Fork draft for canonical. scope=="" matches any fork
// scope (draft reuse); scope!= "" pins to that sandbox scope.
func (s *Service) findFork(ctx context.Context, canonical, scope string) (*models.Content, error) {
	filter := bson.M{"canonical_full_path": canonical, "path_active": true}
	if scope != "" {
		filter["path_scope"] = scope
	} else {
		filter["path_scope"] = bson.M{"$ne": "live"}
	}
	var c models.Content
	err := s.db.FindOne(ctx, "content", filter, &c)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if c.Deleted {
		return nil, nil
	}
	return &c, nil
}

func actorKind(a Actor) string {
	if a.ActorKind != "" {
		return a.ActorKind
	}
	if a.AgentSession != "" {
		return "agent"
	}
	return "human"
}

// lowerCanonical recomputes the canonical key for a moved path without
// re-validating (caller already validated via pathkey.Canonical).
func lowerCanonical(fullPath string) string {
	return strings.ToLower(fullPath)
}

// mapDupKey translates duplicate-key driver errors to a typed generation code.
func mapDupKey(err error, code, msg string) error {
	if err == nil {
		return nil
	}
	if mongo.IsDuplicateKeyError(err) || strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "E11000") {
		return genErr(code, msg, err)
	}
	return err
}

// ForkIDValue returns the fork workspace ID for a fork content row.
func forkIDValue(c *models.Content) primitive.ObjectID {
	if c != nil && c.ForkID != nil {
		return *c.ForkID
	}
	return primitive.NilObjectID
}

// completeValidationOp caches a pure 422 for the owned publish idempotency
// record (the caller Begin/took-over the op before validating, so no second
// Begin is needed — and a second Begin would wedge on our own live lease).
// Field details ride along so a same-key replay rebuilds the identical 422
// instead of degrading to a code-only error.
func (s *Service) completeValidationOp(ctx context.Context, op *idempotency.Operation, status int, req GenerateRequest, tv templatecontract.TemplateVersion, details []FieldDetail) {
	if s.idem == nil || op == nil {
		return
	}
	resp := map[string]any{"code": CodeFieldValidationFailed, "template": tv.Slug, "template_version": tv.Version}
	if len(details) > 0 {
		det := make([]any, 0, len(details))
		for _, e := range details {
			det = append(det, map[string]any{"code": e.Code, "field": e.Field, "message": e.Message})
		}
		resp["details"] = det
	}
	_, _ = s.idem.Complete(ctx, op.ID, op.Attempt, status, resp, true)
}

// canonicalGenerateBody is the idempotency hash input for publish. It must
// be stable across template upgrades for the same logical request (R06):
// the caller's expected_template_version is hashed (different expectations
// are different requests), but the RESOLVED current version is not —
// otherwise every upgrade turns same-key retries into false conflicts
// instead of replays. (Pre-R06 hashes included the resolved version; keys
// begun before this change 409 on retry-after-upgrade instead of replaying
// — fail-closed, never a duplicate publication.)
func canonicalGenerateBody(req GenerateRequest, tv templatecontract.TemplateVersion) map[string]any {
	m := map[string]any{
		"template": req.Template, "title": req.Title, "slug": req.Slug,
		"folder_path": req.FolderPath, "mode": req.Mode, "upsert": req.Upsert,
		"data": req.Data,
	}
	if req.ExpectedTemplateVersion != nil {
		m["expected_template_version"] = *req.ExpectedTemplateVersion
	}
	return m
}

// ensure time import is used (now func).
var _ = time.Now
