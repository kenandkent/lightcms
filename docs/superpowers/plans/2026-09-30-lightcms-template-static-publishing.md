# LightCMS Template and Static Publishing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship one LightCMS application that creates template driven pages, keeps published pages stable while drafts change, and publishes verified static HTML through a recoverable publication protocol.

**Architecture:** Extend the existing Go server with focused in-process packages under `internal/product/`. All Admin UI, REST, MCP backing logic, scheduled work, publication, outbox, and recovery run in the same LightCMS application binary. MongoDB is a replica set; the MVP static store is a persistent filesystem with immutable publication objects and an atomic canonical file rename.

**Tech Stack:** Go, existing Gorilla Mux/http handlers, MongoDB Go driver v1, Go `html/template`, existing LightCMS Admin HTML, filesystem, existing REST/MCP/API key/OAuth/RBAC.

**Spec:** `docs/LightCMS_V3_模板化页面生成与静态发布系统_最终设计方案.md` at SHA-256 `15db7eab7e7818906f56d156401b101c416a1b2caf48c0687d299da2e0659e48`.

## Global Constraints

- Baseline LightCMS commit: `c1165be1327dc605bd196cd4dff4ebe35bfaa6f4`, version `7.2.2`. Record a new baseline and reconcile this plan before rebasing.
- Project root for delegated work: `/Users/ken/workspace/newsPage`. Files outside a delegated task's project root are read-only unless the user explicitly authorizes exact paths.
- One backend application binary and one Admin UI. `*Service` types are in-process Go types, not separately deployed services.
- Product Page remains LightCMS `Content`; published drafts use existing Forks. Do not add `pages`, `page_versions`, new auth, new MCP backend, or new webhook engine.
- MongoDB production and test environments must support transactions through replica set mode. Tests that require Mongo must fail the release gate when no test Mongo URI is configured; the existing skip behavior is not a successful integration run.
- MVP static provider is filesystem. R2/S3 remains a separately approved, post-MVP adapter.
- `Content.Published/PublishedAt` are compatibility projections; the unique active Publication is control-plane truth; the canonical file is actual served content.
- `Content.UpdateContent` must stop implicitly writing or deleting live files. Treat this as a documented external API behavior change.
- External live-changing requests require `Idempotency-Key`; only pure 400/422 validation and successful responses are replay cached. Preview ignores the key.
- Publication lifecycle and storage lifecycle are distinct; rollback creates a new Publication.
- All product code and documentation are repository-relative under `/Users/ken/workspace/newsPage`; documentation stays in `docs/` of this same Git repository.
- `go test ./...`, `go vet ./...`, and server/MCP/CLI builds are final gates. Run DB tests with `-p 1` and a database name containing `test`.

## Review Focus

1. Two simultaneous first-page creates using `/News/Foo` and `/news/foo` yield one live path, a stable 409 for the loser, and no second canonical file. Covered by Tasks 2 and 17.
2. A process crash between canonical rename and Mongo commit restores or reconciles the correct file and never silently marks a wrong hash active. Covered by Tasks 8 and 10.
3. An idempotent retry after a lost response reuses the completed response, while a retry after a terminal staging failure uses a new Publication ID. Covered by Tasks 11 and 17.
4. A publish request with only `content.publish` cannot create or edit data; authorization failure leaves all content, versions, publications, files, and outbox records untouched. Covered by Tasks 12 and 17.
5. A legacy page whose existing HTML differs from current re-rendering keeps serving throughout migration and is not quarantined by the recovery scanner. Covered by Tasks 14 and 17.

---

## 1. Execution Rules and Parallel Ownership

The plan is a single master plan with independently reviewable tasks. Each worker gets one task and the exact spec sections named by that task. The execution controller passes the project file boundary rule above to every worker. Separate workers use isolated checkouts rooted inside the active project root or the repository's managed worktree mechanism. Changes are integrated only through reviewed commits; workers do not edit each other's checkout.

第三方执行时，每个子 Agent 只领取一个 Task ID。任务消息可直接使用下面的模板，替换方括号内的具体内容；`文件所有权` 必须与上表一致：

```text
你负责 Task [编号和标题]。项目根目录是 /Users/ken/workspace/newsPage。
必须先阅读：
1. docs/LightCMS_V3_模板化页面生成与静态发布系统_最终设计方案.md 的相关章节；
2. docs/superpowers/plans/2026-09-30-lightcms-template-static-publishing.md 的 Global Constraints、Shared interface contract 和你的 Task 全文；
3. 本仓库中 Task 列出的现有代码。

最高优先级文件边界：项目根目录内的文件可按本任务需要检查和修改；根目录外只读，未经用户对具体路径的直接授权不得创建、修改、移动或删除。将同样规则传给你委派的任何子 Agent。

只修改本任务拥有的文件。共享文件交由 Integration owner。先写失败测试并记录失败，再实现、运行聚焦测试、提交。不得改变 Shared interface contract；若发现冲突，先报告具体接口和证据，不自行修改其他任务的文件。

交付时报告：Task ID、baseline SHA、commit SHA、文件清单、失败测试输出摘要、通过测试输出摘要、公共接口变更和剩余风险。
```

实际派发顺序：先完成 G0；随后同时派发 Task 2、6、13；Task 2 合并后同时派发 Task 3、5、11；后续按下表依赖推进。每个 Gate 的集成负责人先审查接口和测试，再合并。任何并行工作不得直接提交到同一个共享 checkout 的同一文件。

| Lane | Task IDs | Exclusive file ownership while parallel |
|---|---|---|
| A: template contract | 3, 4 | `internal/product/templatecontract/*` |
| B: static store | 6 | `internal/product/storage/*` |
| C: publication model | 5 | `internal/product/publication/model.go`, `repository.go`, `state.go` |
| D: idempotency | 11 | `internal/product/idempotency/*` |
| E: API contract | 12 | `internal/product/generation/*`, `internal/product/httpapi/*` |
| F: security and URL | 13 | `internal/product/publicurl/*`, `internal/handlers/api_assets.go` |
| G: migration | 14 | `internal/product/migration/*` |
| H: Admin UX | 15 | `internal/handlers/admin_templates.go` and new focused Admin handler file |
| Integration owner | 0–2, 7–10, 16–19 | `internal/models/models.go`, `internal/database/mongo.go`, `internal/services/*` legacy entry points, `cmd/server/main.go`, shared wiring |

Do not execute tasks that modify a shared file concurrently. After Task 1, Tasks 2, 6, and 13 may run in parallel. After Task 2 is integrated, Tasks 3, 5, and 11 may run in parallel with Tasks 6 and 13 still underway. Task 4 depends on Task 3. Task 9 depends on Task 5. Task 7 depends on Tasks 3–6. Task 8 depends on Tasks 5–7, 9, and 11. Task 10 depends on Tasks 5, 6, and 8. Task 12 depends on Tasks 3–5, 8, 11, and 13. Task 14 depends on Tasks 2–6 and 10. Task 15 can develop UI prototypes against the Task 12 contract but cannot merge until Task 12 is integrated. Task 16 starts after Tasks 3–15. Task 17 is the whole-system gate after all MVP tasks. Task 18 is external documentation and Task 19 is release rehearsal.

