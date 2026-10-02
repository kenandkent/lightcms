package generation_test

// Task 17B coverage-gap tests: Generate validation matrix, fork/sandbox
// routing, idempotency-matrix publishes (external, DB-backed).

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/database"
	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/generation"
	"github.com/jonradoff/lightcms/v7/internal/product/idempotency"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/publicurl"
	"github.com/jonradoff/lightcms/v7/internal/product/storage"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"
	"github.com/jonradoff/lightcms/v7/internal/testutil"

	"net/url"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

type gapRateLimiter struct{ allow bool }

func (l gapRateLimiter) Allow(ctx context.Context, a generation.Actor) (bool, int) {
	if l.allow {
		return true, 0
	}
	return false, 9
}

func gapGenSetup(t *testing.T, mut func(*generation.Options)) (*generation.Service, *database.DB, *templatecontract.Service, *idempotency.Service, string) {
	t.Helper()
	db, _ := testutil.MustConnectTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.EnsureProductIndexes(ctx); err != nil {
		t.Fatalf("EnsureProductIndexes: %v", err)
	}
	_ = db.Collection(generation.CollectionUpgradeJobs).Drop(ctx)
	root := t.TempDir()
	store := storage.NewFilesystemStore(root)
	tpls := templatecontract.NewService(db)
	repo := publication.NewRepository(db, nil)
	base, _ := url.Parse("http://localhost:8080")
	resolver, _ := publicurl.NewResolver(base)
	idem, _ := idempotency.NewService(db, idempotency.Options{})
	saga := publication.NewService(db, repo, store, publication.Options{Templates: tpls, Idem: idem, URLs: resolver})
	audits := 0
	opts := generation.Options{
		Templates: tpls, Pubs: saga, PubRepo: repo, Idem: idem, URLs: resolver,
		Audit: func(ctx context.Context, action string, fields map[string]any) { audits++ },
	}
	if mut != nil {
		mut(&opts)
	}
	_ = audits
	return generation.NewService(db, opts), db, tpls, idem, root
}

func gapSeedTemplate(t *testing.T, tpls *templatecontract.Service, slug string) int64 {
	t.Helper()
	_, ver, err := tpls.Create(context.Background(), templatecontract.TemplateInput{
		Slug: slug, Name: slug + " name", Category: "news", Status: "active",
		HTMLLayout: genLayout,
		Fields: []models.TemplateField{
			{Name: "headline", Label: "Headline", Type: "text", Required: true},
			{Name: "body", Label: "Body", Type: "textarea", Required: false},
		},
	})
	if err != nil {
		t.Fatalf("seed %s: %v", slug, err)
	}
	return ver.Version
}

func gapDraftReq(slug, title, folder string, data map[string]any) generation.GenerateRequest {
	if folder == "" {
		folder = "/news"
	}
	if data == nil {
		data = map[string]any{"headline": "hello"}
	}
	return generation.GenerateRequest{
		Template: slug, Title: title, Slug: slug + "-page",
		FolderPath: folder, Mode: "draft", Data: data,
	}
}

