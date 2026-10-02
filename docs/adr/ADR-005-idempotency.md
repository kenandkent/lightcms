# ADR-005: Idempotency (execution snapshot, lease, attempt)

- **Status:** proposed
- **Date:** 2026-09-30
- **Task:** 0 (LightCMS V3 template static publishing program)
- **Spec:** `docs/LightCMS_V3_模板化页面生成与静态发布系统_最终设计方案.md`
  (§39.6 Idempotency, §39.11 rollback replay rules)
- **Plan:** `docs/superpowers/plans/2026-09-30-lightcms-template-static-publishing.md`
  (Tasks 11/12/16C/16D, Global Constraints, Review Focus 3)

| Role | Approver | Decision |
|---|---|---|
| Tech lead | _(slot)_ | ☐ approved / ☐ changes requested |
| API owner (co-sign, header contract + status mapping) | _(slot)_ | ☐ approved / ☐ changes requested |
| Security owner (co-sign, collision/replay abuse) | _(slot)_ | ☐ approved / ☐ changes requested |

## 1. Scope / Context

All externally-triggered live-changing requests (single/batch publish,
rollback; unpublish is naturally idempotent) REQUIRE an `Idempotency-Key`
header; missing key ⇒ `428 IDEMPOTENCY_KEY_REQUIRED` with zero business side
effects (spec §39.6/plan 16C). Preview ignores the key and creates no record.
Internal jobs (scheduler, import, template upgrade) derive stable internal
operation keys with identical record semantics instead of HTTP headers
(plan 16D). Lane D (Task 11) owns the mechanism; Lane E (Task 12) owns the
HTTP contract; this ADR fixes the shared semantics both build on.

## 2. Decision records

- **D1 — Operation identity.** `(owner, method, path, key, canonicalBodyHash)`.
  Same key + same canonical body ⇒ replay; same key + changed body ⇒ `409`
  conflict. Canonical body hash is computed over a normalized JSON form so
  semantically identical retries match.
- **D2 — Replay cache policy.** Completed 2xx responses replay. Pure
  validation 400/422 MAY replay (cached 24h). 401/403/404/409/429/5xx are
  NEVER cached as completed (permissions, resources, locks, service state may
  change). `REQUEST_IN_PROGRESS` + retryable 503 carry `Retry-After`.
- **D3 — Attempt vs lease separation.** A business `attempt` is stable across
  worker crashes; the worker `lease_generation` is CAS-guarded with heartbeat
  (default `IDEMPOTENCY_LEASE_MINUTES=5`, heartbeat 60s; spec §36). Lease
  expiry ⇒ another worker takes over the SAME attempt (same Publication ID,
  same logical publish time). Takeover workers MUST NOT re-execute completed
  side effects; they resume from the durable execution snapshot.
- **D4 — Execution snapshot frozen before Render.** `FreezeExecution`
  persists operation ID, Publication ID, logical publish time, target
  content/version, template version, canonical path BEFORE rendering
  (plan Shared interface contract: `Begin`, `BindContentAndVersion`,
  `FreezeExecution`, `RenewLease`, `TakeOver`, `Complete`). Crash before
  Render recovers the durable Publication ID + logical time — never allocates
  a second one for the same attempt.
  Mutable render dependencies are frozen as a complete JSON render snapshot
  (content/template inputs, policy, snippets, wikilink indices, query expansions)
  by CAS before rendering. Maximum serialized snapshot: 8 MiB; oversize is
  `RENDER_VALIDATION_FAILED`, never an implicit live-dependency fallback.
  Dependency hashes include actual dependency contents. Terminal retry clears
  this snapshot; uncertain takeover preserves it. Command kind and response
  metadata (including warnings) persist before business effects.
- **D5 — Terminal retry allocates anew.** A terminal pre-activation failure
  marks the attempt terminal; a retry with the same key increments `attempt`
  and allocates a NEW Publication ID + logical time. Lost-response retry after
  success reuses the completed response (Review Focus 3).
