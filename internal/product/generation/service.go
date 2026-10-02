// Generation orchestration: mode × target-state matrix, full-replace
// upsert, 5 MiB cap, scopes, Fork routing, and publish via the Task 8 saga.
//
// Pipeline (§20.5): auth → template resolve → publish preconditions (428) →
// data cap + strict validation (422) → path normalize (422) → target lookup →
// scope check (403, zero mutation) → upsert check (409) → mode branch.
//
// Purity rules enforced here (Task 11 handoff): permission/scope/sandbox and
// mode legality complete before any persistent side effect; mode=publish on a
// published target is a direct authorized command through PublicationService
// (no transient Fork); draft on a published target never touches the main
// Content (Fork only); preview writes nothing (no Content/Version/Fork/
// Publication/Idempotency row).
package generation

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jonradoff/lightcms/v7/internal/database"
	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/idempotency"
	"github.com/jonradoff/lightcms/v7/internal/product/pathkey"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/publicurl"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// RateLimiter gates generation publish throughput (§30.1: 60/min suggested).
// Tests inject a fake; production wiring (Task 16) uses the existing limiter.
type RateLimiter interface {
	Allow(ctx context.Context, actor Actor) (allowed bool, retryAfterSeconds int)
}

// Options wires the service. DB + Templates are required; Pubs/PubRepo/Idem/
// URLs default to DB-backed instances when nil (tests override Store via the
// saga Options). Audit is a structured-log sink (nil disables). Now defaults
// to time.Now. StoreAvailable, when non-nil, is consulted before any publish
// mutation: a non-nil error maps to 503 with zero mutation.
type Options struct {
	Templates      *templatecontract.Service
	Pubs           *publication.Service
	PubRepo        *publication.Repository
	Idem           *idempotency.Service
	URLs           *publicurl.Resolver
	Audit          func(ctx context.Context, action string, fields map[string]any)
	Now            func() time.Time
	Limiter        RateLimiter
	StoreAvailable func(ctx context.Context) error
	// SandboxForkResolver binds a mode=sandbox request to the caller's
	// active agent-sandbox fork by (userID, agentSession). R09: the server
	// must never trust a client-supplied fork ID on its own — resolution
	// verifies ownership + active status. Nil means no sandbox support
	// (mode=sandbox is rejected, preserving pre-R09 behavior for embedded
	// and unit-test services).
	SandboxForkResolver func(ctx context.Context, userID, agentSession string) (*primitive.ObjectID, error)
}

// Service is the one-command generation orchestrator.
type Service struct {
	db        *database.DB
	templates *templatecontract.Service
	pubs      *publication.Service
	pubRepo   *publication.Repository
	idem      *idempotency.Service
	urls      *publicurl.Resolver
	audit     func(ctx context.Context, action string, fields map[string]any)
	now       func() time.Time
	limiter   RateLimiter
	storeOK   func(ctx context.Context) error
	// forkResolve is Options.SandboxForkResolver (nil = no sandbox support).
	forkResolve func(ctx context.Context, userID, agentSession string) (*primitive.ObjectID, error)
}

// NewService builds the orchestrator. Templates/Repo default to DB-backed
// instances; Pubs/Idem/URLs stay nil unless wired (publish without them is an
// explicit INTERNAL_ERROR, never a nil-store panic).
func NewService(db *database.DB, opts Options) *Service {
	tpls := opts.Templates
	if tpls == nil && db != nil {
		tpls = templatecontract.NewService(db)
	}
	repo := opts.PubRepo
	if repo == nil && db != nil {
		repo = publication.NewRepository(db, nil)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		db: db, templates: tpls, pubs: opts.Pubs, pubRepo: repo,
		idem: opts.Idem, urls: opts.URLs, audit: opts.Audit, now: now,
		limiter: opts.Limiter, storeOK: opts.StoreAvailable,
		forkResolve: opts.SandboxForkResolver,
	}
}

func (s *Service) auditf(ctx context.Context, action string, fields map[string]any) {
	if s.audit == nil {
		return
	}
	s.audit(ctx, action, fields)
}