func TestCoverGapGenerateValidation(t *testing.T) {
	gen, _, tpls, _, _ := gapGenSetup(t, nil)
	ctx := context.Background()
	tv := gapSeedTemplate(t, tpls, "gapval")

	if _, err := gen.Generate(ctx, authed(), generation.GenerateRequest{Mode: "bogus"}); generation.CodeOf(err) != generation.CodeInvalidRequest {
		t.Fatalf("unknown mode: %v", err)
	}
	if _, err := gen.Generate(ctx, generation.Actor{Role: "admin"}, gapDraftReq("gapval", "T", "", nil)); generation.CodeOf(err) != generation.CodeUnauthenticated {
		t.Fatalf("unauthenticated: %v", err)
	}
	if _, err := gen.Generate(ctx, authed(), generation.GenerateRequest{Mode: "draft"}); generation.CodeOf(err) != generation.CodeFieldValidationFailed {
		t.Fatalf("empty template: %v", err)
	}
	if _, err := gen.Generate(ctx, authed(), gapDraftReq("missing", "T", "", nil)); generation.CodeOf(err) != generation.CodeTemplateNotFound {
		t.Fatalf("unknown template: %v", err)
	}
	// Rate limiter deny (own setup at the end; each setup wipes the DB).
	// Store unavailable gate (publish only).
	// Publish preconditions.
	if _, err := gen.Generate(ctx, authed(), gapDraftReq("gapval", "T", "", nil)); err != nil {
		// draft ok baseline (also covers slug-from-nothing? no slug+title covered below)
		_ = err
	}
	pubNoVer := gapDraftReq("gapval", "T", "", nil)
	pubNoVer.Mode = "publish"
	if _, err := gen.Generate(ctx, authed(), pubNoVer); generation.CodeOf(err) != generation.CodeTemplatePreconditionRequired {
		t.Fatalf("publish no expected version: %v", err)
	}
	stale := tv + 5
	pubStale := pubNoVer
	pubStale.ExpectedTemplateVersion = &stale
	// R06 ordering: the key-presence 428 precedes the version-value check,
	// so the stale expectation needs a key to reach the 409.
	staleCtx := generation.WithIdempotency(ctx, generation.IdempotencyParams{
		Owner: "cover", Method: "POST", Path: "/p", Key: "k-gap-stale",
	})
	if _, err := gen.Generate(staleCtx, authed(), pubStale); generation.CodeOf(err) != generation.CodeTemplateVersionChanged {
		t.Fatalf("stale version: %v", err)
	}
	pubNoKey := pubNoVer
	pubNoKey.ExpectedTemplateVersion = &tv
	if _, err := gen.Generate(ctx, authed(), pubNoKey); generation.CodeOf(err) != generation.CodeIdempotencyKeyRequired {
		t.Fatalf("publish no key: %v", err)
	}
	// Data cap + unserializable.
	big := map[string]any{"headline": "h", "pad": strings.Repeat("x", 6<<20)}
	if _, err := gen.Generate(ctx, authed(), generation.GenerateRequest{
		Template: "gapval", Title: "T", Slug: "big", FolderPath: "/news", Mode: "draft", Data: big,
	}); generation.CodeOf(err) != generation.CodeFieldValidationFailed {
		t.Fatalf("data cap: %v", err)
	}
	if _, err := gen.Generate(ctx, authed(), generation.GenerateRequest{
		Template: "gapval", Title: "T", Slug: "badjson", FolderPath: "/news", Mode: "draft",
		Data: map[string]any{"f": func() {}},
	}); generation.CodeOf(err) != generation.CodeFieldValidationFailed {
		t.Fatalf("unserializable data: %v", err)
	}
	// Missing required headline → 422 (+ publish variant caches validation).
	badData := gapDraftReq("gapval", "T", "", map[string]any{})
	if _, err := gen.Generate(ctx, authed(), badData); generation.CodeOf(err) != generation.CodeFieldValidationFailed {
		t.Fatalf("validation fail: %v", err)
	}
	// Slug/title/path matrix.
	if _, err := gen.Generate(ctx, authed(), generation.GenerateRequest{
		Template: "gapval", FolderPath: "/news", Mode: "draft", Data: map[string]any{"headline": "h"},
	}); generation.CodeOf(err) != generation.CodePathInvalid {
		t.Fatalf("no slug+title: %v", err)
	}
	if _, err := gen.Generate(ctx, authed(), generation.GenerateRequest{
		Template: "gapval", Slug: "notitle", FolderPath: "/news", Mode: "draft", Data: map[string]any{"headline": "h"},
	}); generation.CodeOf(err) != generation.CodeFieldValidationFailed {
		t.Fatalf("no title on create: %v", err)
	}
	for _, slug := range []string{"a/b", ".", "..", "a\x01b", "a?b"} {
		r := gapDraftReq("gapval", "T", "", nil)
		r.Slug = slug
		if _, err := gen.Generate(ctx, authed(), r); generation.CodeOf(err) != generation.CodePathInvalid {
			t.Fatalf("bad slug %q: %v", slug, err)
		}
	}
	r := gapDraftReq("gapval", "T", "relative", nil)
	if _, err := gen.Generate(ctx, authed(), r); generation.CodeOf(err) != generation.CodePathInvalid {
		t.Fatalf("relative folder: %v", err)
	}
	r = gapDraftReq("gapval", "T", "/../escape", nil)
	if _, err := gen.Generate(ctx, authed(), r); generation.CodeOf(err) != generation.CodePathInvalid {
		t.Fatalf("escape folder: %v", err)
	}
	// Title-derived slug (covers slugifyTitle) incl. unicode + truncation.
	r = generation.GenerateRequest{Template: "gapval", Title: "Hello World — 2026! Café", FolderPath: "/news",
		Mode: "draft", Data: map[string]any{"headline": "h"}}
	resp, err := gen.Generate(ctx, authed(), r)
	if err != nil {
		t.Fatalf("slugify draft: %v", err)
	}
	if !strings.HasPrefix(resp.FullPath, "/news/hello-world") {
		t.Fatalf("slugified path = %q", resp.FullPath)
	}
	r2 := generation.GenerateRequest{Template: "gapval", Title: strings.Repeat("Very Long Title Segment ", 20),
		FolderPath: "/", Mode: "draft", Data: map[string]any{"headline": "h"}}
	if _, err := gen.Generate(ctx, authed(), r2); err != nil {
		t.Fatalf("long title slug: %v", err)
	}
	// Scope matrix: view-only actor cannot draft; sandbox-only key.
	viewOnly := authed("content.view")
	if _, err := gen.Generate(ctx, viewOnly, gapDraftReq("gapval", "T", "", nil)); generation.CodeOf(err) != generation.CodePermissionDenied {
		t.Fatalf("scope denied draft: %v", err)
	}
	if _, err := gen.Generate(ctx, viewOnly, generation.GenerateRequest{Template: "gapval", Title: "T",
		Slug: "preview-ok", FolderPath: "/news", Mode: "preview",
		Data: map[string]any{"headline": "h"}}); err != nil {
		t.Fatalf("preview with view scope: %v", err)
	}
	sandboxOnly := generation.Actor{Role: "admin", ID: "a", Email: "a@e", Authenticated: true, SandboxOnly: true,
		Scopes: []string{"content.create", "content.edit", "content.publish", "content.view"}}
	if _, err := gen.Generate(ctx, sandboxOnly, gapDraftReq("gapval", "T", "", nil)); generation.CodeOf(err) != generation.CodePermissionDenied {
		t.Fatalf("sandbox-only draft: %v", err)
	}
	if _, err := gen.Generate(ctx, sandboxOnly, generation.GenerateRequest{Template: "gapval", Title: "T",
		Slug: "sbx", FolderPath: "/news", Mode: "sandbox",
		Data: map[string]any{"headline": "h"}}); generation.CodeOf(err) != generation.CodeAgentSandboxRequired {
		t.Fatalf("sandbox without fork: %v", err)
	}
	// Upsert=false conflict + empty title on hit.
	first := gapDraftReq("gapval", "Upsert Me", "", nil)
	first.Slug = "upsert-me"
	if _, err := gen.Generate(ctx, authed(), first); err != nil {
		t.Fatalf("first draft: %v", err)
	}
	if _, err := gen.Generate(ctx, authed(), first); generation.CodeOf(err) != generation.CodePathConflict {
		t.Fatalf("upsert=false conflict: %v", err)
	}
	emptyTitle := first
	emptyTitle.Title = ""
	emptyTitle.Upsert = true
	if _, err := gen.Generate(ctx, authed(), emptyTitle); generation.CodeOf(err) != generation.CodeFieldValidationFailed {
		t.Fatalf("empty title on hit: %v", err)
	}

	// Separate setups (each wipes the shared DB): rate limiter deny.
	limGen, _, limTpls, _, _ := gapGenSetup(t, func(o *generation.Options) { o.Limiter = gapRateLimiter{allow: false} })
	gapSeedTemplate(t, limTpls, "gaplim")
	if _, err := limGen.Generate(ctx, authed(), gapDraftReq("gaplim", "T", "", nil)); generation.CodeOf(err) != generation.CodeRateLimited {
		t.Fatalf("rate limited: %v", err)
	}
	allowGen, _, allowTpls, _, _ := gapGenSetup(t, func(o *generation.Options) { o.Limiter = gapRateLimiter{allow: true} })
	gapSeedTemplate(t, allowTpls, "gaplim2")
	if _, err := allowGen.Generate(ctx, authed(), gapDraftReq("gaplim2", "T", "", nil)); err != nil {
		t.Fatalf("rate allow: %v", err)
	}
	// Store unavailable gate (publish only).
	soGen, _, soTpls, _, _ := gapGenSetup(t, func(o *generation.Options) {
		o.StoreAvailable = func(ctx context.Context) error { return errGapGen("store down") }
	})
	tvB := gapSeedTemplate(t, soTpls, "gapstore")
	preq := gapDraftReq("gapstore", "T", "", nil)
	preq.Mode = "publish"
	preq.ExpectedTemplateVersion = &tvB
	if _, err := soGen.Generate(pubCtx(authed(), "k-store-1", preq, tvB), authed(), preq); generation.CodeOf(err) != generation.CodeStoreUnavailable {
		t.Fatalf("store unavailable: %v", err)
	}
}

