package publication

// Saga mechanics: page locks, deterministic rendering, plan assembly and the
// shared stage → verify → cutover → commit → compensate pipeline.
//
// Locking (spec §12.8, §16.5): one operation takes content:{id} plus every
// canonical path it touches (rename: old + new), sorted byte-wise and
// released in reverse. Contention is a non-blocking 409
// PAGE_PUBLISH_IN_PROGRESS. The DB unique indexes stay the final arbiter;
// locks only serialize the filesystem cutover on this instance.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/observe"
	"github.com/jonradoff/lightcms/v7/internal/product/idempotency"
	"github.com/jonradoff/lightcms/v7/internal/product/pathkey"
	"github.com/jonradoff/lightcms/v7/internal/product/storage"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// --- locks ---

type sagaLockRegistry struct {
	mu   sync.Mutex
	sems map[string]chan struct{}
}

var sagaLocks = &sagaLockRegistry{sems: map[string]chan struct{}{}}

// lockPathsFor normalizes candidate paths to canonical lock keys, dedupes,
// and sorts them for stable acquisition order.
func lockPathsFor(paths ...string) []string {
	seen := map[string]struct{}{}
	for _, p := range paths {
		if p == "" {
			continue
		}
		key := p
		if canon, err := pathkey.Canonical(p); err == nil {
			key = canon
		}
		seen["path:"+key] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// acquireSagaLocks try-acquires content + path locks in stable order.
// ok=false means another saga holds the page (409 upstream).
func acquireSagaLocks(contentID primitive.ObjectID, pathKeys []string) (release func(), ok bool) {
	keys := append([]string{"content:" + contentID.Hex()}, pathKeys...)
	sort.Strings(keys)

	sagaLocks.mu.Lock()
	sems := make([]chan struct{}, len(keys))
	for i, k := range keys {
		sem, exists := sagaLocks.sems[k]
		if !exists {
			sem = make(chan struct{}, 1)
			sagaLocks.sems[k] = sem
		}
		sems[i] = sem
	}
	sagaLocks.mu.Unlock()

	acquired := sems[:0]
	for _, sem := range sems {
		select {
		case sem <- struct{}{}:
			acquired = append(acquired, sem)
		default:
			for i := len(acquired) - 1; i >= 0; i-- {
				<-acquired[i]
			}
			return nil, false
		}
	}
	return func() {
		for i := len(acquired) - 1; i >= 0; i-- {
			<-acquired[i]
		}
	}, true
}

// activePath returns the frozen serving path of the current active record.
func activePath(active *Publication) string {
	if active == nil {
		return ""
	}
	return active.FullPath
}

// --- rendering ---

// RenderInput is the frozen render snapshot for one attempt: immutable
// template bytes + content data + publication identity. Re-rendering the same
// input yields identical bytes (Task 7 consumes this shape when it lands;
// until then the saga owns the default renderer).
type RenderInput struct {
	Content            models.Content
	Template           templatecontract.TemplateVersion
	PublicationID      primitive.ObjectID
	ContentVersion     int64
	TemplateVersionNum int64
	LogicalPublishedAt time.Time
	PublicURL          string
	// AuthorIsAdmin resolves policy=admin_only at plan time: admins render
	// raw, everyone else renders strict. Frozen into the snapshot so
	// takeover replays the same decision. Zero value (false) is fail-closed.
	AuthorIsAdmin bool
}

// Renderer turns a frozen snapshot into final HTML bytes. Returning an error
// fails the attempt before InsertStaged or any file write.
type Renderer func(ctx context.Context, in RenderInput) ([]byte, error)

// SnapshotRenderFunc is the full-pipeline renderer (R02): PlanSnapshot +
// RenderDetailed with Markdown, sanitizer policy, snippets, wikilinks and
// TOC. Unlike Renderer it also returns the render provenance (renderer
// version, dependency hash + snapshot) so the staged record reflects the
// ACTUAL render, not construction-time defaults.
type SnapshotRenderFunc func(ctx context.Context, in RenderInput) (RenderResult, error)

// renderedOutput carries render bytes plus the provenance the staged
// record must persist (R02: from the actual render, not defaults).
type renderedOutput struct {
	html            []byte
	rendererVersion string
	depsHash        string
	depSnapshot     map[string]any
}

// renderForPlan renders through the snapshot pipeline when wired
// (production), else the minimal Renderer (tests/fakes).
func (s *Service) renderForPlan(ctx context.Context, in RenderInput) (*renderedOutput, error) {
	if s.snapshot != nil {
		res, rerr := s.snapshot(ctx, in)
		if rerr != nil {
			return nil, rerr
		}
		return &renderedOutput{
			html: res.HTML, rendererVersion: res.RendererVersion,
			depsHash: res.RenderDependenciesHash, depSnapshot: res.DependencySnapshot,
		}, nil
	}
	html, rerr := s.renderer(ctx, in)
	if rerr != nil {
		return nil, rerr
	}
	// Legacy path: construction-time values, byte-identical to pre-R02
	// records (service renderer version + template render hash).
	return &renderedOutput{
		html: html, rendererVersion: s.rendererVersion,
		depsHash:    in.Template.RenderHash,
		depSnapshot: map[string]any{"template_render_hash": in.Template.RenderHash},
	}, nil
}

// DefaultRenderer is the deterministic in-process renderer: required-field
// validation, html/template execution with missingkey=error, and a minimal
// script-policy gate (policy "none" rejects script output). Full sanitizer /
// snippet / TOC / wikilink extraction is Task 7's render.go; this renderer is
// intentionally small and its limits are recorded in the Task 8 report.
func DefaultRenderer(_ context.Context, in RenderInput) ([]byte, error) {
	missing := []string{}
	for _, f := range in.Template.Fields {
		if !f.Required {
			continue
		}
		v, present := in.Content.Data[f.Name]
		if !present {
			missing = append(missing, f.Name)
			continue
		}
		if str, isStr := v.(string); isStr && strings.TrimSpace(str) == "" {
			missing = append(missing, f.Name)
		}
		if v == nil {
			missing = append(missing, f.Name)
		}
	}
	if len(missing) > 0 {
		return nil, sagaErr(CodeValidationFailed,
			"missing required fields: "+strings.Join(missing, ", "), nil)
	}

	data := make(map[string]any, len(in.Content.Data)+9)
	for k, v := range in.Content.Data {
		if str, ok := v.(string); ok {
			data[k] = template.HTML(str)
		} else {
			data[k] = v
		}
	}
	data["title"] = in.Content.Title
	data["slug"] = in.Content.Slug
	data["full_path"] = in.Content.FullPath
	data["published_at"] = in.LogicalPublishedAt
	data["public_url"] = in.PublicURL
	data["content_id"] = in.Content.ID.Hex()
	data["template_slug"] = in.Template.Slug
	data["template_version"] = in.TemplateVersionNum
	data["publication_id"] = in.PublicationID.Hex()

	tpl, err := template.New("layout").Option("missingkey=error").Parse(in.Template.HTMLLayout)
	if err != nil {
		return nil, sagaErr(CodeRenderFailed, "parse template layout", err)
	}
	var buf strings.Builder
	if err := tpl.Execute(&buf, data); err != nil {
		return nil, sagaErr(CodeRenderFailed, "render template layout", err)
	}
	html := buf.String()
	if html == "" {
		return nil, sagaErr(CodeRenderFailed, "renderer produced empty output", nil)
	}
	if in.Template.ScriptPolicy == "none" && strings.Contains(strings.ToLower(html), "<script") {
		return nil, sagaErr(CodeRenderFailed, "script output rejected by template script policy", nil)
	}
	return []byte(html), nil
}

// --- content / template loading ---

func (s *Service) loadContent(ctx context.Context, id primitive.ObjectID) (models.Content, error) {
	var content models.Content
	if err := s.db.FindOne(ctx, "content", bson.M{"_id": id}, &content); err != nil {
		return models.Content{}, sagaErr(CodeContentNotFound, "content "+id.Hex()+" not found", err)
	}
	return content, nil
}

// dbReloadContent re-reads the live record under the page lock so version
// freezing sees the latest draft state.
func (s *Service) dbReloadContent(ctx context.Context, content *models.Content) error {
	fresh, err := s.loadContent(ctx, content.ID)
	if err != nil {
		return err
	}
	*content = fresh
	return nil
}

// resolveContentVersion freezes the attempt's content version: an explicit pin
// must equal the live CurrentVersion (optimistic concurrency), otherwise the
// latest version under lock wins.
func resolveContentVersion(reqVersion, current int64) (int64, error) {
	if reqVersion != 0 {
		if current != 0 && reqVersion != current {
			return 0, sagaErr(CodeContentVersionConflict,
				fmt.Sprintf("pinned content version %d, current is %d", reqVersion, current), nil)
		}
		return reqVersion, nil
	}
	if current != 0 {
		return current, nil
	}
	return 0, sagaErr(CodeContentVersionConflict, "content has no version to publish", nil)
}

// resolveTemplateVersion loads the attempt's immutable template version: an
// explicit ID is read directly, otherwise the template's current version is
// frozen under lock (spec §16.7).
func (s *Service) resolveTemplateVersion(ctx context.Context, content models.Content, pin primitive.ObjectID) (templatecontract.TemplateVersion, error) {
	if !pin.IsZero() {
		tv, err := s.templates.GetVersion(ctx, pin)
		if err != nil {
			return templatecontract.TemplateVersion{}, sagaErr(templatecontract.CodeVersionNotFound,
				"template version "+pin.Hex()+" not found", err)
		}
		return tv, nil
	}
	var tpl models.Template
	if err := s.db.FindOne(ctx, "templates", bson.M{"_id": content.TemplateID}, &tpl); err != nil {
		return templatecontract.TemplateVersion{}, sagaErr(templatecontract.CodeNotFound,
			"template for content "+content.ID.Hex()+" not found", err)
	}
	tv, err := s.templates.GetCurrent(ctx, tpl.Slug)
	if err != nil {
		return templatecontract.TemplateVersion{}, sagaErr(templatecontract.CodeVersionNotFound,
			"current template version for "+tpl.Slug+" not found", err)
	}
	return tv, nil
}

// requirePublishableTemplate rejects draft/deprecated templates for ordinary
// publish. Rollback intentionally skips this: history stays traceable and the
// operator explicitly chose the source.
func requirePublishableTemplate(tv templatecontract.TemplateVersion) error {
	if templatecontract.NormalizeStatus(tv.Status) != templatecontract.StatusActive {
		return sagaErr(CodeTemplateNotActive,
			fmt.Sprintf("template %s version %d is %s, not active", tv.Slug, tv.Version, tv.Status), nil)
	}
	return nil
}

// --- cutover plan ---

type cutoverPlan struct {
	contentID      primitive.ObjectID
	fullPath       string
	contentVersion int64
	tv             templatecontract.TemplateVersion
	oldActive      *Publication
	oldPath        string // active serving path when renamed away from fullPath
	renamed        bool
	pubID          primitive.ObjectID
	logicalAt      time.Time
	publicURL      string
	html           []byte
	rawHash        string // hex SHA-256 for the file layer
	recordHash     string // "sha256:<hex>" for the publication record (spec §15.2)
	verification   VerificationStatus
	snapshot       map[string]any
	// Render provenance for the staged record. The legacy renderer path
	// fills these with today's construction-time values (service renderer
	// version + template render hash + the diagnostic snapshot above, i.e.
	// byte-identical to pre-R02 records); the snapshot pipeline fills them
	// from the actual RenderResult.
	rendererVersion string
	renderDepsHash  string
	depSnapshot     map[string]any
	opID            *primitive.ObjectID
	attempt         int64
	useIdem         bool
	startedAt       time.Time // plan freeze time for duration_ms logs.
	// Lane 2B: caller attribution carried from the request into the minted
	// Publication record (empty = unattributed, as before).
	actor        string
	via          string
	agentSession string
}

// planFields builds the Task 16F structured log fields for one plan at a
// lifecycle stage. Actor/provenance are attached by the caller when known
// (the saga itself is actor-agnostic); duration runs from plan freeze.
func (plan *cutoverPlan) planFields(stage string, statusCode int, errCode string) observe.Fields {
	var elapsed int64
	if !plan.startedAt.IsZero() {
		elapsed = time.Since(plan.startedAt).Milliseconds()
	}
	return observe.Fields{
		TemplateSlug:    plan.tv.Slug,
		TemplateVersion: plan.tv.Version,
		ContentID:       plan.contentID.Hex(),
		ContentVersion:  plan.contentVersion,
		PublicationID:   plan.pubID.Hex(),
		FullPath:        plan.fullPath,
		StorageProvider: "filesystem",
		Stage:           stage,
		DurationMS:      elapsed,
		StatusCode:      statusCode,
		ErrorCode:       errCode,
	}
}

// observeStageFail records a pre-cutover (stage/verify/record) failure:
// counter + structured log + failure-rate P0 input.
func (plan *cutoverPlan) observeStageFail(code string) {
	observe.Default().IncStageFailed()
	observe.Default().ObservePublicationOutcome(false)
	observe.LogPublication("stage_failed", plan.planFields("stage", 500, code))
}

// observeActivateFail records a cutover/commit failure: counter +
// structured log + failure-rate P0 input.
func (plan *cutoverPlan) observeActivateFail(code string) {
	observe.Default().IncActivateFailed()
	observe.Default().ObservePublicationOutcome(false)
	observe.LogPublication("activate_failed", plan.planFields("activate", 500, code))
}

func contentHashRecord(rawHex string) string { return "sha256:" + strings.ToLower(rawHex) }

// buildPublishPlan freezes versions, logical time, URL, idempotency snapshot
// and rendered bytes for an ordinary publish. Everything mutating-adjacent
// runs under the page lock held by the caller.
func (s *Service) buildPublishPlan(ctx context.Context, req PublishRequest, content models.Content, canonical string) (*cutoverPlan, error) {
	if err := s.dbReloadContent(ctx, &content); err != nil {
		return nil, err
	}
	if _, err := pathkey.Canonical(content.FullPath); err != nil {
		return nil, sagaErr(CodePathInvalid, "invalid content path "+content.FullPath, err)
	}
	cv, err := resolveContentVersion(req.ContentVersion, content.CurrentVersion)
	if err != nil {
		return nil, err
	}
	oldActive, err := s.repo.GetActive(ctx, req.ContentID)
	if err != nil {
		return nil, sagaErr(CodeInternal, "read active publication", err)
	}
	if req.ExpectedActiveID != nil {
		switch {
		case oldActive == nil:
			return nil, sagaErr(CodeConflict, "expected an active publication but none exists", nil)
		case oldActive.ID != *req.ExpectedActiveID:
			return nil, sagaErr(CodeConflict, "stale expected active publication", nil)
		}
	}
	tv, err := s.resolveTemplateVersion(ctx, content, req.TemplateVersionID)
	if err != nil {
		return nil, err
	}
	if err := requirePublishableTemplate(tv); err != nil {
		return nil, err
	}

	op, attempt, useIdem, err := s.idemAttempt(ctx, req.IdempotencyRecord)
	if err != nil {
		return nil, err
	}
	var opID *primitive.ObjectID
	if useIdem {
		opID = &op.ID
		if berr := s.idem.BindContentAndVersion(ctx, nil, op.ID, req.ContentID, cv, canonical); berr != nil {
			return nil, sagaErr(idempotency.CodeOf(berr), "bind idempotency execution", berr)
		}
	}

	pubID, logicalAt, err := s.freezeExecution(ctx, op, attempt, useIdem, tv.ID)
	if err != nil {
		return nil, err
	}

	publicURL, err := s.resolvePublicURL(content.FullPath)
	if err != nil {
		return nil, err
	}

	ro, err := s.renderForPlan(ctx, RenderInput{
		Content: content, Template: tv, PublicationID: pubID,
		ContentVersion: cv, TemplateVersionNum: tv.Version,
		LogicalPublishedAt: logicalAt, PublicURL: publicURL,
		AuthorIsAdmin: req.AuthorIsAdmin,
	})
	if err != nil {
		s.markTerminal(ctx, opID, attempt, CodeRenderFailed)
		return nil, err
	}
	html := ro.html

	plan := &cutoverPlan{
		contentID: req.ContentID, fullPath: content.FullPath, contentVersion: cv, tv: tv,
		oldActive: oldActive, pubID: pubID, logicalAt: logicalAt, publicURL: publicURL,
		html: html, verification: VerificationVerified,
		snapshot:        map[string]any{"template_render_hash": tv.RenderHash},
		rendererVersion: ro.rendererVersion, renderDepsHash: ro.depsHash, depSnapshot: ro.depSnapshot,
		opID: opID, attempt: attempt, useIdem: useIdem,
		startedAt: s.now(),
		// Lane 2B: thread caller attribution into the minted record.
		actor: req.Actor, via: req.Via, agentSession: req.AgentSession,
	}
	sum := sha256.Sum256(html)
	plan.rawHash = hex.EncodeToString(sum[:])
	plan.recordHash = contentHashRecord(plan.rawHash)
	if oldActive != nil && oldActive.FullPath != "" && oldActive.FullPath != content.FullPath {
		plan.renamed = true
		plan.oldPath = oldActive.FullPath
	}
	return plan, nil
}

// buildRollbackPlan assembles the same pipeline from a historical source:
// exact immutable bytes when retained, else the weaker re-render from the
// retained version + template snapshots. The new record inherits the source
// verification status on the exact path — so a legacy_unverified source trips
// the activation guard (Task 5 handoff) instead of going live silently.
func (s *Service) buildRollbackPlan(ctx context.Context, req RollbackRequest, content models.Content, canonical string, source *Publication) (*cutoverPlan, error) {
	if err := s.dbReloadContent(ctx, &content); err != nil {
		return nil, err
	}
	oldActive, err := s.repo.GetActive(ctx, req.ContentID)
	if err != nil {
		return nil, sagaErr(CodeInternal, "read active publication", err)
	}
	if req.ExpectedActiveID != nil {
		switch {
		case oldActive == nil:
			return nil, sagaErr(CodeConflict, "expected an active publication but none exists", nil)
		case oldActive.ID != *req.ExpectedActiveID:
			return nil, sagaErr(CodeConflict, "stale expected active publication", nil)
		}
	}

	op, attempt, useIdem, err := s.idemAttempt(ctx, req.IdempotencyRecord)
	if err != nil {
		return nil, err
	}
	var opID *primitive.ObjectID
	if useIdem {
		opID = &op.ID
		if berr := s.idem.BindContentAndVersion(ctx, nil, op.ID, req.ContentID, source.ContentVersion, canonical); berr != nil {
			return nil, sagaErr(idempotency.CodeOf(berr), "bind idempotency execution", berr)
		}
	}

	pubID, logicalAt, err := s.freezeExecution(ctx, op, attempt, useIdem, source.TemplateVersionID)
	if err != nil {
		return nil, err
	}
	publicURL, err := s.resolvePublicURL(content.FullPath)
	if err != nil {
		return nil, err
	}

	html, verification, snapshot, rendVer, depsHash, depSnap, err := s.rollbackBytes(ctx, content, source, pubID, logicalAt, publicURL, req.AuthorIsAdmin)
	if err != nil {
		s.markTerminal(ctx, opID, attempt, CodeRenderFailed)
		return nil, err
	}

	plan := &cutoverPlan{
		contentID: req.ContentID, fullPath: content.FullPath, contentVersion: source.ContentVersion,
		oldActive: oldActive, pubID: pubID, logicalAt: logicalAt, publicURL: publicURL,
		html: html, verification: verification, snapshot: snapshot,
		rendererVersion: rendVer, renderDepsHash: depsHash, depSnapshot: depSnap,
		opID: opID, attempt: attempt, useIdem: useIdem,
		startedAt: s.now(),
		// Lane 2B: thread caller attribution into the minted record.
		actor: req.Actor, via: req.Via, agentSession: req.AgentSession,
	}
	tv, terr := s.templates.GetVersion(ctx, source.TemplateVersionID)
	if terr != nil {
		s.markTerminal(ctx, opID, attempt, CodeRenderFailed)
		return nil, sagaErr(templatecontract.CodeVersionNotFound,
			"rollback template version "+source.TemplateVersionID.Hex()+" not found", terr)
	}
	plan.tv = tv
	sum := sha256.Sum256(html)
	plan.rawHash = hex.EncodeToString(sum[:])
	plan.recordHash = contentHashRecord(plan.rawHash)
	if oldActive != nil && oldActive.FullPath != "" && oldActive.FullPath != content.FullPath {
		plan.renamed = true
		plan.oldPath = oldActive.FullPath
	}
	return plan, nil
}

// rollbackBytes loads exact retained bytes, falling back to re-render.
// It also returns the render provenance for the staged record: the exact
// path reuses the source record's stored provenance (the bytes were rendered
// then); the re-render path returns the actual render's provenance with the
// rollback diagnostics merged into the dependency snapshot (full pipeline)
// or today's diagnostic map (legacy renderer, byte-identical records).
func (s *Service) rollbackBytes(ctx context.Context, content models.Content, source *Publication, pubID primitive.ObjectID, logicalAt time.Time, publicURL string, authorIsAdmin bool) (html []byte, verification VerificationStatus, snapshot map[string]any, rendererVersion, depsHash string, depSnapshot map[string]any, err error) {
	immutable := source.StoragePath
	if immutable == "" {
		immutable = s.store.ImmutablePath(content.ID, source.ID)
	}
	obj := storage.StoredObject{
		ContentID: content.ID, PublicationID: source.ID,
		Path: immutable, SHA256: strings.TrimPrefix(source.ContentHash, "sha256:"),
	}
	rc, err := s.store.Open(ctx, obj)
	if err == nil {
		defer rc.Close()
		var buf strings.Builder
		tmp := make([]byte, 32*1024)
		for {
			n, rerr := rc.Read(tmp)
			if n > 0 {
				buf.Write(tmp[:n])
			}
			if rerr != nil {
				break
			}
		}
		snap := map[string]any{
			"rollback_mode": "exact", "source_publication_id": source.ID.Hex(),
			"source_verification": string(source.VerificationStatus),
		}
		return []byte(buf.String()), source.VerificationStatus, snap,
			source.RendererVersion, source.RenderDependenciesHash, snap, nil
	}
	if storage.CodeOf(err) != storage.CodeNotFound {
		return nil, "", nil, "", "", nil, sagaErr(CodeRenderFailed, "read retained rollback bytes", err)
	}
	// Retention-expired immutable object: weaker re-render path.
	var ver models.ContentVersion
	if ferr := s.db.FindOne(ctx, "content_versions",
		bson.M{"content_id": content.ID, "version": source.ContentVersion}, &ver); ferr != nil {
		return nil, "", nil, "", "", nil, sagaErr(CodeRenderFailed,
			fmt.Sprintf("rollback source bytes expired and version %d not retained", source.ContentVersion), ferr)
	}
	tv, terr := s.templates.GetVersion(ctx, source.TemplateVersionID)
	if terr != nil {
		return nil, "", nil, "", "", nil, sagaErr(templatecontract.CodeVersionNotFound,
			"rollback template version "+source.TemplateVersionID.Hex()+" not found", terr)
	}
	renderContent := content
	renderContent.Data = ver.Data
	renderContent.Title = ver.Title
	ro, rerr := s.renderForPlan(ctx, RenderInput{
		Content: renderContent, Template: tv, PublicationID: pubID,
		ContentVersion: source.ContentVersion, TemplateVersionNum: tv.Version,
		LogicalPublishedAt: logicalAt, PublicURL: publicURL,
		AuthorIsAdmin: authorIsAdmin,
	})
	if rerr != nil {
		return nil, "", nil, "", "", nil, rerr
	}
	snap := map[string]any{
		"rollback_mode": "re-render", "source_publication_id": source.ID.Hex(),
		"source_content_version": source.ContentVersion,
	}
	depSnap := snap
	if s.snapshot != nil && ro.depSnapshot != nil {
		// Full pipeline: persist the actual render dependencies with the
		// rollback diagnostics merged in (rollback keys win).
		merged := make(map[string]any, len(ro.depSnapshot)+len(snap))
		for k, v := range ro.depSnapshot {
			merged[k] = v
		}
		for k, v := range snap {
			merged[k] = v
		}
		depSnap = merged
	}
	return ro.html, VerificationVerified, snap, ro.rendererVersion, ro.depsHash, depSnap, nil
}

// freezeExecution allocates (or crash-reuses) the attempt's publication ID +
// logical time and CAS-persists the execution snapshot BEFORE render. Persist
// failure blocks render. Without an idempotency record the IDs are purely
// in-memory.
func (s *Service) freezeExecution(ctx context.Context, op idempotency.Operation, attempt int64, useIdem bool, tvID primitive.ObjectID) (primitive.ObjectID, time.Time, error) {
	if !useIdem {
		return primitive.NewObjectID(), s.now().UTC().Truncate(time.Millisecond), nil
	}
	if op.PublicationID != nil && op.LogicalAt != nil {
		// Uncertain/crash takeover within the SAME attempt reuses the durable
		// snapshot so HTML hashes stay reproducible. A divergent template
		// pin conflicts instead of silently switching snapshots.
		if op.TemplateVersion != nil && *op.TemplateVersion != tvID {
			return primitive.NilObjectID, time.Time{}, sagaErr(idempotency.CodeConflict,
				"attempt already frozen to a different template version; reuse the durable snapshot", nil)
		}
		if ferr := s.idem.FreezeExecution(ctx, op.ID, attempt, *op.PublicationID, *op.LogicalAt, tvID); ferr != nil {
			return primitive.NilObjectID, time.Time{}, sagaErr(idempotency.CodeOf(ferr), "persist idempotency execution snapshot", ferr)
		}
		return *op.PublicationID, (*op.LogicalAt).UTC(), nil
	}
	pubID := primitive.NewObjectID()
	// Millisecond truncation: Mongo datetimes carry ms precision, and the
	// logical time is both rendered into HTML and stored as PublishedAt —
	// truncation keeps the frozen value identical across bytes, record and
	// projection.
	logicalAt := s.now().UTC().Truncate(time.Millisecond)
	if ferr := s.idem.FreezeExecution(ctx, op.ID, attempt, pubID, logicalAt, tvID); ferr != nil {
		return primitive.NilObjectID, time.Time{}, sagaErr(idempotency.CodeOf(ferr), "persist idempotency execution snapshot", ferr)
	}
	return pubID, logicalAt, nil
}

// markTerminal records a proven pre-activation failure (no live side effect)
// so the next same-key Begin allocates a new attempt + publication ID. Best
// effort: races with completion are the caller's to resolve via Begin.
func (s *Service) markTerminal(ctx context.Context, opID *primitive.ObjectID, attempt int64, code string) {
	if s.idem == nil || opID == nil {
		return
	}
	_, _ = s.idem.MarkTerminal(ctx, *opID, attempt, code)
}

func (s *Service) resolvePublicURL(fullPath string) (string, error) {
	if s.urls == nil {
		return fullPath, nil
	}
	u, err := s.urls.Resolve(fullPath)
	if err != nil {
		return "", sagaErr(CodePublicURLFailed, "resolve public URL for "+fullPath, err)
	}
	return u.String(), nil
}

// --- shared commit pipeline ---

// executeCutoverPlan runs stage → verify → file cutover → Mongo commit with
// compensation. Ordinary business activation REJECTS legacy_unverified here:
// the repository permits it for migration, the saga never does.
func (s *Service) executeCutoverPlan(ctx context.Context, plan *cutoverPlan) (PublicationResult, error) {
	fail := func(code, message string, err error) (PublicationResult, error) {
		return PublicationResult{}, sagaErr(code, message, err)
	}
	// R07: single effective executor. When this attempt runs under an
	// idempotency lease, verify we still own it (a takeover by a newer
	// worker must stop us BEFORE any side effect) and heartbeat it for
	// the duration of the cutover. Losing the lease cancels ctx: forward
	// progress stops; compensation paths are ctx-resilient or best-effort.
	if plan.opID != nil && s.idem != nil {
		cur, gerr := s.idem.Get(ctx, *plan.opID)
		if gerr != nil || cur.State != idempotency.StateProcessing ||
			cur.AttemptState != idempotency.AttemptProcessing || cur.Attempt != plan.attempt {
			return fail(CodePagePublishInProgress,
				"idempotency lease is no longer owned by this attempt; retry to take over", gerr)
		}
		hctx, stop := s.idem.Heartbeat(ctx, cur.ID, plan.attempt, cur.LeaseGeneration)
		defer stop()
		ctx = hctx
	}
	if !ActivatableVerification(plan.verification, false) {
		// Defense in depth: the saga only ever stages verified output, except
		// exact rollback bytes inherited from a legacy_unverified source —
		// which must NOT go live through the business path.
		s.markTerminal(ctx, plan.opID, plan.attempt, CodeActivateFailed)
		plan.observeActivateFail(CodeActivateFailed)
		return fail(CodeActivateFailed,
			"refusing to activate "+string(plan.verification)+" output through ordinary publish (migration approval required)", nil)
	}

	rec := &Publication{
		ContentID: plan.contentID, ContentVersion: plan.contentVersion,
		TemplateID: plan.tv.TemplateID, TemplateVersionID: plan.tv.ID, TemplateVersion: plan.tv.Version,
		FullPath: plan.fullPath, ContentHash: plan.recordHash,
		// Task 16E: persist the resolved public URL on the record so the
		// activation-transaction outbox insert carries it (no post-commit
		// join required for delivery).
		PublicURL:          plan.publicURL,
		StorageProvider:    "filesystem",
		StoragePath:        s.store.ImmutablePath(plan.contentID, plan.pubID),
		LogicalPublishedAt: plan.logicalAt,
		// Lane 2B: caller attribution from the request (empty when the
		// caller provided none — omitempty keeps old docs byte-identical).
		Actor:           plan.actor,
		Via:             plan.via,
		AgentSession:    plan.agentSession,
		RendererVersion: plan.rendererVersion, ProductBuildSHA: s.buildSHA,
		RenderDependenciesHash: plan.renderDepsHash, DependencySnapshot: plan.depSnapshot,
	}
	rec.ID = plan.pubID
	if err := s.repo.InsertStaged(ctx, rec); err != nil {
		s.markTerminal(ctx, plan.opID, plan.attempt, CodeStageFailed)
		plan.observeStageFail(CodeOf(err))
		return fail(CodeOf(err), "persist staged publication", err)
	}

	staged, err := s.store.Stage(ctx, storage.StageRequest{
		ContentID: plan.contentID, PublicationID: plan.pubID,
		CanonicalPath: plan.fullPath, HTML: plan.html, ExpectedSHA256: plan.rawHash,
	})
	if err != nil {
		_ = s.repo.MarkFailed(ctx, plan.pubID, "stage: "+err.Error())
		s.markTerminal(ctx, plan.opID, plan.attempt, CodeStageFailed)
		plan.observeStageFail(CodeStageFailed)
		return fail(CodeStageFailed, "stage immutable publication object", err)
	}
	if err := s.store.Verify(ctx, staged); err != nil {
		_ = s.store.Abort(ctx, staged)
		_ = s.repo.MarkFailed(ctx, plan.pubID, "verify: "+err.Error())
		s.markTerminal(ctx, plan.opID, plan.attempt, CodeVerifyFailed)
		plan.observeStageFail(CodeVerifyFailed)
		return fail(CodeVerifyFailed, "verify staged publication object", err)
	}
	if err := s.markVerifiedPresent(ctx, plan.pubID); err != nil {
		_ = s.store.Abort(ctx, staged)
		_ = s.repo.MarkFailed(ctx, plan.pubID, "verify-commit: "+err.Error())
		s.markTerminal(ctx, plan.opID, plan.attempt, CodeVerifyFailed)
		plan.observeStageFail(CodeVerifyFailed)
		return fail(CodeVerifyFailed, "record staged verification", err)
	}

	var oldID *primitive.ObjectID
	if plan.oldActive != nil {
		id := plan.oldActive.ID
		oldID = &id
	}
	if plan.renamed {
		if exists, eerr := s.store.Exists(ctx, plan.fullPath); eerr != nil {
			return s.failCutover(ctx, plan, staged, CodeActivateFailed, "check rename target", eerr)
		} else if exists {
			return s.failCutover(ctx, plan, staged, CodePathConflict,
				"rename target "+plan.fullPath+" already has a canonical file", nil)
		}
	}
	if oldID == nil {
		err = s.store.Activate(ctx, staged, plan.fullPath)
	} else {
		err = s.store.ActivateWithPrevious(ctx, staged, plan.fullPath, oldID)
	}
	if err != nil {
		if storage.CodeOf(err) == storage.CodeNeedsPreviousID {
			return s.failCutover(ctx, plan, staged, CodeActivateFailed,
				"canonical appeared concurrently; retry with a fresh expected active ID", err)
		}
		return s.failCutover(ctx, plan, staged, CodeActivateFailed, "atomic canonical cutover", err)
	}

	if s.faults.BeforeCommit != nil {
		if herr := s.faults.BeforeCommit(ctx); herr != nil {
			if errors.Is(herr, ErrStopAfterRename) {
				// Crash fixture: NO compensation, NO MarkFailed — files stay
				// for the Task 10 scanner, the DB still shows the old active.
				return fail(CodeCrashStop, herr.Error(), herr)
			}
			return s.failCommitted(ctx, plan, staged, oldID, "pre-commit hook", herr)
		}
	}

	var commitErr error
	if plan.renamed {
		// R08: the rename redirect commits atomically with the activation —
		// a crash can never leave the new page live with the old link
		// redirect-less. finishCommit keeps its idempotent backfill for
		// pre-R08 rows and same-key retries.
		commitErr = s.repo.ActivateCASWithRedirect(ctx, plan.contentID, plan.pubID, oldID, plan.oldPath, plan.fullPath)
	} else {
		commitErr = s.repo.ActivateCAS(ctx, plan.contentID, plan.pubID, oldID)
	}
	if commitErr != nil {
		// Commit ambiguity: a commit that actually landed reports here as a
		// success once the new record reads back as active — never compensate
		// a live page.
		active, rerr := s.repo.GetActive(ctx, plan.contentID)
		if s.faults.CommitReadError != nil {
			rerr = s.faults.CommitReadError
			active = nil
		}
		if rerr == nil && active != nil && active.ID == plan.pubID {
			return s.finishCommit(ctx, plan, oldID)
		}
		if rerr != nil {
			// R11: commit result UNKNOWN (the transaction errored AND the
			// read-back failed too). The commit may have landed — never
			// compensate a possibly-live page, never fail the staged
			// record, never mark the attempt terminal: leave everything
			// for the scanner / same-key retry (which converges via the
			// idempotent ActivateCAS + frozen snapshot reuse).
			plan.observeActivateFail(CodeActivationUnknown)
			s.auditf(ctx, "publication.activate_unknown", map[string]any{
				"content_id": plan.contentID.Hex(), "publication_id": plan.pubID.Hex(),
				"full_path": plan.fullPath, "commit_error": commitErr.Error(),
				"readback_error": rerr.Error(),
			})
			return PublicationResult{}, sagaErr(CodeActivationUnknown,
				"activation commit result unknown; staged publication retained for recovery — retry the same key", commitErr)
		}
		if IsConflict(commitErr) {
			return s.failCommitted(ctx, plan, staged, oldID, "activation conflict", commitErr)
		}
		return s.failCommitted(ctx, plan, staged, oldID, "activation transaction", commitErr)
	}
	return s.finishCommit(ctx, plan, oldID)
}

// failCutover handles a file-cutover failure: the store already restored the
// previous file when it moved one, so only the attempt record remains.
func (s *Service) failCutover(ctx context.Context, plan *cutoverPlan, staged storage.StagedObject, code, message string, err error) (PublicationResult, error) {
	_ = s.store.Abort(ctx, staged)
	_ = s.repo.MarkFailed(ctx, plan.pubID, "cutover: "+message)
	s.markTerminal(ctx, plan.opID, plan.attempt, code)
	plan.observeActivateFail(code)
	return PublicationResult{}, sagaErr(code, message, err)
}

// failCommitted compensates a cutover whose Mongo transaction returned an
// error: remove the uncommitted canonical (first publish) or restore the
// previous file (republish), fail the attempt, emit no success event.
func (s *Service) failCommitted(ctx context.Context, plan *cutoverPlan, staged storage.StagedObject, oldID *primitive.ObjectID, message string, err error) (PublicationResult, error) {
	code := CodeActivateFailed
	if IsConflict(err) {
		code = CodeConflict
	}
	cerr := s.compensateCutover(ctx, plan, staged, oldID)
	_ = s.repo.MarkFailed(ctx, plan.pubID, message+": "+err.Error())
	s.markTerminal(ctx, plan.opID, plan.attempt, code)
	plan.observeActivateFail(code)
	if cerr != nil {
		return PublicationResult{}, sagaErr(code,
			message+" failed AND file compensation failed (P0: scanner repair required)", errors.Join(err, cerr))
	}
	return PublicationResult{}, sagaErr(code, message+"; live page restored", err)
}

// compensateCutover undoes the file cutover after a failed commit.
func (s *Service) compensateCutover(ctx context.Context, plan *cutoverPlan, staged storage.StagedObject, oldID *primitive.ObjectID) error {
	if oldID == nil {
		// First publish: no previous file exists; remove the uncommitted
		// canonical only if it still carries our bytes.
		info, err := s.store.Inspect(ctx, plan.fullPath)
		if err != nil {
			return err
		}
		if !info.Exists {
			return nil
		}
		if !strings.EqualFold(info.SHA256, staged.SHA256) {
			return sagaErr(storage.CodeConflict,
				"canonical no longer carries the new bytes; refusing compensation", nil)
		}
		return s.store.Delete(ctx, plan.fullPath)
	}
	return s.store.CompensateActivate(ctx, staged, plan.fullPath, oldID)
}

// finishCommit runs the post-commit steps: confirm away .previous, finalize a
// rename (redirect + old-path retirement), enrich the outbox payload with the
// public URL, purge CDN best-effort, and return the result.
func (s *Service) finishCommit(ctx context.Context, plan *cutoverPlan, oldID *primitive.ObjectID) (PublicationResult, error) {
	if oldID != nil {
		// Post-commit cleanup; leftovers are scanner-convergent, never fatal.
		_ = s.store.ConfirmActivate(ctx, plan.fullPath, *oldID)
	}
	if plan.renamed {
		if rerr := s.upsertRedirect(ctx, plan.oldPath, plan.fullPath); rerr != nil {
			// Activation stands; the old canonical is RETAINED (not deleted)
			// until the redirect exists. Never silently swallow: loud error.
			return PublicationResult{}, sagaErr(CodeRedirectFailed,
				"page is live at "+plan.fullPath+" but the redirect from "+plan.oldPath+" was not recorded; retry redirect creation", rerr)
		}
		_ = s.store.Delete(ctx, plan.oldPath) // scanner quarantines leftovers.
		s.bestEffortPurge(ctx, []string{plan.oldPath, plan.fullPath})
	} else {
		s.bestEffortPurge(ctx, []string{plan.fullPath})
	}
	s.enrichOutboxURL(ctx, plan)
	s.auditf(ctx, "publication.activate", map[string]any{
		"content_id": plan.contentID.Hex(), "publication_id": plan.pubID.Hex(),
		"full_path": plan.fullPath, "content_hash": plan.recordHash,
		"renamed": plan.renamed,
	})
	// Task 16F: structured activation log + failure-rate denominator.
	observe.Default().ObservePublicationOutcome(true)
	observe.LogPublication("activated", plan.planFields("activate", 200, ""))
	return PublicationResult{
		PublicationID: plan.pubID, ContentID: plan.contentID,
		ContentVersion: plan.contentVersion, TemplateVersionID: plan.tv.ID,
		FullPath: plan.fullPath, PublicURL: plan.publicURL, ContentHash: plan.recordHash,
		LogicalPublishedAt: plan.logicalAt,
	}, nil
}

// markVerifiedPresent flips a staged record to verified/present after the
// store verification passes.
func (s *Service) markVerifiedPresent(ctx context.Context, pubID primitive.ObjectID) error {
	res, err := s.db.Collection(CollectionPublications).UpdateOne(ctx,
		bson.M{"_id": pubID, "status": string(StatusStaged)},
		bson.M{"$set": bson.M{
			"verification_status": string(VerificationVerified),
			"storage_state":       string(StoragePresent),
		}},
	)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return sagaErr(CodeInternal, "staged publication vanished before verification", nil)
	}
	return nil
}

// enrichOutboxURL backfills the resolved public URL onto the transactionally
// created content.publish event for rows written before Task 16E persisted
// it at activation time. Exactly-once-safe by construction:
//   - the activation transaction already carries public_url in the payload
//     (eventPayload reads the frozen Publication.PublicURL), so a worker that
//     delivers between commit and this backfill still sends a complete
//     payload — there is no race window, only a redundant no-op update;
//   - the missing-only filter ({$exists: false}) means this update can never
//     clobber a transaction-time value, so concurrent/duplicate finishCommit
//     calls converge instead of last-writer-winning. Delivery joins on IDs +
//     path and never depends on this field.
func (s *Service) enrichOutboxURL(ctx context.Context, plan *cutoverPlan) {
	_, _ = s.db.Collection(CollectionOutbox).UpdateOne(ctx,
		bson.M{"event_type": EventPublished, "aggregate_id": plan.pubID, "payload.public_url": bson.M{"$exists": false}},
		bson.M{"$set": bson.M{"payload.public_url": plan.publicURL}})
}

// upsertRedirect records old → new after a rename commit (idempotent retry
// safe via from_path upsert).
func (s *Service) upsertRedirect(ctx context.Context, fromPath, toPath string) error {
	if fromPath == "" || toPath == "" || fromPath == toPath {
		return sagaErr(CodeRedirectFailed, "invalid rename redirect endpoints", nil)
	}
	now := s.now()
	_, err := s.db.Collection("redirects").UpdateOne(ctx,
		bson.M{"from_path": fromPath},
		bson.M{"$set": bson.M{
			"from_path": fromPath, "to_path": toPath, "status_code": 301,
			"description": "rename-and-publish redirect", "updated_at": now,
		}, "$setOnInsert": bson.M{"created_at": now}},
		options.Update().SetUpsert(true),
	)
	return err
}
