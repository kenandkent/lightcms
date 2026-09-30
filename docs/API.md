# LightCMS 页面生成 API 指南（V3）

> 适用对象：需要直接调用 REST API 的第三方集成方。
> 权威契约：`docs/openapi/page-generation-v1.yaml`（12 条路由、全部状态码与错误码）。
> Agent 专用指引见 `docs/AGENT-INTEGRATION.md`；模板编写见 `docs/TEMPLATE-GUIDE.md`；
> 发布运维见 `docs/PUBLICATION-RUNBOOK.md`；部署与备份见 `docs/DEPLOYMENT.md`、
> `docs/OPERATIONS.md`、`docs/BACKUP-RESTORE.md`；升级见 `docs/UPGRADE.md`；
> 安全模型见 `docs/SECURITY.md`；产品总览见 `docs/README-product.md`；
> 管理员界面操作见 `docs/USER-MANUAL.md`。

本文所有示例均已对照实现校验：路由注册见 `cmd/server/main.go`（Task 16 接线），
请求/响应字段见 `internal/product/httpapi/*.go` 与 `internal/product/generation/*.go`，
行为断言见 `internal/product/httpapi/httpapi_test.go`、`httpapi_extra_test.go` 与
`docs/implementation/test-report.md`（58 个 E2E + 6 个 verdict，0 失败）。

下文约定：

```bash
BASE=https://pages.example.com   # 替换为你的 BASE_URL，本地开发一般为 http://localhost:8082
KEY=<你的 API Key>               # Authorization: Bearer $KEY
```

## 1. 认证

所有 `/api/v1` 路由共用同一条中间件链（`cmd/server/main.go`）：

```text
认证（API Key → 用户 + scopes/sandbox；OAuth 2.1；管理后台 session cookie 兜底）
→ 突发限流 20 请求/秒 → 滑动窗口限流 300 请求/分钟
→ 请求体上限 10 MiB → 来源标记（X-Agent-Session → actor=agent，审计用）
```

每个请求都携带 Key：

```bash
curl -H "Authorization: Bearer $KEY" "$BASE/api/v1/..."
```

未认证返回 `401 UNAUTHENTICATED`：

```json
{"error": {"code": "UNAUTHENTICATED", "message": "authentication is required", "retryable": false}}
```

Agent 请求请额外携带 `X-Agent-Session: <会话 ID>`，用于审计归因与会话级回滚，
详见 `docs/AGENT-INTEGRATION.md`。回显请求 ID 时请发送 `X-Request-ID`，
错误包中的 `request_id` 即为其回显。

## 2. 权限与 scope 矩阵

API Key 归属于某个用户，携带该用户的角色权限，并可进一步收窄：

- `scopes` 允许列表：为空表示拥有角色的全部权限；非空则只保留列出的 scope；
- `sandbox_only=true` 的 Key 只能做沙盒写入（见第 3 节与 `docs/AGENT-INTEGRATION.md`）。

页面生成 Facade 的 scope 组合（实现见 `generation.checkScopes`，缺一即 `403`）：

| mode / 目标 | 所需 scope |
|---|---|
| `draft` + 新页面 | `content.create` |
| `draft` + 已有页面 | `content.edit` |
| `preview` | `content.view`（若携带未保存数据还需对应的 create/edit） |
| `sandbox` + 新页面 | `content.create`（+ 沙盒归属） |
| `sandbox` + 已有页面 | `content.edit`（+ 沙盒归属） |
| `publish` + 新页面 | `content.create` + `content.publish` |
| `publish` + 已有页面 | `content.edit` + `content.publish` |
| 读模板 schema / 升级预览 / 查 job | `template.view` |
| 启动升级 job / 迁移 slug | 管理员 + `template.edit` |
| 执行升级 job / 回滚 / 恢复发布 / 还原上线 | `content.edit` + `content.publish` |
| 资源导入（from-url） | `asset.upload` |

