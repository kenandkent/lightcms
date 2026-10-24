# 静态内容导出与 Cloudflare Workers 多账号部署设计

## 2026-10-21 已批准调整：纯静态资源，不保留 JavaScript 兜底

本节覆盖下文“静态优先”模式中仍执行 health/fallback 脚本的约定。

- 新发布包固定 `delivery_mode=assets-only-v1`；已有冻结的 `assets-first-v1`
  和无模式字段的旧包仍按原协议读取，不在执行期间偷换包内容。
- 新初始化和新版本上传只发送 metadata，不包含 `main_module`、JavaScript
  module 或运行时 bindings。assets.config 不设置 `run_worker_first`，保持
  `html_handling=none`、`not_found_handling=none`；不存在的地址由平台原生返回404。
- `/.__lc_delivery/health` 是原生 JSON
  静态文件，保留协议、installation、target、release 字段，并设置 `no-store`。
  它只在初始化、发布核验或用户主动检查时访问，不新增定时公网轮询。
- 归属写入版本 annotation `workers/tag=lc1:<installation>:<target>:<release>`，
  从 Version API 的顶层 `annotations` 读取；无脚本 ETag 时以不可变 version ID
  做漂移基线。已有脚本版本的 binding 归属识别保持兼容。
- 上传前文件数计入额外 health 文件。HTML、正文、域名绑定、原 CMS 部署方式
  和发布确认/并发控制/失败恢复逻辑不变。保留完整字节校验及三轮公开检查。
- “静态请求不执行用户脚本”不表示没有平台使用限制；文件数量、单文件大小等
  仍遵循 Cloudflare 套餐和项目配额。部署统计也不会清空历史请求。

实施计划：[纯静态发布调整](../plans/2026-10-21-assets-only-delivery.md)。

## 2026-10-21 调整：失败释放、五分钟上限与自动导出

本节覆盖下文旧的手动创建导出步骤、30分钟任务超时和激活前失败持续占锁要求。
发布页选择目标后点击预览，即自动导出并等待完成，然后生成预览；确认发布仍是
独立动作。导出失败不得继续预览或上传。独立导出 API 和成功后的下载保留。

任务从持久化 created_at 累计最多五分钟，重启不重置；旧的更大超时配置按五分钟
执行。尚无激活意图的失败/超时，事务内标记 failed 并释放自身目标占用，保留错误码。
历史 blocked 任务仅在无激活意图且租约过期时自动释放。已经发起激活的超时保留
恢复状态和占用，checkpoint.deadline_exceeded=true，页面显示超时待核实；只核实
而不重复激活，确认真实成功后可恢复 succeeded。不能因本地超时谎报远端未发布。

模块协议修正：附加配置JSON是内部冻结描述，仅转换为assets metadata，不作为
Worker模块上传。routes.json按text/plain上传，入口解析其字符串；避免Cloudflare
10162 unsupported Content-Type application/json。详见[诊断与实测](../../reviews/2026-10-20-cloudflare-version-upload-diagnosis.md)。

## 2026-10-20 已批准调整：普通报告静态资源优先

用户明确要求部署产物是独立HTML静态页面，并批准定向调整现有发布流程。本节覆盖本文旧的“所有读取先执行Worker”及“所有ETag等于SHA-256”的要求，不改变原CMS生命周期、账号/目标管理、任务幂等/确认、版本激活与恢复契约。

- 新release冻结 `delivery_mode=assets-first-v1`；字段缺失表示旧脚本优先模式。不能在执行旧冻结任务时临时套用新配置。新预览在保留整个站点文章后生成静态优先候选包；没有用户确认不迁移远端。
- 私有产物仍用现有目录布局，导出ZIP/HTML不变；上传manifest将每个HTML映射为原canonical路径，并明确发送 `text/html` MIME。首页文件映射为 `/.__lc_delivery/root.html`，通过静态 `_redirects` 的 `/ /.__lc_delivery/root.html 200` 保持公开 `/` 不跳转。HTML handling与SPA fallback均为none，避免 `/a`、`/a.html`、中文路径被隐式合并。
- multipart版本上传的assets.config携带 `run_worker_first=["/.__lc_delivery/health"]`、原生 `_headers` 和首页 `_redirects`。这些不是公开上传的额外控制文件；配置以附加JSON模块随冻结版本携带并受模块大小上限约束。正式路径命中静态资源时不运行用户Worker；旧别名/未命中路径走原fallback逻辑，仍可能计入Worker请求。
- 原生 `_headers` 使用常数数量的规则提供 `X-LC-Release`、nosniff、测试noindex及hash资源immutable缓存，不按文章生成100规则上限以内的逐页表。HTML沿用平台 `public, max-age=0, must-revalidate`，允许等价no-cache。不覆盖平台ETag为自定义hash。
- 发布后依旧三轮核验health/版本标识、全部文件GET字节hash/MIME/缓存及页面HEAD。新模式接受平台ETag格式，但完整性依实际字节SHA-256，不能仅凭ETag或HTTP200报成功；旧模式保留原严格ETag校验。
- 正常报告访问不回源CMS，不运行服务器渲染。100%免脚本配额不是承诺：health、fallback、旧别名和无效URL仍可执行脚本。静态文件数量、单文件大小、模块大小及项目内存预算继续有效。自定义域名仍在Cloudflare绑定，不自动修改DNS。