// Generate runs the mode × target-state matrix (§20.6).
func (s *Service) Generate(ctx context.Context, actor Actor, req GenerateRequest) (GenerateResponse, error) {
	var zero GenerateResponse
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode == "" {
		mode = ModeDraft
	}
	switch mode {
	case ModeDraft, ModePreview, ModeSandbox, ModePublish:
	default:
		return zero, genErr(CodeInvalidRequest, fmt.Sprintf("unknown mode %q: want draft|preview|sandbox|publish", req.Mode), nil)
	}
	if !actor.Authenticated {
		return zero, genErr(CodeUnauthenticated, "authentication is required", nil)
	}
	if s.limiter != nil {
		if ok, after := s.limiter.Allow(ctx, actor); !ok {
			return zero, &Error{Code: CodeRateLimited, Message: "rate limit exceeded", RetryAfter: after}
		}
	}
	if mode == ModePublish && s.storeOK != nil {
		if err := s.storeOK(ctx); err != nil {
			return zero, &Error{Code: CodeStoreUnavailable, Message: "static store temporarily unavailable", RetryAfter: 30, Err: err}
		}
	}

	// Resolve the immutable template version first (read-only).
	tplSlug := strings.TrimSpace(req.Template)
	if tplSlug == "" {
		return zero, genErr(CodeFieldValidationFailed, "template is required", nil)
	}
	tv, err := s.templates.GetCurrent(ctx, tplSlug)
	if err != nil {
		if templatecontract.CodeOf(err) == templatecontract.CodeNotFound {
			return zero, genErr(CodeTemplateNotFound, fmt.Sprintf("template %q not found", tplSlug), err)
		}
		return zero, genErr(CodeInternal, "resolve template", err)
	}
	if templatecontract.NormalizeStatus(tv.Status) != templatecontract.StatusActive {
		return zero, genErr(CodeTemplateNotActive, fmt.Sprintf("template %s version %d is %s, not active", tv.Slug, tv.Version, tv.Status), nil)
	}

	// Publish preconditions (§20.2, §21): optimistic expected version +
	// Idempotency-Key, both 428 with zero mutation when absent. Only the
	// PRESENCE checks run here; the expected-version VALUE check runs after
	// the replay branch (R06) so a completed request replays from cache
	// even after the template moved on.
	var idemParams IdempotencyParams
	var hasIdem bool
	if mode == ModePublish {
		if req.ExpectedTemplateVersion == nil {
			return zero, genErr(CodeTemplatePreconditionRequired, "mode=publish requires expected_template_version", nil)
		}
		idemParams, hasIdem = IdempotencyFrom(ctx)
		if !hasIdem {
			return zero, genErr(CodeIdempotencyKeyRequired, "mode=publish requires Idempotency-Key", nil)
		}
	}

	// Data cap (§20.7): canonical JSON of Data only.
	data := req.Data
	if data == nil {
		data = map[string]any{}
	}
	if err := checkDataCap(data); err != nil {
		return zero, err
	}

	// Title is required on create (request shape, not a template field).
	title := strings.TrimSpace(req.Title)

	// Normalize the target path (full-replace: slug falls back to title).
	fullPath, slug, folderPath, err := normalizeTargetPath(req.FolderPath, req.Slug, req.Title)
	if err != nil {
		return zero, err
	}
	canonical, err := pathkey.Canonical(fullPath)
	if err != nil {
		return zero, genErr(CodePathInvalid, fmt.Sprintf("invalid path %q: %v", fullPath, err), err)
	}

	// Target lookup (read-only, before scope checks per §20.5).
	live, err := s.findLive(ctx, canonical)
	if err != nil {
		return zero, genErr(CodeInternal, "lookup target", err)
	}
	var active *publication.Publication
	if live != nil && s.pubRepo != nil {
		active, err = s.pubRepo.GetActive(ctx, live.ID)
		if err != nil {
			return zero, genErr(CodeInternal, "read active publication", err)
		}
	}
	published := active != nil
	targetExists := live != nil

	// Authorization BEFORE any idempotency write (R05): scope matrix,
	// sandbox_only, sandbox fork. A 403 here creates no lease, so an
	// authorized retry of the same key proceeds immediately instead of
	// wedging on REQUEST_IN_PROGRESS. (Scope matrix §22.2.)
	if err := checkScopes(actor, mode, targetExists); err != nil {
		return zero, err
	}
	if actor.SandboxOnly && mode != ModeSandbox {
		return zero, genErr(CodePermissionDenied, "sandbox-only key must use mode=sandbox", nil)
	}
	if mode == ModeSandbox {
		forkID, ferr := s.resolveSandboxFork(ctx, actor)
		if ferr != nil {
			return zero, ferr
		}
		if forkID != nil {
			actor.SandboxForkID = forkID
		}
	}

	// Title checks (pure request-shape validation) stay shared and early.
	if targetExists && title == "" {
		// Full-replace keeps title required even on upsert hits: an empty
		// title would silently blank the page heading.
		return zero, &Error{Code: CodeFieldValidationFailed, Message: "title is required",
			Details: []FieldDetail{{Code: "FIELD_REQUIRED", Field: "title", Message: "title is required"}}}
	}
	if !targetExists && title == "" {
		return zero, &Error{Code: CodeFieldValidationFailed, Message: "title is required",
			Details: []FieldDetail{{Code: "FIELD_REQUIRED", Field: "title", Message: "title is required"}}}
	}

	// Publish idempotency AFTER auth (R05: no lease exists before this
	// point). Begin, replay short-circuit, or lease takeover + resume
	// (R04). Replay precedes the template-value precondition, validation,
	// the upsert gate, and every mutation (R06): same key + same body
	// replays the completed response even after the template moved on.
	// Replay requires the CURRENT caller to be authorized (checked above) —
	// a permission change never resurrects another identity's cached result.
	//
	// Every failure exit below releases the lease (R05: 403/409/422/5xx all
	// release) via completePublishError so the same key retries cleanly.
	var earlyOp *idempotency.Operation
	releaseOp := func(perr error) {
		if earlyOp != nil && s.idem != nil {
			s.completePublishError(ctx, *earlyOp, perr)
		}
	}
	if mode == ModePublish {
		op, replayResp, berr := s.beginOrResumePublish(ctx, actor, req, tv, idemParams)
		if berr != nil {
			return zero, berr
		}
		if replayResp != nil {
			return *replayResp, nil
		}
		earlyOp = &op
		ctx = idempotency.WithLeaseGeneration(ctx, op.LeaseGeneration)
		if *req.ExpectedTemplateVersion != tv.Version {
			verr := genErr(CodeTemplateVersionChanged,
				fmt.Sprintf("template %s is at version %d, expected %d", tv.Slug, tv.Version, *req.ExpectedTemplateVersion), nil)
			releaseOp(verr)
			return zero, verr
		}
	}

	// Strict validation against the immutable version (Task 4). Runs after
	// replay for publish (R06) and after auth everywhere (R05: the 422
	// cache write below is an operation write and must not precede
	// authorization).
	verrs, warns := templatecontract.ValidateData(tv, data)
	if len(verrs) > 0 {
		details := make([]FieldDetail, 0, len(verrs))
		for _, e := range verrs {
			details = append(details, FieldDetail{Code: e.Code, Field: e.Field, Message: e.Message})
		}
		// Pure validation failure on the publish path is replay-cacheable:
		// Complete the owned record (earlyOp exists exactly when this
		// attempt owns one).
		if mode == ModePublish && earlyOp != nil && s.idem != nil {
			s.completeValidationOp(ctx, earlyOp, 422, req, tv, details)
		}
		return zero, &Error{Code: CodeFieldValidationFailed, Message: "content data does not match the template", Details: details}
	}
	// Materialize server-side defaults for missing keys (mirrors
	// ValidateData's FIELD_DEFAULT_APPLIED contract).
	data = materializeDefaults(tv, data)

	// Upsert gate (not for preview — preview never writes; not for publish
	// — publish checks it above after replay). Positioned after validation
	// to preserve the pre-R05 422-before-409 precedence.
	if mode != ModePreview && mode != ModePublish && targetExists && !req.Upsert {
		return zero, genErr(CodePathConflict, fmt.Sprintf("page %s already exists (upsert=false)", fullPath), nil)
	}

	// Publish upsert gate AFTER replay and validation (same 422-before-409
	// precedence as before): a replay never trips on the page its first
	// attempt created, and a fresh conflict releases the lease (R05) so the
	// same key retries cleanly once the caller fixes upsert/body.
	if mode == ModePublish && targetExists && !req.Upsert {
		uerr := genErr(CodePathConflict, fmt.Sprintf("page %s already exists (upsert=false)", fullPath), nil)
		releaseOp(uerr)
		return zero, uerr
	}

	switch mode {
	case ModePreview:
		return s.preview(ctx, actor, tv, live, active, fullPath, warns)
	case ModeDraft:
		return s.draft(ctx, actor, tv, live, active, published, targetExists, fullPath, slug, folderPath, title, canonical, data, warns)
	case ModeSandbox:
		return s.sandbox(ctx, actor, tv, live, active, published, fullPath, slug, folderPath, title, canonical, data, warns)
	case ModePublish:
		return s.publishWithOp(ctx, actor, tv, live, active, published, targetExists, fullPath, slug, folderPath, title, canonical, data, warns, req, idemParams, earlyOp)
	}
	return zero, genErr(CodeInternal, "unreachable mode", nil)
}

