package templatecontract

import (
	"context"
	"strings"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/database"
	"github.com/jonradoff/lightcms/v7/internal/models"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// Service orchestrates template creation and versioned updates (plan Task 3).
// It imports models; models never imports templatecontract (spec §8.4).
//
// Concurrency model (spec §9.6): version numbers are allocated by
// CAS-incrementing templates.current_version inside one Mongo transaction
// that also inserts the immutable version document and refreshes the mutable
// record. A lost CAS race returns TEMPLATE_VERSION_CONFLICT after a single
// reload-and-retry; no duplicate version document can exist thanks to the
// CAS filter plus the UNIQUE(template_id, version) backstop.
type Service struct {
	db   *database.DB
	repo *Repository
}

// NewService builds a Service over db.
func NewService(db *database.DB) *Service {
	return &Service{db: db, repo: NewRepository(db)}
}

// validateWriteInput validates everything except slug identity (Create checks
// the pattern; Update checks immutability against the stored slug). It
// normalizes empty status to draft and returns the normalized input.
func validateWriteInput(in TemplateInput) (TemplateInput, error) {
	if strings.TrimSpace(in.Name) == "" {
		return in, inputInvalid("template name is required")
	}
	status := NormalizeStatus(in.Status)
	if !IsValidStatus(status) {
		return in, inputInvalid("unknown template status %q", in.Status)
	}
	if err := ValidateFields(in.Fields); err != nil {
		return in, err
	}
	if err := ValidateScriptPolicy(in.ScriptPolicy); err != nil {
		return in, err
	}
	in.Status = status
	return in, nil
}

// Create inserts a new template with its immutable version 1. Duplicate slugs
// are rejected with TEMPLATE_SLUG_CONFLICT (fast-path read plus the unique
// index backstop for races).
func (s *Service) Create(ctx context.Context, in TemplateInput) (models.Template, TemplateVersion, error) {
	var zeroTpl models.Template
	var zeroVer TemplateVersion

	if err := ValidateSlug(in.Slug); err != nil {
		return zeroTpl, zeroVer, err
	}
	norm, err := validateWriteInput(in)
	if err != nil {
		return zeroTpl, zeroVer, err
	}
	in = norm

	if _, err := s.repo.FindTemplateBySlug(ctx, in.Slug); err == nil {
		return zeroTpl, zeroVer, &Error{Code: CodeSlugConflict,
			Message: "template slug " + in.Slug + " already exists"}
	} else if CodeOf(err) != CodeNotFound {
		return zeroTpl, zeroVer, err
	}

	contractHash := ContractHash(in)
	renderHash := RenderHash(in)
	now := time.Now()
	tpl := models.Template{
		Name:           in.Name,
		Slug:           in.Slug,
		Category:       in.Category,
		Fields:         in.Fields,
		HTMLLayout:     in.HTMLLayout,
		CurrentVersion: 1,
		Status:         in.Status,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	ver := TemplateVersion{
		Version:      1,
		Slug:         in.Slug,
		Name:         in.Name,
		Category:     in.Category,
		Status:       in.Status,
		Fields:       in.Fields,
		HTMLLayout:   in.HTMLLayout,
		ScriptPolicy: in.ScriptPolicy,
		ContractHash: contractHash,
		RenderHash:   renderHash,
		CreatedAt:    now,
	}

	if err := s.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		res, err := s.db.Collection("templates").InsertOne(sc, &tpl)
		if err != nil {
			return mapCreateDupKey(err)
		}
		tpl.ID = res.InsertedID.(primitive.ObjectID)
		ver.TemplateID = tpl.ID
		res, err = s.db.Collection("template_versions").InsertOne(sc, &ver)
		if err != nil {
			return mapCreateDupKey(err)
		}
		ver.ID = res.InsertedID.(primitive.ObjectID)
		return nil
	}); err != nil {
		return zeroTpl, zeroVer, err
	}
	return tpl, ver, nil
}

func mapCreateDupKey(err error) error {
	if !IsDuplicateKey(err) {
		return internalErr("create template", err)
	}
	// Fresh template ObjectIDs cannot collide on (template_id, version);
	// a duplicate here is the slug race the fast-path read missed.
	if strings.Contains(err.Error(), "template_versions") {
		return versionConflict("template version already exists")
	}
	return &Error{Code: CodeSlugConflict, Message: "template slug already exists"}
}

