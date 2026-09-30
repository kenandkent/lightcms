// Task 17C: startup and legacy-contract verdicts (config validation path +
// /api/v1/regenerate no-op). New symbols are Con*-prefixed: existing e2e
// files belong to Task 17A and must not be edited.
//
// Red-green note: these are pure-verdict locks (behavior already shipped by
// Task 16). Green-on-first-run is expected and stated per verdict; each test
// would fail if the locked behavior regressed (rejection lifted, diagnostic
// reworded, regenerate gaining side effects).
package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonradoff/lightcms/v7/config"
	"github.com/jonradoff/lightcms/v7/internal/database"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

// ConTestMongoURI returns the replica-set URI the test binary must use:
// MONGODB_URI env first, then the nearest ancestor .env.test (same lookup
// as testutil). Fatal (never skip) when absent — skipped-DB evidence is
// not integration evidence.
func ConTestMongoURI(t *testing.T) string {
	t.Helper()
	if uri := strings.TrimSpace(os.Getenv("MONGODB_URI")); uri != "" {
		return uri
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 6; i++ {
		raw, rerr := os.ReadFile(filepath.Join(dir, ".env.test"))
		if rerr == nil {
			for _, line := range strings.Split(string(raw), "\n") {
				line = strings.TrimSpace(line)
				if v, ok := strings.CutPrefix(line, "MONGODB_URI="); ok && strings.TrimSpace(v) != "" {
					return strings.TrimSpace(v)
				}
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("E2E requires a replica-set test MongoDB: set MONGODB_URI or create .env.test")
	return ""
}

// ConModuleRoot walks up to the directory holding go.mod.
func ConModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, serr := os.Stat(filepath.Join(dir, "go.mod")); serr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above working directory")
		}
		dir = parent
	}
}

// TestConStartupStandaloneRefused locks verdict 1: production-mode startup
// with STANDALONE (non-replica-set) Mongo is REFUSED with an actionable
// diagnostic.
//
// Path used (documented per brief): the config/validation unit path.
// requireReplicaSet/buildPublicationRuntime live in package main
// (cmd/server, unexported) so an e2e package cannot invoke the composed
// guard directly. This test locks every piece the guard is composed of:
//   - topology detection: IsReplicaSet == true on the Task 0 replica-set
//     fixture (the serving predicate);
//   - failure input: IsReplicaSet errors on an unreachable/dead connection
//     (the guard wraps exactly this as "cannot verify MongoDB topology
//     (replica set required ...)");
//   - environment predicate: IsProd is true for production, false for dev
//     (dev keeps serving with a warning);
//   - diagnostic wording: the production refusal text (with the --replSet
//     remediation) is pinned present in publication_runtime.go.
//
// The live-standalone-mongod probe (real mongod without --replSet on a
// scratch port) is owned by TestE2E_StartupRejections (Task 17A). No
// containers are created, removed, or stopped here (task container rule:
// reuse the healthy lightcms-mongo-test fixture only).
func TestConStartupStandaloneRefused(t *testing.T) {
	requireLiveMongo(t)
	e := newEnv(t, envOpts{})

	// Detection side: the fixture every E2E row serves from IS a replica
	// set — the guard's allow predicate holds here.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ok, err := e.db.IsReplicaSet(ctx)
	if err != nil {
		t.Fatalf("IsReplicaSet on test fixture: %v", err)
	}
	if !ok {
		t.Fatal("test fixture is not a replica set — Task 0 fixture broken, verdict void")
	}

	// Failure input: a dead connection makes IsReplicaSet fail, which is
	// what the production guard refuses to serve on ("cannot verify ...").
	// Separate connection + dedicated name; the shared test DB is untouched.
	uri := ConTestMongoURI(t)
	pctx, pcancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer pcancel()
	wc := writeconcern.New(writeconcern.WMajority())
	dead, derr := database.Connect(pctx, uri, "lightcms-test-con-deadprobe", options.Client().SetWriteConcern(wc))
	if derr != nil {
		t.Fatalf("connect dead-probe db: %v", derr)
	}
	_ = dead.Disconnect(context.Background())
	if _, derr := dead.IsReplicaSet(context.Background()); derr == nil {
		t.Fatal("IsReplicaSet on a disconnected client succeeded, want error (guard's refuse input)")
	}

	// Environment predicate: production refuses, development tolerates.
	prod := &config.Config{Env: "production"}
	if !prod.IsProd() {
		t.Fatal("IsProd() false for Env=production")
	}
	dev := &config.Config{Env: "development"}
	if dev.IsProd() {
		t.Fatal("IsProd() true for Env=development (dev must keep serving throwaway standalones)")
	}

	// Diagnostic wording pinned in the guard source (package-main access
	// barrier documented above): actionable remediation, not a bare error.
	src, serr := os.ReadFile(filepath.Join(ConModuleRoot(t), "cmd", "server", "publication_runtime.go"))
	if serr != nil {
		t.Fatalf("read publication_runtime.go: %v", serr)
	}
	body := string(src)
	for _, want := range []string{
		"cannot verify MongoDB topology (replica set required for publication transactions)",
		"production requires MongoDB replica set mode (standalone detected)",
		"--replSet",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("startup diagnostic drift: publication_runtime.go lost %q", want)
		}
	}
	t.Logf("VERDICT 1 LOCKED (validation-unit path): RS fixture IsReplicaSet=true; dead topology errors; prod refuses / dev tolerates; --replSet diagnostic pinned. Live-standalone probe owned by TestE2E_StartupRejections (17A).")
}