`publish-only` 的 Key 不能修改 Content，`create-only` / `edit-only` 的 Key 不能发布。
组合权限检查发生在**任何持久化副作用之前**：权限不足直接 `403 PERMISSION_DENIED`，
且保证零写入（不创建 Content/Fork、不写 Version、不建 Publication、不落盘、
不发 webhook、不记审计变更事件、不完成幂等记录），系统也不会自动降级为 draft。
想存草稿请显式用 `mode=draft`（换一个新的 `Idempotency-Key`）重新请求。

## 3. 破坏性变更：PUT 不再直接上线

V3 之前 `PUT /api/v1/content/{id}` 会直接改变线上页面。V3 起该接口为**纯草稿写入**：

- 更新已发布页面不会触碰线上字节，而是写入该页面的 Fork 草稿；
- 每次成功写入都返回 `requires_publish: true`，表示存在未上线改动；
- 之前依赖 PUT 立即改变线上的客户端，**必须显式调用发布**（`mode=publish` 的
  `POST /api/v1/page-generation`，或既有发布路由）才能上线；
- 同一时刻线上存在 active Publication 时，响应会出现
  `published: true` 与 `requires_publish: true` 并存——这是正常的“线上版本 + 未上线草稿”状态。

## 4. 取模板 schema（200 + ETag）

```bash
curl -i -H "Authorization: Bearer $KEY" \
  "$BASE/api/v1/templates/financial-news/schema"
```

成功 `200`，响应头携带与版本绑定的 ETag（模板每次升级版本号都变，
实现见 `HandleTemplateSchema`）：

```http
ETag: "template-version-1"
```

```json
{
  "template": "financial-news",
  "template_version": 1,
  "fields": [{"name": "headline", "type": "text", "required": true}],
  "json_schema": {
    "type": "object",
    "additionalProperties": false,
    "properties": {"headline": {"type": "string"}},
    "required": ["headline"]
  }
}
```

发布前先取 schema，记下 `template_version`，下一步作为
`expected_template_version` 传回——这是乐观并发前置条件，不是“任选历史版本”
（实现只接受当前版本，过期即 `409 TEMPLATE_VERSION_CHANGED`，零副作用）。

未知 slug 返回 `404 TEMPLATE_NOT_FOUND`。

## 5. 存草稿（201，requires_publish=true）

```bash
curl -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{
  "template": "financial-news",
  "title": "Bitcoin Market Update",
  "slug": "bitcoin-market-update",
  "folder_path": "/news",
  "mode": "draft",
  "data": {"headline": "Bitcoin Rallies"}
}' "$BASE/api/v1/page-generation"
```

成功 `201`（实现：`action=created` 即 201；upsert 命中已有页面则 `200`）：

```json
{
  "id": "66f4c2a1b2c3d4e5f6071829",
  "action": "created",
  "mode": "draft",
  "published": false,
  "requires_publish": true,
  "content_version": 1,
  "template": "financial-news",
  "template_version": 1,
  "publication_id": null,
  "full_path": "/news/bitcoin-market-update",
  "public_url": null,
  "warnings": []
}
```

注意：`requires_publish` 在每个生成响应里**永远存在**；草稿/沙盒写入为 `true`。

## 6. 预览（200，零写入）

```bash
curl -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{
  "template": "financial-news",
  "title": "Preview",
  "slug": "preview-once",
  "folder_path": "/news",
  "mode": "preview",
  "data": {"headline": "Bitcoin Rallies"}
}' "$BASE/api/v1/page-generation"
```

成功 `200` 且 `requires_publish: false`；preview 不写 Content、Version、Fork、
Publication，也不创建幂等记录（实现用测试断言了写入前后 `content` 计数不变），
`Idempotency-Key` 可不带，带了也会被忽略。

## 7. 发布（200/201；必须带版本号 + Idempotency-Key）

