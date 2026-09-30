# Entry-Point Matrix — Task 1 (baseline `534e3c7`)

Columns: `entry` → `handler` → `service` → `permission` → `static side effect` → `target task`.
Every row with a live-file side effect is a **Task 16 migration target**:
after Task 16 no live-writing caller may invoke legacy `GenerateStaticPage`
directly. Permissions cite `requirePermission` call sites in
`internal/handlers/api_content.go` unless noted.

## 1. `/api/v1` content publish / mutation surface (`cmd/server/main.go:535-554`)

| entry | handler | service | permission | static side effect | target task |
|---|---|---|---|---|---|
| `POST /api/v1/content` | `APICreateContent` (`api_content.go:244`) | `ContentService.CreateContent` (`services/content.go:207`) | `PermContentCreate` | `GenerateStaticPage` if `published=true` (`content.go:250-256`) | 16A |
| `PUT /api/v1/content/{id}` | `APIUpdateContent` (`api_content.go:393-394`) | `UpdateContent` (`content.go:441`) | `PermContentEdit` | generate if published / `removeStaticPage` if not (`content.go:520-531`) | 16A |
| `DELETE /api/v1/content/{id}` | `APIDeleteContent` (`api_content.go:516`) | `DeleteContent` (`content.go:619`) | `PermContentDelete` | `removeStaticPage` (`content.go:639`) | 16A |
| `POST /api/v1/content/{id}/restore` | `APIRestoreContent` (`api_content.go:536`) | `RestoreContent` (`content.go:664`) | `PermContentEdit` | `GenerateStaticPage` if still published (`content.go:685-687`) | 16A |
| `POST /api/v1/content/{id}/publish` | `APIPublishContent` (`api_content.go:555-556`) | `PublishContent` (`content.go:560` → `UpdateContent`) | `PermContentPublish` | full generate path | 16C |
| `POST /api/v1/content/{id}/unpublish` | `APIUnpublishContent` (`api_content.go:575-576`) | `UnpublishContent` (`content.go:590` → `UpdateContent`, fires `content.unpublish`) | `PermContentPublish` | `removeStaticPage` via `UpdateContent` unpublished branch | 16C |
| `GET,POST /api/v1/content/{id}/preview` | `APIPreviewContent` (`api_content.go:1259-1260`) | in-memory `renderContentWithWarnings`, optional overrides | `PermContentView` | **none** (no save, no file) — V3 preview model already | 12/16C |
| `POST /api/v1/content/batch-publish` | `APIBatchPublishContent` (`api_content.go:1201-1202`) | `PublishContent` per id (`api_content.go:1241`) | `PermContentPublish` | generate per item | 16C |
| `PUT /api/v1/content/by-path` | `APIUpdateContentByPath` (`api_content.go:1302-1303`) | `UpdateContent` (`api_content.go:1374`) | `PermContentEdit` | same as update | 16C |
| `POST /api/v1/content/bulk-create` | `APIBulkCreateContent` (`api_content.go:1736-1737`) | `BulkCreateContent` (`content.go:326`; parallel generate ≤10, `content.go:404-424`) | `PermContentCreate` (+ `BulkUpdateLimiter`) | generate per published item | 16C |
| `POST /api/v1/content/bulk-update` | `APIBulkUpdateContent` (`api_content.go:1907-1908`) | `UpdateContent` per item (`api_content.go:2067`) | `PermContentEdit` (+ `BulkUpdateLimiter`) | generate per published item | 16C |
| `POST /api/v1/content/bulk-field-op` | `APIBulkFieldOperation` (`api_content.go:2136-2137`) | `UpdateContent` per item (`api_content.go:2258`) | `PermContentEdit` (+ `BulkUpdateLimiter`) | generate per published item | 16C |
| `GET /api/v1/content/{id}/versions…`, `POST …/revert` | `APIList/Get/RevertContentVersion` (`api_content.go:596-640`) | `RevertToVersion` → `UpdateContent` (`content.go:981`) | View / `PermContentEdit` for revert | generate via `UpdateContent` on revert | 16A |
| `POST /api/v1/content/scheduled`, `POST …/{id}/schedule` | `APIListScheduledContent`, `APIScheduleContentPublish` | `SchedulerService` queue | gated in handler (`api_schedule.go`) | deferred `PublishContent` (see §7) | 16D |

## 2. Templates / theme / search-replace / forks / approvals (`cmd/server/main.go:556-710`)