### Integration checkpoints

| Gate | Prerequisites | Required evidence |
|---|---|---|
| G0 baseline | Tasks 0–1 | pinned SHA, dirty-tree inventory, test/build output, replica-set fixture and route/tool inventory |
| G1 contracts | Tasks 2–6, 11, 13 and ADR-001–005 | models compile, indexes verified, approved ADRs and package unit tests pass |
| G2 publication | Tasks 7–10 | failure injection and recovery tests pass |
| G3 entry points | Tasks 12, 14–16 | every write/publish entry follows one policy/service |
| G4 release | Tasks 17–19 | migration rehearsal, E2E, security, rollback, docs |

### File map

| Path | Responsibility |
|---|---|
| `internal/models/models.go` | additive Content/Template/Field properties and compatibility JSON |
| `internal/database/mongo.go` | indexes and transaction/replica set verification |
| `internal/product/templatecontract/` | slug, versions, field validation, schema conversion |
| `internal/product/publication/` | state machine, repository, renderer input, Saga, scanner, outbox |
| `internal/product/storage/` | immutable stage, verify, canonical cutover, restore, GC |
| `internal/product/idempotency/` | execution snapshot, lease, response replay |
| `internal/product/generation/` | one-command orchestration and mode matrix |
| `internal/product/httpapi/` | product handlers and stable response/error mapping |
| `internal/product/publicurl/` | canonical HTTPS public URL generation |
| `internal/product/migration/` | existing data reconciliation and migration gate |
| `internal/services/content.go` | draft-only data/version mutation; legacy publish delegates |
| `internal/services/template.go` | template updates no longer regenerate live files |
| `internal/services/fork.go` | merge into draft without live file writes |
| `cmd/server/main.go` | all routes and in-process components wired once |
| `internal/testutil/testutil.go` | cleanup new collections and replica set test guard |
| `docs/openapi/page-generation-v1.yaml` | published HTTP contract |

### Shared interface contract

Workers use the following signatures as their handoff contract. They may add private helpers but must not rename or change these exported methods without updating this plan and the consuming task before integration. All packages are under `internal/product/`.

```go
// pathkey
func Canonical(fullPath string) (string, error)

// internal/models (this declaration belongs in models, not templatecontract)
type FieldValidation struct {
    MinLength, MaxLength *int
    Pattern string
    Min, Max *float64
    AllowedProtocols []string
    MaxItems *int
}

// templatecontract (imports models; models never imports templatecontract)
type FieldError struct { Code, Field, Message string }
type FieldWarning struct { Code, Field, Message string }
type TemplateInput struct {
    Slug, Name, Category, Status, HTMLLayout, ScriptPolicy string
    Fields []models.TemplateField
}
func ValidateData(v TemplateVersion, data map[string]any) ([]FieldError, []FieldWarning)
func ToJSONSchema(v TemplateVersion) ([]byte, error)

// publication
type PublishRequest struct {
    ContentID primitive.ObjectID
    ContentVersion int64
    TemplateVersionID primitive.ObjectID
    ExpectedActiveID *primitive.ObjectID
    Reason string
    IdempotencyRecord *primitive.ObjectID
}
type PublicationResult struct {
    PublicationID primitive.ObjectID
    ContentID primitive.ObjectID
    ContentVersion int64
    TemplateVersionID primitive.ObjectID
    FullPath, PublicURL, ContentHash string
    LogicalPublishedAt time.Time
}
type UnpublishRequest struct {
    ContentID primitive.ObjectID
    ExpectedActiveID *primitive.ObjectID
}
type RollbackRequest struct {
    ContentID, SourcePublicationID primitive.ObjectID
    ExpectedActiveID *primitive.ObjectID
    IdempotencyRecord *primitive.ObjectID
}

// storage
type StageRequest struct {
    ContentID, PublicationID primitive.ObjectID
    CanonicalPath string
    HTML []byte
    ExpectedSHA256 string
}
type StagedObject struct { ContentID, PublicationID primitive.ObjectID; Path, SHA256 string }
type StoredObject struct { ContentID, PublicationID primitive.ObjectID; Path, SHA256 string }

// generation
type GenerateRequest struct {
    Template, Title, Slug, FolderPath, Mode string
    ExpectedTemplateVersion *int64
    Data map[string]any
    Upsert bool
}
type GenerateResponse struct {
    ID, Action, Template, FullPath, Mode string
    Published, RequiresPublish bool
    PublicURL *string
    ContentVersion, TemplateVersion int64
    PublicationID *string
    Warnings []templatecontract.FieldWarning
}
```

The Mongo wrapper added in Task 2 must expose `WithTransaction(ctx context.Context, fn func(mongo.SessionContext) error) error`. Repository methods called inside a transaction accept `mongo.SessionContext` as `context.Context`; they must not open nested transactions. `models.ContentVersion.Version` is currently `int`; migration changes it consistently to `int64` before publication interfaces compile.

## 2. Task Template for Workers

Each task is delivered as one or more small commits. Within each task, write a failing behavior test, run it and record the failure, make the smallest implementation change, run its focused tests, then commit. Do not leave an unverified green claim. Commit messages use `feat:`, `fix:`, `test:`, or `docs:` and name the task's deliverable. Before handoff, report touched files, test commands and actual results, unresolved risks, and the commit SHA. The integration owner checks both interface signatures and filesystem ownership before cherry-picking.

### Task 0: Reproducible Test Environment and ADR Gate

**Owner:** Integration owner and architecture reviewer. **Depends on:** none. **Deliverable:** reproducible MongoDB replica-set integration environment and five approved ADRs. This task may run alongside Task 1, but G0/G1 cannot pass without its evidence.

**Files:** Create `docs/adr/ADR-001-draft-mutation.md` through `ADR-005-idempotency.md` with the exact names and required contents in design §42; create `docker-compose.test.yml`, `.env.test.example`, and `docs/implementation/test-environment.md`; update the repository CI workflow only after inspecting the pre-existing working-tree state and obtaining a reviewed integration commit. Do not silently restore or overwrite a user-deleted workflow.

- [ ] Write each ADR with context, decision, rejected alternatives, invariants, crash/failure behavior, rollback and test evidence; get architecture, security and operations signoff before G1.
- [ ] Define a single-node MongoDB replica set fixture, deterministic initialization and readiness check, a `test`-named isolated database, and teardown that cannot target production; document exact local and CI commands.
- [ ] Add an integration test gate that fails when the replica-set URI is absent or required DB tests skip; keep a separate fast unit-test command where skip is allowed. Record actual Mongo version and test counts.
- [ ] Inventory dirty/untracked files and CI workflow deletions before editing; preserve unrelated user changes. Record CI source and release evidence chain in `docs/implementation/test-environment.md`.

### Task 1: Pin and Audit the Baseline

