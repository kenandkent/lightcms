// Package e2e implements Task 17: whole-system contract and fault tests.
//
// Every test runs against ONE application instance (an httptest-style server
// that wires the exact Task 16 production runtime: filesystem store,
// publication saga, idempotency service, public URL resolver, generation
// orchestrator, product HTTP handlers, legacy publish/unpublish/regenerate
// handlers, the recovery scanner and the outbox worker) backed by the Task 0
// replica-set MongoDB and a temporary persistent directory.
//
// No DB-backed assertion may skip: requireLiveMongo fails the test when no
// replica-set URI is configured (Global Constraints: a green run with DB
// tests skipped is NOT integration evidence).
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/jonradoff/lightcms/v7/internal/auth"
	"github.com/jonradoff/lightcms/v7/internal/database"
	"github.com/jonradoff/lightcms/v7/internal/handlers"
	"github.com/jonradoff/lightcms/v7/internal/middleware"
	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/generation"
	"github.com/jonradoff/lightcms/v7/internal/product/httpapi"
	"github.com/jonradoff/lightcms/v7/internal/product/idempotency"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/publicurl"
	"github.com/jonradoff/lightcms/v7/internal/product/storage"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"
	"github.com/jonradoff/lightcms/v7/internal/services"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

// e2eDBName is the DEDICATED e2e database (Task 17A isolation hardening).
// It MUST contain "test" per the safety guard in internal/testutil/testutil.go
// (MustConnectTestDB / CleanupCollections refuse non-test names). The e2e
// package previously shared `lightcms-test` with every other DB suite and
// left data behind (e.g. the `financial-news` slug), breaking suites run
// after it (TEMPLATE_SLUG_CONFLICT) and causing IndexBuildAborted flakiness
// from concurrent index builds. A dedicated DB makes the package hermetic.
//
// Resolution order (mirrors testutil.loadEnvTest + DATABASE_NAME override):
// MONGODB_URI comes from the environment or the repo-root .env.test walk
// (stock .env.test works unchanged); the DB NAME is always forced to
// e2eDBName and never taken from .env.test/DATABASE_NAME, so CI with a stock
// .env.test (DATABASE_NAME=lightcms-test) still isolates e2e writes.
const e2eDBName = "lightcms-test-e2e"

var (
	e2eOnce sync.Once
	e2eDB   *database.DB
	e2eErr  error
)

// TestMain forces the dedicated DB name process-wide (defense in depth: any
// code path that still reads DATABASE_NAME, including a future
// testutil.MustConnectTestDB call, lands in the e2e database) before running
// the package. NOTE: testutil.loadEnvTest overwrites DATABASE_NAME from
// .env.test on first connect, so the name is ALSO forced inside
// mustConnectE2EDB; TestMain alone would not survive a stock .env.test.
func TestMain(m *testing.M) {
	_ = os.Setenv("DATABASE_NAME", e2eDBName)
	os.Exit(m.Run())
}

