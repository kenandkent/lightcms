# LightCMS V3 用户使用手册

> 面向内容编辑、运营管理员与 API 集成者。本文只讲“怎么做”：每个功能一节，
> 每节 = 前置条件 + 编号步骤 + 成功标志 + 出错怎么办。
>
> 相关文档（同目录，文件名精确引用）：
>
> - 接口参数与返回体细节：`API.md`
> - Agent / MCP 深度集成（鉴权、scope、沙箱策略）：`AGENT-INTEGRATION.md`
> - 模板字段与版本规则：`TEMPLATE-GUIDE.md`
> - 本地部署与联调：`DEPLOYMENT-LOCAL.md`
> - 生产部署与运维：`DEPLOYMENT-PRODUCTION.md`
>
> 术语：Content（内容页）、Template（模板）、Template Version（模板不可变版本）、
> Publication（一次发布记录，不可变）、Fork（已发布页面的草稿副本）、
> Public URL（线上访问地址）、canonical path（页面唯一路径 `full_path`）。
>
> 本手册所有步骤均对照已合并代码验证：
> `internal/handlers/admin_publications.go`（Admin 按钮与文案）、
> `cmd/server/main.go`（路由注册）、`internal/product/httpapi/`（接口行为与状态码）、
> `internal/product/generation/types.go`（错误码与 HTTP 映射），
> 以及 `docs/implementation/test-report.md`（E2E 证据：1709 个测试通过、0 失败，
> e2e 包 58 个场景 + 6 个结论锁通过）。

---

## 1. 用模板新建页面（模板选择与字段填写）

### 1.1 在 Admin 后台新建

前置条件：已登录 Admin（浏览器打开 `/cm/login` 登录）；目标模板状态为 `active`（`draft` 模板不可选，`deprecated` 模板不允许新建）。

1. 打开内容列表 `/cm/content`，进入新建页 `/cm/content/new`。
2. 在模板选择区点选一张模板卡片（卡片显示名称、分类、描述与必填字段列表），进入 `/cm/content/new/{templateID}` 填写表单。
3. 按字段类型填写（见 §1.3 填写规则），必填项标 `*`。
4. 提交保存为草稿；或直接发布（发布见第 2 章 `publish` 模式说明，后台发布按钮见第 9 章）。

成功标志：页面出现在 `/cm/content` 列表中，状态为未发布草稿；字段校验错误会以内联红色文字显示在对应字段下方（格式为 `错误码: 说明`）。

出错怎么办：
- 字段下标红（如 `FIELD_REQUIRED: …`）：按提示补填后重新提交，草稿未丢失。
- 模板不在选择列表：确认模板状态为 `active`（见第 10 章模板管理 `/cm/templates`）。

### 1.2 用 API 新建（`POST /api/v1/page-generation`）

前置条件：持有有效 API Key（请求头 `Authorization: Bearer <key>`，权限至少 `content.create`）；已通过 `GET /api/v1/templates/{slug}/schema` 拿到字段定义（见 `TEMPLATE-GUIDE.md` 第 3 章）。

1. 先取 schema，记下 `template_version`（作为 `expected_template_version`，见第 8 章）。
2. 发送创建请求（默认 `mode=draft`，不需要幂等键）：
   ```json
   POST /api/v1/page-generation
   {
     "template": "financial-news",
     "expected_template_version": 3,
     "title": "示例标题",
     "slug": "example-page",
     "folder_path": "/news",
     "data": { "headline": "…", "body": "…" },
     "mode": "draft"
   }
   ```
   新建必填 `title`、`template`、`data`；`slug` 可选（不填则由标题生成）。
3. 如该路径已存在且想覆盖式更新，加 `"upsert": true`；不加则已存在路径返回 `409 PATH_CONFLICT`（不覆盖）。

成功标志：新建返回 `201`（`action=created`）；upsert 命中已存在页面返回 `200`。响应含 `id`、`content_version`、`full_path`、`requires_publish: true`。

