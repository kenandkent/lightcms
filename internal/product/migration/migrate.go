// Package migration implements Task 14: existing-site migration and
// reconciliation (plan Task 14, Lane G; spec §35).
//
// Scope boundary: this package owns internal/product/migration/* only. It
// IMPORTS Task 2 (database.EnsureProductIndexes, pathkey.Canonical), Task 3
// (templatecontract hashes/model), Task 5 (publication.Repository), Task 6
// (storage.Store) and Task 10 (publication migration-flag key/value strings)
// and must not redefine their indexes, models, or flag constants.
//
// CLI wiring belongs to the integration owner in Task 16: do NOT create a
// second binary and do NOT edit cmd/server/main.go. Task 16 wires:
//
//	m := migration.New(migration.Config{DB: db, Store: store, BaseURL: baseURL})
//	rep, err := m.DryRun(ctx) // lightcms migrate-publications --dry-run
//	rep, err := m.Run(ctx)    // lightcms migrate-publications --apply
//
// Exit code: non-zero when err != nil, including ErrMigrationBlocked.
// Print or persist the returned Report as the migration audit record.
//
// Flag contract (shared with the Task 10 scanner, which only READS):
// collection "system_migrations", document {_id, key} = "publication_model_v1",
// state field "status" in {"not_started","running","completed"}. The scanner
// quarantines orphan files only after "completed"; while "running" it only
// reports canonical-without-active.
package migration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/database"
	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/pathkey"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/storage"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Migration flag contract. Collection/key/completed/running alias the Task 10
// scanner constants so the strings cannot drift; not_started is defined here
// (the scanner treats absent/unknown as "not completed", which covers it).
const (
	// CollectionMigrations tracks one-shot model migrations (spec §34.2).
	CollectionMigrations = publication.CollectionMigrations
	// MigrationKey is the flag this package flips not_started→running→completed.
	MigrationKey = publication.MigrationKeyPublicationModelV1
	// StatusNotStarted is the initial state (no flag document yet, or explicit).
	StatusNotStarted = "not_started"
	// StatusRunning means reconciliation is underway: the scanner reports but
	// never quarantines canonical-without-active.
	StatusRunning = publication.MigrationRunning
	// StatusCompleted opens the scanner orphan-quarantine gate.
	StatusCompleted = publication.MigrationCompleted
)

// Legacy index dropped only after the new canonical index is verified
// (spec §12.8). The name is the driver default for keys
// (full_path, fork_id) created by DB.createIndexes.
const legacyIndexName = "full_path_1_fork_id_1"

// New canonical index name created by Task 2's EnsureProductIndexes.
const canonicalIndexName = "content_canonical_path_scope_unique"

// ErrMigrationBlocked is returned (wrapped, with the blocking count) when
// apply cannot complete: invalid slugs, canonical collisions, version
// duplicates, invalid paths, missing files, or render errors remain. Fix the
// reported items and re-run; completed pages are skipped, never duplicated.
var ErrMigrationBlocked = errors.New("migration blocked: resolve the reported items and re-run")

// Config wires the migrator. DB and Store are required. Render defaults to
// the production renderer (publication.PlanSnapshot + publication.Render);
// tests inject a fake. BaseURL prefixes resolved public URLs; empty falls
// back to the bare full path (hash comparison only needs determinism).
type Config struct {
	DB      *database.DB
	Store   storage.Store
	Render  RenderFunc
	BaseURL string
}

// RenderInput is the frozen per-page render request.
type RenderInput struct {
	Content       models.Content
	Template      templatecontract.TemplateVersion
	PublicationID primitive.ObjectID
	LogicalAt     time.Time
	PublicURL     string
}

// RenderFunc renders one page deterministically: same input → same bytes.
// It performs no DB or file I/O. The returned hash must be "sha256:<hex>".
type RenderFunc func(ctx context.Context, in RenderInput) ([]byte, string, error)

// Migrator runs dry-run analysis (DryRun, zero writes) and the restartable
// apply pass (Run). Construct with New; Task 16 may also use the
// package-level Run/DryRun helpers with the same Config.
type Migrator struct {
	db     *database.DB
	store  storage.Store
	render RenderFunc
	base   string
	repo   *publication.Repository
}

// New builds a Migrator. It panics on a nil DB or Store (wiring error); a
// nil Render selects the default production renderer.
func New(cfg Config) *Migrator {
	if cfg.DB == nil {
		panic("migration: Config.DB is required")
	}
	if cfg.Store == nil {
		panic("migration: Config.Store is required")
	}
	render := cfg.Render
	if render == nil {
		render = defaultRender
	}
	return &Migrator{db: cfg.DB, store: cfg.Store, render: render, base: cfg.BaseURL,
		repo: publication.NewRepository(cfg.DB, nil)}
}

// Run executes the apply pass (see package doc for the full order).
// Re-running after interruption resumes; re-running after completion is a
// no-op returning Completed=true with no new publications.
func Run(ctx context.Context, cfg Config) (Report, error) {
	return New(cfg).Run(ctx)
}

// DryRun analyzes without writing: no DB inserts/updates, no file stages,
// no flag transitions, no index changes.
func DryRun(ctx context.Context, cfg Config) (Report, error) {
	return New(cfg).DryRun(ctx)
}

// GetState reads the current migration flag ("", not_started, running,
// completed). "" (absent/unparseable) means not started; the Task 10 scanner
// treats it the same way (quarantine gate shut).
func GetState(ctx context.Context, db *database.DB) string {
	return readFlag(ctx, db)
}