| entry | handler | service | permission | static side effect | target task |
|---|---|---|---|---|---|
| `POST /api/v1/templates`, `PUT /api/v1/templates/{id}` | `APICreate/UpdateTemplate` | `TemplateService.UpdateTemplate` (`services/template.go:49`) | template perms (`api_templates.go`) | layout change → `RegenQueue.Enqueue` else goroutine `regenerateContentByTemplate` → `GenerateStaticPage` per published page (`template.go:75-81,164`) | 16B |
| `PUT /api/v1/theme` | `APIUpdateTheme` | `SettingsService.UpdateTheme` (`services/settings.go`) | admin | header/footer change → goroutine `RegenerateAllContent` (`settings.go:77`) | 16B/16D |
| `POST /api/v1/regenerate` | `APIRegenerateAllContent` (`api_settings.go:687`) | `RegenerateAllContent` (`content.go:1965`, hash-gated) | admin (+ `RegenerateLimiter` 2/min) | full rewrite loop (`content.go:1951`) | 16B |
| `POST /api/v1/search-replace/execute`, `POST …/scoped/execute` | `APISearchReplaceExecute` (`api_content.go:948-949`), `APIScopedSearchReplaceExecute` (`:1502-1503`) | `UpdateContent` + conditional `PublishContent` when `AutoRepublish && wasPublished` (`api_content.go:1049-1054,1593-1598`) | `PermSearchReplace` (+ `SearchReplaceExecuteLimiter`) | generate per touched published page | 16D |
| `POST /api/v1/forks/{id}/merge`, `/archive` | `APIMergeFork`, `APIArchiveFork` | `ForkService.Merge` (`services/fork.go:197`) | fork perms (`api_forks.go`) | `GenerateStaticPage` for created/updated published pages (`fork.go:237,283`) | 16B |
| `POST /api/v1/approval-requests/{id}/approve` | `APIApproveRequest` | `ApprovalService` approve path (`services/approval.go:315-336`) | approval perms | direct `published=true` DB write + goroutine `GenerateStaticPage` (`approval.go:331`) | 16B/16D |
| `POST /api/v1/imports/markdown`, `/csv`, `/sources/{id}/trigger` | `APIImportMarkdown/CSV/TriggerImportSource` | `ImportService` (`services/import.go`) | import perms | `UpdateContent` + `PublishContent` when `autoPublish`/`src.AutoPublish` (`import.go:281+308,424+458,532+557`) | 16D |
| `POST /api/v1/assets/from-url` | `APIUploadAssetFromURL` | `AssetService` | asset perms (+ `AssetFromURLLimiter`) | none (asset bytes, not pages; Task 13 hardens size/SSRF) | 13/16D |
| `POST /api/v1/reindex-embeddings` | `APIReindexEmbeddings` | `SearchService` | admin (+ `ReindexLimiter`) | embedding index only, no static files | 16D |

## 3. Direct `GenerateStaticPage` / `PublishContent` / `UpdateContent` callers

`rg 'PublishContent\(|GenerateStaticPage\(|UpdateContent\(' internal cmd --glob '*.go' --glob '!*_test.go'`
(41 hits, case-sensitive; definitions excluded below — callers only.)
A case-insensitive follow-up found one further parallel path the capitalised
pattern misses: the Admin UI's own `h.generateStaticPage`
(`internal/handlers/handlers.go:3790`) with 6 call sites — see §4. It has
**no `ForkID` guard** (unlike the service version), a latent fork-safety gap
for Task 16.

**`GenerateStaticPage` direct callers (legacy live-write — all Task 16 targets):**

| file:line | caller context | target task |
|---|---|---|
| `internal/services/content.go:251` | `CreateContent` — published create | 16A |
| `internal/services/content.go:416` | `BulkCreateContent` — parallel generate (≤10) | 16A/16C |
| `internal/services/content.go:523` | `UpdateContent` — published branch (fork-guarded, `:520`) | 16A |
| `internal/services/content.go:686` | `RestoreContent` — published restore | 16A |
| `internal/services/content.go:1951` | `RegenerateAllContent` loop | 16B |
| `internal/services/template.go:164` | `regenerateContentByTemplate` (via RegenQueue or goroutine) | 16B |
| `internal/services/fork.go:237,283` | `Merge` — new + updated published pages | 16B |
| `internal/services/regen_queue.go:178` | `RegenQueue` worker batch | 16B |
| `internal/services/content_watcher.go:74` | change-stream watcher (published → generate; else `removeStaticPage`) | 16B |
| `internal/services/approval.go:331` | approval threshold → goroutine generate | 16B/16D |

**`PublishContent` callers (all route to `UpdateContent` today; Task 16 routes to PublicationService):**

| file:line | caller context | target task |
|---|---|---|
| `internal/handlers/api_content.go:566` | `APIPublishContent` | 16C |
| `internal/handlers/api_content.go:1053,1597` | search-replace (+scoped) `AutoRepublish` | 16D |
| `internal/handlers/api_content.go:1241` | `APIBatchPublishContent` | 16C |
| `internal/handlers/copilot.go:341` | copilot `publish_content` tool | 16D |
| `internal/services/scheduler.go:85` | scheduled-publish ticker | 16D |
| `internal/services/import.go:308,458,557` | RSS / markdown / CSV auto-publish | 16D |
| `internal/mcp/content_tools.go:543` | MCP `publish_content` (via API client) | 16C/16D |
| `internal/mcp/content_tools.go:719` | MCP `publish_multiple` (via `BatchPublishContent`) | 16C/16D |
| `internal/cli/content.go:213` | CLI `content publish` (via API client) | 16C |