出错怎么办：
- `422 FIELD_VALIDATION_FAILED`：看 `error.details` 数组，逐字段修正（`field` 指字段名，`code` 见第 11 章）。
- `409 PATH_CONFLICT`：换 slug，或确认要覆盖后加 `upsert: true`（注意 upsert 的 `data` 是**完整替换**，不是合并，未提供的字段按必填/默认值处理）。
- `428 TEMPLATE_VERSION_PRECONDITION_REQUIRED`：外部发布缺 `expected_template_version`（见第 8 章）。
- `409 TEMPLATE_VERSION_CHANGED`：取 schema 后模板刚好升级，重新取 schema 再试。

### 1.3 字段填写规则（11 种类型）

| 类型 | 填什么 | 为空时 |
|---|---|---|
| text / textarea | 纯文本 | `""` |
| richtext | 富文本（沿用站内富文本编辑器） | `""` |
| markdown | Markdown 文本 | `""` |
| rawhtml | 原始 HTML（受脚本策略限制） | `""` |
| date | `YYYY-MM-DD`（如 `2026-09-30`，不存在的日期如 `2026-02-30` 会被拒绝） | 可选时整个 key 不填 |
| image | 站内资源路径（通过图片选择器上传后填入；会校验资源存在与类型） | `""` |
| select | 下拉选项之一（必须与模板定义的选项完全一致） | 可选时 `""` |
| url（新增） | 完整 URL（只允许模板限定的协议，默认 http/https） | 可选时 `""` |
| number（新增） | 数字（受 min/max 限制） | 可选时整个 key 不填 |
| boolean（新增） | 复选框 勾选=true / 不勾=`false`；未勾选的可选 boolean 不提交该字段；模板设了 `default=true` 时新建自动为 `true` | 可选且未填时 key 不存在（与“显式 false”不同） |

字段名规则：`[a-z][a-z0-9_]{0,63}`，不能使用保留名（`id`、`full_path`、`published`、`public_url` 等系统字段由渲染器注入，不要自己填）。
完整类型与校验细节见 `TEMPLATE-GUIDE.md` 第 1–2 章。

---

## 2. 草稿 / 预览 / 沙箱 / 发布四种模式

`POST /api/v1/page-generation` 的 `mode` 字段决定行为。不填默认为 `draft`。

### 2.1 `mode=draft`（保存草稿，不上线）

前置条件：`content.create`（新建）或 `content.edit`（改已有页）权限；不需要 `Idempotency-Key`。

1. 按 §1.2 组装请求，`mode` 取 `draft`（或省略）。
2. 发送请求。命中**已发布页面**时，系统自动创建/更新 Fork 草稿，**线上页面不受影响**。

成功标志：`201`（新建）或 `200`（更新），`published=false`、`requires_publish=true`。

出错怎么办：`409 AGENT_SANDBOX_REQUIRED` 表示当前 Key 是沙箱专用 Key，只能用 `mode=sandbox`（见 §2.3）。

### 2.2 `mode=preview`（只渲染，不写库）

前置条件：`content.view` 权限（如携带未保存数据还需对应的 create/edit 权限）。

1. 发送与 draft 相同的请求体，仅把 `mode` 改为 `preview`。
2. 读取响应中的渲染结果确认排版。**Preview 不创建 Content、版本、Fork、Publication，也不创建幂等记录**，可随意重试。

成功标志：`200`，`requires_publish=false`，数据库无任何新增。

### 2.3 `mode=sandbox`（Agent 沙箱草稿）

前置条件：已调用 `start_agent_sandbox` 获得有效的 Agent 沙箱会话（MCP 工具，见第 10 章）；Key 需有沙箱权限。沙箱专用 Key（`sandbox_only=true`）**只能**用此模式，用 draft/publish 会返回 `409 AGENT_SANDBOX_REQUIRED`。

1. 确认沙箱会话有效（无会话时此模式返回 `409 AGENT_SANDBOX_REQUIRED`）。
2. 发送请求，`mode=sandbox`，其余同 draft。写入进入沙箱/Fork 草稿，不触碰线上。

成功标志：`201`/`200`，`requires_publish=true`，线上页面不变。

出错怎么办：`409 AGENT_SANDBOX_REQUIRED` → 先走 `start_agent_sandbox` 流程（见 `AGENT-INTEGRATION.md`），不要改用 draft 绕过。

### 2.4 `mode=publish`（创建并直接上线）

前置条件：组合权限——新建要求 `content.create + content.publish`，改已有页要求 `content.edit + content.publish`；**必须带 `Idempotency-Key` 请求头**（见第 8 章）；建议带 `expected_template_version` 锁定模板版本。

