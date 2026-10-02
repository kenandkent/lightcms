// Deterministic render snapshot and publication planning (plan Task 7).
//
// Scope (spec §§8.6, 9.6, 15–16; plan Task 7):
//   - Freeze the render input BEFORE any file write: content/version,
//     immutable template version (layout, fields, script policy), publication
//     ID, frozen logical publish time, canonical public URL, and a dependency
//     snapshot (theme, snippets, sanitizer policy, wikilink/TOC versions).
//   - Render in memory with the SAME Markdown/Snippet/Theme/Wikilink/TOC
//     semantics as the legacy ContentService path, but WITHOUT reading a
//     newer mutable template, WITHOUT touching MongoDB, and WITHOUT writing
//     a canonical file. File staging/cutover is the Task 8 saga; the Task 10
//     scanner owns repair/GC policy.
//   - Record renderer version, product build SHA, and dependency hashes in
//     the result; verify bytes via SHA-256 (BytesHash/render-hash verify).
//
// Frozen-time rule: Render never calls time.Now. The only clock input is
// RenderSnapshot.LogicalPublishedAt, so re-rendering the same snapshot yields
// byte-identical HTML and an identical content hash. Crash/uncertain takeover
// reuses the persisted publication_id + logical_published_at (spec §16.1);
// a terminal pre-activation retry allocates a NEW publication ID and logical
// time (owned by Task 8/11, not here).
//
// System variables (spec §8.6) are injected from the frozen snapshot, never
// from mutable records: title, slug, full_path, published_at, public_url,
// content_id, template_slug, template_version, publication_id.
package publication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"
	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	goldmarkhtml "github.com/yuin/goldmark/renderer/html"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// RendererVersion is recorded on every render (spec §15.2 renderer_version).
// It identifies the Markdown/Snippet/Wikilink/TOC pipeline, not the product
// release: a pipeline change bumps this while ProductBuildSHA tracks the binary.
const RendererVersion = "lightcms-renderer-v1"

// ProductBuildSHA identifies the binary that rendered the bytes
// (spec §15.2 product_build_sha). Overridden at link time by Task 16 wiring
// (-ldflags "-X ...ProductBuildSHA=<sha>"); the dev default keeps unit
// renders reproducible without a build stamp.
var ProductBuildSHA = "7.2.2"

// Frozen processor versions captured in the dependency snapshot
// (spec §18.5: dependency_snapshot fixes every mutable render dependency).
const (
	MarkdownProcessorVersion = "goldmark-gfm-v1"
	SanitizerVersion         = "bluemonday-editor-safe-v1"
	WikilinkProcessorVersion = "wikilink-v1"
	TOCProcessorVersion      = "toc-v1"
)

// Render error codes. Validation/unsafe/template failures happen BEFORE
// InsertStaged or any file write (spec §16.1: validation → render → security
// validation → hash → stage).
const (
	// CodeRenderValidation marks a frozen-input or required-field failure.
	CodeRenderValidation = "RENDER_VALIDATION_FAILED"
	// CodeRenderUnsafe marks raw script vectors under a restrictive policy.
	CodeRenderUnsafe = "RENDER_UNSAFE_CONTENT"
	// CodeRenderTemplate marks layout parse/execute failures.
	CodeRenderTemplate = "RENDER_TEMPLATE_FAILED"
	// CodeRenderHashMismatch marks a BytesHash verification failure.
	CodeRenderHashMismatch = "RENDER_HASH_MISMATCH"
)

// renderErr builds a typed publication error with a render code.
func renderErr(code, message string, err error) *Error {
	return &Error{Code: code, Message: message, Err: err}
}

// CodeOfRender maps any error to its render code; non-render errors yield "".
func CodeOfRender(err error) string {
	if err == nil {
		return ""
	}
	var pe *Error
	if asErr(err, &pe) {
		switch pe.Code {
		case CodeRenderValidation, CodeRenderUnsafe, CodeRenderTemplate, CodeRenderHashMismatch:
			return pe.Code
		}
	}
	return ""
}

