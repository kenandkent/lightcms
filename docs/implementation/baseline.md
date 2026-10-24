# Baseline Pin — Task 1 (Template & Static Publishing Program)

- **Pinned SHA:** `534e3c7` (full `534e3c7dda2ecd82d3302c01a2469997621a6142`)
- **Branch:** `task/1-baseline-audit` (worktree `/Users/ken/workspace/worktrees/task-1`)
- **Date:** 2026-09-30
- **Spec:** `docs/LightCMS_V3_模板化页面生成与静态发布系统_最终设计方案.md`,
  SHA-256 `60222a89251cd6e15fc1d9e10d8ad7ca11f45bf6bf44fdb9f7a3c16100704f86` — verified to match the plan's §Spec hash.

## 1. Baseline chain (reconciliation with plan Global Constraints)

The plan names baseline `c1165be` (v7.2.2) and instructs recording a new
baseline before rebasing. The pinned baseline is:

```
c1165be  v7.2.2 (plan's named baseline)
e01c1cc  docs: add LightCMS V3 template static publishing design and plan
29a7276  chore: remove GitHub workflows (ci, publish-mcp-image) — owner decision
534e3c7  chore: task 0 reproducible test environment and ADR drafts (Task 0)
```

No Go-source delta vs `c1165be` outside the two docs commits plus Task 0's
test fixture/ADR additions (`docker-compose.test.yml`, `.env.test.example`,
`docs/adr/ADR-001…005`, `docs/implementation/test-environment.md`).
Task 16's migration targets therefore start from unmodified v7.2.2 behavior.

## 2. Dirty-tree / CI state (re-verified)

- `git status --short --branch` at audit start: clean, on
  `task/1-baseline-audit`, tracking nothing ahead/behind (verified 2026-09-30;
  only the two new Task 1 docs below are untracked until committed).
- **`.github/` does not exist in this worktree at all** (stronger than
  "workflows/ empty"): owner commit `29a7276` deleted `ci.yml` and
  `publish-mcp-image.yml` along with the directory. Task 0 §5 decision stands:
  **no workflow file was created or restored in Task 1**, per the plan
  ("Do not silently restore or overwrite a user-deleted workflow"). CI must be
  re-added by the repo owner before G4.
- Abort rule: if `HEAD` moves off `534e3c7` without a revised matrix, this
  audit is stale and Task 16 must not consume it.

## 3. Build / vet / test evidence (this worktree, 2026-09-30)

| Check | Command | Result |
|---|---|---|
| Pin | `git rev-parse HEAD` | `534e3c7dda2ecd82d3302c01a2469997621a6142` |
| Build (plan gates) | `go build ./cmd/server ./cmd/mcp ./cmd/cli` | exit 0 |
| Build (all) | `go build ./...` | exit 0 |
| Vet | `go vet ./...` | exit 0 |
| Unit gate (no Mongo) | `go test ./... -count=1` | all packages `ok`; DB-backed tests `SKIP` (`skipping: MONGODB_URI not set`, no `.env.test` in tree). Per Task 0 §3 this is NOT integration evidence. |
| Replica-set fixture (Task 0 stack) | `docker compose -f docker-compose.test.yml up -d` + mongosh | `mongo-init`: `RS ready (myState=1)`; `db.version()` → `7.0.14` |
| DB spot check on fixture | `go test -p 1 ./internal/database/ -count=1` (with gitignored `.env.test` copy) | `ok … 1.158s`; 41 `--- PASS`, 0 skips |
| Teardown | `docker compose -f docker-compose.test.yml down -v`; removed local `.env.test` | tree clean (`git status --short --branch` shows only branch line) |

## 4. Index baseline (`internal/database/mongo.go`, `createIndexes`)

- 46 `mongo.IndexModel` specs, all created eagerly in `Connect`.
- **Confirmed: `content` UNIQUE `(full_path, fork_id)`, SPARSE**
  (`mongo.go:78-84`). Live pages carry no `fork_id` (null); fork copies share
  `full_path` with an ObjectID `fork_id`. Task 14 drops this only after the new
  canonical index is verified.
- **Confirmed: `templates.slug` has NO index at all** — the only unique
  template index is `templates.name` (`mongo.go:159-165`). Task 2 adds unique
  Template slug / Template Version indexes; Task 14 remediates collisions first.
- `content.slug` is a NON-unique index (`mongo.go:93-98`; old `slug_1` unique
  dropped at `mongo.go:90`).
- **No `partialFilterExpression` anywhere** in `internal/` (rg: zero hits) —
  the Task 2 `UNIQUE(canonical_full_path, path_scope)` partial index does not
  exist yet.
- Other unique indexes (unchanged): `snippets.name`, `folders.path`,
  `custom_pages.slug`, `redirects.from_path`, `assets.full_path`,
  `api_keys.key_hash`, `oauth_*`, `users.email`, `content_locks.content_id`.
- TTL indexes: `audit_logs` 365d (`audit_ttl_365d`), `oauth_*` expiry,
  `webhook_deliveries` 30d, `import_logs` 90d.