// resolveSandboxFork binds a mode=sandbox request to the caller's active
// fork (R09). A pre-set SandboxForkID is re-verified against the resolver
// when one is configured — a client-supplied fork ID is never trusted on
// its own. Otherwise the resolver maps (userID, agentSession) to the owned
// active fork and the resolved ID is returned for the caller to inject. No
// resolver, no session, or no owned active fork → AgentSandboxRequired
// (fail closed). A nil return with nil error means the pre-set fork was
// verified in place.
func (s *Service) resolveSandboxFork(ctx context.Context, actor Actor) (*primitive.ObjectID, error) {
	verify := func(want *primitive.ObjectID) error {
		owned, err := s.forkResolve(ctx, actor.ID, actor.AgentSession)
		if err != nil {
			return genErr(CodeAgentSandboxRequired, "mode=sandbox requires an active agent sandbox", err)
		}
		if owned == nil || want == nil || *owned != *want {
			return genErr(CodePermissionDenied, "sandbox fork does not belong to this session", nil)
		}
		return nil
	}
	if actor.SandboxForkID != nil && !actor.SandboxForkID.IsZero() {
		if s.forkResolve == nil {
			return nil, nil
		}
		if err := verify(actor.SandboxForkID); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if s.forkResolve == nil {
		return nil, genErr(CodeAgentSandboxRequired, "mode=sandbox requires an active agent sandbox", nil)
	}
	forkID, err := s.forkResolve(ctx, actor.ID, actor.AgentSession)
	if err != nil {
		return nil, genErr(CodeAgentSandboxRequired, "mode=sandbox requires an active agent sandbox", err)
	}
	return forkID, nil
}

// checkDataCap enforces the 5 MiB Data cap owned by Task 12.
func checkDataCap(data map[string]any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return genErr(CodeFieldValidationFailed, "data is not JSON-serializable", err)
	}
	if len(raw) > MaxDataBytes {
		return &Error{Code: CodeFieldValidationFailed,
			Message: fmt.Sprintf("data is %d bytes, maximum is %d (5 MiB)", len(raw), MaxDataBytes),
			Details: []FieldDetail{{Code: CodeDataTooLarge, Field: "data",
				Message: fmt.Sprintf("data exceeds 5 MiB (%d bytes)", len(raw))}}}
	}
	return nil
}