// RenderSnapshot is the immutable render input (plan Task 7 interface).
// Every field is frozen at plan time under the publish lock (Task 8): the
// renderer must never re-read the mutable template record, the wall clock,
// or live Mongo state. Maps/slices are deep-copied by PlanSnapshot so later
// mutable edits cannot leak into a frozen snapshot.
type RenderSnapshot struct {
	PublicationID     primitive.ObjectID `json:"publication_id"`
	ContentID         primitive.ObjectID `json:"content_id"`
	ContentVersion    int64              `json:"content_version"`
	TemplateID        primitive.ObjectID `json:"template_id,omitempty"`
	TemplateVersionID primitive.ObjectID `json:"template_version_id"`
	TemplateVersion   int64              `json:"template_version"`
	TemplateSlug      string             `json:"template_slug"`

	FullPath string         `json:"full_path"`
	Title    string         `json:"title"`
	Slug     string         `json:"slug"`
	Data     map[string]any `json:"data"`

	// Frozen immutable template contract (from TemplateVersion, never the
	// mutable Template record).
	HTMLLayout   string                 `json:"html_layout"`
	Fields       []models.TemplateField `json:"fields"`
	ScriptPolicy string                 `json:"script_policy"`
	// AuthorIsAdmin resolves policy=admin_only at plan time: admins render
	// raw, editors render strict. Frozen so takeover replays the same decision.
	AuthorIsAdmin bool `json:"author_is_admin"`

	// LogicalPublishedAt is frozen before Render (spec §15.3, §16.1). It feeds
	// template {{.published_at}}, Content.PublishedAt, and the webhook payload.
	// ActivatedAt (physical commit time) never enters the rendered bytes.
	LogicalPublishedAt time.Time `json:"logical_published_at"`
	// PublicURL is the resolved canonical public URL, frozen before Render.
	PublicURL string `json:"public_url"`

	// DependencySnapshot freezes every mutable render dependency
	// (spec §18.5): theme hash, snippet hashes, sanitizer policy,
	// wikilink/TOC processor versions, plus template-referenced extras.
	DependencySnapshot map[string]any `json:"dependency_snapshot,omitempty"`

	// Frozen snippet bodies (name → HTML) for [[include:name]] expansion.
	// Missing names are left in place, matching the legacy miss behavior.
	Snippets map[string]string `json:"snippets,omitempty"`
	// Frozen wikilink index: lowercase title → path, path → title.
	TitleToPath map[string]string `json:"title_to_path,omitempty"`
	PathToTitle map[string]string `json:"path_to_title,omitempty"`
	// Frozen lc:query expansions (raw directive → rendered HTML). Layouts with
	// lc:query but no cached entry render a deterministic placeholder comment
	// instead of querying live MongoDB during Render.
	LCQueryCache map[string]string `json:"lc_query_cache,omitempty"`
}

// RenderResult is the deterministic render output plus its provenance
// (spec §15.2, §18.5). HTML/ContentHash are the staging input; the remaining
// fields populate the staged Publication record.
type RenderResult struct {
	HTML                   []byte         `json:"-"`
	ContentHash            string         `json:"content_hash"`
	RendererVersion        string         `json:"renderer_version"`
	ProductBuildSHA        string         `json:"product_build_sha"`
	RenderDependenciesHash string         `json:"render_dependencies_hash"`
	DependencySnapshot     map[string]any `json:"dependency_snapshot,omitempty"`
}

// PlanOptions carries the plan-time resolutions for PlanSnapshot.
type PlanOptions struct {
	ScriptPolicy       string
	AuthorIsAdmin      bool
	DependencySnapshot map[string]any
	Snippets           map[string]string
	TitleToPath        map[string]string
	PathToTitle        map[string]string
	LCQueryCache       map[string]string
}