**Owner:** Integration owner. **Depends on:** none. **Deliverable:** evidence-backed baseline and route/tool matrix.

**Files:** Create `docs/implementation/baseline.md`, `docs/implementation/entry-point-matrix.md`; inspect `cmd/server/main.go`, `internal/mcp/server.go`, `internal/services/content.go`, `template.go`, `fork.go`, `webhook.go`, `import.go`, `scheduler.go`.

**Interfaces:** Produces an entry-point list with columns `entry`, `handler`, `service`, `permission`, `static side effect`, `target task`. Task 16 consumes it.

```bash
git rev-parse HEAD
go test ./... && go vet ./... && go build ./cmd/server ./cmd/mcp ./cmd/cli
```

- [ ] Record `git rev-parse HEAD` and `git status --short --branch`; abort implementation if HEAD differs from the baseline SHA without a revised matrix.
- [ ] Run `go test ./...`, `go vet ./...`, and `go build ./cmd/server ./cmd/mcp ./cmd/cli`; record exit codes and whether Mongo tests skipped.
- [ ] Enumerate `/api/v1`, Admin, MCP, CLI, scheduler, import, copilot, search replace, template regeneration, fork merge, approval, and watcher routes/callers using `rg 'PublishContent\(|GenerateStaticPage\(|UpdateContent\(' internal cmd --glob '*.go'`.
- [ ] Create a table row for every direct live file or publish mutation. Mark existing `GenerateStaticPage` paths in `content.go`, `template.go`, `fork.go`, `regen_queue.go`, `content_watcher.go`, and the API handlers as migration targets.
- [ ] Run MCP `tools/list` in a configured test environment and save the returned names/schemas; if no configured Mongo exists, record the missing runtime prerequisite and require it before G3.
- [ ] Commit only the two audit documents: `docs: pin LightCMS baseline and entry points`.

### Task 2: Mongo Transactions, Canonical Paths, and Shared Model Fields

**Owner:** Integration owner. **Depends on:** Task 1. **Deliverable:** collision-safe path and version primitives.

**Files:** Modify `internal/models/models.go`, `internal/database/mongo.go`, `internal/testutil/testutil.go`; create `internal/product/pathkey/pathkey.go`, `pathkey_test.go`, `internal/database/product_indexes_test.go`.

**Interfaces:** Produce `pathkey.Canonical(fullPath string) (string, error)` and Content fields `CanonicalFullPath`, `PathScope`, `PathActive`, `CurrentVersion`, `HasUnpublishedChanges`; Template fields `CurrentVersion`, `Status`; TemplateField `Description`, `Example`, `Validation`.

```bash
go test -p 1 ./internal/product/pathkey ./internal/database -run 'TestCanonical|TestProductCanonicalPathIndex' -count=1
```

- [ ] Write `pathkey_test.go`: `/News/Foo` and `/news/foo` yield the same key; traversal, empty segment, query, fragment, and encoded slash are rejected; valid authored `FullPath` casing stays unchanged.
- [ ] Run `go test ./internal/product/pathkey -run TestCanonical -count=1`; expect failure because the package does not exist.
- [ ] Implement `Canonical` using path normalization and Unicode NFC/case folding as specified; return typed invalid-path errors. Keep authored `FullPath` separate.
- [ ] Add the fields above to existing models with `bson`/`json` tags, preserving backward reads for records missing them. Change `models.ContentVersion.Version` from `int` to `int64` and update its handler/API conversions and tests in the same commit.
- [ ] Add `DB.WithTransaction(ctx, fn)` that starts a Mongo session, executes `session.WithTransaction`, and returns the callback error. Test callback success and forced failure against the test replica set.
- [ ] Add `DB.EnsureProductIndexes(ctx)` as an explicit post-migration method for `UNIQUE(canonical_full_path, path_scope)` with `partialFilterExpression: {path_active: true}`, `UNIQUE(content_id, version)`, unique Template slug, unique Template Version, unique active Publication, Idempotency, and Outbox. Do not call it from `Connect` before Task 14 has cleaned legacy collisions. Do not drop the old `(full_path, fork_id)` index in this task.
- [ ] Write a Mongo integration test that calls `EnsureProductIndexes` on a clean test DB, concurrently inserts two live paths with casing differences and asserts one duplicate-key error; test fork scope can share the live key. Run `go test -p 1 ./internal/database -run TestProductCanonicalPathIndex -count=1` with replica set test URI.
- [ ] Add new collection names to `CleanupCollections` as their models land; never run cleanup against a database without `test` in its name.
- [ ] Commit `feat: add canonical path and product model primitives`.

### Task 3: Template Slug, Version, and Field Contract

**Owner:** Lane A. **Depends on:** Tasks 1–2. **Deliverable:** immutable template contract with atomic version allocation.

**Files:** Create `internal/product/templatecontract/model.go`, `repository.go`, `service.go`, `slug.go`, `service_test.go`; integration owner later edits `internal/services/template.go` and `internal/database/mongo.go` in Task 16.

**Interfaces:** Produce `type TemplateVersion struct {...}` matching design §9.6; `Create(ctx, TemplateInput) (models.Template, TemplateVersion, error)`, `Update(ctx, id, expectedVersion, TemplateInput) (TemplateVersion, error)`, `GetCurrent(ctx, slug) (TemplateVersion, error)`, `GetVersion(ctx, id) (TemplateVersion, error)`.

```bash
go test -p 1 ./internal/product/templatecontract -run 'TestSlug|TestVersion' -count=1
```

- [ ] Test slug validation (`financial-news` accepted, uppercase/space/empty/65 characters rejected), duplicate slug rejection, status transitions, and immutable slug on ordinary update.
- [ ] Test same ContractHash produces no new version and changes to field description, default, options, validation, HTML, script policy, or status produce a new version; RenderHash may remain unchanged for description-only changes.
- [ ] Test two concurrent updates from the same expected version: one commits version N+1, the other returns `TEMPLATE_VERSION_CONFLICT`, with no duplicate version document.
- [ ] Run `go test -p 1 ./internal/product/templatecontract -run 'TestSlug|TestVersion' -count=1`; expect failures before implementation.
- [ ] Implement canonical JSON hashing, `UNIQUE(template_id, version)` repository operations, and a Mongo transaction that CAS increments `templates.current_version` and inserts the immutable version.
- [ ] Run focused tests and `go test ./internal/product/templatecontract`; commit `feat: version template contracts`.

### Task 4: Field Validation, JSON Schema, and New Field Types

**Owner:** Lane A. **Depends on:** Task 3. **Deliverable:** one validator shared by Admin and API and deterministic Schema output.

**Files:** Create `internal/product/templatecontract/validate.go`, `schema.go`, `validate_test.go`, `schema_test.go`; integration owner updates existing Admin form parsing in Task 15.

**Interfaces:** Produce `ValidateData(v TemplateVersion, data map[string]any) (errors []FieldError, warnings []FieldWarning)` and `ToJSONSchema(v TemplateVersion) ([]byte, error)`.

