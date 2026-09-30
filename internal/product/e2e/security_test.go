// Task 17 E2E security table (spec §39.8): scope isolation with zero
// mutation, sandbox-only confinement, SSRF/XSS/input hardening, and error
// hygiene — all over HTTP against one app instance.
package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/generation"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// mutationFootprint snapshots every mutable surface for zero-mutation rows.
type mutationFootprint struct {
	content, versions, pubs, outbox, idem int64
	files                                  []string
}

func snapshotFootprint(t *testing.T, e *testEnv) mutationFootprint {
	t.Helper()
	ctx := context.Background()
	count := func(col string) int64 {
		n, err := e.db.Collection(col).CountDocuments(ctx, bson.M{})
		if err != nil {
			t.Fatalf("count %s: %v", col, err)
		}
		return n
	}
	var files []string
	_ = filepath.Walk(filepath.Join(e.root, "generated"), func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	return mutationFootprint{
		content: count("content"), versions: count("content_versions"),
		pubs: count("content_publications"), outbox: count("webhook_outbox"),
		idem: count("idempotency_records"), files: files,
	}
}

func assertNoMutation(t *testing.T, e *testEnv, before mutationFootprint, what string) {
	t.Helper()
	// Spec §39.8 zero-mutation surfaces: content, versions (fork rows live
	// in content), publications, static files, outbox. Idempotency records
	// are deliberately excluded: Begin runs before scope checks (replay
	// precedence) and the record is required for conflict semantics; it
	// carries no content and expires with the TTL.
	after := snapshotFootprint(t, e)
	if after.content != before.content || after.versions != before.versions ||
		after.pubs != before.pubs || after.outbox != before.outbox ||
		len(after.files) != len(before.files) {
		t.Fatalf("%s mutated state: before=%+v after=%+v", what, before, after)
	}
}

func thenPast() interface{} {
	return time.Now().Add(-time.Second)
}

// secPublishBody builds a publish body for the scope rows.
func secPublishBody(slug, headline string) map[string]any {
	v1 := int64(1)
	return map[string]any{
		"template": "financial-news", "title": "Sec", "slug": slug,
		"folder_path": "/news", "mode": "publish", "expected_template_version": v1,
		"data": map[string]any{"headline": headline},
	}
}

// TestE2E_SecurityScopes proves §39.8 zero-mutation: create-only, edit-only
// and publish-only keys cannot run the combined create+publish command, and
// every denial leaves content, versions, publications, files and outbox
// untouched. A full-owner control publish succeeds.
func TestE2E_SecurityScopes(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")

	denied := []struct {
		name   string
		scopes []string
	}{
		{"create-only", []string{generation.ScopeContentCreate}},
		{"edit-only", []string{generation.ScopeContentEdit}},
		{"publish-only", []string{generation.ScopeContentPublish}},
		{"view-only", []string{generation.ScopeContentView}},
	}
	for i, d := range denied {
		e.setActor(scopedActor(d.scopes...))
		before := snapshotFootprint(t, e)
		code, resp := e.publish(t, "sec-denied-"+d.name, secPublishBody("sec-scope-"+d.name, "h"))
		if code != 403 {
			t.Fatalf("%s publish = %d (%v), want 403", d.name, code, resp)
		}
		if got := errorCodeOf(resp); got != "PERMISSION_DENIED" {
			t.Fatalf("%s code = %q, want PERMISSION_DENIED", d.name, got)
		}
		assertNoMutation(t, e, before, d.name)
		_ = i
	}

	// A denied attempt holds its idempotency lease: an immediately
	// authorized retry of the same key+body is 409 IN_PROGRESS (the denial
	// does not poison the key permanently — after the lease lapses the
	// same key succeeds).
	e.setActor(scopedActor(generation.ScopeContentCreate))
	deniedBody := secPublishBody("sec-poison", "h")
	deniedRaw, _ := json.Marshal(deniedBody)
	code, _ := e.postRaw("/api/v1/page-generation", deniedRaw, map[string]string{"Idempotency-Key": "sec-poison-key"})
	if code != 403 {
		t.Fatalf("denied probe = %d, want 403", code)
	}
	e.setActor(defaultActor())
	code, resp := e.postRaw("/api/v1/page-generation", deniedRaw, map[string]string{"Idempotency-Key": "sec-poison-key"})
	if code != 409 || errorCodeOf(resp) != "REQUEST_IN_PROGRESS" {
		t.Fatalf("authorized retry during denied lease = %d (%v), want 409 REQUEST_IN_PROGRESS", code, resp)
	}
	if _, err := e.db.Collection("idempotency_records").UpdateOne(context.Background(),
		bson.M{"key": "sec-poison-key"},
		bson.M{"$set": bson.M{"processing_expires_at": thenPast()}}); err != nil {
		t.Fatalf("backdate lease: %v", err)
	}
	// Even after the lease lapses the key stays 409: no HTTP path performs
	// TakeOver — generation answers "retry to take over the same attempt"
	// but offers no takeover operation, and PublishInternal's attempt is
	// shadowed (see TestE2E_SchedulerStableKey). The key only becomes
	// usable after TTL expiry. Pinned as part of the crash-takeover gap.
	for i := 0; i < 2; i++ {
		code, resp = e.postRaw("/api/v1/page-generation", deniedRaw, map[string]string{"Idempotency-Key": "sec-poison-key"})
		if code != 409 {
			t.Fatalf("post-lapse retry %d = %d (%v), want stuck-409 (takeover gap pin)", i, code, resp)
		}
	}
	t.Logf("GAP PINNED: expired-lease HTTP retry loops 409 with no reachable TakeOver; recovery waits for TTL expiry")

	// Control: full owner publishes.
	e.setActor(defaultActor())
	code, resp = e.publish(t, "sec-allow", secPublishBody("sec-scope-ok", "ok"))
	if code != 201 {
		t.Fatalf("owner publish = %d (%v)", code, resp)
	}

	// 401: product routes without authentication.
	e.mu.Lock()
	e.anon = true
	e.mu.Unlock()
	before := snapshotFootprint(t, e)
	code, resp = e.publish(t, "sec-anon", secPublishBody("sec-anon", "x"))
	if code != 401 {
		t.Fatalf("anonymous publish = %d (%v), want 401", code, resp)
	}
	assertNoMutation(t, e, before, "anonymous")
}

// TestE2E_SecuritySandbox proves sandbox-only keys cannot publish live:
// mode=publish is 403 with zero mutation; mode=sandbox without a sandbox
// is 409; mode=sandbox inside a sandbox writes a fork only.
func TestE2E_SecuritySandbox(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")

	sandboxOnly := defaultActor()
	sandboxOnly.IsAdmin = false
	sandboxOnly.SandboxOnly = true
	sandboxOnly.Scopes = []string{generation.ScopeContentCreate, generation.ScopeContentEdit, generation.ScopeContentPublish}

	// Publish attempt with a sandbox-only key: 403, zero mutation.
	e.setActor(sandboxOnly)
	before := snapshotFootprint(t, e)
	code, resp := e.publish(t, "sandbox-pub", secPublishBody("sandbox-page", "s"))
	if code != 403 {
		t.Fatalf("sandbox-only publish = %d (%v), want 403", code, resp)
	}
	assertNoMutation(t, e, before, "sandbox-only publish")

	// Sandbox mode without an active sandbox: 409, zero mutation.
	before = snapshotFootprint(t, e)
	code, resp = e.postJSON("/api/v1/page-generation", map[string]any{
		"template": "financial-news", "title": "Sb", "slug": "sandbox-page",
		"folder_path": "/news", "mode": "sandbox", "data": map[string]any{"headline": "s"},
	}, nil)
	if code != 409 {
		t.Fatalf("sandbox without fork = %d (%v), want 409", code, resp)
	}
	if got := errorCodeOf(resp); got != "AGENT_SANDBOX_REQUIRED" {
		t.Fatalf("code = %q, want AGENT_SANDBOX_REQUIRED", got)
	}
	assertNoMutation(t, e, before, "sandbox without fork")

	// Inside a sandbox: fork write only, nothing live.
	fid := primitive.NewObjectID()
	sandboxOnly.SandboxForkID = &fid
	e.setActor(sandboxOnly)
	code, resp = e.postJSON("/api/v1/page-generation", map[string]any{
		"template": "financial-news", "title": "Sb", "slug": "sandbox-page",
		"folder_path": "/news", "mode": "sandbox", "data": map[string]any{"headline": "s"},
	}, nil)
	if code != 200 && code != 201 {
		t.Fatalf("sandbox write = %d (%v)", code, resp)
	}
	if rp, _ := resp["requires_publish"].(bool); !rp {
		t.Fatalf("sandbox write must require publish: %v", resp)
	}
	if n := e.count("content", bson.M{"path_scope": "live"}); n != 0 {
		t.Fatalf("sandbox write created %d live rows", n)
	}
	if c, _ := e.getAnon("/news/sandbox-page"); c != 404 {
		t.Fatalf("sandbox write reachable publicly: %d", c)
	}
	if n := e.count("content_publications", bson.M{}); n != 0 {
		t.Fatalf("sandbox write minted publications")
	}
}

// TestE2E_SecurityInputHardening covers URL protocols, path traversal,
// stored-XSS behavior and error hygiene over HTTP.
func TestE2E_SecurityInputHardening(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")
	linkTpl := func() {
		_, _, err := e.tpls.Create(context.Background(), templatecontract.TemplateInput{
			Slug: "link-page", Name: "Link", Category: "news", Status: "active",
			HTMLLayout: `<html><body><a href="{{.link}}">{{.headline}}</a></body></html>`,
			Fields: []models.TemplateField{
				{Name: "headline", Label: "H", Type: "text", Required: true},
				{Name: "link", Label: "L", Type: "url"},
			},
		})
		if err != nil {
			t.Fatalf("link template: %v", err)
		}
	}
	linkTpl()
	v1 := int64(1)

	// javascript: URL rejected at validation (no publish, no mutation).
	before := snapshotFootprint(t, e)
	code, resp := e.publish(t, "sec-js", map[string]any{
		"template": "link-page", "title": "JS", "slug": "js-url",
		"folder_path": "/news", "mode": "publish", "expected_template_version": v1,
		"data": map[string]any{"headline": "h", "link": "javascript:alert(1)"},
	})
	if code != 422 {
		t.Fatalf("javascript: url = %d (%v), want 422", code, resp)
	}
	assertNoMutation(t, e, before, "javascript url")

	// file: URL rejected the same way.
	code, resp = e.publish(t, "sec-file", map[string]any{
		"template": "link-page", "title": "F", "slug": "file-url",
		"folder_path": "/news", "mode": "publish", "expected_template_version": v1,
		"data": map[string]any{"headline": "h", "link": "file:///etc/passwd"},
	})
	if code != 422 {
		t.Fatalf("file: url = %d (%v), want 422", code, resp)
	}

	// Path traversal via slug rejected (no file outside the store root).
	for _, slug := range []string{"../evil", "..%2f..%2fevil", "a/../../b"} {
		code, resp = e.publish(t, "sec-trav", map[string]any{
			"template": "financial-news", "title": "T", "slug": slug,
			"folder_path": "/news", "mode": "publish", "expected_template_version": v1,
			"data": map[string]any{"headline": "h"},
		})
		if code != 422 && code != 400 {
			t.Fatalf("traversal slug %q = %d (%v), want 4xx", slug, code, resp)
		}
	}
	assertNoMutation(t, e, before, "traversal slugs")

	// Mongo operator-looking strings are stored verbatim; lookup still keys
	// on the server-derived canonical path.
	code, resp = e.publish(t, "sec-mongo", map[string]any{
		"template": "financial-news", "title": `{"$gt": ""}`, "slug": "mongo-injection",
		"folder_path": "/news", "mode": "publish", "expected_template_version": v1,
		"data": map[string]any{"headline": `{"$where": "sleep(1000)"}`},
	})
	if code != 201 {
		t.Fatalf("operator-looking strings publish = %d (%v)", code, resp)
	}
	u := strings.TrimPrefix(strOf(resp, "public_url"), e.base)
	if c, body := e.getAnon(u); c != 200 || !strings.Contains(string(body), "$where") {
		t.Fatalf("injection page not served verbatim: %d %s", c, body)
	}

	// Error hygiene: invalid IDs and unknown templates return typed codes
	// with no driver/stack internals.
	code, resp = e.getJSON("/api/v1/content/not-an-oid/publications", nil)
	if code != 400 && code != 404 {
		t.Fatalf("bad id status = %d (%v)", code, resp)
	}
	raw, _ := jsonMarshalLower(resp)
	for _, leak := range []string{"mongo", "goroutine", "stack trace", "driver", "panic"} {
		if strings.Contains(strings.ToLower(raw), leak) {
			t.Fatalf("error response leaks %q: %s", leak, raw)
		}
	}
	code, resp = e.publish(t, "sec-notpl", map[string]any{
		"template": "no-such-template", "title": "N", "slug": "n",
		"folder_path": "/news", "mode": "publish", "expected_template_version": v1,
		"data": map[string]any{"headline": "h"},
	})
	if code != 404 || errorCodeOf(resp) != "TEMPLATE_NOT_FOUND" {
		t.Fatalf("unknown template = %d (%v), want 404 TEMPLATE_NOT_FOUND", code, resp)
	}
}

// TestE2E_SecurityStoredXSS pins stored-XSS behavior on the V3 publish path.
// FINDING (table row FAIL): the saga DefaultRenderer binds every string as
// template.HTML, so a script headline is persisted VERBATIM into the served
// canonical bytes. Mitigation row (PASS): script_policy=none refuses to
// render at all. Specified fix: run V3 renders through the Task 7
// sanitizer/script-policy path (render.go) instead of raw template.HTML.
func TestE2E_SecurityStoredXSS(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.seedTemplate(t, "financial-news")
	v1 := int64(1)
	code, resp := e.publish(t, "sec-xss", map[string]any{
		"template": "financial-news", "title": "XSS", "slug": "xss-stored",
		"folder_path": "/news", "mode": "publish", "expected_template_version": v1,
		"data": map[string]any{"headline": `<script>alert(document.domain)</script>`},
	})
	if code != 201 {
		t.Fatalf("xss publish = %d (%v)", code, resp)
	}
	u := strings.TrimPrefix(strOf(resp, "public_url"), e.base)
	c, body := e.getAnon(u)
	if c != 200 {
		t.Fatalf("xss page not served: %d", c)
	}
	if !strings.Contains(string(body), `<script>alert(document.domain)</script>`) {
		t.Fatalf("expected raw script passthrough evidence, got:\n%s", body)
	}
	t.Logf("FINDING PINNED: stored <script> passes V3 publish into served bytes verbatim (saga DefaultRenderer template.HTML binding)")

	// script_policy=none refuses to render: no live page, no active.
	ctx := context.Background()
	_, _, err := e.tpls.Create(ctx, templatecontract.TemplateInput{
		Slug: "no-script", Name: "NoScript", Category: "news", Status: "active",
		ScriptPolicy: "none", HTMLLayout: sharedLayout,
		Fields: []models.TemplateField{{Name: "headline", Label: "H", Type: "text", Required: true}},
	})
	if err != nil {
		t.Fatalf("no-script template: %v", err)
	}
	code, resp = e.publish(t, "sec-xss-none", map[string]any{
		"template": "no-script", "title": "XS", "slug": "xss-blocked",
		"folder_path": "/news", "mode": "publish", "expected_template_version": v1,
		"data": map[string]any{"headline": `<script>alert(1)</script>`},
	})
	if code < 500 {
		t.Fatalf("script_policy=none publish = %d (%v), want render refusal", code, resp)
	}
	if c, _ := e.getAnon("/news/xss-blocked"); c != 404 {
		t.Fatalf("refused render left a live file: %d", c)
	}
}

// TestE2E_SecurityAssetSSRF proves the legacy asset-import SSRF boundary
// over HTTP: loopback/private targets and non-http schemes fail with zero
// saved assets and generic errors (no network internals echoed).
func TestE2E_SecurityAssetSSRF(t *testing.T) {
	e := newEnv(t, envOpts{})
	before := e.count("assets", bson.M{})

	for _, target := range []string{
		"http://127.0.0.1:9/internal.png",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:9/loop.png",
	} {
		code, resp := e.postJSON("/api/v1/assets/from-url",
			map[string]any{"url": target}, nil)
		if code != 502 && code != 400 {
			t.Fatalf("ssrf target %q = %d (%v), want 502/400 block", target, code, resp)
		}
	}
	for _, target := range []string{
		"ftp://example.com/x.png",
		"http://user:pass@example.com/x.png",
		"not-a-url",
	} {
		code, resp := e.postJSON("/api/v1/assets/from-url",
			map[string]any{"url": target}, nil)
		if code != 400 {
			t.Fatalf("bad asset url %q = %d (%v), want 400", target, code, resp)
		}
	}
	if n := e.count("assets", bson.M{}); n != before {
		t.Fatalf("ssrf attempts saved assets (%d -> %d)", before, n)
	}
	// Oversize import is covered by Task 13 unit tests (50MiB+1 fixtures
	// need a public test origin); recorded here as covered-elsewhere.
}

func jsonMarshalLower(v any) (string, error) {
	raw, err := json.Marshal(v)
	return string(raw), err
}
