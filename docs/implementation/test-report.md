# Task 17D — Whole-System Test Report (FINAL Task 17 review)

- **Task:** 17D (test report + final review; LAST Task 17 agent)
- **Baseline:** `90a42d2` — Merge task/17b (e2e suite + 17A hardening + 17B gap tests + 17C verdicts, all merged)
- **Branch:** `task/17d-report` (worktree `/Users/ken/workspace/worktrees/task-17d`)
- **Date (UTC):** 2026-09-30
- **Scope rule:** read-only re-verification + this report. No source, test, or config file was
  modified. One test-run side effect was OBSERVED and left untouched (see §8).

Required contents per plan Task 17 (`record URL, Mongo version, test count and failures in
docs/implementation/test-report.md`) and spec §39 (upstream regression §39.1, product unit +
≥85% coverage §39.2, draft/live §39.3, template-version §39.4, failure injection §39.5,
idempotency §39.6, agent/sandbox §39.7, security §39.8, legacy entry points §39.9, mutation
policy §39.10, immutability/rollback §39.11).

---

## 1. Environment (independently re-verified)

| Item | Value (this run) |
|---|---|
| MongoDB image / container | `mongo:7.0.14` / `lightcms-mongo-test` (reused running stack, healthy, up ~2h) |
| `mongosh --eval db.version()` | `7.0.14` |
| `rs.status().myState` / members | `1` (PRIMARY) / 1 member — single-node replica set `rs0`, transaction-capable |
| Test URI (stock `.env.test.example` values via env, NO file created) | `mongodb://127.0.0.1:27017/lightcms-test?replicaSet=rs0&directConnection=true` |
| `DATABASE_NAME` (full suite) | `lightcms-test` |
| `DATABASE_NAME` (e2e package, forced by 17A `TestMain`) | `lightcms-test-e2e` (dedicated, full-drop per test) |
| `DATABASE_NAME` (coverage runs, §4) | unique per package: `lightcms-test-cov-<pkg>` |
| Go toolchain | `go1.25.0 darwin/arm64` |
| `.env.test` files on disk | NONE — verified absent in worktree and every ancestor dir up to `/Users`; tests ran purely on exported `MONGODB_URI`/`DATABASE_NAME` |

**Methodological note (stack freshness):** the brief asked for a FRESH volume via
`docker compose -f docker-compose.test.yml up -d`. The container rule for this task
(`reuse running lightcms-mongo-test if healthy … NEVER down/rm containers/volumes`) takes
precedence: the healthy running container was reused, so pre-existing data in `lightcms-test`
from prior runs was present. Mitigations in force: every DB suite uses
`testutil.CleanupCollections`; the e2e package writes ONLY to its dedicated `lightcms-test-e2e`
(full `wipeE2EDB` drop); all §4 coverage runs used unique per-package database names.
No cross-package contamination was observed (0 failures). The stack was left RUNNING for
orchestrator cleanup — no `down`/`rm` was executed.

---

## 2. Full suite: `go test -p 1 ./... -count=1` — EXIT 0

Two runs, same result: plain (package lines) and `-v` (test-level counts).
Logs retained outside the repo: `/tmp/17d-fullsuite.log`, `/tmp/17d-fullsuite-verbose.log`.

| Package | Result | Time |
|---|---|---|
| `config` | ok | 0.511s |
| `internal/apiclient` | ok | 0.406s |
| `internal/auth` | ok | 4.099s |
| `internal/build` | ok | 0.411s |
| `internal/cli` | ok | 0.383s |
| `internal/database` | ok | 1.451s |
| `internal/dbutil` | ok | 0.309s |
| `internal/errors` | ok | 0.327s |
| `internal/handlers` | ok | ~121s |
| `internal/jsonlog` | ok | 0.517s |
| `internal/mcp` | ok | 1.298s |
| `internal/middleware` | ok | 0.962s |
| `internal/models` | ok | 0.304s |
| `internal/oauth` | ok | 9.610s |
| `internal/observe` | ok | 0.295s |
| `internal/product/e2e` | ok | ~20s |
| `internal/product/generation` | ok | 6.438s |
| `internal/product/httpapi` | ok | 3.477s |
| `internal/product/idempotency` | ok | 2.582s |
| `internal/product/migration` | ok | 15.243s |
| `internal/product/pathkey` | ok | 0.294s |
| `internal/product/publication` | ok | 12.582s |
| `internal/product/publicurl` | ok | 0.293s |
| `internal/product/storage` | ok | 1.167s |
| `internal/product/templatecontract` | ok | 3.859s |
| `internal/services` | ok | 15.172s |
| `internal/services/importer` | ok | 0.729s |
| 7× `cmd/*` + `internal/testutil` | no test files (8 entries) | — |