// PlanSnapshot freezes content + immutable template version + publication ID +
// logical time + public URL into a RenderSnapshot (spec §16.1: freeze before
// Render; idempotency snapshot persists publication_id + logical_published_at
// before Render). It deep-copies Data/Fields/dependency maps so later mutable
// edits cannot affect the frozen input. No file or Mongo I/O happens here.
func PlanSnapshot(
	content models.Content,
	contentVersion int64,
	tmplVer templatecontract.TemplateVersion,
	publicationID primitive.ObjectID,
	logicalAt time.Time,
	publicURL string,
	opts PlanOptions,
) (RenderSnapshot, error) {
	if publicationID.IsZero() {
		return RenderSnapshot{}, renderErr(CodeRenderValidation, "publication_id is required", nil)
	}
	if content.ID.IsZero() {
		return RenderSnapshot{}, renderErr(CodeRenderValidation, "content_id is required", nil)
	}
	if contentVersion < 1 {
		return RenderSnapshot{}, renderErr(CodeRenderValidation, "content_version must be >= 1", nil)
	}
	if tmplVer.ID.IsZero() {
		return RenderSnapshot{}, renderErr(CodeRenderValidation, "template_version_id is required", nil)
	}
	if tmplVer.Version < 1 {
		return RenderSnapshot{}, renderErr(CodeRenderValidation, "template_version must be >= 1", nil)
	}
	if strings.TrimSpace(content.FullPath) == "" {
		return RenderSnapshot{}, renderErr(CodeRenderValidation, "full_path is required", nil)
	}
	if strings.TrimSpace(tmplVer.HTMLLayout) == "" {
		return RenderSnapshot{}, renderErr(CodeRenderValidation, "template html_layout is required", nil)
	}
	if logicalAt.IsZero() {
		return RenderSnapshot{}, renderErr(CodeRenderValidation, "logical_published_at is required", nil)
	}
	if strings.TrimSpace(publicURL) == "" {
		return RenderSnapshot{}, renderErr(CodeRenderValidation, "public_url is required", nil)
	}
	policy := normalizeScriptPolicy(opts.ScriptPolicy)
	if policy == "" {
		// Empty inherits the site default; the planner resolves the effective
		// policy before freezing. An explicit unknown policy is a bug.
		if strings.TrimSpace(opts.ScriptPolicy) != "" {
			return RenderSnapshot{}, renderErr(CodeRenderValidation,
				fmt.Sprintf("unknown script policy %q", opts.ScriptPolicy), nil)
		}
		policy = "all"
	}

	data := deepCopyData(content.Data)
	fields := deepCopyFields(tmplVer.Fields)
	deps := deepCopySnapshot(opts.DependencySnapshot)
	if deps == nil {
		deps = DefaultDependencySnapshot(policy)
	}
	for name, value := range map[string]any{"snippets": opts.Snippets, "title_to_path": opts.TitleToPath, "path_to_title": opts.PathToTitle, "lc_query_cache": opts.LCQueryCache} {
		raw, err := json.Marshal(value)
		if err != nil {
			return RenderSnapshot{}, renderErr(CodeRenderValidation, "encode dependency "+name, err)
		}
		h := sha256.Sum256(raw)
		deps[name+"_hash"] = "sha256:" + hex.EncodeToString(h[:])
	}
	return RenderSnapshot{
		PublicationID:      publicationID,
		ContentID:          content.ID,
		ContentVersion:     contentVersion,
		TemplateID:         tmplVer.TemplateID,
		TemplateVersionID:  tmplVer.ID,
		TemplateVersion:    tmplVer.Version,
		TemplateSlug:       tmplVer.Slug,
		FullPath:           content.FullPath,
		Title:              content.Title,
		Slug:               content.Slug,
		Data:               data,
		HTMLLayout:         tmplVer.HTMLLayout,
		Fields:             fields,
		ScriptPolicy:       policy,
		AuthorIsAdmin:      opts.AuthorIsAdmin,
		LogicalPublishedAt: logicalAt.UTC(),
		PublicURL:          publicURL,
		DependencySnapshot: deps,
		Snippets:           copyStringMap(opts.Snippets),
		TitleToPath:        copyStringMap(opts.TitleToPath),
		PathToTitle:        copyStringMap(opts.PathToTitle),
		LCQueryCache:       copyStringMap(opts.LCQueryCache),
	}, nil
}

// normalizeScriptPolicy maps "" (inherit) to "" and lowercases known values;
// unknown values yield "" so callers can reject them explicitly.
func normalizeScriptPolicy(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "":
		return ""
	case "all":
		return "all"
	case "admin_only":
		return "admin_only"
	case "none":
		return "none"
	default:
		return ""
	}
}

// DefaultDependencySnapshot builds the minimal frozen dependency record
// (spec §18.5) when the planner has no richer theme/snippet hashes.
func DefaultDependencySnapshot(scriptPolicy string) map[string]any {
	return map[string]any{
		"renderer_version":   RendererVersion,
		"markdown_processor": MarkdownProcessorVersion,
		"sanitizer":          SanitizerVersion,
		"sanitizer_policy":   scriptPolicy,
		"wikilink_processor": WikilinkProcessorVersion,
		"toc_processor":      TOCProcessorVersion,
	}
}

// HashDependencySnapshot hashes the frozen dependency record with canonical
// JSON (map keys sorted by encoding/json) as "sha256:<hex>" (spec §9.6 hash
// format). A nil snapshot hashes as "{}".
func HashDependencySnapshot(snapshot map[string]any) string {
	raw := []byte("{}")
	if snapshot != nil {
		if b, err := json.Marshal(snapshot); err == nil {
			raw = b
		}
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// BytesHash is the render-hash verify primitive: "sha256:<hex>" over the
// exact rendered bytes (spec §15.2 content_hash). The Task 8 saga strips the
// "sha256:" prefix when calling storage.Stage, which expects 64 raw hex chars.
func BytesHash(html []byte) string {
	sum := sha256.Sum256(html)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// VerifyContentHash recomputes the bytes hash and compares it with the
// expected value. Both "sha256:<hex>" and raw 64-char hex are accepted so the
// same helper verifies publication ContentHash values and storage SHA256
// handles. Any mismatch yields CodeRenderHashMismatch.
func VerifyContentHash(html []byte, expected string) error {
	want := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(expected), "sha256:")))
	if want == "" {
		return renderErr(CodeRenderValidation, "expected content hash is required", nil)
	}
	sum := sha256.Sum256(html)
	if got := hex.EncodeToString(sum[:]); got != want {
		return renderErr(CodeRenderHashMismatch,
			fmt.Sprintf("content hash mismatch: expected %s, got sha256:%s", expected, got), nil)
	}
	return nil
}

