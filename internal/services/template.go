package services

import (
	"context"
	"fmt"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/database"
	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// TemplateService handles template operations
type TemplateService struct {
	db             *database.DB
	contentService *ContentService
	regenQueue     *RegenQueue
}

// NewTemplateService creates a new template service
func NewTemplateService(db *database.DB, contentService *ContentService) *TemplateService {
	return &TemplateService{db: db, contentService: contentService}
}

// SetRegenQueue sets the incremental regeneration queue.
func (s *TemplateService) SetRegenQueue(rq *RegenQueue) {
	s.regenQueue = rq
}

// CreateTemplate creates a new template
// Task 16B (spec §9.5, §9.7): creates the immutable TemplateContract v1 via
// the shared contract service. No live pages are touched.
func (s *TemplateService) CreateTemplate(ctx context.Context, tmpl *models.Template) error {
	desc := tmpl.Description
	wantSystem := tmpl.IsSystem
	status := tmpl.Status
	if status == "" {
		// Legacy parity: templates without an explicit status are immediately
		// usable (active). Explicit draft/deprecated is honored.
		status = templatecontract.StatusActive
	}
	contract := templatecontract.NewService(s.db)
	in := templatecontract.TemplateInput{
		Slug: tmpl.Slug, Name: tmpl.Name, Category: tmpl.Category,
		Status: templatecontract.NormalizeStatus(status),
		HTMLLayout: tmpl.HTMLLayout, Fields: tmpl.Fields,
	}
	created, _, err := contract.Create(ctx, in)
	if err != nil {
		return err
	}
	*tmpl = created
	// Description/IsSystem live outside the versioned contract; restore the
	// caller's values without touching versions.
	tmpl.Description = desc
	tmpl.IsSystem = wantSystem
	set := bson.M{}
	if desc != "" {
		set["description"] = desc
	}
	if wantSystem {
		set["is_system"] = true
	}
	if len(set) > 0 {
		_ = s.db.UpdateOne(ctx, "templates", bson.M{"_id": tmpl.ID}, bson.M{"$set": set})
	}
	return nil
}

// UpdateTemplate updates a template and creates a new immutable version.
// Task 16B (spec §9.5): never auto-regenerates live pages. Existing
// Publications stay unchanged; an explicit Upgrade Job republishes.
func (s *TemplateService) UpdateTemplate(ctx context.Context, tmpl *models.Template) error {
	// Get original for expected-version CAS + slug immutability.
	var original models.Template
	if err := s.db.FindOne(ctx, "templates", bson.M{"_id": tmpl.ID}, &original); err != nil {
		return fmt.Errorf("template not found: %w", err)
	}
	if tmpl.Slug != "" && tmpl.Slug != original.Slug {
		return fmt.Errorf("template slug is immutable on ordinary update (use migrate-slug)")
	}
	expected := original.CurrentVersion
	contract := templatecontract.NewService(s.db)
	status := firstNonEmpty(tmpl.Status, original.Status)
	if status == "" {
		status = templatecontract.StatusActive
	}
	in := templatecontract.TemplateInput{
		Slug: original.Slug, Name: tmpl.Name, Category: tmpl.Category,
		Status: templatecontract.NormalizeStatus(status),
		HTMLLayout: tmpl.HTMLLayout, Fields: tmpl.Fields,
	}
	// Pre-V3 rows have CurrentVersion 0 and no v1 doc: backfill v1 first so
	// the contract CAS has a base (Task 14 migration does this at scale;
	// this is the per-row safety net, never a silent skip).
	if expected <= 0 {
		if err := s.backfillV1(ctx, &original); err != nil {
			return err
		}
		expected = 1
	}
	newVer, err := contract.Update(ctx, tmpl.ID, expected, in)
	if err != nil {
		return err
	}
	// Refresh caller + preserve description (presentation-only, not versioned).
	tmpl.CurrentVersion = newVer.Version
	tmpl.Slug = newVer.Slug
	tmpl.Status = newVer.Status
	tmpl.UpdatedAt = time.Now()
	if tmpl.Description != "" {
		_ = s.db.UpdateOne(ctx, "templates", bson.M{"_id": tmpl.ID}, bson.M{"$set": bson.M{"description": tmpl.Description, "updated_at": tmpl.UpdatedAt}})
	}
	// Task 16B: no RegenQueue.Enqueue, no regenerateContentByTemplate.
	// Live pages stay on their pinned TemplateVersion until an explicit
	// Upgrade Job publishes per page via PublicationService.
	return nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// backfillV1 creates Version 1 + CurrentVersion=1 for a pre-V3 template row
// (spec §35.2; Task 14 migration does this at scale, this is the per-row
// safety net). Idempotent: existing v1 wins, no duplicate. Empty legacy
// status defaults to active so existing pages remain publishable.
func (s *TemplateService) backfillV1(ctx context.Context, tpl *models.Template) error {
	status := tpl.Status
	if status == "" {
		status = templatecontract.StatusActive
	}
	status = templatecontract.NormalizeStatus(status)
	// Direct check for (template_id, version=1).
	var v1 templatecontract.TemplateVersion
	if err := s.db.FindOne(ctx, "template_versions", bson.M{"template_id": tpl.ID, "version": int64(1)}, &v1); err == nil {
		_, _ = s.db.Collection("templates").UpdateOne(ctx,
			bson.M{"_id": tpl.ID},
			bson.M{"$set": bson.M{"current_version": int64(1), "status": status, "updated_at": time.Now()}})
		return nil
	}
	in := templatecontract.TemplateInput{
		Slug: tpl.Slug, Name: tpl.Name, Category: tpl.Category,
		Status: status,
		HTMLLayout: tpl.HTMLLayout, Fields: tpl.Fields,
	}
	ver := templatecontract.TemplateVersion{
		TemplateID: tpl.ID, Version: 1,
		Slug: in.Slug, Name: in.Name, Category: in.Category, Status: in.Status,
		Fields: in.Fields, HTMLLayout: in.HTMLLayout,
		ContractHash: templatecontract.ContractHash(in),
		RenderHash:   templatecontract.RenderHash(in),
		CreatedAt:    time.Now(),
	}
	if _, err := s.db.Collection("template_versions").InsertOne(ctx, &ver); err != nil {
		// Resume/race: another writer won; repair CurrentVersion.
		_, _ = s.db.Collection("templates").UpdateOne(ctx,
			bson.M{"_id": tpl.ID},
			bson.M{"$set": bson.M{"current_version": int64(1), "updated_at": time.Now()}})
		return nil
	}
	_, _ = s.db.Collection("templates").UpdateOne(ctx,
		bson.M{"_id": tpl.ID},
		bson.M{"$set": bson.M{"current_version": int64(1), "status": in.Status, "updated_at": time.Now()}})
	return nil
}

// DeleteTemplate deletes a template (if not system template)
func (s *TemplateService) DeleteTemplate(ctx context.Context, id primitive.ObjectID) error {
	var tmpl models.Template
	if err := s.db.FindOne(ctx, "templates", bson.M{"_id": id}, &tmpl); err != nil {
		return fmt.Errorf("template not found: %w", err)
	}

	if tmpl.IsSystem {
		return fmt.Errorf("cannot delete system template")
	}

	// Check if any content uses this template
	count, err := s.db.Count(ctx, "content", bson.M{"template_id": id})
	if err != nil {
		return fmt.Errorf("failed to check content: %w", err)
	}
	if count > 0 {
		return fmt.Errorf("cannot delete template: %d content items use this template", count)
	}

	if err := s.db.DeleteOne(ctx, "templates", bson.M{"_id": id}); err != nil {
		return fmt.Errorf("failed to delete template: %w", err)
	}

	return nil
}

// GetTemplate retrieves a template by ID
func (s *TemplateService) GetTemplate(ctx context.Context, id primitive.ObjectID) (*models.Template, error) {
	var tmpl models.Template
	if err := s.db.FindOne(ctx, "templates", bson.M{"_id": id}, &tmpl); err != nil {
		return nil, fmt.Errorf("template not found: %w", err)
	}
	return &tmpl, nil
}

// GetTemplateBySlug retrieves a template by slug
func (s *TemplateService) GetTemplateBySlug(ctx context.Context, slug string) (*models.Template, error) {
	var tmpl models.Template
	if err := s.db.FindOne(ctx, "templates", bson.M{"slug": slug}, &tmpl); err != nil {
		return nil, fmt.Errorf("template not found: %w", err)
	}
	return &tmpl, nil
}

// ListTemplates lists all templates
func (s *TemplateService) ListTemplates(ctx context.Context) ([]models.Template, error) {
	cursor, err := s.db.FindMany(ctx, "templates", bson.M{},
		options.Find().SetSort(bson.D{{Key: "name", Value: 1}}))
	if err != nil {
		return nil, fmt.Errorf("failed to list templates: %w", err)
	}

	var templates []models.Template
	if err := cursor.All(ctx, &templates); err != nil {
		return nil, fmt.Errorf("failed to decode templates: %w", err)
	}

	return templates, nil
}

// regenerateContentByTemplate is retained as a deprecated no-op.
// Task 16B (spec §9.5): ordinary template updates never regenerate live
// pages. Explicit Template Upgrade Jobs publish per page via
// PublicationService. This stub exists so legacy callers fail safe (no live
// write) until Task 16 removes them.
func (s *TemplateService) regenerateContentByTemplate(ctx context.Context, templateID primitive.ObjectID) {
	fmt.Printf("[template] regenerateContentByTemplate disabled for %s: V3 requires an explicit Upgrade Job via PublicationService (no live pages touched)\n", templateID.Hex())
}
