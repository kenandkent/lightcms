# LightCMS 模板化页面生成与原子静态发布系统最终设计方案

> 文档状态：正式发布，可用于第三方技术评审、研发拆分、实施与验收  
> 文档版本：Final  
> 日期：2026-09-29  
> LightCMS 基线：`v7.2.2`  
> 基线 Commit：`c1165be1327dc605bd196cd4dff4ebe35bfaa6f4`  
> 适用项目：新闻、财经、Web3/Crypto、广告、软文、公告、活动页等结构化 HTML 页面生成与发布平台

---

# 1. 文档目的

本文定义一个基于 LightCMS v7 的模板化页面生成与原子静态发布平台。

系统面向三类使用者：

1. 运营人员通过 Admin UI 选择模板、填写字段、预览和发布页面；
2. 第三方系统通过 REST API 自动生成、预览和发布页面；
3. AI Agent 通过现有 MCP 或 REST API 创建内容，并根据权限进入自动发布或人工审核流程。

本文是一份完整、自洽的实施设计，研发团队可以直接依据本文完成技术拆分、开发、测试、部署和验收。

---

# 2. 产品目标

核心业务链路：

```text
Template Contract
        +
Structured Page Data
        ↓
Strict Validation
        ↓
Draft / Fork
        ↓
Preview
        ↓
Stage Publication
        ↓
Verify
        ↓
Atomic Activate
        ↓
Public URL
```

系统必须满足：

1. 管理员可以创建新闻、广告、软文、公告等模板；
2. 模板定义稳定 machine slug、字段说明、验证规则和 HTML；
3. 运营人员不需要编辑 HTML，只填写结构化字段；
4. 页面支持草稿、预览、发布、下线、更新和回滚；
5. 已发布页面的后续编辑不能在保存时直接改变线上内容；
6. 模板修改不能隐式改变历史已发布页面；
7. 发布失败不得损坏或替换当前线上版本；
8. 发布成功后返回稳定、绝对、可分享的 Public URL；
9. 第三方系统可以通过 API 一步生成草稿或生成并发布；
10. AI Agent 可以通过现有 MCP 完成模板查询、素材导入、内容创建、预览、审核和发布；
11. 所有变更保留版本、来源、操作者、Agent Session 和发布审计；
12. 系统继续吸收 LightCMS 上游安全修复和新能力。

---

# 3. 非目标

MVP 不建设：

- Webflow/GrapesJS 类自由拖拽页面设计器；
- 新富文本框架；
- 新 CMS Core；
- 新 Page Backend；
- 新 Agent Runtime；
- 新 MCP Server；
- 新 OAuth Server；
- 新 RBAC；
- 新 Audit Engine；
- 新 Webhook Engine；
- 新 Approval Workflow；
- 新 Content Fork；
- 新通用 Versioning；
- 多租户计费；
- 广告投放系统；
- A/B Test；
- AI 文案或图片模型本身；
- 通用 Workflow Engine。

---

# 4. 强制架构原则

## 4.1 复用优先级

```text
CONFIGURE
>
REUSE
>
WRAP
>
EXTEND
>
MODIFY CORE
>
REPLACE
```

## 4.2 Page 不建立平行领域

```text
Product Page = LightCMS Content
Product Draft Workspace = LightCMS Fork
Product Page Version = LightCMS Content Version
```

禁止新增：

```text
pages
page_versions
page_auth
page_audit
page_webhooks
```

## 4.3 REST 与 MCP 共用业务能力

```text
REST Facade ─────────────┐
                        ├─ Shared Product Services ─ LightCMS Core
Existing MCP/API Client ┘
```

MCP 不得直接访问 MongoDB。

## 4.4 Renderer 复用，发布事务扩展

现有 LightCMS Renderer、Markdown、Snippet、Theme、Wikilink、TOC 和 HTML 处理逻辑继续复用。

现有 Publish 的数据库状态切换和文件覆盖方式不能直接作为产品级原子发布事务，必须通过 Publication Orchestrator 扩展。

## 4.5 已发布页面通过 Fork 形成新草稿

普通 Content 仅用于：

- 尚未发布的新页面；
- 当前 live 主记录。

已发布页面的后续编辑必须进入 Fork。保存 Fork 不生成或覆盖 live 静态文件。

Fork merge 必须使用产品级 draft merge 路径：合并数据和创建 Content Version，但不得触发已发布 Content 的静态重生成。Merge 完成后仍需显式 Publish 才能替换 active Publication。

## 4.6 模板版本不可变

发布必须引用准确的 Template Version。修改模板只能产生新版本，不能改变已存在 Publication 的模板快照。

## 4.7 发布采用 Stage / Verify / Activate

发布前先生成并验证候选静态输出，验证成功后才原子激活。任何失败均保持旧 live 页面不变。

这里的“原子”只指同一文件系统内 canonical public file 的 atomic rename/cutover。MongoDB 与 Filesystem 之间不构成跨资源原子事务，整体发布协议是：

```text
immutable stage
+ verified candidate
+ atomic canonical file cutover
+ transactional metadata switch
+ compensation
+ crash recovery scanner
```

正式术语统一为：

```text
Atomic public-file cutover
+ Compensating publication Saga
```

文档、代码注释和对外材料不得宣称存在 “MongoDB + Filesystem distributed atomic transaction”。

## 4.8 新模型必须小而明确

本方案仅增加解决产品语义所必需的扩展：

- Template metadata；
- Template Version；
- Publication；
- Idempotency Record；
- 可选 Static Storage metadata。

## 4.9 单体模块化部署

本项目最终交付物是一个完整服务，不采用微服务拆分。

部署单元固定为：

```text
一个 Go Backend 进程
+
一套由该进程提供的 Admin Web UI
+
一个 MongoDB 数据库
+
一个静态文件存储后端（默认本地持久卷，可配置 R2/S3）
```

本文中的 `TemplateContractService`、`PublicationService`、`GenerationService`、`StaticPageStore` 等名称均表示同一 Go 进程内的 package、struct 或 interface，不是独立部署服务，不拥有独立数据库，不通过网络互相调用。

统一入口：

```text
LightCMS Server Process
├── Admin Web UI
├── REST API
├── MCP HTTP / stdio integration backend
├── Scheduler
├── Import Jobs
├── Publication Worker / Recovery Scanner
└── Static File Serving
```

禁止交付：

- 多个独立后端进程；
- 独立 Publication Server；
- 独立 Generation Server；
- 独立 Admin SPA 服务；
- REST 与 MCP 各自维护一套业务实现；
- 每个模块独立数据库；
- 为模块通信引入 Kafka、RabbitMQ 或内部 HTTP/RPC。

---

# 5. LightCMS v7 基线

当前基线已实际验证：

```text
UPSTREAM_COMMIT=c1165be1327dc605bd196cd4dff4ebe35bfaa6f4
UPSTREAM_VERSION=7.2.2

go test ./...         PASS
go vet ./...          PASS
build server          PASS
build MCP             PASS
build CLI             PASS
```

现有能力直接复用：

| 能力 | 策略 |
|---|---|
| Template CRUD | REUSE |
| Typed Fields | REUSE/EXTEND |
| HTML Template | REUSE |
| Richtext / Markdown | REUSE |
| Content CRUD | REUSE |
| Content Version | REUSE |
| Preview | REUSE |
| Static Renderer | REUSE |
| Bulk / Upsert | REUSE |
| Asset / Asset from URL | REUSE/HARDEN |
| REST `/api/v1` | REUSE |
| MCP | REUSE |
| OAuth | REUSE |
| API Key / scopes | REUSE |
| Agent Sandbox / Fork | REUSE |
| Agent Ledger / Rollback | REUSE |
| Provenance | REUSE |
| Approval | REUSE |
| Webhook | REUSE |
| Audit | REUSE |
| Rate Limit | REUSE |
| Analytics | REUSE |
| Cloudflare Purge | REUSE |
| Import Pipeline | REUSE |

---

# 6. 总体架构

```text
┌──────────────────────────────────────────────────────────────┐
│                         Consumers                            │
│  Admin UI        Third-party REST        AI Agent / MCP      │
└──────────┬────────────────┬───────────────────┬───────────────┘
           │                │                   │
           ▼                ▼                   ▼
┌──────────────────────────────────────────────────────────────┐
│ Existing Auth / OAuth / API Key / RBAC / Scopes / Sandbox    │
└────────────────────────────┬─────────────────────────────────┘
                             │
          ┌──────────────────┴──────────────────┐
          ▼                                     ▼
┌──────────────────────┐              ┌────────────────────────┐
│ Product HTTP Facade  │              │ Existing REST / MCP    │
│ - template schema    │              │ - content / template   │
│ - page generation    │              │ - asset / fork / bulk  │
│ - publication result │              │ - approval / audit     │
└──────────┬───────────┘              └────────────┬───────────┘
           │                                       │
           └──────────────────┬────────────────────┘
                              ▼
┌──────────────────────────────────────────────────────────────┐
│                      LightCMS Core                           │
│ Template / Content / Version / Fork / Asset / Audit          │
└────────────────────────────┬─────────────────────────────────┘
                             │
           ┌─────────────────┴─────────────────┐
           ▼                                   ▼
┌────────────────────────┐           ┌─────────────────────────┐
│ Template Contract      │           │ Existing Renderer       │
│ + Template Version     │           │ HTML/Markdown/Snippets  │
└────────────┬───────────┘           └────────────┬────────────┘
             └──────────────────┬─────────────────┘
                                ▼
┌──────────────────────────────────────────────────────────────┐
│ Publication Orchestrator                                    │
│ validate → render → stage → verify → activate → audit       │
└────────────────────────────┬─────────────────────────────────┘
                             │
           ┌─────────────────┴──────────────────┐
           ▼                                    ▼
┌────────────────────────┐            ┌────────────────────────┐
│ Atomic Filesystem      │            │ Optional R2 / S3       │
└────────────┬───────────┘            └────────────┬───────────┘
             └──────────────────┬──────────────────┘
                                ▼
                     CDN / Public URL
```

---

# 7. 产品概念映射

| 产品概念 | 实现 |
|---|---|
| Template | LightCMS Template |
| Template Alias | 强化后的 `Template.Slug` |
| Template Version | 新增不可变 `template_versions` |
| Field Schema | 扩展后的 `TemplateField` |
| JSON Schema | Template Schema Adapter 输出 |
| Page | LightCMS Content |
| New Page Draft | Unpublished Content |
| Published Page Draft | Fork Content |
| Page Version | Content Version |
| Publication | 新增最小 `content_publications` |
| Preview | Existing Content/Fork Preview |
| Asset | Existing Asset |
| Agent Workspace | Existing Agent Sandbox/Fork |
| Audit | Existing Audit + publication action |
| External Auth | Existing API Key/OAuth |
| Public URL | Base URL + canonical full path |

---

# 8. Template Product Contract

## 8.1 Template Slug

现有 `Template.Slug` 作为唯一 machine identifier，不新增重复 Alias 字段。

约束：

```text
lowercase
[a-z0-9][a-z0-9-_]{0,63}
unique
indexed
immutable by default
```

示例：

```text
financial-news
crypto-news
product-advertorial
press-release
announcement
```

普通 Template Update 不允许修改 slug。

如业务必须修改，提供管理员专用 migration：

```text
POST /api/v1/templates/{id}/migrate-slug
```

MVP 中迁移操作必须：

- 校验新 slug 唯一；
- 记录 Audit；
- 不改变历史 Template Version；
- 返回受影响的外部集成提示；
- 不自动创建 URL redirect，因为 Template slug 不是 Public Page path。

## 8.2 Category

继续复用 `Template.Category`。

推荐值：

```text
news
advertisement
advertorial
press-release
announcement
landing-page
custom
```

允许 `custom`，不在数据库层硬编码业务枚举；Product UI 提供推荐选项。

## 8.3 Template Status

增加轻量状态：

```text
draft
active
deprecated
```

规则：

- `draft`：仅管理员/模板设计者可见，不允许外部生成；
- `active`：可用于 Page Generation；
- `deprecated`：历史页面继续可追溯，不允许新页面选择。

## 8.4 TemplateField 扩展

保留现有字段并增加 optional metadata：

`FieldValidation` 定义在 `internal/models`，作为现有 `models.TemplateField` 的字段。`internal/product/templatecontract` 可以依赖 `models`，`models` 不得反向依赖 `templatecontract`。

```go
type TemplateField struct {
    Name        string
    Label       string
    Type        string
    Required    bool
    Placeholder string
    Options     string
    Default     string

    Description string
    Example     string
    Validation  FieldValidation
}
```

```go
type FieldValidation struct {
    MinLength       *int
    MaxLength       *int
    Pattern         string
    Min             *float64
    Max             *float64
    AllowedProtocols []string
    MaxItems        *int
}
```

MVP 支持类型：

```text
text
textarea
richtext
markdown
rawhtml
date
image
select
url
number
boolean
```

`group` 和 `repeatable` 延后到 Phase 2，除非首批模板明确需要。

字段类型实现契约：