// resolveE2EMongoURI returns the test replica-set URI from the environment,
// falling back to the .env.test walk (same search as requireLiveMongo and
// testutil.loadEnvTest: cwd up to 6 levels). It never touches DATABASE_NAME.
func resolveE2EMongoURI() string {
	if uri := strings.TrimSpace(os.Getenv("MONGODB_URI")); uri != "" {
		return uri
	}
	dir, _ := os.Getwd()
	for i := 0; i < 6; i++ {
		data, err := os.ReadFile(filepath.Join(dir, ".env.test"))
		if err == nil {
			sc := bufio.NewScanner(strings.NewReader(string(data)))
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				if idx := strings.IndexByte(line, '='); idx > 0 {
					key := strings.TrimSpace(line[:idx])
					val := strings.TrimSpace(line[idx+1:])
					if key == "MONGODB_URI" && val != "" {
						return val
					}
				}
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// wipeE2EDB drops the ENTIRE dedicated database (single round-trip). The
// guard refuses anything but e2eDBName so a wiring bug can never wipe the
// shared `lightcms-test` DB or production. A full drop (rather than
// testutil.CleanupCollections' per-collection drops) is hermetic against
// collections missing from that list (e.g. template_upgrade_jobs,
// import_logs) and any future product collections.
func wipeE2EDB(t *testing.T, db *database.DB) {
	t.Helper()
	if got := db.DatabaseName(); got != e2eDBName {
		t.Fatalf("e2e: refusing to wipe unexpected database %q (want %q)", got, e2eDBName)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.Collection("content").Database().Drop(ctx); err != nil {
		t.Fatalf("e2e: drop database %q: %v", e2eDBName, err)
	}
}

// mustConnectE2EDB returns the shared connection to the DEDICATED e2e
// database, wiped BEFORE the test runs, and registers a t.Cleanup wipe so
// each test also LEAVES the DB clean. Every test therefore starts clean AND
// leaves clean; no test may depend on execution order or assume an
// empty/full shared DB.
func mustConnectE2EDB(t *testing.T) *database.DB {
	t.Helper()
	requireLiveMongo(t)
	e2eOnce.Do(func() {
		uri := resolveE2EMongoURI()
		if uri == "" {
			return // e2eDB stays nil; caller fails via requireLiveMongo below
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		wc := writeconcern.New(writeconcern.WMajority())
		db, err := database.Connect(ctx, uri, e2eDBName, options.Client().SetWriteConcern(wc))
		if err != nil {
			e2eErr = fmt.Errorf("e2e: failed to connect to dedicated database %q: %w", e2eDBName, err)
			return
		}
		e2eDB = db
	})
	if e2eErr != nil {
		t.Fatalf("e2e: %v", e2eErr)
	}
	if e2eDB == nil {
		t.Fatalf("E2E requires a replica-set test MongoDB: set MONGODB_URI or create .env.test (see docs/implementation/test-environment.md section 2). Refusing to record skipped-DB evidence.")
	}
	// Start-clean: each test (including a second newEnv within one test)
	// begins from an empty dedicated DB.
	wipeE2EDB(t, e2eDB)
	// End-clean: leave the dedicated DB empty for the next test AND for any
	// suite run after this package. DB drops are idempotent, so double-wipes
	// (start of next test + cleanup of previous) are harmless. A single Drop
	// covers every collection (no allow-list to drift out of sync).
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if e2eDB == nil {
			return
		}
		if got := e2eDB.DatabaseName(); got != e2eDBName {
			return
		}
		_ = e2eDB.Collection("content").Database().Drop(ctx)
	})
	return e2eDB
}

// sharedLayout is the deterministic template layout used by most E2E pages.
// It renders the frozen render snapshot (title, headline, publication_id,
// logical published_at, public_url) so tests can prove snapshot freezing.
const sharedLayout = `<html><head><title>{{.title}}</title></head><body><h1>{{.headline}}</h1><p data-pub="{{.publication_id}}">{{.published_at}}</p><a href="{{.public_url}}">link</a></body></html>`

// requireLiveMongo enforces the Task 17 evidence rule: DB-backed tests FAIL
// (never skip) when no test Mongo URI is configured.
func requireLiveMongo(t *testing.T) {
	t.Helper()
	if strings.TrimSpace(os.Getenv("MONGODB_URI")) != "" {
		return
	}
	dir, _ := os.Getwd()
	found := false
	for i := 0; i < 6; i++ {
		data, err := os.ReadFile(filepath.Join(dir, ".env.test"))
		if err == nil {
			sc := bufio.NewScanner(strings.NewReader(string(data)))
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if strings.HasPrefix(line, "MONGODB_URI=") && strings.TrimSpace(strings.TrimPrefix(line, "MONGODB_URI=")) != "" {
					found = true
					break
				}
			}
		}
		if found {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if !found {
		t.Fatalf("E2E requires a replica-set test MongoDB: set MONGODB_URI or create .env.test (see docs/implementation/test-environment.md section 2). Refusing to record skipped-DB evidence.")
	}
}

// deliveredEvent records one outbox delivery attempt observed by the test.
type deliveredEvent struct {
	Type    string
	ID      string
	Payload map[string]any
}

// envOpts customises one test application instance. Zero values select the
// production-equivalent defaults.
type envOpts struct {
	// faults injects saga fault hooks (publication.Faults). Production
	// leaves it zero; fault-table rows set BeforeCommit hooks.
	faults publication.Faults
	// renderer overrides the saga renderer (nil = production DefaultRenderer).
	renderer publication.Renderer
	// maxWriteBytes truncates store writes (stage/verify failure simulation).
	maxWriteBytes int64
	// buildSHA is stamped into publication records (proves the 16E
	// ProductBuildSHA plumbing; production uses the ldflag git SHA).
	buildSHA string
	// actor overrides the default admin product actor.
	actor *generation.Actor
	// anon makes product routes reject as unauthenticated (401 row).
	anon bool
	// scanTimeout overrides the scanner stale-stage horizon.
	scanTimeout time.Duration
}

// testEnv is ONE LightCMS application instance: the Task 16 single runtime
// (store, saga, idempotency, URLs, generation, product + legacy handlers,
// scanner, outbox worker) served over HTTP against the test replica set.
type testEnv struct {
	t          *testing.T
	db         *database.DB
	root       string
	store      *storage.FilesystemStore
	idem       *idempotency.Service
	repo       *publication.Repository
	tpls       *templatecontract.Service
	saga       *publication.Service
	gen        *generation.Service
	api        *httpapi.Handlers
	legacy     *handlers.APIHandler
	forks      *services.ForkService
	contentSvc *services.ContentService
	scanner    *publication.Scanner
	outbox     *publication.OutboxWorker
	base       string
	resolver   *publicurl.Resolver

	router   *mux.Router
	listener net.Listener

	mu         sync.Mutex
	actor      generation.Actor
	anon       bool
	purges     [][]string
	deliveries []deliveredEvent
	deliverErr error
	purgeErr   error
	alerts     []publication.Alert
}

// defaultActor is the full-owner admin actor (empty scopes = owner).
func defaultActor() generation.Actor {
	return generation.Actor{
		ID: "e2e-admin", Email: "admin@e2e.test", Authenticated: true,
		IsAdmin: true, Scopes: []string{}, Via: "api", ActorKind: "human",
	}
}

// newEnv builds one application instance with a wiped DEDICATED e2e database
// (lightcms-test-e2e, never the shared lightcms-test) and a fresh temporary
// store root. mustConnectE2EDB wipes before AND registers a wipe-after via
// t.Cleanup, so each test starts clean and leaves clean with no ordering
// dependency.
func newEnv(t *testing.T, opts envOpts) *testEnv {
	t.Helper()
	db := mustConnectE2EDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.EnsureProductIndexes(ctx); err != nil {
		t.Fatalf("EnsureProductIndexes: %v", err)
	}
	_ = db.Collection(generation.CollectionUpgradeJobs).Drop(ctx)

	e := &testEnv{t: t, db: db, root: t.TempDir()}
	if opts.actor != nil {
		e.actor = *opts.actor
	} else {
		e.actor = defaultActor()
	}
	e.anon = opts.anon
	e.wire(opts, true)
	return e
}

// wire builds every service for this instance. When first is true it also
// opens the listener and starts HTTP serving; otherwise it rewires the
// services in place (same listener, same data) to model a same-binary
// process restart with cleared fault hooks.
func (e *testEnv) wire(opts envOpts, first bool) {
	t := e.t
	e.store = storage.NewFilesystemStore(e.root)
	e.store.MaxWriteBytes = opts.maxWriteBytes

	if first {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		e.listener = ln
		e.base = "http://" + ln.Addr().String()
		baseURL, err := url.Parse(e.base)
		if err != nil {
			t.Fatalf("base url: %v", err)
		}
		resolver, err := publicurl.NewResolver(baseURL)
		if err != nil {
			t.Fatalf("resolver: %v", err)
		}
		e.resolver = resolver
		idem, err := idempotency.NewService(e.db, idempotency.Options{})
		if err != nil {
			t.Fatalf("idem: %v", err)
		}
		e.idem = idem
	}

	sha := opts.buildSHA
	if sha == "" {
		sha = "task17-e2e-sha"
	}
	e.repo = publication.NewRepository(e.db, nil)
	e.tpls = templatecontract.NewService(e.db)
	e.saga = publication.NewService(e.db, e.repo, e.store, publication.Options{
		Templates: e.tpls, Idem: e.idem, URLs: e.resolver,
		Renderer:  opts.renderer, Faults: opts.faults,
		Purge: func(ctx context.Context, urls []string) error {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.purges = append(e.purges, append([]string{}, urls...))
			return e.purgeErr
		},
		BuildSHA: sha,
	})
	e.gen = generation.NewService(e.db, generation.Options{
		Templates: e.tpls, Pubs: e.saga, PubRepo: e.repo, Idem: e.idem, URLs: e.resolver,
	})
	e.api = &httpapi.Handlers{
		Gen: e.gen,
		ActorExtractor: func(r *http.Request) (generation.Actor, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			if e.anon {
				return generation.Actor{}, fmt.Errorf("authentication is required")
			}
			return e.actor, nil
		},
		IdempotencyExtractor: func(r *http.Request) (string, bool) {
			k := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
			return k, k != ""
		},
	}

	// Legacy entry points (Task 16C wiring, same services as production
	// main.go): single publish / unpublish / regenerate / by-path / assets.
	e.contentSvc = services.NewContentService(e.db)
	templateSvc := services.NewTemplateService(e.db, e.contentSvc)
	assetSvc := services.NewAssetService(e.db)
	settingsSvc := services.NewSettingsService(e.db, e.contentSvc)
	apiKeySvc := services.NewAPIKeyService(e.db)
	auditSvc := services.NewAuditService(e.db)
	snippetSvc := services.NewSnippetService(e.db)
	e.legacy = handlers.NewAPIHandler(e.contentSvc, templateSvc, assetSvc, settingsSvc, apiKeySvc, auditSvc, snippetSvc)
	e.legacy.SetPublicationRuntime(e.saga, e.idem, e.gen)
	e.forks = services.NewForkService(e.db, e.contentSvc)

	// Legacy delegation seams (Task 16C/16D globals, pointed at this
	// instance — same as production main.go wiring).
	services.SetPublicationPublisher(e.saga)
	services.SetInternalIdempotency(e.idem)

	e.outbox = publication.NewOutboxWorker(e.db,
		func(ctx context.Context, eventType, eventID string, payload map[string]any) error {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.deliveries = append(e.deliveries, deliveredEvent{Type: eventType, ID: eventID, Payload: payload})
			return e.deliverErr
		}, publication.WorkerOptions{})
	scanTimeout := opts.scanTimeout
	e.scanner = publication.NewScanner(e.db, e.repo, e.store, publication.ScannerOptions{
		StageTimeout: scanTimeout,
		Alert: func(a publication.Alert) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.alerts = append(e.alerts, a)
		},
	})

	if first {
		e.router = e.buildRouter()
		ln := e.listener
		go func() {
			_ = http.Serve(ln, e.router)
		}()
		t.Cleanup(func() { _ = ln.Close() })
	} else {
		// Rewire the live router handlers to the new service set: the
		// handler closures below read e.api / e.legacy dynamically.
	}
}

// refault models a same-binary process restart: identical listener, data
// and store root, with new (usually cleared) fault hooks.
func (e *testEnv) refault(opts envOpts) {
	e.t.Helper()
	if opts.actor != nil {
		e.mu.Lock()
		e.actor = *opts.actor
		e.mu.Unlock()
	}
	// Preserve the store root listener/base/resolver/idem; rebuild saga+.
	keepStore := e.store
	_ = keepStore
	e.wire(envOpts{
		faults:        opts.faults,
		renderer:      opts.renderer,
		maxWriteBytes: opts.maxWriteBytes,
		buildSHA:      opts.buildSHA,
		scanTimeout:   opts.scanTimeout,
	}, false)
}

// buildRouter mirrors the production route table (cmd/server/main.go) for
// the entries under test: specific V3 paths stay above generic ones, one
// shared auth/provenance chain, anonymous static serving last.
func (e *testEnv) buildRouter() *mux.Router {
	r := mux.NewRouter()
	// Test auth chain: stamp the admin API user + provenance, mirroring the
	// production /api/v1 middleware (auth, provenance, editor email).
	admin := &auth.SessionUser{
		ID: "e2e-admin", Email: "admin@e2e.test", Role: models.RoleAdmin, ViaAPIKey: true,
	}
	withAuth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := req.Context()
			ctx = middleware.InjectAPIUser(ctx, admin)
			ctx = services.WithProvenance(ctx, services.Provenance{Actor: "human", Via: "api"})
			ctx = services.WithEditorEmail(ctx, "admin@e2e.test")
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	api := r.PathPrefix("/api/v1").Subrouter()
	api.Use(withAuth)
	// V3 product routes (Task 12/16C registration order).
	api.HandleFunc("/page-generation", func(w http.ResponseWriter, req *http.Request) {
		e.api.HandleGenerate(w, req)
	}).Methods("POST")
	api.HandleFunc("/templates/upgrade-jobs/{job_id}/run", func(w http.ResponseWriter, req *http.Request) {
		e.api.HandleRunUpgradeJob(w, req)
	}).Methods("POST")
	api.HandleFunc("/templates/upgrade-jobs/{job_id}", func(w http.ResponseWriter, req *http.Request) {
		e.api.HandleGetUpgradeJob(w, req)
	}).Methods("GET")
	api.HandleFunc("/templates/{slug}/schema", func(w http.ResponseWriter, req *http.Request) {
		e.api.HandleTemplateSchema(w, req)
	}).Methods("GET")
	api.HandleFunc("/templates/{slug}/upgrade-preview", func(w http.ResponseWriter, req *http.Request) {
		e.api.HandleUpgradePreview(w, req)
	}).Methods("GET")
	api.HandleFunc("/templates/{slug}/upgrade-jobs", func(w http.ResponseWriter, req *http.Request) {
		e.api.HandleStartUpgradeJob(w, req)
	}).Methods("POST")
	api.HandleFunc("/templates/{id}/migrate-slug", func(w http.ResponseWriter, req *http.Request) {
		e.api.HandleMigrateSlug(w, req)
	}).Methods("POST")
	api.HandleFunc("/content/{id}/publications/{publication_id}/rollback", func(w http.ResponseWriter, req *http.Request) {
		e.api.HandleRollback(w, req)
	}).Methods("POST")
	api.HandleFunc("/content/{id}/publications/{publication_id}", func(w http.ResponseWriter, req *http.Request) {
		e.api.HandleGetPublication(w, req)
	}).Methods("GET")
	api.HandleFunc("/content/{id}/publications", func(w http.ResponseWriter, req *http.Request) {
		e.api.HandleListPublications(w, req)
	}).Methods("GET")
	api.HandleFunc("/content/{id}/restore-and-publish", func(w http.ResponseWriter, req *http.Request) {
		e.api.HandleRestoreAndPublish(w, req)
	}).Methods("POST")
	api.HandleFunc("/content/{id}/revert-live", func(w http.ResponseWriter, req *http.Request) {
		e.api.HandleRevertLive(w, req)
	}).Methods("POST")
	// Legacy entry points (Task 16C: same URLs, V3 semantics).
	api.HandleFunc("/content/{id}/publish", func(w http.ResponseWriter, req *http.Request) {
		e.legacy.APIPublishContent(w, req)
	}).Methods("POST")
	api.HandleFunc("/content/batch-publish", func(w http.ResponseWriter, req *http.Request) {
		e.legacy.APIBatchPublishContent(w, req)
	}).Methods("POST")
	api.HandleFunc("/content/{id}/unpublish", func(w http.ResponseWriter, req *http.Request) {
		e.legacy.APIUnpublishContent(w, req)
	}).Methods("POST")
	api.HandleFunc("/content/by-path", func(w http.ResponseWriter, req *http.Request) {
		e.legacy.APIUpdateContentByPath(w, req)
	}).Methods("PUT")
	api.HandleFunc("/regenerate", func(w http.ResponseWriter, req *http.Request) {
		e.legacy.APIRegenerateAllContent(w, req)
	}).Methods("POST")
	api.HandleFunc("/assets/from-url", func(w http.ResponseWriter, req *http.Request) {
		e.legacy.APIUploadAssetFromURL(w, req)
	}).Methods("POST")
	// Anonymous public serving (canonical projections), last.
	r.HandleFunc("/", e.servePublic).Methods("GET")
	r.HandleFunc("/{rest:.*}", e.servePublic).Methods("GET")
	return r
}

// servePublic serves canonical projections over HTTP (the public URL): the
// filesystem canonical file for the requested path, 404 when absent.
func (e *testEnv) servePublic(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if p == "" {
		p = "/"
	}
	rel := strings.TrimPrefix(filepath.Clean("/"+strings.TrimPrefix(p, "/")), "/")
	var name string
	if rel == "" || rel == "." {
		name = "index.html"
	} else {
		name = rel + ".html"
	}
	abs := filepath.Join(e.root, "generated", filepath.FromSlash(name))
	if rel2, err := filepath.Rel(filepath.Join(e.root, "generated"), abs); err != nil ||
		rel2 == ".." || strings.HasPrefix(rel2, ".."+string(filepath.Separator)) {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, abs)
}

// --- HTTP helpers ---------------------------------------------------------

func (e *testEnv) do(method, path string, body []byte, headers map[string]string) (int, []byte, http.Header) {
	e.t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, e.base+path, rdr)
	if err != nil {
		e.t.Fatalf("request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("do %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, raw, resp.Header
}

func decodeObj(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	out := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return map[string]any{"_raw": string(raw)}
		}
		if out == nil {
			return map[string]any{"_raw": string(raw)}
		}
	}
	return out
}

func (e *testEnv) postJSON(path string, body any, headers map[string]string) (int, map[string]any) {
	e.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		e.t.Fatalf("marshal: %v", err)
	}
	if headers == nil {
		headers = map[string]string{}
	}
	headers["Content-Type"] = "application/json"
	code, respRaw, _ := e.do("POST", path, raw, headers)
	return code, decodeObj(e.t, respRaw)
}

func (e *testEnv) postRaw(path string, raw []byte, headers map[string]string) (int, map[string]any) {
	e.t.Helper()
	if headers == nil {
		headers = map[string]string{}
	}
	headers["Content-Type"] = "application/json"
	code, respRaw, _ := e.do("POST", path, raw, headers)
	return code, decodeObj(e.t, respRaw)
}

func (e *testEnv) getJSON(path string, headers map[string]string) (int, map[string]any) {
	e.t.Helper()
	code, raw, _ := e.do("GET", path, nil, headers)
	return code, decodeObj(e.t, raw)
}

func (e *testEnv) getAnon(path string) (int, []byte) {
	e.t.Helper()
	code, raw, _ := e.do("GET", path, nil, nil)
	return code, raw
}

func (e *testEnv) count(col string, filter bson.M) int64 {
	e.t.Helper()
	n, err := e.db.Collection(col).CountDocuments(context.Background(), filter)
	if err != nil {
		e.t.Fatalf("count %s: %v", col, err)
	}
	return n
}

// seedTemplate creates an active templatecontract template (v1) with the
// shared layout and a required headline field.
func (e *testEnv) seedTemplate(t *testing.T, slug string) (models.Template, templatecontract.TemplateVersion) {
	t.Helper()
	return e.seedTemplateFields(t, slug, "active", []models.TemplateField{
		{Name: "headline", Label: "Headline", Type: "text", Required: true},
	})
}

func (e *testEnv) seedTemplateFields(t *testing.T, slug, status string, fields []models.TemplateField) (models.Template, templatecontract.TemplateVersion) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tpl, tv, err := e.tpls.Create(ctx, templatecontract.TemplateInput{
		Slug: slug, Name: "E2E " + slug, Category: "news", Status: status,
		HTMLLayout: sharedLayout, Fields: fields,
	})
	if err != nil {
		t.Fatalf("seed template %s: %v", slug, err)
	}
	return tpl, tv
}

