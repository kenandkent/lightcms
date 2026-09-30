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
	// Idempotency-Key, both 428 with zero mutation when absent, 409 on stale.
	var idemParams IdempotencyParams
	var hasIdem bool
	if mode == ModePublish {
		if req.ExpectedTemplateVersion == nil {
			return zero, genErr(CodeTemplatePreconditionRequired, "mode=publish requires expected_template_version", nil)
		}
		if *req.ExpectedTemplateVersion != tv.Version {
			return zero, genErr(CodeTemplateVersionChanged,
				fmt.Sprintf("template %s is at version %d, expected %d", tv.Slug, tv.Version, *req.ExpectedTemplateVersion), nil)
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

	// Strict validation against the immutable version (Task 4).
	verrs, warns := templatecontract.ValidateData(tv, data)
	if len(verrs) > 0 {
		details := make([]FieldDetail, 0, len(verrs))
		for _, e := range verrs {
			details = append(details, FieldDetail{Code: e.Code, Field: e.Field, Message: e.Message})
		}
		// Pure validation failure on the publish path is replay-cacheable:
		// Complete the idempotency record when we own one.
		if mode == ModePublish && hasIdem && s.idem != nil {
			s.completeValidation(ctx, idemParams, 422, req, tv)
		}
		return zero, &Error{Code: CodeFieldValidationFailed, Message: "content data does not match the template", Details: details}
	}
	// Materialize server-side defaults for missing keys (mirrors
	// ValidateData's FIELD_DEFAULT_APPLIED contract).
	data = materializeDefaults(tv, data)

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

	// Publish replay precedence (§21.3): same key + same body replays the
	// completed response even if the target state moved (create → exists) or
	// the caller's scopes would now differ. Check replay BEFORE the mutable
	// scope/upsert gates; non-replay attempts still enforce them with zero
	// mutation. Non-publish modes never touch idempotency.
	var earlyOp *idempotency.Operation
	if mode == ModePublish {
		op, berr := s.beginForPublish(ctx, actor, req, tv, idemParams)
		if berr != nil {
			return zero, berr
		}
		if op.Replay {
			resp, rerr := responseFromCache(op)
			if rerr != nil {
				return zero, genErr(CodeInternal, "decode idempotency replay", rerr)
			}
			return resp, nil
		}
		earlyOp = &op
	}

	// Scope matrix (§22.2) + sandbox_only enforcement — before any mutation.
	if err := checkScopes(actor, mode, targetExists); err != nil {
		return zero, err
	}
	if actor.SandboxOnly && mode != ModeSandbox {
		return zero, genErr(CodePermissionDenied, "sandbox-only key must use mode=sandbox", nil)
	}
	if mode == ModeSandbox {
		if actor.SandboxForkID == nil || actor.SandboxForkID.IsZero() {
			return zero, genErr(CodeAgentSandboxRequired, "mode=sandbox requires an active agent sandbox", nil)
		}
	}

	// Upsert gate (not for preview — preview never writes).
	if mode != ModePreview && targetExists && !req.Upsert {
		return zero, genErr(CodePathConflict, fmt.Sprintf("page %s already exists (upsert=false)", fullPath), nil)
	}
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
		if !actor.HasScope(s) {
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

// beginForPublish owns the idempotency Begin for publish (Task 11 handoff).
// It never TakesOver (not even on lease expiry — the caller maps expiry to
// 409 + Retry-After and the client retries), and it never Completes here;
// completion belongs to publishWithOp / completeValidation.
func (s *Service) beginForPublish(ctx context.Context, actor Actor, req GenerateRequest, tv templatecontract.TemplateVersion, p IdempotencyParams) (idempotency.Operation, error) {
	if s.idem == nil {
		return idempotency.Operation{}, genErr(CodeInternal, "idempotency service is not wired", nil)
	}
	owner := p.Owner
	if owner == "" {
		owner = actor.Owner()
	}
	method := p.Method
	if method == "" {
		method = "POST"
	}
	path := p.Path
	if path == "" {
		path = "/api/v1/page-generation"
	}
	body := p.Body
	if len(body) == 0 {
		raw, _ := json.Marshal(canonicalGenerateBody(req, tv))
		body = raw
	}
	op, err := s.idem.Begin(ctx, owner, method, path, p.Key, body)
	if err != nil {
		return idempotency.Operation{}, mapIdemBeginErr(err)
	}
	return op, nil
}