```bash
curl -H "Authorization: Bearer $KEY" \
     -H 'Content-Type: application/json' \
     -H 'Idempotency-Key: partner-news-20260930-001' -d '{
  "template": "financial-news",
  "expected_template_version": 1,
  "title": "Bitcoin Market Update",
  "slug": "bitcoin-market-update",
  "folder_path": "/news",
  "mode": "publish",
  "upsert": true,
  "data": {"headline": "Bitcoin Rallies"}
}' "$BASE/api/v1/page-generation"
```

新页面发布成功为 `201`，已有页面发布成功为 `200`：

```json
{
  "id": "66f4c2a1b2c3d4e5f6071829",
  "action": "updated",
  "mode": "publish",
  "published": true,
  "requires_publish": false,
  "content_version": 2,
  "template": "financial-news",
  "template_version": 1,
  "publication_id": "66f4c2a1b2c3d4e5f6071830",
  "full_path": "/news/bitcoin-market-update",
  "public_url": "https://pages.example.com/news/bitcoin-market-update",
  "warnings": []
}
```

两把缺一不可的“锁”，缺失都是 `428` 且零副作用（实现见
`generation.Service.Generate`，先做前置检查再做任何写入）：

- 缺 `expected_template_version` → `428 TEMPLATE_VERSION_PRECONDITION_REQUIRED`；
- 缺 `Idempotency-Key` → `428 IDEMPOTENCY_KEY_REQUIRED`；
- 版本过期（模板已升级）→ `409 TEMPLATE_VERSION_CHANGED`，零副作用，
  客户端应重取 schema 后重试；
- `upsert: false` 且规范路径已存在 → `409 PATH_CONFLICT`；
- 同一 Key + 不同请求体 → `409 IDEMPOTENCY_CONFLICT`；
- 同一 Key 且上次执行仍持有租约 → `409 REQUEST_IN_PROGRESS`（带 `Retry-After`，
  默认 5 秒），等租约释放或被接管后再试，禁止换 Key 重试。

请求体细节（`mode=publish` 命中已发布页面是明确的线上变更命令，
直接替换主 Content 并产生新 Version 与新 Publication，不创建过渡 Fork）：

- `data` 是**完整替换**，不做隐式合并；缺失字段按 schema 的 required/default 处理，
  不解释为“保留旧值”；`upsert` 时未传的可选 `slug` 会按 `title` 重新生成；
- 未知顶层字段 / 未知 `data` 字段 → `422`；显式 `"data": null` → `422`
  （MVP 没有可空字段）；空字符串是明确值，走 required/minLength 校验；
- `data` 规范 JSON 上限 5 MiB（`422 DATA_TOO_LARGE`），整包仍受 10 MiB 限制。

## 8. 已发布页面的更新与上线

```bash
# 步骤 1：改草稿（PUT 是草稿写入，不上线；返回更新后的 content 对象）
curl -X PUT -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"data": {"headline": "Bitcoin Surges"}}' \
  "$BASE/api/v1/content/66f4c2a1b2c3d4e5f6071829"
# 线上仍是旧版本；如需确认待发布状态，请用 page-generation 的 draft/upsert
# 响应（其中 requires_publish=true 表示存在未上线改动），或查 publication 历史

# 步骤 2：显式发布（upsert + publish，一次完成替换与上线）
curl -H "Authorization: Bearer $KEY" \
     -H 'Content-Type: application/json' \
     -H 'Idempotency-Key: partner-news-20260930-002' -d '{
  "template": "financial-news",
  "expected_template_version": 1,
  "title": "Bitcoin Market Update",
  "slug": "bitcoin-market-update",
  "folder_path": "/news",
  "mode": "publish",
  "upsert": true,
  "data": {"headline": "Bitcoin Surges"}
}' "$BASE/api/v1/page-generation"
```

## 9. 回滚到历史 publication（200，新 publication）

先查历史，再回滚。回滚需要 `content.edit + content.publish` 与 `Idempotency-Key`
（缺 Key 即 `428`，零副作用；实现见 `HandleRollback` 与 `RollbackPublication`）：