| Type | JSON/BSON | Admin Input | Empty Value | Renderer Value | 核心验证 |
|---|---|---|---|---|---|
| text | string | text input | `""` | escaped string | length/pattern |
| textarea | string | textarea | `""` | escaped string | length/pattern |
| richtext | string | existing rich editor | `""` | sanitized trusted HTML | script policy |
| markdown | string | textarea/editor | `""` | rendered sanitized HTML | script policy |
| rawhtml | string | code textarea | `""` | policy-controlled HTML | admin permission/script policy |
| date | ISO 8601 calendar date string `YYYY-MM-DD` | date input | missing when optional | string/time helper | JSON Schema `format: date` + strict calendar validation |
| image | string asset path | asset picker | `""` | canonical asset URL/path | asset exists/type |
| select | string | select | `""` when optional | escaped string | options enum |
| url | string | URL input | `""` when optional | escaped URL | allowed protocols |
| number | JSON number | number input | missing when optional | float64/int-compatible | min/max |
| boolean | JSON boolean | checkbox | missing when optional | bool | type/default |

`url`、`number`、`boolean` 是明确的 MVP 新增字段类型，必须同步实现 models、Admin form parse/render、REST/MCP serialization、Schema Adapter、Validator、Preview 和测试，不得只更新字段下拉框。

`date` 不称为 RFC 3339 date subset；它是 ISO 8601 calendar date，并映射 JSON Schema `format: date`。必须拒绝不存在的日历日期，例如 `2026-02-30`。

Boolean 不支持 nullable。数据库必须区分“字段缺失”和“显式 false”；Admin checkbox 提交逻辑先应用 Template Default，再写入明确 boolean。Optional boolean 未填写时不写 key；如果 Template 定义 `default=true`，创建时物化为 `true`。

## 8.5 字段命名

```text
[a-z][a-z0-9_]{0,63}
```

禁止与系统保留字段冲突：

```text
id
template_id
template_version
full_path
published
published_at
created_at
updated_at
publication_id
public_url
```

## 8.6 HTML 模板变量

继续使用 Go `html/template`。

业务字段：

```html
<h1>{{.headline}}</h1>
<article>{{.body}}</article>
```

系统字段：

```text
title
slug
full_path
published_at
public_url
content_id
template_slug
template_version
publication_id
```

系统字段由 Renderer Context 注入，不写入用户 `data`。

---

# 9. Template Version

## 9.1 目的

Template Version 用于保证：

- 已发布页面可以追溯准确模板；
- 模板修改不会隐式改变旧 Publication；
- 回滚可以恢复 Content 与 Template 的准确组合；
- 批量升级模板前可以预览影响。

## 9.2 数据模型

Collection：

```text
template_versions
```

```json
{
  "id": "...",
  "template_id": "...",
  "version": 3,
  "slug": "financial-news",
  "name": "Financial News",
  "category": "news",
  "fields": [],
  "html_layout": "...",
  "contract_hash": "sha256:...",
  "render_hash": "sha256:...",
  "created_by": "...",
  "created_at": "..."
}
```

## 9.3 创建规则

以下变更产生新版本：`slug`、`category`、`status`、完整 Fields 契约（含说明、示例、默认值、选项与验证约束）、HTMLLayout、ScriptPolicy 和系统变量契约。仅修改后台显示名称或缩略图不产生新版本；这些显示属性不进入 TemplateVersion 的对外契约。

版本判定只比较 §9.6 定义的 `ContractHash`。`RenderHash` 仅用于判断 HTML 渲染输入是否变化；字段说明只改变 ContractHash，也必须产生新版本。

## 9.4 不可变规则

- Template Version 一经创建不可修改；
- 删除逻辑 Template 不删除历史版本；
- deprecated Template 的历史版本仍可用于回滚；
- Publication 必须引用 `template_version_id`。

## 9.5 模板更新行为

取消“修改 Template HTML 后自动重新生成所有已发布页面”的产品默认行为。

改为：

```text
Update Template
↓
Create Template Version
↓
Existing Publications unchanged
↓
Admin may run Upgrade Preview
↓
Explicit bulk republish to new Template Version
```

现有自动 regeneration 能力保留为内部工具，但 Product UI 默认不触发。

## 9.6 TemplateVersion 精确模型

```go
type TemplateVersion struct {
    ID             primitive.ObjectID    `bson:"_id,omitempty" json:"id"`
    TemplateID     primitive.ObjectID    `bson:"template_id" json:"template_id"`
    Version        int64                 `bson:"version" json:"version"`
    Slug           string                `bson:"slug" json:"slug"`
    Name           string                `bson:"name" json:"name"`
    Category       string                `bson:"category" json:"category"`
    Status         string                `bson:"status" json:"status"`
    Fields         []models.TemplateField `bson:"fields" json:"fields"`
    HTMLLayout     string                `bson:"html_layout" json:"html_layout"`
    ScriptPolicy   string                `bson:"script_policy,omitempty" json:"script_policy,omitempty"`
    ContractHash   string                `bson:"contract_hash" json:"contract_hash"`
    RenderHash     string                `bson:"render_hash" json:"render_hash"`
    CreatedBy      *primitive.ObjectID   `bson:"created_by,omitempty" json:"created_by,omitempty"`
    CreatedByEmail string                `bson:"created_by_email,omitempty" json:"created_by_email,omitempty"`
    CreatedAt      time.Time             `bson:"created_at" json:"created_at"`
}
```

索引：

```text
UNIQUE(template_id, version)
INDEX(template_id, created_at DESC)
INDEX(slug, version DESC)
```

`ContractHash` 对所有会改变 Template Version 外部契约的字段计算 SHA-256：slug、category、status、完整 Fields metadata/validation/default/options、HTMLLayout、ScriptPolicy 和系统变量契约。`RenderHash` 只对影响 HTML 字节输出的字段、HTMLLayout、ScriptPolicy 和渲染依赖声明计算 SHA-256。

两者均使用 RFC 8785 风格 canonical JSON：UTF-8、对象 key 排序、无无意义空白、数组保持定义顺序、数字使用规范表示。Template Version 是否变化以 `ContractHash` 为准；相同 ContractHash 的重复保存不创建新版本。RenderHash 用于渲染缓存与对比，不能代替版本契约 hash。

版本号不能使用 `Count()+1`。Template 主记录增加：

```go
CurrentVersion int64 `bson:"current_version" json:"current_version"`
```

创建新版本必须在 Mongo transaction 内执行：

```text
read expected current_version
↓
compare ContractHash
↓ changed
$inc templates.current_version
↓
insert template_versions(template_id, new_version)
↓
update current Template fields/html
```

并发冲突重试一次；再次冲突返回 `409 TEMPLATE_VERSION_CONFLICT`。

## 9.7 Template 更新入口统一

所有入口统一调用进程内 `TemplateContractService.Update`：

| 入口 | 新行为 |
|---|---|
| Admin Template Update | 创建/复用 Template Version，不自动 regenerate live |
| REST Template Update | 同上 |
| MCP `update_template` | 通过 REST/API client 调用同一逻辑 |
| CLI Template Update | 调用同一 REST endpoint |
| Migration/Seed | 显式创建 Version 1 |

旧 `regenerateContentByTemplate` 不再由普通 Template Update 调用。显式 Template Upgrade Job 必须逐页调用 `PublicationService.Publish`，为每页创建新 Publication，不得直接调用 `GenerateStaticPage` 覆盖 canonical 文件。

---

# 10. JSON Schema

## 10.1 Schema Adapter

```go
type TemplateSchemaAdapter interface {
    ToJSONSchema(v TemplateVersion) ([]byte, error)
}
```

Schema endpoint 必须先解析并固定不可变 TemplateVersion，再调用 Adapter。响应中的 `template_version`、ETag 和 JSON Schema 必须来自同一版本对象；不得使用 mutable Template 主记录生成历史版本的 Schema。

要求：

- pure transform；
- deterministic；
- 不写数据库；
- 不复制 Template Model；
- 同一 Template Version 始终输出相同 schema；
- 单元测试覆盖所有字段类型。

## 10.2 Endpoint

```http
GET /api/v1/templates/{slug}/schema
```

响应：

```json
{
  "template": "financial-news",
  "template_version": 3,
  "fields": [
    {
      "name": "headline",
      "type": "text",
      "required": true,
      "description": "Primary headline displayed at the top of the article",
      "example": "Bitcoin Rallies as Institutional Demand Returns"
    }
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

## 10.3 Additional Properties

Product Facade 默认：

```text
additionalProperties=false
```

原始 `/api/v1/content` 为保持上游兼容，继续允许现有行为。

---

# 11. Strict Field Validation

## 11.1 验证层级

```text
Request shape validation
↓
Template schema validation
↓
Business/path validation
↓
Render validation
↓
Publication validation
```

## 11.2 字段验证

至少验证：

- required；
- type；
- unknown field；
- select enum；
- min/max length；
- regex；
- numeric min/max；
- URL protocol；
- asset reference；
- date format；
- maximum payload size。

## 11.3 错误与警告

```text
error    阻止 create/publish
warning  允许保存 draft，也允许 publish；必须在响应与 Admin UI 中展示
```

MVP 固定为只有 `errors` 阻止发布，不提供“warnings 也阻止发布”的配置。安全、必填字段、类型、模板解析和资源校验失败必须归类为 error，不能降为 warning。

示例：

```json
{
  "valid": false,
  "errors": [
    {
      "code": "FIELD_REQUIRED",
      "field": "headline",
      "message": "headline is required"
    }
  ],
  "warnings": []
}
```

## 11.4 兼容策略

- Product Facade 严格验证；
- Product Admin UI 使用相同验证器；
- Product Publish 使用相同验证器；
- 原始 Content API 不做破坏性全局强制；
- 后续可通过配置让原始 API 启用 strict mode。

---

# 12. Page 与状态模型

## 12.1 Page

Page 继续映射 `models.Content`。

Product DTO：

```json
{
  "id": "mongo-content-id",
  "template": "financial-news",
  "template_version": 3,
  "title": "Bitcoin Market Update",
  "slug": "bitcoin-market-update",
  "folder_path": "/news",
  "full_path": "/news/bitcoin-market-update",
  "data": {},
  "state": "draft",
  "active_publication_id": null,
  "public_url": null,
  "created_at": "...",
  "updated_at": "..."
}
```

## 12.2 产品状态

```text
draft
in_review
published
unpublished
deleted
```

映射：

| 产品状态 | LightCMS 状态 |
|---|---|
| draft | unpublished Content 或 Fork page |
| in_review | active Fork / pending Approval |
| published | Content 有 active Publication |
| unpublished | Content 存在但无 active Publication |
| deleted | soft-deleted Content |

## 12.3 已发布页面编辑

```text
Published Content
↓ Edit
Create/reuse active Fork
↓
Edit Fork Copy
↓
Preview Fork
↓
Diff / Review
↓
Merge
↓
Merge Draft State Without Live Regeneration
↓
Explicit Publish
```

保存 Fork 不修改：

- live Content static output；
- active Publication；
- CDN 内容；
- public URL response body。

现有 Fork merge 如果最终调用已发布 Content 的普通 `UpdateContent`，可能触发 LightCMS 当前的静态重生成副作用。产品实现必须提供明确的 suppress-live-regeneration 路径：

```text
Merge Fork data into main Content
↓
Create Content Version
↓
Preserve active Publication and canonical static output
↓
Mark main Content as having unpublished changes
↓
Wait for explicit Publish
```

不得通过临时设置 `published=false` 再改回的方式规避，因为这会引入静态删除、Webhook、Audit 和状态竞争副作用。

## 12.4 新页面

新页面可以直接以 unpublished Content 保存。

若使用 `sandbox_only` Key，则新页面必须创建在 Agent Sandbox/Fork 中，遵守现有 Governance。

## 12.5 Content Mutation Policy

Draft/Live 隔离是全局规则，不只适用于 Generation Facade。所有 Admin、REST、MCP、CLI、Copilot、Import、Search Replace 和 Agent 写入口必须调用同一个进程内 Mutation Policy。

```go
type ContentMutationMode string