// Render is the plan Task 7 interface: deterministic in-memory render of a
// frozen snapshot. It returns the HTML bytes and the "sha256:<hex>" content
// hash. No Mongo I/O, no file writes, no wall-clock reads.
func Render(ctx context.Context, snap RenderSnapshot) ([]byte, string, error) {
	res, err := RenderDetailed(ctx, snap)
	if err != nil {
		return nil, "", err
	}
	return res.HTML, res.ContentHash, nil
}

// RenderDetailed is Render plus its provenance record for the staged
// Publication (renderer_version, product_build_sha, render_dependencies_hash,
// dependency_snapshot). Task 8 persists these on InsertStaged.
func RenderDetailed(ctx context.Context, snap RenderSnapshot) (RenderResult, error) {
	var zero RenderResult
	if err := checkCtx(ctx); err != nil {
		return zero, err
	}
	if err := validateSnapshot(&snap); err != nil {
		return zero, err
	}
	allowUnsafe, err := resolveAllowUnsafe(snap.ScriptPolicy, snap.AuthorIsAdmin)
	if err != nil {
		return zero, err
	}

	// Work on a copy so the caller's frozen maps stay immutable.
	data := deepCopyData(snap.Data)
	applyFieldDefaults(data, snap.Fields)

	if err := validateRequiredFields(data, snap.Fields); err != nil {
		return zero, err
	}
	if !allowUnsafe {
		if offender, found := findUnsafeField(data); found {
			return zero, renderErr(CodeRenderUnsafe,
				fmt.Sprintf("field %q contains raw script vectors under script policy %q", offender, snap.ScriptPolicy), nil)
		}
	}

	fieldTypes := make(map[string]string, len(snap.Fields))
	for _, f := range snap.Fields {
		fieldTypes[f.Name] = f.Type
	}

	// Snippet includes first (legacy order), using only the frozen map.
	for k, v := range data {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if fieldTypes[k] != "markdown" && fieldTypes[k] != "richtext" && fieldTypes[k] != "rawhtml" {
			continue // Plain text is literal, including include/wiki/query syntax.
		}
		s = expandSnippetIncludesFrozen(s, snap.Snippets, snippetRenderData(snap, data))
		// Markdown conversion per frozen field type; richtext sanitized when strict.
		switch fieldTypes[k] {
		case "markdown":
			s = markdownToHTMLFrozen(s, allowUnsafe)
		default:
			if !allowUnsafe && fieldTypes[k] == "richtext" {
				s = renderSafePolicy.Sanitize(s)
			}
		}
		data[k] = s
	}

	// System variables from the frozen snapshot (spec §8.6). published_at is
	// the frozen logical time formatted deterministically, never time.Now.
	tmplData := make(map[string]any, len(data)+16)
	for k, v := range data {
		if s, ok := v.(string); ok && (fieldTypes[k] == "markdown" || fieldTypes[k] == "richtext" || fieldTypes[k] == "rawhtml") {
			tmplData[k] = template.HTML(s)
		} else if v != nil {
			tmplData[k] = v
		}
	}
	tmplData["title"] = snap.Title
	tmplData["slug"] = snap.Slug
	tmplData["full_path"] = snap.FullPath
	tmplData["published_at"] = snap.LogicalPublishedAt.UTC().Format(time.RFC3339)
	tmplData["public_url"] = snap.PublicURL
	tmplData["content_id"] = snap.ContentID.Hex()
	tmplData["template_slug"] = snap.TemplateSlug
	tmplData["template_version"] = snap.TemplateVersion
	tmplData["publication_id"] = snap.PublicationID.Hex()
	tmplData["content_version"] = snap.ContentVersion
	tmplData["lc_toc"] = template.HTML(tocPlaceholderFrozen)

	// lc:query directives expand BEFORE html/template parsing (html/template
	// strips HTML comments, which carry the directives) using only the frozen
	// cache — never live MongoDB.
	layout := expandLCQueryFrozen(snap.HTMLLayout, snap.LCQueryCache)

	htmlStr, err := executeLayoutFrozen(layout, tmplData)
	if err != nil {
		return zero, err
	}
	htmlStr = injectHeadingIDsFrozen(htmlStr)
	htmlStr = buildAndInjectTOCFrozen(htmlStr)
	if strings.Contains(htmlStr, "[[") {
		htmlStr = processWikiLinksFrozen(newFrozenWikilinkIndex(snap.TitleToPath, snap.PathToTitle), htmlStr)
	}

	if !allowUnsafe {
		if containsUnsafeHTML(htmlStr) && layoutContainsUnsafeVector(snap.HTMLLayout) == false {
			// Data vectors were sanitized above; a residual vector here can
			// only come from snippet bodies or lc:query cache — both frozen
			// inputs the planner must have vetted. Fail rather than stage.
			return zero, renderErr(CodeRenderUnsafe,
				"rendered HTML contains script vectors under restrictive script policy", nil)
		}
	}

	html := []byte(htmlStr)
	contentHash := BytesHash(html)
	deps := deepCopySnapshot(snap.DependencySnapshot)
	if deps == nil {
		deps = DefaultDependencySnapshot(snap.ScriptPolicy)
	}
	return RenderResult{
		HTML:                   html,
		ContentHash:            contentHash,
		RendererVersion:        RendererVersion,
		ProductBuildSHA:        ProductBuildSHA,
		RenderDependenciesHash: HashDependencySnapshot(deps),
		DependencySnapshot:     deps,
	}, nil
}