// defaultRender is the production renderer: freeze a publication snapshot
// from the live content + immutable template v1 and render in memory. No
// DB or file I/O; no wall-clock reads (logical time comes from the caller).
func defaultRender(ctx context.Context, in RenderInput) ([]byte, string, error) {
	version := in.Content.CurrentVersion
	if version < 1 {
		version = 1
	}
	snap, err := publication.PlanSnapshot(
		in.Content, version, in.Template,
		in.PublicationID, in.LogicalAt, in.PublicURL,
		publication.PlanOptions{},
	)
	if err != nil {
		return nil, "", err
	}
	return publication.Render(ctx, snap)
}

// page is one live content row with its derived migration state.
type page struct {
	content   models.Content
	canonical string // pathkey.Canonical(FullPath); "" when invalid
	invalid   string // invalid-path reason; "" when valid
	maxVer    int64  // max content_versions version; >=1
	collided  bool
}

// analysis is the shared read-only pass used by both DryRun and Run.
type analysis struct {
	now       time.Time
	flag      string
	templates []models.Template
	tmplByID  map[primitive.ObjectID]models.Template
	v1ByTmpl  map[primitive.ObjectID]templatecontract.TemplateVersion
	pages     []*page // live contents sorted by full_path
	dupGroups map[primitive.ObjectID][]VersionDuplicate
	tmplFail  map[primitive.ObjectID]string
	rep       Report
}

// DryRun analyzes without writing (see package doc).
func (m *Migrator) DryRun(ctx context.Context) (Report, error) {
	a, err := m.analyze(ctx)
	if err != nil {
		return Report{}, err
	}
	a.rep.DryRun = true
	// Per-page file/render reconciliation, read-only.
	for _, p := range a.pages {
		m.reconcileReadOnly(ctx, a, p)
	}
	sortReport(a)
	a.rep.MigrationTo = a.flag // unchanged
	return a.rep, nil
}

// Run executes the apply pass.
func (m *Migrator) Run(ctx context.Context) (Report, error) {
	flag := readFlag(ctx, m.db)
	if flag == StatusCompleted {
		// Idempotent no-op: report completion without touching anything.
		return Report{MigrationFrom: flag, MigrationTo: flag, Completed: true}, nil
	}
	a, err := m.analyze(ctx)
	if err != nil {
		return Report{}, err
	}
	a.rep.MigrationFrom = flag

	// 1. Flag not_started→running (resume keeps running).
	if err := writeFlag(ctx, m.db, StatusRunning, nil); err != nil {
		return a.rep, err
	}
	a.rep.MigrationTo = StatusRunning

	// 2. Template backfill: CurrentVersion=1 + v1 doc where missing (§35.2).
	m.backfillTemplates(ctx, a)

	// 3. Content backfill: canonical fields + CurrentVersion (§12.8, §13).
	m.backfillContents(ctx, a)

	// 4. Page-by-page reconciliation (resume-safe: actives are skipped).
	for _, p := range a.pages {
		if err := m.reconcileApply(ctx, a, p); err != nil {
			// Persistence failures are recorded as blocking render errors;
			// the loop continues so one bad page never blocks unrelated ones.
			a.rep.RenderErrors = append(a.rep.RenderErrors, PageItem{
				ContentID: p.content.ID.Hex(), FullPath: p.content.FullPath,
				Canonical: p.canonical, Detail: "migrate: " + err.Error(),
			})
		}
	}
	sortReport(a)

	// 5. Completion gate: every blocker must be zero.
	if n := a.rep.BlockingCount(); n > 0 {
		_ = writeFlag(ctx, m.db, StatusRunning, flagSummary(a))
		return a.rep, fmt.Errorf("%w: %d blocking items remain", ErrMigrationBlocked, n)
	}

	// 6. Collision remediation is done (zero collisions above): create the
	// new index set via Task 2. Any failure keeps the old index and aborts.
	if err := m.db.EnsureProductIndexes(ctx); err != nil {
		_ = writeFlag(ctx, m.db, StatusRunning, flagSummary(a))
		return a.rep, err
	}

	// 7. Verify the new canonical index before touching the legacy one.
	ok, err := hasIndexName(ctx, m.db, "content", canonicalIndexName)
	if err != nil {
		_ = writeFlag(ctx, m.db, StatusRunning, flagSummary(a))
		return a.rep, err
	}
	if !ok {
		_ = writeFlag(ctx, m.db, StatusRunning, flagSummary(a))
		return a.rep, errors.New("migration: new canonical index " + canonicalIndexName + " not found after EnsureProductIndexes")
	}
	a.rep.NewIndexesVerified = true

	// 8. Only now drop the old (full_path, fork_id) index so deleted paths
	// can be reused under the new path_active rule. Absent → already done.
	if err := dropIndex(ctx, m.db, "content", legacyIndexName); err != nil {
		_ = writeFlag(ctx, m.db, StatusRunning, flagSummary(a))
		return a.rep, err
	}
	a.rep.LegacyIndexDropped = true

	// 9. Final gate: every serving (published, live) canonical must have an
	// active record. With zero missing/render errors this holds; assert it.
	if missing := m.assertAllServingHaveActive(ctx, a); len(missing) > 0 {
		for _, p := range missing {
			a.rep.MissingFiles = append(a.rep.MissingFiles, PageItem{
				ContentID: p.content.ID.Hex(), FullPath: p.content.FullPath,
				Canonical: p.canonical, Detail: "final gate: serving canonical has no active publication",
			})
		}
		_ = writeFlag(ctx, m.db, StatusRunning, flagSummary(a))
		return a.rep, errors.Join(ErrMigrationBlocked, errors.New("final gate: serving pages without active records"))
	}

	// 10. Mark completed with summary counts (the durable migration audit).
	a.rep.Completed = true
	a.rep.MigrationTo = StatusCompleted
	if err := writeFlag(ctx, m.db, StatusCompleted, flagSummary(a)); err != nil {
		return a.rep, err
	}
	return a.rep, nil
}

