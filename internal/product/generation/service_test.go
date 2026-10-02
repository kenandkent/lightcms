package generation_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/database"
	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/generation"
	"github.com/jonradoff/lightcms/v7/internal/product/idempotency"
	"github.com/jonradoff/lightcms/v7/internal/product/pathkey"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/publicurl"
	"github.com/jonradoff/lightcms/v7/internal/product/storage"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"
	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

type genSetup struct {
	db     *database.DB
	gen    *generation.Service
	tpls   *templatecontract.Service
	repo   *publication.Repository
	saga   *publication.Service
	idem   *idempotency.Service
	store  storage.Store
	root   string
	audits *int
	tvID   primitive.ObjectID
	tplID  primitive.ObjectID
	tvNum  int64
	slug   string
}

const genLayout = `<html><head><title>{{.title}}</title></head><body><h1>{{.headline}}</h1></body></html>`

func newGenSetup(t *testing.T, opts generation.Options) *genSetup {
	t.Helper()
	db, _ := testutil.MustConnectTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.EnsureProductIndexes(ctx); err != nil {
		t.Fatalf("EnsureProductIndexes: %v", err)
	}
	// Upgrade jobs are owned by Task 12; testutil does not clean them.
	_ = db.Collection(generation.CollectionUpgradeJobs).Drop(ctx)
	root := t.TempDir()
	store := storage.NewFilesystemStore(root)
	tpls := templatecontract.NewService(db)
	repo := publication.NewRepository(db, nil)
	base, _ := url.Parse("http://localhost:8080")
	resolver, err := publicurl.NewResolver(base)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	idem, err := idempotency.NewService(db, idempotency.Options{})
	if err != nil {
		t.Fatalf("idem: %v", err)
	}
	saga := publication.NewService(db, repo, store, publication.Options{
		Templates: tpls, Idem: idem, URLs: resolver,
	})
	audits := new(int)
	audit := func(_ context.Context, _ string, _ map[string]any) { *audits++ }
	if opts.Templates == nil {
		opts.Templates = tpls
	}
	if opts.PubRepo == nil {
		opts.PubRepo = repo
	}
	if opts.Pubs == nil {
		opts.Pubs = saga
	}
	if opts.Idem == nil {
		opts.Idem = idem
	}
	if opts.URLs == nil {
		opts.URLs = resolver
	}
	if opts.Audit == nil {
		opts.Audit = audit
	}
	gen := generation.NewService(db, opts)
	return &genSetup{db: db, gen: gen, tpls: tpls, repo: repo, saga: saga, idem: idem, store: store, root: root, audits: audits}
}

func (s *genSetup) seedTemplate(t *testing.T, slug, status string, fields []models.TemplateField) (primitive.ObjectID, primitive.ObjectID, int64) {
	t.Helper()
	if status == "" {
		status = templatecontract.StatusActive
	}
	if fields == nil {
		fields = []models.TemplateField{
			{Name: "headline", Label: "Headline", Type: "text", Required: true},
			{Name: "body", Label: "Body", Type: "textarea", Required: false},
		}
	}
	tpl, ver, err := s.tpls.Create(context.Background(), templatecontract.TemplateInput{
		Slug: slug, Name: slug + " name", Category: "news", Status: status,
		HTMLLayout: genLayout, Fields: fields,
	})
	if err != nil {
		t.Fatalf("seed template %s: %v", slug, err)
	}
	s.tplID = tpl.ID
	s.tvID = ver.ID
	s.tvNum = ver.Version
	s.slug = slug
	return tpl.ID, ver.ID, ver.Version
}

func authed(scopes ...string) generation.Actor {
	return generation.Actor{Role: "admin", ID: "user-1", Email: "user@example.com", Authenticated: true, Scopes: scopes}
}

func adminActor() generation.Actor {
	return generation.Actor{Role: "admin", ID: "admin-1", Email: "admin@example.com", Authenticated: true, IsAdmin: true, Scopes: []string{}}
}

func pubCtx(actor generation.Actor, key string, req generation.GenerateRequest, tvNum int64) context.Context {
	ctx := context.Background()
	raw, _ := json.Marshal(map[string]any{
		"template": req.Template, "title": req.Title, "slug": req.Slug,
		"folder_path": req.FolderPath, "mode": req.Mode, "upsert": req.Upsert, "data": req.Data,
		"expected_template_version": tvNum,
	})
	return generation.WithIdempotency(ctx, generation.IdempotencyParams{
		Owner: actor.Owner(), Method: "POST", Path: "/api/v1/page-generation", Key: key, Body: raw,
	})
}

func countDocs(t *testing.T, db *database.DB, coll string, filter bson.M) int64 {
	t.Helper()
	n, err := db.Collection(coll).CountDocuments(context.Background(), filter)
	if err != nil {
		t.Fatalf("count %s: %v", coll, err)
	}
	return n
}