**`UpdateContent` callers (become draft-only data/version ops in 16A):**

| file:line | caller context | target task |
|---|---|---|
| `internal/services/content.go:300` | `UpsertContent` — existing-path update | 16A |
| `internal/services/content.go:570,599` | `PublishContent` / `UnpublishContent` internals | 16A/16C |
| `internal/services/content.go:1015` | `RevertToVersion` | 16A |
| `internal/handlers/api_content.go:506,1374,1593,2067,2258` | update, by-path, scoped-S/R, bulk update, bulk field-op | 16A/16C/16D |
| `internal/handlers/api_content.go:1049` | search-replace execute | 16D |
| `internal/handlers/copilot.go:232` | copilot `update_content` tool | 16D |
| `internal/handlers/handlers.go:1235` (Admin `UpdateContent`) | Admin edit POST → service (verify on read) | 16C/15 |
| `internal/services/import.go:281,424,532` | RSS / markdown / CSV upsert paths | 16D |
| `internal/mcp/content_tools.go:514,829,1012` | MCP update / sandbox update / bulk-update (via API client) | 16C/16D |
| `internal/cli/content.go:186` | CLI `content update` (via API client) | 16C |
| `internal/apiclient/client.go:162,178,603,815` | API client wrappers (not direct service calls) | 16C |

Fork-safety guards confirmed in baseline: `GenerateStaticPage` no-ops for
`ForkID != nil` (`content.go:1140-1143`); `UpdateContent` skips static +
embedding for forks (`content.go:520-522`); `CreateContent` likewise
(`content.go:250`). These guards stay until Task 16 replaces the paths.

## 4. Admin UI (`/cm`, 134 routes, `cmd/server/main.go:242-397`)

Session + CSRF (`csrfMiddleware`, Gorilla CSRF, path `/cm`) + RBAC in handlers.
Mutation-relevant entries. **Key finding: the Admin UI bypasses
`ContentService` — it writes Mongo directly (`h.db.UpdateOne`) and renders
files via its own `h.generateStaticPage` (`handlers.go:3790`, no fork guard),
a parallel live-write path Task 16 must unify alongside §3:**

| entry | handler | static side effect | target task |
|---|---|---|---|
| `POST /cm/content/create` | `CreateContent` (`handlers.go:887`) | direct DB insert + `h.generateStaticPage` if published (`:1093`) | 16C/15 |
| `POST /cm/content/{id}` | `UpdateContent` (`handlers.go:1235`) | direct `h.db.UpdateOne` (`~:1487`) + `h.generateStaticPage` if published (`:1558`), file remove if unpublished | 16C/15 |
| `POST /cm/content/{id}/delete` | `DeleteContent` (`handlers.go:1585`) | direct `os.Remove` of static file (`~:1606-1611`) | 16C/15 |
| `POST /cm/content/{id}/regenerate` | `RegenerateContent` (`handlers.go:2191`) | `h.generateStaticPage` if published | 16C/15 |
| `POST /cm/content/{id}/versions/{v}/revert` | `RevertContentVersion` (`handlers.go:2118`) | `h.generateStaticPage` if published (`:2184`) | 16C/15 |
| `POST /cm/content/{id}/change-template/{t}/confirm` | `ConfirmChangeTemplate` (`handlers.go:1847`) | `h.generateStaticPage` if published (`:1947`) | 16C/15 |
| (seed, startup) | `SeedDefaults` | `h.generateStaticPage` for hello-world + 404 (`:263,373`) | keep |
| `POST /cm/templates/{id}`, `POST …/delete` | `UpdateTemplate`, `DeleteTemplate` | template regen (§2) | 16B/15 |
| `POST /cm/theme`, `POST /cm/config` | `UpdateTheme`, `UpdateSiteConfiguration` | theme regen (§2) | 16B/15 |
| `POST /cm/forks/{id}/merge`, `/archive`, `/delete`, `/fork-page`, `/pages/{p}/remove` | `MergeFork`… | via `ForkService.Merge` (§2) | 16B/15 |
| `POST /cm/content/replace-execute` (legacy `/api/content/replace-execute`) | `ReplaceExecute` | via update+republish | 16D |
| `POST /cm/copilot/chat` | `CopilotChat` → 10 service-layer tools (§8) | update + publish paths | 16D/15 |
| `POST /cm/imports/markdown`, `/csv`, `/sources/{id}/trigger` | `DoImportMarkdown`, `DoImportCSV`, `TriggerRSSSource` | via `ImportService` (§2) | 16D |