// analyze loads templates, template v1 docs, live contents, version stats,
// and computes the blocking preconditions (slug issues, collisions,
// version duplicates). The flag is only READ here (writes happen in Run).
func (m *Migrator) analyze(ctx context.Context) (*analysis, error) {
	a := &analysis{
		now:       time.Now(),
		tmplByID:  map[primitive.ObjectID]models.Template{},
		v1ByTmpl:  map[primitive.ObjectID]templatecontract.TemplateVersion{},
		dupGroups: map[primitive.ObjectID][]VersionDuplicate{},
		tmplFail:  map[primitive.ObjectID]string{},
	}
	a.flag = readFlag(ctx, m.db)
	a.rep.MigrationFrom = a.flag
	a.rep.MigrationTo = a.flag

	// Templates (all, sorted by slug for determinism).
	cur, err := m.db.Collection("templates").Find(ctx, bson.M{},
		options.Find().SetSort(bson.D{{Key: "slug", Value: 1}, {Key: "name", Value: 1}}))
	if err != nil {
		return nil, err
	}
	if err := cur.All(ctx, &a.templates); err != nil {
		_ = cur.Close(ctx)
		return nil, err
	}
	_ = cur.Close(ctx)
	for _, t := range a.templates {
		a.tmplByID[t.ID] = t
	}
	a.checkTemplateSlugs()

	// Template v1 docs present in DB (backfill adds the rest in apply).
	vcur, err := m.db.Collection("template_versions").Find(ctx, bson.M{"version": int64(1)})
	if err != nil {
		return nil, err
	}
	var v1s []templatecontract.TemplateVersion
	if err := vcur.All(ctx, &v1s); err != nil {
		_ = vcur.Close(ctx)
		return nil, err
	}
	_ = vcur.Close(ctx)
	for _, v := range v1s {
		a.v1ByTmpl[v.TemplateID] = v
	}

	// Live contents (fork copies excluded: fork_id nil covers missing+null).
	ccur, err := m.db.Collection("content").Find(ctx, bson.M{"fork_id": nil},
		options.Find().SetSort(bson.D{{Key: "full_path", Value: 1}}))
	if err != nil {
		return nil, err
	}
	var contents []models.Content
	if err := ccur.All(ctx, &contents); err != nil {
		_ = ccur.Close(ctx)
		return nil, err
	}
	_ = ccur.Close(ctx)

	maxByContent, err := maxContentVersions(ctx, m.db)
	if err != nil {
		return nil, err
	}
	dups, err := duplicateContentVersions(ctx, m.db)
	if err != nil {
		return nil, err
	}
	for _, d := range dups {
		cid, _ := primitive.ObjectIDFromHex(d.ContentID)
		a.dupGroups[cid] = append(a.dupGroups[cid], d)
	}

	for _, c := range contents {
		p := &page{content: c}
		if v, ok := maxByContent[c.ID]; ok && v > 0 {
			p.maxVer = v
		} else {
			p.maxVer = 1 // genesis when no history exists
		}
		canon, cerr := pathkey.Canonical(c.FullPath)
		if cerr != nil {
			p.invalid = cerr.Error()
			a.rep.InvalidPaths = append(a.rep.InvalidPaths, PageItem{
				ContentID: c.ID.Hex(), FullPath: c.FullPath, Detail: p.invalid,
			})
		} else {
			p.canonical = canon
		}
		a.pages = append(a.pages, p)
	}
	a.checkCanonicalCollisions()
	for _, d := range dups {
		a.rep.VersionDuplicates = append(a.rep.VersionDuplicates, d)
	}
	sort.Slice(a.rep.VersionDuplicates, func(i, j int) bool {
		if a.rep.VersionDuplicates[i].ContentID != a.rep.VersionDuplicates[j].ContentID {
			return a.rep.VersionDuplicates[i].ContentID < a.rep.VersionDuplicates[j].ContentID
		}
		return a.rep.VersionDuplicates[i].Version < a.rep.VersionDuplicates[j].Version
	})
	return a, nil
}

