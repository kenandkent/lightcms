# LightCMS REST API — 第三方集成指南（模板与内容管理）

适用对象：需要在 LightCMS 中**创建/编辑/管理模板与内容**的第三方系统。
全量机器可读契约见 `docs/openapi/page-generation-v1.yaml`（V3 发布管线）；
本文是面向人类的操作指南，只覆盖模板与内容管理必需的部分。

- 基础地址：`https://<你的域名>/api/v1`（本文示例用 `https://ibreeze.agency/api/v1`）
- 认证：HTTP Bearer（API Key）。Key 继承其属主用户的 RBAC 权限
  （admin 全权 / editor 内容资产 / viewer 只读）——报 403 先查 Key 属主角色。
- 限流：每个 Key **300 请求/分钟**（滑动窗口）+ **突发 20 请求/秒**；
  超限返回 429 并带 `Retry-After` 头，请指数退避重试。
- 请求体一律 `Content-Type: application/json`；单请求体上限 10 MiB。

约定：下文 `curl` 示例中 `$BASE=https://ibreeze.agency/api/v1`，
`$KEY` 为你的 API Key。

```bash
BASE=https://ibreeze.agency/api/v1
KEY=lc_xxx   # 你的 Key
```

## 1. 通用约定

### 1.1 错误体（两套，不可混用）

经典 CRUD 接口（模板/内容增删改查）返回**扁平信封**：

```json
{ "error": "人话描述", "code": "MACHINE_CODE" }
```

V3 发布管线（`/page-generation` 及 publications 相关）返回**嵌套信封**：

```json
{ "error": { "code": "MACHINE_CODE", "message": "人话描述" } }
```

解析时：先看 `error` 是字符串还是对象，再取 code。常见 code：

| HTTP | code | 含义与对策 |
|---|---|---|
| 400 | `INVALID_REQUEST` | 参数缺失/非法，看 message 补参数 |
| 401 | `UNAUTHENTICATED` | Key 缺失/无效/格式非 `Bearer <key>` |
| 403 | `PERMISSION_DENIED` | Key 属主角色不够，或沙盒 Key 越权（见 1.4） |
| 404 | `NOT_FOUND` | ID/slug/路径不存在 |
| 409 | `PATH_CONFLICT` / `PUBLICATION_CONFLICT` | 规范路径已被占（路径比较**不分大小写**） |
| 422 | `PATH_INVALID` / `FIELD_VALIDATION_FAILED` | 路径非法或模板字段校验失败 |
| 428 | `IDEMPOTENCY_KEY_REQUIRED` | 发布类请求缺 `Idempotency-Key` 头 |
| 429 | `RATE_LIMITED` | 触发限流，按 `Retry-After` 等待重试 |

### 1.2 分页

列表接口（内容）支持 `?limit=N&offset=M`（N 最大 500）。带 `limit` 时返回信封：

```json
{ "items": [...], "total": 123, "limit": 100, "offset": 0, "has_more": true }
```

不带 `limit` 则直接返回数组（兼容老客户端，大站慎用）。
注意：**模板列表（`GET /templates`）恒返回裸数组**，不走上面这个信封。

### 1.3 幂等与重放

- 写操作尽量带业务幂等键：发布管线要求 `Idempotency-Key` 请求头（见 3.2）；
  同一 Key 重放返回**首次结果**，不会重复建页。
- 内容创建支持 `upsert: true`：同路径已存在则转为更新，失败重试安全。

### 1.4 路径规范（重要）

- 站内路径以 `/` 开头，不允许 `? # \ //`、尾斜杠（根 `/` 除外）与 `.`/`..` 段。
- **路径按小写 + Unicode NFC 归一后比较**：`/News/Foo` 与 `/news/foo`
  是同一页面（后者胜出者落盘，前者 409）。静态文件一律落在
  `generated/` 下的小写路径（如 `/news/foo.html`）。
- `public_url` 永远是规范小写 URL，直接可用。

### 1.5 版本注释

经典 CRUD 的创建/更新/回滚建议带 `version_comment`（一句话说明改了什么），
版本历史可读。发布管线（`/page-generation`）**不接受**该字段（传了会 422，
见 3.2），它会自动生成版本注释。系统为每次更新建版本，可随时回滚（见 3.4）。

## 2. 模板管理

模板 = 字段定义（JSON Schema）+ HTML 布局。页面发布时按模板渲染成静态 HTML。

### 2.1 字段类型