```bash
go test ./internal/product/templatecontract -run 'TestValidateData|TestJSONSchema' -count=1
```

- [ ] Write table tests for `text`, `textarea`, `richtext`, `markdown`, `rawhtml`, `date`, `image`, `select`, `url`, `number`, `boolean`; test unknown fields, required, defaults, min/max, pattern, protocol, enum, asset reference, and missing vs explicit `false`.
- [ ] Include `date=2026-02-30` as a failing calendar-date case; optional boolean missing must remain missing, while `default=true` materializes `true`.
- [ ] Test JSON Schema for the same immutable version is byte-for-byte stable and carries `additionalProperties:false`, `format:date`, enum, defaults, descriptions, and examples.
- [ ] Run `go test ./internal/product/templatecontract -run 'TestValidateData|TestJSONSchema' -count=1`; expect red tests.
- [ ] Implement the validator and schema adapter from the immutable `TemplateVersion`, not from the mutable Template record; reuse existing sanitization and rendering policy at publish time.
- [ ] Run focused and package tests; commit `feat: validate template fields and emit JSON Schema`.

### Task 5: Publication Records and State Machine

**Owner:** Lane C. **Depends on:** Task 2. **Deliverable:** repository and state rules without file effects.

**Files:** Create `internal/product/publication/model.go`, `state.go`, `repository.go`, `state_test.go`, `repository_test.go`; Task 2 owns index definitions and Task 14 invokes index creation after remediation.

**Interfaces:** Produce `Publication` with lifecycle `staged|active|superseded|failed|unpublished`, storage state `pending|present|deleting|deleted|missing|corrupt`, verification `pending|verified|failed|legacy_unverified`; `Repository.GetActive`, `InsertStaged`, `ActivateCAS`, `UnpublishCAS`, `MarkFailed`, `ListHistory`.

```bash
go test -p 1 ./internal/product/publication -run 'TestState|TestActivateCAS|TestUnpublishCAS' -count=1
```

- [ ] Test all allowed and disallowed lifecycle transitions. A failed/superseded/unpublished record may not become active; rollback creates a new record.
- [ ] Test `ActivateCAS` on the expected old active ID; a stale ID yields `PUBLICATION_CONFLICT` without changing either publication.
- [ ] Test `UnpublishCAS` synchronizes `Content.Published=false`, `PublishedAt=nil`, and the `content.unpublish` outbox insert within one Mongo transaction.
- [ ] Run `go test -p 1 ./internal/product/publication -run 'TestState|TestActivateCAS|TestUnpublishCAS' -count=1`; expect failure until repository is implemented.
- [ ] Implement repository transaction operations and typed duplicate-key mapping against Task 2's partial unique active index. Outbox repository interface is injected and wired in Task 9.
- [ ] Run focused tests; commit `feat: model publication lifecycle and CAS`.

### Task 6: Filesystem Immutable Store

**Owner:** Lane B. **Depends on:** Task 1. **Deliverable:** staged immutable bytes and atomic canonical projection.

**Files:** Create `internal/product/storage/store.go`, `filesystem.go`, `filesystem_test.go`.

**Interfaces:** Produce `Stage(ctx, StageRequest) (StagedObject,error)`, `Verify`, `Activate`, `Restore`, `Open`, `Abort`, `Delete`, `Exists`; returned object exposes immutable path and SHA-256.

```bash
go test ./internal/product/storage -run 'TestStage|TestActivate|TestRestore' -count=1
```

- [ ] Test `Stage` writes `.tmp`, fsyncs, verifies SHA-256, and renames to `content/publications/{content}/{publication}/index.html`; injected short write leaves no verified object.
- [ ] Test `Activate` copies immutable bytes to same-directory `.next-{publicationID}`, verifies the copy, atomically renames old canonical to `.previous-{oldID}` and next to canonical.
- [ ] Test crash fixtures after each rename. A retained immutable object and previous backup remain available for Task 8 recovery.
- [ ] Implement `.unpublish-backup-{publicationID}` staging, restoration and post-commit cleanup with directory fsync; expose enough metadata for scanner reconciliation.
- [ ] Run `go test ./internal/product/storage -run 'TestStage|TestActivate|TestRestore' -count=1`; expect red first.
- [ ] Implement with explicit path validation, close/fsync errors, directory fsync, and no reliance on hard links or cross-filesystem rename.
- [ ] Run package tests including race test; commit `feat: stage and atomically switch static files`.

### Task 7: Render Snapshot and Publication Planning

**Owner:** Integration owner. **Depends on:** Tasks 3–6. **Deliverable:** deterministic render input before file writes.

**Files:** Create `internal/product/publication/render.go`, `render_test.go`; modify only the minimal rendering entry in `internal/services/content.go` to expose an in-memory render result without writing a canonical file.

**Interfaces:** Produce `RenderSnapshot{PublicationID, ContentVersion, TemplateVersionID, LogicalPublishedAt, PublicURL, DependencySnapshot}` and `Render(ctx, RenderSnapshot) (html []byte, hash string, error)`.

```bash
go test ./internal/product/publication -run TestRenderSnapshot -count=1
```

- [ ] Test `{{.publication_id}}`, `{{.published_at}}`, `{{.public_url}}`, and `{{.template_version}}` render from the frozen snapshot; re-rendering the same snapshot yields identical bytes/hash.
- [ ] Test missing required field and unsafe content fail before `InsertStaged` or file write.
- [ ] Run focused test red, then extract existing Markdown/Snippet/Theme/Wikilink/TOC processing into an in-memory renderer path that accepts immutable TemplateVersion data.
- [ ] Record renderer version, product build SHA, and dependency hashes/snapshot in the result; do not read a newer mutable template during rendering.
- [ ] Run `go test ./internal/product/publication -run TestRenderSnapshot -count=1` and existing content markup tests; commit `feat: render deterministic publication snapshots`.

### Task 8: Publish and Unpublish Saga

**Owner:** Integration owner. **Depends on:** Tasks 5–7 and 11. **Deliverable:** only live mutation service.

**Files:** Create `internal/product/publication/service.go`, `service_test.go`; modify `internal/services/content.go` to delegate legacy publish/unpublish; keep `cmd/server/main.go` wiring for Task 16.

**Interfaces:** Produce `Publish(ctx, PublishRequest) (PublicationResult,error)`, `Unpublish(ctx, UnpublishRequest) error`, `Rollback(ctx, RollbackRequest) (PublicationResult,error)`.

```bash
go test -p 1 ./internal/product/publication -run 'TestPublishSaga|TestUnpublishSaga|TestRollback' -count=1
```