// checkTemplateSlugs reports empty/invalid/duplicate/case-conflicting slugs
// (§34.1, §35.1) using Task 3's ValidateSlug — never redefined here.
// Conflicts need admin repair; nothing is silently renamed.
func (a *analysis) checkTemplateSlugs() {
	seen := map[string][]models.Template{}
	folded := map[string][]models.Template{}
	for _, t := range a.templates {
		if strings.TrimSpace(t.Slug) == "" {
			a.rep.InvalidSlugs = append(a.rep.InvalidSlugs, SlugIssue{
				TemplateID: t.ID.Hex(), TemplateName: t.Name,
				Reason: "empty slug",
			})
			continue
		}
		if err := templatecontract.ValidateSlug(t.Slug); err != nil {
			a.rep.InvalidSlugs = append(a.rep.InvalidSlugs, SlugIssue{
				TemplateID: t.ID.Hex(), TemplateName: t.Name,
				Slug: t.Slug, Reason: "invalid slug: " + err.Error(),
			})
		}
		seen[t.Slug] = append(seen[t.Slug], t)
		folded[strings.ToLower(t.Slug)] = append(folded[strings.ToLower(t.Slug)], t)
	}
	for slug, group := range seen {
		if len(group) < 2 {
			continue
		}
		for _, t := range group {
			a.rep.InvalidSlugs = append(a.rep.InvalidSlugs, SlugIssue{
				TemplateID: t.ID.Hex(), TemplateName: t.Name,
				Slug: slug, Reason: "duplicate slug: shared by multiple templates",
			})
		}
	}
	for fold, group := range folded {
		if len(group) < 2 {
			continue
		}
		distinct := map[string]bool{}
		for _, t := range group {
			distinct[t.Slug] = true
		}
		if len(distinct) < 2 {
			continue // already reported as exact duplicates above
		}
		for _, t := range group {
			a.rep.InvalidSlugs = append(a.rep.InvalidSlugs, SlugIssue{
				TemplateID: t.ID.Hex(), TemplateName: t.Name,
				Slug: t.Slug, Reason: "case-fold collision on slug " + fold,
			})
		}
	}
	sort.Slice(a.rep.InvalidSlugs, func(i, j int) bool {
		if a.rep.InvalidSlugs[i].TemplateID != a.rep.InvalidSlugs[j].TemplateID {
			return a.rep.InvalidSlugs[i].TemplateID < a.rep.InvalidSlugs[j].TemplateID
		}
		return a.rep.InvalidSlugs[i].Reason < a.rep.InvalidSlugs[j].Reason
	})
}

// checkCanonicalCollisions groups non-deleted live contents by canonical key;
// groups with >1 member block the new unique index and page reconciliation.
// (Deleted rows backfill path_active=false and never compete.)
func (a *analysis) checkCanonicalCollisions() {
	groups := map[string][]*page{}
	for _, p := range a.pages {
		if p.invalid != "" || p.content.Deleted {
			continue
		}
		groups[p.canonical] = append(groups[p.canonical], p)
	}
	for canon, g := range groups {
		if len(g) < 2 {
			continue
		}
		sort.Slice(g, func(i, j int) bool { return g[i].content.FullPath < g[j].content.FullPath })
		col := Collision{Canonical: canon}
		for _, p := range g {
			p.collided = true
			col.FullPaths = append(col.FullPaths, p.content.FullPath)
			col.ContentIDs = append(col.ContentIDs, p.content.ID.Hex())
		}
		a.rep.CanonicalCollisions = append(a.rep.CanonicalCollisions, col)
	}
	sort.Slice(a.rep.CanonicalCollisions, func(i, j int) bool {
		return a.rep.CanonicalCollisions[i].Canonical < a.rep.CanonicalCollisions[j].Canonical
	})
}

// serving reports whether the page is expected to serve a canonical file:
// published, live, not soft-deleted.
func serving(p *page) bool {
	return p.content.Published && !p.content.Deleted
}

// reconcilable reports whether file/render reconciliation may proceed:
// serving, valid path, and not in a canonical collision. Version duplicates
// do NOT skip the page (index-level blocker only); invalid-slug templates
// do not skip either (their v1 still renders).
func reconcilable(p *page) bool {
	return serving(p) && p.invalid == "" && !p.collided
}

// resolveTemplate returns the immutable v1 for the page's template: the
// persisted doc when present, else an in-memory synthesis (dry-run before
// backfill). The boolean reports existence of the persisted doc.
func (a *analysis) resolveTemplate(p *page) (templatecontract.TemplateVersion, bool) {
	if v, ok := a.v1ByTmpl[p.content.TemplateID]; ok {
		return v, true
	}
	t, ok := a.tmplByID[p.content.TemplateID]
	if !ok {
		return templatecontract.TemplateVersion{}, false
	}
	v := buildV1(t, a.now)
	// Synthesis marker only: dry-run renders against this in-memory v1
	// (never persisted — apply backfills the real doc first). PlanSnapshot
	// requires a non-zero version ID.
	v.ID = primitive.NewObjectID()
	return v, false
}

// renderInput freezes the deterministic render input for a page.
func (m *Migrator) renderInput(a *analysis, p *page, tmpl templatecontract.TemplateVersion, pubID primitive.ObjectID) RenderInput {
	content := p.content
	if content.CurrentVersion < 1 {
		content.CurrentVersion = p.maxVer
	}
	logicalAt := content.PublishedAt
	var lat time.Time
	if logicalAt != nil {
		lat = *logicalAt
	} else {
		lat = a.now
	}
	return RenderInput{
		Content: content, Template: tmpl,
		PublicationID: pubID, LogicalAt: lat,
		PublicURL: m.publicURL(content.FullPath),
	}
}

