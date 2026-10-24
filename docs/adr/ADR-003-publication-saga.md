# ADR-003: Publication Saga (state machine, transaction, outbox, crash recovery contract)

- **Status:** proposed
- **Date:** 2026-09-30
- **Task:** 0 (LightCMS V3 template static publishing program)
- **Spec:** `docs/LightCMS_V3_模板化页面生成与静态发布系统_最终设计方案.md`
  (§39.5 Failure Injection, §39.11 Publication Immutability & Rollback, §45.3)
- **Plan:** `docs/superpowers/plans/2026-09-30-lightcms-template-static-publishing.md`
  (Tasks 5/8/9/10, Shared interface contract)

| Role | Approver | Decision |
|---|---|---|
| Tech lead | _(slot)_ | ☐ approved / ☐ changes requested |
| Operations owner (co-sign, recovery/scanner/GC) | _(slot)_ | ☐ approved / ☐ changes requested |
| Security owner (co-sign, permission zero-side-effect) | _(slot)_ | ☐ approved / ☐ changes requested |

## 1. Scope / Context

V3 replaces "save == live" with a Publication lifecycle. New collections
(`content_publications`, outbox) and new indexes land via Task 2
(`DB.WithTransaction`, `EnsureProductIndexes`) and are exercised here. The saga
is the ONLY live-mutation path (ADR-001 D2); every entry in §39.9 (REST
single/batch, MCP publish, Admin, CLI, scheduler, import, copilot,
search-replace auto-republish, template upgrade, fork-merge-then-publish) must
funnel into it (Task 16C/16D).

## 2. Decision records

- **D1 — Publication record.** Lifecycle
  `staged → active → superseded | failed | unpublished`; storage state
  `pending | present | deleting | deleted | missing | corrupt`; verification
  `pending | verified | failed | legacy_unverified`. Exactly one active
  Publication per content, enforced by a partial unique index
  (`UNIQUE(content_id)` where `lifecycle=active`, Task 2).
- **D2 — CAS transitions.** `ActivateCAS(expectedOldActiveID)`,
  `UnpublishCAS`, `MarkFailed` are compare-and-swap: stale expected ID ⇒
  `PUBLICATION_CONFLICT`, neither record changes. A `failed / superseded /
  unpublished` record can NEVER become active; rollback creates a NEW
  Publication (from retained immutable bytes ≤ retention, else explicit
  weaker re-render path).
- **D3 — Publish saga order.** Lease content+path locks (stable lock order) →
  freeze versions + logical time + idempotency execution snapshot (ADR-005) →
  render in-memory (Task 7 snapshot) → stage + verify (ADR-004) → canonical
  cutover → ONE Mongo transaction (active pointer CAS + `Content.Published /
  PublishedAt` projection + outbox insert) → return URL. URL is returned only
  after commit success.
- **D4 — Compensation table (per failure point).**

  | Fails at | Compensation |
  |---|---|
  | render / stage / verify | mark Publication `failed`; no file, no DB pointer change, no outbox success event |
  | canonical rename | restore old canonical from `.previous-*` backup (ADR-004); old active stays active |
  | Mongo commit | transaction abort + file compensation above; new attempt is `failed` |
  | crash between cutover and commit (process dies) | Task 10 startup scanner reconciles: DB-active + immutable bytes ⇒ rebuild canonical; never mark a wrong hash active |
  | outbox/CDN delivery after commit | retry from durable outbox row (D6); delivery is at-least-once |

- **D5 — Unpublish.** Canonical rename to `.unpublish-backup-*`, then ONE
  Mongo transaction (lifecycle + Content projection + outbox). Transaction
  failure ⇒ restore backup. Retry after commit ⇒ 200 without a new event.
  Immutable object retained 90 days for exact rollback (§39.11).
- **D6 — Durable outbox.** Event identity `(event_type, publication_id)`;
  `InsertUnique` inside the activation transaction; a Mongo claim-lease lets one
  app replica deliver at a time; receiver 500 schedules retry with a stable
  event-ID header + existing HMAC (reuses `internal/services/webhook.go`
  signing via a new synchronous `DeliverRecordedEvent` entry point, Task 9).
- **D7 — Permission boundary.** `mode=publish` without publish scope ⇒ zero
  mutation: Content, Fork, Version, Publication, static object, and outbox
  counts all unchanged (§39.8). Verified per create-only/edit-only/publish-only
  key matrix in Task 17.
- **D8 — Rename-and-publish is one saga.** Old + new canonical paths locked in
  stable order; redirect metadata committed with the active Publication;
  cutovers compensated together.

## 3. Rejected alternatives

- **A1 — DB commit before file cutover ("DB first").** Rejected: crash window
  serves the OLD file while DB claims the NEW publication with no reconciling
  evidence direction; scanner cannot distinguish "not yet cut" from "cut and
  lost".
- **A2 — Best-effort webhook after commit (no outbox).** Rejected: commit +
  crash loses the event silently; downstream (CDN purge, partners) diverges
  with no redelivery source.
- **A3 — In-place lifecycle updates without CAS.** Rejected: concurrent
  publish/unpublish/rollback under retry produzir split-brain active pointers;
  the review-focus cases (§plan Review Focus 1–3) explicitly require 409 +
  single canonical file.

## 4. Consequences / risks

- Write amplification: each publish does file stage + verify + rename + Mongo
  txn; P95 publish target <3s (§38) must be measured in Task 17, not assumed.
- File/Mongo split-brain is inherent to a dual-write system; contained by D4 +
  Task 10 scanner, never eliminated — residual risk tracked to G2/G4.
- At-least-once webhook delivery obliges receivers to dedupe on the stable
  event ID (documented in Task 18).

## 5. Crash / failure / concurrency behavior

See D4 table. Fault-injection coverage required (§39.5): render, stage, verify,
rename, Mongo commit, outbox/CDN failures, crash after stage, crash after
activate-before-response, unpublish rename failure, unpublish txn failure +
compensation, unpublish crash after commit. Every scenario asserts: old live
served or scanner-restored, state consistent, operation retryable. Crash
recovery tests MUST restart the full LightCMS process (spec §39.5).

## 6. Migration & rollback

- Migrate: Task 14 creates verified or `legacy_unverified` active Publications
  for existing pages; `legacy_unverified` pages keep serving and are never
  scanner-quarantined (Task 10 gate on `system_migrations.publication_model_v1`).
- Rollback: lifecycle transitions are forward-only by design; operational
  rollback = Rollback command (new Publication) or Unpublish. Code rollback
  pre-G3 = revert Task 5/8/9/10 commits.

## 7. Verification (commands + expected evidence)

```bash
go test -p 1 ./internal/product/publication -run 'TestState|TestActivateCAS|TestUnpublishCAS' -count=1
go test -p 1 ./internal/product/publication -run 'TestPublishSaga|TestUnpublishSaga|TestRollback' -count=1
go test -p 1 ./internal/product/publication -run 'TestOutbox|TestWebhookRetry' -count=1
go test -p 1 ./internal/product/publication -run 'TestRecovery|TestGC' -count=1
# All red before Tasks 5/8/9/10 respectively; green after, with fault-injection
# and (Task 17) full-process restart evidence recorded in docs/implementation/test-report.md.
```

## 8. Approval record

_To be filled at G1 (state machine + repository) and G2 (saga + recovery)
reviews. Unapproved ADRs MUST NOT be handed to parallel developers for
independent interpretation (spec §42)._