Read-only/admin-support routes (login, lists, versions view/diff, analytics,
audit, users, API keys, webhooks CRUD, locks, snippets, tools/search/chat
config, approvals page, asset library) have no static side effects.

## 5. MCP (`internal/mcp/server.go:37-58`; `/mcp` authed, `/mcp-public` open)

- 121 authenticated tools over 16 `register*Tools` groups + resources; all
  execute via the API client against §1–§2 routes (no direct service calls).
  Live-changing tools: `create_content`, `update_content`,
  `update_content_by_path`, `publish_content`, `publish_multiple`,
  `unpublish_content`, `delete_content`, `restore_content`, `revert_to_version`,
  `bulk_create_content`, `bulk_update_content`, `bulk_field_operation`,
  `search_replace_execute`, `scoped_search_replace_execute`,
  `create/update/delete_template`, `fork_page`, `merge_fork`,
  `remove_fork_page`, `approve_request`, `import_markdown/csv`,
  `trigger_import_source`, `schedule_content_publish`, `regenerate_all_content`,
  `update_theme/site_config`, sandbox writes (`start_agent_sandbox` forces
  fork-targeted writes; publish/delete/settings/search-replace rejected for
  sandbox-only keys).
- 4 public read-only tools (`public_server.go:72-150`): `get_site_info`,
  `list_pages`, `search_site`, `get_page` — no side effects.
- `tools/list` runtime dump not taken (no configured Mongo in this worktree);
  required before G3. Static registration list above is the baseline record.

## 6. CLI (`internal/cli/cli.go:32-59`; `cmd/cli/main.go`)

12 commands, all via API client: `content` (list/get/create/update/delete/
publish/unpublish/restore/versions/revert — `content.go:16`), `template`,
`asset`, `theme`, `config`, `redirect`, `folder`, `collection`, `search`,
`search-replace`, `api-key`, `regenerate`. Publish path:
`cli/content.go:213` → `client.PublishContent` → §1 publish route (16C).

## 7. Background / async entry points (all in-process, one binary)

| entry | code | static side effect | target task |
|---|---|---|---|
| Scheduled publish ticker (60s) | `services/scheduler.go:33-91`, `runOnce` → `PublishContent` (`:85`) | generate per due item | 16D |
| Regen queue worker | `services/regen_queue.go:178` | `GenerateStaticPage` per queued item | 16B |
| Content change watcher | `services/content_watcher.go:50-86` change stream | generate / `removeStaticPage` | 16B (remove or route via PublicationService) |
| Index/keyword/embedding workers | `content.go:136-204` (`indexRegenWorker`, `keywordRebuildWorker`, `triggerEmbedding`) | none on static files | keep (projections invariant) |
| Agent service + maintenance | `main.go:402-411` (`maintenanceService`, `agentService` goroutines) | read-only scans; no direct file writes | 16D |
| Theme CSS ensure on boot | `main.go:133` | `static/css/theme-vars.css` only | keep |
| `checkVersionMigration`, `MigrateAssetServePaths`, `EnsureThemeVersion1`, `SeedDefaults` | `main.go:112-135` | none on page HTML | keep |

## 8. Copilot (10 tools, `internal/handlers/copilot.go:49-96`)

`search_content`, `get_content`, `list_recent_content`, `list_templates`,
`create_content`, `update_content` (`copilot.go:232` → `UpdateContent`),
`publish_content` (`:341` → `PublishContent`),
`unpublish_content`, `get_maintenance_report`, `get_analytics`. Direct
service-layer calls with explicit `auth.HasPermission` checks + audit-logged
writes. Target: 16D.

## 9. Task 16 consumption checklist

- 16A (draft-only): §3 `UpdateContent` callers + `CreateContent:251`,
  `BulkCreateContent:416`, `RestoreContent:686`, `RevertToVersion:1015`.
- 16B (template/fork isolation): `template.go:75-81,164`,
  `fork.go:237,283`, `regen_queue.go:178`, `content_watcher.go:74`,
  `approval.go:331`, `settings.go:77`, `RegenerateAllContent:1965`.
- 16C (interactive routes): §1 publish/batch/bulk rows + Admin §4 (including
  the parallel `h.generateStaticPage` path + direct `os.Remove` on delete,
  both without fork guards) + CLI §6 + MCP publish/mutation tools (§5).
- 16D (background): scheduler, import auto-publish, copilot, search-replace
  auto-republish, approval publish, `get_template_schema` (new).
- Post-16E verification: re-run
  `rg 'GenerateStaticPage\(|PublishContent\(' internal cmd --glob '*.go' --glob '!**/*_test.go'`
  and account for every remaining hit here.
