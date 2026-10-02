# Release Report — LightCMS 7.3.0 (Task 19: release rehearsal + signoff)

- **Task:** 19 (release rehearsal + signoff; integration owner, external-reviewer mindset)
- **Baseline:** `6e1cd15` (main with everything merged through Task 18D)
- **Branch:** `task/19-release` (worktree `/Users/ken/workspace/worktrees/task-19`)
- **Date (UTC):** 2026-09-30
- **Scope rule:** rehearsals + minimal gap fixes (≤~30 lines each, red-green tested, spec-mandated) + version/changelog + these two docs. No merge, no push, no tag. `/Users/ken/workspace/newsPage` untouched.
- **Companion:** `docs/implementation/release-checklist.md` (the actionable checklist). Prior evidence: `docs/implementation/test-report.md` (Task 17D: 27 pkgs, 1709 pass, 86.9% coverage).

**Headline finding:** rehearsal 3 caught a genuine release-blocker class bug the in-suite tests missed — the recovery scanner enumerated fork copies as live pages and quarantined the SERVING canonical out from under a live page once migration completed (`TestScannerForkCopyKeepsLiveOnline` failed red, fixed, green). Without the fix, any post-migration scanner pass would take live pages offline wherever an editor/agent fork exists. Fixed in this commit (§6, fix F1).

---

## 1. Full regression + vet + builds (rehearsal 1)

- `go test -p 1 ./... -count=1` → **exit 0, 27 packages ok, 0 FAIL** (log `/tmp/19-final2.log`).
- Verbose recount: **1712 `--- PASS`, 0 `--- FAIL`, 1 `--- SKIP`** (log `/tmp/19-final-verbose.log`). The single SKIP is the known pre-existing conditional `TestHomepage_WebsiteJSONLD` (no homepage content in test DB), same as 17D.
- `go vet ./...` → exit 0, no output.
- `go build -o … ./cmd/server|mcp|cli` → all three exit 0.
- Environment: `mongo:7.0.14` container `lightcms-mongo-test` reused healthy (container rule: reuse, never down/rm); `MONGODB_URI=mongodb://127.0.0.1:27017/lightcms-test?replicaSet=rs0&directConnection=true`, `-p 1` throughout; no `.env.test` created (verified absent); Go `go1.25.0 darwin/arm64`.

Note: the first final-suite attempt exposed one failure — `TestE2E_RestoreVersion404` — which pinned the OLD gap-(d) 500 behavior while explicitly naming this task's fix as specified. The pin was updated to the intended 404 (§6, fix F3) and the suite re-ran fully green. This is the locks-working-as-designed path, landed explicitly, not silently.

Coverage spot-check on touched packages (17D method: isolated runs, unique DB names, block-count aggregation): publication **1501/1817 = 82.6%** (identical to 17D), generation **856/987 = 86.7%** (17D: 855/987 = 86.6%, +1 from the new test), migration **365/432 = 84.5%** (17D: 366/432 = 84.7%; −1 statement inside the pre-existing diffuse partial branches of `backfillTemplates` noted by 17D — timing-dependent branch, not a new uncovered path; the new `$or` filter lines are covered). Migration stays a sub-85% follow-up as at 17D; the ≥85% aggregate gate is unaffected (Δ −1 stmt of 4962).

## 2. MCP binary rebuild + tool inventory (rehearsal 2)

- `go build -o bin/lightcms-mcp ./cmd/mcp` per CLAUDE.md → ok (`bin/` is gitignored; local verification only, nothing committed).
- Runtime `tools/list` via in-memory MCP session: **122 tools**, including `get_template_schema` — matches the Task 16 documented inventory (122).
- Full runtime tool list retained at `/tmp/19-mcptools.txt`.
- **Doc drift (non-blocking, docs owner):** `MCP.md` claims "92 tools total", `CLAUDE.md` claims 115. Both predate V3 additions. Refresh deferred; runtime is authoritative. *(Resolved 2026-10-02: `MCP.md`/`CLAUDE.md` → 122 via wave 3C; `README.md` counts + tool breakdown refreshed with waves 1–4 evidence — see §9/§11.)*

