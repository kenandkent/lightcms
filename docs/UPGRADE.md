# LightCMS V3 版本升级（Task 18C）

> 关联文档：本地联调见 `docs/DEPLOYMENT-LOCAL.md`；备份恢复见 `docs/BACKUP-RESTORE.md`；
> 发布操作见 `docs/PUBLICATION-RUNBOOK.md`；模板编写见 `docs/TEMPLATE-GUIDE.md`；
> API 契约见 `docs/API.md` 与 `docs/openapi/page-generation-v1.yaml`。
> 路由与语义以 Task 12 契约（`internal/product/httpapi/*`、`internal/product/generation/*`）
> 为准；Upgrade Job 的端到端执行已列入 `docs/DEPLOYMENT-LOCAL.md` §8（Task 19 复验），
> 本文件先给完整操作契约，凡标注“契约引用”的条目以代码/单测为准、待复验补实测。

核心原则（Task 12/15/16D）：**改模板 HTML 只产出新 TemplateVersion，绝不静默重生成
任何线上页**。线上页换模板渲染，必须走显式的 Upgrade Preview → Upgrade Job，
每页经 PublicationService 产生**新 Publication**（可回滚、可审计）。

## 1. Template Upgrade Preview（只读）与 Job（显式执行）

### 1.1 Preview：先看再动，不写任何东西

```http
GET /api/v1/templates/{slug}/upgrade-preview
```

- 权限：`template.view`。
- 返回：`template`、`from_version`（各页 active 模板版本的最小值）、
  `to_version`（当前版）、`total_pages`、`would_republish`、逐页 `items`
 （含 `content_id`、`full_path`、`has_active`、`active_template_version`、
  `target_template_version`、`would_republish`、`validation_errors`）。
- `would_republish=true` 的条件：有 active 记录 **且** active 模板版 ≠ 当前版
  **且** 现有数据能通过新版校验。校验不过的页（`validation_errors>0`）不会被自动重发，
  先修数据。
- 查询范围：同模板 live 页（MVP 上限 500）。

### 1.2 Job：建任务 → 执行 → 重试/续跑

```http
POST /api/v1/templates/{slug}/upgrade-jobs        # 建任务 → 201，items 预填
GET  /api/v1/templates/upgrade-jobs/{job_id}      # 查任务
POST /api/v1/templates/upgrade-jobs/{job_id}/run  # 执行 → 200，返回最新 job
```

Admin 端同名入口：模板页 `upgrade-preview`（GET）→ `upgrade-start`（POST 建任务）
→ `upgrade-jobs/{jobID}/run`（POST 执行）。

- 建任务权限：admin + `template.edit`。建任务本身**不碰任何线上页**；
  `would_republish=false` 的页直接记 `skipped`。
- 执行权限：`content.edit` + `content.publish`。逐页经 PublicationService 发布，
  每页一个新 Publication；失败记 `failed` + 原因，任务状态变为 `partial`。
- 重试/续跑语义：`done`/`skipped` 跳过，`pending`/`failed` 继续——**重调 `/run`
  即续跑**，不需要重建任务。每页有稳定操作键（`upgrade/<jobID>/<contentID>`），
  崩溃后续跑走幂等重放，不会双发 Publication/outbox。
- 状态机：`running` → 全过 `completed`；有失败无待办 `partial`。
  任务行（含逐页 `attempts`、`publication_id`、`error`）持久化在
  `template_upgrade_jobs` 表，可审计、可重查。
- 升级与迁移的关系：升级只处理“模板变了、页没跟上”；历史遗留页的首次建制
  （verified / `legacy_unverified`）是迁移的事，见 `docs/DEPLOYMENT-LOCAL.md` §3。

## 2. slug 迁移（模板改名，不是页面改 URL）

```http
POST /api/v1/templates/{id}/migrate-slug
Content-Type: application/json

{"new_slug": "<新slug>"}
```

- 权限：**admin + `template.edit`**，非 admin 一律拒绝。
- 效果：只改模板可变记录的 `slug`；**历史 `template_versions` 保留旧 slug**
  （可追溯）；**不建**任何页面 URL redirect（模板 slug 不是公网路径）；
  **不**拿路径锁、**不**切换任何 Publication——受影响页的线上字节与 active
  记录保持**字节一致**。
- 冲突：新 slug 已存在 → 拒绝；与当前相同 → 拒绝；非法格式 → 422。
- 返回 `affected_pages` + `guidance`：外部集成（MCP/REST 调用方）把引用旧 slug
  的地方换成新 slug；新起的页面生成必须用新 slug。
- 操作后：重跑受影响模板的 Upgrade Preview（§1.1），确认 `to_version` 链无断裂。

## 3. restore / revert 的选择（回滚三入口）

三者都**新建 Publication**（旧记录保留），都要 `content.edit` + `content.publish`，
外部调用都要 `Idempotency-Key`（缺则 428 零写入），都支持 `expected_active_id`
CAS（stale 报 `PUBLICATION_CONFLICT` 且零写入）。区别只在**源**与**字节语义**：

| 入口 | 源 | 字节语义 | 什么时候用 |
|---|---|---|---|
| `POST /api/v1/content/{id}/publications/{pid}/rollback` | 历史 **Publication ID** | 精确：用源 Publication 的保留不可变字节发新版。源字节已过保留期则降级为重渲染（saga 决定） | 线上版整体错了，要整页回到某次发布原样 |
| `POST /api/v1/content/{id}/revert-live`（body `source_publication_id`） | 历史 **Publication ID** | 同 rollback（精确字节优先，过期降级重渲染） | 同上；Admin 有独立按钮，语义与 rollback 一致 |
| `POST /api/v1/content/{id}/restore-and-publish`（body `version`） | 历史 **ContentVersion 号** | 重渲染：把历史版本数据先恢复成新草稿版，再走当前管线渲染发布 | 数据层面想找回某版内容、且接受按当前模板重渲染 |

决策树：

1. 灾难（删库/丢盘）→ 都别用，去 `docs/BACKUP-RESTORE.md` §4（回滚三入口
   **不能**替代备份恢复）。
2. 认准某次线上发布、一字节都不想差 → `rollback` / `revert-live`（先确认源
   Publication 的不可变对象还在保留期内）。
3. 只记得“第 N 版数据是对的”、模板可能已变 → `restore-and-publish`
  （body 给 `version` 数字）。
4. 模板升错级、大面积错版 → 先 `GET .../upgrade-preview` 看影响面，再决定是逐页
   回滚还是重跑一次 Upgrade Job（§1.2）。

## 4. 版本升级一般流程（建议顺序）

1. 备份（`docs/BACKUP-RESTORE.md` §2–§3：Mongo 归档 + `content/` 打包）。
2. 读 Preview（§1.1），处理 `validation_errors>0` 的页。
3. 建 Job 并执行（§1.2）；`partial` 则修因后续跑 `/run`。
4. 抽查公网 URL 与 `GET /api/v1/content/{id}/publications` 历史。
5. 需要改名再做 slug 迁移（§2），做完重看 Preview。
6. 出事用 §3 回滚；灾难用备份恢复。升级记录（job 行 + audit）保留备查。