// validateSnapshot enforces the frozen-input contract before any rendering.
func validateSnapshot(snap *RenderSnapshot) error {
	if snap == nil {
		return renderErr(CodeRenderValidation, "render snapshot is required", nil)
	}
	if snap.PublicationID.IsZero() {
		return renderErr(CodeRenderValidation, "publication_id is required", nil)
	}
	if snap.ContentID.IsZero() {
		return renderErr(CodeRenderValidation, "content_id is required", nil)
	}
	if snap.ContentVersion < 1 {
		return renderErr(CodeRenderValidation, "content_version must be >= 1", nil)
	}
	if snap.TemplateVersionID.IsZero() {
		return renderErr(CodeRenderValidation, "template_version_id is required", nil)
	}
	if snap.TemplateVersion < 1 {
		return renderErr(CodeRenderValidation, "template_version must be >= 1", nil)
	}
	if strings.TrimSpace(snap.FullPath) == "" {
		return renderErr(CodeRenderValidation, "full_path is required", nil)
	}
	if !strings.HasPrefix(snap.FullPath, "/") {
		return renderErr(CodeRenderValidation, "full_path must start with /", nil)
	}
	if strings.TrimSpace(snap.HTMLLayout) == "" {
		return renderErr(CodeRenderValidation, "template html_layout is required", nil)
	}
	if snap.LogicalPublishedAt.IsZero() {
		return renderErr(CodeRenderValidation, "logical_published_at is required", nil)
	}
	if strings.TrimSpace(snap.PublicURL) == "" {
		return renderErr(CodeRenderValidation, "public_url is required", nil)
	}
	if snap.Data == nil {
		snap.Data = map[string]any{}
	}
	policy := normalizeScriptPolicy(snap.ScriptPolicy)
	if strings.TrimSpace(snap.ScriptPolicy) != "" && policy == "" {
		return renderErr(CodeRenderValidation,
			fmt.Sprintf("unknown script policy %q", snap.ScriptPolicy), nil)
	}
	return nil
}

// resolveAllowUnsafe freezes the script-policy decision (spec §Script Policy).
// "" inherits the site default ("all"); admin_only passes admins through.
func resolveAllowUnsafe(policy string, authorIsAdmin bool) (bool, error) {
	switch normalizeScriptPolicy(policy) {
	case "", "all":
		return true, nil
	case "none":
		return false, nil
	case "admin_only":
		return authorIsAdmin, nil
	default:
		return false, renderErr(CodeRenderValidation,
			fmt.Sprintf("unknown script policy %q", policy), nil)
	}
}

// applyFieldDefaults materializes template defaults for missing keys so
// required checks and rendering see the same values as the write path.
// Boolean "true"/"false" defaults become bools; everything else stays a string.
func applyFieldDefaults(data map[string]any, fields []models.TemplateField) {
	for _, f := range fields {
		if f.Default == "" {
			continue
		}
		if _, ok := data[f.Name]; ok {
			continue
		}
		if f.Type == "boolean" {
			switch strings.ToLower(strings.TrimSpace(f.Default)) {
			case "true":
				data[f.Name] = true
				continue
			case "false":
				data[f.Name] = false
				continue
			}
		}
		data[f.Name] = f.Default
	}
}