// reconcileReadOnly performs the dry-run per-page file/render comparison.
// No writes of any kind.
func (m *Migrator) reconcileReadOnly(ctx context.Context, a *analysis, p *page) {
	if !reconcilable(p) {
		return
	}
	if msg, ok := a.tmplFail[p.content.TemplateID]; ok {
		a.rep.RenderErrors = append(a.rep.RenderErrors, PageItem{
			ContentID: p.content.ID.Hex(), FullPath: p.content.FullPath,
			Canonical: p.canonical, Detail: "template backfill: " + msg,
		})
		return
	}
	tmpl, persisted := a.resolveTemplate(p)
	if !persisted {
		if _, ok := a.tmplByID[p.content.TemplateID]; !ok {
			a.rep.RenderErrors = append(a.rep.RenderErrors, PageItem{
				ContentID: p.content.ID.Hex(), FullPath: p.content.FullPath,
				Canonical: p.canonical, Detail: "unknown template " + p.content.TemplateID.Hex(),
			})
			return
		}
	}
	info, err := m.store.Inspect(ctx, p.content.FullPath)
	if err != nil {
		a.rep.RenderErrors = append(a.rep.RenderErrors, PageItem{
			ContentID: p.content.ID.Hex(), FullPath: p.content.FullPath,
			Canonical: p.canonical, Detail: "inspect: " + err.Error(),
		})
		return
	}
	if !info.Exists {
		a.rep.MissingFiles = append(a.rep.MissingFiles, PageItem{
			ContentID: p.content.ID.Hex(), FullPath: p.content.FullPath,
			Canonical: p.canonical, Detail: "canonical file absent; no active record created",
		})
		return
	}
	rendered, hash, err := m.render(ctx, m.renderInput(a, p, tmpl, primitive.NewObjectID()))
	if err != nil {
		a.rep.RenderErrors = append(a.rep.RenderErrors, PageItem{
			ContentID: p.content.ID.Hex(), FullPath: p.content.FullPath,
			Canonical: p.canonical, Detail: "render: " + err.Error(),
		})
		return
	}
	if strings.EqualFold(stripHash(hash), strings.ToLower(info.SHA256)) {
		a.rep.Verified = append(a.rep.Verified, PageItem{
			ContentID: p.content.ID.Hex(), FullPath: p.content.FullPath,
			Canonical: p.canonical, ContentVersion: contentVersionOf(p),
			TemplateVersion: 1, ContentHash: withPrefix(hash),
		})
		return
	}
	a.rep.LegacyUnverified = append(a.rep.LegacyUnverified, PageItem{
		ContentID: p.content.ID.Hex(), FullPath: p.content.FullPath,
		Canonical: p.canonical, ContentVersion: contentVersionOf(p),
		TemplateVersion: 1, ContentHash: "sha256:" + strings.ToLower(info.SHA256),
		Detail: fmt.Sprintf("canonical sha256:%s != fresh render %s; dry-run would import legacy bytes",
			strings.ToLower(info.SHA256), withPrefix(hash)),
	})
	_ = rendered
}

// reconcileApply migrates one page (resume-safe). Report entries are
// appended for every outcome; a non-nil error is an apply-time persistence
// failure the caller records as blocking.
func (m *Migrator) reconcileApply(ctx context.Context, a *analysis, p *page) error {
	if !reconcilable(p) {
		return nil
	}
	if msg, ok := a.tmplFail[p.content.TemplateID]; ok {
		a.rep.RenderErrors = append(a.rep.RenderErrors, PageItem{
			ContentID: p.content.ID.Hex(), FullPath: p.content.FullPath,
			Canonical: p.canonical, Detail: "template backfill: " + msg,
		})
		return nil
	}
	// Resume: an existing active record means a previous pass migrated this
	// page — skip, never duplicate.
	if active, err := m.repo.GetActive(ctx, p.content.ID); err != nil {
		return fmt.Errorf("get active: %w", err)
	} else if active != nil {
		a.rep.SkippedActive = append(a.rep.SkippedActive, PageItem{
			ContentID: p.content.ID.Hex(), FullPath: p.content.FullPath,
			Canonical: p.canonical, PublicationID: active.ID.Hex(),
			ContentHash: active.ContentHash,
			Detail:      "already migrated; reused existing active " + string(active.VerificationStatus),
		})
		return nil
	}
	tmpl, persisted := a.resolveTemplate(p)
	if !persisted {
		if _, ok := a.tmplByID[p.content.TemplateID]; !ok {
			a.rep.RenderErrors = append(a.rep.RenderErrors, PageItem{
				ContentID: p.content.ID.Hex(), FullPath: p.content.FullPath,
				Canonical: p.canonical, Detail: "unknown template " + p.content.TemplateID.Hex(),
			})
			return nil
		}
		// Backfill should have persisted v1; fall back to the synthesis.
	}
	info, err := m.store.Inspect(ctx, p.content.FullPath)
	if err != nil {
		a.rep.RenderErrors = append(a.rep.RenderErrors, PageItem{
			ContentID: p.content.ID.Hex(), FullPath: p.content.FullPath,
			Canonical: p.canonical, Detail: "inspect: " + err.Error(),
		})
		return nil
	}
	if !info.Exists {
		// Spec §35.3 matrix: canonical absent → NO active record, blocking.
		a.rep.MissingFiles = append(a.rep.MissingFiles, PageItem{
			ContentID: p.content.ID.Hex(), FullPath: p.content.FullPath,
			Canonical: p.canonical, Detail: "canonical file absent; no active record created",
		})
		return nil
	}
	pubID := primitive.NewObjectID()
	rendered, hash, err := m.render(ctx, m.renderInput(a, p, tmpl, pubID))
	if err != nil {
		// Spec §35.3 matrix: render failure → old canonical kept, blocking.
		a.rep.RenderErrors = append(a.rep.RenderErrors, PageItem{
			ContentID: p.content.ID.Hex(), FullPath: p.content.FullPath,
			Canonical: p.canonical, Detail: "render: " + err.Error(),
		})
		return nil
	}
	canonBytes, err := m.readCanonicalBytes(p.content.FullPath)
	if err != nil {
		a.rep.RenderErrors = append(a.rep.RenderErrors, PageItem{
			ContentID: p.content.ID.Hex(), FullPath: p.content.FullPath,
			Canonical: p.canonical, Detail: "read canonical: " + err.Error(),
		})
		return nil
	}
	if strings.EqualFold(stripHash(hash), shaHex(canonBytes)) {
		return m.activatePage(ctx, a, p, tmpl, pubID, rendered, withPrefix(hash), publication.VerificationVerified)
	}
	// Spec §35.3 matrix: hash differs → copy old bytes to an immutable
	// legacy object, create active legacy_unverified, leave the canonical
	// serving. Never silently mark the old bytes verified.
	if err := m.activatePage(ctx, a, p, tmpl, pubID, canonBytes,
		"sha256:"+shaHex(canonBytes), publication.VerificationLegacyUnverified); err != nil {
		return err
	}
	return nil
}