**Totals (`-v` run): 27 packages ok, 0 FAIL package, `--- PASS` 1709 / `--- FAIL` 0 /
`--- SKIP` 1, `skipping: MONGODB_URI not set` 0 lines** (zero skipped DB tests — full
integration evidence per the Task 0 gate rule).

The single SKIP is the known pre-existing conditional skip, NOT a product gap:

- `--- SKIP: TestHomepage_WebsiteJSONLD (0.60s)` (`internal/handlers/llms_test.go:214`) —
  `t.Skipf("homepage render returned %d (no homepage content in test DB)")`. Pre-existing
  behavior, recorded as-is for Task 19 (see §7).

---

## 3. e2e package: 58 + 6 verdicts, 0 SKIP — CONFIRMED

From the `-v` log (e2e section, package `ok … 20.028s`):

- `--- PASS: TestE2E_*` — **58/58** (lifecycle, fork drafts, canonical case collision,
  fault table incl. cutover-crash legacy, idempotency table, migration table incl. resume /
  scanner-keeps-legacy-online, security table incl. sandbox/scopes/SSRF/XSS, rename,
  rollback-retry, retention/GC, outbox, scheduler stable key, template upgrade, legacy parity).
- `--- PASS: TestCon*` verdict locks — **6/6**:
  `TestConStartupStandaloneRefused` (V1), `TestConStartupStorageProviderRefused` (V2),
  `TestConBuildSHAProvenance` (V3), `TestConRegenerateNoopVerdict` (V4),
  `TestConSchedulerCrashTakeover` (V5), `TestConOutboxPublicURLDelivered` (V6).
- `--- FAIL` in e2e scope: **0**. `--- SKIP` in e2e scope: **0** (the one repo-wide SKIP
  sits in the `internal/handlers` section, verified by log line position).
- Fault-injection extension tests also pass in the full run:
  `--- PASS: TestFaultInjection_APIHandlers`, `--- PASS: TestFaultInjection_LegacyRoutes`.

Spec §39 mapping (evidence, not re-argument): §39.3 draft/live → `TestE2E_ForkDraftReuse`,
`TestE2E_ForkVisibility`, `TestE2E_ForkPageScopeBug`; §39.4 template versions →
`TestE2E_TemplateUpgradeJob`, `TestE2E_UpgradeResume`, `TestE2E_TemplateStatusGate`;
§39.5 faults → `TestE2E_Fault{RenderStage,ActivateCommit,CutoverCrash,CutoverCrashLegacy,OutboxCDN}`,
`TestE2E_FirstPublishCrashGap`, `TestE2E_MissingImmutableP0`; §39.6 idempotency →
`TestE2E_Idempotency{Replay,Parallel,Conflict,TerminalRetry,Heartbeat,LeaseMatrix,KeyGate,CreateTerminalGap}`,
`TestE2E_ValidationReplay`, `TestE2E_RollbackRetry`; §39.7 sandbox →
`TestE2E_SecuritySandbox`; §39.8 security → `TestE2E_Security{Scopes,StoredXSS,AssetSSRF,InputHardening}`;
§39.9 legacy entries → `TestE2E_LegacyParity`, `TestE2E_BatchPublishLegacy`; §39.10 mutation
policy → fork-visibility/draft tests; §39.11 immutability/rollback → `TestE2E_RenamePublish`,
`TestE2E_RestoreRevertLive`, `TestE2E_RestoreVersion404`, `TestE2E_RetentionGC`,
`TestE2E_StaleExpectedActive`, `TestE2E_QuarantineOrphan`.

---

## 4. Coverage: 86.9% aggregate CONFIRMED; two packages <85% recorded as follow-up