// validateRequiredFields rejects snapshots missing required values BEFORE any
// staging or file write. Boolean false counts as present (key existence, not
// truthiness); strings must be non-blank.
func validateRequiredFields(data map[string]any, fields []models.TemplateField) error {
	for _, f := range fields {
		if !f.Required {
			continue
		}
		v, ok := data[f.Name]
		if !ok || v == nil {
			return renderErr(CodeRenderValidation,
				fmt.Sprintf("required field %q is missing", f.Name), nil)
		}
		if s, isStr := v.(string); isStr && strings.TrimSpace(s) == "" {
			return renderErr(CodeRenderValidation,
				fmt.Sprintf("required field %q is empty", f.Name), nil)
		}
	}
	return nil
}

// --- unsafe-content detection (strict publication path) ---

var eventHandlerRE = regexp.MustCompile(`(?i)\son[a-z]+\s*=`)

func containsUnsafeHTML(s string) bool {
	lower := strings.ToLower(s)
	if strings.Contains(lower, "<script") ||
		strings.Contains(lower, "<iframe") ||
		strings.Contains(lower, "<object") ||
		strings.Contains(lower, "<embed") ||
		strings.Contains(lower, "<form") ||
		strings.Contains(lower, "<input") ||
		strings.Contains(lower, "javascript:") {
		return true
	}
	return eventHandlerRE.MatchString(s)
}

func findUnsafeField(data map[string]any) (string, bool) {
	// Sorted scan keeps the reported offender deterministic.
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if s, ok := data[k].(string); ok && containsUnsafeHTML(s) {
			return k, true
		}
	}
	return "", false
}

// layoutContainsUnsafeVector reports whether the frozen template layout itself
// carries script vectors (trusted template code, not user data). Layout
// vectors do not fail strict renders; only user-data vectors do.
func layoutContainsUnsafeVector(layout string) bool {
	return containsUnsafeHTML(layout)
}

// --- frozen Markdown / sanitizer (same semantics as ContentService) ---

var renderSafePolicy = func() *bluemonday.Policy {
	p := bluemonday.NewPolicy()
	p.AllowElements(
		"a", "abbr", "acronym", "address", "article", "aside", "audio",
		"b", "blockquote", "br", "caption", "cite", "code", "col", "colgroup",
		"dd", "del", "details", "dfn", "div", "dl", "dt",
		"em", "figcaption", "figure", "footer",
		"h1", "h2", "h3", "h4", "h5", "h6", "header", "hr",
		"i", "img", "ins", "kbd", "li", "main", "mark", "nav",
		"ol", "p", "picture", "pre", "q", "rp", "rt", "ruby",
		"s", "samp", "section", "small", "source", "span", "strong", "sub", "summary", "sup",
		"table", "tbody", "td", "tfoot", "th", "thead", "time", "tr", "track",
		"u", "ul", "var", "video", "wbr",
	)
	p.AllowAttrs("href").OnElements("a")
	p.AllowURLSchemes("http", "https", "mailto", "tel")
	p.AllowAttrs("target", "rel").OnElements("a")
	p.AllowAttrs("src", "alt", "width", "height", "loading", "decoding").OnElements("img")
	p.AllowAttrs("src", "type").OnElements("source", "track")
	p.AllowAttrs("src", "controls", "autoplay", "loop", "muted", "preload", "width", "height").OnElements("video", "audio")
	p.AllowAttrs("id", "class", "style", "title", "lang", "dir").Globally()
	p.AllowAttrs("colspan", "rowspan", "scope", "headers").OnElements("td", "th")
	p.AllowAttrs("span").OnElements("col", "colgroup")
	p.AllowAttrs("open").OnElements("details")
	p.AllowAttrs("datetime").OnElements("del", "ins", "time")
	p.AllowAttrs("start", "reversed", "type").OnElements("ol")
	p.AllowAttrs("type", "value").OnElements("li")
	p.AllowAttrs("cite").OnElements("blockquote", "q", "del", "ins")
	p.AllowAttrs("srcset", "media", "sizes").OnElements("source", "picture")
	return p
}()

func markdownToHTMLFrozen(text string, allowUnsafe bool) string {
	var opts []goldmark.Option
	opts = append(opts, goldmark.WithExtensions(extension.GFM))
	if allowUnsafe {
		opts = append(opts, goldmark.WithRendererOptions(goldmarkhtml.WithUnsafe()))
	}
	md := goldmark.New(opts...)
	var buf bytes.Buffer
	if err := md.Convert([]byte(text), &buf); err != nil {
		return text
	}
	result := buf.String()
	if !allowUnsafe {
		result = renderSafePolicy.Sanitize(result)
	}
	return result
}

// --- frozen snippet includes (depth 3, cycle-safe, same as ContentService) ---

var snippetIncludeREFrozen = regexp.MustCompile(`\[\[include:([a-zA-Z0-9_-]+)\]\]`)