`fields` 数组每项：`{name, label, type, required}`。
`type` 取值：`text` `textarea` `richtext` `markdown` `date` `image` `select`。
`markdown` 类型在发布时渲染为 HTML（含 GFM 表格/删除线/任务列表）。

布局中用 `{{.字段名}}` 取值，另有 `{{.Title}} {{.FullPath}} {{.Slug}}`
`{{.MetaDescription}} {{.PublishedAt}}` 等内置变量；
`{{.lc_toc}}` 位置自动生成目录。

### 2.2 创建模板

```bash
curl -X POST $BASE/templates \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{
    "name": "产品分析",
    "slug": "product-analysis",
    "description": "产品 deep-dive",
    "category": "reports",
    "fields": [
      {"name": "headline", "label": "标题", "type": "text", "required": true},
      {"name": "body", "label": "正文", "type": "markdown", "required": true},
      {"name": "cover", "label": "封面", "type": "image", "required": false}
    ],
    "html_layout": "<article><h1>{{.headline}}</h1><div>{{.body}}</div></article>"
  }'
# 201 返回模板对象（含 id，注意首个版本号为 1）
```

必填：`name`、`slug`（全站唯一，建议小写短横线）。

### 2.3 列表 / 获取 / 更新 / 删除

```bash
curl "$BASE/templates?limit=100" -H "Authorization: Bearer $KEY"
curl $BASE/templates/<id> -H "Authorization: Bearer $KEY"

# 更新（只传要改的字段；每次更新版本号 +1，发布需带新版本号见 3.2）
curl -X PUT $BASE/templates/<id> \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"description": "新描述"}'

# 删除（有页面在用时拒绝；系统模板拒绝，见下）
curl -X DELETE $BASE/templates/<id> -H "Authorization: Bearer $KEY"
```

限制：
- `is_system=true` 的系统模板**不可删除**（400）。
- 仍有内容引用时不可删除（400，提示引用数）——先删/改内容。

### 2.4 取模板 Schema（发布前校验用）

```bash
curl $BASE/templates/product-analysis/schema -H "Authorization: Bearer $KEY"
# 返回该模板当前版本的字段约束，第三方表单可据此动态生成
```

## 3. 内容管理

LightCMS 有**两套内容写入口**，选型如下：

| 场景 | 用哪个 |
|---|---|
| 简单建页/改草稿/删页/上下线 | 经典 CRUD（3.1） |
| 正式发布（生成静态 HTML 上线） | 发布管线 `/page-generation`（3.2，推荐生产发布） |

两套共用同一内容库与版本历史。

### 3.1 经典 CRUD

```bash
# 列表（支持 category / folder_id / include_deleted / include_data 过滤）
curl "$BASE/content?limit=100&offset=0" -H "Authorization: Bearer $KEY"

# 按路径取（不用先查 id）
curl "$BASE/content/by-path?path=/reports/demo" -H "Authorization: Bearer $KEY"

# 创建（template_id/title/slug 必填；folder_path 如 /reports；upsert 防重）
curl -X POST $BASE/content \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{
    "template_id": "<模板id>",
    "title": "示例页",
    "slug": "demo",
    "folder_path": "/reports",
    "data": {"headline": "标题", "body": "# 正文"},
    "version_comment": "首版",
    "upsert": true
  }'
# 201 返回内容对象（含 id 与 full_path=/reports/demo）

# 更新（草稿状态可改；已发布页的正文更新走发布管线重发）
curl -X PUT $BASE/content/<id> \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"title": "新标题", "version_comment": "改标题"}'

# 上线 / 下线（经典路径；大流量发布建议 3.2）
curl -X POST $BASE/content/<id>/publish -H "Authorization: Bearer $KEY"
curl -X POST $BASE/content/<id>/unpublish -H "Authorization: Bearer $KEY"

# 删除（软删进回收站）
curl -X DELETE $BASE/content/<id> -H "Authorization: Bearer $KEY"
```

### 3.2 发布管线（创建即发布，推荐）

一次调用完成"建页 + 渲染 + 上线"，返回可直接访问的 `public_url`：

```bash
curl -X POST $BASE/page-generation \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -H "Idempotency-Key: <每次发布唯一的键，如 uuid>" \
  -d '{
    "template": "product-analysis",
    "title": "Q3 产品复盘",
    "slug": "q3-review",
    "folder_path": "/reports",
    "mode": "publish",
    "expected_template_version": 1,
    "data": {"headline": "Q3 复盘", "body": "# 要点\n\n- ..."}
  }'
```