// activatePage stages bytes as the immutable object, verifies them, inserts
// the staged record, and CAS-activates it (expecting no prior active — the
// resume check above guarantees it; a conflict re-checks and skips).
func (m *Migrator) activatePage(ctx context.Context, a *analysis, p *page, tmpl templatecontract.TemplateVersion,
	pubID primitive.ObjectID, body []byte, hash string, verification publication.VerificationStatus) error {
	rawHex := stripHash(hash)
	staged, err := m.store.Stage(ctx, storage.StageRequest{
		ContentID: p.content.ID, PublicationID: pubID,
		CanonicalPath: p.content.FullPath, HTML: body, ExpectedSHA256: rawHex,
	})
	if err != nil {
		return fmt.Errorf("stage immutable object: %w", err)
	}
	if err := m.store.Verify(ctx, staged); err != nil {
		return fmt.Errorf("verify immutable object: %w", err)
	}
	logicalAt := p.content.PublishedAt
	var lat time.Time
	if logicalAt != nil {
		lat = *logicalAt
	} else {
		lat = a.now
	}
	pub := &publication.Publication{
		ID: pubID, ContentID: p.content.ID,
		ContentVersion:     contentVersionOf(p),
		TemplateID:         p.content.TemplateID,
		TemplateVersionID:  tmpl.ID,
		TemplateVersion:    tmpl.Version,
		FullPath:           p.content.FullPath,
		ContentHash:        hash,
		StorageProvider:    "filesystem",
		StoragePath:        m.store.ImmutablePath(p.content.ID, pubID),
		StorageState:       publication.StoragePresent,
		Status:             publication.StatusStaged,
		VerificationStatus: verification,
		LogicalPublishedAt: lat,
		Actor:              "system",
		Via:                "migration",
		CreatedAt:          a.now,
		RendererVersion:    publication.RendererVersion,
		ProductBuildSHA:    publication.ProductBuildSHA,
	}
	if err := m.repo.InsertStaged(ctx, pub); err != nil {
		return fmt.Errorf("insert staged publication: %w", err)
	}
	if err := m.repo.ActivateCAS(ctx, p.content.ID, pubID, nil); err != nil {
		if publication.IsConflict(err) {
			// Lost race (or resume overlap): reuse the winner, never duplicate.
			if active, gerr := m.repo.GetActive(ctx, p.content.ID); gerr == nil && active != nil {
				a.rep.SkippedActive = append(a.rep.SkippedActive, PageItem{
					ContentID: p.content.ID.Hex(), FullPath: p.content.FullPath,
					Canonical: p.canonical, PublicationID: active.ID.Hex(),
					ContentHash: active.ContentHash, Detail: "activation race; reused existing active",
				})
				return nil
			}
		}
		return fmt.Errorf("activate publication: %w", err)
	}
	item := PageItem{
		ContentID: p.content.ID.Hex(), FullPath: p.content.FullPath,
		Canonical: p.canonical, ContentVersion: pub.ContentVersion,
		TemplateVersion: tmpl.Version, PublicationID: pubID.Hex(), ContentHash: hash,
	}
	if verification == publication.VerificationVerified {
		a.rep.Verified = append(a.rep.Verified, item)
	} else {
		item.Detail = "canonical differed from fresh render; legacy bytes imported, canonical still serving"
		a.rep.LegacyUnverified = append(a.rep.LegacyUnverified, item)
	}
	return nil
}

// backfillTemplates creates Version 1 + CurrentVersion=1 for pre-V3
// templates (§35.2), idempotently. Hashes come from Task 3; invalid-slug
// templates are still backfilled (render needs a v1) but stay blocking via
// the slug report. Per-template failures are recorded in tmplFail so the
// affected pages report render errors instead of silently skipping.
func (m *Migrator) backfillTemplates(ctx context.Context, a *analysis) {
	for _, t := range a.templates {
		if _, ok := a.v1ByTmpl[t.ID]; ok {
			if t.CurrentVersion <= 0 {
				_, _ = m.db.Collection("templates").UpdateOne(ctx,
					bson.M{"_id": t.ID, "current_version": bson.M{"$lte": int64(0)}},
					bson.M{"$set": bson.M{"current_version": int64(1)}})
			}
			continue
		}
		ver := buildV1(t, a.now)
		res, err := m.db.Collection("template_versions").InsertOne(ctx, &ver)
		if err != nil {
			if isDupKey(err) {
				// Lost race / resume overlap: reload the winner.
				var existing templatecontract.TemplateVersion
				if rerr := m.db.Collection("template_versions").FindOne(ctx,
					bson.M{"template_id": t.ID, "version": int64(1)}).Decode(&existing); rerr == nil {
					a.v1ByTmpl[t.ID] = existing
					continue
				}
			}
			a.tmplFail[t.ID] = "insert version 1: " + err.Error()
			continue
		}
		// The driver does not write the generated _id back into the struct:
		// capture it explicitly (same pattern as Task 3 Create).
		if id, ok := res.InsertedID.(primitive.ObjectID); ok {
			ver.ID = id
		}
		a.v1ByTmpl[t.ID] = ver
		if t.CurrentVersion <= 0 {
			_, _ = m.db.Collection("templates").UpdateOne(ctx,
				bson.M{"_id": t.ID, "current_version": bson.M{"$lte": int64(0)}},
				bson.M{"$set": bson.M{"current_version": int64(1)}})
		}
	}
}