func snippetRenderData(snap RenderSnapshot, data map[string]any) map[string]any {
	base := make(map[string]any, len(data)+16)
	for k, v := range data {
		base[k] = v
	}
	base["title"] = snap.Title
	base["slug"] = snap.Slug
	base["full_path"] = snap.FullPath
	base["published_at"] = snap.LogicalPublishedAt.UTC().Format(time.RFC3339)
	base["public_url"] = snap.PublicURL
	base["content_id"] = snap.ContentID.Hex()
	base["template_slug"] = snap.TemplateSlug
	base["template_version"] = snap.TemplateVersion
	base["publication_id"] = snap.PublicationID.Hex()
	return base
}

func expandSnippetIncludesFrozen(text string, snippets map[string]string, data map[string]any) string {
	return expandSnippetDepthFrozen(text, snippets, data, 0, map[string]bool{})
}

func expandSnippetDepthFrozen(text string, snippets map[string]string, data map[string]any, depth int, visited map[string]bool) string {
	if depth >= 3 || !strings.Contains(text, "[[include:") {
		return text
	}
	return snippetIncludeREFrozen.ReplaceAllStringFunc(text, func(match string) string {
		sub := snippetIncludeREFrozen.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		name := sub[1]
		if visited[name] {
			return ""
		}
		body, ok := snippets[name]
		if !ok {
			return match
		}
		// Render snippet bodies carrying template vars against the frozen
		// system data; plain-HTML snippets pass through unchanged.
		if strings.Contains(body, "{{") {
			if rendered, err := executeLayoutFrozen(body, data); err == nil {
				body = rendered
			}
		}
		visited[name] = true
		body = expandSnippetDepthFrozen(body, snippets, data, depth+1, visited)
		delete(visited, name)
		return body
	})
}

// --- frozen lc:query (deterministic, no live Mongo reads) ---

var lcQueryREFrozen = regexp.MustCompile(`(?s)<!--\s*lc:query\b(.*?)-->`)

// ExtractLCQueryDirectives returns the raw directive comment strings the
// frozen expander will look up in LCQueryCache. Snapshot builders MUST key
// expansions by these exact (TrimSpace-normalized) strings — the frozen
// expander matches on them verbatim, so any other keying silently yields
// placeholder comments.
func ExtractLCQueryDirectives(layout string) []string {
	if !strings.Contains(layout, "lc:query") {
		return nil
	}
	raw := lcQueryREFrozen.FindAllString(layout, -1)
	out := make([]string, 0, len(raw))
	seen := map[string]struct{}{}
	for _, m := range raw {
		k := strings.TrimSpace(m)
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, m)
	}
	return out
}

func expandLCQueryFrozen(layout string, cache map[string]string) string {
	if !strings.Contains(layout, "lc:query") {
		return layout
	}
	return lcQueryREFrozen.ReplaceAllStringFunc(layout, func(match string) string {
		if cache == nil {
			return "<!-- lc:query unresolved: frozen snapshot has no cached result -->"
		}
		if out, ok := cache[strings.TrimSpace(match)]; ok {
			return out
		}
		if out, ok := cache["*"]; ok {
			return out
		}
		return "<!-- lc:query unresolved: frozen snapshot has no cached result -->"
	})
}

// --- frozen template execution / TOC / wikilinks ---

func executeLayoutFrozen(layout string, data map[string]any) (string, error) {
	t, err := template.New("content").Parse(layout)
	if err != nil {
		return "", renderErr(CodeRenderTemplate, fmt.Sprintf("failed to parse template: %v", err), err)
	}
	var buf strings.Builder
	if err := t.Execute(&buf, data); err != nil {
		return "", renderErr(CodeRenderTemplate, fmt.Sprintf("failed to execute template: %v", err), err)
	}
	return buf.String(), nil
}

const tocPlaceholderFrozen = "__LCTOC__"

var headingREFrozen = regexp.MustCompile(`(?is)<(h[1-6])([^>]*)>(.*?)</h[1-6]>`)
var headingWithIDREFrozen = regexp.MustCompile(`(?is)<(h[1-6])[^>]*\sid="([^"]+)"[^>]*>(.*?)</h[1-6]>`)
var tagStripREFrozen = regexp.MustCompile(`<[^>]+>`)
var slugifyREFrozen = regexp.MustCompile(`[^a-z0-9]+`)
var wikilinkREFrozen = regexp.MustCompile(`\[\[([^\]|]+)(?:\|([^\]]*))?\]\]`)

func headingSlugFrozen(text string) string {
	text = tagStripREFrozen.ReplaceAllString(text, "")
	text = strings.ToLower(strings.TrimSpace(text))
	text = slugifyREFrozen.ReplaceAllString(text, "-")
	return strings.Trim(text, "-")
}