1. 先用 `preview` 或 `draft` 确认内容无误。
2. 生成一个新的幂等键（如 `news-20260930-001`），放入请求头 `Idempotency-Key`。
3. 发送请求（`mode=publish`）。目标不存在时创建内容+版本+Publication；目标存在时替换内容+新版本+Publication；**命中已发布页面时直接发布新版本，不创建中间 Fork**。
4. 记录响应中的 `publication_id` 与 `public_url`。

成功标志：新建 `201` / 已存在 `200`，`published=true`、`requires_publish=false`，`public_url` 可访问。

出错怎么办：
- 缺键返回 `428 IDEMPOTENCY_KEY_REQUIRED`（零副作用，直接补键重发）。
- 权限不足返回 `403` 且**零副作用**（不会降级保存草稿；想留稿就用新键显式重发 `mode=draft`）。
- 发布中并发冲突 `409 PAGE_PUBLISH_IN_PROGRESS`：等几秒后用**同一键+同一请求体**重发（见第 8 章）。

> 破坏性变更提醒：旧版本“保存已发布内容即自动上线”已取消。现在任何更新只产生草稿/Fork/版本（`has_unpublished_changes=true`、`requires_publish=true`），**必须显式 Publish 线上才会变**。过去依赖“PUT 即上线”的脚本必须在更新后补一次 Publish 调用。

---

## 3. 已发布页面的改稿流程：改稿 → Fork → 合并 → 发布

前置条件：页面已发布（存在 active Publication）；改稿人不直接碰线上字节，所有改动先入 Fork 草稿。

### Admin 后台流程

1. 打开内容编辑页 `/cm/content/{id}`。若页面已发布，页顶会出现横幅 **“Editing Draft — Live page unchanged.”**（附 Fork 编号），表示你正在改草稿，线上不变。
2. 修改字段并保存（保存只写 Fork 草稿，可反复保存）。
3. 打开 Fork 列表 `/cm/forks` → 进入对应 Fork `/cm/forks/{id}` 查看差异，确认改动范围。
4. 在 Fork 页提交合并（`POST /cm/forks/{id}/merge`）。合并结果页会显示更新/新增条数与 `requires_publish` 列表，并明确提示 **“Live canonical HTML is unchanged until you Publish.”**——合并只合草稿，不上线。
5. 回到内容页，点 **“发布上线”** 按钮（`POST /cm/content/{id}/publish`，见 §4.1），拿到 Public URL 即完成。

成功标志：发布后响应页显示 Public URL、Publication ID、内容版本与模板版本；旧 Publication 保留为历史（状态变为 superseded）。

### API / MCP 流程

1. 改已发布页面：`PUT /api/v1/content/{id}`（或 `POST /api/v1/page-generation` + `mode=draft`）——系统自动写入 Fork，不碰线上。
2. 查差异：Fork 查询接口 / MCP `get_fork_diff`。
3. 合并：MCP `merge_fork`（语义与后台合并一致：只合草稿，返回 `requires_publish`）。
4. 发布：`POST /api/v1/content/{id}/publish`（旧版单页发布接口，需带 `Idempotency-Key`）或 `POST /api/v1/page-generation`（`mode=publish` + 键）。

出错怎么办：合并后发现漏改 → 直接再改 Fork、再合并、再发布即可，每次发布都是新 Publication，旧版本都在，可回滚（见第 4 章）。

---

## 4. 发布 / 回滚 / 下线

### 4.1 发布已保存的草稿（Admin）

前置条件：草稿已保存且通过字段校验；登录 Admin。

1. 打开内容编辑页 `/cm/content/{id}`，在主表单下方（保存/删除按钮之后）点 **“发布上线”** 按钮。按钮只对已存在的页面出现（新建页先保存一次），提交 `POST /cm/content/{id}/publish`；页面已有 active Publication 时表单自动附带隐藏字段 `expected_active_id`（当前 active Publication ID）做并发保护，期间若被别人发布会报冲突而不会覆盖。
2. 阅读结果页：
   - 成功：显示 Public URL（可点击）、Publication ID、Content ID、内容版本、模板版本、路径。
   - 失败：显示错误码（如 `PUBLICATION_STAGE_FAILED`）；**原线上 URL 继续服务**；若错误可重试，页上有 **“重试发布”** 按钮。