依据：Cloudflare官方 [Direct Uploads](https://developers.cloudflare.com/workers/static-assets/direct-upload/)、[HTML handling](https://developers.cloudflare.com/workers/static-assets/routing/advanced/html-handling/)、[Headers](https://developers.cloudflare.com/workers/static-assets/headers/)、[Redirects](https://developers.cloudflare.com/workers/static-assets/redirects/)。本地使用官方Miniflare/workerd验证原生静态路由；不把本地验证称为真实线上发布。

- 版本：1.1（含2026-10-20用户批准的静态优先调整；以下为原设计及其历史基线）
- 日期：2026-10-19
- 项目根目录：`/Users/ken/workspace/newsPage`
- 核对代码基线：`c73c1c8a778857f0f79e7a6f71e86df6d9081b99`，另有既存模板工作区修改；不是仅针对干净 HEAD 的审查。
- 状态：设计文档；本文不表示功能已实现、Cloudflare 凭证已验证或远端部署已成功。
- 配套审查：[设计 review 与修正记录](../../reviews/2026-10-19-static-export-cloudflare-workers-review.md)。

## 1. 目标、范围与不变项

新增三项能力：独立静态报告导出、多个 Cloudflare 账号管理、选择账号及 Worker 部署报告。最终读者通过 Workers 访问完整静态产物，不依赖 CMS 在线。

“不改现有功能”指现有行为和运维契约保持不变，不代表禁止新增路由、后台入口、新集合和任务启动接线。

| 必须保持 | 本次允许新增 | 明确不做 |
| --- | --- | --- |
| 现有本地/测试/服务器启动方式、部署脚本和 CI | 独立模块、可选模块配置、后台入口、API | 将 Go CMS/MongoDB 搬到 Workers |
| 原编辑、预览、发布、撤下、回滚的行为 | 从既有 Publication 读取产物 | 在原 publish/unpublish 流程中自动调用 Cloudflare |
| `Content.Published`、Publication 状态、原 webhook 语义 | 独立导出/远端部署状态 | 让 Cloudflare 结果决定原系统是否发布成功 |
| `STATIC_STORAGE_PROVIDER=filesystem`、原存储及扫描器 | 单独的私有导出存储 | 改为 r2/s3 provider，搬迁原 content 目录 |
| 原 `BASE_URL`、`PUBLIC_BASE_URL`、清缓存配置 | 每个目标自己的公开 origin | 改当前服务器域名、DNS、路由、证书 |

默认不联动：CMS 撤下/删除不撤下 Workers 副本；Workers 撤下不改变 CMS。界面必须在部署和撤下确认中提示。不能仅依赖用户阅读文档。

## 2. 当前项目兼容性事实

以下路径均相对于项目根目录；是实施接口依据，不得凭名称另建一套旧发布流程。

| 当前证据 | 事实 | 新增模块的约束 |
| --- | --- | --- |
| `internal/services/cloudflare.go` | `CloudflareService` 使用 Zone ID，仅清缓存和检查 zone | 不改造此服务为多账号上传器；不挪用其 Token/Zone ID |
| `cmd/server/publication_runtime.go` | 唯一原发布 runtime 使用 `NewFilesystemStore("content")` | 复用该实例的只读入口；禁止新建另一套原发布 runtime |
| `internal/product/publication/model.go`、`repository.go` | Publication 存在 hash、版本、路径和验证状态；通过 `GetActive` 找当前版本 | 不假设 Content 有 `active_publication_id`；不从 mutable Content 重渲染 |
| `internal/product/storage/store.go`、`filesystem.go` | `Open(StoredObject)` 校验 immutable 文件 hash | 通过受信 Publication 构造 StoredObject；不接受客户端文件路径 |
| `internal/handlers/handlers.go` 的 `renderPublicWithSEO` | 完整 HTML 独立输出；片段页面在访问时套 publicLayout | 本期只支持完整文档，不能把片段当完整页面导出 |
| `ServeAsset`、`internal/services/asset.go`、`internal/database/mongo.go` | 资源可能在 generated、static、uploads 或 legacy DB 二进制字段中 | 必须有明确资源解析器，不能遍历一个目录代替资源收集 |
| `cmd/server/main.go` | `/api/v1` 有认证、请求大小/限流/归因中间件，后台可用 session | 新 API 接入同一中间件；不另开无认证管理端口 |
| `internal/auth/permissions.go` | 权限需同时满足 role、API scope、sandbox 限制 | 使用 `UserHasPermission`，不能仅比较角色 |
| `internal/services/apikey.go` | 现有 API key 密文绑定 session secret，且有 reveal 能力 | CF Token 按用户确认独立明文存库，不复用 reveal API 或改变旧 key 行为 |
| `config/config_publication_16e_test.go` | 原 provider 仅允许 filesystem | 不放宽旧 guard；新增能力不是原 storage provider |

结论：可以以附加模块兼容接入；当前项目没有现成 Workers 部署实现，不能仅配置一个 Account ID 就宣称完成。

## 3. 选型与平台边界

采用 Workers Static Assets + 精简 ES module Worker，后端用 Go HTTP 客户端调用 Cloudflare REST API；CMS 不新增 Node/Wrangler 运行依赖。Wrangler/本地 Worker 测试工具可作为开发工具单独管理。

不使用 Workers Sites 旧 KV 方案、不使用 Pages API、不使用 Workers for Platforms dispatch namespace。本期不引入 R2、KV、D1、Durable Objects。

本期定位小中型报告站点。Static Assets 当前每版本文件数上限免费 20,000、付费 100,000，单文件 25 MiB；这是文件数而非报告数。部署清单包含旧页面、共享资源、保留资源和辅助文件，均计入上限。目标默认按免费额度保守检查，管理员可明确登记付费档位；不能从套餐名称猜测无限额度。[平台限制](https://developers.cloudflare.com/workers/platform/limits/)

本设计使用 Worker-first 保证路由、响应头及版本识别一致，因此请求会调用 Worker，不能承诺所有访问免费。免费调用配额耗尽可能返回 429；生产容量和费用须按实际账号确认。[计费规则](https://developers.cloudflare.com/workers/static-assets/billing-and-limitations/)

## 4. 模块边界与存储

| 建议目录 | 职责 | 不允许做的事 |
| --- | --- | --- |
| `internal/product/staticexport/` | 读取已发布字节、收集资源、制作可下载包 | 写原 Publication 或触发原发布 |
| `internal/product/staticdelivery/` | 账号、目标、不可变 release、任务、并发与恢复 | 修改原站点配置、扫描原目录 GC |
| `internal/product/staticdelivery/cloudflare/` | REST、认证、上传、版本与部署查询 | 使用全局共享可变 Token 切换账号 |
| `internal/product/staticdelivery/httpapi/` | 新路由、严格 DTO、鉴权、错误映射 | 改旧 generation DTO/错误码行为 |
| `workers/report-static/` | Worker 源码、路由表模块、开发测试 | 执行报告渲染、请求 CMS |

新增 runtime 使用既有 DB、服务 context、原 Store 只读适配器，在现有进程后台运行；不增加必须部署的独立任务服务器。服务关闭时停止领取任务，已持久化任务重启后恢复。

默认私有根目录 `var/static-delivery/`，位于项目根内；可选 `STATIC_DELIVERY_ROOT` 也必须位于项目根内，并避开 `content/generated`、`static` 和 `static/uploads`。

目录：`exports/{export_id}/` 保存源副本和资源；`releases/{release_id}/public/` 只放公开资源；该 release 的 `private/` 保存来源清单、诊断、路由 ownership。ZIP 和 Worker 上传均按显式 allowlist 构造，不打包整个目录或整个仓库。私有目录不得添加公开 FileServer。

新集合统一前缀 `static_delivery_`，包括 accounts、credentials、targets、exports、releases、jobs、operations，以及确认预览 previews、安装身份 settings。索引由新增初始化函数创建，不修改原 `EnsureProductIndexes` 的行为。新模块初始化/索引失败仅禁用新模块相关操作并报告原因，不能阻止旧服务启动。新增账号和Token不依赖服务器加密密钥。

### 4.1 数据落库和唯一性（强制，不是模型命名示意）

表中集合省略共同前缀 `static_delivery_`。所有记录带 schema_version、created_at、updated_at；所有 revision 是 int64，由服务端 CAS 递增，不接受客户端任意赋值。

| 集合 | 必需的身份/持久字段 | 索引 |
| --- | --- | --- |
| accounts | _id、name、account_id、status、credential_revision、verify_result | UNIQUE(account_id) |
| credentials | _id、account_ref、revision、token（明文，不对外序列化）、status；历史ciphertext/nonce/key_id仅兼容读取 | UNIQUE(account_ref,revision) |
| targets | _id、account_ref、cf_account_id、worker_name、environment、public_origin、config_revision、current_revision、current_release_id、active_job_id、remote_version_id、remote_deployment_id、status、installation_id | UNIQUE(cf_account_id,worker_name)；(status,active_job_id) |
| exports | _id、operation_id、state、冻结items、manifest_hash、relative_artifact_path、diagnostics、source_captured_at | UNIQUE(operation_id)；(state,created_at) |
| releases | _id、target_id、job_id、base_revision、config_revision、origin、完整pages/files清单摘要及私有清单文件引用、worker_code_hash、state | UNIQUE(job_id)；(target_id,created_at) |
| jobs | _id、operation_id、kind、attempt、state、target_id、expected_revision、release_id、actor_snapshot、credential_ref、checkpoints、remote IDs、heartbeat、error | UNIQUE(operation_id)；(state,next_run_at)；(target_id,created_at) |
| operations | _id、owner、action、target_scope、idempotency_key_digest、request_hash、预分配job/export IDs、attempts、state、expire_at | UNIQUE(owner,action,target_scope,idempotency_key_digest)；(state,expire_at) |

大清单写入私有文件并落 hash，Mongo 只存引用和有限摘要，避免单文档16MiB上限；目标当前页清单以 current_release 指向的不可变清单为准，不把无限增长的全部正文塞进 targets。

初次创建任务的 operation 对应一个 job，retry operation 引用原 job 并追加其 attempt 日志，不插入第二个 job；jobs.operation_id始终为初次创建operation ID，不被重试覆盖。operations 未落完成前按其预分配ID补建 job/export，重复消费不会创建第二任务。operation 的幂等 request_hash 不含服务端重新解析的“当前Publication”，第一次解析后的items存入 operation，重放直接返回首次结果。export任务的job.kind=export且target_id为空，远端部署任务的target_id必填，不因共用jobs集合给导出强加Cloudflare配置。

Target 状态固定为 `draft|initializing|ready|disabled|drifted|archived`，未决发布由 active_job_id/job state 表示，不另造 target=activation_unknown；初始化结果未明时保持 initializing。Release 为 `prepared|committed|abandoned`；CF 已激活但访问校验失败的候选保持 prepared 并由 job=active_unverified 阻塞目标，不能先标 committed 释放目标。远端激活事实单独存 checkpoint，任何界面都必须展示。

## 5. 多账号管理

### 5.1 账号身份与凭证

Account 包含：内部 ObjectID、中文 `name`、Cloudflare `account_id`、`enabled|disabled|archived`、备注、当前凭证 revision、连接检查结果、创建/更新时间。`account_id` 为 32 位十六进制，规范化小写，集合全局唯一，包括 archived 记录；重复录入返回冲突，引导恢复原记录。

一个 Cloudflare 登录邮箱可能访问多个 account；本系统按 Account ID 区分，不存登录密码，不提供 OAuth 登录 Cloudflare。凭证固定绑定一个账号，即使 Token 权限覆盖其他账号也不能自动跨账号调用。

Credential 单独存储：account 内部 ID、revision、明文 `token`、创建时间、active/retired。按用户2026-10-20确认，新建和轮换凭证不加密，不需要服务器配置 `STATIC_DELIVERY_MASTER_KEY`；Token必须在后台直接录入，保存后不在页面或JSON响应中回显，不写入日志或公开产物。数据库和备份包含明文Token。历史加密revision不自动解密/改写，不能误作明文使用；管理员可直接填写新Token完成轮换，无需旧密钥。仅为历史任务读取保留可选旧密钥兼容，不作为任何新增账号/目标操作的前置条件。

Token 只写入，不提供 reveal；所有响应、审计、错误、导出包不得含 Token、JWT、Authorization 或密文。初次创建必须提供 Token，普通 PATCH 不接收 Token；轮换接口必须提供非空 Token。不能通过空字符串清空，禁止把“省略字段”和“删除凭证”混为一谈。

### 5.2 连接检查不是写权限保证

检查必须调用所填 Account ID 下的 Workers 读取接口，不能只验证 Token 格式/有效性或调用 zone 检查。返回分项：账号范围可访问、Worker 读取可用、账号 workers.dev 子域是否已开通、指定目标是否可访问、最后检查时间。

标准配置使用限定到该账号的 Workers Scripts Edit 权限；不能要求 Global API Key。若 Token 仅授权单个 Worker、无法列出账号全部 Worker，可手动输入 Worker 名，执行该 Worker 的读取验证；列表不可见不等于账号无效。读取验证通过不证明有部署写权限，写能力由专用 Worker 的初始化/实际部署验证，UI 必须区分“读取检查通过”和“已完成部署验证”。[Worker 列表 API](https://developers.cloudflare.com/api/resources/workers/subresources/scripts/methods/list/)

本期自定义域名由管理员在 Cloudflare 控制台配置，所以常规发布 Token 不要求 DNS/Zone 编辑权限。

### 5.3 账号生命周期

新账号未验证时可保存但不可部署。更新 Token 先验证相同 Account ID 后原子切换 revision；已排队任务保留原 revision。原凭证被撤销时任务进入阻塞，用户确认“用新凭证重试”后显式换 revision 并记录事件，不偷偷改变任务身份。账号 ID 一经创建不可修改。

停用/归档阻止新部署和排队任务执行；已进入远端变更阶段的任务允许只读核实并完成记账，不再发出新的变更请求。远端状态未确认时目标保持阻塞。停用本地配置不撤销 Cloudflare Token，不删除 Worker，不下线页面。

禁用前若尚未提交激活，任务进入 blocked；已经持久化激活intent且请求可能发出时按unknown规则核实。disabled时恢复只允许使用保存的凭证做读取，不得因为“恢复任务”绕过禁用发起新激活。归档后可由管理员恢复为disabled并重新验证，再显式启用；目标同理。恢复是本地配置操作，不恢复被管理员在Cloudflare删除的Worker。

账号有活动任务时不能归档；有历史时只归档、不物理删除。不得归档全部可用凭证后又声称仍能操作远端回滚。

## 6. Worker 目标、初始化与域名

Target 包含：内部 ID、account 内部 ID、中文名称、`worker_name`、`test|production`、`public_origin`、config revision、当前 release/revision、远端 version/deployment ID、module ownership marker、状态。唯一键为 Cloudflare Account ID + worker_name，跨本地重复配置亦须拒绝。

初期 Worker 名限定小写字母/数字/连字符、1–63 字符、字母或数字开头结尾；远端拒绝时返回明确原因，不自动改名重试。一个目标对应一个 Worker；不为每篇报告创建 Worker。

### 6.1 接管规则（修正原先过宽的“关联已有 Worker”）

1. 推荐“创建专用 Worker”；显示账号、名称、将发生的写入，用户明确点击初始化。
2. 远端同名 Worker 已存在时，只有其 ownership marker、安装实例 ID、target ID、代码协议版本与本地保留的 release 清单相符才可恢复关联。
3. 不能接管不属于本模块的任意 Worker；即使勾选“确认覆盖”也不支持。无法从远端完整恢复私有站点清单时拒绝接管。
4. 本地丢失但有完整备份时，通过备份恢复；不能把远端已有页面当空站点覆盖。
5. 整个初始化和发布期要求该 Worker 只有本系统一个控制端写入；远端 API 无通用 create-only/CAS 保证，预检查不能防止其他工具同时抢占名称。外部写入属于 drift，阻塞后续发布，不宣称本地锁能阻挡 Cloudflare 控制台。

新 Worker 先通过 module upload PUT 创建专用占位版本：普通路径 404，无报告；只有约定 health 路径返回 ownership marker。先持久化初始化 intent，超时后先读取脚本确认归属，不能盲目覆盖。这个 PUT 会激活占位 Worker，只允许用于已确认不存在的专用名称，禁止用于正常报告更新。

### 6.2 访问地址

默认使用账号已经开通的 workers.dev 子域，通过账号子域查询及 Worker 子域启用接口确认，不能拼一个未知邮箱/账号名当子域。账号尚未开通子域时提示管理员先在控制台开通，不自动注册任意子域。[子域启用 API](https://developers.cloudflare.com/api/resources/workers/subresources/scripts/subresources/subdomain/methods/create/)

`public_origin` 必须为真实可访问的 HTTPS origin：无用户名、密码、query、fragment、路径前缀；去掉末尾 `/`。workers.dev 可用于第一版上线，不必等待自定义域名。自定义域名必须已由管理员绑定到该专用 Worker，并通过 HTTPS health marker 检查。禁止覆盖当前 CMS 的域名/DNS。

首次部署前可改 origin；已有页面后，改 origin 是显式的“整站地址迁移”：无活动任务、检查新地址、增加 config revision、从全部保留导出副本生成新 release、全量重写分享元数据并部署，失败不提交新配置。旧域名解绑/跳转不在本期自动处理范围。

测试目标返回 `X-Robots-Tag: noindex, nofollow`；生产不附加此限制。来源 HTML 中用户已有的 noindex 不自动删除。

## 7. 静态导出契约

### 7.1 选择与冻结

请求为 `items: [{content_id, publication_id?}]`，每批 1–100 项，重复 content_id 拒绝。省略 publication_id 时，在任务创建时解析并持久化当时 GetActive 结果，不能等异步执行时再选“最新”。

仅接受非 fork、未软删除内容的 `active|superseded` 且 `verification_status=verified`、`storage_state=present` 的完整文档。没有 Publication 的 legacy 页面、legacy_unverified、staged、failed、unpublished、已 GC 文件均返回明确不支持/不可用错误；不能自动调用 publish 或重渲染补齐。需要部署旧版本时从已保留 export/release 选择，不依赖旧 Content 状态仍可读取。

通过既有 repo/Store.Open 读取不可变字节并校验 Publication.ContentHash，立刻复制到私有 export staging。期间 GC/读取失败则该批失败，不改原 pin/GC 策略，不降级读取 canonical。批量是逐项冻结的版本向量，不承诺整个 CMS 的同一数据库时刻快照。

导出一批全部成功才 ready；每项诊断可查看，但半成品不可下载或部署。文件写入临时目录，完成后 rename，并且 DB 写 ready 前校验完成标记及全部 hash。重启能恢复准备完成但未记账的包。

### 7.2 完整文档与语言

要求存在真实文档级 html/head/body；复用或提取与现有 `isStandaloneDocument` 一致的检测规则，保留 script/style 原始内容，不用简单全文字符串查找 doctype 判断。片段报 `STATIC_DOCUMENT_NOT_STANDALONE`，不能加当前 mutable theme 冒充历史完整产物。

九套模板作为首批兼容对象，但必须使用实际已发布产物测试，不直接上传含 Go 占位符的 examples HTML。保留现有布局、JS、loading、文案与语言选择逻辑；导出器不翻译、不重新生成正文、不修模板。当前工作区 crypto 模板脚本明确固定英文，所以不能宣称九套模板均已支持浏览器自动中英切换；语言行为按各产物现状回归，此差异不是部署器隐式修复的范围。

### 7.3 资源解析与快照

收集 HTML 的 src、srcset、资源类 href、poster、inline CSS、style、CSS url()/@import 依赖；a[href] 的文章链接不当资源下载。JSON-LD image、og:image、twitter:image、icon/touch icon 也必须收集。支持本地 script[src]，但 JS 动态 import、动态拼 URL、fetch/XHR、CSS 循环依赖无法被静态推断时必须报告，不能声称已自动收齐任意程序的依赖。

| 来源 URL | 字节解析规则 |
| --- | --- |
| `/static/...` | 项目 `static/` 下对应文件 |
| `/uploads/...` | `static/uploads/` 下对应文件 |
| `/assets/...` | 按原 ServeAsset 的去前缀及带前缀磁盘候选顺序，再查 legacy DB Data |
| 其他本地资源路径 | 优先已登记 Asset 的 serve_path/full_path 与 generated 对应字节；不得误读为文章 canonical |
| CMS 已配置源 origin 的绝对 URL | 按同源资源解析，不经 HTTP 请求公网/回环地址下载 |
| `data:` | 原样保留；社交预览图不接受 data URL |
| 外部 HTTP(S) 依赖 | 记录为 external，不自动下载任意第三方文件 |
| 页内 `#`、mailto、tel 等链接 | 非依赖，不打包 |

文件读取只允许上述已知来源，拒绝穿越、越界 symlink、回收 sidecar、备份、目录扫描；原查询串保存在 provenance 中。v1 不支持靠 query 动态变换的图片或资源，无法映射到静态文件则报错。

资源可能是 mutable：记录 Asset ID（如有）、源路径、读取时间、字节 hash，并检查读取前后文件状态或 DB revision/更新时间；变化则重试该资源快照，仍不稳定则失败。**Publication 的 HTML 历史 hash 不等于图片也保存了历史版本**；新导出只保证导出时捕获的资源，历史页面的旧图片无法凭空恢复。exact rollback 只针对已经保留的完整 export/release。

### 7.4 导出形式、manifest 与保留

提供 ZIP 和条件满足时的单 HTML。ZIP 顶层为 `public/`、`manifest.json`、`README.txt`；公开内容仅位于 public，manifest 不含本地绝对路径、凭证、内部审计和私有备注。私有 provenance 单独保留。

public 内普通静态文件布局按实际请求路径构造，CMS 无扩展名 `/news/a` 在下载包保存为 `news/a/index.html`，部署映射时显式路由到该文件；README 声明普通文件服务器可用 `/news/a/` 浏览，Worker 保留原 canonical `/news/a`。不承诺 file:// 下剪贴板等浏览器 API 可用。相对引用必须以原页面 URL 为基准解析，不能把无扩展名路径误当目录。

导出同时保存原HTML source副本和可浏览的public副本：将已收集的本系统资源URL变为包内root-relative URL，不保留指向原CMS的绝对图片/脚本地址；源hash和local_export_sha256分开记录。canonical/OG页面URL在未选目标时仍保留来源地址，不影响资源独立性。外部渲染依赖未解决的ZIP可用于诊断下载，但标记 portability=incomplete，不能称独立可用；部署严格拒绝。CSS相对引用同样按CSS源URL解析，不能只改HTML。

单 HTML 仅在所有渲染所需依赖已经内联时允许；不能把 PNG、字体等默默丢弃，也不自动把任意依赖内联成巨型 data URI。没有 target origin 的独立导出保留原 canonical 并明确标注，不得拿本地包直接宣称 Cloudflare 分享地址已就绪。

manifest 至少包含 schema_version、export_id、每篇 Publication ID/FullPath/source_html_sha256、每个文件的 bytes_sha256/size/MIME、依赖关系、external 列表、exporter_version、资源捕获时间。export ready 后不再从源文件或 mutable 模板读取。

## 8. 目标化 release 构建与站点合并

Export 是可复用源包；Release 是针对一个目标 origin 的不可变完整站点包，两者不能混用。每个 release 保存 target/config revision、base revision、完整页面 ownership map、完整文件清单、源 export 引用、rewriter/Worker 版本和校验值。

1. 根据已冻结 export 构建目标副本，只修改副本，不修改源 Publication、原模板或原 export。
2. 重写 canonical、og:url、JSON-LD url/@id/mainEntityOfPage、社交图片、icon、资源引用；使用 HTML/CSS/JSON 结构化处理并保留 inline script/style 原始内容，禁止全局替换域名字符串。
3. 页面标题优先原 head title/og:title，其次已冻结正文标题；description 优先原 meta/og description，其次导出时从已发布正文提取的纯文本前 200 Unicode 字符。不读取当前草稿 summary。保留原标题中的站点品牌，不自动换名称。
4. 缺少分享图/icon 时，可按 frozen template slug 从项目九模板现有 `static/images/chain-lens-*` / `static/images/report-templates/{slug}-*` 显式映射取默认文件；不是任意 slug 猜路径。没有映射或文件缺失则阻止生产部署，测试导出只报告缺项。
5. 将本地资源重命名为 `/_lc-assets/{完整sha256}.{ext}` 并同步重写引用，校验目标 MIME；CSS 中依赖先构建、再计算自身最终 hash，循环依赖报错。同 hash 不同 MIME/扩展名按最终路径分别处理。
6. JSON-LD 只改明确 URL 属性；普通正文里的来源引用不能改成新域名。同 CMS 站内文章链接，仅当目标清单有该路径时改为目标路径；未部署文章链接保留源地址并显示依赖警告。
7. 含 `publisher.example` 等已知示例 origin、未解析 Go 占位符、缺失资源、localhost/私网渲染依赖、被禁止的后台调用时，部署校验失败。模板代码示例中的普通文本不当作待替换标记。

部署 v1 固定为严格独立模式：external **渲染依赖** 必须移除、先导入本地或内联后重新导出；普通外部引用链接允许。不能把“允许外部链接”和“允许外部图片/脚本”混为一谈。不新增隐式 SSRF 下载器。导出器的静态检查不能证明任意JS无动态依赖；对九套已知模板执行浏览器网络回归，对其他带脚本文档要求人工审阅并在部署预览中明确确认，不宣称一个正则就完成程序级依赖证明。

默认合并为“当前站点清单 + 本次选择的页面替换”，不是仅上传新页面；站点清单按 path 唯一且保存 content ownership。同 path 不同 content_id 默认 409，不因两者来自不同 export 而覆盖。相同内容路径变更默认是新增，不自动删除旧路径；用户显式勾选移除该内容旧路径后列入同一部署变更清单，不创建隐式 redirect。

本期不开放任意全站 replace API；全站恢复只允许选择完整历史 release，并确认差异清单。公共路径 `/cm`、`/api`、`/mcp`、`/_lc-assets`、`/.__lc_delivery` 及其子路径保留；原报告与保留路径冲突则拒绝本次部署，不改原 CMS 路径。

## 9. Cloudflare 实际调用协议

Cloudflare base 固定为 `https://api.cloudflare.com/client/v4`。每次请求按任务冻结的账号和凭证 revision 构造独立客户端，不修改全局 Token，不信任客户端直接提交的 API base。

普通更新协议如下，Cloudflare API 返回 HTTP 成功还必须检查 JSON success/result：

| 顺序 | 调用 | 必须持久化/校验 |
| --- | --- | --- |
| 1 | 读取目标当前 deployments/settings | 与本地记录的版本、归属、配置对照；漂移则停止 |
| 2 | `POST /accounts/{a}/workers/scripts/{w}/assets-upload-session` | 提交完整 release manifest，不是本次增量列表 |
| 3 | `POST /accounts/{a}/workers/assets/upload?base64=true` | 按返回 buckets 上传；使用 session JWT，不是账号 Token |
| 4 | `POST /accounts/{a}/workers/scripts/{w}/versions` | multipart 发送 module、路由模块和 assets completion token；先保存 version_id，此时尚未上线 |
| 5 | `POST /accounts/{a}/workers/scripts/{w}/deployments` | `strategy=percentage`、仅该 version_id、100% 流量；发送前持久化激活 intent |
| 6 | GET deployments + 公开 HTTPS 检查 | 记录远端 deployment_id、version_id，核对 marker/文件结果后更新本地站点 revision |

上传 session 可能返回空 buckets，此时直接使用 completion token；JWT 过期则重新登记同一清单，不重新选取内容版本。[资源上传协议](https://developers.cloudflare.com/workers/static-assets/direct-upload/)

内部文件完整 SHA-256 与 Cloudflare asset hash 分开存；适配器按官方当前示例计算 `hex(SHA256(base64(bytes) + extension))[0:32]`，size 是原字节长度，MIME 使用 multipart part 的 Content-Type；不得把完整 64 位 SHA-256 直接塞入 asset hash。此协议须有固定向量与真实账号集成测试，不能仅测 HTTP mock。

multipart metadata 使用 `main_module`、`bindings: [{type:"assets", name:"ASSETS"}]`、`assets.jwt`、`assets.config`、固定 compatibility_date 和可追溯版本 annotations；API metadata 不是 wrangler 配置原样粘贴。约定assets.config中的 `run_worker_first=true`、`html_handling="none"`、`not_found_handling="none"`，不启用SPA。module part使用JavaScript module MIME，路由表以独立JSON module打包并被Worker import；Worker脚本与全部模块的大小也需按目标套餐做上传前检查，不能只算assets文件。[multipart 说明](https://developers.cloudflare.com/workers/configuration/multipart-upload-metadata/)

**平台契约风险**：官方Direct Uploads指南明确提到Version endpoint可附加assets，但当前自动生成的Upload Version API参考页没有完整列出这些metadata字段。因此必须使用可发送显式JSON metadata的REST适配器，不能让SDK未知字段过滤掉assets。第一阶段先以专用测试Worker验证上述metadata及真实回读，留存脱敏请求fixture；若实际API拒绝这些字段，暂停部署引擎实施并修订契约，不能改用会直接激活的PUT冒充staged upload。这个文档差异是已识别的集成门槛，不是已完成的验证。

版本上传与激活分开是上线判定的基础：[Upload Version](https://developers.cloudflare.com/api/resources/workers/subresources/scripts/subresources/versions/methods/create/)、[Create Deployment](https://developers.cloudflare.com/api/resources/workers/subresources/scripts/subresources/deployments/methods/create/)。资源、Worker 代码、路由表在同一 version；不使用 keep_assets 拼接增量资源替代完整清单。

## 10. Worker 请求契约

Worker 不连接 Mongo、不读取 CMS、不渲染模板、不携带 Cloudflare Token。输入只允许部署时生成的路由表和 ASSETS binding。

| 请求 | 行为 |
| --- | --- |
| GET canonical `/news/a` | 查路由表，内部请求 ASSETS 的 `/news/a/index.html`，返回 HTML，不改变地址栏 |
| GET `/` | 仅当有显式首页文章时返回该文章，否则 404；不自动生成会暴露全部报告的目录页 |
| HEAD 已有路由 | 状态/响应头同 GET，无 body |
| 非 canonical `/news/a/` 或 `/news/a.html` | 仅在别名表无冲突时 308 到 canonical；不猜测或覆盖另一篇合法路径 |
| GET `/_lc-assets/{hash}.{ext}` | 只允许当前资源清单中的文件，通过 ASSETS 返回 |
| GET `/.__lc_delivery/health` | 返回协议版本、installation/target/release marker；不暴露私有清单或凭证 |
| 其他方法 | 405，Allow: GET, HEAD |
| 不存在/保留后台路径 | 404；不代理 CMS、不 SPA 回退、不重定向后台 |

路径使用 URL 解析器，只解码一次，拒绝 NUL、反斜线、编码斜线及穿越；不做大小写折叠。构建路由时检查 canonical、别名、文件路径的相互冲突，歧义时拒绝该别名或拒绝部署并列明冲突。query 不参与文件身份且不反射到 HTML，保留浏览器 query 不影响静态正文。

HTML `Content-Type: text/html; charset=utf-8`，`Cache-Control: no-cache`；hash 资源 `public,max-age=31536000,immutable`；health/404 `no-store`。Worker 直接设置最终头，不依赖 `_headers` 自动作用于代码生成响应。返回 `X-LC-Release`/ETag 供核实；自定义 Worker 响应要正确处理 If-None-Match 和 HEAD。用户点击 query 相同页面不得命中旧 CMS 数据。

默认 ASSETS HTML 自动跳转会改变无扩展名路径，因此显式禁用，所有 canonical 策略由路由表定义。[HTML handling](https://developers.cloudflare.com/workers/static-assets/routing/advanced/html-handling/)、[Worker-first](https://developers.cloudflare.com/workers/static-assets/binding/)

JS/CSS 内联允许继续执行；不得附加会禁止九模板 inline script/style 的 CSP。500ms loading 是客户端显示行为，不延迟服务器返回 meta。浏览器语言逻辑按导出原样执行；社交爬虫默认读取英文 head，不承诺爬虫执行 JS 翻译。Telegram、WhatsApp、LINE、LinkedIn、Facebook 是否显示图标及刷新缓存由各平台决定，不承诺全部以相同卡片展示。

## 11. 任务状态、并发与崩溃恢复

Export：`queued → building → ready|failed`；缺失本地包则 ready 转 `unavailable`，不伪装可下载。

Deployment Job：`queued → preparing → uploading → version_ready → activating → verifying → succeeded`。附加状态：`blocked`（凭证/目标等可人工解决）、`activation_unknown`（可能已激活）、`active_unverified`（已确认远端激活但公开校验未通过）、`failed`、`cancelled`。failed 仅用于确定未改变远端当前版本或已明确终结的失败，不能将未知激活当普通 failed。

Task 包含 operation_id、actor/credential owner、account/credential revision、target/config revision、所选 export IDs、期望 base revision、release ID、remote IDs、attempt、step checkpoints、错误码、时间。创建时冻结目标，切换默认账号不会改变任务。

每个目标最多一个未终结发布 owner，通过 targets 的单文档 CAS `active_job_id` 竞争；不同账号/目标可并行（初期全局并发 2、每账号上传并发 2）。Mongo 是 durable queue，不使用仅内存队列。每个 owner 定时心跳，领取前检查账号/权限/目标状态。

激活前把完整 release、base revision、远端旧 version_id、operation marker、激活 intent 持久化。只有已确认上一任务结束或完成恢复的目标能提交下一激活。普通 lease 过期不意味着可以绕过远端未决请求。

| 崩溃/错误位置 | 恢复规则 |
| --- | --- |
| preparing/uploading | 复用固定 export/release；重建过期 upload session；不选新 Publication |
| version 上传请求超时 | 按 operation marker 查询版本；重复 inactive 版本可以记录并遗弃，但不能顺便激活 |
| activating/POST deployment 超时 | 进入 activation_unknown，冻结该目标；读取 deployments/marker 核实，不盲重发或回滚 |
| 远端为新版本，本地记账未完成 | 复核 hash/marker 后 CAS 提交 release 与 revision；可重复恢复 |
| 远端仍为旧版本，原请求可能仍执行 | 保持 unknown，不能把一次 GET 未出现当明确失败；人工处理前先停止旧写入进程并确认远端请求终结 |
| 远端出现非本模块版本、混合流量或非预期设置 | 标记 target=drifted，阻止写入，管理员按保留 release 明确恢复 |
| 激活确认但 HTTPS 失败/返回旧 marker | active_unverified；继续只读校验，页面显示“远端已激活，访问校验未通过”，不能谎称保持旧版 |

CF 与 Mongo 没有跨系统原子事务；不承诺全球边缘节点同一时刻切换，也不能撤回读者已下载的文件。单写入控制端为本期前提，多套不共享 DB 的 CMS 不得管理同一 Worker。

API 的 expected_revision 绑定用户确认的站点状态；排队后被其他任务改变则任务记录STATIC_REVISION_CONFLICT，必须重新预览确认，不自动覆盖；已经返回202的请求不再“异步返回HTTP409”。成功提交以 targets 单文档原子更新 current_release/current_revision 和清除 active_job 完成；release committed/jobs succeeded 可随后幂等补写。targets已指向某release就必须在保留清扫时保护该release，即使其状态补写尚未完成。无需为新模块提高当前 Mongo 拓扑要求。

公开校验在激活后以有界流量GET检查：health marker和远端version一致；本次所有新增/变化页面200且meta正确、响应HTML hash与release一致；新增/变化资源200、MIME/大小/hash匹配；删除路径404或预期canonical跳转；不变页面至少按确定性样本验证。HEAD不得有正文。连续3轮（间隔2秒）通过后成功；30分钟仍不通过为active_unverified，后续reconcile可继续核实，不能把这一检查描述为已证明全球所有节点传播完成。

## 12. 更新、撤下、回滚、保留与备份

- 更新：同 content/path 的目标副本替换，其余页面复用该目标已有 release，不从原 CMS 重新生成。
- 撤下：显式选择目标和 route，在新 release 删除映射；不存在时幂等成功，不创建无意义新版本。修改 CMS 的 published 状态不在此操作内。
- 单篇回滚：从同目标历史 release 取该篇已保存的导出副本，合并当前其他页面，创建新 release；不是原 Publication.Rollback，不新增原 Publication。
- 全站回滚：选择同目标历史完整 release，预览所有恢复/移除项，按新任务重新部署。origin/config 不匹配时拒绝，先执行地址迁移；不能把另一账号的版本 ID 拿来回滚。
- 优先用已保留产物重发，而非完全依赖 Cloudflare 最近版本；平台原生 rollback 只有最近 100 个已发布版本可选，且影响整个 Worker，不能冒充单篇回滚。[回滚限制](https://developers.cloudflare.com/workers/versions-and-deployments/rollbacks/)
- 新模块默认独立保留：无引用 export 和非当前历史 release 90 天，失败临时文件 7 天；当前 release、待执行/未决任务引用及管理员 pin 的包不删。按引用标记清扫，禁止只按文件 mtime 删除。
- 新 release 对刚退出当前页面依赖闭包的 hash 资源保留 7 天，记录首次退出时间和retained_until；从上一release继承的保留资源不重新延长，达到期限且无当前引用才可移除，避免每次部署都续期形成永久堆积。撤下不承诺旧资源立即不可访问。保留资源也计配额。紧急全撤资源是后续独立能力，不隐式提供。
- CMS 源被删除或 GC 后，已 ready export 和已部署页面不受影响；本地导出包丢失不影响远端已在服务的页面，但阻止需要该包的合并部署，返回 unavailable，不清空站点。
- 备份必须同时覆盖新增集合、私有文件根目录及 Mongo 中的 installation ID；仅仍有历史加密凭证引用时才需要相应旧密钥，新明文凭证不要求密钥。不把恢复后的新安装当作有权覆盖未知 Worker。账号归档不删除远端任何对象。本期没有自动远端 DELETE Worker API。

## 13. API、鉴权与操作确认

以下全部是拟新增 API，不是当前已存在的接口。统一 `/api/v1`，沿用认证、限流、body size、归因链；不把压缩包上传塞入旧 generation 请求。

| 方法/路径（相对 /api/v1） | 请求/用途 | 权限 |
| --- | --- | --- |
| POST/GET `/cloudflare-accounts` | 创建账号 / 管理列表（不含 Token） | settings.edit |
| PATCH `/cloudflare-accounts/{id}` | name/remark/enabled；Token 独立轮换 | settings.edit |
| POST `/cloudflare-accounts/{id}/credentials` | Token 验证并增加 revision | settings.edit |
| POST `/cloudflare-accounts/{id}/verify` | 只读连接检查 | settings.edit |
| GET `/cloudflare-accounts/{id}/workers` | 远端列表/权限诊断；不代表允许接管 | settings.edit |
| POST `/cloudflare-accounts/{id}/archive` | 归档本地配置 | settings.edit |
| POST `/cloudflare-accounts/{id}/restore` | archived恢复为disabled；不操作远端 | settings.edit |
| POST/GET `/static-targets` | 创建 draft 目标 / 管理目标列表 | settings.edit |
| PATCH `/static-targets/{id}` | 未上线配置或描述；带 expected_config_revision | settings.edit |
| POST `/static-targets/{id}/initialize` | 明确新建专用 Worker（外部写入） | settings.edit |
| POST `/static-targets/{id}/verify` | 只读归属、origin、remote version 核对 | settings.edit |
| POST `/static-targets/{id}/archive` | 无活动任务时归档，不删除远端 | settings.edit |
| POST `/static-targets/{id}/restore` | archived恢复为disabled；重新检查归属后才启用 | settings.edit |
| GET `/static-delivery/options` | 发布者可见的精简账号/目标选项 | content.view + content.publish |
| POST `/static-exports` | items；创建导出任务，202 | content.view |
| GET `/static-exports/{id}` | 状态、逐项诊断、清单 | content.view |
| GET `/static-exports/{id}/download?format=zip\|html` | ready 包流式下载；不可用格式拒绝 | content.view |
| POST `/static-targets/{id}/deployment-preview` | export_ids、remove_paths、expected_revision、expected_config_revision；生成完整差异及 confirmation_digest，不激活 | content.view + content.publish |
| POST `/static-targets/{id}/deployments` | 与preview相同字段，增加confirmation_digest；202 | content.view + content.publish |
| GET `/static-targets/{id}/deployments` | 目标历史及逐篇状态 | content.view + content.publish |
| GET `/static-deployments/{id}` | 任务、remote 状态、访问地址 | content.view + content.publish |
| POST `/static-deployments/{id}/reconcile` | 只读远端核实 + 本地恢复；不重发激活 | content.view + content.publish |
| POST `/static-deployments/{id}/retry` | expected_attempt；仅确定未激活的失败/blocked；明确新凭证时带credential_revision | content.view + content.publish |
| POST `/static-deployments/{id}/cancel` | 取消排队/blocked/尚未发起远端激活的任务，返回最终或取消待处理状态 | content.view + content.publish |
| POST `/static-targets/{id}/withdraw-preview` | paths、expected_revision、expected_config_revision；只预览 | content.view + content.publish |
| POST `/static-targets/{id}/withdrawals` | 与withdraw-preview相同字段，增加confirmation_digest；202 | content.view + content.publish |
| POST `/static-targets/{id}/rollback-preview` | release_id、scope=page/site、page_paths（page时必填）、expected_revision、expected_config_revision | content.view + content.publish |
| POST `/static-targets/{id}/rollbacks` | 与rollback-preview相同字段，增加confirmation_digest；202 | content.view + content.publish |
| POST `/static-targets/{id}/origin-migration-preview` | new_origin、expected_revision、expected_config_revision；全站迁移预览 | settings.edit + content.publish |
| POST `/static-targets/{id}/origin-migrations` | 与origin-migration-preview相同字段，增加confirmation_digest；202 | settings.edit + content.publish |

补充契约：

1. 复用既有权限，不给 editor 新增 settings.edit，不因为加本模块而放大旧角色权限。sandbox-only 凭证禁止本模块所有创建/变更/任务操作；只读原有功能保持不变。无权限时先 403，无新 DB/文件/CF 副作用。
2. 每个 content_id/pub_id、export、目标、release 都由服务端核实引用关系；客户端最多提交 target 内部 ID，不能在部署 body 中覆写其 account_id/worker_name。若 UI 同时发送 account 内部 ID，必须与 target 一致。
3. 管理者可见账户配置；发布者只能看到 options 中启用目标的名称/内部 ID/公开地址。普通 viewer 不得查看 CF 账号凭证管理。新端点不自动提供 MCP/CLI 薄包装，本期 REST 和后台为完整交付面。
4. 创建 export、初始化 Worker、部署、撤下、回滚、地址迁移、retry、cancel 必须有 Idempotency-Key；缺失 428。作用域为 actor credential owner + 动作 + target（如有）+ key；同 key 不同规范化请求返回 409，同请求返回原 operation/job。预览/连接检查不创建远端资源。
5. operations 使用持久化唯一索引，保留 90 天且活动操作不 TTL；job/operation ID 预分配并在同一 operation 记录冻结，避免插入 job 前崩溃生成两个任务。retry通过独立幂等operation引用原job，在expected_attempt匹配时CAS加一，不改变原选定内容；unknown 不允许 retry。重试原失败提交的旧Idempotency-Key只返回旧结果，不自动重做业务。
6. 所有 mutation body 严格拒绝未知字段；所有列表分页 limit 默认 20、最大 100。GET 返回 UTC RFC3339 时间；包下载不加载整个 ZIP 进内存，不受 request body 限制误伤。
7. preview digest 覆盖目标、revision、config、导出 hashes、增删路径及模式；提交时重新计算，改变任何影响项都需重新预览。preview 生成的临时包也纳入有界清理，不触发外部写入。
8. release/job IDs 在 HTTP 和 Mongo 使用 ObjectID 的 24 位 hex 字符串；Cloudflare version/deployment UUID 单独存，不与 Publication ID 混用。202 返回 operation_id、job_id、status_url，不能返回 success=true 表示远端已上线。
9. retry 不覆盖失败记录：attempt 追加，失败原因和远端 ID 保留；post-activation 校验失败只 reconcile，不重新选内容渲染。
10. 执行前重新确认原操作者仍有效、角色/当时 credential scope 与目标可用，角色被撤销则 blocked；不假设排队时的授权永久有效。远端变更已发出后的恢复只核实记账，不新发越权变更。
11. Job kind固定为export/initialize/deploy/withdraw/rollback/origin_migrate。initialize和origin_migrate的retry/cancel仍需settings.edit，不能借通用任务接口绕过初次操作权限。cancel先CAS设置取消意图，由worker在激活intent前检查；与激活intent竞争失败、初始化PUT可能发出、unknown或active_unverified均不能直接标cancelled或释放目标，返回STATIC_ACTIVATION_UNKNOWN或继续只读核实。已激活报告要撤下必须走withdrawal，不把cancel当删除。

示例部署请求（ID 为示意值，必须换真实已存在 ID）：

```json
{
  "export_ids": ["000000000000000000000001"],
  "remove_paths": [],
  "expected_revision": 7,
  "expected_config_revision": 2,
  "confirmation_digest": "sha256:<preview-returned-digest>"
}
```

错误响应沿用现有 error/code/message/request_id/retryable/details 外形，由新包自己映射，不修改旧 generation.StatusForCode。HTTP 请求被接受后的异步错误出现在 job 中；409/422 等同步码仅指提交前检查。

| HTTP | 新错误码 | 含义 |
| --- | --- | --- |
| 400 | STATIC_INVALID_REQUEST | 未知字段、ID/路径/格式不合法 |
| 403 | STATIC_PERMISSION_DENIED | 本地权限/sandbox 限制 |
| 404 | STATIC_RESOURCE_NOT_FOUND | 引用对象不存在或不允许获知 |
| 409 | CF_ACCOUNT_EXISTS / CF_TARGET_EXISTS | 账号/目标唯一性冲突 |
| 409 | CF_WORKER_UNMANAGED / CF_REMOTE_DRIFT | 未管理 Worker 或外部配置漂移 |
| 409 | STATIC_REVISION_CONFLICT / STATIC_PATH_CONFLICT / STATIC_IDEMPOTENCY_CONFLICT | 版本、路径、幂等请求冲突 |
| 409 | STATIC_TARGET_BUSY / STATIC_ACTIVATION_UNKNOWN | 有未决操作，不得另启激活 |
| 409 | CF_ACCOUNT_DISABLED / CF_TARGET_NOT_READY | 账号停用或目标非ready |
| 409 | STATIC_SOURCE_NOT_EXPORTABLE | Publication 状态不允许 |
| 410 | STATIC_ARTIFACT_UNAVAILABLE | 源/保留包已删除或缺失 |
| 422 | STATIC_DOCUMENT_NOT_STANDALONE / STATIC_DEPENDENCY_UNRESOLVED | 文档或依赖不独立 |
| 422 | STATIC_METADATA_INVALID / STATIC_ASSET_LIMIT_EXCEEDED | 分享元数据/示例地址/配额不合格 |
| 422 | CF_ORIGIN_NOT_READY / CF_PERMISSION_INSUFFICIENT | 目标访问地址/远端权限不满足 |
| 428 | STATIC_IDEMPOTENCY_KEY_REQUIRED / STATIC_CONFIRMATION_REQUIRED | 缺幂等键、revision 或预览确认 |
| 429 | STATIC_RATE_LIMITED | 本地限流；远端429记录CF_RATE_LIMITED并遵循Retry-After |
| 503 | STATIC_MODULE_UNAVAILABLE / CF_TEMPORARILY_UNAVAILABLE | 新模块不可用/远端临时故障 |

Job 专用 code：CF_UPLOAD_FAILED、CF_VERSION_UPLOAD_FAILED、CF_ACTIVATION_UNKNOWN、CF_PUBLIC_VERIFICATION_FAILED、CF_CREDENTIAL_UNAVAILABLE、STATIC_EXPORT_FAILED。不能复用原 publication.failed/webhook 表达这些失败。外部 HTTP 403 是 CF 权限问题，不能冒充当前用户的本地 403。

## 14. 后台交互

账号页：中文名称、Account ID、状态、读取检查/部署验证分别展示、Worker 数量、备注；添加/轮换 Token 均为密码输入且不回显。Worker 数量读取失败显示未知，不显示 0 冒充无资源。

目标页：账号联动选择 Worker；切换账号清空旧 Worker。对未知 Worker 显示“不可接管”，提供创建专用 Worker；显示真实 origin、生产/测试标记、远端 drift/未决任务、最后成功部署。

内容页新增“导出静态包”“部署到 Workers”，原发布按钮不替换。部署对话框先选账号再选该账号目标，显示原 Publication、export、完整影响清单、外链警告和“与原站点独立”提示，生产确认后执行。一次选一个目标，同一内容可再次部署到其他账号；不提供跨账号全成全败事务。

显示按目标区分的状态：未部署、已部署选定版本、存在其他 CMS 版本、待核实、远端已激活但访问验证失败、失败、已撤下。原内容之后发生编辑，不把既有 Cloudflare 页面标成失败；比较 Publication/export 身份显示差异即可。

选定版本后生成导出包可作为“部署”流程中的子任务，但 UI 必须显示导出/部署分阶段结果；失败时不重发原 publish。后台显示独立进度和访问链接，不强行阻塞一个长 HTTP 请求等待完成。

## 15. 配置、运行与审计

新增可选配置：STATIC_DELIVERY_ROOT（默认 var/static-delivery）、STATIC_DELIVERY_GLOBAL_CONCURRENCY（默认 2）、STATIC_DELIVERY_RETENTION_DAYS（90）、STATIC_DELIVERY_FAILED_RETENTION_DAYS（7）、STATIC_DELIVERY_JOB_TIMEOUT_MINUTES（30）。无需配置服务器密钥；STATIC_DELIVERY_MASTER_KEY仅为历史加密revision可选读取兼容，不影响新明文Token管理。账号/Worker/origin/套餐上限在新集合维护，不增设单一全局 CLOUDFLARE_ACCOUNT_ID/TOKEN 覆盖多账号。

HTTP 调用超时默认 30 秒/次，上传按 bucket 有界流式处理；读取/上传临时错误指数退避、遵循 Retry-After，最多 5 次。整体任务超时：激活前可确定失败；激活后/未决必须 unknown 或 active_unverified，不能因 30 分钟超时自动释放目标。

任务审计记录操作者、账号内部 ID、target、action、job/attempt/release/remote IDs、各阶段时间、结果；复用现有审计写入方式但不得改变原审计语义。本期不新增对外 webhook 类型或改造旧 outbox；后台/API 状态是本期交付依据。将来新增部署 webhook 需独立事件定义和持久化投递设计。

原服务器内页面访问统计不会统计 Workers 流量，本期不增加回源追踪脚本，也不承诺原 analytics 包含新流量。监控与备份说明作为新模块文档，不修改旧部署脚本和 CI；独立测试工作流可新增但不得覆盖原工作流。

## 16. 验收与真实部署门槛

每项必须有自动测试或留存实测证据；未执行标记未执行，禁止跳过后称通过。

| 编号 | 必须证明的结果 |
| --- | --- |
| C01 | 无任何新配置，原启动、原发布/撤下/回滚/清缓存测试不变；新密钥/索引故障不拖垮旧服务 |
| C02 | 任务固定 Publication；创建后编辑/再次发布原内容不改变任务源字节；原数据、pin、outbox、canonical 不被导出修改 |
| C03 | 片段、legacy_unverified、failed、fork、GC、hash 损坏均按定义失败，不偷用 canonical 或重渲染 |
| C04 | 三类磁盘资源、去 /assets 前缀、legacy DB Data、中文文件名、srcset、嵌套 CSS、资源覆盖竞态有用例 |
| C05 | 导出 ZIP 可独立 HTTP 访问，单 HTML 条件严格，private 文件不公开，外部依赖不被漏报 |
| C06 | 九模板真实发布产物逐一在 390px/1440px、英文/中文浏览器环境检查；按现有语言行为对比，loading 不小于原500ms契约，不新增布局变化 |
| C07 | 目标化 rewrite 不改变正文、日期/作者/品牌及脚本；不同账号 origin 的 canonical/OG/JSON-LD/图标正确，无示例地址 |
| C08 | 非权限用户、scope不足、sandbox-only在写入前失败；Token/JWT不出响应、审计、ZIP或上传public目录 |
| C09 | 同名 Worker 在不同账号相互隔离；同一账号重复登记拒绝；未知已有Worker不可覆盖；列表无权限有明确诊断 |
| C10 | 新 Worker 初始化、workers.dev 启用、assets JWT、空 buckets、MIME、hash 固定向量、version 与 deployment 两步均实测 |
| C11 | 发布 A 后发布 B，再更新 A：B 和资源仍可访问；同path不同content冲突；路径变更不隐式删旧页 |
| C12 | 同目标并发只能一个owner；排队revision过期409；不同目标部署互不覆盖；Token轮换不导致跨账号执行 |
| C13 | 注入上传/版本创建/激活前后/DB记账崩溃，unknown阻塞后续写入，可读回恢复；不重复调用旧publish |
| C14 | 远端已激活但公开HTTPS失败显示active_unverified；CF控制台改版本产生drift；不能误报旧版仍在服务 |
| C15 | GET/HEAD、canonical/别名冲突、404/405、If-None-Match、query、无SPA回退、保留路径及UTF-8均符合契约 |
| C16 | 停止CMS与Mongo后，从另一网络访问全部选定报告、封面和图标；社交爬虫UA无需JS即可读meta |
| C17 | 多账号分别部署同内容；A撤下/回滚不影响B和CMS；删除CMS内容不删除Workers副本 |
| C18 | 单篇回滚保留其他页；全站回滚显示完整影响；源GC后用完整export回滚；丢包不清空远端 |
| C19 | current/active/unknown/pin引用不被GC；90/7天规则、7天旧资源保留、配额含保留文件均验证 |
| C20 | 备份恢复、installation ownership、主密钥错误/轮换、账号停用/归档和凭证失效流程可操作 |

真实发布验收至少用两个不同 Account ID 的测试目标、一个实际workers.dev地址，验证同名Worker隔离；再用已配置的生产目标验证域名。没有第二账号只能记录多账号真实验收未完成，mock 不能代替。生产不测试覆盖未知Worker或删除现有业务。

成功证据必须包含：脱敏账号/目标、operation/export/release ID、Cloudflare version/deployment ID、全量manifest摘要、公开地址、时间、GET/HEAD/资源/元数据检查、断开CMS后访问结果。不得提交Token/JWT。此门槛通过后才可称“已经能顺利发布”。

## 17. 建议实施分界与交付物

本节只定义交付边界，不代替逐步实施计划，也不授权开始实现。

1. 静态导出与只读适配器：完整文档/依赖解析、来源冻结、ZIP、下载、诊断；C01–C07。
2. 多账号与目标：凭证、权限、专用Worker初始化、域名核验、归档；C08–C10、C20。
3. 部署引擎与Worker：完整站点合并、版本激活、持久任务、恢复、回滚/撤下；C11–C19。
4. 后台与文档：所有入口、操作确认、API/OpenAPI、运维/备份/实测报告；不得将缺失界面当“只有API也完成”。

交付文档：新API OpenAPI（建议独立 `docs/openapi/static-delivery-v1.yaml`）、导出与多账号使用说明、Worker专用部署/恢复runbook、新模块配置说明、C01–C20测试报告及真实Cloudflare验收记录。原 `docs/API.md` 仅追加新接口索引，不改旧接口契约；旧大设计/实施计划保留原义，引用本附加设计即可。

## 18. 审查结论与已知限制

在上述边界下，与当前项目架构兼容；Cloudflare 官方接口支持所需资源上传、独立版本上传和部署激活。**这是代码与平台文档层面的可行性结论，不是运行成功保证。** 必须完成 C10/C16 及双账号真实验收才能关闭上线门槛。

已知限制明确接受：仅完整HTML、静态依赖可解析的报告；不自动接管未知Worker；不自动改DNS；不自动同步CMS撤下；不在Worker上提供表单/搜索/后台；不恢复导出前已经丢失的历史图片；不承诺无限规模、永久免费或社交平台即时刷新；不支持多控制端无协调地同时写同一Worker。