Method (per brief): ISOLATED per-package invocations, each with a UNIQUE `DATABASE_NAME`
(`lightcms-test-cov-<pkg>`), `-count=1 -coverprofile`. All 10 runs exit 0.
My numbers are byte-identical to 17B's BEFORE→AFTER claim, so the table below serves as both
re-measurement and confirmation (no BEFORE re-run needed — nothing changed since `90a42d2`).

| Package | Statements (have/total) | Final | 17B before → after | ≥85%? |
|---|---|---|---|---|
| `pathkey` | 38/39 | 97.4% | 79.5 → 97.4 | yes |
| `templatecontract` | 398/416 | 95.7% | 84.6 → 95.7 | yes |
| `publication` | 1501/1817 | **82.6%** | 68.5 → 82.6 | **NO — gap 44 stmts** |
| `storage` | 359/420 | 85.5% | 65.7 → 85.5 | yes (margin 2 stmts) |
| `publicurl` | 91/94 | 96.8% | 70.2 → 96.8 | yes |
| `idempotency` | 239/261 | 91.6% | 68.6 → 91.6 | yes |
| `generation` | 855/987 | 86.6% | 55.1 → 86.6 | yes |
| `httpapi` | 374/406 | 92.1% | 60.6 → 92.1 | yes |
| `migration` | 366/432 | **84.7%** | 74.1 → 84.7 | **NO — gap 2 stmts** |
| `observe` (`internal/observe`) | 89/90 | 98.9% | 66.7 → 98.9 | yes |
| **AGGREGATE** | **4310/4962** | **86.9%** | 66.9 → 86.9 | yes (need 4218, have 4310; margin 92 stmts) |

Aggregate computed by summing covered/total statements across the 10 profiles
(`awk` over `coverprofile` block counts), matching 17B's `4218 needed, 4310 have` exactly.

**Sub-85% gaps (recorded precisely; NO gap tests written — 17B/follow-up owns them):**

1. `publication` 82.6% — needs 1545/1817, has 1501 → **44 statements short**. Lowest
   functions (`go tool cover -func`): `recovery.go: reinspect` 42.9%, `sweepOrphans` 54.3%,
   `reapStaleStaged` 55.6%, `reconcileActive` 58.3%, `cleanStaleNext`/`renameFile` 57.1%,
   `reconcileNoActive` 68.2%, `Run` 71.4%, `listActive`/`generatedRoot`/`syncDir` 75.0%,
   `sweepExpiredObjects` 75.6%; `gc.go: sweepQuarantineDir` 75.0%, `poisonOutbox` 81.8%;
   `repository.go: getByIDIn` 0.0% (only fully-uncovered function in the package).
   Gap concentrates in scanner/GC edge branches and recovery helpers.
2. `migration` 84.7% — needs 368/432, has 366 → **2 statements short**. Diffuse partial
   branches across `migrate.go` (`Run` 70.5%, `backfillTemplates` 65.0%, `activatePage` 69.2%,
   `hasIndexName` 72.7%, `assertAllServingHaveActive`/`readCanonicalBytes` 75.0%, …).
   Smallest possible follow-up; any single small test closes it.

---

## 5. `fault_injection_test.go` Task 17 extension — verdict: TEST-ONLY, PASS, no production impact

- Extension source: salvaged commit `21c9ca3` (`internal/handlers/fault_injection_test.go`,
  +83 lines: `TestFaultInjection_LegacyRoutes` + `doJSONQuery` + `mustOID` helpers + imports).
- Contents (read-only review): write-error pins for legacy routes — `APIUpdateContentByPath`,
  `APIUnpublishContent`, `APIRestoreContent` (UpdateOne fault), `APIRevertContentVersion`
  (InsertOne fault) — plus the `/api/v1/regenerate` no-op pin (200 + "regenerated" message,
  zero side effects), which locks the Task 16B verdict premise for the Task 19
  410-vs-upgrade-redirect decision.
- Production-behavior check: the diff touches NO non-test file; the new test calls only
  existing handlers through `httptest` with `SetFaultHook` save/restore (`SetFaultHook(nil)`
  at end). Covers legacy-route cases only. **Verdict: PASS** in the full run
  (`--- PASS: TestFaultInjection_LegacyRoutes (0.02s)`); safe, no follow-up.

