// Task 16E: one LightCMS publication runtime (spec §16.6, §36, §42).
//
// All live mutations use this single in-process construction: filesystem
// immutable store, publication saga, idempotency service, public URL
// resolver, generation orchestrator, product HTTP handlers, durable outbox
// worker and recovery scanner. No caller constructs a parallel
// store-rooted service; main.go calls buildPublicationRuntime exactly once.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jonradoff/lightcms/v7/config"
	"github.com/jonradoff/lightcms/v7/internal/auth"
	"github.com/jonradoff/lightcms/v7/internal/build"
	"github.com/jonradoff/lightcms/v7/internal/database"
	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/generation"
	"github.com/jonradoff/lightcms/v7/internal/product/httpapi"
	"github.com/jonradoff/lightcms/v7/internal/product/idempotency"
	"github.com/jonradoff/lightcms/v7/internal/product/migration"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/publicurl"
	"github.com/jonradoff/lightcms/v7/internal/product/storage"
	"github.com/jonradoff/lightcms/v7/internal/services"
	"net/http"
)

// publicationRuntime is the single wired V3 service set. Built once by
// buildPublicationRuntime; workers run in the existing server process and
// stop when the server context is cancelled.
type publicationRuntime struct {
	Store      storage.Store
	PubRepo    *publication.Repository
	Idem       *idempotency.Service
	URLs       *publicurl.Resolver
	Pubs       *publication.Service
	Gen        *generation.Service
	ProductAPI *httpapi.Handlers
	Scanner    *publication.Scanner
	Outbox     *publication.OutboxWorker
}

// runtimeDeps are the pre-existing services the runtime integrates with
// (CDN purge, audit trail, durable webhook delivery).
type runtimeDeps struct {
	Cloudflare *services.CloudflareService
	Audit      *services.AuditService
	Webhooks   *services.WebhookService
}

// buildPublicationRuntime constructs every V3 publication service exactly
// once and applies the Task 16E startup guards: unsupported storage
// providers and production standalone Mongo are rejected before serving.
// Callers must have applied cfg.ApplyPublicationDefaults.
func buildPublicationRuntime(ctx context.Context, db *database.DB, cfg *config.Config, deps runtimeDeps) (*publicationRuntime, error) {
	if err := cfg.ValidatePublicationConfig(); err != nil {
		return nil, err
	}
	if err := requireReplicaSet(ctx, db, cfg); err != nil {
		return nil, err
	}

	store := storage.NewFilesystemStore("content")
	pubRepo := publication.NewRepository(db, nil)
	idem, err := idempotency.NewService(db, idempotency.Options{
		TTLHours: cfg.IdempotencyTTLHours, LeaseMinutes: cfg.IdempotencyLeaseMinutes,
	})
	if err != nil {
		return nil, fmt.Errorf("idempotency service: %w", err)
	}
	var urls *publicurl.Resolver
	if base, uerr := url.Parse(cfg.PublicBaseURL); uerr == nil && base != nil {
		if resolver, rerr := publicurl.NewResolver(base); rerr == nil {
			urls = resolver
		} else {
			log.Printf("Warning: public URL resolver disabled: %v", rerr)
		}
	}
	pubAudit := func(ctx context.Context, action string, fields map[string]any) {
		deps.Audit.LogAsync(models.AuditLog{Action: action, Resource: "publication", Details: fields})
	}
	pubs := publication.NewService(db, pubRepo, store, publication.Options{
		Idem: idem,
		URLs: urls,
		Purge: func(ctx context.Context, urls []string) error {
			return deps.Cloudflare.PurgeByURLs(ctx, urls)
		},
		Audit:    pubAudit,
		BuildSHA: publicationBuildSHA(),
	})
	gen := generation.NewService(db, generation.Options{
		Pubs: pubs, PubRepo: pubRepo,
		Idem: idem, URLs: urls, Audit: pubAudit,
		Limiter: newPublicationRateLimiter(cfg.PageGenerationRateLimit),
	})

	productAPI := &httpapi.Handlers{
		Gen: gen,
		ActorExtractor: func(r *http.Request) (generation.Actor, error) {
			u, ok := auth.UserFromAPIContext(r.Context())
			if !ok || u == nil {
				return generation.Actor{}, fmt.Errorf("authentication is required")
			}
			kind := "human"
			session := r.Header.Get("X-Agent-Session")
			if session != "" {
				kind = "agent"
			}
			return generation.Actor{
				ID: u.ID, Email: u.Email, Authenticated: true,
				IsAdmin: u.Role == models.RoleAdmin, Scopes: u.Scopes,
				SandboxOnly: u.SandboxOnly, AgentSession: session,
				Via: "api", ActorKind: kind,
			}, nil
		},
		IdempotencyExtractor: func(r *http.Request) (string, bool) {
			key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
			return key, key != ""
		},
	}

	scanner := publication.NewScanner(db, pubRepo, store, publication.ScannerOptions{
		StageTimeout:            time.Duration(cfg.PublicationStageTimeoutMinutes) * time.Minute,
		RetentionDays:           cfg.PublicationRetentionDays,
		FailedRetentionDays:     cfg.PublicationFailedRetentionDays,
		QuarantineRetentionDays: cfg.PublicationQuarantineRetentionDays,
		ScanInterval:            time.Duration(cfg.PublicationScanIntervalMinutes) * time.Minute,
		Audit:                   pubAudit,
		Alert: func(a publication.Alert) {
			log.Printf("[P0-ALERT] scanner severity=%s code=%s path=%s msg=%s",
				a.Severity, a.Code, a.Path, a.Message)
		},
	})
	outbox := publication.NewOutboxWorker(db,
		func(ctx context.Context, eventType, eventID string, payload map[string]any) error {
			return deps.Webhooks.DeliverRecordedEvent(ctx, eventType, eventID, payload)
		}, publication.WorkerOptions{})

	return &publicationRuntime{
		Store: store, PubRepo: pubRepo, Idem: idem, URLs: urls,
		Pubs: pubs, Gen: gen, ProductAPI: productAPI,
		Scanner: scanner, Outbox: outbox,
	}, nil
}