const (
    MutationDraft      ContentMutationMode = "draft"
    MutationFork       ContentMutationMode = "fork"
    MutationDirectLive ContentMutationMode = "direct_live"
)
```

最终策略：

| 目标状态 | 调用方/入口 | 默认写入位置 | 是否影响 active Publication |
|---|---|---|---|
| 新建、未发布 | Admin/REST/CLI | Main Content draft | 否 |
| 新建、sandbox-only | MCP/REST Agent | Active Sandbox Fork | 否 |
| 已发布页面普通编辑 | Admin/REST/CLI/Copilot | 新建或复用 Fork | 否 |
| 已发布页面 Agent 编辑 | sandbox-only Agent | Active Sandbox Fork | 否 |
| 已发布页面 Search Replace | 默认 | Batch Fork Draft | 否 |
| 已发布页面 Import Upsert | 默认 | Fork Draft | 否 |
| 已发布页面显式直接编辑 | Admin + `content.live_edit` scope | Main Content draft state | 否，仍需 Publish |

`MutationDirectLive` 只表示绕过 Fork、直接更新主 Content 数据，不表示立即修改线上静态页面。任何 Content 更新都不得自行调用 `GenerateStaticPage`、删除 canonical 文件或切换 Publication。

原始 `ContentService.UpdateContent` 必须改为纯数据与版本操作：

```text
validate path/concurrency
↓
update Content
↓
create Content Version
↓
audit/provenance
↓
mark has_unpublished_changes=true when active Publication exists
```

新增兼容字段：

```go
HasUnpublishedChanges bool `bson:"has_unpublished_changes,omitempty" json:"has_unpublished_changes"`
```

只有 `PublicationService` 可以更新 live static output、`Published`、`PublishedAt` 和 active Publication 状态。

## 12.6 Fork Merge Contract

现有 Fork Merge 改为进程内统一的 draft merge，不再表示“立即上线”。REST、Admin 与 MCP 使用相同实现。

```go
type ForkMergeResult struct {
    Updated         int             `json:"updated"`
    Created         int             `json:"created"`
    Conflicts       []MergeConflict `json:"conflicts"`
    ContentIDs      []string        `json:"content_ids"`
    RequiresPublish []string        `json:"requires_publish"`
    Failed          []MergeFailure  `json:"failed"`
}
```

规则：

- 已有 published 页面：合并 Content 数据、创建版本、保留 active Publication，加入 `requires_publish`；
- 新页面：合并为 unpublished Content，加入 `requires_publish`；
- 每页使用独立 Mongo transaction；
- 多页 Fork 允许 per-page partial result；
- 单页失败不回滚其他已成功页面；
- 存在冲突时遵循现有 fork-wins 规则并返回 conflict 明细；
- 合并完成后 Fork 状态仍使用现有 `merged`，上线状态由每个 Content 的 active Publication 和 `HasUnpublishedChanges` 表达；
- Admin/MCP 文案统一改为 “Merge drafts; publishing is a separate action”。

## 12.7 Breaking Change 与兼容契约

本版本明确改变 LightCMS 原有行为：过去更新 `Published=true` 的 Content 可能立即重生成 live static page；升级后所有 Content Update 只产生 Draft/Fork/Content Version，必须显式 Publish 才改变线上页面。

从本产品版本开始：

```text
PUT/Update Content
    → data/version update only
    → live static page unchanged
    → has_unpublished_changes=true
    → requires_publish=true

POST Publish
    → Publication Saga
    → live changes after successful activation
```

Content update response 增加：

```json
{
  "has_unpublished_changes": true,
  "requires_publish": true,
  "active_publication_id": "..."
}
```

这是公开 API 行为变更，必须进入：

```text
UPGRADE.md
API.md
OpenAPI changelog
Release Notes
MCP tool descriptions
CLI help
Admin UI release notice
```

升级文档必须列出过去依赖 “PUT 即上线” 的调用方迁移方式：更新后再显式调用 Publish。不得仅将此变化作为内部实现细节。

## 12.8 Canonical Path 唯一性与并发

`FullPath` 保留作者输入的 canonical casing；新增 `CanonicalFullPath` 作为大小写不敏感、Unicode/斜杠规范化后的业务唯一键。

```go
CanonicalFullPath string `bson:"canonical_full_path" json:"-"`
PathScope         string `bson:"path_scope" json:"-"`   // "live" or fork ObjectID hex
PathActive        bool   `bson:"path_active" json:"-"`
```

数据库唯一索引：

```javascript
keys: { canonical_full_path: 1, path_scope: 1 }
unique: true
partialFilterExpression: { path_active: true }
```

规则：

- 主 Content 使用 `path_scope="live"`；
- Fork copy 使用对应 fork ID，因此可与 live 共享 path，但同一 Fork 内不能重复；
- soft delete 在同一 transaction 中设置 `path_active=false`，之后才允许复用路径；
- restore 前重新争抢路径，冲突返回 `409 PATH_CONFLICT`；
- canonicalization 在唯一索引写入前完成，大小写变体映射为同一个 key；
- 迁移时根据现有 `FullPath/ForkID/Deleted` 回填以上字段，再创建唯一索引。

现有 LightCMS 索引是 `UNIQUE(full_path, fork_id)`，并不等于本节的 case-folded canonical 唯一性。迁移必须先扫描大小写/Unicode 归一化冲突并由管理员处理，再回填字段、建立新索引，最后移除旧索引；若新索引创建失败，旧索引保持原样且升级中止。路径创建与 Rename 均以数据库新唯一索引为最终裁决，duplicate-key 映射为 `409 PATH_CONFLICT`。

锁键：

```text
publish existing: content:{id} + path:{canonical_full_path}
create:           path:{canonical_full_path}
rename:           content:{id} + path:{old} + path:{new}
```

一次操作需要多个锁时，先生成全部 lock key，按字节序排序后依次获取，释放时逆序执行，避免 deadlock。多实例使用 Mongo lease lock；数据库唯一索引是最终竞争裁决，锁只用于减少冲突和保护 Filesystem cutover。

---

# 13. Content Version

继续复用 LightCMS `ContentVersion`。

要求：

- 每次有效 Content 变更产生版本；
- 保留 actor/via/agent_session；
- Publication 引用具体 `content_version`；
- Content revert 默认只恢复编辑状态，不自动替换 active Publication；
- “恢复草稿”和“恢复并发布”是两个显式动作。

现有 `ContentVersion` 的 `Count()+1` 分配方式不能直接用于并发发布。产品写入路径必须为 Content 主记录增加 `current_version`，在同一 Mongo transaction 中对预期版本做 CAS、递增版本号并插入 `content_versions`；数据库增加 `UNIQUE(content_id, version)`。冲突返回 `409 CONTENT_VERSION_CONFLICT`，不能产生两个相同版本号。旧数据迁移以各 Content 的最大历史版本号回填 `current_version`，发现重复历史版本时生成阻塞报告并人工修复。

API：

```http
GET  /api/v1/content/{id}/versions
GET  /api/v1/content/{id}/versions/{version}
POST /api/v1/content/{id}/versions/{version}/revert
```

Product UI 增加：

```text
Restore as Draft
Restore and Publish
```

---

# 14. Preview

## 14.1 Existing Preview

继续复用：

```http
GET  /api/v1/content/{id}/preview
POST /api/v1/content/{id}/preview
```

支持：

- saved Content；
- unsaved overrides；
- Fork content；
- rendered HTML；
- errors/warnings。

## 14.2 Product Preview

Generation Facade 支持：

```json
{
  "mode": "preview"
}
```

Preview 不创建 Publication，不修改 live。

## 14.3 Preview URL

MVP 优先使用现有 authenticated Content Preview 和 Fork Preview。

只有必须向无后台账号的外部审核者分享 Preview 时，才增加短期 Preview Token：

- 128-bit 以上随机值；
- 数据库存 hash；
- 默认 TTL 1 小时；
- 最大 TTL 24 小时；
- `noindex,nofollow`；
- 不进入长缓存；
- 可撤销；
- 不计正式页面访问统计。

该能力不属于首批 MVP 强制范围。

---

# 15. Publication Model

## 15.1 目的

Publication 表示一次已经渲染、验证并可激活的静态部署，不是第二套 Page。

它解决：

- 当前线上版本指针；
- Content Version + Template Version 追踪；
- 原子切换；
- 发布失败隔离；
- 精确回滚；
- Storage path/hash 审计。

## 15.2 Collection

```text
content_publications
```

```json
{
  "id": "...",
  "content_id": "...",
  "content_version": 8,
  "template_id": "...",
  "template_version_id": "...",
  "template_version": 3,
  "full_path": "/news/bitcoin-market-update",
  "content_hash": "sha256:...",
  "storage_provider": "filesystem",
  "storage_path": "publications/.../index.html",
  "storage_state": "present",
  "status": "active",
  "verification_status": "verified",
  "pinned": false,
  "logical_published_at": "...",
  "published_by": "...",
  "actor": "human",
  "via": "api",
  "agent_session": "",
  "created_at": "...",
  "activated_at": "...",
  "superseded_at": null,
  "unpublished_at": null,
  "storage_deleted_at": null,
  "renderer_version": "lightcms-renderer-v1",
  "product_build_sha": "...",
  "render_dependencies_hash": "sha256:...",
  "dependency_snapshot": {},
  "failure_reason": ""
}
```

## 15.3 状态

```text
staged
active
superseded
failed
unpublished
```

Publication lifecycle 与 storage lifecycle 分开：

```text
publication.status:
    staged | active | superseded | failed | unpublished

publication.storage_state:
    pending | present | deleting | deleted | missing | corrupt

publication.verification_status:
    pending | verified | failed | legacy_unverified
```

`unpublished` 表示页面已下线但历史 Publication 仍存在；`storage_state=deleted` 才表示对应 immutable object 已被 GC。生命周期状态不得复用为存储删除状态。

新 Publication 创建时 `verification_status=pending`；Stage/Verify 通过并核对渲染 hash 后改为 `verified`；验证失败改为 `failed` 并进入失败处理；迁移中保留的旧 HTML 使用 `legacy_unverified`。只有 `verified` 或明确迁移批准的 `legacy_unverified` 才可进入 `active`，普通业务发布不得激活 `legacy_unverified`。

`Content.PublishedAt`、模板 Render Context 的 `published_at` 和 `content.publish` Webhook payload 的 `published_at` 均等于该 Publication 的 `logical_published_at`。`activated_at` 只用于发布事务时延、故障排查和审计，不替代业务发布时间。

## 15.4 约束

- 同一 Content 同时最多一个 active Publication；
- Publication 创建后渲染输入和 hash 不可修改；
- 已持久化且标记 `failed` 的 Publication 不可修改或重新 stage；业务重试必须创建新的 Publication。Crash takeover 仅在该 Publication 尚未进入 terminal state 时复用当前 ID。
- 旧 active 只有在新 Publication 激活成功后才 superseded；
- Content soft delete 不立即物理删除历史 Publication metadata；
- retention job 根据策略清理 superseded static objects。

## 15.5 索引

```text
content_id + status
content_id + activated_at desc
template_version_id
content_hash
created_at
```

使用 partial unique index 保证每个 Content 最多一个 active Publication。

## 15.6 Publication 状态机

允许的状态转换固定为：

```text
staged ───────→ active
   │
   └─────────→ failed

active ───────→ superseded
   │
   └─────────→ unpublished   # unpublish
```

禁止：

- `failed → active`；
- `superseded → active`；
- `unpublished → active`；
- 修改已存在 Publication 的 Content/Template Version 或 hash。

Storage 状态转换：

```text
pending → present
pending → missing
present → deleting → deleted
present → missing | corrupt
missing/corrupt → present     # scanner 从可靠 immutable source 修复成功
```

Rollback 必须创建一个新的 staged Publication，引用历史 Content Version 和 Template Version，再走完整激活流程。每次 Publish、Republish 和 Rollback 都产生新的 Publication ID；即使渲染 hash 相同也保留新的发布审计记录，但 StaticPageStore 可以内部复用相同 immutable object。

Publication ID 使用新 Mongo ObjectID；对外以 hex string 返回。

## 15.7 发布状态真值

最终权威关系：

```text
content_publications(status=active)
    = 发布历史、控制面与期望状态真值

Content.Published / Content.PublishedAt
    = 为兼容现有查询保留的投影

Filesystem canonical static file
    = 数据面实际 serving 内容真值

immutable publication object
    = 可验证、可恢复的内容真值