// Update creates a new immutable version when the contract changed, and is a
// no-op (returns the current version) when ContractHash is unchanged.
// Ordinary updates cannot change the slug (TEMPLATE_SLUG_IMMUTABLE); slug
// renames are the admin-only migrate-slug operation owned by Task 12.
// A stale expectedVersion yields TEMPLATE_VERSION_CONFLICT with zero mutation.
func (s *Service) Update(ctx context.Context, id primitive.ObjectID, expectedVersion int64, in TemplateInput) (TemplateVersion, error) {
	var zero TemplateVersion

	tpl, err := s.repo.FindTemplateByID(ctx, id)
	if err != nil {
		return zero, err
	}
	if tpl.CurrentVersion != expectedVersion {
		return zero, versionConflict("template %s: expected version %d, current is %d",
			id.Hex(), expectedVersion, tpl.CurrentVersion)
	}
	if in.Slug != tpl.Slug {
		return zero, &Error{Code: CodeSlugImmutable,
			Message: "template slug is immutable on ordinary update (use migrate-slug)"}
	}
	norm, err := validateWriteInput(in)
	if err != nil {
		return zero, err
	}
	in = norm
	if err := ValidateStatusTransition(tpl.Status, in.Status); err != nil {
		return zero, err
	}

	contractHash := ContractHash(in)
	renderHash := RenderHash(in)
	cur, err := s.repo.FindVersion(ctx, id, tpl.CurrentVersion)
	if err != nil {
		return zero, err
	}
	if cur.ContractHash == contractHash {
		// Same contract: no new version. Display-only mutable state (Name)
		// still follows so the admin list stays accurate.
		if in.Name != tpl.Name {
			if _, err := s.db.Collection("templates").UpdateOne(ctx,
				bson.M{"_id": id},
				bson.M{"$set": bson.M{"name": in.Name, "updated_at": time.Now()}}); err != nil {
				return zero, internalErr("follow display name", err)
			}
		}
		return cur, nil
	}

	newNum := expectedVersion + 1
	if err := s.insertVersionTxn(ctx, id, expectedVersion, newNum, in, contractHash, renderHash); err != nil {
		if CodeOf(err) != CodeVersionConflict {
			return zero, err
		}
		// Spec §9.6: retry once after a fresh reload, then 409.
		reloaded, err2 := s.repo.FindTemplateByID(ctx, id)
		if err2 != nil {
			return zero, err2
		}
		if cur2, err2 := s.repo.FindVersion(ctx, id, reloaded.CurrentVersion); err2 == nil &&
			cur2.ContractHash == contractHash {
			return cur2, nil // Converged: another writer made the same change.
		}
		if reloaded.CurrentVersion != expectedVersion {
			return zero, versionConflict("template %s: expected version %d, current is %d",
				id.Hex(), expectedVersion, reloaded.CurrentVersion)
		}
		if err := s.insertVersionTxn(ctx, id, expectedVersion, newNum, in, contractHash, renderHash); err != nil {
			if CodeOf(err) == CodeVersionConflict {
				return zero, versionConflict("template %s: concurrent update, retry with version %d",
					id.Hex(), reloaded.CurrentVersion)
			}
			return zero, err
		}
	}
	return s.repo.FindVersion(ctx, id, newNum)
}

// insertVersionTxn CAS-increments templates.current_version (pinned to the
// expected value), inserts the immutable version, and refreshes the mutable
// record — all in one transaction. CAS loss or a duplicate version insert
// maps to TEMPLATE_VERSION_CONFLICT.
func (s *Service) insertVersionTxn(ctx context.Context, id primitive.ObjectID, expected, newNum int64, in TemplateInput, contractHash, renderHash string) error {
	now := time.Now()
	return s.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		ures, err := s.db.Collection("templates").UpdateOne(sc,
			bson.M{"_id": id, "current_version": expected},
			bson.M{
				"$inc": bson.M{"current_version": 1},
				"$set": bson.M{
					"name":        in.Name,
					"category":    in.Category,
					"status":      in.Status,
					"fields":      in.Fields,
					"html_layout": in.HTMLLayout,
					"updated_at":  now,
				},
			})
		if err != nil {
			return internalErr("increment template version", err)
		}
		if ures.MatchedCount == 0 {
			return versionConflict("template %s: concurrent update on version %d", id.Hex(), expected)
		}
		ver := TemplateVersion{
			TemplateID:   id,
			Version:      newNum,
			Slug:         in.Slug,
			Name:         in.Name,
			Category:     in.Category,
			Status:       in.Status,
			Fields:       in.Fields,
			HTMLLayout:   in.HTMLLayout,
			ScriptPolicy: in.ScriptPolicy,
			ContractHash: contractHash,
			RenderHash:   renderHash,
			CreatedAt:    now,
		}
		if _, err := s.db.Collection("template_versions").InsertOne(sc, ver); err != nil {
			if IsDuplicateKey(err) {
				return versionConflict("template %s: version %d already exists", id.Hex(), newNum)
			}
			return internalErr("insert template version", err)
		}
		return nil
	})
}

// GetCurrent returns the current immutable version for a slug.
func (s *Service) GetCurrent(ctx context.Context, slug string) (TemplateVersion, error) {
	var zero TemplateVersion
	tpl, err := s.repo.FindTemplateBySlug(ctx, slug)
	if err != nil {
		return zero, err
	}
	return s.repo.FindVersion(ctx, tpl.ID, tpl.CurrentVersion)
}

// GetVersion returns one immutable version by its document ID.
func (s *Service) GetVersion(ctx context.Context, id primitive.ObjectID) (TemplateVersion, error) {
	return s.repo.FindVersionByID(ctx, id)
}