// setActor swaps the product actor for subsequent requests (scope rows).
func (e *testEnv) setActor(a generation.Actor) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.actor = a
	e.anon = false
}

func scopedActor(scopes ...string) generation.Actor {
	a := defaultActor()
	a.IsAdmin = false
	a.Scopes = scopes
	return a
}

// publish posts one mode=publish generation over HTTP and returns the
// decoded response.
func (e *testEnv) publish(t *testing.T, key string, body map[string]any) (int, map[string]any) {
	t.Helper()
	return e.postJSON("/api/v1/page-generation", body, map[string]string{"Idempotency-Key": key})
}

func strOf(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	s, _ := m[k].(string)
	return s
}

func intOf(m map[string]any, k string) int64 {
	if m == nil {
		return 0
	}
	switch v := m[k].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
}

// activePub loads the active publication for contentID (nil when none).
func (e *testEnv) activePub(t *testing.T, cid primitive.ObjectID) *publication.Publication {
	t.Helper()
	p, err := e.repo.GetActive(context.Background(), cid)
	if err != nil {
		t.Fatalf("GetActive: %v", err)
	}
	return p
}

// outboxCount counts outbox rows for an event/publication pair.
func (e *testEnv) outboxCount(t *testing.T, event string, pubID primitive.ObjectID) int64 {
	t.Helper()
	return e.count(publication.CollectionOutbox, bson.M{"event_type": event, "aggregate_id": pubID})
}