要点（缺一即 4xx，message 会明说）：
- 请求体是**严格模式**：多传未知顶层字段直接 422（`FIELD_UNKNOWN`），
  字段以本文和 2.4 的 Schema 为准，不要自行加料。
- `template` 填模板 **slug**；`title`/`slug`/`folder_path`/`data` 按模板 Schema 填。
- `mode`: `draft` 只存草稿；`publish` 直接上线。
- `expected_template_version`：发布必填，取自模板当前版本号（模板改版后旧号发布会被拒绝，防止按过期结构渲染）。
- `Idempotency-Key`：发布必填。网络超时重试时**沿用同一 Key**，服务端返回首次结果，绝不重复建页。
- 成功返回含 `id` `full_path` `public_url` `content_version`；`public_url` 即线上地址。

### 3.3 并发与冲突

- 同一规范路径的并发创建：胜者 201，败者 **409 + `PATH_CONFLICT`**——客户端应把 409 当"已存在"处理（读回现页即可），不要当错误重试风暴。
- 先调 `GET /content/by-path?path=...` 再决定创建/更新，是最省事的防冲突姿势。

### 3.4 版本与回滚

```bash
curl $BASE/content/<id>/versions -H "Authorization: Bearer $KEY"
curl $BASE/content/<id>/versions/<n> -H "Authorization: Bearer $KEY"
curl -X POST $BASE/content/<id>/versions/<n>/revert \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"version_comment": "回滚到 n"}'
```

### 3.5 批量发布

```bash
curl -X POST $BASE/content/batch-publish \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"ids": ["<id1>", "<id2>"]}'
# 另支持 {"publish_all_drafts": true} 发布全部草稿
```

### 3.6 跳转（Redirects）与根路径

`slug` 在两套写入口都**必填非空**，因此根路径 `/` 无法直接建页。
站点根访问的标准做法是一条跳转：

```bash
# 列表
curl $BASE/redirects -H "Authorization: Bearer $KEY"

# 新建（from_path/to_path 必填；status_code 如 301）
curl -X POST $BASE/redirects \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"from_path": "/", "to_path": "/reports/flagship",
       "status_code": 301, "description": "root to flagship"}'
```

访问时按 `from_path` 精确匹配后按 `status_code` 跳转（页面不存在时先生效，
先生效于 404）。同理可做旧路径迁移（改版换 slug 时保留外链）。

## 4. 端到端示例：新模板 + 新页面上线

```bash
# 1) 建模板（记下返回的版本号，首次为 1）
curl -X POST $BASE/templates -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" -d @template.json

# 2) 发布页面（幂等键每次新页面换一个新的）
curl -X POST $BASE/page-generation \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -H "Idempotency-Key: 550e8400-e29b-41d4-a716-446655440000" \
  -d '{"template":"product-analysis","title":"Q3 复盘","slug":"q3-review",
       "folder_path":"/reports","mode":"publish","expected_template_version":1,
       "data":{"headline":"Q3 复盘","body":"# 正文"}}'

# 3) 验证线上（返回 200 即成功）
curl -o /dev/null -w "%{http_code}\n" https://ibreeze.agency/reports/q3-review
```

`examples/templates/` 下有 9 套可直接套用的模板 JSON
（`*.template.json` 建模板）与配套内容示例（`*.example.json`，
把 `mode` 改 `publish` 并加上 `expected_template_version` 即可照抄发布）。

## 5. 排障速查

- 401：Key 错/没传 `Bearer ` 前缀。
- 403：Key 属主是 viewer，或沙盒 Key 想碰正式内容/发布/删除。
- 409 `PATH_CONFLICT`：换个 slug，或按 3.3 读回现页。
- 422 `FIELD_VALIDATION_FAILED`：`data` 对不上模板 Schema，先调 2.4 对字段；
  若是 `FIELD_UNKNOWN`，说明请求体有多余顶层字段（`page-generation` 不收
  `version_comment`/`author` 这类，删掉）。
- 428：发布忘记 `Idempotency-Key` 头。
- 发布报 `TEMPLATE_VERSION_PRECONDITION_REQUIRED`：补 `expected_template_version`
 （模板每次更新版本号都会变，发布前重取）。
- 429：降速重试（看 `Retry-After`）。
- 公共只读场景（访客侧 Agent）另有免认证的 `/mcp-public` 与 `/llms.txt`，
  不在本指南范围。