// requireReplicaSet rejects production startup on a standalone MongoDB:
// multi-document transactions (activation, unpublish, idempotency binding,
// outbox insert) cannot run without replica set mode (spec §12/§16).
// Development keeps serving (with a warning) so throwaway standalones stay
// usable for non-publication work.
func requireReplicaSet(ctx context.Context, db *database.DB, cfg *config.Config) error {
	ok, err := db.IsReplicaSet(ctx)
	if err != nil {
		return fmt.Errorf("cannot verify MongoDB topology (replica set required for publication transactions): %w", err)
	}
	if ok {
		return nil
	}
	if cfg.IsProd() {
		return fmt.Errorf("production requires MongoDB replica set mode (standalone detected): transactions for publication activation are unavailable — start mongod with --replSet and initiate the set")
	}
	log.Printf("WARNING: MongoDB is standalone — publication transactions will fail; use the Task 0 replica-set fixture for any publish path")
	return nil
}

// publicationRateLimiter is a process-global fixed-window limiter for
// generation publish throughput (§30.1: PAGE_GENERATION_RATE_LIMIT/min).
type publicationRateLimiter struct {
	mu     sync.Mutex
	window time.Time
	count  int
	limit  int
}

func newPublicationRateLimiter(perMinute int) *publicationRateLimiter {
	if perMinute <= 0 {
		perMinute = config.DefaultPageGenerationRateLimit
	}
	return &publicationRateLimiter{limit: perMinute}
}

func (l *publicationRateLimiter) Allow(_ context.Context, _ generation.Actor) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now().UTC().Truncate(time.Minute)
	if now.After(l.window) {
		l.window = now
		l.count = 0
	}
	if l.count >= l.limit {
		return false, 60
	}
	l.count++
	return true, 0
}

// Lane 2B: degraded boot on legacy canonical collisions.
//
// EnsureProductIndexes builds the V3 control-plane guards (canonical
// uniqueness, active-publication pointer, idempotency, outbox). On legacy
// DBs with canonical collisions (case-variant paths sharing one canonical
// key) the build fails with a duplicate-key error. That must NOT
// log.Fatalf: the operator's diagnostic (`migrate-publications --dry-run`,
// served by this same binary) needs a running process and a connectable DB,
// and the site should keep serving reads/drafts in a migration-required
// degraded state until the migration completes.