```

因此 Filesystem 模式不声称 MongoDB 与公开文件在每一个瞬间严格一致。atomic rename 后、Mongo transaction 前存在短暂窗口，此时新页面可能已被读取而控制面仍指向旧 Publication。该窗口由 page lock、补偿和 startup/periodic scanner 收敛。监控必须分别检查 control-plane state 和 data-plane hash。

R2/Worker pointer 模式可以由数据库/映射 pointer 决定实际 serving object，但这属于同一应用的可配置存储实现，不改变 Publication 状态机。

Content 不保存 `active_publication_id`，Product DTO 中的 `active_publication_id` 通过 Publication Repository 查询唯一 active 记录生成，避免 Content 与 Publication 双向指针。

Publication 激活时，在同一 Mongo transaction 中：

1. 使用 `content_id + expected_old_publication_id` 做 compare-and-swap；
2. 将旧 active 更新为 superseded；
3. 将新 staged 更新为 active；
4. 更新 `Content.Published=true`、`PublishedAt` 和 `HasUnpublishedChanges=false`。

Unpublish 在同一 transaction 中将 active 更新为 unpublished，并更新 Content 投影为 unpublished。后续 GC 只修改 `storage_state`，不改写 Publication 生命周期历史。

生产 MongoDB 必须使用 replica set 以支持 transaction。开发和自动化测试也使用单节点 replica set。项目不支持以 standalone MongoDB 作为正式运行模式；若启动时检测到不支持 transaction，服务拒绝进入 production mode。

## 15.8 Publication 精确索引

```javascript
{ content_id: 1, status: 1 }
{ content_id: 1, activated_at: -1 }
{ template_version_id: 1 }
{ content_hash: 1 }
{ created_at: 1 }
```

Partial unique index：

```javascript
keys: { content_id: 1, status: 1 }
unique: true
partialFilterExpression: { status: "active" }
```

激活 transaction 还必须校验调用方读取到的旧 active ID 未变化；否则返回 `409 PUBLICATION_CONFLICT`。

---

# 16. Atomic Publish Pipeline

## 16.1 流程

```text
Acquire page publish lock
↓
Load Content/Fork result
↓
Resolve Content Version
↓
Resolve immutable Template Version
↓
Strict field validation
↓
Allocate Publication ID in memory
↓
Freeze logical_published_at
↓
Resolve canonical Public URL
↓
CAS-persist Idempotency Execution Snapshot
↓
Build Render Context with publication_id / logical_published_at / public_url
↓
Render using existing Renderer
↓
Rendered HTML security validation
↓
Compute SHA-256
↓
Persist staged Publication
↓
StaticPageStore.Stage
↓
StaticPageStore.Verify
↓
Atomic Activate
↓
DB transaction/conditional update active pointer
↓
Mark old Publication superseded
↓
Update Content published state
↓
Create transactional Webhook Outbox event
↓
Commit idempotency resource/result metadata
↓
Purge CDN asynchronously/retryably
↓
Deliver Webhook Outbox asynchronously/retryably
↓
Audit
↓
Return Publication + Public URL
```

Publication ID 使用 `primitive.NewObjectID()` 在 Render 前分配；`logical_published_at` 同时冻结，并作为模板中的 `published_at`；`activated_at` 仅表示实际完成 activation 的时间，不进入渲染结果。二者必须在 Render 前作为 Idempotency Execution Snapshot 持久化，不能只保存在进程内存。

Crash/uncertain takeover 必须复用当前 attempt 的 `publication_id` 和 `logical_published_at`，从而保证 HTML hash 可重现。已确认的 terminal pre-activation failure 不复用 failed Publication；同一 Idempotency-Key 的显式重试增加 attempt 并分配新的 Publication ID。两种场景不得混为一条规则。

## 16.2 失败规则

| 失败阶段 | 行为 |
|---|---|
| Validation | 不创建 Publication |
| Render | 不修改 live |
| Stage | Publication=failed，旧 live 保留 |
| Verify | Publication=failed，清理 staged object，旧 live 保留 |
| Activate | Publication=failed，旧 live 保留 |
| DB active pointer | Filesystem 恢复旧 canonical 文件；对象存储保持旧 pointer；新对象进入 GC |
| CDN purge | Publication 保持 active，记录告警并重试 |
| Webhook | Publication 保持 active，按现有机制重试 |

## 16.3 成功定义

只有满足以下条件才返回 `published=true`：

1. Renderer 成功；
2. staged output 验证成功；
3. static output 激活成功；
4. active Publication pointer 更新成功；
5. Content published 状态更新成功。

CDN purge 和 Webhook 属于激活后的可重试 side effect，不应回滚已经成功的 Publication。

## 16.4 Filesystem 激活补偿

Filesystem 直接通过 canonical 文件提供页面时，文件 rename 和 MongoDB 更新不能组成单一跨资源事务。因此激活必须具备补偿能力：

```text
Create same-directory backup/reference of current canonical file
↓
Atomic rename staged file to canonical path
↓
Conditional update active Publication + Content state
↓ success
Delete/retain previous file according to retention
```

如果 DB conditional update 或 Content state update 失败：

```text
Atomic restore previous canonical file
↓
Mark new Publication failed
↓
Keep previous Publication active
```

首次发布没有 previous file 时，DB 提交失败必须移除新 canonical file。补偿失败属于最高级别告警，并由 consistency scanner 修复。

## 16.5 并发

同一 Content 同时只允许一个 Publish。

单实例使用现有 per-page mutex；多实例部署使用 Mongo lease + conditional update，不引入独立 distributed-lock 服务。

冲突返回：

```http
409 PAGE_PUBLISH_IN_PROGRESS
```

## 16.6 统一发布入口

`PublicationService` 是同一 Go 进程内唯一允许改变 live 页面状态的模块：

```go
type PublicationService interface {
    Publish(ctx context.Context, req PublishRequest) (*PublicationResult, error)
    Unpublish(ctx context.Context, req UnpublishRequest) error
    Rollback(ctx context.Context, req RollbackRequest) (*PublicationResult, error)
}
```

旧 `ContentService.PublishContent`、`UnpublishContent` 和直接 `GenerateStaticPage` 的业务调用必须移除或改为仅委托 PublicationService。Renderer 内部函数可以保留，但不得自行落盘或更新发布状态。

旧入口迁移矩阵：

| 现有入口 | 最终调用路径 | 迁移要求 |
|---|---|---|
| REST `POST content/{id}/publish` | PublicationService.Publish | 保持 URL，替换 handler 实现 |
| REST batch publish | 循环/批量 PublicationService.Publish | per-item result，不直接 ContentService |
| MCP `publish_content` | API Client → REST publish | 自动继承新语义 |
| MCP `publish_multiple` | API Client → REST batch publish | 自动继承新语义 |
| Admin Publish | PublicationService.Publish | 显示 publication ID/URL |
| CLI Publish | REST publish | 不直连 service/DB |
| Scheduled Publish | PublicationService.Publish | 失败保留 schedule retry 状态 |
| Import AutoPublish | PublicationService.Publish | 每页结果写 Import Job log |
| Copilot Publish | PublicationService.Publish | 保留 permission/audit |
| Search Replace AutoRepublish | PublicationService.Publish | 先完成 draft update，再逐页发布 |
| Fork Merge | 只 merge draft | 不发布；返回 requires_publish |
| Template Upgrade | PublicationService.Publish | 使用指定 Template Version |
| Approval Approve | 只改变审批状态 | 是否发布由显式后续动作决定 |
| Content Watcher | 禁止写 canonical live | 仅做索引/告警或移除 |

所有入口必须产生相同 Publication、Audit、Webhook、CDN purge 和错误语义。禁止保留“旧 publish”和“product publish”两套实现。

## 16.7 Publish Request

```go
type PublishRequest struct {
    ContentID          primitive.ObjectID
    ContentVersion     int64
    TemplateVersionID  primitive.ObjectID
    ExpectedActiveID   *primitive.ObjectID
    Reason             string
    IdempotencyRecord  *primitive.ObjectID
}
```

未显式提供 ContentVersion 时，Service 在获得 publish lock 后读取当前最新版本并固定；未提供 TemplateVersionID 时，读取 Template.CurrentVersion 并固定。完成固定后，后续并发编辑不会进入本次 Publication。

---

# 17. StaticPageStore

## 17.1 接口

```go
type StaticPageStore interface {
    Stage(ctx context.Context, req StageRequest) (StagedObject, error)
    Verify(ctx context.Context, obj StagedObject) error
    Activate(ctx context.Context, obj StagedObject, publicPath string) error
    Restore(ctx context.Context, obj StoredObject, publicPath string) error
    Open(ctx context.Context, obj StoredObject) (io.ReadCloser, error)
    Abort(ctx context.Context, obj StagedObject) error
    Delete(ctx context.Context, publicPath string) error
    Exists(ctx context.Context, publicPath string) (bool, error)
}
```

## 17.2 Filesystem 实现

第一阶段默认实现。

要求：

- stage 与 final path 位于同一文件系统；
- 写临时文件；
- close/sync；
- 校验 size/hash；
- atomic rename；
- 不直接 `os.WriteFile` 覆盖 live 文件；
- delete 错误必须返回；
- path 必须经过 canonicalization 和 traversal protection。

固定目录：

```text
content/publications/{content_id}/{publication_id}/index.html
content/generated/{canonical_path}.html
```

其中 publications 目录保存 immutable object，generated path 是 active projection。MVP 必须保留 immutable publication file，供回滚、崩溃恢复和一致性修复使用。

## 17.3 R2/S3 实现

满足以下条件之一时启用：

- 多实例；
- 应用容器无持久盘；
- 需要全球 CDN；
- 需要 immutable object；
- 需要跨实例原子 pointer。

对象路径：

```text
pages/{content_id}/{publication_id}/index.html
```

激活方式：

- DB/current mapping；或
- Worker/KV pointer；或
- 复制到 canonical key 后切换 metadata。

具体实现必须另有 Storage ADR。

## 17.4 Filesystem 文件布局与事务算法

固定布局：

```text
content/publications/{content_id}/{publication_id}/index.html.tmp
content/publications/{content_id}/{publication_id}/index.html
content/generated/{canonical_path}.html
content/generated/{canonical_path}.html.previous-{publication_id}
content/generated/{canonical_path}.html.unpublish-backup-{publication_id}
```

Stage：

1. 创建 publication 目录；
2. 以 `O_CREATE|O_EXCL` 写 `.tmp`；
3. 写完整 body；
4. `fsync` 文件并关闭；
5. 校验 size 和 SHA-256；
6. rename `.tmp` 为 immutable `index.html`；
7. `fsync` 父目录。

Activate：

1. 若 canonical 存在，将其 atomic rename 为 `.previous-{oldPublicationID}`；
2. 从 immutable `index.html` 在 canonical 同目录创建 `.next-{newPublicationID}`；
3. 校验 `.next` hash；
4. atomic rename `.next` 为 canonical；
5. `fsync` canonical 父目录；
6. 执行 Mongo activation transaction；
7. transaction 成功后按 retention 处理 previous；
8. transaction 失败时删除新 canonical，并将 previous atomic rename 回 canonical。

实现不得假设 hard link 在所有部署文件系统可用；默认使用文件复制到同目录临时文件，再 atomic rename。R2/S3 使用 immutable object 和数据库 active pointer，不执行本地 rename。

## 17.5 Crash Recovery 与 Consistency Scanner

`PublicationConsistencyService` 是同一 Go 进程内模块，不是独立服务。它在启动完成数据库连接后运行一次，此后每 10 分钟运行；同一时刻只允许一个扫描任务。

权威顺序：

```text
Mongo active Publication metadata
↓
immutable publication object
↓
canonical projection（可重建）
```

规则：

| 检测结果 | 处理 |
|---|---|
| active DB + canonical missing | 从 immutable object 重建 canonical，记录告警 |
| active DB + canonical hash mismatch | 隔离错误文件并重建 canonical，P0 告警 |
| active DB + immutable object missing | 不自动猜测，P0 告警并将站点标记 degraded |
| staged 超过 15 分钟 | 标记 failed，Abort staged object |
| failed/superseded object 超过 retention | 删除 object，成功后设置 `storage_state=deleted` |
| multiple active | 停止该 Content 自动修复，P0 告警 |
| canonical exists + no active | migration flag 为 running 时只告警；completed 后移到 quarantine，不对外服务 |
| `.previous-*` + DB old active | 恢复 previous |
| `.previous-*` + DB new active | 确认 canonical hash 后清理 previous |
| `.unpublish-backup-*` + DB old active | 将 backup 恢复为 canonical |
| `.unpublish-backup-*` + DB unpublished | 确认 canonical 不在提供服务后清理 backup |
| `.next-*` orphan | 超过 15 分钟后删除 |

扫描期间跳过当前持有 publish lock 的 Content。每次修复写 Audit，指标记录修复结果。

`.previous-*` 与 `.unpublish-backup-*` 均按本表恢复或清理；不能仅按文件年龄删除。恢复及清理以 Mongo lifecycle 和文件 hash 为依据，scanner 必须在持有相同 content/path lock 时操作。

## 17.6 Retention 与 GC

```text
active                    永不自动删除
pinned superseded         永不自动删除
pinned unpublished        永不自动删除
superseded                默认保留 90 天
unpublished               默认保留 90 天
failed/staged             默认保留 7 天
quarantine                默认保留 30 天
Publication metadata      默认永久保留，object 删除后 `storage_state=deleted`
```

GC 必须先删除 storage object，成功后再更新 metadata；失败进入重试，不得先删除审计记录。

---

# 18. Unpublish、Delete、Rename 与 Restore

## 18.1 Unpublish

```text
Acquire lock
↓
Read and freeze expected active Publication
↓
Stage canonical removal by atomic rename to .unpublish-backup-{publication_id}
↓
Mongo transaction:
    active Publication → unpublished
    Content.Published=false
    Content.PublishedAt=nil
    INSERT UNIQUE content.unpublish Webhook Outbox
