# ADR-001: Draft Mutation Policy (Published Content stays stable while drafts change)

- **Status:** proposed
- **Date:** 2026-09-30
- **Task:** 0 (LightCMS V3 template static publishing program)
- **Spec:** `docs/LightCMS_V3_模板化页面生成与静态发布系统_最终设计方案.md`
  (§39.10 Content Mutation Policy, §39.9 Legacy Entry Point Contract)
- **Plan:** `docs/superpowers/plans/2026-09-30-lightcms-template-static-publishing.md`
  (Global Constraints, Tasks 8/15/16A/16B)

| Role | Approver | Decision |
|---|---|---|
| Tech lead | _(slot)_ | ☐ approved / ☐ changes requested |
| API owner (co-sign, external behavior change) | _(slot)_ | ☐ approved / ☐ changes requested |
| Security owner (co-sign, permission boundary) | _(slot)_ | ☐ approved / ☐ changes requested |

## 1. Scope / Context

Today `ContentService.UpdateContent` (`internal/services/content.go:441`) is a
data write **and** a live-site mutation: saving a published page implicitly
re-renders and rewrites its served HTML (via the `GenerateStaticPage` path) and
can emit publish webhooks. The same implicit-live-write behavior exists across
the legacy entry points inventoried for Task 1:

- `internal/services/content.go` — `CreateContent`, `UpsertContent`,
  `BulkCreateContent`, `UpdateContent`, `PublishContent`, `UnpublishContent`,
  `RegenerateAllContent`, `RegenerateIndexPages`, `renderContent`
- `internal/services/template.go` — template edit auto-regenerates live pages
- `internal/services/fork.go` — `ForkService.Merge` touches live output
- `internal/services/regen_queue.go`, `internal/services/content_watcher.go` —
  background regeneration writes live files
- Callers: `internal/handlers/api_content.go`, `internal/handlers/handlers.go`,
  `internal/handlers/copilot.go`, `internal/mcp/content_tools.go`,
  `internal/cli/content.go`, `internal/apiclient/client.go`,
  `internal/services/{scheduler,approval,import}.go`

`models.Content` (`internal/models/models.go:49-50`) carries `Published bool`
and `PublishedAt *time.Time`, which today are treated as live truth. The V3
program redefines them as **compatibility projections**; the unique active
Publication is control-plane truth and the canonical file is served truth
(plan Global Constraints).

This ADR fixes the code-level boundary so parallel workers (Tasks 8, 15, 16A,
16B) do not each re-interpret "save vs publish".

## 2. Decision records

- **D1 — Draft-only mutation.** `CreateContent` / `UpdateContent` (and
  upsert/bulk variants) perform data + `content_versions` writes only. They
  MUST NOT write or delete canonical static files, MUST NOT flip
  `Published`/`PublishedAt`, and MUST NOT emit publish webhooks. On a
  published page they set `HasUnpublishedChanges=true` (new field, Task 2) and
  route the edit through a Fork per the matrix below.
- **D2 — Explicit publish command.** Only `PublicationService.Publish /
  Unpublish / Rollback` (Task 8) may change the active Publication pointer,
  canonical files, `Published`/`PublishedAt` projections, and publish outbox
  rows. Legacy `PublishContent` becomes a delegating adapter in Task 16A.
- **D3 — Mutation Policy matrix (entry × state → behavior).**

  | Entry | Unpublished page | Published page |
  |---|---|---|
  | Admin / REST / MCP / CLI / Copilot / Import / Search-replace edit | write draft in place | open or reuse Fork; live bytes + active Publication unchanged |
  | Template HTML edit (`TemplateService.UpdateTemplate`) | create new TemplateVersion (ADR-002) | same; NEVER regenerate live pages |
  | Fork merge | merge into draft, `requires_publish=true` | merge into draft, live unchanged until explicit Publish |
  | Scheduler / import auto-publish, template upgrade | — | route through PublicationService with stable internal operation key (Task 16D) |

- **D4 — Documented breaking change.** Updating published Content creates
  unpublished changes; clients that depended on PUT immediately changing live
  MUST call Publish explicitly. `GenerateResponse` carries `requires_publish`;
  `published=true` may coexist with `requires_publish=true`.
- **D5 — Direct-live draft requires explicit permission.** Any bypass of the
  Fork path (e.g. trusted internal jobs) needs a dedicated scope/permission
  checked in the service layer, not a handler flag.

## 3. Rejected alternatives

- **A1 — Keep implicit live write, add an opt-out flag.** Rejected: every
  caller must remember the flag; one forgotten caller silently changes live
  content. Default-open is the wrong default for a publishing system.
- **A2 — Dual-write (draft + live) with "last writer wins".** Rejected:
  destroys the draft/live distinction the whole program is built on, and makes
  crash recovery (§39.5) untestable.
- **A3 — Deprecate but preserve old behavior behind a config switch.**
  Rejected: two live-write paths must both be migrated, tested, and recovered;
  doubles the Task 16/17 surface with no release benefit.

## 4. Consequences / risks

- Old REST/MCP/CLI clients that relied on "save == live" will see stale live
  pages until they call Publish; mitigated by D4 migration docs (Task 18) and
  the `requires_publish` signal in every mutation response.
- Fork-volume growth: every published-page edit creates Fork data; bounded by
  existing Fork lifecycle + Task 10 retention rules.
- Temporary dual truth during migration (Task 14): `Published` projection vs
  active Publication; scanner treats `legacy_unverified` pages as served and
  never quarantines them.

## 5. Crash / failure / concurrency behavior

- A crash mid-`UpdateContent` leaves draft data without any live effect by
  construction (no live write exists on this path). Mongo transaction
  (Task 2 `WithTransaction`) keeps Content + Version + operation-ID binding
  atomic (ADR-005).
- Concurrent edits to the same draft resolve via Content Version CAS
  (Task 2 `CurrentVersion`); concurrent creates on `/News/Foo` vs `/news/foo`
  resolve via the canonical-path unique index (Task 2), never via this policy.

## 6. Migration & rollback

- Migrate: Task 16A rewrites `UpdateContent`; Task 16B rewrites template/fork
  paths; Task 16 gate 16A/16B tests assert canonical HTML unchanged after
  published PUT / template edit / fork merge.
- Rollback: revert the 16A/16B commits; draft-only behavior disappears and
  implicit live writes return — acceptable only pre-G3, recorded in
  `docs/implementation/release-report.md`.

## 7. Verification (commands + expected evidence)

```bash
go test -p 1 ./internal/services ./internal/handlers -count=1
# Task 16A/16B tests must assert: published PUT leaves canonical HTML unchanged,
# active Publication ID unchanged, HasUnpublishedChanges=true, no publish webhook.
rg 'GenerateStaticPage\(|PublishContent\(' internal cmd --glob '*.go' --glob '!**/*_test.go'
# Every remaining hit accounted for in docs/implementation/entry-point-matrix.md (Task 16).
```

Current (pre-implementation) evidence: the `rg` above returns hits in
`content.go`, `template.go`, `fork.go`, `regen_queue.go`, `content_watcher.go`,
API handlers, MCP tools, CLI, apiclient, scheduler, approval, import, copilot —
this list is the migration target set for Tasks 16A–16D.

## 8. Approval record

_To be filled at G1 review. Unapproved ADRs MUST NOT be handed to parallel
developers for independent interpretation (spec §42)._