成功标志：结果页出现 Public URL 且可访问。

出错怎么办：点 **“重试发布”**（重发同一次发布，安全）；若错误不可重试（提示先修错误），先修字段/模板问题再发。

**“已发布”勾选框**：编辑页“页面设置”里的 **“已发布”** 决定保存后的发布状态——勾选后保存即通过发布流程上线；线上版本可在发布历史（发布上线按钮旁的 **“发布历史”** 链接，`GET /cm/content/{id}/publications`）中查看和回滚。只想更新草稿、暂不动线上时不要勾选，用 **“更新”** 保存，改完再点 **“发布上线”**。

### 4.2 回滚到某次历史发布（精确回滚）

前置条件：知道目标历史 Publication ID（从发布历史查到，见第 5 章）；回滚会创建一个**新** Publication，旧记录永不篡改。

Admin：
1. 打开发布历史 `GET /cm/content/{id}/publications`（从内容编辑页的 **“发布历史”** 链接进入），找到目标行。
2. 点该行 **“回滚线上到该发布 (revert_live)”**，在表单中填入本次操作的幂等键（表单字段 `idempotency_key`，必填，不填整页报错 `IDEMPOTENCY_KEY_REQUIRED`）。
3. 提交 `POST /cm/content/{id}/publications/{publicationID}/revert_live`。

API：`POST /api/v1/content/{id}/revert-live`，请求体 `{"source_publication_id": "<历史PublicationID>", "expected_active_id": "<可选>"}`，必须带 `Idempotency-Key` 请求头。

成功标志：`200`，返回新 Publication 信息；线上字节与目标历史发布**完全一致**（精确回滚，保留对象 90 天内有效；被 pin 的版本更久有效）。

出错怎么办：`428` → 补幂等键；`409`（如目标版本冲突）→ 刷新历史列表确认 active 是否变化，带上最新的 `expected_active_id` 重试；源发布对象已被 GC（超保留期）→ 改用 §4.3 的重新渲染式恢复。

### 4.3 从内容版本恢复并发布（重新渲染式）

适用场景：想恢复的是“某版内容数据”而非“某次发布的精确字节”（例如源发布对象已过期，或只想找回文字）。

Admin：在发布历史页（内容编辑页 **“发布历史”** 链接进入）点 **“恢复并发布 (restore_and_publish)”**（`POST /cm/content/{id}/versions/{version}/restore_and_publish`，同样必填 `idempotency_key` 表单字段）。
API：`POST /api/v1/content/{id}/restore-and-publish`，请求体 `{"version": <版本号>, "expected_active_id": "<可选>"}` + `Idempotency-Key` 头。

成功标志：`200`，新 Publication 上线；注意这是用历史数据**重新渲染**，不承诺与当年字节完全一致。

与 §4.2 的区别：`revert_live` = 恢复历史发布的保留字节（精确）；`restore_and_publish` = 用历史内容数据重新渲染（尽力）。选错了也无妨——两次操作都产生新 Publication，可互相覆盖。

### 4.4 下线（Unpublish）

前置条件：页面当前已发布；调用者有下线权限。

1. 调用 `POST /api/v1/content/{id}/unpublish`（幂等键**可选**，可带可不带）。
2. 确认线上 URL 已不可访问。

成功标志：`200`；重复调用同样返回 `200` 且不会产生第二个下线事件（天然幂等）。

出错怎么办：`503 PUBLICATION_UNPUBLISH_STAGE_FAILED` → 文件层瞬时故障，稍后重试（可重试，旧页面保持原状）。

---

## 5. 版本历史查看

前置条件：登录 Admin（后台）或持有 `content.view` scope 的 Key（API）。