- [ ] Test publish holds content/path leases, freezes versions and logical time, persists idempotency execution snapshot, renders, stages, verifies, cuts canonical file, commits Mongo active/Content projection/outbox, and returns URL only after success.
- [ ] Inject failures at render, stage, verify, rename, and Mongo commit. Verify old canonical remains or is restored, old active stays active, new attempt is failed, and no success event appears.
- [ ] Add a fault fixture that stops after canonical rename before Mongo commit and preserves the on-disk files for Task 10. In this task, verify immediate compensation on a returned transaction error; Task 10 and Task 17 test process restart repair.
- [ ] Test Unpublish: canonical rename to backup, one Mongo transaction for lifecycle/Content/outbox, restore backup on transaction failure, retry CDN/outbox after commit. A second Unpublish returns 200 without a new event.
- [ ] Test rename-and-publish as one path-level saga: old and new canonical paths locked in stable order, redirect metadata committed with active Publication, and cutovers compensated together on failure.
- [ ] Test rollback creates a new Publication from retained immutable bytes, and after retention takes the explicitly weaker re-render path.
- [ ] Run `go test -p 1 ./internal/product/publication -run 'TestPublishSaga|TestUnpublishSaga|TestRollback' -count=1` red, implement, rerun and commit `feat: publish with compensating file cutover`.

### Task 9: Durable Webhook Outbox

**Owner:** Integration owner. **Depends on:** Task 5. **Deliverable:** once-created events with retryable delivery in the same process.

**Files:** Create `internal/product/publication/outbox.go`, `outbox_test.go`; modify `internal/services/webhook.go` only to add a synchronous `DeliverRecordedEvent` entry point that reuses signing, endpoint selection, delivery history and retry policy.

**Interfaces:** Produce `Outbox.InsertUnique(ctx, session, eventType, publicationID, payload) error` and `OutboxWorker.Run(ctx)`; event identity is `(event_type, publication_id)`.

```bash
go test -p 1 ./internal/product/publication -run 'TestOutbox|TestWebhookRetry' -count=1
```

- [ ] Test activation transaction commit inserts one `content.publish` outbox row; repeated execution with same Publication ID does not insert a second row.
- [ ] Test Unpublish and failed transitions insert matching event rows in the same Mongo transaction as lifecycle updates.
- [ ] Test process restart after commit but before HTTP delivery sends pending row; receiver 500 schedules retry; receiver sees stable event ID and existing HMAC signature.
- [ ] Run focused tests red, implement a Mongo claim lease so only one app replica delivers a row at a time, and rerun.
- [ ] Confirm downstream semantics are at-least-once HTTP delivery; expose a stable event ID header and give its exact name and payload shape to Task 18 for API/Agent documentation.
- [ ] Commit `feat: persist publication webhook events before delivery`.

### Task 10: Recovery Scanner and Retention

**Owner:** Integration owner. **Depends on:** Tasks 5, 6, 8. **Deliverable:** startup and ten-minute repair with safe migration gate.

**Files:** Create `internal/product/publication/recovery.go`, `recovery_test.go`, `gc.go`, `gc_test.go`; wiring in Task 16.

**Interfaces:** Produce `ScanOnce(ctx) (ScanReport,error)` and `Run(ctx)`; checks `system_migrations.publication_model_v1` before orphan quarantine.

```bash
go test -p 1 ./internal/product/publication -run 'TestRecovery|TestGC' -count=1
```

- [ ] Test active DB + missing/mismatched canonical is rebuilt from immutable bytes; missing immutable object creates P0 alert and no guessed replacement.
- [ ] Test `legacy_unverified` active pages remain served; migration `running` + no active canonical is reported but not quarantined; only completed migration permits orphan quarantine.
- [ ] Test stale stage and `.next-*` cleanup at 15 minutes; skip any held publish lock; restore `.previous-*` when DB old active remains.
- [ ] Test `.unpublish-backup-*`: restore canonical when DB remains active; remove backup when DB is unpublished and canonical is absent; never infer a new active Publication from files. Quarantine orphan files only after migration completion; `legacy_unverified` pages have an active Publication and are never orphaned.
- [ ] Test active and pinned objects are never GC'd; superseded/unpublished objects default to 90 days; failed/staged to 7 days; quarantine to 30 days; storage deletion precedes `storage_state=deleted`.
- [ ] Run `go test -p 1 ./internal/product/publication -run 'TestRecovery|TestGC' -count=1` red and then green; commit `feat: reconcile publication files and expire old objects`.

### Task 11: Idempotency Execution Snapshot and Lease

**Owner:** Lane D. **Depends on:** Task 2. **Deliverable:** durable request operation with business attempt separate from worker lease generation.

**Files:** Create `internal/product/idempotency/model.go`, `repository.go`, `service.go`, `service_test.go`.

**Interfaces:** Produce `Begin(ctx, owner, method, path, key, canonicalBody) (Operation,error)`, `BindContentAndVersion(ctx, session, opID, contentID, version, canonicalPath) error`, `FreezeExecution(ctx, opID, attempt, publicationID, logicalAt, templateVersionID) error`, `RenewLease`, `TakeOver`, `Complete`.

```bash
go test -p 1 ./internal/product/idempotency -run 'TestReplay|TestLease|TestExecutionSnapshot' -count=1
```

- [ ] Test same owner/method/path/key + same canonical body replays completed 2xx; changed body returns 409; 401/403/404/409/429/5xx never become completed; pure 400/422 may replay.
- [ ] Test worker lease expires while business attempt remains stable: `lease_generation` CAS changes, `attempt` does not; heartbeat at 60 seconds prevents takeover. Freeze and CAS-persist operation ID, Publication ID, logical publish time, target content/version, template version and canonical path before Render.
- [ ] Test crash before Render uses durable Publication ID and logical time; a terminal pre-activation failure marks the attempt terminal, then a retry increments attempt and allocates a new Publication ID.
- [ ] Test `BindContentAndVersion` is transactionally coupled to Content/Version creation or replacement, leaving no committed Content without recoverable operation ID.
- [ ] Run `go test -p 1 ./internal/product/idempotency -run 'TestReplay|TestLease|TestExecutionSnapshot' -count=1` red, implement, rerun; commit `feat: persist idempotent publication attempts`.

### Task 12: Generation Service, Exact HTTP Contract, and Scopes

**Owner:** Lane E. **Depends on:** Tasks 3–5, 8, 11, 13. **Deliverable:** one REST generation command and immutable schema endpoint.

**Files:** Create `internal/product/generation/types.go`, `service.go`, `service_test.go`, `internal/product/httpapi/generation.go`, `templates.go`, `publications.go`, `httpapi_test.go`; integration owner adds routes in Task 16.

**Interfaces:** `Generate(ctx, actor, GenerateRequest) (GenerateResponse,error)` with modes `draft|preview|sandbox|publish`; `GET /api/v1/templates/{slug}/schema`; `POST /api/v1/page-generation`; publication list/detail/rollback endpoints.

```bash
go test -p 1 ./internal/product/generation ./internal/product/httpapi -count=1
```