## 5. Absent V3 primitives (correctly not-yet-built)

- No `internal/product/` tree.
- No `DB.WithTransaction`, no `DB.EnsureProductIndexes` (rg: zero hits).
- No `pathkey.Canonical`; no `CanonicalFullPath`/`PathScope`/`PathActive`/
  `CurrentVersion`/`HasUnpublishedChanges` on `models.Content`.
- `models.ContentVersion.Version` is `int` (`internal/models/models.go:93`) —
  Task 2 changes it to `int64` with handler/API conversions.
- No `DeliverRecordedEvent` on `WebhookService`
  (`internal/services/webhook.go`: only `FireEvent`/`deliver`/`sign`/
  `retryWithBackoff`) — Task 9 adds the synchronous entry point.
- No `get_template_schema` MCP tool — Task 16D adds it.

## 6. Test-guard state (re-verified, read-only)

- `internal/testutil/testutil.go:85-89` — `MustConnectTestDB` refuses any
  `DATABASE_NAME` not containing `test`. (Task 0 verified the REFUSING path;
  not re-triggered here — no source change to re-verify.)
- **Known gap, unchanged (owned by Task 2, NOT fixed here):**
  `internal/database/mongo_test.go:17` local `testDB` helper has no `test`
  guard (kept local to avoid an import cycle). Task 2 owns
  `internal/database/mongo.go` + `internal/testutil/testutil.go` and should
  add the guard or route through testutil.
- No Go source files were modified in Task 1 (audit-only); `go build`/`go vet`
  above ran against the pristine baseline tree plus the two new docs.

## 7. Route / tool inventory counts (detail in `entry-point-matrix.md`)

- `cmd/server/main.go`: 293 `HandleFunc`/`Handle` registrations —
  134 Admin (`/cm`), 126 `/api/v1`, 14 legacy `/api`, 5 OAuth, 1 authed MCP
  (`/mcp`), 1 public MCP (`/mcp-public`), plus health/healthz/well-known/
  sitemap/robots/llms.txt/llms-full.txt/static/ServePage catch-alls.
- `/api/v1` middleware chain (in order): `apiAuthMiddleware.Middleware`
  (Bearer key → user+scopes/sandbox; OAuth 2.1 → system key; session-cookie
  fallback for Admin UI) → `APIBurstRateLimit` (20 req/s) →
  `APIRateLimit` (300 req/min) → `APIBodySizeLimit` (10 MiB) → provenance
  stamper (`WithProvenance` human|agent via `X-Agent-Session`,
  `WithEditorEmail`).
- MCP: 121 authenticated tools + 4 public read-only tools
  (`get_site_info`, `list_pages`, `search_site`, `get_page`); no `tools/list`
  runtime dump was possible without a configured Mongo (recorded prerequisite
  for G3, per plan checklist). Note: `CLAUDE.md` says "115 total" — stale;
  actual count is 125 (see §8 drift notes).
- CLI (`internal/cli/cli.go:32-59`): 12 top-level commands
  (`content`, `template`, `asset`, `theme`, `config`, `redirect`, `folder`,
  `collection`, `search`, `search-replace`, `api-key`, `regenerate`).
- Copilot (`internal/handlers/copilot.go:49-96`): 10 tools, executing directly
  against the service layer (not via MCP).
- Direct `GenerateStaticPage`/`PublishContent` callers: enumerated in
  `entry-point-matrix.md` §3 (rg across `internal/` + `cmd/`, excluding
  `*_test.go`); every live-file mutation row is marked as a Task 16 migration
  target. Notable find: the Admin UI has a parallel static-write path
  (`h.generateStaticPage`, `handlers.go:3790`, 6 call sites, NO fork guard)
  that bypasses `ContentService` — see matrix §4.

## 8. Plan/brief/spec drift notes (no silent choices)

1. **Plan path drift (plan ↔ repo):** plan Global Constraints give the project
   root as `/Users/ken/workspace/newsPage/lightcms`, which does not exist;
   the repo root (Go module, `cmd/`, `internal/`) is `/Users/ken/workspace/newsPage`
   (this worktree mirrors it). Audit recorded per repo root.
2. **Brief wording vs fact:** the task brief says "`.github/workflows/` is
   empty"; fact is `.github/` is absent entirely. No workflow file created or
   restored either way.
3. **Stale tool count (docs ↔ code):** `CLAUDE.md` claims 115 MCP tools;
   the tree registers 121 authenticated + 4 public = 125 tools. Docs drift only.
4. **Commit-message deviation (plan ↔ brief):** plan Task 1 suggests
   `docs: pin LightCMS baseline and entry points`; the task brief mandates
   `chore: task 1 baseline pin and audit`. Followed the brief.
5. **No spec↔plan conflicts** within Task 1 scope (spec §42 ADR contents ⊇
   plan Task 0 file list; Task 1 file list matches the two docs created here).

## 9. Deliverables

- `docs/implementation/baseline.md` (this file)
- `docs/implementation/entry-point-matrix.md` (entry × handler × service ×
  permission × static side effect × target task; Task 16 consumes it)