1. 后台：打开 `/cm/content/{id}/versions` 看版本列表；点某版本进 `/cm/content/{id}/versions/{version}/view` 查看内容，进 `…/diff` 对比差异。
2. 只想恢复为草稿（不上线）：在版本页提交传统的恢复操作 `POST /cm/content/{id}/versions/{version}/revert`（只改草稿数据，线上不动）。
3. API：`GET /api/v1/content/{id}/versions`（列表）、`GET /api/v1/content/{id}/versions/{version}`（单版）、`POST …/revert`（恢复为草稿）；MCP 对应 `get_content_versions` / `revert_to_version`。
4. 发布历史（含每次上线的版本对照）：后台 `GET /cm/content/{id}/publications`（内容编辑页 **“发布历史”** 链接进入），API `GET /api/v1/content/{id}/publications` 与 `GET …/publications/{publication_id}`。

成功标志：能看到“内容版本 ↔ 发布记录”对应关系（哪个版本在哪次发布上线）。

出错怎么办：版本不存在 → `404`，核对版本号是否为整数且存在；无权查看 → `403`，检查 Key 的 scope（见 `AGENT-INTEGRATION.md`）。

---

## 6. slug 迁移申请（改模板 slug，不改页面）

前置条件：你是 **admin** 且 Key 有 `template.edit` scope；明确新 slug（规则：小写，`[a-z0-9][a-z0-9-_]{0,63}`，全站唯一）。注意：模板 slug **不是**页面访问路径，迁移不改变任何页面 URL 与 Publication。

1. 记下模板 ID（`GET /api/v1/templates` 列表中取，或后台 `/cm/templates`）。
2. 调用（仅 API，无后台按钮）：
   ```http
   POST /api/v1/templates/{id}/migrate-slug
   {"new_slug": "new-slug"}
   ```
3. 阅读响应中的受影响外部集成提示，逐一更新调用方配置（模板 slug 变了，调 `page-generation` 时 `template` 字段要用新 slug；旧页面不受影响）。

成功标志：`200`；历史 Template Version 保留原 slug 记录；审计日志记一笔。

出错怎么办：
- `403`：非 admin 或缺 `template.edit`，找管理员操作。
- `422 TEMPLATE_SLUG_INVALID` / slug 冲突：换名再试。
- `400`（新 slug 与当前相同）：检查是否拿错了 ID。

---

## 7. 模板升级预览与任务跟踪

背景：模板 HTML/字段改了只会产生**新模板版本**，已发布页面**保持原样**，必须显式走升级任务逐页重新发布。升级任务逐页创建新 Publication，不直接覆盖线上文件。

### 7.1 升级预览（只看不动）

前置条件：模板已有新版本（后台保存模板后会有提示条，含 **“Upgrade Preview”** 入口）；登录 Admin 或持有相应 scope 的 Key。

1. 后台打开 `GET /cm/templates/{id}/upgrade-preview`（从模板页的 **“Upgrade Preview”** 入口进），或 API `GET /api/v1/templates/{slug}/upgrade-preview`。
2. 阅读预览：标题显示 `某模板 v旧 → v新`；`X of Y pages would republish`（Y 中有 X 页会重发）；逐行显示每页路径、当前模板版本、目标版本与结论（`would republish` / `up to date` / `blocked: validation errors`）。
3. 先处理所有 `blocked: validation errors` 的页面（按第 11 章修字段），再启动任务。预览本身不碰任何线上页面，可反复查看。

### 7.2 启动与跟踪升级任务

1. 在预览页点 **“Start Upgrade Job”**（后台 `POST /cm/templates/{id}/upgrade-start`；API `POST /api/v1/templates/{slug}/upgrade-jobs`，返回 `201` + 任务 ID）。
2. 执行任务：后台 `POST /cm/upgrade-jobs/{jobID}/run`；API `POST /api/v1/templates/upgrade-jobs/{job_id}/run`。执行后看任务页表格：每行有路径、状态（`pending` / `done` / `failed` / `skipped`）、尝试次数与明细（成功显示 Publication ID，失败显示错误）。
3. 跟踪：后台直接看任务页；API 用 `GET /api/v1/templates/upgrade-jobs/{job_id}` 轮询。任务状态为 `running`（进行中）或 `partial`（部分失败）时页上有 **“Retry / resume failed items”** 按钮，点它只重试失败项（已成功的页不会重发）。

成功标志：任务状态 `completed`，全部行为 `done`（`skipped` 为无变化跳过，属正常）。

出错怎么办：`partial` → 修好失败页的问题后点 **“Retry / resume failed items”** 恢复；`UPGRADE_JOB_CONFLICT`（`409`）→ 已有同模板任务在跑，等它结束或先处理它。