// materializeDefaults fills missing keys from template defaults (string
// family verbatim, number parsed, boolean only when Default parses true —
// mirrors templatecontract.fieldDefault so ValidateData warnings and stored
// bytes agree).
func materializeDefaults(tv templatecontract.TemplateVersion, data map[string]any) map[string]any {
	out := make(map[string]any, len(data)+len(tv.Fields))
	for k, v := range data {
		out[k] = v
	}
	for _, f := range tv.Fields {
		if _, ok := out[f.Name]; ok {
			continue
		}
		switch f.Type {
		case "boolean":
			if b, err := parseBoolDefault(f.Default); err == nil && b {
				out[f.Name] = true
			}
		case "number":
			if s := strings.TrimSpace(f.Default); s != "" {
				var n float64
				if err := json.Unmarshal([]byte(s), &n); err == nil {
					out[f.Name] = n
				}
			}
		default:
			if f.Default != "" {
				out[f.Name] = f.Default
			}
		}
	}
	return out
}

func parseBoolDefault(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("not a bool")
	}
}

// normalizeTargetPath joins FolderPath + Slug into FullPath. An empty Slug is
// regenerated from Title (full-replace rule §20.7).
func normalizeTargetPath(folderPath, slug, title string) (fullPath, outSlug, outFolder string, err error) {
	outSlug = strings.TrimSpace(slug)
	if outSlug == "" {
		outSlug = slugifyTitle(title)
	}
	if outSlug == "" {
		return "", "", "", &Error{Code: CodePathInvalid, Message: "slug is required (or provide a title to derive it)",
			Details: []FieldDetail{{Code: CodePathInvalid, Field: "slug", Message: "slug is required"}}}
	}
	if strings.Contains(outSlug, "/") || outSlug == "." || outSlug == ".." {
		return "", "", "", genErr(CodePathInvalid, fmt.Sprintf("invalid slug %q", outSlug), nil)
	}
	for _, r := range outSlug {
		if r < 0x20 || r == 0x7f || r == '\\' || r == '?' || r == '#' || r == 0 {
			return "", "", "", genErr(CodePathInvalid, fmt.Sprintf("invalid slug %q", outSlug), nil)
		}
	}
	outFolder = strings.TrimSpace(folderPath)
	if outFolder == "" {
		outFolder = "/"
	}
	if !strings.HasPrefix(outFolder, "/") {
		return "", "", "", genErr(CodePathInvalid, fmt.Sprintf("folder_path %q must start with /", folderPath), nil)
	}
	outFolder = strings.TrimSuffix(outFolder, "/")
	if outFolder == "" {
		outFolder = "/"
	}
	if outFolder == "/" {
		fullPath = "/" + outSlug
	} else {
		fullPath = outFolder + "/" + outSlug
	}
	return fullPath, outSlug, outFolder, nil
}