func injectHeadingIDsFrozen(html string) string {
	return headingREFrozen.ReplaceAllStringFunc(html, func(match string) string {
		parts := headingREFrozen.FindStringSubmatch(match)
		if len(parts) < 4 {
			return match
		}
		tag, attrs, content := parts[1], parts[2], parts[3]
		if strings.Contains(strings.ToLower(attrs), "id=") {
			return match
		}
		id := headingSlugFrozen(content)
		if id == "" {
			return match
		}
		return fmt.Sprintf(`<%s id="%s"%s>%s</%s>`, tag, id, attrs, content, tag)
	})
}

func buildAndInjectTOCFrozen(html string) string {
	type tocEntry struct {
		level int
		id    string
		text  string
	}
	var entries []tocEntry
	headingWithIDREFrozen.ReplaceAllStringFunc(html, func(match string) string {
		parts := headingWithIDREFrozen.FindStringSubmatch(match)
		if len(parts) < 4 {
			return match
		}
		tag, id, content := parts[1], parts[2], parts[3]
		level := int(tag[1] - '0')
		text := tagStripREFrozen.ReplaceAllString(content, "")
		entries = append(entries, tocEntry{level, id, strings.TrimSpace(text)})
		return match
	})
	if len(entries) == 0 {
		return strings.ReplaceAll(html, tocPlaceholderFrozen, "")
	}
	var toc strings.Builder
	toc.WriteString(`<nav class="lc-toc"><ul>`)
	for _, e := range entries {
		toc.WriteString(fmt.Sprintf(`<li class="toc-h%d"><a href="#%s">%s</a></li>`,
			e.level, e.id, template.HTMLEscapeString(e.text)))
	}
	toc.WriteString(`</ul></nav>`)
	return strings.ReplaceAll(html, tocPlaceholderFrozen, toc.String())
}

type frozenWikilinkIndex struct {
	titleToPath map[string]string
	pathToTitle map[string]string
}

func newFrozenWikilinkIndex(titleToPath, pathToTitle map[string]string) *frozenWikilinkIndex {
	idx := &frozenWikilinkIndex{
		titleToPath: make(map[string]string),
		pathToTitle: make(map[string]string),
	}
	for k, v := range titleToPath {
		idx.titleToPath[strings.ToLower(k)] = v
	}
	for k, v := range pathToTitle {
		idx.pathToTitle[k] = v
	}
	return idx
}

func processWikiLinksFrozen(idx *frozenWikilinkIndex, html string) string {
	if !strings.Contains(html, "[[") {
		return html
	}
	if idx == nil {
		idx = newFrozenWikilinkIndex(nil, nil)
	}
	return wikilinkREFrozen.ReplaceAllStringFunc(html, func(match string) string {
		// Skip snippet-include syntax already handled upstream.
		if strings.HasPrefix(match, "[[include:") {
			return match
		}
		sub := wikilinkREFrozen.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		target := strings.TrimSpace(sub[1])
		var display string
		if len(sub) > 2 {
			display = strings.TrimSpace(sub[2])
		}
		var href, label string
		if strings.HasPrefix(target, "/") {
			if title, ok := idx.pathToTitle[target]; ok {
				href = target
				label = display
				if label == "" {
					label = title
				}
			}
		} else {
			if path, ok := idx.titleToPath[strings.ToLower(target)]; ok {
				href = path
				label = display
				if label == "" {
					label = target
				}
			}
		}
		if href == "" {
			dispText := display
			if dispText == "" {
				dispText = target
			}
			return `<span class="broken-link" data-target="` +
				template.HTMLEscapeString(target) + `">` +
				template.HTMLEscapeString(dispText) + `</span>`
		}
		return `<a href="` + template.HTMLEscapeString(href) + `">` +
			template.HTMLEscapeString(label) + `</a>`
	})
}

// --- frozen-copy helpers (snapshot immutability) ---

func deepCopyData(src map[string]any) map[string]any {
	if src == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func deepCopyFields(src []models.TemplateField) []models.TemplateField {
	if src == nil {
		return nil
	}
	out := make([]models.TemplateField, len(src))
	copy(out, src)
	for i := range out {
		if src[i].Validation.AllowedProtocols != nil {
			cp := make([]string, len(src[i].Validation.AllowedProtocols))
			copy(cp, src[i].Validation.AllowedProtocols)
			out[i].Validation.AllowedProtocols = cp
		}
	}
	return out
}

func deepCopySnapshot(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func copyStringMap(src map[string]string) map[string]string {
	if src == nil {
		return nil
	}
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func checkCtx(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