```bash
# 列出历史（200；无记录时 publications 为 []）
curl -H "Authorization: Bearer $KEY" \
  "$BASE/api/v1/content/66f4c2a1b2c3d4e5f6071829/publications"

# 查单条（404 PUBLICATION_NOT_FOUND：未知 ID，或该条不属于此页面）
curl -H "Authorization: Bearer $KEY" \
  "$BASE/api/v1/content/66f4c2a1b2c3d4e5f6071829/publications/66f4c2a1b2c3d4e5f6071830"

# 回滚（mint 出与来源不同的 NEW publication ID，mode=rollback）
curl -X POST -H "Authorization: Bearer $KEY" \
     -H 'Content-Type: application/json' \
     -H 'Idempotency-Key: partner-news-20260930-rb1' \
     -d '{"expected_active_id": "66f4c2a1b2c3d4e5f6071830"}' \
  "$BASE/api/v1/content/66f4c2a1b2c3d4e5f6071829/publications/66f4c2a1b2c3d4e5f6071830/rollback"
```

```json
{
  "content_id": "66f4c2a1b2c3d4e5f6071829",
  "content_version": 3,
  "publication_id": "66f4c2a1b2c3d4e5f6071844",
  "full_path": "/news/bitcoin-market-update",
  "public_url": "https://pages.example.com/news/bitcoin-market-update",
  "mode": "rollback"
}
```

`expected_active_id` 是可选的并发保护：与当前 active 不一致时返回
`409 PUBLICATION_CONFLICT`，此时应重读历史再决定。

回滚/还原命令的精确错误行为（实现见 `publication/service.go:Rollback` 经
`generation.mapSagaErr` 映射，已逐项核对源码；未知 ID 的 500 映射是已知缺口，
Task 19 负责是否改为 4xx，见 `docs/implementation/test-report.md` 第 7 节第 1 条）：

- 来源 publication 不属于该页面，或来源是 failed 状态 → `422 FIELD_VALIDATION_FAILED`；
- 内容 ID 或来源 publication ID 查无此行 → `500 INTERNAL_ERROR`；
- 只有**只读的详情接口**（`GET …/publications/{publication_id}`）会对未知/
  越界 ID 返回 `404 PUBLICATION_NOT_FOUND`。

两个“恢复”命令的区别（实现见 `restore.go`，测试断言两者 mint 出不同 publication）：

- `POST /api/v1/content/{id}/restore-and-publish` `{"version": 1}`——来源是**版本号**，
  按当前管线重渲染后发布，响应 `mode=restore_and_publish`；缺 `version` 即 `400`，
  版本号查无此行即 `404 CONTENT_NOT_FOUND`；
- `POST /api/v1/content/{id}/revert-live` `{"source_publication_id": "..."}`——来源是
  **publication ID**，按保留的不可变字节精确恢复，响应 `mode=revert_live`；
  缺来源即 `400`，其未知 ID / 越界来源的映射与回滚完全一致（422 / 500，
  无 404）。两者都需要 `content.edit + content.publish` 与 `Idempotency-Key`。

## 10. 下线（天然幂等，200）

```bash
curl -X POST -H "Authorization: Bearer $KEY" \
  "$BASE/api/v1/content/66f4c2a1b2c3d4e5f6071829/unpublish"
```

下线是天然幂等操作：已下线时仍返回 `200`，且不会产生第二个 outbox 事件。
`Idempotency-Key` 可带可不带（不是必需）。

## 11. 资源导入（from-url）

```bash
curl -X POST -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{
  "url": "https://cdn.example.com/hero.png",
  "serve_path": "/assets/hero.png",
  "description": "homepage hero"
}' "$BASE/api/v1/assets/from-url"
```

需要 `asset.upload` 权限；缺 `url` 即 `400`。服务端对远端抓取做了 SSRF 与体积加固
（Task 13），拿到 serve 地址后再填入页面 `data` 的图片字段。

## 12. 常见错误速查

统一错误包（实现见 `httpapi.WriteError`，契约见规范 §20.8/§27）：

```json
{"error": {"code": "FIELD_VALIDATION_FAILED", "message": "…",
           "request_id": "…", "retryable": false, "details": []}}
```