func TestCoverGapForkSandboxAndIdemMatrix(t *testing.T) {
	gen, db, tpls, idem, _ := gapGenSetup(t, nil)
	ctx := context.Background()
	tv := gapSeedTemplate(t, tpls, "gapfork")
	actor := authed()

	// Publish a page, then draft twice: fork create + fork reuse.
	pubReq := generation.GenerateRequest{Template: "gapfork", Title: "Fork Me", Slug: "fork-me",
		FolderPath: "/news", Mode: "publish", ExpectedTemplateVersion: &tv,
		Data: map[string]any{"headline": "v1"}}
	pres, err := gen.Generate(pubCtx(actor, "k-gapfork-1", pubReq, tv), actor, pubReq)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if pres.Action != "created" {
		t.Fatalf("publish action = %q", pres.Action)
	}
	draftReq := generation.GenerateRequest{Template: "gapfork", Title: "Fork Me", Slug: "fork-me",
		FolderPath: "/news", Mode: "draft", Upsert: true, Data: map[string]any{"headline": "v2"}}
	agent := actor
	agent.AgentSession = "sess-1"
	agent.ActorKind = "agent"
	d1, err := gen.Generate(ctx, agent, draftReq)
	if err != nil {
		t.Fatalf("fork create draft: %v", err)
	}
	if !d1.Published || !d1.RequiresPublish || d1.Action != "created" {
		t.Fatalf("fork draft = %+v", d1)
	}
	d2, err := gen.Generate(ctx, agent, draftReq)
	if err != nil {
		t.Fatalf("fork reuse draft: %v", err)
	}
	if d2.Action != "updated" {
		t.Fatalf("fork reuse action = %q", d2.Action)
	}

	// Sandbox flow with a real sandbox fork ID.
	forkID := primitive.NewObjectID()
	sandboxActor := generation.Actor{Role: "admin", ID: "ag", Email: "ag@e", Authenticated: true,
		Scopes: []string{}, SandboxForkID: &forkID, ActorKind: "agent"}
	sbReq := generation.GenerateRequest{Template: "gapfork", Title: "Sandbox Me", Slug: "sandbox-me",
		FolderPath: "/news", Mode: "sandbox", Data: map[string]any{"headline": "s1"}}
	s1, err := gen.Generate(ctx, sandboxActor, sbReq)
	if err != nil {
		t.Fatalf("sandbox create: %v", err)
	}
	if s1.Mode != "sandbox" || !s1.RequiresPublish {
		t.Fatalf("sandbox resp = %+v", s1)
	}
	s2, err := gen.Generate(ctx, sandboxActor, sbReq)
	if err != nil {
		t.Fatalf("sandbox reuse: %v", err)
	}
	if s2.Action != "updated" {
		t.Fatalf("sandbox reuse action = %q", s2.Action)
	}

	// Idempotency matrix on publish: conflict / in-progress / lease-expired.
	cReq := generation.GenerateRequest{Template: "gapfork", Title: "Idem", Slug: "idem-1",
		FolderPath: "/news", Mode: "publish", ExpectedTemplateVersion: &tv,
		Data: map[string]any{"headline": "i"}}
	rawA, _ := json.Marshal(map[string]any{"k": "A"})
	rawB, _ := json.Marshal(map[string]any{"k": "B"})
	opA, err := idem.Begin(ctx, actor.Owner(), "POST", "/p", "k-gap-idem", rawA)
	if err != nil {
		t.Fatalf("manual Begin: %v", err)
	}
	_ = opA
	// Same key + different body via Generate: conflict. Generate builds its
	// own canonical body, so any manual Begin with the same key conflicts.
	confCtx := generation.WithIdempotency(ctx, generation.IdempotencyParams{
		Owner: actor.Owner(), Method: "POST", Path: "/p", Key: "k-gap-idem", Body: rawB,
	})
	if _, err := gen.Generate(confCtx, actor, cReq); generation.CodeOf(err) != generation.CodeIdempotencyConflict {
		t.Fatalf("idem conflict: %v", err)
	}
	// Same key + same body while lease live: in-progress (beginForPublish
	// honors the caller-supplied body for the hash).
	liveCtx := generation.WithIdempotency(ctx, generation.IdempotencyParams{
		Owner: actor.Owner(), Method: "POST", Path: "/p", Key: "k-gap-idem", Body: rawA,
	})
	if _, err := gen.Generate(liveCtx, actor, cReq); generation.CodeOf(err) != generation.CodeRequestInProgress {
		t.Fatalf("in progress: %v", err)
	}
	// Expire the manual op lease, then Begin-equivalent via Generate with the
	// same body: the expired lease is CAS-taken-over and resumed (R04) —
	// nothing was ever written under it, so the publish completes instead
	// of wedging on REQUEST_IN_PROGRESS. Same key retried again replays
	// the minted publication.
	if _, err := db.Collection("idempotency_records").UpdateOne(ctx,
		bson.M{"owner": actor.Owner(), "key": "k-gap-idem"},
		bson.M{"$set": bson.M{"processing_expires_at": time.Now().Add(-time.Minute)}}); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	expCtx := generation.WithIdempotency(ctx, generation.IdempotencyParams{
		Owner: actor.Owner(), Method: "POST", Path: "/p", Key: "k-gap-idem", Body: rawA,
	})
	toRes, err := gen.Generate(expCtx, actor, cReq)
	if err != nil {
		t.Fatalf("lease takeover resume: %v", err)
	}
	if toRes.PublicationID == nil {
		t.Fatal("takeover resume must mint a publication")
	}
	toAgain, err := gen.Generate(expCtx, actor, cReq)
	if err != nil {
		t.Fatalf("post-takeover replay: %v", err)
	}
	if toAgain.PublicationID == nil || *toAgain.PublicationID != *toRes.PublicationID {
		t.Fatal("post-takeover retry must replay the same publication")
	}

	// Publish validation caching: invalid data + key → 422 (cached as
	// validation-only; a later VALID same-key publish with identical bytes
	// replays it — but validation always runs first, so exercise the cache
	// through a valid double-publish below).
	invReq := generation.GenerateRequest{Template: "gapfork", Title: "Inv", Slug: "inv-1",
		FolderPath: "/news", Mode: "publish", ExpectedTemplateVersion: &tv, Data: map[string]any{}}
	invCtx := pubCtx(actor, "k-gap-inv-1", invReq, tv)
	if _, err := gen.Generate(invCtx, actor, invReq); generation.CodeOf(err) != generation.CodeFieldValidationFailed {
		t.Fatalf("publish validation: %v", err)
	}

	// Valid double-publish with the same key: second replays from cache.
	repReq := generation.GenerateRequest{Template: "gapfork", Title: "Replay", Slug: "replay-1",
		FolderPath: "/news", Mode: "publish", ExpectedTemplateVersion: &tv,
		Data: map[string]any{"headline": "r"}}
	r1, err := gen.Generate(pubCtx(actor, "k-gap-replay-1", repReq, tv), actor, repReq)
	if err != nil {
		t.Fatalf("first publish: %v", err)
	}
	r2, err := gen.Generate(pubCtx(actor, "k-gap-replay-1", repReq, tv), actor, repReq)
	if err != nil {
		t.Fatalf("replay publish: %v", err)
	}
	if r2.ID != r1.ID || r2.PublicationID == nil || *r2.PublicationID != *r1.PublicationID {
		t.Fatalf("replay mismatch: %+v vs %+v", r1, r2)
	}
	if n := countDocs(t, db, "content_publications", bson.M{"content_id": mustObjectID(r1.ID)}); n != 1 {
		t.Fatalf("replay minted a second publication: %d", n)
	}
}

func mustObjectID(hex string) primitive.ObjectID {
	id, err := primitive.ObjectIDFromHex(hex)
	if err != nil {
		panic(err)
	}
	return id
}