- **D6 — Content binding is transactional.** `BindContentAndVersion` commits
  atomically with Content/Version creation or replacement: no committed
  Content without a recoverable operation ID, no duplicate Content Version on
  retry.
- **D7 — Outbox dedupe.** Rollback and legacy single-publish replays create
  no second Publication and no second publish outbox row for the same
  Publication ID (identity `(event_type, publication_id)`, ADR-003 D6).
- **D8 — TTL.** Idempotency records TTL-default `IDEMPOTENCY_TTL_HOURS=24`
  (spec §36); expiry after completion degrades to "unknown key" (safe to
  re-execute as a new operation), never to phantom replay.
- **D9 — Ownership fences.** The original lease generation is held throughout
  planning/render/cutover, with heartbeat and an execution deadline of half
  the configured lease. Final filesystem renames recheck ownership; activation
  writes the owned operation inside the same Mongo transaction as Publication
  and Outbox. Never adopt a newer worker's generation from a fresh read.
- **D10 — Credential namespace.** API keys use `apikey:<database ID>`; OAuth
  uses `oauth:<client_id>:<resolved subject user_id>`. Legacy OAuth tokens do
  not store a subject, so it is the existing configured system user; client
  isolation still survives MCP loopback requests. Rotating into a new API-key
  record starts a new namespace: retry an outstanding operation with its
  original credential, not a newly issued key. User/sandbox IDs remain separate.

## 3. Rejected alternatives

- **A1 — Client-generated Publication ID as the idempotency key.** Rejected:
  leaks server-side allocation + logical-time authority to clients; collides
  with D4/D5 attempt semantics.
- **A2 — Cache all error responses for replay.** Rejected: a 403 replayed
  after a permission grant (or a 503 replayed after recovery) returns stale
  failures; only pure validation (deterministic on the request bytes) is safe.
- **A3 — Single global lock per key with no lease/takeover.** Rejected: a
  crashed worker holds the key until TTL; long publishes stall for 24h. Lease
  + takeover bounds the stall to minutes.

## 4. Consequences / risks

- Clients MUST send stable keys per logical operation and MUST treat
  same-key-changed-body 409 as "you changed your mind — use a new key".
  Documented with curl examples in Task 18.
- Clock skew across app replicas affects lease timing margins only, never
  correctness (CAS generation, not wall-clock comparison, decides takeover).
- TTL expiry + very-late retry ⇒ duplicate execution is possible by design;
  bounded by documenting TTL as the retry horizon.

## 5. Crash / failure / concurrency behavior

Parallel same-key requests: exactly one executes, others wait-or-replay.
Crash takeover reuses current attempt's Publication ID + logical time.
Terminal pre-activation failure + same-key retry ⇒ new attempt + new
Publication ID. `mode=preview` ignores the key entirely (no record, no lock).
Concurrent same-key + changed payload ⇒ 409 without side effects.

## 6. Migration & rollback

- Migrate: no legacy idempotency state exists; records begin accumulating on
  Task 12 route cutover (16C). Internal-job stable keys assigned in 16D.
- Rollback: records are advisory (replay cache + snapshot); deleting them only
  loses replay (retries become fresh executions). Safe to drop pre-G3.

## 7. Verification (commands + expected evidence)

```bash
go test -p 1 ./internal/product/idempotency -run 'TestReplay|TestLease|TestExecutionSnapshot' -count=1
# Red before Task 11 (package does not exist); green after: replay matrix,
# lease-expiry/takeover with stable attempt, crash-before-Render recovery,
# terminal-retry new Publication ID, BindContentAndVersion atomicity.
```

Task 17 idempotency table (lost response, parallel key, changed payload,
terminal retry, crash takeover, rollback retry, no duplicate outbox) is the
system-level proof, recorded in `docs/implementation/test-report.md`.

## 8. Approval record

_To be filled at G1 review. Unapproved ADRs MUST NOT be handed to parallel
developers for independent interpretation (spec §42)._
