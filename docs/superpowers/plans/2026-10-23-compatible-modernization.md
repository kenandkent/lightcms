# Compatible Modernization Implementation Plan

> **For agentic workers:** Use superpowers:executing-plans inline with test-driven-development. User explicitly requires main and previously requested no subagents. Do not deploy production content as part of testing.

**Goal:** Implement all seven review fixes and the approved staged modernization without replacing current public contracts.
**Architecture:** Modular Go monolith; transactional authoring, persistent workflow execution, versioned artifacts and explicit target maintenance commands. Legacy readers and endpoints remain adapters.
**Tech Stack:** Go 1.25, Mongo replica set, existing server-rendered admin UI, native Cloudflare Static Assets.
**Spec:** docs/superpowers/specs/2026-10-23-compatible-modernization-design.md

## Global Constraints

- Active root is /Users/ken/workspace/newsPage; outside paths read-only.
- Work on main. Do not modify umx-home.html, online templates/articles, Worker bindings or existing deployments.
- Preserve API/MCP/CLI and original Publication saga; approval/merge/edit never implicitly publish.
- Five-minute publication deadline remains measured from submission; uncertain activation retains reconciliation protection.
- Tests use isolated local Mongo databases containing test; temporary files stay inside this plan workspace.
- Current user approved implementation of the written design. No repeated implementation permission question between tasks.
- Commit/push/deployment are separate from proving local implementation. Do not report the entire project complete while any task below remains open.

## Review Focus

1. Failed database transaction must not leave content/history divergence or emit external side effects (Task 1).
2. Duplicate/concurrent votes, old workflows, and cancellation with a newer pending request (Task 2).
3. Partial import/merge and crash after a page commit but before progress persistence (Tasks 3/4/7).
4. New artifact reference racing GC, and old gob/manifest compatibility (Task 8).
5. Unknown Cloudflare activation or configuration drift during rollback/migration (Task 9).

### Task 1: Atomic authoring records (R01)

**Files:** internal/services/content.go; new authoring_atomic_test.go; extract atomic helpers only when useful.
**Interfaces:** preserve CreateContent/UpdateContent/BulkCreateContent signatures; consume DB.WithTransaction; do not nest transactions when an existing mongo.SessionContext is active.
- [x] RED: failed history insert leaves create absent, update unchanged, and bulk item failed; successful peer items remain committed.
- [x] GREEN: content CAS and complete ContentVersion commit together; notifications/indexing only after commit. Preserve bulk draft-only and per-item outcomes.
- [x] Verify: targeted authoring/bulk/content/version tests and full service suite PASS; retry-conflict and outer rollback tests PASS with race detector.

### Task 2: Approval transaction and immutable rules (R03/R05)

**Files:** internal/services/approval.go, internal/models/models.go, approval_atomic_test.go, approval handlers as needed.
**Interfaces:** preserve Approve/Reject/Cancel; add request schema/version snapshot and optional explicit conflict information without auto-publishing.
- [x] RED: same reviewer counts once, two reviewers complete correctly, cancelled request clears only its own pending association. Concurrent duplicate votes and approval-versus-cancellation race PASS.
- [x] GREEN: freeze rules/stored subject version for new requests; transactional decisions and pending flags; legacy workflow requests require explicit resubmission rather than mutable workflow lookup.
- [x] Verify approval service + HTTP tests; PASS including no-publication assertions and missing-request 404.

### Task 3: Import cancellation barrier (R02)

**Files:** internal/services/import.go, import_cancel_test.go.
**Interfaces:** retain CancelJob; add execution cancellation check consumed before each item and terminal status CAS. Persist cancel_requested.
- [x] RED: cancellation cannot become done; cancelled jobs cannot commit new content. Repeated cancellation is a no-op.
- [x] GREEN: per-item check, guarded finalization and visible partial counts; no rollback of completed pages.
- [x] Verify existing import suites and cancellation write-fence regression; PASS. Durable restart recovery remains Task 7.

### Task 4: Resumable fork merge (R04)

**Files:** internal/services/fork.go, fork merge model/result, fork_merge_resume_test.go.
**Interfaces:** preserve Merge result; add partially_merged and persisted item result keyed by fork/page/base version, consumed by Task 7.
- [x] RED: failed item is retryable and completed item is not re-applied. Concurrent merge checkpoint regression PASS with race detector.
- [x] GREEN: durable job lease/CAS and transactional per-item marker with draft write, keep fork-wins existing contract and no implicit publication. Frozen attribution and submission/execution authorization retained.
- [x] Verify fork/sandbox/version suites and concurrent merge regression; PASS.

### Task 5: Atomic advisory editing leases (R06)

**Files:** internal/services/lock.go and lock tests; optional token-aware API additions retain legacy owner-only adapter.
**Interfaces:** existing Acquire/Get/Refresh/Release remain; add lease generation/token for new consumers.
- [x] RED: competing acquisitions cannot both steal a fresh lease; expired/same-owner renewal works; stale token cannot release new lease.
- [x] GREEN: conditional DB operation with verified unique index and matched-count handling. Token-aware HTTP additions retain legacy owner-only callers.
- [x] Verify lock/handler tests and contender/stale-token race tests; PASS.

### Task 6: Complete native routing budget (R07)