- `request_id` 为请求头 `X-Request-ID` 的回显；
- `details` 只在字段级 422 里出现，形如
  `{"code": "FIELD_REQUIRED", "field": "data.headline", "message": "…"}`；
- 内部的 Mongo/文件系统/网络错误**永远不会**直接透出，
  一律映射为上述稳定业务码；
- `429`、409 的 `REQUEST_IN_PROGRESS`、可重试的 `503` 会携带 `Retry-After`。

状态码总表（实现见 `generation.StatusForCode`，HTTP 层测试逐项锁定）：

| 状态 | 含义 |
|---|---|
| 200 | upsert 更新、preview、已存在页面的发布、列表/详情/回滚/恢复/还原、下线 |
| 201 | 新建草稿/沙盒、新页面的发布、新建升级 job |
| 400 | `INVALID_REQUEST`（非法 ID、坏 JSON、缺 `version`/`source_publication_id` 等） |
| 401 | `UNAUTHENTICATED` |
| 403 | `PERMISSION_DENIED`（scope 不足；线上变更命令保证零副作用） |
| 404 | `TEMPLATE_NOT_FOUND` / `CONTENT_NOT_FOUND` / `PUBLICATION_NOT_FOUND` / `TEMPLATE_VERSION_NOT_FOUND` / `UPGRADE_JOB_NOT_FOUND` |
| 409 | `PATH_CONFLICT`、`TEMPLATE_NOT_ACTIVE`、`TEMPLATE_VERSION_CHANGED`、`TEMPLATE_VERSION_CONFLICT`、`CONTENT_VERSION_CONFLICT`、`PUBLICATION_CONFLICT`、`PAGE_PUBLISH_IN_PROGRESS`（可重试）、`IDEMPOTENCY_CONFLICT`、`REQUEST_IN_PROGRESS`（可重试）、`AGENT_SANDBOX_REQUIRED`、`UPGRADE_JOB_CONFLICT` |
| 422 | `FIELD_VALIDATION_FAILED`（含 `FIELD_REQUIRED` 等 details）、`PATH_INVALID`、`DATA_TOO_LARGE`、`TEMPLATE_SCHEMA_INVALID` |
| 428 | `TEMPLATE_VERSION_PRECONDITION_REQUIRED`、`IDEMPOTENCY_KEY_REQUIRED`（均为零副作用） |
| 429 | `RATE_LIMITED`（带 `Retry-After`） |
| 503 | `PUBLICATION_STAGE_FAILED`（静态存储暂时不可用，`Retry-After: 30`） |
| 500 | `INTERNAL_ERROR`（不可重试的内部错误） |

## 13. 错误恢复

- **428**：补上缺失的 `expected_template_version`（先重取 schema）或
  `Idempotency-Key` 后重试；之前无任何副作用，可安全重放同一请求。
- **409 TEMPLATE_VERSION_CHANGED**：模板已升级，重取 schema 拿到新版本，
  用新 `Idempotency-Key` 重发（旧 Key 绑定旧请求体，沿用会触发 `IDEMPOTENCY_CONFLICT`）。
- **409 REQUEST_IN_PROGRESS**：上次执行仍在跑，等待 `Retry-After` 秒后用**同一 Key**
  重试以取回结果；切勿换 Key，否则可能产生重复 publication。
- **409 IDEMPOTENCY_CONFLICT**：同一 Key 换了请求体——换新 Key 重发正确请求。
- **503 / 429**：按 `Retry-After` 退避重试（`retryable: true` 的都可重试）。
- **409 PUBLICATION_CONFLICT**（`expected_active_id` 过期）：重读 publication 历史，
  确认当前线上版本后再决定是否继续。
- **422**：按 `details` 逐字段修正（未知字段删掉、缺失必填补上、null 改掉），
  修正后用新 Key 重发 publish。
- Webhook 接收方去重用 `X-LightCMS-Event-ID`（投递至少一次），
  详见 `docs/AGENT-INTEGRATION.md`。