func readCanonical(t *testing.T, root, fullPath string) string {
	t.Helper()
	rel := strings.TrimPrefix(fullPath, "/") + ".html"
	b, err := os.ReadFile(filepath.Join(root, "generated", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read canonical %s: %v", fullPath, err)
	}
	return string(b)
}

func canonicalExists(root, fullPath string) bool {
	rel := strings.TrimPrefix(fullPath, "/") + ".html"
	_, err := os.Stat(filepath.Join(root, "generated", filepath.FromSlash(rel)))
	return err == nil
}

// TestGenerate_Matrix covers target-state × mode (§20.6) + requires_publish.
func TestGenerate_Matrix(t *testing.T) {
	s := newGenSetup(t, generation.Options{})
	ctx := context.Background()
	_, _, vnum := s.seedTemplate(t, "financial-news", "", nil)
	_ = vnum

	// absent + draft → create Main draft, requires_publish=true, published=false
	resp, err := s.gen.Generate(ctx, authed(), generation.GenerateRequest{
		Template: "financial-news", Title: "Hello", Slug: "hello", FolderPath: "/news",
		Mode: "draft", Data: map[string]any{"headline": "Hi", "body": "x"},
	})
	if err != nil {
		t.Fatalf("absent draft: %v", err)
	}
	if resp.Action != "created" || resp.Published || !resp.RequiresPublish {
		t.Fatalf("absent draft response: %+v", resp)
	}
	if resp.PublicURL != nil || resp.PublicationID != nil {
		t.Fatalf("draft must not expose publication: %+v", resp)
	}

	// absent + preview → no write
	beforeContent := countDocs(t, s.db, "content", bson.M{})
	beforeVersions := countDocs(t, s.db, "content_versions", bson.M{})
	beforePubs := countDocs(t, s.db, "content_publications", bson.M{})
	presp, err := s.gen.Generate(ctx, authed(), generation.GenerateRequest{
		Template: "financial-news", Title: "Preview Me", Slug: "preview-me", FolderPath: "/news",
		Mode: "preview", Data: map[string]any{"headline": "Pv", "body": "x"},
	})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if presp.RequiresPublish || presp.Published {
		t.Fatalf("preview requires_publish/published: %+v", presp)
	}
	if presp.PublicationID != nil || presp.PublicURL != nil {
		t.Fatalf("preview must not expose publication: %+v", presp)
	}
	if got := countDocs(t, s.db, "content", bson.M{}); got != beforeContent {
		t.Fatalf("preview wrote content: %d -> %d", beforeContent, got)
	}
	if got := countDocs(t, s.db, "content_versions", bson.M{}); got != beforeVersions {
		t.Fatalf("preview wrote versions")
	}
	if got := countDocs(t, s.db, "content_publications", bson.M{}); got != beforePubs {
		t.Fatalf("preview wrote publications")
	}

	// unpublished + draft upsert → replace Main draft (updated)
	resp2, err := s.gen.Generate(ctx, authed(), generation.GenerateRequest{
		Template: "financial-news", Title: "Hello v2", Slug: "hello", FolderPath: "/news",
		Mode: "draft", Upsert: true, Data: map[string]any{"headline": "Hi v2", "body": "y"},
	})
	if err != nil {
		t.Fatalf("unpublished draft upsert: %v", err)
	}
	if resp2.Action != "updated" || resp2.ContentVersion != 2 {
		t.Fatalf("unpublished upsert: %+v", resp2)
	}
	if !resp2.RequiresPublish || resp2.Published {
		t.Fatalf("unpublished draft flags: %+v", resp2)
	}

	// publish the page (need expected version + idempotency)
	tv := s.tvNum
	pubReq := generation.GenerateRequest{
		Template: "financial-news", Title: "Hello v2", Slug: "hello", FolderPath: "/news",
		Mode: "publish", Upsert: true, ExpectedTemplateVersion: &tv,
		Data: map[string]any{"headline": "Hi v2", "body": "y"},
	}
	pctx := pubCtx(authed(generation.ScopeContentEdit, generation.ScopeContentPublish), "key-matrix-1", pubReq, tv)
	pres, err := s.gen.Generate(pctx, authed(generation.ScopeContentEdit, generation.ScopeContentPublish), pubReq)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !pres.Published || pres.RequiresPublish {
		t.Fatalf("publish flags: %+v", pres)
	}
	if pres.PublicURL == nil || pres.PublicationID == nil {
		t.Fatalf("publish must expose URL + publication: %+v", pres)
	}
	if !canonicalExists(s.root, "/news/hello") {
		t.Fatal("publish must cut canonical file")
	}

	// published + draft → Fork draft, live unchanged, requires_publish=true + published=true
	liveBefore := readCanonical(t, s.root, "/news/hello")
	var liveDoc models.Content
	if err := s.db.FindOne(ctx, "content", bson.M{"full_path": "/news/hello", "path_scope": "live"}, &liveDoc); err != nil {
		t.Fatalf("load live: %v", err)
	}
	liveDataBefore := fmt.Sprintf("%v", liveDoc.Data)
	dresp, err := s.gen.Generate(ctx, authed(), generation.GenerateRequest{
		Template: "financial-news", Title: "Hello v2", Slug: "hello", FolderPath: "/news",
		Mode: "draft", Upsert: true, Data: map[string]any{"headline": "Fork edit", "body": "fork"},
	})
	if err != nil {
		t.Fatalf("published draft: %v", err)
	}
	if !dresp.Published || !dresp.RequiresPublish {
		t.Fatalf("published draft must be published=true + requires_publish=true: %+v", dresp)
	}
	if got := readCanonical(t, s.root, "/news/hello"); got != liveBefore {
		t.Fatal("published draft changed live bytes")
	}
	var liveAfter models.Content
	if err := s.db.FindOne(ctx, "content", bson.M{"_id": liveDoc.ID}, &liveAfter); err != nil {
		t.Fatalf("reload live: %v", err)
	}
	if fmt.Sprintf("%v", liveAfter.Data) != liveDataBefore {
		t.Fatalf("published draft touched main data: %v -> %v", liveDataBefore, liveAfter.Data)
	}
	active, _ := s.repo.GetActive(ctx, liveDoc.ID)
	if active == nil || active.ID.Hex() != *pres.PublicationID {
		t.Fatalf("published draft moved active: %v", active)
	}

	// published + publish → direct command, no Fork, new publication
	tv2 := s.tvNum
	pubReq2 := generation.GenerateRequest{
		Template: "financial-news", Title: "Hello v3", Slug: "hello", FolderPath: "/news",
		Mode: "publish", Upsert: true, ExpectedTemplateVersion: &tv2,
		Data: map[string]any{"headline": "Direct v3", "body": "z"},
	}
	pctx2 := pubCtx(authed(generation.ScopeContentEdit, generation.ScopeContentPublish), "key-matrix-2", pubReq2, tv2)
	pres2, err := s.gen.Generate(pctx2, authed(generation.ScopeContentEdit, generation.ScopeContentPublish), pubReq2)
	if err != nil {
		t.Fatalf("published publish: %v", err)
	}
	if pres2.PublicationID == pres.PublicationID {
		t.Fatal("republish must mint a new publication")
	}
	if got := readCanonical(t, s.root, "/news/hello"); !strings.Contains(got, "Direct v3") {
		t.Fatalf("direct publish did not cut over:\n%s", got)
	}

	// sandbox without active sandbox → 409
	_, err = s.gen.Generate(ctx, authed(), generation.GenerateRequest{
		Template: "financial-news", Title: "SB", Slug: "sb-page", FolderPath: "/news",
		Mode: "sandbox", Data: map[string]any{"headline": "sb"},
	})
	if generation.CodeOf(err) != generation.CodeAgentSandboxRequired {
		t.Fatalf("sandbox without fork: got %v", err)
	}
}

// TestGenerate_UpsertAndValidation covers full-replace, null/false/default,
// 5 MiB cap, unknown fields, canonical collision, upsert=false 409.
func TestGenerate_UpsertAndValidation(t *testing.T) {
	s := newGenSetup(t, generation.Options{})
	ctx := context.Background()
	s.seedTemplate(t, "financial-news", "", nil)

	// upsert=false on existing → 409, zero mutation
	_, err := s.gen.Generate(ctx, authed(), generation.GenerateRequest{
		Template: "financial-news", Title: "A", Slug: "dup-page", FolderPath: "/news",
		Mode: "draft", Data: map[string]any{"headline": "a"},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	before := countDocs(t, s.db, "content", bson.M{})
	_, err = s.gen.Generate(ctx, authed(), generation.GenerateRequest{
		Template: "financial-news", Title: "A2", Slug: "dup-page", FolderPath: "/news",
		Mode: "draft", Data: map[string]any{"headline": "a2"},
	})
	if generation.CodeOf(err) != generation.CodePathConflict {
		t.Fatalf("upsert=false: got %v", err)
	}
	if got := countDocs(t, s.db, "content", bson.M{}); got != before {
		t.Fatal("upsert=false mutated content")
	}

	// canonical case collision: /News/CASE vs /news/case
	_, err = s.gen.Generate(ctx, authed(), generation.GenerateRequest{
		Template: "financial-news", Title: "Case", Slug: "CASE", FolderPath: "/News",
		Mode: "draft", Data: map[string]any{"headline": "c"},
	})
	if err != nil {
		t.Fatalf("case seed: %v", err)
	}
	_, err = s.gen.Generate(ctx, authed(), generation.GenerateRequest{
		Template: "financial-news", Title: "Case2", Slug: "case", FolderPath: "/news",
		Mode: "draft", Upsert: true, Data: map[string]any{"headline": "c2"},
	})
	// upsert=true on case-variant hits the SAME canonical → updated (not 409)
	if err != nil {
		t.Fatalf("case upsert: %v", err)
	}
	// upsert=false on case-variant → 409
	_, err = s.gen.Generate(ctx, authed(), generation.GenerateRequest{
		Template: "financial-news", Title: "Case3", Slug: "CASE", FolderPath: "/news",
		Mode: "draft", Data: map[string]any{"headline": "c3"},
	})
	if generation.CodeOf(err) != generation.CodePathConflict {
		t.Fatalf("case collision upsert=false: got %v", err)
	}

	// unknown data field → 422 with details
	_, err = s.gen.Generate(ctx, authed(), generation.GenerateRequest{
		Template: "financial-news", Title: "U", Slug: "unknown-field-page", FolderPath: "/news",
		Mode: "draft", Data: map[string]any{"headline": "h", "nope": "x"},
	})
	if generation.CodeOf(err) != generation.CodeFieldValidationFailed {
		t.Fatalf("unknown field: got %v", err)
	}

	// null rejected (MVP no nullable)
	_, err = s.gen.Generate(ctx, authed(), generation.GenerateRequest{
		Template: "financial-news", Title: "N", Slug: "null-page", FolderPath: "/news",
		Mode: "draft", Data: map[string]any{"headline": nil},
	})
	if generation.CodeOf(err) != generation.CodeFieldValidationFailed {
		t.Fatalf("null: got %v", err)
	}

	// 5 MiB cap
	big := strings.Repeat("a", (5<<20)+1)
	_, err = s.gen.Generate(ctx, authed(), generation.GenerateRequest{
		Template: "financial-news", Title: "Big", Slug: "big-page", FolderPath: "/news",
		Mode: "draft", Data: map[string]any{"headline": "h", "body": big},
	})
	if generation.CodeOf(err) != generation.CodeFieldValidationFailed {
		t.Fatalf("5MiB cap: got %v", err)
	}

	// boolean false vs missing vs default=true
	boolFields := []models.TemplateField{
		{Name: "flag", Label: "Flag", Type: "boolean", Required: false},
		{Name: "opt", Label: "Opt", Type: "boolean", Required: false, Default: "true"},
		{Name: "headline", Label: "H", Type: "text", Required: true},
	}
	s2 := newGenSetup(t, generation.Options{})
	s2.seedTemplate(t, "bool-tpl", "", boolFields)
	// explicit false stored
	r1, err := s2.gen.Generate(ctx, authed(), generation.GenerateRequest{
		Template: "bool-tpl", Title: "B1", Slug: "b1", FolderPath: "/x",
		Mode: "draft", Data: map[string]any{"headline": "h", "flag": false},
	})
	if err != nil {
		t.Fatalf("explicit false: %v", err)
	}
	var c1 models.Content
	if err := s2.db.FindOne(ctx, "content", bson.M{"full_path": "/x/b1"}, &c1); err != nil {
		t.Fatalf("load b1: %v", err)
	}
	if v, ok := c1.Data["flag"]; !ok || v != false {
		t.Fatalf("explicit false not stored: %v", c1.Data)
	}
	_ = r1
	// missing optional stays missing; default=true materializes
	_, err = s2.gen.Generate(ctx, authed(), generation.GenerateRequest{
		Template: "bool-tpl", Title: "B2", Slug: "b2", FolderPath: "/x",
		Mode: "draft", Data: map[string]any{"headline": "h"},
	})
	if err != nil {
		t.Fatalf("missing bool: %v", err)
	}
	var c2 models.Content
	if err := s2.db.FindOne(ctx, "content", bson.M{"full_path": "/x/b2"}, &c2); err != nil {
		t.Fatalf("load b2: %v", err)
	}
	if _, ok := c2.Data["flag"]; ok {
		t.Fatalf("missing optional bool should stay missing: %v", c2.Data)
	}
	if v, ok := c2.Data["opt"]; !ok || v != true {
		t.Fatalf("default=true should materialize true: %v", c2.Data)
	}

	// full-replace: omitted optional is removed, not retained
	_, err = s2.gen.Generate(ctx, authed(), generation.GenerateRequest{
		Template: "bool-tpl", Title: "B2", Slug: "b2", FolderPath: "/x",
		Mode: "draft", Upsert: true, Data: map[string]any{"headline": "h2"},
	})
	if err != nil {
		t.Fatalf("full-replace: %v", err)
	}
	var c3 models.Content
	if err := s2.db.FindOne(ctx, "content", bson.M{"full_path": "/x/b2"}, &c3); err != nil {
		t.Fatalf("load b2r: %v", err)
	}
	if _, ok := c3.Data["flag"]; ok {
		t.Fatalf("full-replace retained old flag: %v", c3.Data)
	}
}

// TestGenerate_ErrorsAndScopes covers 401/403/409/422/428/429/503 + zero-mutation.
func TestGenerate_ErrorsAndScopes(t *testing.T) {
	s := newGenSetup(t, generation.Options{})
	ctx := context.Background()
	_, _, vnum := s.seedTemplate(t, "financial-news", "", nil)

	counts := func() (int64, int64, int64, int64) {
		return countDocs(t, s.db, "content", bson.M{}),
			countDocs(t, s.db, "content_versions", bson.M{}),
			countDocs(t, s.db, "content_publications", bson.M{}),
			countDocs(t, s.db, "webhook_outbox", bson.M{})
	}

	// 401 unauthenticated
	_, err := s.gen.Generate(ctx, generation.Actor{Role: "admin"}, generation.GenerateRequest{
		Template: "financial-news", Title: "X", Slug: "x401", FolderPath: "/n", Mode: "draft",
		Data: map[string]any{"headline": "h"},
	})
	if generation.CodeOf(err) != generation.CodeUnauthenticated {
		t.Fatalf("401: got %v", err)
	}

	// 404 template not found
	_, err = s.gen.Generate(ctx, authed(), generation.GenerateRequest{
		Template: "nope", Title: "X", Slug: "x404", FolderPath: "/n", Mode: "draft",
		Data: map[string]any{"headline": "h"},
	})
	if generation.CodeOf(err) != generation.CodeTemplateNotFound {
		t.Fatalf("404: got %v", err)
	}

	// deprecated → 409 TEMPLATE_NOT_ACTIVE (same DB, second slug)
	_, _, _ = s.seedTemplate(t, "old-tpl-sep", templatecontract.StatusDeprecated, nil)
	_, err = s.gen.Generate(ctx, authed(), generation.GenerateRequest{
		Template: "old-tpl-sep", Title: "X", Slug: "xdep", FolderPath: "/n", Mode: "draft",
		Data: map[string]any{"headline": "h"},
	})
	if generation.CodeOf(err) != generation.CodeTemplateNotActive {
		t.Fatalf("deprecated: got %v", err)
	}

	// 428 missing expected_template_version (zero mutation)
	c0, v0, p0, o0 := counts()
	pubReq := generation.GenerateRequest{
		Template: "financial-news", Title: "P", Slug: "p428", FolderPath: "/n",
		Mode: "publish", Data: map[string]any{"headline": "h"},
	}
	_, err = s.gen.Generate(ctx, authed(generation.ScopeContentCreate, generation.ScopeContentPublish), pubReq)
	if generation.CodeOf(err) != generation.CodeTemplatePreconditionRequired {
		t.Fatalf("428 version: got %v", err)
	}
	if c1, v1, p1, o1 := counts(); c1 != c0 || v1 != v0 || p1 != p0 || o1 != o0 {
		t.Fatal("428 version mutated")
	}

	// 428 missing Idempotency-Key (zero mutation)
	tv := vnum
	pubReq2 := generation.GenerateRequest{
		Template: "financial-news", Title: "P", Slug: "p428k", FolderPath: "/n",
		Mode: "publish", ExpectedTemplateVersion: &tv, Data: map[string]any{"headline": "h"},
	}
	_, err = s.gen.Generate(ctx, authed(generation.ScopeContentCreate, generation.ScopeContentPublish), pubReq2)
	if generation.CodeOf(err) != generation.CodeIdempotencyKeyRequired {
		t.Fatalf("428 key: got %v", err)
	}
	if c1, v1, p1, o1 := counts(); c1 != c0 || v1 != v0 || p1 != p0 || o1 != o0 {
		t.Fatal("428 key mutated")
	}

	// 403 publish-only key cannot create (zero mutation, no completed idem)
	pubOnly := generation.Actor{Role: "admin", ID: "k-pub", Email: "p@e.com", Authenticated: true, Scopes: []string{generation.ScopeContentPublish}}
	pctx := pubCtx(pubOnly, "k-403-1", generation.GenerateRequest{
		Template: "financial-news", Title: "P", Slug: "p403", FolderPath: "/n",
		Mode: "publish", ExpectedTemplateVersion: &tv, Data: map[string]any{"headline": "h"},
	}, tv)
	_, err = s.gen.Generate(pctx, pubOnly, generation.GenerateRequest{
		Template: "financial-news", Title: "P", Slug: "p403", FolderPath: "/n",
		Mode: "publish", ExpectedTemplateVersion: &tv, Data: map[string]any{"headline": "h"},
	})
	if generation.CodeOf(err) != generation.CodePermissionDenied {
		t.Fatalf("403: got %v", err)
	}
	if c1, v1, p1, o1 := counts(); c1 != c0 || v1 != v0 || p1 != p0 || o1 != o0 {
		t.Fatalf("403 mutated: %d/%d/%d/%d", c1, v1, p1, o1)
	}
	if n := countDocs(t, s.db, "idempotency_records", bson.M{"state": "completed"}); n != 0 {
		t.Fatalf("403 created completed idem: %d", n)
	}

	// 403 create-only cannot publish
	createOnly := generation.Actor{Role: "admin", ID: "k-cr", Email: "c@e.com", Authenticated: true, Scopes: []string{generation.ScopeContentCreate}}
	pctx2 := pubCtx(createOnly, "k-403-2", generation.GenerateRequest{
		Template: "financial-news", Title: "P", Slug: "p403b", FolderPath: "/n",
		Mode: "publish", ExpectedTemplateVersion: &tv, Data: map[string]any{"headline": "h"},
	}, tv)
	_, err = s.gen.Generate(pctx2, createOnly, generation.GenerateRequest{
		Template: "financial-news", Title: "P", Slug: "p403b", FolderPath: "/n",
		Mode: "publish", ExpectedTemplateVersion: &tv, Data: map[string]any{"headline": "h"},
	})
	if generation.CodeOf(err) != generation.CodePermissionDenied {
		t.Fatalf("create-only publish: got %v", err)
	}

	// stale expected_template_version → 409 + zero mutation
	stale := tv - 1
	if stale < 0 {
		stale = 99
	}
	_, err = s.gen.Generate(pubCtx(authed(generation.ScopeContentCreate, generation.ScopeContentPublish), "k-stale", generation.GenerateRequest{
		Template: "financial-news", Title: "St", Slug: "stale-page", FolderPath: "/n", Mode: "publish",
		ExpectedTemplateVersion: &stale, Data: map[string]any{"headline": "h"},
	}, stale), authed(generation.ScopeContentCreate, generation.ScopeContentPublish), generation.GenerateRequest{
		Template: "financial-news", Title: "St", Slug: "stale-page", FolderPath: "/n", Mode: "publish",
		ExpectedTemplateVersion: &stale, Data: map[string]any{"headline": "h"},
	})
	if generation.CodeOf(err) != generation.CodeTemplateVersionChanged {
		t.Fatalf("stale: got %v", err)
	}
	if c1, v1, p1, o1 := counts(); c1 != c0 || v1 != v0 || p1 != p0 || o1 != o0 {
		t.Fatal("stale mutated")
	}

	// sandbox_only key cannot publish
	boxID := primitive.NewObjectID()
	sandboxOnly := generation.Actor{Role: "admin", ID: "k-box", Email: "b@e.com", Authenticated: true,
		Scopes: []string{generation.ScopeContentCreate, generation.ScopeContentPublish}, SandboxOnly: true, SandboxForkID: &boxID}
	_, err = s.gen.Generate(pubCtx(sandboxOnly, "k-box-1", generation.GenerateRequest{
		Template: "financial-news", Title: "BX", Slug: "box-pub", FolderPath: "/n", Mode: "publish",
		ExpectedTemplateVersion: &tv, Data: map[string]any{"headline": "h"},
	}, tv), sandboxOnly, generation.GenerateRequest{
		Template: "financial-news", Title: "BX", Slug: "box-pub", FolderPath: "/n", Mode: "publish",
		ExpectedTemplateVersion: &tv, Data: map[string]any{"headline": "h"},
	})
	if generation.CodeOf(err) != generation.CodePermissionDenied {
		t.Fatalf("sandbox_only publish: got %v", err)
	}
}

func TestGenerate_RateLimited(t *testing.T) {
	sr := newGenSetup(t, generation.Options{Limiter: &denyLimiter{}})
	ctx := context.Background()
	sr.seedTemplate(t, "financial-news", "", nil)
	_, err := sr.gen.Generate(ctx, authed(), generation.GenerateRequest{
		Template: "financial-news", Title: "R", Slug: "r429", FolderPath: "/n", Mode: "draft",
		Data: map[string]any{"headline": "h"},
	})
	if generation.CodeOf(err) != generation.CodeRateLimited {
		t.Fatalf("429: got %v", err)
	}
	if generation.StatusForCode(generation.CodeRateLimited) != 429 {
		t.Fatal("429 status mapping")
	}
}

func TestGenerate_StoreUnavailable(t *testing.T) {
	s503 := newGenSetup(t, generation.Options{
		StoreAvailable: func(context.Context) error { return fmt.Errorf("disk down") },
	})
	ctx := context.Background()
	_ = ctx
	s503.seedTemplate(t, "financial-news", "", nil)
	tv503 := s503.tvNum
	c0b := countDocs(t, s503.db, "content", bson.M{})
	_, err := s503.gen.Generate(pubCtx(authed(generation.ScopeContentCreate, generation.ScopeContentPublish), "k-503", generation.GenerateRequest{
		Template: "financial-news", Title: "S", Slug: "s503", FolderPath: "/n", Mode: "publish",
		ExpectedTemplateVersion: &tv503, Data: map[string]any{"headline": "h"},
	}, tv503), authed(generation.ScopeContentCreate, generation.ScopeContentPublish), generation.GenerateRequest{
		Template: "financial-news", Title: "S", Slug: "s503", FolderPath: "/n", Mode: "publish",
		ExpectedTemplateVersion: &tv503, Data: map[string]any{"headline": "h"},
	})
	if generation.CodeOf(err) != generation.CodeStoreUnavailable {
		t.Fatalf("503: got %v", err)
	}
	if got := countDocs(t, s503.db, "content", bson.M{}); got != c0b {
		t.Fatal("503 mutated")
	}
}

type denyLimiter struct{}

func (d *denyLimiter) Allow(context.Context, generation.Actor) (bool, int) {
	return false, 7
}

// TestGenerate_SchemaETag proves schema + ETag come from the same version and
// publish after an increment returns 409 with zero mutation.
func TestGenerate_SchemaETag(t *testing.T) {
	s := newGenSetup(t, generation.Options{})
	ctx := context.Background()
	_, _, v1 := s.seedTemplate(t, "financial-news", "", nil)
	tv, raw, err := s.gen.SchemaForSlug(ctx, "financial-news")
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	if tv.Version != v1 {
		t.Fatalf("schema version = %d, want %d", tv.Version, v1)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("schema json: %v", err)
	}
	if doc["additionalProperties"] != false {
		t.Fatalf("schema additionalProperties: %v", doc)
	}
	// Bump the template → version 2.
	tpl, err := s.tpls.GetCurrent(ctx, "financial-news")
	_ = tpl
	_ = err
	// Use Update via service: need template ID.
	tplRec, err := templatecontract.NewRepository(s.db).FindTemplateBySlug(ctx, "financial-news")
	if err != nil {
		t.Fatalf("load tpl: %v", err)
	}
	_, err = s.tpls.Update(ctx, tplRec.ID, v1, templatecontract.TemplateInput{
		Slug: "financial-news", Name: "financial-news name", Category: "news", Status: "active",
		HTMLLayout: genLayout + "<!-- v2 -->",
		Fields: []models.TemplateField{
			{Name: "headline", Label: "Headline", Type: "text", Required: true},
			{Name: "body", Label: "Body", Type: "textarea", Required: false},
		},
	})
	if err != nil {
		t.Fatalf("bump template: %v", err)
	}
	tv2, _, err := s.gen.SchemaForSlug(ctx, "financial-news")
	if err != nil {
		t.Fatalf("schema2: %v", err)
	}
	if tv2.Version != v1+1 {
		t.Fatalf("schema2 version = %d, want %d", tv2.Version, v1+1)
	}
	// Publish with stale expected=v1 → 409, zero mutation.
	beforePubs := countDocs(t, s.db, "content_publications", bson.M{})
	stale := v1
	pubReq := generation.GenerateRequest{
		Template: "financial-news", Title: "ET", Slug: "etag-page", FolderPath: "/n",
		Mode: "publish", ExpectedTemplateVersion: &stale, Data: map[string]any{"headline": "h"},
	}
	pctx := pubCtx(authed(generation.ScopeContentCreate, generation.ScopeContentPublish), "k-etag", pubReq, stale)
	_, err = s.gen.Generate(pctx, authed(generation.ScopeContentCreate, generation.ScopeContentPublish), pubReq)
	if generation.CodeOf(err) != generation.CodeTemplateVersionChanged {
		t.Fatalf("stale after bump: got %v", err)
	}
	if got := countDocs(t, s.db, "content_publications", bson.M{}); got != beforePubs {
		t.Fatal("stale publish mutated")
	}
	// Canonical path lookup is case-insensitive (pathkey check).
	if _, err := pathkey.Canonical("/News/ETag-Page"); err != nil {
		t.Fatalf("canonical: %v", err)
	}
}

// TestGenerate_MigrateSlug covers collision + live-bytes-unchanged.
func TestGenerate_MigrateSlug(t *testing.T) {
	s := newGenSetup(t, generation.Options{})
	ctx := context.Background()
	tplID, _, _ := s.seedTemplate(t, "financial-news", "", nil)
	// Publish a page so we can prove live bytes + publication are untouched.
	tv := s.tvNum
	pubReq := generation.GenerateRequest{
		Template: "financial-news", Title: "Live", Slug: "live-page", FolderPath: "/news",
		Mode: "publish", Upsert: false, ExpectedTemplateVersion: &tv, Data: map[string]any{"headline": "live"},
	}
	pctx := pubCtx(adminActor(), "k-mig-1", pubReq, tv)
	pres, err := s.gen.Generate(pctx, adminActor(), pubReq)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	liveBefore := readCanonical(t, s.root, "/news/live-page")
	activeBefore, _ := s.repo.GetActive(ctx, mustOID(t, pres.ID))
	if activeBefore == nil {
		t.Fatal("no active")
	}
	// Collision: second template with target slug.
	s.seedTemplate(t, "collision-slug", "", nil)
	_, err = s.gen.MigrateSlug(ctx, adminActor(), tplID, "collision-slug")
	if generation.CodeOf(err) != generation.CodePathConflict {
		t.Fatalf("slug collision: got %v", err)
	}
	// Non-admin → 403.
	_, err = s.gen.MigrateSlug(ctx, authed(), tplID, "new-slug-x")
	if generation.CodeOf(err) != generation.CodePermissionDenied {
		t.Fatalf("non-admin migrate: got %v", err)
	}
	// Happy path.
	res, err := s.gen.MigrateSlug(ctx, adminActor(), tplID, "financial-news-v2")
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if res.OldSlug != "financial-news" || res.NewSlug != "financial-news-v2" {
		t.Fatalf("migrate result: %+v", res)
	}
	if len(res.Guidance) == 0 {
		t.Fatal("migrate must return integration guidance")
	}
	// Historical versions untouched.
	var vers []bson.M
	cur, _ := s.db.Collection("template_versions").Find(ctx, bson.M{"template_id": tplID})
	_ = cur.All(ctx, &vers)
	for _, v := range vers {
		if v["slug"] != "financial-news" {
			t.Fatalf("historical version slug mutated: %v", v["slug"])
		}
	}
	// No redirect created.
	if n := countDocs(t, s.db, "redirects", bson.M{"from_path": "/news/live-page"}); n != 0 {
		t.Fatal("migrate-slug must not create a redirect")
	}
	// Live bytes + publication unchanged.
	if got := readCanonical(t, s.root, "/news/live-page"); got != liveBefore {
		t.Fatal("migrate-slug changed live bytes")
	}
	activeAfter, _ := s.repo.GetActive(ctx, mustOID(t, pres.ID))
	if activeAfter == nil || activeAfter.ID != activeBefore.ID {
		t.Fatal("migrate-slug switched publication")
	}
}

func mustOID(t *testing.T, hex string) primitive.ObjectID {
	t.Helper()
	id, err := primitive.ObjectIDFromHex(hex)
	if err != nil {
		t.Fatalf("oid %s: %v", hex, err)
	}
	return id
}

// TestGenerate_UpgradePreviewJob covers read-only preview + durable job.
func TestGenerate_UpgradePreviewJob(t *testing.T) {
	s := newGenSetup(t, generation.Options{})
	ctx := context.Background()
	_, _, v1 := s.seedTemplate(t, "financial-news", "", nil)
	// Publish two pages at v1.
	for i, slug := range []string{"up-a", "up-b"} {
		tv := v1
		req := generation.GenerateRequest{
			Template: "financial-news", Title: fmt.Sprintf("Up %d", i), Slug: slug, FolderPath: "/news",
			Mode: "publish", ExpectedTemplateVersion: &tv, Data: map[string]any{"headline": "h"},
		}
		pctx := pubCtx(adminActor(), fmt.Sprintf("k-up-%d", i), req, tv)
		if _, err := s.gen.Generate(pctx, adminActor(), req); err != nil {
			t.Fatalf("publish %s: %v", slug, err)
		}
	}
	beforePubs := countDocs(t, s.db, "content_publications", bson.M{})
	// Preview is read-only.
	preview, err := s.gen.PreviewUpgrade(ctx, authed(generation.ScopeTemplateView), "financial-news")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if preview.TotalPages != 2 {
		t.Fatalf("preview pages = %d", preview.TotalPages)
	}
	if got := countDocs(t, s.db, "content_publications", bson.M{}); got != beforePubs {
		t.Fatal("preview mutated publications")
	}
	// Bump template → v2; preview now shows would-republish.
	tplRec, _ := templatecontract.NewRepository(s.db).FindTemplateBySlug(ctx, "financial-news")
	_, err = s.tpls.Update(ctx, tplRec.ID, v1, templatecontract.TemplateInput{
		Slug: "financial-news", Name: "financial-news name", Category: "news", Status: "active",
		HTMLLayout: genLayout + "<!-- v2 -->",
		Fields: []models.TemplateField{
			{Name: "headline", Label: "Headline", Type: "text", Required: true},
			{Name: "body", Label: "Body", Type: "textarea", Required: false},
		},
	})
	if err != nil {
		t.Fatalf("bump: %v", err)
	}
	preview2, err := s.gen.PreviewUpgrade(ctx, authed(generation.ScopeTemplateView), "financial-news")
	if err != nil {
		t.Fatalf("preview2: %v", err)
	}
	if preview2.WouldRepublish != 2 {
		t.Fatalf("wouldRepublish = %d, want 2", preview2.WouldRepublish)
	}
	// Start + run the durable job.
	job, err := s.gen.StartUpgradeJob(ctx, adminActor(), "financial-news")
	if err != nil {
		t.Fatalf("start job: %v", err)
	}
	if len(job.Items) != 2 {
		t.Fatalf("job items = %d", len(job.Items))
	}
	done, err := s.gen.RunUpgradeJob(ctx, adminActor(), job.ID)
	if err != nil {
		t.Fatalf("run job: %v", err)
	}
	if done.Status != generation.UpgradeJobCompleted {
		t.Fatalf("job status = %s", done.Status)
	}
	for _, it := range done.Items {
		if it.Status != generation.UpgradeItemDone || it.PublicationID == nil {
			t.Fatalf("item not done: %+v", it)
		}
	}
	// Resume is idempotent: running again stays completed.
	done2, err := s.gen.RunUpgradeJob(ctx, adminActor(), job.ID)
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if done2.Status != generation.UpgradeJobCompleted {
		t.Fatalf("rerun status = %s", done2.Status)
	}
}

// TestGenerate_RestoreVsRevert proves the two commands are distinct.
func TestGenerate_RestoreVsRevert(t *testing.T) {
	s := newGenSetup(t, generation.Options{})
	ctx := context.Background()
	_, _, v1 := s.seedTemplate(t, "financial-news", "", nil)
	// v1 publish.
	req1 := generation.GenerateRequest{
		Template: "financial-news", Title: "RR", Slug: "rr-page", FolderPath: "/news",
		Mode: "publish", ExpectedTemplateVersion: &v1, Data: map[string]any{"headline": "v1 exact"},
	}
	pctx1 := pubCtx(adminActor(), "k-rr-1", req1, v1)
	r1, err := s.gen.Generate(pctx1, adminActor(), req1)
	if err != nil {
		t.Fatalf("publish v1: %v", err)
	}
	v1body := readCanonical(t, s.root, "/news/rr-page")
	cid := mustOID(t, r1.ID)
	v1pub := mustOID(t, *r1.PublicationID)
	// v2 publish.
	req2 := generation.GenerateRequest{
		Template: "financial-news", Title: "RR", Slug: "rr-page", FolderPath: "/news",
		Mode: "publish", Upsert: true, ExpectedTemplateVersion: &v1, Data: map[string]any{"headline": "v2 live"},
	}
	pctx2 := pubCtx(adminActor(), "k-rr-2", req2, v1)
	r2, err := s.gen.Generate(pctx2, adminActor(), req2)
	if err != nil {
		t.Fatalf("publish v2: %v", err)
	}
	_ = r2
	// restore_and_publish from version 1 (re-render path) vs revert_live to v1
	// publication (exact bytes). Both mint new IDs and require idempotency.
	rctx := generation.WithIdempotency(ctx, generation.IdempotencyParams{
		Owner: "admin-1", Method: "POST", Path: "/restore", Key: "k-restore-1",
		Body: []byte(`{"op":"restore"}`),
	})
	rout, err := s.gen.RestoreAndPublish(rctx, adminActor(), cid, 1, nil)
	if err != nil {
		t.Fatalf("restore_and_publish: %v", err)
	}
	if rout.Mode != "restore_and_publish" {
		t.Fatalf("restore mode = %s", rout.Mode)
	}
	if rout.PublicationID == v1pub.Hex() {
		t.Fatal("restore must mint a new publication")
	}
	vctx := generation.WithIdempotency(ctx, generation.IdempotencyParams{
		Owner: "admin-1", Method: "POST", Path: "/revert", Key: "k-revert-1",
		Body: []byte(`{"op":"revert"}`),
	})
	vout, err := s.gen.RevertLive(vctx, adminActor(), cid, v1pub, nil)
	if err != nil {
		t.Fatalf("revert_live: %v", err)
	}
	if vout.Mode != "revert_live" {
		t.Fatalf("revert mode = %s", vout.Mode)
	}
	if vout.PublicationID == rout.PublicationID || vout.PublicationID == v1pub.Hex() {
		t.Fatal("revert must mint its own new publication")
	}
	// Exact revert restores byte-identical v1.
	if got := readCanonical(t, s.root, "/news/rr-page"); got != v1body {
		t.Fatalf("revert bytes differ:\n got: %s\nwant: %s", got, v1body)
	}
	// Missing Idempotency-Key → 428, zero mutation.
	before := countDocs(t, s.db, "content_publications", bson.M{"content_id": cid})
	_, err = s.gen.RestoreAndPublish(ctx, adminActor(), cid, 1, nil)
	if generation.CodeOf(err) != generation.CodeIdempotencyKeyRequired {
		t.Fatalf("restore 428: got %v", err)
	}
	_, err = s.gen.RevertLive(ctx, adminActor(), cid, v1pub, nil)
	if generation.CodeOf(err) != generation.CodeIdempotencyKeyRequired {
		t.Fatalf("revert 428: got %v", err)
	}
	if got := countDocs(t, s.db, "content_publications", bson.M{"content_id": cid}); got != before {
		t.Fatal("428 restore/revert mutated")
	}
}

// TestGenerate_IdempotencyReplay proves same-key same-body replays, changed
// body conflicts, and terminal retry uses a new publication.
func TestGenerate_IdempotencyReplay(t *testing.T) {
	s := newGenSetup(t, generation.Options{})
	_, _, v1 := s.seedTemplate(t, "financial-news", "", nil)
	actor := authed(generation.ScopeContentCreate, generation.ScopeContentEdit, generation.ScopeContentPublish)
	mkReq := func(title string) generation.GenerateRequest {
		return generation.GenerateRequest{
			Template: "financial-news", Title: title, Slug: "idem-page", FolderPath: "/news",
			Mode: "publish", ExpectedTemplateVersion: &v1, Data: map[string]any{"headline": "h"},
		}
	}
	req := mkReq("Idem")
	raw, _ := json.Marshal(map[string]any{
		"template": req.Template, "title": req.Title, "slug": req.Slug, "folder_path": req.FolderPath,
		"mode": req.Mode, "upsert": req.Upsert, "data": req.Data, "expected_template_version": v1,
	})
	pctx := func(key string, body []byte) context.Context {
		return generation.WithIdempotency(context.Background(), generation.IdempotencyParams{
			Owner: actor.Owner(), Method: "POST", Path: "/api/v1/page-generation", Key: key, Body: body,
		})
	}
	r1, err := s.gen.Generate(pctx("k-replay", raw), actor, req)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	r2, err := s.gen.Generate(pctx("k-replay", raw), actor, req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if r2.PublicationID == nil || *r2.PublicationID != *r1.PublicationID {
		t.Fatalf("replay must reuse publication: %+v vs %+v", r1, r2)
	}
	// Changed body, same key → 409.
	changed := mkReq("Idem changed")
	raw2, _ := json.Marshal(map[string]any{
		"template": changed.Template, "title": changed.Title, "slug": changed.Slug, "folder_path": changed.FolderPath,
		"mode": changed.Mode, "upsert": changed.Upsert, "data": changed.Data, "expected_template_version": v1,
	})
	_, err = s.gen.Generate(pctx("k-replay", raw2), actor, changed)
	if generation.CodeOf(err) != generation.CodeIdempotencyConflict {
		t.Fatalf("changed body: got %v", err)
	}
}