- [ ] Test target-state × mode matrix: absent/unpublished/published with draft/preview/sandbox/publish. Preview writes nothing. Published draft goes to Fork; published publish is direct authorized command through PublicationService.
- [ ] Assert every Generation response contains `requires_publish`: successful draft/sandbox writes return `true`, preview and completed publish return `false`; `published=true` may coexist with `requires_publish=true` when a live page has unpublished changes.
- [ ] Test full-replace Upsert, omitted/null/false/default behavior, 5 MiB data cap, unknown fields, canonical path case collision, and `upsert=false` 409.
- [ ] Test external publish requires `expected_template_version`, `Idempotency-Key`, and combined create+publish or edit+publish scopes. Test all 401/403/409/422/428/429/503 mappings and zero mutation on 403.
- [ ] Assert deprecated Template maps to `409 TEMPLATE_NOT_ACTIVE` and unknown/invalid data field to `422 FIELD_VALIDATION_FAILED` with field-level details.
- [ ] Implement admin-only `POST /api/v1/templates/{id}/migrate-slug`: enforce unique new Template slug, record audit and return affected external-integration guidance. Do not alter historical Template Versions, create a public-page URL redirect, acquire page-path locks or switch any Publication. Test slug collision and unchanged live page bytes/Publication.
- [ ] Implement Template Upgrade Preview (read-only) and an explicit durable Upgrade Job with per-content progress, retry/resume and per-item outcomes; never silently regenerate live pages.
- [ ] Implement `restore_and_publish` as a new publication command distinct from exact `revert_live`; both require scopes, preconditions and idempotency when externally invoked.
- [ ] Test schema response and ETag are from the same immutable TemplateVersion; publish after a template increment returns `409 TEMPLATE_VERSION_CHANGED`.
- [ ] Test stale or missing `expected_template_version` causes zero mutation; it is an optimistic precondition, not historical template selection.
- [ ] Run package tests red, implement handlers using existing `/api/v1` auth/rate/body/provenance middleware and service calls, rerun tests.
- [ ] Commit `feat: expose strict page generation and schema API`.

### Task 13: Public URL and Asset URL Hardening

**Owner:** Lane F. **Depends on:** Task 1. **Deliverable:** safe URLs and bounded remote asset import.

**Files:** Create `internal/product/publicurl/resolver.go`, `resolver_test.go`; modify `internal/handlers/api_assets.go`, `api_settings_test.go`; integration owner handles `config/config.go` in Task 16.

**Interfaces:** `NewResolver(baseURL *url.URL)` and `Resolve(canonicalFullPath string) (*url.URL,error)`; asset import returns `ASSET_TOO_LARGE` for 50 MiB + 1 byte.

```bash
go test ./internal/product/publicurl ./internal/handlers -run 'TestPublicURL|TestAPIUploadAssetFromURL' -count=1
```

- [ ] Test production URL must be HTTPS, path casing is preserved, path is escaped, and user-supplied path cannot change scheme/host/query/fragment.
- [ ] Test asset URL import with Content-Length over 50 MiB and chunked body over 50 MiB; both reject without saving truncated bytes.
- [ ] Test redirect to loopback/private, IPv6 loopback, DNS private address, fake MIME and oversized body against existing SSRF client.
- [ ] Run focused tests red, implement `io.LimitReader(resp.Body, maxSize+1)` plus explicit length check, and rerun.
- [ ] Commit `fix: validate public URLs and remote asset size`.

### Task 14: Existing Site Migration and Reconciliation

**Owner:** Lane G. **Depends on:** Tasks 2–6, 10. **Deliverable:** restartable migration with zero silent page loss.

**Files:** Create `internal/product/migration/migrate.go`, `report.go`, `migrate_test.go`; Integration owner adds `lightcms migrate-publications --dry-run|--apply` to the existing `cmd/server/main.go` binary in Task 16. Do not create a second migration binary.

**Interfaces:** `Run(ctx) (Report,error)`; `system_migrations.publication_model_v1` moves `not_started→running→completed`; `Report` lists invalid slugs, canonical collisions, missing files, render errors, verified and `legacy_unverified` pages.

```bash
go test -p 1 ./internal/product/migration -run TestMigrate -count=1
```

- [ ] Test dry-run reports invalid/duplicate template slugs, canonical path collisions, Content Version duplicates, missing static files, and render hash mismatches without writing data.
- [ ] Test matching existing canonical and current render creates verified active Publication with immutable bytes; mismatch copies existing bytes into immutable legacy object, creates active `legacy_unverified`, and leaves canonical serving.
- [ ] Test interrupted migration resumes page-by-page without duplicate active Publication; scanner does not quarantine no-active pages while flag is `running`.
- [ ] Test migration cannot mark `completed` while any blocking missing file/render error remains; final unique index creation follows collision remediation and keeps old index if new creation fails. Only after the new canonical index is verified, drop the old `(full_path, fork_id)` index so deleted paths can be reused under the new `path_active` rule.
- [ ] For each `legacy_unverified` page, persist an active Publication pointing to the copied immutable legacy object before marking migration complete; there must never be a serving canonical with no active record after completion. Test scanner after completion leaves these pages online. Missing-file or unrecorded pages block completion and require administrator action.
- [ ] Run `go test -p 1 ./internal/product/migration -run TestMigrate -count=1` red, implement, rerun and commit `feat: reconcile existing published pages`.

### Task 15: Admin Editor and Publication UX

**Owner:** Lane H. **Depends on:** Tasks 3, 4, 12 contract. **Deliverable:** one existing Admin UI with a safe draft/publish flow.

**Files:** Modify `internal/handlers/admin_templates.go`, `internal/handlers/handlers.go`; create `internal/handlers/admin_publications.go`, `admin_publications_test.go`; integration owner coordinates any shared `handlers.go` edit with Task 16.

**Interfaces:** UI calls the same TemplateContract, Generation, Fork Merge and Publication services as REST. No SPA or second frontend server.

```bash
go test -p 1 ./internal/handlers -run 'TestAdminProduct|TestAdminPublication' -count=1
```

- [ ] Test template selector shows category, description and required fields; Admin form renders new `url`, `number`, `boolean`, date, image, richtext and markdown fields with shared validation errors.
- [ ] Test published page Edit opens/reuses Fork and visibly states live is unchanged; Merge result shows `requires_publish` and does not change canonical HTML.
- [ ] Test Publish result displays Public URL, Publication ID, Content/Template versions; failed publish leaves prior URL available and shows a retryable error.
- [ ] Test template HTML change creates a new TemplateVersion and does not regenerate existing live pages; Template Upgrade requires explicit action.
- [ ] Implement explicit Upgrade Preview and Upgrade Job UI; implement separate `restore_and_publish` and `revert_live` buttons with clearly distinct outcomes.
- [ ] Run `go test -p 1 ./internal/handlers -run 'TestAdminProduct|TestAdminPublication' -count=1` red, implement, rerun; commit `feat: guide operators through draft and publication`.

### Task 16: Legacy Entry Point Migration and Single Binary Wiring

**Owner:** Integration owner. **Depends on:** Tasks 3–15. **Deliverable:** all live mutations use one in-process service and one server binary.

