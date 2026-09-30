# LightCMS V3 模板作者指南

> 面向创建与维护模板的人（模板作者、前端、管理员）。读完能：选对字段类型、
> 写对校验约束、用 schema 接口驱动外部表单、理解版本规则与升级流程。
>
> 相关文档（同目录，文件名精确引用）：
>
> - 最终用户操作步骤：`USER-MANUAL.md`
> - 接口参数与返回体细节：`API.md`
> - Agent / MCP 集成：`AGENT-INTEGRATION.md`
> - 本地部署与联调：`DEPLOYMENT-LOCAL.md`
> - 生产部署与运维：`DEPLOYMENT-PRODUCTION.md`
>
> 实现对照：字段契约 `internal/product/templatecontract/`（`schema.go` 类型映射、
> `validate.go` 校验），模板版本 `internal/product/generation/`（升级预览与任务），
> schema 接口 `internal/product/httpapi/templates.go`（`HandleTemplateSchema`），
> Admin 表单 `internal/handlers/admin_publications.go`（`RenderProductField` /
> `ParseAdminFieldData`），路由 `cmd/server/main.go`。
> 全量回归证据见 `docs/implementation/test-report.md`（1709 通过 / 0 失败）。

---

## 1. 字段类型速查

共 11 种。`url`、`number`、`boolean` 为 V3 新增，三端（models、Admin 表单、REST/MCP 序列化、Schema、校验、预览）同步实现，不只是下拉框多三项。`group` / `repeatable` 已延期，当前不可用。

| 类型 | 存什么（JSON） | Admin 表单控件 | 空值形态 | 渲染值 | 核心校验 |
|---|---|---|---|---|---|
| text | string | 单行输入 | `""` | 转义文本 | 长度/正则 |
| textarea | string | 多行文本 | `""` | 转义文本 | 长度/正则 |
| richtext | string | 站内富文本编辑器 | `""` | 按脚本策略消毒后的可信 HTML | 脚本策略 |
| markdown | string | 多行文本/编辑器 | `""` | 渲染并消毒后的 HTML | 脚本策略 |
| rawhtml | string | 代码文本框 | `""` | 按权限/策略控制的 HTML | 管理员权限/脚本策略 |
| date | `YYYY-MM-DD` 字符串 | 日期选择器 | 可选时 key 缺失 | 日期字符串/时间辅助 | JSON Schema `format: date` + 真实日历校验 |
| image | 资源路径 string | 图片选择器（文件上传） | `""` | 规范资源 URL/路径 | 资源存在 + 类型 |
| select | string | 下拉框 | 可选时 `""` | 转义文本 | 枚举（选项清单） |
| url（新增） | string | URL 输入 | 可选时 `""` | 转义 URL | 协议白名单 |
| number（新增） | JSON number | 数字输入 | 可选时 key 缺失 | float64（兼容整数） | min/max |
| boolean（新增） | JSON boolean | 复选框 | 可选未填时 key 缺失 | bool | 类型/默认值 |

逐类型说明：

1. **text / textarea**：纯文本，渲染时转义。用 `MinLength` / `MaxLength` / `Pattern` 约束。
2. **richtext**：沿用站内富文本编辑器，输出按脚本策略消毒（`<script>`、`<iframe>`、`<form>`、事件属性、`javascript:` 等按策略剥离）。
3. **markdown**：GFM 渲染后同样过脚本策略消毒器。想在正文里放表格、任务列表用它。
4. **rawhtml**：直接写 HTML，唯一受作者权限与脚本策略双重管制的高危类型。只给可信管理员模板用。
5. **date**：必须是 ISO 8601 日历日期 `YYYY-MM-DD`，映射 JSON Schema `format: date`。`2026-02-30` 这类不存在的日期会被拒绝（`FIELD_INVALID_DATE`）。注意它不是 RFC 3339 时间戳，不要带时分秒。
6. **image**：填站内资源路径。Admin 端是文件上传控件并回显当前图；API 端填已上传资源的路径字符串，服务端校验资源存在与类型（不存在报 `FIELD_INVALID_ASSET`）。先上传后引用。
7. **select**：选项来自字段定义的 `Options`（逗号分隔）。提交值必须与某一选项**完全一致**，否则 `FIELD_NOT_IN_ENUM`。
8. **url**：完整 URL。默认只允许 http/https，模板可用 `AllowedProtocols` 另行限定；非法格式报 `FIELD_INVALID_URL`，协议越界报 `FIELD_PROTOCOL_NOT_ALLOWED`。
9. **number**：JSON 数字。Admin 表单提交的是字符串，服务端转 float64；转不过去按类型错报给校验器。用 `Min` / `Max` 限范围（越界报 `FIELD_TOO_SMALL` / `FIELD_TOO_LARGE`）。
10. **boolean**：JSON 布尔。**不支持 null**。数据库区分“key 缺失”与“显式 false”：可选 boolean 不勾选则不写 key；模板设 `Default: "true"` 时新建自动物化为 `true`。Admin 用“隐藏域 + 复选框”提交，勾选为 `on/true/1`，其余为 false。

