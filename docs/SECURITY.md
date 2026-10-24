# 安全（Security）

> 配套文档：[DEPLOYMENT-PRODUCTION.md](DEPLOYMENT-PRODUCTION.md)（生产部署）·
> [OPERATIONS.md](OPERATIONS.md)（日常运维）·
> [AGENT-INTEGRATION.md](AGENT-INTEGRATION.md)（Agent 集成）·
> [API.md](API.md)（HTTP 契约）· [USER-MANUAL.md](USER-MANUAL.md)（用户手册）

本文档描述 V3 发布面 + LightCMS 原有面的安全现状：权限组合、sandbox-only key、SSRF/XSS 防护、secret 管理。按仓库现状如实写，未实现的项明确标为缺口，不虚构。

## 1. 角色与 scope 组合

权限常量（`internal/auth/permissions.go:20-68`）：content（view/create/edit/delete/publish）、template（view/create/edit/delete）、asset（view/upload/delete）、settings（view/edit）、`search_replace.execute`、`apikey.manage`（管自己的 key）/`apikey.manage_all`（管所有 key）、`user.manage`、`audit.view`、fork（create/merge）、approval（submit/view/decide/manage_workflows）、discussion（post）、comment（delete）。

角色矩阵（`permissions.go:70-113`）：

- **viewer**：只读（content/template/asset/settings 的 view）。
- **contributor**：读 + 建内容 + 提审批 + 评论 + 传 asset（标记 pending_review）+ 管自己的 API key。
- **editor**：在 contributor 上加改/删/发内容、审批决断、管理 workflow、删 asset、建 fork。
- **admin**：全部（含用户管理、apikey.manage_all、fork.merge、comment.delete 等）。

API key scope（`permissions.go:130-137,155-176` `UserHasPermission`）：key 可带 `scopes` allowlist；**非空 scopes = 在角色权限基础上再收窄**（逐项命中才放行）；空 scopes = 继承角色全部权限。创建时未知 scope 被拒绝（`IsKnownPermission`，`permissions.go:180-189`）。实践：

- 给发布机器人只授 `content.view + content.create + content.edit + content.publish`（+ 可选 `asset.upload`），不要给 `settings.edit` / `user.manage` / `apikey.manage_all`。
- V3 高危操作另有独立门槛：`migrate-slug` 要求 admin 且带 `template.edit`（`internal/product/generation/migrate.go:35-41`）；`restore_and_publish` 要求 `content.edit + content.publish`（`internal/product/generation/restore.go:41-47`）；template upgrade preview/job 走 generation service 的 actor 检查（见 [API.md](API.md)）。
- `search_replace.execute` 是独立高危 scope（破坏性批量写），只授给走 preview→确认流程的人/钥（见 [USER-MANUAL.md](USER-MANUAL.md)）。

## 2. Sandbox-only key

创建时 `sandbox_only: true` 的 key 在服务端被强制约束（`permissions.go:139-176`），不是客户端自律：

- 允许的权限白名单（`sandboxAllowedPerms`，`permissions.go:143-153`）：content 读/建/改、template 读、asset 读/传、settings 读、fork.create、discussion.post。**直接改线上的一切权限都被排除**：content.publish/delete、template 写、settings 写、fork.merge、审批决断等一律拒绝。
- 内容 handler 二次强制 fork 归属（`internal/handlers/api_content.go:308-309,416-417,1492-1493`、`internal/handlers/api.go:106-107`）：sandbox-only key 建内容必须带 `fork_id`，改内容只能改 fork 副本（`ForkID != nil`），碰 live 内容返回 403（"work inside a fork and submit it for human review"）。
- 用途：给外部 Agent / 不可信自动化发 sandbox-only key + scopes 双约束（ scopes 先收窄到最小，再用 sandbox-only 锁死 fork 内），人工在 `/cm/forks/{id}` 审 diff 后合并（见 [AGENT-INTEGRATION.md](AGENT-INTEGRATION.md)）。

注意：sandbox-only 不代替 code review——fork 合并（`fork.merge`，仅 admin）是最后的人工关口，合并前必须看 diff。

## 3. SSRF 防护现状

`POST /api/v1/assets/from-url`（服务端代抓远程 asset）是 SSRF 面，已做纵深（`internal/handlers/api_assets.go`）：