// TestConStartupStorageProviderRefused locks verdict 2:
// STATIC_STORAGE_PROVIDER=s3 (or any non-filesystem provider pre-ADR) is
// REFUSED at startup with an actionable diagnostic. This is the first guard
// in buildPublicationRuntime (cfg.ValidatePublicationConfig), exercised here
// directly since config is an importable package.
func TestConStartupStorageProviderRefused(t *testing.T) {
	// s3 refused with an actionable diagnostic: names the variable, the
	// only approved provider, and the unblocker (Storage ADR + suite).
	cfg := &config.Config{StaticStorageProvider: "s3", IdempotencyTTLHours: 24, IdempotencyLeaseMinutes: 5}
	cfg.ApplyPublicationDefaults()
	err := cfg.ValidatePublicationConfig()
	if err == nil {
		t.Fatal("STATIC_STORAGE_PROVIDER=s3 accepted, want refusal")
	}
	msg := err.Error()
	for _, want := range []string{"STATIC_STORAGE_PROVIDER", `"s3"`, "filesystem", "ADR"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("s3 diagnostic not actionable (missing %q): %v", want, err)
		}
	}

	// r2 refused identically (the other pre-ADR candidate).
	cfg.StaticStorageProvider = "r2"
	if err := cfg.ValidatePublicationConfig(); err == nil {
		t.Fatal("STATIC_STORAGE_PROVIDER=r2 accepted, want refusal")
	} else if !strings.Contains(err.Error(), "ADR") {
		t.Fatalf("r2 diagnostic not actionable: %v", err)
	}

	// Unset provider defaults to filesystem and is accepted (startup path
	// for operators who set nothing).
	cfg.StaticStorageProvider = ""
	cfg.ApplyPublicationDefaults()
	if cfg.StaticStorageProvider != "filesystem" {
		t.Fatalf("default provider = %q, want filesystem", cfg.StaticStorageProvider)
	}
	if err := cfg.ValidatePublicationConfig(); err != nil {
		t.Fatalf("default filesystem provider rejected: %v", err)
	}

	// Wiring order: the storage guard runs BEFORE the replica-set probe in
	// buildPublicationRuntime, so a bad provider fails fast without
	// touching the database.
	src, serr := os.ReadFile(filepath.Join(ConModuleRoot(t), "cmd", "server", "publication_runtime.go"))
	if serr != nil {
		t.Fatalf("read publication_runtime.go: %v", serr)
	}
	body := string(src)
	iValidate := strings.Index(body, "cfg.ValidatePublicationConfig()")
	iReplica := strings.Index(body, "requireReplicaSet(")
	if iValidate < 0 || iReplica < 0 || iValidate > iReplica {
		t.Fatal("startup guard order drift: ValidatePublicationConfig must precede requireReplicaSet")
	}
	t.Logf("VERDICT 2 LOCKED: s3/r2 refused with ADR diagnostic; empty defaults to filesystem; storage guard precedes topology probe.")
}

// TestConRegenerateNoopVerdict locks verdict 4: /api/v1/regenerate keeps its
// EXACT current behavior — 200 + "regenerated" message with zero side
// effects (silent no-op). Production behavior changes are FORBIDDEN in this
// task; the 410-vs-upgrade-redirect decision belongs to Task 19. Any silent
// behavior change (new status, altered bytes, minted rows) fails this test.
func TestConRegenerateNoopVerdict(t *testing.T) {
	e, cid, pid1, u := faultSetup(t, envOpts{})

	code, before := e.getAnon(u)
	if code != 200 {
		t.Fatalf("live page missing before regenerate: %d", code)
	}
	pubsBefore := e.count("content_publications", bson.M{})
	outBefore := e.count(publication.CollectionOutbox, bson.M{})

	start := time.Now()
	rcode, resp := e.postJSON("/api/v1/regenerate", map[string]any{}, nil)
	if rcode != 200 {
		t.Fatalf("regenerate status = %d (%v), want 200 (exact current behavior)", rcode, resp)
	}
	if ok, _ := resp["success"].(bool); !ok {
		t.Fatalf("regenerate success != true: %v", resp)
	}
	msg, _ := resp["message"].(string)
	if !strings.Contains(msg, "regenerated") {
		t.Fatalf("regenerate message changed (verdict premise): %v", resp)
	}
	_ = start

	// Byte-exact no-op proof: canonical bytes, active record, and row
	// counts are all unchanged.
	if c, after := e.getAnon(u); c != 200 || string(after) != string(before) {
		t.Fatalf("regenerate altered canonical bytes (status=%d)", c)
	}
	if active := e.activePub(t, cid); active == nil || active.ID.Hex() != pid1 {
		t.Fatalf("regenerate changed the active publication (was %s)", pid1)
	}
	if n := e.count("content_publications", bson.M{}); n != pubsBefore {
		t.Fatalf("regenerate minted publications (%d -> %d)", pubsBefore, n)
	}
	if n := e.count(publication.CollectionOutbox, bson.M{}); n != outBefore {
		t.Fatalf("regenerate touched outbox (%d -> %d)", outBefore, n)
	}
	t.Logf("VERDICT 4 LOCKED: /api/v1/regenerate is a silent no-op (200 + %q, bytes + active + rows unchanged). 410-vs-upgrade-redirect decision REQUIRED in Task 19 — do not change behavior silently.", msg)
}
