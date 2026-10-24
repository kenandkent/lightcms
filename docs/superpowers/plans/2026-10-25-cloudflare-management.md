# Cloudflare Management Implementation Plan

> **For agentic workers:** Use superpowers:executing-plans for the account/Worker changes; the two independent content pages follow superpowers:dispatching-parallel-agents. Steps use checkbox syntax for tracking.

**Goal:** Consolidate account management and make the three requested pages operate through lists, detail dialogs and row actions.

**Architecture:** Keep current routes, permissions and publication state machines. Add transactional local configuration CRUD; retain remote operations through existing services. Keep task center and operations pages unchanged.

**Tech Stack:** Go, MongoDB replica set, html/template, CSS variables, vanilla JavaScript, Node test fixtures.

**Spec:** ../specs/2026-10-25-cloudflare-management-design.md

## Global Constraints

- Work directly on main; no production content mutations or browser previews. User authorized commit/push after complete CI-equivalent validation and verification of Actions and production deployment.
- Outside-project paths are read-only; task runtime files stay under `.superpowers/sdd/2026-10-25-cloudflare-ui/runtime`.
- Worker and account deletion remove local configuration only. Initialized Worker identities stay fixed.
- Keep CSRF, RBAC, audit, immutable preview and publication verification contracts.

## Review Focus

- Cross-account Worker updates/deletes must be rejected without exposing credentials.
- Queued or uncertain activation/recovery must block configuration removal, including submission races.
- Stale modal submissions must not overwrite newer configuration or change published HTML metadata in place.
- Switching/closing dialogs while requests run must not display stale results or retain confirmation for another target.
- Large identifiers, empty lists, disabled/archived records and unavailable modules must render useful bounded views.

### Task 1: Local account/Worker configuration operations

Files: `internal/product/staticdelivery/management.go`, `management_test.go`, `targets.go`, and submission transaction boundaries.

- [x] Write failing tests for local cascading delete, busy target/job protection, account scoping, update version checks and initialized/published configuration restrictions.
- [x] Implement `AccountService.Delete`, `TargetService.Delete`, `TargetService.Update` and transaction fences; no remote delete calls.
- [x] Run targeted Mongo-backed tests and existing delivery tests.

### Task 2: Cloudflare management UI and menus

Files: `internal/handlers/admin_static_delivery*.go`, `admin_templates.go`, relevant rendering/handler tests.

- [x] Add failed assertions for account table, scoped detail dialogs, edit/delete actions and exact navigation ordering.
- [x] Replace long forms with account list/add dialog/Worker detail dialogs; preserve established operations and report errors inside selected account context.
- [x] Move only Worker maintenance/artifact links into content; rename settings link to Cloudflare 管理.
- [x] Validate escaping, CSRF, rendered JavaScript and scoped handler mutations.

### Task 3: Worker maintenance and artifact management

- [x] Rebuild the two independent pages using list/detail/action patterns with existing APIs and bounded metadata queries.
- [x] Keep preview/confirm/recovery and pin/cleanup protection; add regression tests for stale async results and actionable empty/error states.
- [x] Integrate without edits to task center or operations pages.

### Task 4: Final compatibility verification

- [x] Review complete changes and repair consequential findings with regression tests.
- [x] Run mandatory Mongo-backed `go test -p 1 ./... -count=1`, `go build ./...`, `go vet ./...` and relevant Node tests.
- [x] Record actual validation results and report deployment/commit status accurately.

### Additional requested task: Content-to-Worker publishing interaction

- [x] Keep selected published articles and show a compact target/preview/progress flow.
- [x] Automatically select unique available account/Worker; validate optional target deep links.
- [x] Preserve script review, final impact consent, frozen snapshots, queue policy, retries and refresh recovery; isolate stale asynchronous results.
- [x] Verify bilingual rendering and synthetic DOM interaction tests, then include in the final whole-project gate.

## Verification record

- Independent whole-change review found three consequential compatibility issues: existing scoped Token checks, legacy recovery links, and display-name edits invalidating release recovery. Each was reproduced and repaired with regression tests.
- Final Mongo-backed full suite: 42 packages passed, zero failures. One original homepage SEO test skips because the isolated database has no homepage fixture; database integration gates were mandatory.
- `go build ./...`, `go vet ./...`, and the Docker-equivalent Linux/CGO-disabled server build passed.
- Locked npm installation, ten Worker JavaScript tests and both legacy/assets-only native routing fixtures passed. UI tests use rendered templates and synthetic DOM, with no browser/content-page access.
- Task-center/operations source and the deployment workflow remain unchanged. Production rollout is verified after push through the matching GitHub Actions run, deployed image revision and service health, rather than inferred from local tests.