---

## 8. 幂等键 `Idempotency-Key` 用法

### 8.1 何时必须带

以下操作**必须**带，否则直接返回 `428 IDEMPOTENCY_KEY_REQUIRED` 且**零副作用**（什么都没写，可放心补键重发）：

- `POST /api/v1/page-generation` 且 `mode=publish`
- 旧版单页发布 `POST /api/v1/content/{id}/publish`
- 批量发布每项（顶层一个键，逐项派生）
- 发布回滚 `POST /api/v1/content/{id}/publications/{publication_id}/rollback`
- 恢复发布 `POST /api/v1/content/{id}/restore-and-publish`、精确回滚 `POST /api/v1/content/{id}/revert-live`
- 改名发布（rename-and-publish）、模板升级触发的发布
- 后台两个表单（`restore_and_publish` / `revert_live`）：必填的是表单字段 `idempotency_key`（效果等同请求头）

以下**不用带**：`preview`（直接忽略）、`draft`/`sandbox`（可选支持，实际不记录）、下线 unpublish（天然幂等，可带可不带）。

### 8.2 正确用法

1. 每次“新操作”生成一个新键（建议 `业务前缀-日期-序号`，如 `news-20260930-001`），放入请求头 `Idempotency-Key: news-20260930-001`。
2. 同一键只配同一请求体。键的作用域是（调用者，方法，路径，键），默认保留 24 小时。
3. 网络超时/连接中断拿不到响应时：用**同一键 + 同一请求体**重发，服务端返回第一次的完整结果（重放，不重复发布）。

### 8.3 冲突怎么办

- `409 IDEMPOTENCY_CONFLICT`（同一键配了不同请求体）：说明这个键被别的请求用过了。**换一个新键**重发；不要反复用同一键试不同内容。
- `409 REQUEST_IN_PROGRESS`（上一请求还在处理，响应带 `Retry-After`）：等指定秒数后，用**同一键 + 同一请求体**重发；不要换键（换键会变成第二次发布）。
- 两个并发请求同键同体：一个执行、一个收到 `REQUEST_IN_PROGRESS`，最终两者拿到同一结果——这是正常的，不要当 bug 修。

---

## 9. Admin 后台操作路径速查

基地址 `/cm`，需先登录。下表只收录代码中实际注册的路由与实际渲染的按钮文字。

| 想做什么 | 路径（菜单→页面→按钮） |
|---|---|
| 新建页面 | `/cm/content` → `/cm/content/new` 选模板卡片 → `/cm/content/new/{templateID}` 填表提交 |
| 改未发布草稿 | `/cm/content/{id}` 直接改（横幅 “Editing unpublished draft.”） |
| 改已发布页面 | `/cm/content/{id}`（横幅 “Editing Draft — Live page unchanged.”，自动进 Fork）→ `/cm/forks` 查看 Fork → `/cm/forks/{id}` 提交合并 → 回内容页发布 |
| 发布 | 内容页 `/cm/content/{id}` 主表单下方 **“发布上线”** 按钮 → `POST /cm/content/{id}/publish`；失败页按 **“重试发布”** 重试 |
| 发布历史与回滚 | 内容页 **“发布历史”** 链接 → `GET /cm/content/{id}/publications` → 行内 **“恢复并发布 (restore_and_publish)”** / **“回滚线上到该发布 (revert_live)”**（均需填 `idempotency_key`） |
| 版本查看/对比/恢复草稿 | `/cm/content/{id}/versions` → `…/versions/{v}/view`、`…/versions/{v}/diff`、`POST …/versions/{v}/revert`（恢复草稿，不上线） |
| 升级预览/任务 | 模板页 **“Upgrade Preview”** → `GET /cm/templates/{id}/upgrade-preview` → **“Start Upgrade Job”**（`POST …/upgrade-start`）→ `POST /cm/upgrade-jobs/{jobID}/run` → **“Retry / resume failed items”** 续跑失败项 |
| 模板管理 | `/cm/templates` 列表 → `/cm/templates/new` 新建 → `/cm/templates/{id}` 编辑（保存即产生新版本，旧页面不动） |
| Fork 管理 | `/cm/forks` → `/cm/forks/new`、`…/{id}` 查看、`…/preview` 预览、`…/merge` 合并、`…/archive` 归档、`…/{id}/delete` 删除 |
| 素材/片段/文件 | `/cm/assets`（上传 `/cm/assets/upload`）、`/cm/snippets`、`/cm/folders`、`/cm/collections`、`POST /cm/upload` |
| 发布辅助 | `POST /cm/content/{id}/regenerate`（重新生成）、`/cm/tools/broken-links`（死链检查） |
| 写作辅助 | `/cm/copilot`（对话式写作，需配置大模型 Key）、`/cm/tools/agent` |
| 系统 | `/cm/theme`、`/cm/config`、`/cm/redirects`、`/cm/messages`、`/cm/api-keys`、`/cm/users`（admin）、`/cm/audit`、`/cm/analytics`、`/cm/webhooks*`、`/cm/imports*` |