字段名规则：`[a-z][a-z0-9_]{0,63}`，且不得使用系统保留名：`id`、`template_id`、
`template_version`、`full_path`、`published`、`published_at`、`created_at`、
`updated_at`、`publication_id`、`public_url`（这些由渲染器注入，见 §5）。

---

## 2. 验证规则

### 2.1 三层校验

请求处理顺序：模板存在 → 模板 active → 权限/scope/沙箱 → 路径规范化 → **严格字段校验** → 选 Content/Fork 目标 → 写库 → 渲染 →（发布模式再进发布编排）。权限与模式合法性在**任何持久化副作用之前**完成；发布模式缺权限直接 `403` 且零副作用，不会自动降级存草稿。

### 2.2 必填、默认、空值

- `Required: true` 的字段缺失 → `FIELD_REQUIRED`，阻止创建与发布。
- `Default` 非空时，缺失字段自动填默认值，并在 `warnings` 中以 `FIELD_DEFAULT_APPLIED` 记录实际生效的值。
- MVP 所有字段**不可为 null**：`data` 整体为 `null` 或某字段为 `null` → `FIELD_NULL_NOT_ALLOWED`。
- 空字符串是明确值，照常接受必填/长度校验（不要用 `""` 表示“没填”）。
- 未知字段（`data` 里多出模板没有的 key，或请求顶层多出未知字段）→ `422`（`FIELD_UNKNOWN`），不静默丢弃。
- `boolean` 可选未填 = key 缺失；`number` / `date` 可选未填 = key 缺失；其余字符串类型可选未填 = `""`。
- `page-generation` 的 `upsert` 命中已有页时，`data` 是**完整替换**，不做合并；缺失字段按必填/默认处理，不保留旧值。局部更新请用旧版 Content Update 接口（遵守改稿不直接上线规则，见 `USER-MANUAL.md` 第 3 章）。

### 2.3 约束清单（`FieldValidation`）

| 约束 | 适用类型 | 越界错误码 |
|---|---|---|
| `MinLength` / `MaxLength` | text、textarea、richtext、markdown、rawhtml | `FIELD_TOO_SHORT` / `FIELD_TOO_LONG` |
| `Pattern`（正则） | 文本类 | `FIELD_PATTERN_MISMATCH`（正则本身非法由模板作者负责，服务端报 `FIELD_PATTERN_INVALID`） |
| `Min` / `Max` | number | `FIELD_TOO_SMALL` / `FIELD_TOO_LARGE` |
| `AllowedProtocols` | url | `FIELD_PROTOCOL_NOT_ALLOWED` |
| `MaxItems` | 预留（当前无 repeatable 类型，暂不生效） | — |
| 选项清单（`Options`） | select | `FIELD_NOT_IN_ENUM` |
| 日历合法性 | date | `FIELD_INVALID_DATE` |
| 资源存在与类型 | image | `FIELD_INVALID_ASSET` |

### 2.4 错误与警告

- **error** 阻止创建与发布（如必填、类型、安全、资源校验失败必须归为 error，不许降级）。
- **warning** 不阻止：允许存草稿也允许发布，但必须在响应 `warnings` 与 Admin 表单中展示。当前唯一的 warning 是 `FIELD_DEFAULT_APPLIED`（默认值已应用）。
- 没有“warnings 也阻止发布”的开关；不要指望靠 warning 卡流程。
- Admin 表单与 REST/发布共用**同一校验器**（`templatecontract.ValidateData`），两边永远不会出现“后台能过、接口不过”的分歧。表单错误以内联文字显示在字段下方。

---

## 3. JSON Schema 获取与 ETag 用法

### 3.1 接口

```http
GET /api/v1/templates/{slug}/schema
```

前置条件：已认证；Key 如带 scope 需含 `template.view`。注意用模板 **slug**（不是 ID）。

响应（`200`，附带 `ETag: "template-version-N"`）：

