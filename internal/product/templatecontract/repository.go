package templatecontract

import (
	"context"
	"fmt"

	"github.com/jonradoff/lightcms/v7/internal/database"
	"github.com/jonradoff/lightcms/v7/internal/models"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// Repository is the Mongo persistence layer for templates and their immutable
// versions. Methods take context.Context so callers inside a transaction pass
// the mongo.SessionContext through; the repository never opens nested
// transactions (plan Shared interface contract). Uniqueness backstops come
// from Task 2's EnsureProductIndexes: UNIQUE(slug) on templates and
// UNIQUE(template_id, version) on template_versions.
type Repository struct {
	db *database.DB
}

// NewRepository builds a Repository over db.
func NewRepository(db *database.DB) *Repository {
	return &Repository{db: db}
}

// FindTemplateByID loads the mutable template record by ID.
func (r *Repository) FindTemplateByID(ctx context.Context, id primitive.ObjectID) (models.Template, error) {
	var tpl models.Template
	err := r.db.Collection("templates").FindOne(ctx, bson.M{"_id": id}).Decode(&tpl)
	if err == mongo.ErrNoDocuments {
		return tpl, &Error{Code: CodeNotFound, Message: fmt.Sprintf("template %s not found", id.Hex())}
	}
	if err != nil {
		return tpl, internalErr("find template", err)
	}
	return tpl, nil
}

// FindTemplateBySlug loads the mutable template record by slug.
func (r *Repository) FindTemplateBySlug(ctx context.Context, slug string) (models.Template, error) {
	var tpl models.Template
	err := r.db.Collection("templates").FindOne(ctx, bson.M{"slug": slug}).Decode(&tpl)
	if err == mongo.ErrNoDocuments {
		return tpl, &Error{Code: CodeNotFound, Message: fmt.Sprintf("template slug %q not found", slug)}
	}
	if err != nil {
		return tpl, internalErr("find template by slug", err)
	}
	return tpl, nil
}

// FindVersion loads one immutable version by (templateID, version).
func (r *Repository) FindVersion(ctx context.Context, templateID primitive.ObjectID, version int64) (TemplateVersion, error) {
	var v TemplateVersion
	err := r.db.Collection("template_versions").FindOne(ctx,
		bson.M{"template_id": templateID, "version": version}).Decode(&v)
	if err == mongo.ErrNoDocuments {
		return v, &Error{
			Code:    CodeVersionNotFound,
			Message: fmt.Sprintf("template version %d for template %s not found", version, templateID.Hex()),
		}
	}
	if err != nil {
		return v, internalErr("find template version", err)
	}
	return v, nil
}

// FindVersionByID loads one immutable version by its document ID.
func (r *Repository) FindVersionByID(ctx context.Context, id primitive.ObjectID) (TemplateVersion, error) {
	var v TemplateVersion
	err := r.db.Collection("template_versions").FindOne(ctx, bson.M{"_id": id}).Decode(&v)
	if err == mongo.ErrNoDocuments {
		return v, &Error{Code: CodeVersionNotFound, Message: fmt.Sprintf("template version %s not found", id.Hex())}
	}
	if err != nil {
		return v, internalErr("find template version by id", err)
	}
	return v, nil
}