---

## 10. MCP / Agent 写作接入

前置条件：MCP 服务已按 `AGENT-INTEGRATION.md` 接入（二进制 `bin/lightcms-mcp`，stdio 传输）；Agent Key 的 scope 按最小权限发放（写作一般给 `content.create` + `content.edit` + `content.view` + `template.view`；**可信自动发布才加 `content.publish`**，且 `sandbox_only=false`；普通/高风险 Agent 用 `sandbox_only=true` 的 Key，只能走沙箱）。

### 10.1 可信自动发布流程（拿得到 `content.publish` 的 Agent）

1. `get_template_schema`（或 `get_template`）取目标模板字段定义。
2. 组织结构化数据；图片先经 `upload_asset` / `upload_asset_from_url` 导入。
3. `preview_content` 预览渲染效果，有阻塞错误先修。
4. 带幂等键发布（经 `publish_content` 或 page-generation publish 语义），返回 Public URL。

### 10.2 人工复核流程（推荐默认）

1. `start_agent_sandbox` 开沙箱。
2. 在沙箱内 `create_content` / `update_content` 写稿 + `preview_content` 预览。
3. `get_fork_diff`（Fork 页差异）提交给人类复核。
4. 人类在后台 `/cm/forks/{id}` 复核后合并（`merge_fork`），再由人类显式发布。

### 10.3 批量流程

先取一次 schema，然后 `bulk_create_content` / `import_markdown` 批量建稿，抽查 `preview`，最后 `publish_multiple` 逐项发布（每项独立幂等）。禁止用大量串行单页调用代替批量接口。

鉴权细节（scope 组合、沙箱 Key 限制、`X-Agent-Session` 透传）见 `AGENT-INTEGRATION.md`。

---

## 11. 常见问题排查

所有 API 错误统一为 `{"error": {"code": "…", "message": "…", "request_id": "…", "retryable": …, "details": […]}}`。报障时请附带 `code` + `request_id`。

### 11.1 `428`——缺了必需的键/版本锁定

| code | 含义 | 处理 |
|---|---|---|
| `IDEMPOTENCY_KEY_REQUIRED` | 写操作缺幂等键（publish/rollback/restore/revert 系列），零副作用 | 按第 8 章补键重发 |
| `TEMPLATE_VERSION_PRECONDITION_REQUIRED` | 外部发布缺 `expected_template_version` | 重新取 schema，把最新 `template_version` 填入再发 |

### 11.2 `409`——冲突（先读响应，再决定重发还是换参）

| code | 含义 | 处理 |
|---|---|---|
| `PATH_CONFLICT` | 路径已存在（`upsert=false`） | 换 slug，或加 `upsert:true`（完整替换，谨慎） |
| `TEMPLATE_VERSION_CHANGED` | 取 schema 后模板升级了 | 重新取 schema，用新版本重发 |
| `TEMPLATE_VERSION_CONFLICT` | 模板并发保存冲突 | 重读模板后重试一次 |
| `TEMPLATE_NOT_ACTIVE` | 模板已下线（deprecated） | 换 active 模板；历史页面不受影响 |
| `CONTENT_VERSION_CONFLICT` | 内容并发修改，带了旧版本 CAS | 重读最新内容后重试 |
| `PUBLICATION_CONFLICT` / `PAGE_PUBLISH_IN_PROGRESS` | 有发布正在进行（可重试，带 `Retry-After`） | 等待后**同键同体**重发 |
| `IDEMPOTENCY_CONFLICT` | 同一键配了不同请求体 | **换新键**重发 |
| `REQUEST_IN_PROGRESS` | 同一键的请求还在跑（带 `Retry-After`） | 等待后**同键同体**重发，不要换键 |
| `AGENT_SANDBOX_REQUIRED` | 沙箱 Key 走了非沙箱模式，或 sandbox 模式无会话 | 切 `mode=sandbox` 并确认沙箱会话；或换非沙箱 Key |
| `UPGRADE_JOB_CONFLICT` | 同模板升级任务已在跑 | 等它结束，不要另起任务 |

