# ADR-004: Filesystem Static Store (immutable objects, atomic cutover, scanner, GC)

- **Status:** proposed
- **Date:** 2026-09-30
- **Task:** 0 (LightCMS V3 template static publishing program)
- **Spec:** `docs/LightCMS_V3_模板化页面生成与静态发布系统_最终设计方案.md`
  (§40.1 MVP topology, §38 performance, §45.3)
- **Plan:** `docs/superpowers/plans/2026-09-30-lightcms-template-static-publishing.md`
  (Tasks 6/10/16E; MVP static provider is filesystem, R2/S3 post-MVP)

| Role | Approver | Decision |
|---|---|---|
| Tech lead | _(slot)_ | ☐ approved / ☐ changes requested |
| Operations owner (co-sign, volumes/capacity/GC) | _(slot)_ | ☐ approved / ☐ changes requested |

## 1. Scope / Context

MVP serves static HTML from a persistent filesystem with immutable publication
objects and an atomic canonical rename (plan Architecture). The filesystem MUST
support atomic same-directory rename + fsync; R2/S3 is explicitly post-MVP and
requires a separate Storage ADR + full integration suite before any provider
config beyond `filesystem` is accepted at startup (spec §36, plan Task 16E).
Lane B (Task 6) owns the store; the integration owner owns the scanner/GC
(Task 10) and single-process wiring (Task 16E).

## 2. Decision records

- **D1 — Layout.** Per publication:
  `content/publications/{contentID}/{publicationID}/index.html` (immutable,
  content-addressed by SHA-256, never overwritten); canonical served file per
  existing public path convention; sidecar names `.tmp-*`, `.next-*`,
  `.previous-*`, `.unpublish-backup-*` all in the SAME directory as the
  canonical file (no cross-filesystem renames, no hard-link reliance).
- **D2 — Stage/verify/activate order.** `Stage`: write `.tmp` → fsync file →
  verify SHA-256 → rename to immutable path → fsync directory. `Activate`:
  copy immutable bytes to `.next-{publicationID}` → verify copy → rename old
  canonical to `.previous-{oldID}` → rename next to canonical → fsync
  directory. Every step returns explicit close/fsync errors; injected short
  writes leave no verified object.
- **D3 — Restore/abort/delete.** `Restore` reverses a failed activation from
  `.previous-*`; `Abort` removes staged-but-unactivated objects;
  `.unpublish-backup-*` restores the canonical when DB remains active, is
  removed when DB is unpublished and canonical absent. The store NEVER infers a
  new active Publication from files (Task 10 rule).
- **D4 — Recovery scanner.** `ScanOnce(ctx) → ScanReport` + `Run(ctx)` on
  startup and every `PUBLICATION_SCAN_INTERVAL_MINUTES` (default 10, spec §36).
  Rules: DB-active + missing/mismatched canonical ⇒ rebuild from immutable
  bytes; missing immutable ⇒ P0 alert, no guessed replacement; stale stage and
  `.next-*` older than `PUBLICATION_STAGE_TIMEOUT_MINUTES` (default 15)
  cleaned unless under a held publish lock; orphan quarantine ONLY after
  `system_migrations.publication_model_v1=completed`; `legacy_unverified`
  pages are never orphans.
- **D5 — Retention/GC.** Active + pinned: never GC'd. Superseded/unpublished:
  `PUBLICATION_RETENTION_DAYS` (default 90). Failed/staged: 7 days
  (`PUBLICATION_FAILED_RETENTION_DAYS`). Quarantine: 30 days. Storage deletion
  precedes `storage_state=deleted` marking.
- **D6 — Operations contract.** Production requires a persistent volume sized
  for immutable history (capacity reference §38: up to 1M pages, 100k
  publications/day × 90-day retention); `STATIC_STORAGE_PROVIDER` accepts only
  `filesystem` until the R2/S3 ADR lands; unsupported providers fail startup
  with actionable diagnostics (Task 17 negative test).

## 3. Rejected alternatives

- **A1 — R2/S3 now.** Rejected by program scope: object-store consistency,
  retry, permission, and public-pointer semantics need their own ADR + suite;
  MVP must ship on infrastructure the team already operates.
- **A2 — In-place canonical overwrite.** Rejected: readers observe torn files;
  crash mid-write destroys the only live copy; no `.previous-*` recovery
  source exists.
- **A3 — Content-addressed canonical path (no stable filename).** Rejected:
  breaks existing public URLs, Cloudflare caching, and the Task 14 migration
  promise that no legacy URL silently disappears.

## 4. Consequences / risks

- Disk growth is monotonic within retention windows; GC correctness is
  load-bearing — a GC bug deletes rollback sources. Mitigated by pin rules +
  Task 10 GC tests + P0 alert on missing immutable for an active Publication.
- Same-filesystem constraint limits deployment topologies (no split
  stage/serve mounts); recorded as a scale-out precondition (§40.2).
- Copy-based activation (not rename of the immutable object) doubles transient
  write I/O per publish; accepted for immutability guarantees.

## 5. Crash / failure / concurrency behavior

Crash fixtures required after EACH rename in Task 6 tests. Recovery matrix:
retained immutable + `.previous-*` ⇒ restore authoritative content; DB-active +
no canonical ⇒ rebuild; DB-unpublished + backup ⇒ cleanup; migration `running`
+ no canonical ⇒ report, do NOT quarantine. Concurrent activate vs GC: GC skips
any object under a held publish lock or referenced as active/pinned.

## 6. Migration & rollback

- Migrate: Task 14 copies existing canonical bytes into immutable legacy
  objects for mismatched pages (`legacy_unverified`), leaving canonicals
  serving throughout.
- Rollback: store code is additive beside existing `content/generated/`
  output until Task 16 cutover; rollback = revert Task 6/10/16E commits and
  resume legacy generation.

## 7. Verification (commands + expected evidence)

```bash
go test ./internal/product/storage -run 'TestStage|TestActivate|TestRestore' -count=1
# Red before Task 6 (package does not exist); green after, incl. race test.
go test -p 1 ./internal/product/publication -run 'TestRecovery|TestGC' -count=1
# Red before Task 10; green after (scanner + retention rules).
```

## 8. Approval record

_To be filled at G1 (store) and G2 (scanner/GC) reviews. Unapproved ADRs MUST
NOT be handed to parallel developers for independent interpretation (spec
§42)._
