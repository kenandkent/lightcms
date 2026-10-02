// Admin-only template slug migration (§8.1).
//
// MVP rules: validate the new slug, enforce uniqueness, record audit, return
// affected external-integration guidance. The operation must NOT alter
// historical Template Versions, create a public-page URL redirect, acquire
// page-path locks, or switch any Publication. Live page bytes and the active
// Publication stay byte-identical (tests assert this).
package generation

import (
	"context"
	"fmt"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// MigrateSlugResult is the admin guidance returned by MigrateSlug.
type MigrateSlugResult struct {
	TemplateID    string `json:"template_id"`
	OldSlug       string `json:"old_slug"`
	NewSlug       string `json:"new_slug"`
	AffectedPages int64  `json:"affected_pages"`
	// Guidance for external integrations (MCP/REST/API clients): the slug is
	// a machine identifier, not a page URL — no redirect is created.
	Guidance []string `json:"guidance"`
}

// MigrateSlug renames the mutable Template record's slug. Historical
// template_versions rows keep the old slug (traceability); no redirect row,
// no path lock, no Publication switch happens here.
func (s *Service) MigrateSlug(ctx context.Context, actor Actor, templateID primitive.ObjectID, newSlug string) (MigrateSlugResult, error) {
	var zero MigrateSlugResult
	if !actor.Authenticated {
		return zero, genErr(CodeUnauthenticated, "authentication is required", nil)
	}
	if !actor.IsAdmin || !actor.Can(ScopeTemplateEdit) {
		return zero, genErr(CodePermissionDenied, "migrate-slug requires an admin with template.edit", nil)
	}
	if err := templatecontract.ValidateSlug(newSlug); err != nil {
		return zero, &Error{Code: CodeFieldValidationFailed, Message: fmt.Sprintf("invalid slug %q", newSlug),
			Details: []FieldDetail{{Code: "TEMPLATE_SLUG_INVALID", Field: "slug", Message: err.Error()}}, Err: err}
	}
	tpl, err := s.templatesRepo().FindTemplateByID(ctx, templateID)
	if err != nil {
		if templatecontract.CodeOf(err) == templatecontract.CodeNotFound {
			return zero, genErr(CodeTemplateNotFound, "template not found", err)
		}
		return zero, genErr(CodeInternal, "load template", err)
	}
	if tpl.Slug == newSlug {
		return zero, genErr(CodeInvalidRequest, "new slug equals the current slug", nil)
	}
	// Uniqueness gate (fast-path read; the unique index is the arbiter).
	if _, err := s.templatesRepo().FindTemplateBySlug(ctx, newSlug); err == nil {
		return zero, genErr(CodePathConflict, fmt.Sprintf("template slug %q already exists", newSlug), nil)
	} else if templatecontract.CodeOf(err) != templatecontract.CodeNotFound {
		return zero, genErr(CodeInternal, "check slug uniqueness", err)
	}
	now := s.now()
	if now.IsZero() {
		now = time.Now()
	}
	res, err := s.db.Collection("templates").UpdateOne(ctx,
		bson.M{"_id": templateID, "slug": tpl.Slug},
		bson.M{"$set": bson.M{"slug": newSlug, "updated_at": now}})
	if err != nil {
		return zero, mapDupKey(err, CodePathConflict, fmt.Sprintf("template slug %q already exists", newSlug))
	}
	if res.MatchedCount == 0 {
		return zero, genErr(CodeTemplateVersionConflict, "template changed concurrently; reload and retry", nil)
	}
	// Affected pages: live content rows pointing at this template (guidance
	// only — their bytes, paths and Publications are untouched).
	affected, _ := s.db.Collection("content").CountDocuments(ctx, bson.M{"template_id": templateID})
	s.auditf(ctx, "template.slug.migrate", map[string]any{
		"template_id": templateID.Hex(), "old_slug": tpl.Slug, "new_slug": newSlug,
		"actor": actor.Email, "affected_pages": affected,
	})
	return MigrateSlugResult{
		TemplateID: templateID.Hex(), OldSlug: tpl.Slug, NewSlug: newSlug,
		AffectedPages: affected,
		Guidance: []string{
			fmt.Sprintf("Update external integrations referencing template slug %q to %q.", tpl.Slug, newSlug),
			"Template slug is not a public page path: no URL redirect was created and none is needed.",
			"Historical template versions keep the old slug for traceability; new generations must use the new slug.",
			fmt.Sprintf("%d page(s) reference this template; their live bytes and Publications are unchanged.", affected),
		},
	}, nil
}

func (s *Service) templatesRepo() *templatecontract.Repository {
	// Repository is stateless over DB; rebuild cheaply to avoid storing it.
	return templatecontract.NewRepository(s.db)
}