↓
Commit
↓
Delete/retain backup according to retention
↓
Purge CDN + asynchronous delivery
```

Unpublish 与 Publish 使用同一 page lock。Filesystem rename 与 MongoDB 之间仍是 Saga：

- rename 失败：不修改 DB，返回 `503 PUBLICATION_UNPUBLISH_STAGE_FAILED`；
- DB transaction 失败：将 backup atomic rename 回 canonical，保持旧 active；
- DB 成功、进程在清理 backup 前崩溃：页面在 DB 中已下线，scanner 根据 unpublished 状态删除/隔离 canonical 和 backup；
- compensation 失败：P0 告警，scanner 修复；
- CDN purge/Webhook 失败：不恢复上线状态，进入 retry。

`publication.failed` 同样必须在将 Publication 状态更新为 failed 的 Mongo transaction 内插入唯一 Outbox event，禁止先提交状态再单独创建事件。

Unpublish 成功条件是 canonical 已不可公开访问且 MongoDB transaction 已提交。物理删除 immutable publication object 不属于同步 Unpublish，交由 retention/GC。

## 18.2 Delete

- Content 继续 soft delete；
- 先保证 public path 不再提供 active 内容；
- 历史 Publication metadata 按 retention 保留；
- immutable static objects 异步 GC；
- 删除失败可重试。

## 18.3 Rename

已发布页面修改 path 时：

1. 通过 Mutation Policy 创建新的 Content Version，旧 Content Version 不变；
2. 以新 Content Version 和原/指定 Template Version 创建新的 Publication；
3. 新 path stage/verify/activate 成功；
4. 创建或更新 redirect；
5. 旧 path canonical projection 下线；
6. 两侧 CDN purge；
7. 旧 Publication 保持 immutable，状态变为 superseded。

不得修改旧 Publication 的 `full_path`、storage path 或其他 metadata，不得先删除旧 path。Rename 的 redirect 创建与新 Publication activation 必须在同一 Mongo transaction 中提交；Filesystem 变化按 Publish/Unpublish Saga 补偿。

## 18.4 Restore

Content Version restore 默认生成 draft。

`restore_and_publish=true` 时创建新 Publication，引用历史 Content Version 和准确 Template Version，不复用/篡改旧 Publication 记录。

## 18.5 Rollback 保证范围

回滚分为两级：

```text
Exact Rollback
    = immutable publication object 仍保留
    = 恢复历史字节完全一致的 HTML

Re-render Rollback
    = immutable object 已被 GC
    = Content Version + Template Version + compatible renderer/dependency snapshot
    = 不承诺字节完全一致
```

默认 Exact Rollback SLA 为 Publication superseded 后 90 天。需要更长保证的 Publication 必须 `pinned=true`，其 immutable object 不参与自动 GC。

Publication 必须记录：

```text
renderer_version
product_build_sha
render_dependencies_hash
dependency_snapshot
```

`dependency_snapshot` 至少固定 Theme、Snippet、Markdown/HTML sanitizer policy、Wikilink/TOC 处理版本以及模板引用的其他可变依赖。Product Template 应尽量 self-contained；如果依赖无法版本化或快照化，则超过 Exact Rollback SLA 后只能声明 best-effort re-render，不得宣称精确回滚。

---

# 19. Public URL Contract

## 19.1 Resolver

```go
type PublicURLResolver interface {
    Resolve(canonicalFullPath string) (*url.URL, error)
}
```

## 19.2 Base URL

若公共站点与 Admin/API 同域，继续复用：

```text
BASE_URL
```

若不同域，增加：

```text
PUBLIC_BASE_URL
```

## 19.3 规则

- production 必须 HTTPS；
- Base URL 启动时验证；
- full path 使用 canonical casing；
- path 必须转义；
- 禁止用户输入覆盖 scheme/host；
- query/fragment 不属于 canonical URL；
- 不在公开 URL 中暴露 Mongo ObjectID；
- Publication 激活成功后才返回 URL。

示例：

```text
https://pages.example.com/news/bitcoin-market-update
```

---

# 20. Generation Facade

## 20.1 Endpoint

```http
POST /api/v1/page-generation
```

只增加必要 Facade，不重包完整 Content API。

## 20.2 Request

```json
{
  "template": "financial-news",
  "expected_template_version": 3,
  "title": "Bitcoin Market Update",
  "slug": "bitcoin-market-update",
  "folder_path": "/news",
  "data": {
    "headline": "Bitcoin Rallies",
    "subtitle": "Markets move higher",
    "author": "Market Desk",
    "body": "<p>...</p>"
  },
  "mode": "publish",
  "upsert": false
}
```

`GET /api/v1/templates/{slug}/schema` 返回 `template_version`，并设置：

```http
ETag: "template-version-3"
```

外部 `mode=publish` 请求必须提交 `expected_template_version`；Admin UI 由页面表单自动携带。Publish lock 内解析当前 Template Version，如果与 expected 不同，返回：

```http
409 TEMPLATE_VERSION_CHANGED
```

且零副作用。缺少必需前置条件返回 `428 TEMPLATE_VERSION_PRECONDITION_REQUIRED`。Draft/Preview 可以省略该字段并使用请求时当前版本，但响应必须返回实际版本。该字段只是 optimistic concurrency 前置条件，不允许调用方任意选择历史 Template Version。

## 20.3 Mode

```text
draft
preview
publish
sandbox
```

语义：

| Mode | 行为 |
|---|---|
| draft | 创建/更新未发布 Content；已发布目标进入 Fork |
| preview | 不激活 live，返回渲染结果 |
| publish | 创建并原子发布，要求 publish 权限 |
| sandbox | 强制写入当前 Agent Sandbox/Fork |

## 20.4 Response

```json
{
  "id": "content-id",
  "action": "created",
  "mode": "publish",
  "published": true,
  "requires_publish": false,
  "content_version": 1,
  "template": "financial-news",
  "template_version": 3,
  "publication_id": "publication-id",
  "full_path": "/news/bitcoin-market-update",
  "public_url": "https://pages.example.com/news/bitcoin-market-update",
  "warnings": []
}
```

`requires_publish` 是 Generation 响应的必填布尔值，不是仅供 Content Update 使用的字段。`draft`、`sandbox` 成功写入未上线改动时为 `true`；`publish` 成功完成 Publication activation 后为 `false`；`preview` 无持久化写入，固定为 `false`。`published` 仅表示当前存在 active Publication，因此可以与 `requires_publish=true` 同时出现（已发布页面另有未上线草稿）。

## 20.5 内部流程

```text
Existing middleware
↓
Resolve Template Slug
↓
Check Template active
↓
Check RBAC/scope/sandbox
↓
Normalize Path
↓
Strict Validate
↓
Select Content or Fork target
↓
Create/Upsert using shared Content Service
↓
Preview/Render
↓
If publish: Publication Orchestrator
↓
Audit/Provenance
↓
Response mapping
```

权限、scope、sandbox policy 和 mode 合法性必须在任何持久化副作用之前完成。`mode=publish` 是组合命令：目标不存在时要求 `content.create + content.publish`；目标存在时要求 `content.edit + content.publish`。缺少任意权限直接返回 403，并保证：

```text
no Content/Fork create or update
no Content Version
no Publication
no static stage
no Webhook/Audit mutation event
no Idempotency completed response
```

系统不得自动降级为 draft/Fork 后再返回 403。调用方如希望保存 draft，必须显式以 `mode=draft` 或 `mode=sandbox` 重新请求，并使用新的 Idempotency-Key。

## 20.6 Upsert

`upsert=true` 以 canonical `full_path` 为业务 key。

最终行为按 target state × mode 唯一确定：

| Target | `draft` | `preview` | `sandbox` | `publish` |
|---|---|---|---|---|
| not exist | create Main draft | no write | create Sandbox/Fork draft | create Content + Version + Publication |
| unpublished | replace Main draft | no write | create/update Sandbox/Fork draft | replace Content + new Version + Publication |
| published | create/update Fork draft | no write | create/update Sandbox/Fork draft | direct publish command：replace Main Content + new Version + Publication，不创建 Fork |

补充规则：

- `preview` 永远不写任何资源；
- `upsert=false` 且 canonical path 已存在时返回 `409 PATH_CONFLICT`，不覆盖已有 Content；
- sandbox-only key 无论 target 状态都只能使用 `sandbox` mode；
- case variant 按 `CanonicalFullPath` 唯一约束处理；
- `draft` 命中 published 页面绝不直接修改主 Content；
- `publish` 命中 published 页面是明确的 live-changing command，因此要求 `content.edit + content.publish` 并受 Idempotency-Key 保护。

## 20.7 精确请求语义

Generation 是“创建或完整替换结构化页面数据”的命令，不承担通用 PATCH。

规则：

- Create：`title`、`template`、`data` 必填；
- Upsert 命中已有页面：`data` 为完整替换，不做隐式 merge；
- 缺失字段按 Schema required/default 处理，不能解释为保留旧值；
- JSON `null` 仅在字段 Schema 明确 `nullable=true` 时允许；MVP 默认所有字段不可为 null；
- 空字符串是明确值，并接受 required/minLength 验证；
- 未知顶层字段和未知 `data` 字段返回 422；
- `title/slug/folder_path` 在 upsert 时按请求完整替换；未提供可选 slug 时由 title 重新生成；
- Partial Update 继续使用现有 Content Update API，并遵守 Mutation Policy；
- `mode=preview` 不写 Content、Version、Fork、Publication 或 Idempotency resource；
- `mode=sandbox` 没有 active sandbox 时返回 `409 AGENT_SANDBOX_REQUIRED`；
- `mode=publish + upsert=true` 命中已发布页面时，直接使用请求形成新的 Content Version 和 staged Publication，不创建短暂 Fork；若调用者没有 `content.publish` 权限，在任何写入前返回 403，零副作用；
- Template Version 在请求通过验证、获取 publish lock 后固定；响应返回实际版本；
- MVP 不允许客户端选择任意历史 Template Version；`expected_template_version` 只用于验证当前版本未变化；
- 请求继续受现有 10 MiB body limit；Product 层额外限制 `data` canonical JSON 最大 5 MiB。

## 20.8 HTTP 状态

| 场景 | HTTP |
|---|---:|
| Create draft/sandbox | 201 |
| Upsert update | 200 |
| Preview | 200 |
| Publish new Content | 201 |
| Publish existing Content | 200 |
| Validation/unknown field | 422 `FIELD_VALIDATION_FAILED`（单字段原因写入 `details`，如 `FIELD_REQUIRED`） |
| Template/Content not found | 404 |
| Template deprecated | 409 `TEMPLATE_NOT_ACTIVE` |
| Path/idempotency/publication conflict | 409 |
| Publish in progress | 409 |
| Missing required Idempotency-Key | 428 |
| Missing expected_template_version on external publish | 428 |
| Sandbox required | 409 |
| Permission/scope denied | 403 |
| Unauthenticated | 401 |
| Rate limited | 429 |
| Static store temporarily unavailable | 503 |
| Non-retryable internal error | 500 |

所有错误响应包含：

```json
{
  "error": {
    "code": "PUBLICATION_STAGE_FAILED",
    "message": "The page could not be staged for publication",
    "request_id": "req_...",
    "retryable": true,
    "details": []
  }
}
```

429、`REQUEST_IN_PROGRESS` 和可重试 503 设置 `Retry-After`。

---

# 21. HTTP Idempotency

## 21.1 适用范围

所有外部触发、可能创建新 Publication 的 live-changing command 强制要求：

```http
Idempotency-Key: partner-news-20260929-001
```

范围至少包括：

- Page Generation `mode=publish`；
- legacy single publish endpoint；
- batch/publish_multiple 的每个 item；
- Publication rollback；
- 外部触发的 Template Upgrade publish；
- Rename-and-publish。

缺少必需的 Idempotency-Key 返回 `428 IDEMPOTENCY_KEY_REQUIRED`，并保证零业务副作用。服务内 Scheduler/Import/Template Upgrade 使用稳定的内部 operation key 派生同一 IdempotencyRecord 语义，不依赖外部 HTTP Header。

Batch 请求使用一个顶层 Idempotency-Key，并为每项派生 `HMAC(parent_key, canonical_content_id_or_path)`，保证 per-item replay。Draft 可选支持 Idempotency-Key；Preview 不创建资源并忽略 Idempotency-Key，不创建 IdempotencyRecord。

Unpublish 定义为天然幂等：已 unpublished 时返回 200，并且不创建第二个 Outbox event。调用方可提供 Idempotency-Key，但不是必需。

## 21.2 Collection

```text
idempotency_records
```

```json
{
  "operation_id": "...",
  "owner": "api-key-or-user-id",
  "method": "POST",
  "path": "/api/v1/page-generation",
  "key": "...",
  "request_hash": "sha256:...",
  "state": "completed",
  "attempt": 1,
  "attempt_state": "completed",
  "publication_id": "...",
  "logical_published_at": "...",
  "content_id": "...",
  "content_version": 8,
  "template_version_id": "...",
  "canonical_full_path": "/news/example",
  "status_code": 201,
  "response": {},
  "created_at": "...",
  "expires_at": "..."
}
```

## 21.3 行为

```text
same key + same request + completed
    → replay same response

same key + different request
    → 409 IDEMPOTENCY_CONFLICT

same key + processing
    → 409 REQUEST_IN_PROGRESS