// backfillContents writes canonical path fields + CurrentVersion for live
// contents with valid paths (§12.8, §13), idempotently. Failures are
// recorded as blocking render errors on the affected page.
func (m *Migrator) backfillContents(ctx context.Context, a *analysis) {
	for _, p := range a.pages {
		if p.invalid != "" {
			continue
		}
		set := bson.M{
			"canonical_full_path": p.canonical,
			"path_scope":          "live",
			"path_active":         !p.content.Deleted,
		}
		wantVer := p.content.CurrentVersion
		if wantVer <= 0 {
			wantVer = p.maxVer
			set["current_version"] = wantVer
		}
		if _, err := m.db.Collection("content").UpdateOne(ctx,
			bson.M{"_id": p.content.ID}, bson.M{"$set": set}); err != nil {
			a.rep.RenderErrors = append(a.rep.RenderErrors, PageItem{
				ContentID: p.content.ID.Hex(), FullPath: p.content.FullPath,
				Canonical: p.canonical, Detail: "backfill: " + err.Error(),
			})
			continue
		}
		p.content.CanonicalFullPath = p.canonical
		p.content.PathScope = "live"
		p.content.PathActive = !p.content.Deleted
		if p.content.CurrentVersion <= 0 {
			p.content.CurrentVersion = wantVer
		}
	}
}

// assertAllServingHaveActive returns serving-expected pages with no active
// publication (final gate before completion: every serving canonical must
// have an active record).
func (m *Migrator) assertAllServingHaveActive(ctx context.Context, a *analysis) []*page {
	var missing []*page
	for _, p := range a.pages {
		if !reconcilable(p) {
			continue
		}
		active, err := m.repo.GetActive(ctx, p.content.ID)
		if err != nil || active == nil {
			missing = append(missing, p)
		}
	}
	return missing
}

// buildV1 synthesizes the initial immutable TemplateVersion for a pre-V3
// template (§35.2) with Task 3's canonical hashes.
func buildV1(t models.Template, now time.Time) templatecontract.TemplateVersion {
	in := templatecontract.TemplateInput{
		Slug: t.Slug, Name: t.Name, Category: t.Category,
		Status:       templatecontract.NormalizeStatus(t.Status),
		HTMLLayout:   t.HTMLLayout,
		ScriptPolicy: "",
		Fields:       t.Fields,
	}
	return templatecontract.TemplateVersion{
		TemplateID: t.ID, Version: 1,
		Slug: t.Slug, Name: t.Name, Category: t.Category,
		Status:       in.Status,
		Fields:       t.Fields,
		HTMLLayout:   t.HTMLLayout,
		ScriptPolicy: "",
		ContractHash: templatecontract.ContractHash(in),
		RenderHash:   templatecontract.RenderHash(in),
		CreatedAt:    now,
	}
}

// contentVersionOf resolves the publication content version: the backfilled
// CurrentVersion, else the max history version, else genesis 1.
func contentVersionOf(p *page) int64 {
	if p.content.CurrentVersion > 0 {
		return p.content.CurrentVersion
	}
	if p.maxVer > 0 {
		return p.maxVer
	}
	return 1
}

// withPrefix ensures "sha256:<hex>" record form.
func withPrefix(h string) string {
	if strings.HasPrefix(strings.ToLower(h), "sha256:") {
		return h
	}
	return "sha256:" + h
}

// isDupKey reports Mongo duplicate-key errors without driver coupling.
func isDupKey(err error) bool {
	if err == nil {
		return false
	}
	if mongo.IsDuplicateKeyError(err) {
		return true
	}
	return strings.Contains(err.Error(), "duplicate key")
}

// sortReport orders every list deterministically for stable reports.
func sortReport(a *analysis) {
	byPath := func(x, y PageItem) bool {
		if x.FullPath != y.FullPath {
			return x.FullPath < y.FullPath
		}
		return x.ContentID < y.ContentID
	}
	sort.Slice(a.rep.InvalidPaths, func(i, j int) bool { return byPath(a.rep.InvalidPaths[i], a.rep.InvalidPaths[j]) })
	sort.Slice(a.rep.MissingFiles, func(i, j int) bool { return byPath(a.rep.MissingFiles[i], a.rep.MissingFiles[j]) })
	sort.Slice(a.rep.RenderErrors, func(i, j int) bool { return byPath(a.rep.RenderErrors[i], a.rep.RenderErrors[j]) })
	sort.Slice(a.rep.Verified, func(i, j int) bool { return byPath(a.rep.Verified[i], a.rep.Verified[j]) })
	sort.Slice(a.rep.LegacyUnverified, func(i, j int) bool {
		return byPath(a.rep.LegacyUnverified[i], a.rep.LegacyUnverified[j])
	})
	sort.Slice(a.rep.SkippedActive, func(i, j int) bool {
		return byPath(a.rep.SkippedActive[i], a.rep.SkippedActive[j])
	})
}