- URL 校验（`api_assets.go:63-85` `validateRemoteAssetURL`）：仅 `http/https` scheme、有 host、**禁止 userinfo**、拒绝 opaque；重定向同样校验（最多 5 跳，`api_assets.go:111-129`，重定向禁止 userinfo、限定 http/https）。
- 拨号时 SSRF 拦截（`api_assets.go:106-140` `ssrfSafeClient`）：dial 时重新 DNS 解析并检查**每一个返回 IP**，命中私网/保留段（`ssrfBlockedCIDRs`，`api_assets.go:47-55` `isPrivateOrReservedIP`）即拒绝，防 DNS-rebinding。
- 大小上限：50 MiB 硬截断（`api_assets.go:57-61,87-104`），超限 `ASSET_TOO_LARGE` 且**不落盘截断前缀**；超时 30s（`api_assets.go:109-110`）。
- 残留风险：代抓目标的响应内容仍会被存为 asset 并可能被模板引用——结合 §4 的 script policy（`none`/`admin_only`）限制其执行能力；不要把 from-url 能力授给匿名/低信任调用方（该接口需 `asset.upload` 权限）。

## 4. XSS / 脚本策略现状

站点级 `markdown_script_policy`（`internal/database/mongo.go:959,976`，默认 `"all"`）：

```text
"all"         默认；所有角色可用 raw HTML（含 <script>）
"admin_only"  editor 及以下走 bluemonday 消毒；admin 原样通过
"none"        所有作者内容一律消毒
```

消毒器去掉 `<script>` / `<iframe>` / `<form>` / `<input>`、事件 handler（`onclick=` 等）、`javascript:` URI，其它 HTML 保留。V3 渲染侧：未知 policy 值直接报 `CodeRenderValidation`（`internal/product/publication/render.go:220-229`），空值继承站点默认（回退 `"all"`，`render.go:228`）；policy 归一化见 `render.go:259-274`；生效 policy 冻结进 `dependency_snapshot`（`render.go:276-287`），因此改 policy 后已发布页面的精确回滚语义不变（快照记录的是当时的 policy）。

生产建议：对外投稿/不可信作者场景设 `admin_only` 或 `none`；默认 `"all"` 仅适用于作者全可信的站。改 policy 属站点配置变更，走 `update_site_config` 并记审计（见 [USER-MANUAL.md](USER-MANUAL.md)）。

评论 XSS 已修（历史记录，`comment.delete` 仅 admin）。模板 HTML 本身是受信代码（admin/template.edit 可写任意 layout），**模板作者 = 完全信任**，不要给不可信主体 template 写权限。

## 5. Secret 管理

绝不能提交的文件（`.gitignore` 已覆盖，出事前先确认）：

```text
config.dev.json / config.prod.json   # 含 mongo_uri / session_secret
.env / .env.local / .env.test        # 同上
.mcpregistry_*                       # registry token
deploy.sh                            # 本地部署脚本，不随发行
content/ / static/uploads/*          # 用户内容（顺带：也别提交）
```

规范 §36 明令 "Secret 禁止提交到仓库"。生产 secret 走 `fly secrets set`（`MONGO_URI` / `SESSION_SECRET` / `ANTHROPIC_API_KEY` 等），注意 legacy machine 的 staged-secret 陷阱（`fly secrets set` 后必须 machine update，见 [DEPLOYMENT-PRODUCTION.md](DEPLOYMENT-PRODUCTION.md) §1），用 `fly ssh console -C 'sh -c env'` 验证生效。

其它：

- `SESSION_SECRET` ≥ 32 字符，`openssl rand -hex 32` 生成；生产 `SECURE_COOKIES=true`（HTTPS 前提）。
- 登录限流： escalating lockout（10 次→1min、15 次→5min、20+→15min）；密码 bcrypt cost=12；session SameSite=Strict、24h 过期、生产 Secure。
- CSRF：`/cm` 路由 Gorilla CSRF 保护；管理员 UI 内嵌脚本的 token 由 `html/template` 转义，**不要预转义/预加引号**（历史生产事故：`printf "%q"` 双重包裹导致 copilot CSRF 失效）。
- Webhook 出站：HMAC 签名 + 稳定 event ID（`X-LightCMS-Event-ID`），接收端按 event ID 去重；at-least-once 下不要把“收到重复”当入侵。
- 公开只读面（`/llms.txt`、`/llms-full.txt`、`/mcp-public`）无鉴权是设计（draft/fork 结构性排除），不要把敏感内容放公开字段；公开端点只用投影查询（防 embedding 向量拖慢与泄漏）。