**Files:** internal/product/staticdelivery/assets_first.go, native asset tests and diagnostic preview.
**Interfaces:** nativeAssetConfig still returns config/error; reject encoded redirect lines over1000 characters, retain <=2000 static rules and literal-path behavior.
- [x] RED: long encoded rule rejected; boundary allowed; real .html page still wins.
- [x] GREEN: early typed diagnostics before saving/uploading preview; no JS fallback.
- [x] Verify Go + Miniflare both modes; PASS, including actual generated native assets configuration.

### Task 7: Shared durable workflow execution

**Files:** new internal/product/jobs/{model,repository,runner}.go plus tests; adapt import, merge, upgrade, ordinary-event outbox; server wiring.
**Interfaces:** persisted Job/Item/Event, Claim with fencing generation, CheckCancelled, CommitItem transaction, bounded enqueue, paged queries; distinct executors preserve business states.
- [x] RED: crash/takeover, stale writer, cancelled claim, duplicate key, full queue and actor-revocation tests.
- [x] GREEN: import, fork merge, upgrade and ordinary Webhook adapters use persistent jobs; frozen inputs/attribution, bounded queue, delayed retries, cancellation and expiry finalizers. Unrecoverable legacy imports require explicit resubmission.
- [x] Verify adapted workflows against Mongo: CSV/Markdown crash after draft commit; RSS process recreation; upgrade process recreation/replay; fork frozen input/no implicit publication; Webhook restart/transaction rollback. No duplicate local version or Publication in these regressions.

### Task 8: Versioned object storage, admission and protected GC

**Files:** new artifact-store module, staticexport/staticdelivery adapters, dependency index, maintenance HTTP/admin views and tests.
**Interfaces:** immutable Object{hash,size,mime}, Open/Stat, manifest schema adapters, reference transaction/pin, cleanup preview/digest/confirm; old gob reader stays.
- [x] RED: dedup integrity, legacy read, injected ENOSPC/stream failure and destination-filesystem failure, aggregate admission, protected roots and reference-vs-delete races. No actual developer disk was filled.
- [x] GREEN: async preview/recovery, explicit preparation capacity, current template/snippet/asset and frozen Publication dependency inventory, manual GC/tombstones/pins with90/7-day defaults; no auto production cleanup.
- [x] Verify old rollback sources remain readable, failed deletion resumable, no broad deletion. 64 x4MiB concurrent requests with32MiB admission budget peaked at33554432 reserved bytes; see capacity evidence. This is not a whole-process RSS hard-limit claim.

### Task 9: Target lifecycle commands

**Files:** staticdelivery command/runner/model, HTTP/OpenAPI, admin target workflow, integration tests.
**Interfaces:** explicit preview-confirm commands for origin/environment migration, withdraw, page/site rollback and evidence-based drift recovery; optional queue_mode with per-path expectations.
- [x] RED: stale confirmation, unknown activation, foreign Worker, immutable rollback with unrelated pages and withdraw GET/HEAD/alias404 verification.
- [x] GREEN: explicit lifecycle commands produce immutable releases; configuration commits only after verification; evidence-only recovery; optional path-fenced queue; no DNS mutation; strict expected_revision remains default.
- [x] Verify Mongo command chain, queue owner release/rebase and local protocol/native assets in both modes. No new real remote upload is authorized or claimed by this task.

### Task 10: Admin, observability and restoration acceptance

**Files:** split admin module templates/scripts, task center/read models, health/readiness metrics, operations/API docs and tests.
**Interfaces:** one job detail contract for adapters; separate draft/Publication/Worker version summaries; no periodic all-page Worker polling.
- [x] RED: Node-rendered UI refresh/double-click/lost-response/async failure tests; submission/execution permission change; optional health degradation; corrupt backup/non-overwrite/isolated restore tests.
- [x] GREEN: bounded metadata task center and business links, verified sharing URLs, draft/Publication/Worker read model, dependency freshness, operations view, offline backup/isolated restore and read-only history-gap inventory.
- [x] Local compatibility matrix: full Go tests including Mongo, race regressions, build/vet, Linux build, Worker10/10, both native modes and nine-template structural/language/responsive contracts. Browser report visual review is explicitly waived by the user's instruction not to open reports; production/paid external integrations are not claimed.

## Progress

All ten implementation tasks above are locally implemented and accepted with the explicit validation limits recorded here. Final full-suite including coverage: `.superpowers/sdd/2026-10-23-compatible-modernization/runtime/release-gate.log` (exit0, session86932), with mandatory Mongo integrations. Earlier full acceptance/closure and focused race logs also exited0. Final metadata links/item pagination, pin/audit, recovery budget, index initialization, loop shutdown and partial-merge retry are included in that full suite. Build/vet and Linux amd64 build pass. See [operations guide](../../MODERNIZATION-OPERATIONS.md).

Coverage:80.40% statements using repository ignore rules (cmd, handlers, testutil, content_watcher excluded); core internal libraries80.48%. These local statement measurements are not a claim that remote Codecov's line/patch gate has run. No coverage threshold or ignore policy was changed.

Implementation rulings: offline backup uses the same exclusive writer gate, so no live GC can delete source objects during the recovery point; no default online backup or production GC. Dependency inventory is advisory, complete-generation based, refreshed periodically/on demand; missing frozen provenance remains unknown. Memory admission is a conservative work-reservation budget, not an RSS promise. Legacy readers and explicit original publication remain authoritative.

No report was opened after the user's prohibition. No production content, template, domain binding or Worker resource was changed by this modernization. `umx-home.html` remains unchanged. All work stays on main. The user subsequently authorized committing and pushing to main after successful unit tests/build; deployment success must still be confirmed separately from push success.