## 3. Migration rehearsal on representative fixtures (rehearsal 3)

Harness (temporary, retired after green): `migration_test` fixtures — verified page (canonical == fresh render), hash-mismatch page, missing-static page, soft-deleted page, fork copy sharing a live path, case-collision pair (`/t19col/Item` vs `/t19col/item`), empty-`full_path` row, template doc lacking `current_version` — on the disposable test DB (a copy-class environment; production data never touched).

- **Dry-run:** zero writes (flag never left `not_started`); `invalidPaths=1, collisions=1, missing=1, legacyUnverified=1, verified=1`; `BlockingCount=4`.
- **Apply while blocked:** refused with `ErrMigrationBlocked: 4 blocking items remain`; `Completed=false`. Non-blocked pages still reconciled (resume-safe design), non-related pages unaffected.
- **Admin repair → re-run:** deleted the empty-path row, removed one collision variant, supplied the missing canonical → `Completed=true`, `NewIndexesVerified=true`, `LegacyIndexDropped=true`. Resume re-run: completed no-op.
- **Template backfill (gap b):** `current_version` went from absent → `1` (post-fix; was `<nil>` pre-fix).
- **No legacy URL disappears:** `/t19/verified` and `/t19/mismatch` serve byte-identical bytes before/after; collision survivor `/t19col/Item` serves; repaired `/t19/missing-static` serves. Fork copy untouched by backfill (no `canonical_full_path` written).
- **Scanner pass post-completion:** `MigrationState=completed`, `ContentsScanned=5` (fork excluded — fix F1 working), `OrphanQuarantined=1` — exactly the soft-deleted page's stale file, bytes preserved under `quarantine/` (never deleted). Zero repair errors, zero P0s, all serving files intact.

## 4. Crash rehearsal with real kill -9 (rehearsal 4)

Harness (temporary, retired after green): parent seeds v1 live on a shared test DB + store root; a **separate OS process** (test-binary re-exec) performs the v2 operation with a blocking hook placed exactly at the crash point (post-rename / post-backup-stage, pre-commit — the `ErrStopAfterRename` point, but the process is SIGKILLed, no error returned, no compensation); parent `kill -9`s the victim, then a freshly-wired instance on the same data models "restart same binary".

- **Mid-publish kill:** crash facts confirmed (v2-crash bytes on disk, DB active still v1). Restart + `ScanOnce`: `PreviousRestored=1`, authoritative v1 bytes restored, active unchanged. Clean republish → 200 with a NEW publication id, live v2 bytes served. Outbox drained: `content.publish` for the new publication **delivered**; CDN purge invoked for the page URL.
- **Mid-unpublish kill** (backup staged, CAS never ran): restart + `ScanOnce`: `BackupRestored=1`, live page still serving v2 bytes, active unchanged. Clean unpublish → 200, canonical 404, active cleared. Outbox drained: `content.unpublish` **delivered**.
- Incidental resilience evidence: a timed-out rehearsal run left a still-sleeping victim holding an idempotency lease; the next run's same-key `Begin` correctly returned `REQUEST_IN_PROGRESS` instead of double-publishing. Zombie hygiene (unique keys per spawn, cleanup-kill) was then built into the harness.

## 5. One-binary / one-UI check (rehearsal 5)

- `Dockerfile` builds exactly one binary (`lightcms` from `./cmd/server`); `start.sh` ends in `exec ./lightcms` (single process; WARP sidecar is egress-only, not a LightCMS service).
- `cmd/server/main.go` registers 312 routes in one process: admin UI (`/cm`), REST API (`/api/v1`), authenticated MCP Streamable HTTP, public read-only MCP (`/mcp-public`), public site, `/health`, `/healthz`, `/llms.txt`.
- `fly.toml`: one app, one process group, one `http_service`; only external dependencies are MongoDB and the `lightcms_data` volume mount. The `lightcms-mcp` stdio binary is a client of the HTTP API, not a second service.
- No second LightCMS service is required beyond MongoDB + filesystem storage. **PASS.**