// isIndexCollisionError reports whether err is an index-shape conflict
// (duplicate key on unique-index build, conflicting index options/keys, or
// an already-exists race) as opposed to a connectivity failure.
func isIndexCollisionError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"duplicate key", "e11000",
		"indexoptionsconflict", "indexkeyspecsconflict",
		"already exists",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// ensureProductIndexesOrDegraded ensures the V3 product index set, reporting
// (not fatalling) index-shape conflicts: degraded=true carries the operator
// reason, which the caller latches as migration-required state and surfaces
// on /healthz. Non-collision failures (connectivity, permissions) are
// returned as fatal errors — boot keeps today's fail-fast behavior for those.
func ensureProductIndexesOrDegraded(ctx context.Context, db *database.DB) (degraded bool, reason string, err error) {
	if cerr := db.EnsureProductIndexes(ctx); cerr != nil {
		if !isIndexCollisionError(cerr) {
			return false, "", cerr
		}
		reason = fmt.Sprintf("product indexes unavailable (canonical collision or conflicting index; run `lightcms migrate-publications --dry-run` for blockers): %v", cerr)
		log.Printf("WARNING: migration required — %s. Server starts degraded: publish paths are unprotected until the migration completes.", reason)
		return true, reason, nil
	}
	return false, "", nil
}

// migrationRequiredReason latches the degraded-boot detail for health
// reporting. Set once during boot when ensureProductIndexesOrDegraded
// reports degraded; read by /health handlers.
var (
	migrationRequiredMu     sync.Mutex
	migrationRequiredReason string
)

// setMigrationRequired latches the migration-required degraded state.
func setMigrationRequired(reason string) {
	migrationRequiredMu.Lock()
	defer migrationRequiredMu.Unlock()
	migrationRequiredReason = reason
}

// resetMigrationRequired clears the latch (tests only).
func resetMigrationRequired() {
	migrationRequiredMu.Lock()
	defer migrationRequiredMu.Unlock()
	migrationRequiredReason = ""
}

// migrationRequiredState reports the latched degraded state.
func migrationRequiredState() (bool, string) {
	migrationRequiredMu.Lock()
	defer migrationRequiredMu.Unlock()
	return migrationRequiredReason != "", migrationRequiredReason
}

// runMigrationCommand implements `lightcms migrate-publications
// --dry-run|--apply` (Task 14 contract) inside the existing server binary —
// no second binary. It exits the process with 0 on success, 2 when the
// migration is blocked (report printed as JSON for operators).
func runMigrationCommand(cfg *config.Config, db *database.DB) {
	dryRun, apply := false, false
	for _, arg := range os.Args[2:] {
		switch arg {
		case "--dry-run":
			dryRun = true
		case "--apply":
			apply = true
		default:
			fmt.Fprintf(os.Stderr, "unknown flag %q: usage: lightcms migrate-publications --dry-run|--apply\n", arg)
			os.Exit(2)
		}
	}
	if dryRun == apply {
		fmt.Fprintln(os.Stderr, "usage: lightcms migrate-publications --dry-run|--apply (exactly one)")
		os.Exit(2)
	}
	store := storage.NewFilesystemStore("content")
	migrator := migration.New(migration.Config{DB: db, Store: store, BaseURL: cfg.PublicBaseURL})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	var (
		rep migration.Report
		err error
	)
	if dryRun {
		rep, err = migrator.DryRun(ctx)
	} else {
		rep, err = migrator.Run(ctx)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(rep)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migration %s: %v\n", map[bool]string{true: "--apply", false: "--dry-run"}[apply], err)
		os.Exit(2)
	}
	if rep.Completed {
		log.Printf("migration: completed (build %s)", build.GetVersion())
		return
	}
	if !dryRun {
		fmt.Fprintln(os.Stderr, "migration: not completed — resolve the reported items and re-run")
		os.Exit(2)
	}
}