var nonAlnumRE = regexp.MustCompile(`[^a-z0-9]+`)

// slugifyTitle derives a URL slug from the title (§20.7 fallback).
func slugifyTitle(title string) string {
	s := strings.ToLower(strings.TrimSpace(title))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if r < utf8.RuneSelf && (unicode.IsSpace(r) || strings.ContainsRune("-_./\\", r)) {
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		} else if !prevDash && b.Len() > 0 {
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := strings.Trim(nonAlnumRE.ReplaceAllString(b.String(), "-"), "-")
	if len(out) > 100 {
		out = out[:100]
	}
	return strings.Trim(out, "-")
}

// checkScopes enforces the §22.2 combination matrix before any mutation.
func checkScopes(actor Actor, mode string, targetExists bool) error {
	var need []string
	switch mode {
	case ModeDraft:
		if targetExists {
			need = []string{ScopeContentEdit}
		} else {
			need = []string{ScopeContentCreate}
		}
	case ModePreview:
		need = []string{ScopeContentView}
	case ModeSandbox:
		if targetExists {
			need = []string{ScopeContentEdit}
		} else {
			need = []string{ScopeContentCreate}
		}
	case ModePublish:
		if targetExists {
			need = []string{ScopeContentEdit, ScopeContentPublish}
		} else {
			need = []string{ScopeContentCreate, ScopeContentPublish}
		}
	}
	for _, s := range need {
		if !actor.Can(s) {
			return genErr(CodePermissionDenied, fmt.Sprintf("missing required scope %q for mode=%s", s, mode), nil)
		}
	}
	return nil
}

// findLive loads the live content row for a canonical path, or (nil, nil)
// when absent. Soft-deleted rows (path_active=false) do not count.
func (s *Service) findLive(ctx context.Context, canonical string) (*models.Content, error) {
	var c models.Content
	err := s.db.FindOne(ctx, "content", bson.M{
		"canonical_full_path": canonical,
		"path_scope":          "live",
		"path_active":         true,
	}, &c)
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

// preview is read-only: no Content/Version/Fork/Publication/Idempotency write.
func (s *Service) preview(ctx context.Context, actor Actor, tv templatecontract.TemplateVersion, live *models.Content, active *publication.Publication, fullPath string, warns []templatecontract.FieldWarning) (GenerateResponse, error) {
	published := active != nil
	var id string
	var cv int64
	if live != nil {
		id = live.ID.Hex()
		cv = live.CurrentVersion
	}
	return GenerateResponse{
		ID: id, Action: "preview", Template: tv.Slug, FullPath: fullPath, Mode: ModePreview,
		Published: published, RequiresPublish: false,
		PublicURL: nil, ContentVersion: cv, TemplateVersion: tv.Version,
		PublicationID: nil, Warnings: warns,
	}, nil
}

// publishIdemIdentity derives the stable idempotency coordinates shared by
// Begin and TakeOverByKey so both address the same record.
func publishIdemIdentity(actor Actor, p IdempotencyParams) (owner, method, path, key string) {
	owner = p.Owner
	if owner == "" {
		owner = actor.Owner()
	}
	method = p.Method
	if method == "" {
		method = "POST"
	}
	path = p.Path
	if path == "" {
		path = "/api/v1/page-generation"
	}
	return owner, method, path, p.Key
}

// beginOrResume starts the idempotency operation, transparently taking over
// an expired lease (R04) instead of wedging the caller on
// REQUEST_IN_PROGRESS. Completed operations return with Replay=true; live
// leases still report REQUEST_IN_PROGRESS. A terminal attempt is advanced
// via a fresh Begin (same semantics as a first retry).
func (s *Service) beginOrResume(ctx context.Context, owner, method, path, key string, body []byte) (idempotency.Operation, error) {
	if s.idem == nil {
		return idempotency.Operation{}, genErr(CodeInternal, "idempotency service is not wired", nil)
	}
	op, err := s.idem.Begin(ctx, owner, method, path, key, body)
	if err == nil {
		return op, nil
	}
	if idempotency.CodeOf(err) != idempotency.CodeLeaseExpired {
		return idempotency.Operation{}, err
	}
	top, terr := s.idem.TakeOverByKey(ctx, owner, method, path, key)
	if terr != nil {
		switch idempotency.CodeOf(terr) {
		case idempotency.CodeStaleAttempt, idempotency.CodeNotFound:
			// Terminal attempt (retry advances it) or vanished record
			// (TTL edge): a fresh Begin converges either way.
			return s.idem.Begin(ctx, owner, method, path, key, body)
		default:
			return idempotency.Operation{}, terr
		}
	}
	return top, nil
}

// beginOrResumePublish Begins the publish operation AFTER authorization
// (R05: no lease exists before auth, so 403 creates nothing). Same key +
// same body on a completed operation replays; an expired lease is
// CAS-taken-over and resumed from the durable snapshot (R04). It returns
// the operation to execute under, or a cached replay response when no new
// work remains (including the taken-over attempt whose publication already
// went active — completing its cache instead of minting a duplicate).
func (s *Service) beginOrResumePublish(ctx context.Context, actor Actor, req GenerateRequest, tv templatecontract.TemplateVersion, p IdempotencyParams) (idempotency.Operation, *GenerateResponse, error) {
	owner, method, path, key := publishIdemIdentity(actor, p)
	body := p.Body
	if len(body) == 0 {
		raw, _ := json.Marshal(canonicalGenerateBody(req, tv))
		body = raw
	}
	op, err := s.beginOrResume(ctx, owner, method, path, key, body)
	if err != nil {
		return idempotency.Operation{}, nil, mapIdemBeginErr(err)
	}
	if op.Replay {
		return s.replayOpResponse(op)
	}
	if op.PublicationID != nil && op.Attempt > 0 && s.pubRepo != nil && s.idem != nil {
		// Takeover resume: the crashed attempt froze a Publication ID.
		// If that publication already went active, cache the response and
		// return it — never mint a second Publication for one operation.
		if pub, gerr := s.pubRepo.GetByID(ctx, *op.PublicationID); gerr == nil && pub != nil &&
			pub.Status == publication.StatusActive {
			if resp, rerr := s.completeResumedActive(ctx, op, *pub, tv); rerr == nil {
				return op, resp, nil
			}
			// Cache completion lost a race (another worker finished it):
			// report in-progress so the retry replays the cached result.
			return idempotency.Operation{}, nil, &Error{
				Code: CodeRequestInProgress, Message: "operation completed concurrently; retry to replay",
				RetryAfter: 1,
			}
		}
	}
	return op, nil, nil
}

// replayOpResponse rebuilds the replayable outcome for a completed op.
// Validation-cache replays become their 422 error (with cached field
// details) — a cached failure must never degrade to a 200-empty success.
// Success caches become the stored response.
func (s *Service) replayOpResponse(op idempotency.Operation) (idempotency.Operation, *GenerateResponse, error) {
	if op.ValidationReplay {
		// BSON round-trips decode arrays/docs as primitive.A/M (named
		// types that fail plain []any/map[string]any assertions) —
		// accept both shapes so cached details survive the trip.
		toStrMap := func(item any) (map[string]any, bool) {
			switch m := item.(type) {
			case map[string]any:
				return m, true
			case primitive.M:
				return map[string]any(m), true
			}
			return nil, false
		}
		var list []any
		switch raw := op.Response["details"].(type) {
		case []any:
			list = raw
		case primitive.A:
			list = []any(raw)
		}
		var details []FieldDetail
		for _, item := range list {
			if m, ok := toStrMap(item); ok {
				str := func(k string) string {
					if v, ok := m[k].(string); ok {
						return v
					}
					return ""
				}
				details = append(details, FieldDetail{Code: str("code"), Field: str("field"), Message: str("message")})
			}
		}
		return idempotency.Operation{}, nil, &Error{
			Code: CodeFieldValidationFailed,
			Message: "content data does not match the template",
			Details: details,
		}
	}
	resp, rerr := responseFromCache(op)
	if rerr != nil {
		return idempotency.Operation{}, nil, genErr(CodeInternal, "decode idempotency replay", rerr)
	}
	return op, &resp, nil
}

// completeResumedActive caches the success response for a taken-over attempt
// whose publication is already active and returns the replayable response.
// Template identity comes from the live record (not the current template:
// the publication may predate an upgrade); when the op carries a content
// binding it must agree with the record, otherwise the snapshot belongs to
// a different execution and resuming would certify the wrong bytes.
func (s *Service) completeResumedActive(ctx context.Context, op idempotency.Operation, pub publication.Publication, tv templatecontract.TemplateVersion) (*GenerateResponse, error) {
	action, status := "updated", 200
	if pub.ContentVersion <= 1 {
		action, status = "created", 201
	}
	if op.ContentID != nil && *op.ContentID != pub.ContentID {
		return nil, genErr(CodePublicationConflict,
			"resumed operation is bound to different content; retry with a new key", nil)
	}
	cache := map[string]any{
		"id": pub.ContentID.Hex(), "action": action, "template": tv.Slug,
		"full_path": pub.FullPath, "mode": ModePublish, "published": true,
		"requires_publish": false, "public_url": pub.PublicURL,
		"content_version": float64(pub.ContentVersion),
		"template_version": float64(pub.TemplateVersion),
		"publication_id": pub.ID.Hex(),
	}
	if _, cerr := s.idem.Complete(ctx, op.ID, op.Attempt, status, cache, false); cerr != nil {
		return nil, cerr
	}
	fake := op
	fake.Response = cache
	resp, rerr := responseFromCache(fake)
	if rerr != nil {
		return nil, rerr
	}
	return &resp, nil
}

// assertOpOwned verifies this worker still owns the attempt before
// completing it (R07 fencing): after a lease takeover, Completing would
// write a response onto another worker's attempt.
func (s *Service) assertOpOwned(ctx context.Context, op idempotency.Operation) error {
	if s.idem == nil {
		return nil
	}
	cur, err := s.idem.Get(ctx, op.ID)
	if err != nil {
		return genErr(CodeRequestInProgress, "idempotency lease lost; retry to take over", err)
	}
	if cur.State != idempotency.StateProcessing || cur.AttemptState != idempotency.AttemptProcessing ||
		cur.Attempt != op.Attempt || cur.LeaseGeneration != op.LeaseGeneration {
		return genErr(CodeRequestInProgress, "idempotency lease lost to a newer worker; retry to take over", nil)
	}
	return nil
}

// beginForPublish owns the idempotency Begin for publish (Task 11 handoff).
// Prefer beginOrResumePublish (takeover-aware); this raw Begin stays for
// callers that manage leases themselves. It never Completes here;
// completion belongs to publishWithOp / completeValidationOp.
func (s *Service) beginForPublish(ctx context.Context, actor Actor, req GenerateRequest, tv templatecontract.TemplateVersion, p IdempotencyParams) (idempotency.Operation, error) {
	if s.idem == nil {
		return idempotency.Operation{}, genErr(CodeInternal, "idempotency service is not wired", nil)
	}
	owner, method, path, key := publishIdemIdentity(actor, p)
	body := p.Body
	if len(body) == 0 {
		raw, _ := json.Marshal(canonicalGenerateBody(req, tv))
		body = raw
	}
	op, err := s.idem.Begin(ctx, owner, method, path, key, body)
	if err != nil {
		return idempotency.Operation{}, mapIdemBeginErr(err)
	}
	return op, nil
}
