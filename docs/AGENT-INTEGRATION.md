# LightCMS Agent 集成指南（V3）

> 适用对象：用 API Key / MCP 驱动 LightCMS 做页面生成的外部 Agent 与自动化流程。
> REST 契约见 `docs/openapi/page-generation-v1.yaml`，curl 示例见 `docs/API.md`；
> 模板编写见 `docs/TEMPLATE-GUIDE.md`；发布运维见 `docs/PUBLICATION-RUNBOOK.md`；
> 安全模型见 `docs/SECURITY.md`；管理员界面操作见 `docs/USER-MANUAL.md`；
> 部署与备份见 `docs/DEPLOYMENT.md`、`docs/OPERATIONS.md`、`docs/BACKUP-RESTORE.md`。

行为依据（均已对照源码，而非凭记忆）：scope 组合见
`internal/product/generation/service.go:checkScopes`；沙盒-only 限制见
`internal/auth/permissions.go:sandboxAllowedPerms`；MCP 工具见
`internal/mcp/template_tools.go`（`get_template_schema`）与
`internal/mcp/sandbox_tools.go`；outbox 事件 ID 见
`internal/product/publication/outbox.go:EventID/RecordedPayloadShape`；
投递头见 `internal/services/webhook.go:DeliverRecordedEvent`；
租约规则见规范 §21.5 与 `docs/implementation/test-report.md` 的幂等 E2E 表。

## 1. API Key 申请与最小权限

Key 归属某个用户，继承其角色权限，并支持两种收窄手段：

- `scopes` 允许列表：为空 = 角色全部权限；非空 = 仅保留列出的 scope。
  可用 scope：`content.create`、`content.edit`、`content.publish`、`content.view`、
  `template.view`、`template.edit`、`asset.upload`。
- `sandbox_only=true`：沙盒专用 Key。服务端强制限制（`UserHasPermission`）：
  只允许读、创建/编辑内容（且会被限定打到 Fork 沙盒）、上传资源、
  查看模板/配置、Fork 操作；**直接改变线上的操作一律拒绝**，
  包括 publish、delete、settings、search-replace。

配钥匙原则（规范 §22.3）：

| Agent 类型 | 配置 |
|---|---|
| 外部 / 高风险 Agent（默认） | 独立用户 + 独立 Key，`sandbox_only=true`，走第 3 节的人审流程 |
| 可信自动发布 Agent | 独立用户 + 独立 Key，`sandbox_only=false`，最小 scope（`content.*` 按需 + `template.view` + `asset.upload`），**禁止** `template.edit`、用户/配置/webhook 管理 |
| 只读 Agent（巡检、问答） | 仅 `content.view` + `template.view` |

所有 Agent 请求都必须携带 `X-Agent-Session: <会话 ID>`。它只做来源标记与审计归因
（`actor=agent`，记录会话 ID），不代替认证；审计日志按会话记录，管理端可用
`get_agent_session_changes` 查看本会话动过的一切、用 `rollback_agent_session`
整体撤销。

## 2. 可信自动发布流程（trusted auto-publish）

```text
取模板 schema → 生成结构化数据 → 导入资源 → 预览 → 修阻塞错误
→ mode=publish（带 Idempotency-Key）→ 返回 public URL
```

1. **取 schema**：`GET /api/v1/templates/{slug}/schema`（`template.view`），
   记下 `template_version` 与各字段 required 约束；**不要**从 HTML 反推字段形状。
2. **组装 data**：完整替换语义——缺失字段按 schema required/default 处理，
   未知字段直接 `422`，`null` 一律 `422`。
3. **预览**：`mode=preview`（`content.view`），零写入，可反复调用。
4. **发布**：`mode=publish`（`content.create`/`content.edit` + `content.publish`），
   必须同时带 `expected_template_version`（第 1 步的值）与 `Idempotency-Key`。
   每个发布意图用**唯一** Key，例如 `agent-<session>-<序号>`：
   同一 Key 重发 = 取回上次结果；换了请求体沿用旧 Key = `409`。

## 3. 沙盒人审流程（reviewed agent，默认推荐）

```text
start_agent_sandbox → 在 Fork 里 create/edit → preview
→ get_fork_diff → 人工审核 → merge_fork → 显式 publish
```

- `sandbox_only=true` 的 Key 在 Generation Facade 里**只能用 `mode=sandbox`**，
  用其它 mode 直接 `403`；没有 active sandbox 时用 `mode=sandbox` 返回
  `409 AGENT_SANDBOX_REQUIRED`（实现见 `checkScopes` 与 `service.go:250`）。
- 沙盒内的改动只进 Fork，不触碰线上；`requires_publish=true` 表示有未上线改动。
- 人在管理端（或 MCP）看 diff、合 Fork 后，仍需一次**显式 publish** 才能上线——
  合并不等于上线。
- 会话审计：`get_agent_session_changes` 拉出本会话的全部变更供复核；
  出问题用 `rollback_agent_session` 按会话整体撤销。

## 4. MCP：get_template_schema

MCP 侧新增的薄只读工具（`internal/mcp/template_tools.go:221`），
是对模板 schema 端口的直接透传，与 Facade 校验的是**同一个不可变版本对象**：