### 11.3 `422`——字段没通过校验

顶层 `FIELD_VALIDATION_FAILED`，逐字段原因在 `details` 数组（`field` + `code`）：`FIELD_REQUIRED`（缺必填）、`FIELD_UNKNOWN`（多填了模板没有的字段，顶层未知字段也一样）、`FIELD_NULL_NOT_ALLOWED`（MVP 字段不可为 null）、`FIELD_TYPE_MISMATCH`（如 number 填了文字）、`FIELD_TOO_SHORT` / `FIELD_TOO_LONG`、`FIELD_PATTERN_MISMATCH`、`FIELD_TOO_SMALL` / `FIELD_TOO_LARGE`、`FIELD_INVALID_URL` / `FIELD_PROTOCOL_NOT_ALLOWED`、`FIELD_NOT_IN_ENUM`（select 不在选项内）、`FIELD_INVALID_DATE`（如 `2026-02-30`）、`FIELD_INVALID_ASSET`（图片路径不存在或类型不对）。另有 `PATH_INVALID`（路径非法）、`DATA_TOO_LARGE`（`data` 超 5 MiB）、`TEMPLATE_SCHEMA_INVALID`。处理：一律按 `details` 逐项修正后重发；草稿数据不丢失。

### 11.4 其他状态码

- `401`：未认证——检查 `Authorization: Bearer` 是否有效、Admin 是否登录过期。
- `403`：无权限且**零副作用**——检查 Key 的 scope（publish 需 `content.publish` 等组合，见第 2 章）；不要重复撞同一个请求，换有权限的 Key 或改走 draft。
- `404`：`TEMPLATE_NOT_FOUND` / `CONTENT_NOT_FOUND` / `PUBLICATION_NOT_FOUND` / `TEMPLATE_VERSION_NOT_FOUND` / `UPGRADE_JOB_NOT_FOUND`——核对 slug/ID 是否拼错（模板接口用 slug，升级任务与迁移用 ID，别混）。
- `429 RATE_LIMITED`：触发限流，看 `Retry-After` 等待后重试（可重试）。
- `503 PUBLICATION_STAGE_FAILED`：静态存储瞬时不可用，看 `Retry-After` 重试（可重试，旧页面继续服务）。
- `500`：内部错误，带 `request_id` 联系管理员；不要盲目重试写操作（先确认是否已生成幂等记录）。

---

## 12. 本手册功能均有 E2E 覆盖（证据）

以下行为在 `docs/implementation/test-report.md` 记录的全量回归中被证明可用（1709 通过 / 0 失败；e2e 58 场景 + 6 结论锁；产品包覆盖率 86.9%）：

- 草稿/线上隔离与 Fork 改稿（`TestE2E_ForkDraftReuse`、`TestE2E_ForkVisibility`）
- 模板升级任务与续跑（`TestE2E_TemplateUpgradeJob`、`TestE2E_UpgradeResume`、`TestE2E_TemplateStatusGate`）
- 发布故障注入与恢复（render 失败、提交失败、切换崩溃、旧版切换崩溃、outbox 故障、首次发布崩溃缺口、缺失不可变对象）
- 幂等全表（重放、并发、冲突、终态重试、心跳、租约矩阵、键门禁、创建终态缺口）与校验重放、回滚重试
- 沙箱与安全表（沙箱、scope、XSS、资源 SSRF、输入 hardening）
- 改名发布、精确回滚、保留期 GC、过期清理、outbox 投递、调度器稳定键
- 旧入口兼容（legacy parity、批量发布旧接口）与改名/迁移表（含续跑与旧版在线保持）