---

## 6. `go build ./...` + `go vet ./...` — GREEN (sanity check, nothing changed)

`go build ./... && go vet ./...` → exit 0 (`BUILD_VET_OK`). Expected: this task adds only
`docs/implementation/test-report.md`.

---

## 7. Remaining risks — RECORDED, not resolved (Task 19 decisions / follow-up)

1. **17B behavioral note — `mapSagaErr` INTERNAL_ERROR mapping** (`internal/product/generation/service_publish.go:258-293`,
   pinned by `cover_gap_internal_test.go:46-53`): non-publication codes —
   `templatecontract.CodeNotFound` (template-not-found), `idempotency.CodeConflict`,
   `storage.Error`, generation-local codes, plain errors — all fall through the
   publication-code switch to `genErr(CodeInternal, …)` (HTTP 500). Related quirk pinned in
   the same test (lines 71-87): `templatecontract.CodeOf` never returns `""`, so the
   idempotency/storage/generation branches of `mapSagaCode` (lines 310-318) are unreachable.
   → Task 19: decide whether template-not-found (and kind) deserve precise 4xx codes.
2. **17B behavioral note — dead store-condition** (`service_publish.go:267-270,322`;
   `storage/store.go:90-99`): `storage.CodeOf` maps EVERY non-nil error (unknown → `CodeIO`),
   so `isStoreErr(err)` is always true and `storage.CodeOf(err) != "" || isStoreErr(err)` is a
   tautology — both disjuncts identical, the non-503 fallthrough on line 270 unreachable.
   Pinned by `cover_gap_internal_test.go:91-95` ("assert the call shape"). → Task 19: simplify
   or give the condition a real discriminant; cosmetic + clarity, no behavior change today.
3. **17A pre-existing handlers SKIP** — `TestHomepage_WebsiteJSONLD` conditional skip
   (reproduced in this run, §2). → Task 19: seed homepage fixture or accept the skip.
4. **17A sitemap.xml test-hygiene side effect — REPRODUCED FIRST-HAND in this run:** after the
   full suite, `git status` shows `M internal/handlers/static/sitemap.xml` (base URL
   `http://localhost:8082/` → `https://example.com/`, 1-line diff). A test run mutates a
   TRACKED file in the repo tree. Left UNCOMMITTED and UNREVERTED per this task's file scope
   (commit contains `test-report.md` only); orchestrator/follow-up should decide: restore the
   file, gitignore the generated copy, or redirect the test to a temp dir. Not a product bug;
   hygiene only.
5. **17C verdicts → Task 19:** V4 regenerate-noop (`TestConRegenerateNoopVerdict`,
   `TestE2E_RegenerateVerdict`, fault-injection pin §5) locks the 200-no-op while the
   410-vs-upgrade-redirect decision is pending; V5 scheduler-takeover
   (`TestConSchedulerCrashTakeover`) locks TODAY's `IDEMPOTENCY_NOT_FOUND`-with-zero-effects
   failure caused by the shadowed `TakeOverByKey` result in
   `internal/services/publish_internal.go` — specified fix is assigning the outer op
   (`op, terr = …`); Task 19 must choose fix-and-retry vs scanner-owned lease repair. The
   locks fail on ANY behavior change in either direction, so the decision cannot land silently.

---

## 8. Conflicts / out-of-scope observations

- **Conflicts with other worktrees/branches: NONE.** Work confined to `task/17d-report`;
  no other worktree touched; no merge/push performed.
- **Files changed in this commit: `docs/implementation/test-report.md` ONLY** (new file).
  `internal/handlers/static/sitemap.xml` shows as modified in the working tree SOLELY from
  running the suite (§7.4); it is deliberately NOT staged, NOT reverted, NOT committed.
- **No test bug found requiring a fix** in 17D scope: full suite green, coverage numbers
  reproduce 17B exactly, e2e counts match the 58+6 expectation, fault-injection pins hold.
- **No `.env.test` created or committed** (was never present; env-var flow used instead).
- **No container/volume was stopped, removed, or wiped**; stack left running for
  orchestrator cleanup.