```json
{
  "template": "financial-news",
  "template_version": 3,
  "fields": [
    {"name": "headline", "type": "text", "required": true,
     "description": "…", "example": "…"}
  ],
  "json_schema": {
    "$schema": "https://json-schema.org/draft/2020-12/schema",
    "type": "object",
    "required": ["headline", "body"],
    "properties": {},
    "additionalProperties": false
  }
}
```

保证：`template_version`、ETag、JSON Schema 来自**同一个不可变 TemplateVersion 对象**（纯变换、可确定、不写库）；`additionalProperties=false`（未知字段一律拒绝）。MCP 等价工具：`get_template_schema`。

### 3.2 ETag 与 `expected_template_version` 的配合

1. 第一次拉 schema，存下 `template_version` 与 `ETag`。
2. 外部表单可用 ETag 做缓存版本标记：ETag 变了 = 模板升级了，重新拉 schema 再渲染表单。
3. 发 `page-generation`（尤其是 `mode=publish`）时，把当时看到的版本填入 `expected_template_version`：服务端在拿发布锁后核对，版本已变返回 `409 TEMPLATE_VERSION_CHANGED`（此时重拉 schema、按新字段修正后重发）；外部发布缺该字段返回 `428 TEMPLATE_VERSION_PRECONDITION_REQUIRED`。
4. 不允许客户端指定任意历史版本发布；该字段只是“版本未变”的断言，不是版本选择器。

---

## 4. 版本与升级

### 4.1 什么会产生新版本

以下任一变化即新版本：`slug`、`category`、`status`、完整字段契约（含说明、示例、默认值、选项、校验约束）、`HTMLLayout`、`ScriptPolicy`、系统变量契约。**仅改后台显示名或缩略图不产生新版本**。

版本判定只看 `ContractHash`（全部契约字段的规范 JSON SHA-256）：相同 hash 的重复保存**不**创建新版本。`RenderHash` 只用于渲染缓存对比，不能代替版本判定。

### 4.2 不可变规则

- 版本一旦创建不可修改；删除逻辑模板不删除历史版本（deprecated 模板的历史版本仍可用于回滚）。
- 每次 Publication 必须引用 `template_version_id`，保证可追溯。
- 并发保存冲突返回 `409 TEMPLATE_VERSION_CONFLICT`（服务端重试一次后仍冲突才报，调用方重读后重试即可）。
- 状态机：`draft`（仅作者可见，不可用于生成）→ `active`（可用）→ `deprecated`（历史可追溯，不可再选）。已发布页面不受模板下线影响，但新发布会报 `409 TEMPLATE_NOT_ACTIVE`。

### 4.3 更新模板的正确姿势

```text
更新模板 → 自动创建新 Template Version → 已有线上页面保持不动 →
跑 Upgrade Preview → 启动显式 Upgrade Job 逐页重新发布
```

“改模板 HTML 自动全站重生成”的旧默认行为已取消。保存模板后 Admin 会弹出版本提示条（含 **“Upgrade Preview”** 入口）：

1. 进预览确认 `X of Y` 重发清单，修掉 `blocked: validation errors` 的页面。
2. 点 **“Start Upgrade Job”** 建任务（`201`），执行任务（`POST …/run`），`partial` 时用 **“Retry / resume failed items”** 只续跑失败项。
3. 升级任务逐页调用发布服务、每页一个新 Publication；绝不直接覆盖线上文件。

完整操作步骤见 `USER-MANUAL.md` 第 7 章。

### 4.4 slug 变更

普通模板更新**不允许**改 slug。业务必须改时走管理员专用迁移
`POST /api/v1/templates/{id}/migrate-slug`（admin + `template.edit`，不改历史版本、不改任何页面 URL、不自动建 redirect，调用方自行更新集成配置）。步骤见 `USER-MANUAL.md` 第 6 章。

---

## 5. 模板 HTML 与系统变量

模板 HTML 继续使用 Go `html/template`。业务字段直接引用（如 `{{.headline}}`），
系统字段由渲染上下文注入（不要写入用户 `data`，也不要在字段定义里重名）：

```text
title slug full_path published_at public_url content_id
template_slug template_version publication_id
```

示例：

```html
<h1>{{.headline}}</h1>
<article>{{.body}}</article>
<p><a href="{{.public_url}}">原文链接</a></p>
```

脚本策略（`ScriptPolicy`）随模板版本快照：richtext/markdown 输出固定过消毒器；
rawhtml 是否放行取决于策略与作者权限。策略变更本身即新版本（见 §4.1），
且精确回滚依赖该快照——不要在版本外偷换策略。