- 工具名：`get_template_schema`（`ReadOnlyHint: true`）；
- 入参：`{"slug": "financial-news"}`（slug 必填，空即报错）；
- 返回：schema 端口的完整 JSON（含 `template`、`template_version`、`fields`、
  `json_schema`），Agent 无需二次推导字段形状。

建议的 MCP 调用链（规范 §23–§24）：先 `get_template_schema`（或 `list_templates`），
再 `create_content` / `update_content`，用 `preview_content` 验证，
可信 Agent 直接 `publish_content` / `publish_multiple`，
人审 Agent 走 `start_agent_sandbox` → 改 Fork → `get_fork_diff` → `merge_fork` →
显式发布。批量场景禁止用大量串行单页调用代替 `bulk_create_content` /
`publish_multiple`（`publish_multiple` 的每个 item 各自派生幂等 Key）。

## 5. Outbox 事件 ID 去重（X-LightCMS-Event-ID）

发布事件“创建一次、投递至少一次”（规范 §28.1）：Publication 激活事务内按
`UNIQUE(event_type, aggregate_id)` 写入 outbox，同一次发布的重试不会产生第二个事件；
进程在 commit 后、投递前崩溃，重启后继续投递 pending 事件。因此**接收方必须去重**：

- 去重键：HTTP 头 `X-LightCMS-Event-ID`（精确头名，实现见
  `publication.OutboxEventIDHeader` 与 `webhook.go:DeliverRecordedEvent`），
  其值恒为 `<event_type>:<publication_id hex>`，例如
  `content.publish:66f4c2a1b2c3d4e5f6071830`（实现见 `publication.EventID`）；
- 同一值也会出现在 JSON 体的 `event_id` 字段，体形状固定为
  `{"event_id":…,"event":…,"timestamp":RFC3339,"data":{…}}`
  （实现见 `RecordedPayloadShape`；`data` 内含 `content_id`、`publication_id`、
  `content_version`、`template_version`、`full_path`、`content_hash`、
  `logical_published_at` 等）；
- 同一事件重试时 `event_id` 不变，`timestamp` 会变（每次投递重新签名，
  签名头 `X-LightCMS-Signature` 用端点密钥对原文做 HMAC）——去重只看事件 ID；
- 事件类型：`content.publish`（仅在 Publication 成功激活后发送）、
  `content.unpublish`、`publication.failed`；webhook 投递失败不回滚已激活的发布，
  只进入重试与投递历史。

伪代码：

```python
seen = set()  # 生产环境请用持久化存储
event_id = request.headers["X-LightCMS-Event-ID"]
verify_hmac(request.body, request.headers["X-LightCMS-Signature"], secret)
if event_id in seen:
    return 200  # 重复投递，直接确认
handle(request.json["data"])
seen.add(event_id)
```

## 6. 重试与租约行为

幂等记录以 `(owner, method, path, key)` 唯一（`owner` 取 API Key ID，
OAuth 取 `client_id + subject user_id`；`Authorization`、request_id 等不进入请求哈希）：

- 同 Key + 同请求 + 已完成 → 原样重放（保留原状态码/body/资源 ID，只更新 HTTP `Date`）；
- 同 Key + 不同请求 → `409 IDEMPOTENCY_CONFLICT`（换新 Key 重发正确请求）；
- 同 Key + 上次仍在执行 → `409 REQUEST_IN_PROGRESS` + `Retry-After`
  （实现默认 5 秒）：用**同一 Key** 等待后重试，禁止换 Key；
- 执行租约默认 5 分钟，worker 每 60 秒心跳续租（CAS 校验 owner/attempt，
  失租立即停手）；租约过期后新 worker 接管（`lease_generation` +1，业务 attempt 不变）；
- 只有当“当前 attempt 尚未产生线上副作用且已标记终端失败”时，重试才会
  `attempt++` 并分配新 Publication ID 与新逻辑时间——终端失败永远用新 Publication，
  失败的 Publication 永不复活；
- 可缓存重放的只有 2xx 与纯请求校验产生的 400/422（24 小时）；
  401/403/404/409/429 与所有 5xx **不**缓存为已完成（权限、资源、锁、服务状态会变），
  非缓存错误会释放或缩短租约，修好状态后即可重试；
- Key 的 TTL 默认 24 小时（可配 1–72 小时），到期后同 Key 视为全新请求。

## 7. 错误恢复速查

- `428`（缺版本号 / 缺 Key）：补上后重发，之前零副作用；
- `409 TEMPLATE_VERSION_CHANGED`：重取 schema，用**新 Key**重发；
- `409 REQUEST_IN_PROGRESS`：等 `Retry-After`，**同 Key**重试；
- `409 IDEMPOTENCY_CONFLICT`：换新 Key；
- `409 PUBLICATION_CONFLICT`（`expected_active_id` 过期）：重读 publication 历史再决策；
- `409 AGENT_SANDBOX_REQUIRED`：先 `start_agent_sandbox` 再用 `mode=sandbox`；
- `403`：缺 scope——换有权限的 Key，**不要**反复重试（线上变更类 403 保证零写入，
  但重试也永远不会成功）；
- `503` / `429`（`retryable: true`）：按 `Retry-After` 退避；
- `422`：按 `details` 逐字段修（删未知字段、补必填、去掉 null），修好后新 Key 重发 publish。