**Files:** Modify `cmd/server/main.go`, `internal/services/content.go`, `template.go`, `fork.go`, `scheduler.go`, `import.go`, `approval.go`, `regen_queue.go`, `content_watcher.go`, `internal/handlers/api_content.go`, `api_templates.go`, `api_forks.go`, `copilot.go`, `handlers.go`, `internal/apiclient/client.go`, `internal/mcp/content_tools.go`, `template_tools.go`; touch other direct `GenerateStaticPage` callers from Task 1 inventory.

**Interfaces:** Existing REST/MCP/CLI routes retain their URLs but call the new mutation and publication services. Only in-memory renderer paths may call rendering primitives; no live-writing caller invokes legacy `GenerateStaticPage` directly.

```bash
go test -p 1 ./internal/services ./internal/handlers ./internal/mcp ./internal/apiclient -count=1
```

This is an integration epic with six independent review gates (16A–16F). The integration owner lands each gate as a separate commit and runs focused regression before moving to the next. Gates 16C and 16D may be assigned to different workers after 16A/16B because their files do not overlap; only the integration owner combines them into the branch.

- [ ] **16A, content mutation:** Change `CreateContent/UpdateContent` to data/version operations; remove implicit static write/delete and live publish webhook. Keep compatibility response fields and add `has_unpublished_changes/requires_publish`. Test published PUT leaves canonical HTML unchanged. Commit `feat: make content writes draft-only`.
- [ ] **16B, core and template:** Replace `TemplateService.UpdateTemplate` automatic regeneration with TemplateContract version creation. Change Fork Merge to main Content drafts/versions with `requires_publish`; remove direct static writes from fork/approval/regeneration/watcher code paths. Test existing template edit and fork merge leave live bytes unchanged. Commit `feat: isolate template and fork edits from live pages`.
- [ ] **16C, HTTP and client:** Route REST single/batch publish, Admin publish, API Client and CLI to PublicationService. Register schema and publication routes before generic `{id}` routes; preserve auth, rate and body middleware. Require `Idempotency-Key` with HTTP 428 on externally triggered single/batch publish and rollback; Unpublish is naturally idempotent. Test old URLs return Publication IDs and no raw `GenerateStaticPage` call. Commit `feat: unify interactive publication routes`.
- [ ] **16D, background callers:** Route scheduler, import auto-publish, copilot, search replace auto-republish and template upgrade to PublicationService. Add MCP `get_template_schema` as a thin authorized adapter over the immutable Schema service. Give each internal job a stable operation key; test retries do not create duplicate Publication/outbox rows. Commit `feat: unify background publication callers`.
- [ ] **16E, one process wiring:** Add one server construction function wiring TemplateContract, Mutation Policy, StaticPageStore, PublicationService, GenerationService, Idempotency, OutboxWorker and RecoveryScanner. Add `PUBLIC_BASE_URL`, filesystem provider, `PUBLICATION_STAGE_TIMEOUT_MINUTES` (default 15), retention including quarantine 30 days, scan interval, Idempotency TTL/lease and publish rate settings to the existing config loader; reject production standalone Mongo and unsupported storage providers at startup. Start workers in the existing server process with cancellation on shutdown. Commit `feat: wire one LightCMS publication runtime`.
- [ ] **16F, observability:** Emit structured request/publication logs with actor, content/version, template/version, publication ID, path, stage, duration and error code. Add counters for generation, stage/activation failure, idempotency conflict/replay, outbox backlog and scanner repair; connect P0 alerts to the existing operational logging/metrics surface. Commit `feat: observe publication lifecycle`.
- [ ] After each gate run its focused packages. After 16E run `rg 'GenerateStaticPage\(|PublishContent\(' internal cmd --glob '*.go' --glob '!**/*_test.go'`; account for every remaining call in `docs/implementation/entry-point-matrix.md`.
- [ ] Run full `go test ./...`, `go vet ./...`, and server/MCP/CLI builds; record results and commit the updated entry-point matrix.

### Task 17: Whole-System Contract and Fault Tests

**Owner:** Integration owner with independent reviewer. **Depends on:** Tasks 1–16. **Deliverable:** production-like behavioral evidence.

**Files:** Create `internal/product/e2e/publication_test.go`, `migration_test.go`, `security_test.go`, and a test-only server bootstrap helper in that package. Extend `internal/handlers/fault_injection_test.go` only for cases tied to legacy routes.

**Interfaces:** Test via HTTP and public URL, not only direct service calls; launch one application instance against test replica set and temporary persistent directory.

```bash
go test -p 1 ./internal/product/e2e -count=1
```

- [ ] E2E create template → schema v3 → Generate publish → anonymous GET URL; assert content, `logical_published_at`, publication ID and template version match.
- [ ] Start the application with standalone Mongo in production mode and with `STATIC_STORAGE_PROVIDER=s3` before its ADR; assert both configurations fail at startup with actionable diagnostics.
- [ ] E2E publish v1 → Fork edit v2 → GET still v1 → merge → GET still v1 → publish → GET v2.
- [ ] Concurrent create `/News/Foo` and `/news/foo`: one success, one 409, one Content and one canonical file.
- [ ] Fault table: render/stage/verify/rename/Mongo/outbox/CDN failures and process crash between cutover and commit; assert old page remains or scanner restores authoritative content.
- [ ] Security table: create-only, edit-only, publish-only and sandbox-only keys; unauthorized Publish has zero mutation; SSRF and XSS tests pass.
- [ ] Idempotency table: lost response, parallel same key, changed payload, terminal pre-activation retry, crash takeover, rollback retry, no duplicate outbox.
- [ ] Migration table: verified legacy, `legacy_unverified`, missing file, restart midway; no legacy URL disappears from scanner action.
- [ ] Run with a configured replica set: `go test -p 1 ./internal/product/e2e -count=1`; record URL, Mongo version, test count and failures in `docs/implementation/test-report.md`.
- [ ] Measure new product package statement coverage with `go test -coverprofile` and require at least 85%; publish per-package and aggregate totals, with no skipped DB test counted as evidence.

### Task 18: API, Upgrade, Agent and Operations Documentation

**Owner:** Documentation worker. **Depends on:** Tasks 12, 16, 17 contracts. **Deliverable:** third party can integrate without source-code archaeology.

**Files:** Create `docs/openapi/page-generation-v1.yaml`, `docs/API.md`, `docs/AGENT-INTEGRATION.md`, `docs/TEMPLATE-GUIDE.md`, `docs/USER-MANUAL.md`, `docs/PUBLICATION-RUNBOOK.md`, `docs/DEPLOYMENT-LOCAL.md`, `docs/DEPLOYMENT-PRODUCTION.md` (split from the originally planned single `docs/DEPLOYMENT.md`), `docs/OPERATIONS.md`, `docs/BACKUP-RESTORE.md`, `docs/UPGRADE.md`, `docs/UPSTREAM-SYNC.md`, `docs/SECURITY.md`, `docs/README-product.md`.

**Interfaces:** OpenAPI is the public HTTP contract. Examples use the actual Task 12 route, `expected_template_version`, `Idempotency-Key`, scope combinations and error codes.