```

## 21.4 索引

```text
UNIQUE(owner, method, path, key)
TTL(expires_at)
```

默认保留 24 小时，可配置 1～72 小时。

幂等记录必须覆盖 Content 创建、Content Version、Publication 和 publish webhook 副作用。

## 21.5 Processing Lease 与缓存规则

IdempotencyRecord 精确增加：

```text
processing_expires_at
lease_generation
attempt
attempt_state
content_id
publication_id
logical_published_at
content_version
template_version_id
canonical_full_path
completed_at
```

规则：

- owner 使用数据库 API Key ID；OAuth 使用 `client_id + subject user_id`；
- request hash 对去除无关空白并按 key 排序的 canonical JSON 计算 SHA-256；
- Authorization、User-Agent、request_id 不进入 hash；
- `processing` lease 默认 5 分钟；
- 执行 worker 每 60 秒 heartbeat，通过 CAS 延长 `processing_expires_at` 5 分钟；
- heartbeat 必须校验 owner/attempt，失去 lease 的 worker立即停止后续副作用；
- 所有外部网络、Render、Stage 和 Activate 单步 timeout 必须小于当前 lease 剩余时间；
- lease 未过期的重复请求返回 `409 REQUEST_IN_PROGRESS`；
- lease 过期后新 worker 通过 CAS 增加 `lease_generation` 并接管，但不改变业务 `attempt`；
- crash/uncertain takeover 不增加 attempt，复用当前 execution snapshot；只有能证明当前 attempt 尚未产生 live side effect 且已标记 `terminal_pre_activation_failure` 时，客户端重试才通过 CAS 执行 `attempt++`、分配新 Publication ID 和新 logical time；
- 2xx 响应必须缓存并 replay；
- 仅纯请求验证产生的 400/422 可以缓存 24 小时；
- 401、403、404、409、429 和所有 5xx 不缓存为 completed，因为权限、资源、锁和服务状态可能变化；
- 非缓存错误释放或缩短 lease，允许调用方修复状态后重试；
- replay 响应保留原 status/body/resource IDs，但生成新的 HTTP `Date`；
- response 中不得保存 Authorization、Cookie、原始 API Key 或其他 secret；
- TTL 到期后相同 key 视为新请求。

Execution Snapshot 必须在 Render 前通过 CAS 持久化，字段至少包括 `operation_id/attempt/publication_id/logical_published_at/content_id/content_version/template_version_id/canonical_full_path/attempt_state`。持久化失败不得开始 Render。

Content 创建/更新和 IdempotencyRecord 的 `content_id/content_version/canonical_full_path` 绑定必须位于同一 Mongo transaction。若进程在该 transaction 之前崩溃，重试可安全重新执行；若在 commit 之后崩溃，接管 worker 从 durable snapshot 读取 Content ID 与版本，禁止按 path 再创建第二个 Content。对已存在 Content 的 `mode=publish`，数据替换、新 Content Version 与 snapshot 绑定也必须同一 transaction 提交。

在创建 Content 后发生进程崩溃时，接管 worker 使用 snapshot 查询现有状态并继续或 replay，不得无条件重新创建资源。failed Publication 永远不可重新 stage；terminal failure 的新 attempt 使用新 Publication。IdempotencyRecord 保存各 attempt 的 publication ID 关联，最终 completed response 指向成功 attempt。

---

# 22. Authentication 与权限

## 22.1 直接复用

- API Key；
- OAuth 2.1；
- RBAC；
- API key scopes；
- `sandbox_only`；
- provenance middleware；
- audit attribution。

## 22.2 Generation 权限映射

| 操作 | 权限 |
|---|---|
| Get Template Schema | template view |
| Draft Create | content create |
| Draft Update/Upsert | content edit |
| Preview | content view/edit |
| Publish | content publish |
| Asset Import | asset upload |
| Template Modify | template edit，不授予外部 Agent |

Generation 组合权限：

| Mode/Target | Required Scopes |
|---|---|
| draft + new | `content.create` |
| draft + existing | `content.edit` |
| preview only | `content.view`，若含未保存数据还需相应 create/edit |
| sandbox + new | `content.create` + sandbox permission |
| sandbox + existing | `content.edit` + sandbox permission |
| publish + new | `content.create` + `content.publish` |
| publish + existing | `content.edit` + `content.publish` |

`publish-only` key 不能借 Generation 修改 Content；`create-only`、`edit-only` key 不能发布。组合权限检查在路径查询之后、任何 mutation 之前完成，且不得泄露调用者无权查看的 Content 详情。

## 22.3 Agent Key

外部或高风险 Agent 默认：

```text
sandbox_only=true
```

可信自动发布 Agent：

- 独立 user；
- 独立 key；
- `sandbox_only=false`；
- 最小 content/template-view/asset scopes；
- 禁止 template edit；
- 禁止 users/settings/webhook management；
- 所有请求携带 `X-Agent-Session`。

---

# 23. MCP 策略

不建设第二套 Product MCP。

优先使用现有工具：

```text
list_templates
get_template
create_content
update_content
preview_content
publish_content
bulk_create_content
publish_multiple
upload_asset_from_url
start_agent_sandbox
get_fork_diff
merge_fork
get_agent_session_changes
rollback_agent_session
```

新增 schema endpoint 后，可以为现有 MCP 增加薄工具：

```text
get_template_schema
```

`generate_page` 仅在实测 Agent 调用链显著复杂时增加。它必须调用 Generation Facade/API Client，不得复制业务逻辑。

---

# 24. Agent 工作流

## 24.1 Trusted Auto-Publish

```text
Get Template Schema
↓
Generate structured data
↓
Import assets
↓
Preview
↓
Fix blocking errors
↓
Generate mode=publish with Idempotency-Key
↓
Return Public URL
```

## 24.2 Reviewed Agent

```text
start_agent_sandbox
↓
create/edit Fork content
↓
preview
↓
get_fork_diff
↓
human review
↓
merge_fork
↓
explicit publish
```

## 24.3 Bulk

```text
Get Schema once
↓
bulk_create_content / import_markdown
↓
per-item validation result
↓
preview sample
↓
publish_multiple
```

禁止大量串行单页调用替代现有 Bulk 能力。

---

# 25. Asset

## 25.1 复用能力

继续使用：

```http
POST /api/v1/assets
POST /api/v1/assets/from-url
```

## 25.2 Page Data

优先保存稳定 Asset reference 或 canonical serve path，不保存临时远程 URL。

## 25.3 From URL 安全

保留现有：

- HTTP/HTTPS only；
- private/reserved IP block；
- DNS-at-dial validation；
- timeout；
- serve path allowlist；
- MIME/extension validation。

必须修复大小限制：

```text
maxSize = 50 MiB = 50 << 20 bytes
read maxSize + 1
if len > maxSize → ASSET_TOO_LARGE
```

`Content-Length > maxSize` 可提前拒绝；无长度或分块传输仍必须读取最多 `maxSize+1` 字节并拒绝超限，不能保存截断后的前 50 MiB。MVP 固定该上限，未来调整须同时更新 API 文档、测试和部署容量评估。

禁止保存被截断的远程文件。

## 25.4 Security Tests

覆盖：

- localhost/RFC1918/link-local/metadata IP；
- IPv6 loopback/ULA；
- private DNS；
- redirect-to-private；
- redirect loop；
- oversized Content-Length；
- oversized chunked response；
- fake MIME；
- corrupted image；
- SVG script；
- URL userinfo；
- unsafe serve path。

---

# 26. Script Policy 与 HTML 安全

继续支持：

```text
all
admin_only
none
```

生产建议默认：

```text
admin_only
```

外部 Agent/Partner 内容按非管理员处理。

必须区分：

- Template HTML 权限；
- Content rich HTML 权限；
- Markdown raw HTML 权限；
- Tracking Provider 权限。

发布前验证：

- `<script>`；
- event handler；
- `javascript:` URI；
- `<iframe>`；
- `<form>`；
- unsafe external resource；
- CSP compatibility。

模板只允许有 Template Edit 权限的管理员或模板设计者修改。

---

# 27. Error Contract

Product Facade 返回统一结构：

```json
{
  "error": {
    "code": "FIELD_VALIDATION_FAILED",
    "message": "Content data does not match the template",
    "request_id": "req_...",
    "details": []
  }
}
```

错误码：

```text
TEMPLATE_NOT_FOUND
TEMPLATE_NOT_ACTIVE
TEMPLATE_SCHEMA_INVALID
TEMPLATE_VERSION_NOT_FOUND
TEMPLATE_VERSION_CHANGED
TEMPLATE_VERSION_PRECONDITION_REQUIRED
TEMPLATE_VERSION_CONFLICT
FIELD_VALIDATION_FAILED
FIELD_REQUIRED
PATH_INVALID
PATH_CONFLICT
CONTENT_CREATE_FAILED
CONTENT_UPDATE_FAILED
CONTENT_VERSION_CONFLICT
CONTENT_PREVIEW_FAILED
CONTENT_PUBLISH_FAILED
PUBLICATION_STAGE_FAILED
PUBLICATION_VERIFY_FAILED
PUBLICATION_ACTIVATE_FAILED
PUBLICATION_UNPUBLISH_STAGE_FAILED
PUBLICATION_CONFLICT
PAGE_PUBLISH_IN_PROGRESS
PUBLIC_URL_RESOLUTION_FAILED
ASSET_IMPORT_FAILED
ASSET_TOO_LARGE
IDEMPOTENCY_CONFLICT
IDEMPOTENCY_KEY_REQUIRED
REQUEST_IN_PROGRESS
PERMISSION_DENIED
AGENT_SANDBOX_REQUIRED
RATE_LIMITED
INTERNAL_ERROR
```

不得把 MongoDB、filesystem 或网络内部错误直接返回外部调用方。

---

# 28. Webhook

继续复用现有 Webhook Engine、HMAC、retry 和 delivery history。

事件：

```text
content.create
content.update
content.publish
content.unpublish
publication.failed
```

`content.publish` 只能在 Publication 成功激活后发送。

Payload 至少包含：

```json
{
  "event": "content.publish",
  "content_id": "...",
  "content_version": 8,
  "template": "financial-news",
  "template_version": 3,
  "publication_id": "...",
  "full_path": "/news/example",
  "public_url": "https://pages.example.com/news/example",
  "published_at": "..."
}
```

Webhook 失败不回滚已激活 Publication，但必须进入现有重试和 delivery history。

## 28.1 Transactional Webhook Outbox

为保证 Idempotency retry 不重复创建发布事件，在 Publication activation 的 Mongo transaction 内写入 outbox record：

```text
webhook_outbox
- id
- event_type
- aggregate_type = publication
- aggregate_id = publication_id
- payload
- state = pending | delivering | delivered | failed
- attempt
- next_attempt_at
- created_at
- delivered_at
```

唯一索引：

```text
UNIQUE(event_type, aggregate_id)
```

事件 identity：

```text
content.publish   + publication_id
content.unpublish + publication_id
publication.failed + publication_id
```

同一 Go 进程内的 outbox worker 轮询并调用现有 Webhook Engine。交付语义是“事件只创建一次，HTTP delivery 至少一次”；接收方使用事件 ID 去重。进程在 DB commit 后、HTTP delivery 前崩溃时，重启后继续 pending event；Idempotency retry 因唯一索引不会创建第二个 publish event。

测试必须覆盖：

- commit 后进程崩溃再恢复；
- webhook 500 后重试；
- 相同 Idempotency-Key retry；
- lease takeover；
- `UNIQUE(event_type, publication_id)` 不产生重复 outbox event。

---

# 29. Audit 与 Provenance

继续使用现有 Audit 和 Version Provenance。

新增 action：

```text
template.version.create
page_generation.create
page_generation.update
publication.stage
publication.activate
publication.failed
publication.rollback
publication.unpublish
```

记录：

```text
request_id
user_id/api_key
actor
via
agent_session
content_id
content_version
template_id
template_version
publication_id
full_path
content_hash
storage_provider
duration_ms
error_code
```

Facade 必须继续通过 shared services 写 Content Version 和 provenance，不能只写 orchestration audit。

---

# 30. Rate Limit 与 Quota

## 30.1 Rate Limit

继续复用现有：

```text
300 requests/minute/token
20 requests/second/token
expensive endpoint limits
```

Generation publish endpoint建议增加独立限制，例如：

```text
60 publish requests/minute/token
```

具体值可配置。

单实例继续使用现有 limiter。多实例部署前必须改为共享 limiter 或接受每实例限流语义。

## 30.2 Quota

MVP 不实现商业 quota。

只有确认 SaaS/Partner 需要：

- tenant；
- monthly publish quota；
- storage quota；
- billing；

才增加 Partner Metadata，并建立在现有 User/API Key 上。

---

# 31. Admin UI

## 31.1 原则

继续复用 LightCMS Admin，不重写 React SPA。

运营导航收敛为：

```text
Dashboard
Pages
Templates
Assets
Publishing
Advanced
```

## 31.2 New Page

```text
Choose Template
↓
Fill Fields
↓
Preview
↓
Save Draft / Publish
↓
Copy Public URL
```

Template card 显示：

- thumbnail；
- name；
- category；
- description；
- required fields。

## 31.3 Published Page Edit

点击 Edit 时：

- unpublished：编辑 Content；
- published：创建/打开 Fork Draft；
- UI 明确显示 `Editing Draft — Live page unchanged`；
- 提供 Diff、Review、Merge、Publish。

## 31.4 Template Management

提供：

- Name；
- immutable Slug；
- Category；
- Status；
- Fields；
- HTML；
- Sample Data；
- Preview；
- Version History；
- Usage count；
- Upgrade Preview。

## 31.5 Publishing

展示：

- active Publication；
- content/template version；
- hash；
- published by/at；
- history；
- failed publication；
- rollback action。

---

# 32. API 最小集合

新增：

```http
GET  /api/v1/templates/{slug}/schema
POST /api/v1/page-generation
GET  /api/v1/content/{id}/publications
GET  /api/v1/content/{id}/publications/{publication_id}
POST /api/v1/content/{id}/publications/{publication_id}/rollback
```

继续复用：

```text
/api/v1/content/*
/api/v1/assets/*
/api/v1/templates/*
/api/v1/forks/*
/api/v1/agent-sessions/*
/api/v1/webhooks/*
/api/v1/audit
```

不为“API 漂亮”重复包装所有 LightCMS endpoint。

---

# 33. OpenAPI

提供：

```text
docs/openapi/page-generation-v1.yaml
```

至少覆盖：

- Template Schema；
- Page Generation；
- Publication list/detail/rollback；
- authentication；
- scopes；
- Idempotency-Key；
- error contract；
- rate-limit headers；
- examples。

不要求 MVP 一次性补完所有原生 LightCMS API。

---

# 34. 数据库扩展

## 34.1 Template

扩展：

```text
status
field description/example/validation
```

为现有 `templates.slug` 增加 unique index。

上线前 migration 必须检测：

- 空 slug；
- 重复 slug；
- 非法字符；
- 大小写冲突。

冲突必须生成 migration report，由管理员处理，不能静默改名。

## 34.2 New Collections

```text
template_versions
content_publications
idempotency_records
webhook_outbox
system_migrations
```

必须加入：

- database index bootstrap；
- test cleanup collection list；
- migration/version tracking；
- backup/restore documentation。

## 34.3 不新增

```text
pages
page_versions
page_users
page_api_keys
page_webhooks
page_audit
```

---

# 35. Migration

## 35.1 Template Slug

1. 扫描现有 Template；
2. 输出 invalid/duplicate report；
3. 管理员修复；
4. 创建 unique index；
5. 开启 immutable update rule。

## 35.2 Initial Template Version

为每个现有 Template 创建 Version 1：

```text
fields + html_layout + relevant metadata
```

## 35.3 Existing Published Content

为每个 published Content：

1. 定位当前 Template Version 1；
2. 读取现有静态文件或重新 render；
3. 计算 hash；
4. 创建 initial active Publication；
5. 不改变原 public path；
6. 验证 Public URL；
7. 记录 migration audit。

迁移失败的页面进入报告，不应阻塞无关页面，也不能错误标记 active。

现有静态文件可能无法准确追溯到某个历史 Content Version 或 Template 状态。迁移必须执行对账：

```text
render current Content + initial Template Version
↓
compute rendered hash
↓
compute existing canonical file hash
```

处理：

| 结果 | 行为 |
|---|---|
| hash 相同 | 创建 verified active Publication |
| canonical 不存在 | 不创建 active；标记 migration error |
| hash 不同 | 将旧 canonical 字节复制为 immutable legacy object，并创建 `verification_status=legacy_unverified` 的 active Publication |
| render 失败 | 保留旧 canonical；记录 blocking error |

MigrationStatus：

```text
verified
legacy_unverified
missing_static
render_failed
```

`legacy_unverified` 是 `content_publications.verification_status`，不是孤立的 migration record。它仍然是 active Publication，因此 scanner 不会把正在服务的旧 canonical 判定为 orphan。页面不能执行自动 template upgrade；管理员必须选择：

1. 接受当前重新渲染结果并创建新的 verified Publication；或
2. 人工确认已导入的 legacy immutable object，保持 `legacy_unverified` 并 pin 以延长精确回滚期限；或
3. 下线页面。

禁止把 hash 不匹配的旧文件静默标记为 verified。

迁移运行规则：

1. 在 `system_migrations` 写入 `publication_model_v1=running`；
2. migration flag 为 running 时，PublicationConsistencyService 只报告、不隔离“canonical exists + no active”；
3. 每个旧页面必须创建 verified 或 legacy_unverified active Publication，或者明确标记 blocking error；
4. blocking error 数量为零后才写 `publication_model_v1=completed`；
5. scanner 仅在 completed 后启用 orphan quarantine；
6. 无 active 且迁移失败的页面阻止上线 Gate，不能靠定时 scanner 自动下线。

## 35.4 Rollback

数据库 schema 采用 additive change。旧代码在正式切换前不读取新增字段。

回滚部署时：

- 保留新 collections；
- 不删除历史 metadata；
- 恢复旧 handler；
- static canonical path 保持兼容。

---

# 36. Configuration

现有：

```text
MONGO_URI
SESSION_SECRET
BASE_URL
```

新增：

```text
PUBLIC_BASE_URL                 # 可选，默认 BASE_URL
STATIC_STORAGE_PROVIDER         # MVP 仅 filesystem；R2/S3 经 Storage ADR 后启用
PAGE_GENERATION_RATE_LIMIT      # 默认 60/min
IDEMPOTENCY_TTL_HOURS           # 默认 24
PUBLICATION_RETENTION_DAYS      # 默认 90
PUBLICATION_FAILED_RETENTION_DAYS # 默认 7
PUBLICATION_SCAN_INTERVAL_MINUTES # 默认 10
PUBLICATION_STAGE_TIMEOUT_MINUTES # 默认 15
IDEMPOTENCY_LEASE_MINUTES       # 默认 5
```

R2/S3 不属于 MVP 生产配置。经 Storage ADR 和完整集成测试后，再增加对应 provider 的全部配置；R2 至少需要：

```text
R2_ACCOUNT_ID
R2_BUCKET
R2_ACCESS_KEY_ID
R2_SECRET_ACCESS_KEY
```

S3 至少需要 `S3_REGION/S3_BUCKET/S3_ENDPOINT/S3_ACCESS_KEY_ID/S3_SECRET_ACCESS_KEY`，并验证权限、重试、对象一致性与 public pointer 切换。没有完成该 ADR 前，配置验证只接受 `filesystem`。

Secret 禁止提交到仓库。

---

# 37. Observability

## 37.1 Structured Logs

新增 Facade 和 Publication 继续使用现有 JSON Logger。

字段：

```text
request_id
actor
user_id/api_key
agent_session
template_slug
template_version
content_id
content_version
publication_id
full_path
storage_provider
stage
duration_ms
status_code
error_code
```

## 37.2 Metrics

```text
page_generation_requests_total
page_generation_duration_seconds
page_generation_errors_total
publication_stage_total
publication_activate_total
publication_failed_total
publication_duration_seconds
publication_rollback_total
static_store_operations_total
static_store_errors_total
idempotency_replay_total
idempotency_conflict_total
asset_import_total
asset_import_failed_total
```

## 37.3 Alerts

- Publication failure > 5% / 5 min；
- static store error；
- active pointer inconsistency；
- MongoDB unavailable；
- CDN purge backlog；
- webhook retry backlog；
- API 5xx spike；
- staged Publication 长时间未清理。

---

# 38. 性能与容量

目标：

```text
Draft Create P95 < 1s
Preview P95 < 1.5s
Publish P95 < 3s
Public CDN HIT TTFB < 200ms
```

不包含远程 Asset 下载。

容量参考：

```text
Templates: 100
Pages: 1,000,000
Assets: 10,000,000
API keys/partners: 1,000
Daily publications: 100,000
```

要求：

- 列表查询必须 projection；
- 不加载 Content embedding；
- Publication 以 content_id/activated_at 索引；
- Template Version 不做全表扫描；
- Bulk 使用现有批量机制；
- 不用 1000 次串行 Generate 替代 Bulk。

---

# 39. 测试策略

## 39.1 Upstream Regression

每次改动：

```bash
go test ./...
go vet ./...
go build ./cmd/server
go build ./cmd/mcp
go build ./cmd/cli
```

重点：

- content create/update；
- preview；
- versions；
- fork/sandbox；
- bulk/upsert；
- asset；
- webhook；
- approval；
- OAuth/API key scopes；
- publish/unpublish；
- Cloudflare cache。

## 39.2 Product Unit

覆盖：

- Template slug validation；
- Field validation；
- JSON Schema conversion；
- Public URL resolver；
- Idempotency hashing/replay/conflict；
- Publication state transitions；
- Filesystem atomic store；
- error mapping。

新增 Product 代码覆盖率目标：

```text
>= 85%
```

## 39.3 Draft / Live Integration

```text
publish v1
edit v2 through Fork
assert live still v1
merge
assert live behavior per ADR
publish v2
assert live is v2
```

## 39.4 Template Version Integration

```text
publish with template v1
create template v2
assert live still uses v1
preview upgrade
publish with v2
rollback to v1 publication
```

## 39.5 Failure Injection

模拟：

- render error；
- stage write error；
- verify mismatch；
- activate/rename failure；
- DB pointer failure；
- CDN purge failure；
- webhook failure；
- process crash after stage；
- process crash after activate before response。
- unpublish canonical rename failure；
- unpublish DB transaction failure and compensation；
- unpublish crash after DB commit；

所有场景验证旧 live 可用、状态一致、操作可重试。

Crash recovery 测试必须重启完整 LightCMS 进程，验证 startup scanner 能根据 DB active Publication 和 immutable object 重建 canonical projection。

## 39.6 Idempotency

- same key/same body replay；
- same key/different body 409；
- concurrent same key；
- response lost then retry；
- no duplicate Content Version；
- no duplicate Publication；
- no duplicate publish webhook。
- long-running publish heartbeat renews lease；
- stale worker loses lease and stops side effects；
- 403/409/429/5xx are not replay-cached；
- 400/422 pure validation may be replay-cached；
- uncertain crash takeover 复用当前 attempt 的 publication_id 和 logical_published_at；已确认 terminal pre-activation failure 的业务重试使用新的 attempt、Publication ID 和逻辑时间。
- crash before Render recovers the durable execution snapshot；
- terminal pre-activation failure followed by the same key creates a new attempt and Publication ID；
- rollback and legacy single publish replay do not create another Publication；
- `mode=preview` ignores Idempotency-Key and creates no record。

## 39.7 Agent/Sandbox

- sandbox-only key cannot publish live；
- Agent edits Fork only；
- live unchanged before merge/publish；
- diff correct；
- merge correct；
- session ledger correct；
- rollback correct；
- provenance correct。

## 39.8 Security

- no auth / invalid / revoked key；
- scope denied；
- sandbox escape；
- path traversal；
- public URL injection；
- stored XSS；
- script policy；
- template XSS；
- SSRF/DNS rebinding/redirect；
- oversized/fake asset；
- Mongo query injection；
- idempotency collision；
- error leakage。

权限测试必须验证 `mode=publish` 缺 publish 权限时为零副作用：Content、Fork、Version、Publication、static object 和 outbox 数量均不变化。

还必须验证 create-only、edit-only、publish-only key 分别不能越权执行组合 Publish 命令，所有失败均无 Content/Version/Publication 副作用。

## 39.9 Legacy Entry Point Contract

以下每个入口必须断言最终创建 `content_publications`，且不存在直接 `GenerateStaticPage` live 写入：

```text
REST single publish
REST batch publish
MCP publish_content
MCP publish_multiple
Admin publish
CLI publish
Scheduled publish
Import auto-publish
Copilot publish
Search/replace auto-republish
Template upgrade
Fork merge then explicit publish
```

使用依赖注入的 fake StaticPageStore/PublicationRepository 验证所有入口进入同一个 PublicationService。

## 39.10 Content Mutation Policy

对 Admin、REST、MCP、CLI、Copilot、Import、Search Replace 分别验证：

- published Content 普通编辑不改变 canonical file；
- active Publication ID 不变化；
- `HasUnpublishedChanges=true`；
- 默认创建/更新 Fork 的入口行为符合矩阵；
- direct-live draft 需要专门权限；
- 任何 update 都不会隐式发送 publish webhook。

## 39.11 Publication Immutability 与 Rollback

- Rename 创建新 Content Version 和新 Publication；
- 旧 Publication full_path/storage metadata 不变化；
- 90 天内 Exact Rollback 恢复字节完全一致 HTML；
- pinned Publication 不被 GC；
- 超过 retention 后只提供 Re-render Rollback，并明确非字节级保证；
- Theme/Snippet/Markdown/Wikilink/TOC 依赖 hash 进入 Publication；
- 相同 Publication 只创建一个 publish outbox event。
- Unpublish 的生命周期、Content 投影和 outbox 在同一 Mongo transaction；
- unpublish 后 immutable object 在 90 天内仍可 Exact Rollback；
- `legacy_unverified` active 页面不会被 scanner 当 orphan quarantine；
- schema v3 与 publish 前模板升级 v4 返回 409 且零副作用；
- 两个并发新建请求争夺同一 `CanonicalFullPath` 时仅一个成功；
- Rename 获取多路径锁按固定顺序，无 deadlock。

---

# 40. 部署拓扑

## 40.1 MVP

```text
Cloudflare / Reverse Proxy
          ↓
Single LightCMS Application
├── Admin Web UI
├── REST API
├── MCP Backend
├── Scheduler / Import / Recovery Jobs
├── Static File Serving
├── MongoDB Replica Set
└── Persistent Filesystem
```

以上能力位于同一个 Go 可执行文件和同一个运行进程。Admin UI 由该进程直接提供，不交付第二个前端应用。Filesystem 必须支持 atomic activation。

## 40.2 Scale-out

```text
Cloudflare
    ↓
Multiple LightCMS Instances
    ├── MongoDB Replica Set
    └── R2/S3 Static Store
```

Scale-out 只增加同一个应用二进制的副本，不拆分 Generation、Publication、MCP、Admin 或 Scheduler 为独立服务。定时任务通过 Mongo lease 保证同一任务只有一个实例执行。

多实例前必须解决：

- shared publish lock；
- shared rate limit 或明确 per-instance 限制；
- active publication pointer；
- object activation；
- cache purge consistency。

不默认引入 Redis、Kafka 或独立 Queue。

---

# 41. 实施目录

推荐新增：

```text
internal/product/
├── templatecontract/
│   ├── schema.go
│   ├── validation.go
│   └── version.go
├── generation/
│   ├── service.go
│   ├── types.go
│   └── errors.go
├── publication/
│   ├── service.go
│   ├── model.go
│   ├── repository.go
│   └── state.go
├── publicurl/
│   └── resolver.go
├── storage/
│   ├── store.go
│   └── filesystem.go
├── idempotency/
│   ├── service.go
│   └── middleware.go
└── httpapi/
    ├── generation.go
    ├── templates.go
    └── publications.go
```

尽量不修改现有 public function signature。必须修改 Core 时保持 patch 小而集中。

---

# 42. ADR

下列五份 ADR 是核心代码任务的进入条件，保存在 `docs/adr/`，由集成负责人起草，技术负责人审批；涉及数据迁移、权限或对外 API 的 ADR 还须分别由运维、安全或 API 负责人会签。

| ADR | 决策范围 | 必须写明的证据与交付 | 会签方 |
|---|---|---|---|
| `ADR-001-draft-mutation.md` | Published Content、Fork Merge、全部旧写入口 | 当前 `UpdateContent` 副作用清单、Mutation Policy 矩阵、Breaking Change、旧客户端迁移 | API/安全 |
| `ADR-002-template-version.md` | ContractHash、版本分配、Schema、显式升级 | Template 字段快照、CAS/索引、升级预览和逐页发布、历史版本处理 | API |
| `ADR-003-publication-saga.md` | Publication 状态机、Mongo transaction、Outbox、Publish/Unpublish/Rename/Rollback | 各失败点补偿表、权限与幂等边界、崩溃恢复、故障注入结果 | 运维/安全 |
| `ADR-004-filesystem-store.md` | immutable object、canonical cutover、scanner、GC | 文件布局、rename/fsync 顺序、恢复算法、持久卷与容量要求 | 运维 |
| `ADR-005-idempotency.md` | execution snapshot、lease、attempt、所有 live-changing 命令 | 状态转换、TTL、同 key 冲突/重放、terminal retry、outbox 去重 | API/安全 |

每份 ADR 使用相同结构：背景和 LightCMS v7 代码证据、候选方案及取舍、最终决定、精确接口与数据字段、迁移及回滚、失败和并发场景、验证命令与结果、责任人与审批记录。不得仅提交标题空文件。

本设计已经固定产品行为，ADR 负责落实代码级边界，不得重新引入直接覆盖 live 文件或“已发布 Content 普通保存即上线”。G1 前五份 ADR 必须获得对应审批；未审批的方案不能交给并行开发者各自解释。

---

# 43. EPIC 与开发顺序

## EPIC-00：Current-State Runtime Audit

- Pin commit/version；
- REST route inventory；
- MCP tools/list inventory；
- scopes inventory；
- sandbox trace；
- publish/static trace；
- runtime baseline report。

Exit Gate：实际 runtime 与本文复用假设一致。

## EPIC-01：Template Contract

- Slug validation/unique index；
- migration report；
- Template status；
- Field metadata；
- strict validator；
- url/number/boolean complete field support；
- JSON Schema Adapter；
- schema endpoint；
- tests。

## EPIC-02：Template Version

- model/index；
- initial migration；
- version creation；
- immutable read API；
- template update behavior；
- Admin history；
- tests。

## EPIC-03：Atomic Static Store

- StaticPageStore；
- Filesystem Stage/Verify/Activate；
- atomic rename；
- abort/restore/open；
- crash recovery scanner；
- retention/GC；
- delete/rename handling；
- failure injection；
- regression tests。

## EPIC-04：Publication

- model/index；
- state machine；
- orchestrator；
- legacy publish entry-point migration；
- Content published projection transaction；
- active pointer；
- unpublish/delete/restore；
- rollback；
- webhook/audit；
- migration for existing published content。

## EPIC-05：Generation Facade

- contract；
- exact omission/null/replace semantics；
- template resolver；
- Content/Fork routing；
- draft/preview/publish/sandbox；
- upsert mapping；
- URL resolver；
- errors；
- auth/scope/provenance；
- OpenAPI；
- tests。

## EPIC-06：Idempotency

- model/index/TTL；
- middleware/service；
- request hash；
- processing lock；
- processing lease/crash takeover；
- replay/conflict；
- publication side-effect tests。

## EPIC-07：Admin UX

- Template selector；
- field help；
- preview；
- published-page Fork editing；
- diff/review/publish；
- publication history；
- copy URL。

## EPIC-08：Agent Integration

- template schema MCP；
- sandbox Agent；
- trusted publish Agent；
- bulk；
- session ledger/rollback；
- runbook。

## EPIC-09：Security Hardening

- remote asset size fix；
- SSRF regression；
- script policy；
- path/public URL injection；
- scope/sandbox bypass；
- error sanitization。

## EPIC-10：Documentation and Operations

- API guide；
- Template guide；
- Agent integration；
- deployment；
- operations；
- backup/restore；
- upstream sync；
- security runbook。

## Optional EPIC-11：R2/S3

只有部署决策要求时启动。

---

# 44. Definition of Done

每个新增 Feature 必须：

- 有对应 Requirement/Gap；
- 不复制现有 LightCMS Service；
- 有 Unit Test；
- 有 Integration Test；
- 有 failure-path test；
- 经过 RBAC/scope 检查；
- 确认 sandbox 行为；
- 写入 Audit/Provenance；
- 使用 structured logging；
- 有 API/OpenAPI 文档；
- 有 migration 和 rollback plan；
- `go test ./...` 通过；
- `go vet ./...` 通过；
- server/MCP/CLI build 通过；
- upstream regression 通过；
- security review 通过。

---

# 45. MVP 验收标准

## 45.1 Template

- [ ] Template Slug 唯一、合法、默认不可修改；
- [ ] Template 有 category/status；
- [ ] Field 有 description/example/validation；
- [ ] Schema endpoint 输出确定的 JSON Schema；
- [ ] Template Version 不可变；
- [ ] 修改 Template 不改变旧 Publication。

## 45.2 Page/Draft

- [ ] 新页面可保存未发布草稿；
- [ ] 已发布页面编辑自动进入 Fork；
- [ ] Fork 保存不改变 live；
- [ ] Fork preview 正常；
- [ ] Diff/Merge 正常；
- [ ] Merge/Publish 语义符合 ADR。

## 45.3 Publish

- [ ] 发布引用准确 Content Version；
- [ ] 发布引用准确 Template Version；
- [ ] static output stage/verify/activate；
- [ ] 激活前旧 live 始终可用；
- [ ] 发布失败不改变 active Publication；
- [ ] 成功后才返回 `published=true`；
- [ ] 成功后才发送 publish webhook；
- [ ] Public URL 可匿名访问；
- [ ] Unpublish 后 URL 不再提供 active 内容；
- [ ] rollback 可恢复准确历史输出。

## 45.4 REST

- [ ] Template Schema API 正常；
- [ ] Generation draft/preview/publish/sandbox 正常；
- [ ] strict validation 正常；
- [ ] Public URL 正确；
- [ ] errors 不泄露内部信息；
- [ ] Idempotency replay/conflict 正常；
- [ ] scopes/RBAC/sandbox 正常；
- [ ] OpenAPI 可用于独立接入。

## 45.5 MCP/Agent

- [ ] Agent 可读取 Template Schema；
- [ ] Agent 可创建和预览；
- [ ] sandbox-only Agent 不修改 live；
- [ ] Fork diff/human merge 正常；
- [ ] trusted Agent 可原子发布；
- [ ] Agent Session ledger 正常；
- [ ] Agent rollback 正常；
- [ ] Bulk 正常。

## 45.6 Security

- [ ] Script Policy 正常；
- [ ] SSRF tests 通过；
- [ ] redirect-to-private 被阻止；
- [ ] remote asset 超限不保存截断内容；
- [ ] path traversal 被阻止；
- [ ] public URL host injection 被阻止；
- [ ] sandbox escape tests 通过；
- [ ] scope bypass tests 通过；
- [ ] stored/template XSS tests 通过。

## 45.7 Regression

- [ ] `go test ./...`；
- [ ] `go vet ./...`；
- [ ] server/MCP/CLI build；
- [ ] Cloudflare/cache；
- [ ] Webhook retry；
- [ ] Approval；
- [ ] OAuth；
- [ ] existing Content API；
- [ ] import/bulk/upsert。

---

# 46. 上线 Gate

上线前记录：

```text
UPSTREAM_SHA
PRODUCT_SHA
BUILD_VERSION
MIGRATION_VERSION
OPENAPI_VERSION
TEST_REPORT
SECURITY_REVIEW
ROLLBACK_RUNBOOK
```

必须完成：

1. Template slug migration report 无未处理冲突；
2. Initial Template Version migration 完成；
3. Existing published Content migration 完成；
4. active Publication consistency scan 通过；
5. staging failure injection 通过；
6. production-like E2E 通过；
7. backup/restore 演练通过；
8. rollback 演练通过；
9. Agent sandbox regression 通过；
10. Security Review 签字。

---

# 47. 上游同步策略

保留：

```text
origin   = company fork
upstream = jonradoff/lightcms
```

每月至少：

```bash
git fetch upstream
git log HEAD..upstream/main --oneline
```

升级流程：

```text
upgrade branch
↓
merge upstream
↓
resolve minimal conflicts
↓
upstream tests
↓
product tests
↓
MCP/sandbox tests
↓
publication failure tests
↓
staging
↓
production
```

产品扩展优先放在 `internal/product/`，Core patch 保持小而集中。

---

# 48. 第三方交付物

```text
Source Code
Database Migrations
ADR Documents
OpenAPI
README-product.md
API.md
AGENT-INTEGRATION.md
TEMPLATE-GUIDE.md
PUBLICATION-RUNBOOK.md
DEPLOYMENT.md
OPERATIONS.md
BACKUP-RESTORE.md
UPSTREAM-SYNC.md
SECURITY.md
Test Report
Security Review Report
```

所有示例必须可以直接执行，不接受只有接口名称但没有认证、请求和响应示例的文档。

---

# 49. 实施决策总表

| 能力 | 最终决策 |
|---|---|
| Page | 复用 Content |
| Published Draft | 使用 Fork |
| Agent Draft | 使用 Agent Sandbox/Fork |
| Content Version | 复用 |
| Template Slug | 强化现有字段 |
| Template Category | 复用 |
| Field Metadata | 扩展现有 TemplateField |
| JSON Schema | 新增纯 Adapter |
| Strict Validation | Product Layer 新增 |
| Template Version | 新增不可变版本 |
| Renderer | 复用 |
| Publish | 新增原子 Orchestrator |
| Publication | 新增最小记录，不建立 Page Domain |
| Filesystem | 原子化改造 |
| R2/S3 | 部署需要时可选 |
| Public URL | Base URL + canonical path |
| Generate API | 薄 Facade |
| HTTP Idempotency | External publish 必需 |
| REST | 复用 |
| MCP | 复用，最多增加薄工具 |
| OAuth/API Key/scopes | 复用 |
| Agent Governance | 复用 |
| Audit/Webhook/Approval | 复用并扩展事件 |
| Asset | 复用并修复大小限制 |
| SaaS Quota | 延后 |
| New Page DB | 禁止 |
| New Auth/MCP/Audit Engine | 禁止 |

---

# 50. 最终实施结论

本项目的最终形态是：

```text
LightCMS v7
+
Stable Template Contract
+
Immutable Template Version
+
Strict Generation Facade
+
Fork-based Published Draft
+
Atomic Publication Orchestrator
+
Stable Public URL
```

LightCMS v7 继续负责：

- CMS Core；
- Content；
- Template 基础模型；
- Renderer；
- Version；
- Fork/Sandbox；
- REST/MCP；
- Auth/Scopes；
- Audit/Webhook/Approval；
- Assets/Bulk/Import；
- Cache/Analytics。

产品扩展层负责：

- 稳定 Template Slug 和字段契约；
- JSON Schema；
- Template Version；
- 严格验证；
- 一步 Page Generation；
- Draft/Live 路由；
- Publication stage/verify/activate；
- active Publication 和精确回滚；
- Public URL；
- HTTP Idempotency；
- 运营 UX。

最终必须保证：

```text
编辑草稿不改变线上页面
模板修改不改变历史发布
发布失败不破坏旧页面
发布成功结果真实可信
每个线上版本可追溯、可回滚
REST 和 Agent 可安全自动化
不重复建设 LightCMS 已有核心能力
```