## 6. Gap triage + verdicts

### F1. Scanner quarantined live pages that have fork copies — FIXED (release-blocker class)
- **Repro:** fork copy sharing a live path + `completed` flag + live active → `ScanOnce` quarantined the LIVE canonical (`OrphanQuarantined=1`, red test `TestScannerForkCopyKeepsLiveOnline`).
- **Root cause:** `ScanOnce` enumerated `content` with `bson.M{}` — no `fork_id` filter, while migration and every other reader exclude forks. The fork doc hit `reconcileNoActive` → orphan rule → live page taken offline. In production this fires on any post-migration scan wherever an editor/agent fork exists.
- **Fix (1 line + comment, `recovery.go`):** enumerate with `{"fork_id": nil}` (same predicate as migration's analyze; nil covers missing+null).
- **Tests:** new `TestScannerForkCopyKeepsLiveOnline` (red→green). Full suite green.

### F2. Gap (b): backfill misses templates lacking `current_version` — FIXED
- **Repro:** seed template without the field → post-`Run` value still `<nil>` (red test `TestBackfillTemplateMissingCurrentVersion`).
- **Root cause:** Mongo comparison `$lte` never matches a missing field; Go-side `CurrentVersion <= 0` guard entered the branch but the update matched zero docs.
- **Fix (2 sites, `migrate.go` `backfillTemplates`):** filter gains `$or: [{current_version: {$lte: 0}}, {current_version: {$exists: false}}]`. Spec-mandated (§35.2 backfill `CurrentVersion=1`).
- **Tests:** new `TestBackfillTemplateMissingCurrentVersion` (red→green). Full suite green.

### F3. Gap (d): unknown rollback sources / template-not-found → 500 — FIXED
- **Repro:** `RevertLive` with unknown source id → 500; templatecontract not-found/version codes → 500 (red test `TestMapSagaErrPreservesNotFound`).
- **Root cause:** `mapSagaErr` had no cases for codes `mapSagaCode` legitimately produces (`PUBLICATION_NOT_FOUND`, `TEMPLATE_NOT_FOUND`, `TEMPLATE_VERSION_NOT_FOUND`, `TEMPLATE_VERSION_CONFLICT`), so they fell to the `CodeInternal` default — contradicting spec §27's code list and the module's own `StatusForCode` 404/409 mappings (§20.8).
- **Fix (`service_publish.go`):** preserve the four codes (404/404/404/409). Deliberately updated two pins that locked the old behavior: `cover_gap_internal_test.go` mapping table and `TestE2E_RestoreVersion404`, whose own comment named this exact fix as specified.
- **Tests:** new `TestMapSagaErrPreservesNotFound` asserting code + HTTP status (red→green). Full suite green.
- **Accepted-risk remainder:** BARE storage errors still map 500 — `mapSagaCode`'s `templatecontract.CodeOf` fallthrough (never returns `""`) swallows them before the storage branch. Store failures raised through the saga's stage/verify/activate/unpublish cases correctly yield 503. Reordering `mapSagaCode` would change `completePublishError` status computation + locked pins: deferred to the generation owner.

### F4. Gap (e): `isStoreErr` tautology — FIXED (no behavior change)
- `storage.CodeOf` maps every non-nil error (unknown → `CodeIO`), so `CodeOf(err) != "" || isStoreErr(err)` could never be false and the second return was dead. Both branches returned the identical retryable 503. Removed the condition and the now-unused helper; pin updated to the `CodeOf` shape. Full suite green.

### W1. Gap (a): empty `full_path` blocks migration `completed` — WORKFLOW (correct behavior, no fix)
- **Repro:** seeded empty-path row → dry-run `InvalidPaths=1`, apply refused (`BlockingCount` includes it).
- **Verdict:** spec-correct. `pathkey.Canonical("")` rejects ("empty path"); the homepage legitimately uses `/` (see `content.go`), so `""` is corrupt legacy data, not a valid page. Invalid paths are blocking per §35.1/§35.3 by design: report → admin repairs/deletes the row → resume (verified in rehearsal 3). No code change.

### W2. Gap (c): migrated v1 `draft` → publish 409s until activated — WORKFLOW (correct behavior, no fix)
- **Verified live:** draft template → publish returns 409 `TEMPLATE_NOT_ACTIVE` with zero mutation; `draft → active` transition (explicitly allowed by `ValidateStatusTransition`) → publish succeeds (temporary verification test, retired; the 409 gate itself is pinned in-suite by `TestE2E_TemplateStatusGate`).
- **Verdict:** spec §35.2 mandates creating v1 (fields + layout + metadata) but no status; preserving the legacy status (absent → `draft` via `NormalizeStatus`) is the conservative reading, and the 409 is the correct gate signal, not an error. Activation is the operator workflow. No code change.

### R1. Gap (f): regenerate-noop 410 vs upgrade-redirect — DECISION: keep 200-no-op for 7.3.0 (accepted risk)
- `ContentService.RegenerateAllContent` is a deliberate nil no-op (Task 16B fail-safe: explicit per-page Publish is required); `POST /api/v1/regenerate` answers 200 with a success message while changing nothing.
- Changing to 410 now is NOT small: it breaks the MCP `regenerate_all_content` tool, the CLI, `apiclient`, the admin UI button, and three verdict locks (`TestConRegenerateNoopVerdict`, `TestE2E_RegenerateVerdict`, fault-injection pin) that require an explicit decision. The locks exist precisely so the change cannot land silently.
- **Decision recorded:** keep current behavior for 7.3.0; the success message is misleading and MUST NOT be relied on for cache invalidation. Owner: integration owner. Follow-up: 410 Gone with migration note, or redirect to a template-upgrade job, post-7.3.0.

### R2. Gap (g): scheduler-tick crash-lease `TakeOver` unowned — RECORDED (no fix)
- The shadowed `op, terr := TakeOverByKey(...)` in `publish_internal.go` is real, but the specified one-line fix (`op, terr = …`) is insufficient on its own (a taken-over non-completed op still falls through to a fresh saga publish on a zero op), and `TestConSchedulerCrashTakeover` locks today's `IDEMPOTENCY_NOT_FOUND`-with-zero-effects behavior.
- **Release impact is narrow and self-healing:** the path surfaces `ErrPublishRetryLater`; the next scheduler tick retries the same stable key — no duplicate publication, no data loss, no stuck page.
- Owner: scheduler/idempotency owner. Decision needed post-7.3.0: fix-and-retry (assign + continue with the taken-over op) vs scanner-owned lease repair.

## 7. Version + changelog

- `build.json`: `7.2.2` → `7.3.0` (feature release for the V3 program).
- `CHANGELOG.md`: new `## [7.3.0] - 2026-09-30` entry (V3 system summary + the four Task 19 fixes). Format follows existing entries.
- Tag `v7.3.0` is cut LATER by the owner — this task does NOT tag (per CLAUDE.md release process: bump, changelog, tag later; MCP binary must be rebuilt from tagged source).

## 8. Full file list of this commit

Modified: `build.json`, `CHANGELOG.md`, `internal/product/publication/recovery.go` (F1), `internal/product/migration/migrate.go` (F2), `internal/product/generation/service_publish.go` (F3, F4), `internal/product/generation/cover_gap_internal_test.go` (pin updates), `internal/product/e2e/wave2_test.go` (pin update).
Added: `internal/product/publication/fork_scan_test.go`, `internal/product/migration/currentversion_backfill_test.go`, `internal/product/generation/maperr_notfound_test.go`, `docs/implementation/release-checklist.md`, `docs/implementation/release-report.md` (this file).
Retired after green (not committed): migration rehearsal, kill-9 rehearsal, gap-(c) verification harnesses. Restored: `internal/handlers/static/sitemap.xml` (test-run side effect, reverted — 17D left it dirty; this tree is clean).

## 9. Signoff — PENDING external review (do NOT call production-ready before these)

The rehearsals above verify behavior against the spec on disposable data. They do NOT substitute for independent review. Each box needs a named reviewer, the evidence pointer, and a sign/date before the 7.3.0 tag:

- [ ] **Architecture review.** Evidence: this report §6 (F1–F4 diffs, W1/W2/R1/R2 decisions), spec §§ 20.8/21/27/35, `docs/implementation/entry-point-matrix.md`. Focus: error-contract deltas (new 404/409 surfaces), regenerate keep-200 verdict, scheduler-lease decision (R2), fork-exclusion predicate consistency across readers.
- [ ] **Security review.** Evidence: e2e security table (`TestE2E_SecuritySandbox/Scopes/StoredXSS/AssetSSRF/InputHardening`), §39.8; sandbox/scopes enforcement unchanged by this commit (no auth-path files touched — verify via `git diff --stat`). Focus: 500→404/409 changes don't leak existence info beyond policy; quarantine-preservation (never delete) holds for the new fork path.
- [ ] **Operations review.** Evidence: §3/§4 rehearsal logs, `docs/BACKUP-RESTORE.md`, `docs/DEPLOYMENT-PRODUCTION.md`, `docs/PUBLICATION-RUNBOOK.md`, `docs/UPGRADE.md`. Focus: production migration drill on a COPY (dry-run report as audit record), backup/restore drill, deploy.sh run, post-deploy `/healthz` + scanner clean pass + flag `completed`.
- [ ] **CI gate.** `.github/` is absent in this tree — the release cannot be called fully-gated without CI. Owner must restore CI (or confirm the canonical repo runs it) and attach a green CI run for the release commit. **This is a release condition, not a waiver.**
- [ ] **Docs follow-up (non-blocking):** ~~refresh `MCP.md` (92) and `CLAUDE.md` (115) tool counts~~ — **resolved 2026-10-02**: both now document the runtime-verified 122 (wave 3C); schedule R1 (regenerate) and R2 (scheduler lease) decisions.

## 10. Conflicts / out-of-scope observations

- No other worktree touched; no merge/push performed; `newsPage` untouched.
- Mongo container reused healthy throughout; never stopped/removed/wiped. Test DBs used: shared `lightcms-test` (established pattern + `CleanupCollections`), dedicated `lightcms-test-e2e` (e2e package), `lightcms-test-t19` (kill-9 rehearsal, retired). Production data never touched.
- Pre-existing conditional SKIP unchanged (`TestHomepage_WebsiteJSONLD`).
- No `.env.test` or binaries committed. `bin/lightcms-mcp` rebuilt locally only (gitignored).

## 11. Post-rehearsal addendum — waves 1–4 merged before the tag (2026-10-02)

The `v7.3.0` tag was not cut after §9; integration continued on main through four
hardening waves plus the bilingual UI program. 7.3.0 therefore now includes, on top
of §1–§10:

- **i18n (ui-a–d)**: full zh/en admin + site UI, ~986 dictionary keys, root-scope `Lang` fix.
- **Wave 1**: granular RBAC on destructive admin POSTs; serving-layer safety filters and
  served-truth alignment; outbox idempotency, delivery payload race, search-index pinning.
- **Wave 2**: publish/batch scope enforcement (`content.edit` + `content.publish`);
  428 `IDEMPOTENCY_KEY_REQUIRED` on publish, batch-publish, and search-replace execute;
  per-item batch replay (same-key replay returns original outcomes, no duplicate
  publications); bulk create rejects `published:true`; migration boot + version/actor
  attribution fixes; saga error mapping, comment provenance, template rename race,
  theme writer.
- **Wave 3**: `apiclient` auto-mints idempotency keys and parses both error envelopes
  into `APIError`; legacy JSON errors carry a sibling machine-readable `code`
  (docs/API.md §12); MCP `publish_content`/`publish_multiple` return
  `publication_id`/`public_url` (tool count unchanged at 122).
- **saveVersion CAS fix** (`e596177`): the CAS-allocated version number is inserted and
  heals only when that version already exists — concurrent writers can no longer jump
  history (`UNIQUE(content_id, version)` holds under load).
- **Wave 4 (admin UI ↔ control plane)**: admin form checkbox publish/unpublish now runs
  the PublicationService saga (legacy byte-for-byte fallback when unwired); RBAC gates on
  all seven publication endpoints and content create/update/revert; restore/revert/upgrade
  forms fixed end-to-end (render-time `idempotency_key`, `expected_active_id` CAS,
  registered route targets, CSRF stamping + real layout execution on product pages);
  edit-page 发布上线 button + 发布历史 link; nested delete-form fix; merge-result
  `requires_publish` links.

Evidence refresh (waves 1–4 scope): full regression `go test -p 1 ./... -count=1` on main
at `6e0d8d1` (later deltas: docs, example pages, and a `theme-vars.css` `--primary` default
tweak that no test reads or embeds) → **exit 0, 29 packages ok, 0 FAIL**; `go vet ./...`
clean; `bin/lightcms-mcp` rebuilt (122 tools unchanged — verified by source count: 122
`mcp.AddTool` registrations on the main server, plus the separate 4-tool read-only
`/mcp-public` subset). Log:
`/tmp/wave5-regression-20261002-020908.log`. The §9 external reviews should treat the
wave 1–4 deltas as in-scope additions — security review in particular now covers the new
RBAC gates and saga-delegated form saves.

## 12. Post-review hardening round — R01–R12 + admin/API findings (2026-10-02)

An independent implementation review (`docs/reviews/2026-10-02-code-implementation-review.md`,
12 findings R01–R12 against `de943b7`) plus a self-review of the wave 1–4
surface (5 blockers + 9 majors) were verified claim-by-claim against the
code — every cited location reproduced — and fixed together in this tree:

- **R01/R09 (authz)**: `generation.Actor` carries `Role`; all checks use
  `Can()` (role ∩ sandbox ∩ scopes); extractors populate it; sandbox mode
  resolves the owned active fork by (user, session) with fork-session
  attribution on creation.
- **R02 (render)**: production saga renders via the frozen snapshot
  pipeline (`SnapshotRender`, wired in `buildPublicationRuntime`);
  `AuthorIsAdmin` threaded from every publish caller; provenance from the
  actual result.
- **R03 (cutover)**: prepare-verify-copy-then-single-rename; concurrent-stat
  test proves zero missing-window.
- **R04–R07 (idempotency)**: takeover + resume (skip-written-content,
  complete-cached-when-active), auth-before-Begin, replay-before-gates,
  422-rebuild, stable hashes, heartbeat + ownership fencing.
- **R08/R11 (commit)**: redirect in activation txn; unknown-commit
  no-compensation + idempotent re-stage; symmetric unpublish.
- **R10/R12 (deploy)**: single-instance liveness gate; production HTTPS
  base-URL fail-fast (incl. `prod` env).
- **Admin/API**: ServePage deleted filter; 22 mutating admin endpoints
  gated; copilot edit+publish; middleware 401 codes; REST/admin/comment/
  delete provenance; admin publish idempotency keys; single-point
  draft-only (fork-exempt); 409 CAS mapping; fork-free embeddings with
  projections; MCP description accuracy; `%q` escape fix.

Evidence: full regression `go test -p 1 ./... -count=1` on this tree →
**exit 0, 29 packages ok, 0 FAIL**; `go vet ./...` clean; both MCP and
server binaries rebuild from this tree. Log:
`/tmp/review-fixes-regression-20261002-190543.log`. New regression tests
were added per fix (see commit file list); three pre-existing tests were
updated to the corrected semantics (poison-lease wedge → no-lease 403,
lease-expiry 409 → takeover resume, stale-version-without-key 428).
The §9 external reviews remain the release gate and should treat this
round as in-scope additions — particularly the R01/R09 authz changes and
the R02 renderer swap, which alter externally visible behavior by design.