```bash
rg -n 'expected_template_version|Idempotency-Key|TEMPLATE_VERSION_CHANGED|PUBLICATION_CONFLICT' docs/openapi/page-generation-v1.yaml docs/API.md
```

- [ ] Write executable curl examples for schema, draft, preview, publish, published-page update, rollback, unpublish and asset import; include 201/200/409/422/428 responses.
- [ ] Explain the breaking change: updating published Content creates unpublished changes; clients that depended on PUT immediately changing live must call Publish explicitly.
- [ ] Describe OAuth/API key setup, sandbox Agent review, MCP schema tool, outbox event ID dedupe, retry/lease behavior, and error recovery.
- [ ] Include `README-product.md` with installation, one-process topology, configuration, quickstart, API links and migration warning; document Upgrade Preview/Job, slug migration and both restore commands.
- [ ] Write runbooks for migration dry-run, verified/legacy mismatch handling, scanner alerts, stale stage cleanup, backup/restore, publication rollback and storage retention.
- [ ] Validate OpenAPI using the project's available validator or parse it with a standard YAML/OpenAPI tool; compare every example field and response against Task 17 E2E output.
- [ ] Commit `docs: publish integration and operations contracts`.

### Task 19: Release Rehearsal and Signoff

**Owner:** Integration owner plus external reviewer. **Depends on:** Tasks 1–18. **Deliverable:** releasable single application and explicit signoff record.

**Files:** Create `docs/implementation/release-checklist.md`, `docs/implementation/release-report.md`; update `build.json` and `CHANGELOG.md` only when preparing an actual product release.

```bash
go test -p 1 ./... && go vet ./... && go build ./cmd/server ./cmd/mcp ./cmd/cli
```

- [ ] Re-run `go test -p 1 ./...`, `go vet ./...`, and `go build ./cmd/server ./cmd/mcp ./cmd/cli` against pinned product SHA and configured test replica set; record exact output and skipped tests.
- [ ] Rebuild MCP binary after MCP schema/tool changes, restart the MCP host, and compare runtime `tools/list` to the documented tool inventory.
- [ ] Rehearse migration on a copy of representative data with verified, mismatch, missing static, deleted, fork and case-collision examples; verify no old public URL silently disappears.
- [ ] Rehearse app process crash during Publish and Unpublish cutover; restart the same binary and verify scanner repairs canonical files and pending outbox events deliver.
- [ ] Verify one backend executable, one Admin UI, no required secondary services beyond MongoDB and configured static storage; inspect deployment manifest and process list.
- [ ] Record upstream SHA, product SHA, build and migration versions, OpenAPI version, test report, security review, backup result, rollback result, and remaining accepted risks.
- [ ] Request external architecture, security and operations signoff against the concrete release report; do not label the system production-ready before those checks pass.

## 3. Handoff and Scope Boundaries

Tasks 0–19 define the MVP. Optional R2/S3, custom domains, SaaS quota, analytics conversions and preview tokens are separate future plans; they are not prerequisites for MVP acceptance. The single server binary must retain a filesystem-only production configuration until a separate storage ADR and full integration suite are approved.

The spec and plan are intentionally distinct: the spec defines behavior, while this plan names code owners, order, files and tests. Any conflict discovered during execution is resolved by updating the spec first, then the affected task and contract tests. Workers do not quietly choose a different public behavior.

Every gate has one integration owner. Parallel workers hand off reviewed commits and focused test evidence; the integration owner alone changes shared wiring files. Before the release gate, the team runs a whole-branch review for stale direct static writes, incomplete REST/MCP migration, publication state inconsistencies, and public documentation drift.

### Requirement coverage map

| Design requirement | Implementation task | Proof task |
|---|---|---|
| Single Go application and one Admin UI | 16, 19 | 17, 19 |
| Existing Template slug and immutable versions | 2, 3 | 3, 17 |
| All field types, strict validation, JSON Schema | 2, 4, 15 | 4, 12, 17 |
| Canonical path uniqueness and lock order | 2, 8, 16 | 2, 17 |
| Published draft through Fork, no implicit live write | 8, 15, 16A, 16B | 15, 17 |
| Content Version CAS and immutable render snapshot | 2, 7, 16A | 7, 17 |
| Publication lifecycle and storage state | 5, 8 | 5, 17 |
| Filesystem stage, verify, cutover, compensation | 6, 8 | 6, 17 |
| Unpublish, rename, rollback | 8, 16C | 8, 17 |
| Idempotency lease, attempt, execution snapshot | 11, 12, 16D | 11, 17 |
| Unique outbox and existing signed webhook delivery | 9, 16 | 9, 17 |
| Crash recovery, legacy hold, retention | 10, 14 | 10, 17, 19 |
| REST/MCP/CLI/scheduler/import/copy legacy entry points | 12, 16 | 17, 19 |
| Public URL and asset import security | 13 | 13, 17 |
| Migration and Breaking Change documentation | 14, 18 | 17, 19 |
| OpenAPI and third-party integration examples | 18 | 18, 19 |
| Reproducible replica-set CI and ADR approval | 0, 1 | 0, 17, 19 |
| Template slug migration (no page URL or Publication change) | 12, 16 | 12, 17 |
| Page rename-and-publish saga (path locks, redirect and cutover) | 8, 16 | 8, 17 |
| Template Upgrade Preview and durable Job | 12, 15, 16D | 12, 15, 17 |
| Legacy 428, MCP schema tool, restore UI | 15, 16C, 16D | 15, 17, 19 |
| New product package coverage ≥85% | 17 | 17, 19 |

### Delivery sizing and risk register

Estimates are engineering days per task, excluding external review queues and migration data remediation. They are planning ranges, not release commitments. The integration owner revises them at G0 after baseline and CI inspection.

| Workstream | Tasks | Estimate | Principal risk and gate |
|---|---|---:|---|
| Baseline, test environment, ADRs | 0–2 | 5–8 | Replica-set/CI availability and dirty workflow files; block G0/G1 if absent |
| Template contract and validation | 3–4 | 5–8 | Historical schema and hash drift; block G1 on snapshot tests |
| Publication store, state, render and saga | 5–10 | 15–24 | File/Mongo split-brain and crash recovery; block G2 on fault tests |
| Idempotency, API, URL and migration | 11–14 | 12–18 | Permission side effects and legacy serving; block G3 on E2E |
| Admin and legacy wiring | 15–16 | 10–16 | Hidden direct live writes; block G3 on entry-point inventory |
| E2E, documentation and release | 17–19 | 8–13 | Skipped DB tests or missing coverage; block G4 |

### Acceptance evidence format

Each task report uses this exact short template:

```text
Task ID:
Baseline SHA:
Commit SHA:
Files changed:
Focused red test command and observed failure:
Focused green test command and observed result:
Regression command and observed result:
Public interface changes:
Remaining risk or none:
```

The integration owner records every gate in `docs/implementation/release-report.md`. A green `go test ./...` with DB integration tests skipped does not satisfy G2, G3, or G4.