// readFlag mirrors the Task 10 scanner's lenient read: any doc identifying
// publication_model_v1 via _id/key/name, state from status/state/value.
// Absent/unparseable → "" (not started; quarantine gate shut).
func readFlag(ctx context.Context, db *database.DB) string {
	var doc bson.M
	err := db.Collection(CollectionMigrations).FindOne(ctx, bson.M{
		"$or": []bson.M{
			{"_id": MigrationKey},
			{"key": MigrationKey},
			{"name": MigrationKey},
		},
	}).Decode(&doc)
	if err != nil {
		return ""
	}
	for _, k := range []string{"status", "state", "value"} {
		if v, ok := doc[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// writeFlag upserts {_id, key}=MigrationKey with status=state. summary, when
// non-nil, stores the audit counts on the flag doc itself (durable,
// queryable migration audit without fabricating user attribution).
func writeFlag(ctx context.Context, db *database.DB, state string, summary map[string]any) error {
	set := bson.M{"key": MigrationKey, "status": state, "updated_at": time.Now()}
	for k, v := range summary {
		set[k] = v
	}
	_, err := db.Collection(CollectionMigrations).UpdateOne(ctx,
		bson.M{"_id": MigrationKey},
		bson.M{"$set": set},
		options.Update().SetUpsert(true),
	)
	return err
}

// flagSummary stores report counts on the flag doc for the migration audit.
func flagSummary(a *analysis) map[string]any {
	return map[string]any{
		"invalid_slugs":        len(a.rep.InvalidSlugs),
		"canonical_collisions": len(a.rep.CanonicalCollisions),
		"version_duplicates":   len(a.rep.VersionDuplicates),
		"invalid_paths":        len(a.rep.InvalidPaths),
		"missing_files":        len(a.rep.MissingFiles),
		"render_errors":        len(a.rep.RenderErrors),
		"verified":             len(a.rep.Verified),
		"legacy_unverified":    len(a.rep.LegacyUnverified),
		"skipped_active":       len(a.rep.SkippedActive),
		"completed":            a.rep.Completed,
		"legacy_index_dropped": a.rep.LegacyIndexDropped,
		"new_indexes_verified": a.rep.NewIndexesVerified,
	}
}

// maxContentVersions returns content_id → max version across history.
// Mixed legacy int types are normalized numerically.
func maxContentVersions(ctx context.Context, db *database.DB) (map[primitive.ObjectID]int64, error) {
	out := map[primitive.ObjectID]int64{}
	cur, err := db.Collection("content_versions").Aggregate(ctx, mongo.Pipeline{
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$content_id"},
			{Key: "maxv", Value: bson.D{{Key: "$max", Value: "$version"}}},
		}}},
	})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	for cur.Next(ctx) {
		var row struct {
			ID   primitive.ObjectID `bson:"_id"`
			MaxV any                `bson:"maxv"`
		}
		if err := cur.Decode(&row); err != nil {
			continue
		}
		if v := toInt64(row.MaxV); v > 0 {
			out[row.ID] = v
		}
	}
	return out, cur.Err()
}

// duplicateContentVersions returns (content_id, version) groups with count>1.
func duplicateContentVersions(ctx context.Context, db *database.DB) ([]VersionDuplicate, error) {
	var out []VersionDuplicate
	cur, err := db.Collection("content_versions").Aggregate(ctx, mongo.Pipeline{
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: bson.D{
				{Key: "content_id", Value: "$content_id"},
				{Key: "version", Value: "$version"},
			}},
			{Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
		{{Key: "$match", Value: bson.D{{Key: "count", Value: bson.D{{Key: "$gt", Value: 1}}}}}},
	})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	for cur.Next(ctx) {
		var row struct {
			ID struct {
				ContentID primitive.ObjectID `bson:"content_id"`
				Version   any                `bson:"version"`
			} `bson:"_id"`
			Count int64 `bson:"count"`
		}
		if err := cur.Decode(&row); err != nil {
			continue
		}
		out = append(out, VersionDuplicate{
			ContentID: row.ID.ContentID.Hex(), Version: toInt64(row.ID.Version), Count: row.Count,
		})
	}
	return out, cur.Err()
}

// toInt64 normalizes legacy mixed numeric version types.
func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int32:
		return int64(n)
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}

// hasIndexName reports whether collection has an index with the given name.
func hasIndexName(ctx context.Context, db *database.DB, collection, name string) (bool, error) {
	cur, err := db.Collection(collection).Indexes().List(ctx)
	if err != nil {
		return false, err
	}
	defer cur.Close(ctx)
	for cur.Next(ctx) {
		var spec bson.M
		if err := cur.Decode(&spec); err != nil {
			continue
		}
		if n, _ := spec["name"].(string); n == name {
			return true, nil
		}
	}
	return false, cur.Err()
}

// dropIndex removes a legacy index; "index not found" is success (idempotent).
func dropIndex(ctx context.Context, db *database.DB, collection, name string) error {
	if _, err := db.Collection(collection).Indexes().DropOne(ctx, name); err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "index not found") || strings.Contains(msg, "ns not found") {
			return nil
		}
		return err
	}
	return nil
}

// shaHex returns the lowercase hex SHA-256 of b (storage-layer raw form).
func shaHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// stripHash converts "sha256:<hex>" record form to raw hex.
func stripHash(h string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(h), "sha256:")))
}

// publicURL resolves the frozen public URL for a page.
func (m *Migrator) publicURL(fullPath string) string {
	if m.base != "" {
		return strings.TrimRight(m.base, "/") + fullPath
	}
	return fullPath
}

// readCanonicalBytes loads the serving canonical bytes for a full path.
func (m *Migrator) readCanonicalBytes(fullPath string) ([]byte, error) {
	p, err := m.store.CanonicalFilePath(fullPath)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}
