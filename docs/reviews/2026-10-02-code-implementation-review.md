# LightCMS 代码实现审查

日期：2026-10-02（Asia/Shanghai）  
审查提交：`de943b7`  
基准：`docs/LightCMS_V3_模板化页面生成与静态发布系统_最终设计方案.md` 与实施计划。

## 结论

代码已经形成一个应用进程内的模板版本、Generation、Publication、存储、Outbox 和恢复模块，主体结构符合设计方向。但当前存在可复现的权限绕过和输出安全问题，以及发布文件切换、幂等恢复和流程接线缺口，不能确认已经正确、完整实现全部设计要求，不能据此直接批准生产上线。

本轮只做审查和诊断，没有修改应用实现。下列“实测”使用当前代码与独立测试数据库；“静态确认”指由生产接线和调用链确定，尚未进行对应的故障或跨实例实验。

## 验证范围与证据

- `go vet ./...`：通过。
- 未配置测试 URI 的 `go test ./...`：失败；E2E 明确要求 replica-set MongoDB，普通 DB 测试可能跳过，不能当作全量通过。
- 使用现有 `lightcms-mongo-test` 容器的 replica set，独立数据库 `lightcms-test-review-20261002`，执行 `go test -p 1`，Generation、Publication、Idempotency、TemplateContract、Storage 五个模块全部通过。
- 补充诊断使用另一个独立数据库 `lightcms-test-review-probe-20261002`，未连接开发／生产数据库。结果如下：

```text
renderer: unsafe_onerror_retained=true markdown_not_rendered=true
RBAC: viewer_original_can_create=false facade_create_succeeded=true
denied_first: PERMISSION_DENIED
authorized_retry_after_403: REQUEST_IN_PROGRESS
expired_retry: REQUEST_IN_PROGRESS / IDEMPOTENCY_LEASE_EXPIRED
production_saga_publish: text_field_onerror_retained=true
completed_retry_after_template_update: TEMPLATE_VERSION_CHANGED
```

文件切换诊断使用约 12 MiB 新文件，在切换期间并发 `os.Stat` 观察到 canonical 路径不存在（一次运行 22,914 次）。这是文件不存在的观察次数，不是 HTTP 请求失败率；足以证明当前流程存在缺失窗口。

本轮未运行带数据库的完整 E2E：其数据库名被硬编码为 `lightcms-test-e2e`，会直接 drop 整个数据库。为避免清理正在使用的既有测试数据，本轮仅使用可单独指定数据库名的核心模块测试和隔离诊断。未将既有发布报告中的历史通过记录当成本次验证结果。

## 必须修复的问题

### R01 · P0 · 新产品 API 丢失用户角色权限约束

**位置：** `cmd/server/publication_runtime.go:118`、`internal/product/generation/types.go:98`、`cmd/server/main.go:604`。

生产 ActorExtractor 只传 `IsAdmin` 与 API key scopes，没有传角色权限集合；`HasScope` 又将空 scopes 直接视为全部允许。新产品路由只经过认证链，没有逐条套用原有 `auth.UserHasPermission`。原来的“空 scopes 代表用户角色的全部权限”被扩大成“所有权限”。

**实测：** viewer 的原有 `UserHasPermission(content.create)` 为 false，但按生产 extractor 构造的非管理员、空 scopes Actor，通过 Generation 成功创建 Main Content。

**影响：** 低权限身份通过新接口创建／编辑／发布内容；rollback、restore 和 upgrade 等使用同一 Actor 授权体系的命令也需逐项检查。

**修复：** 保留角色权限，并与 key scopes、sandbox restriction 求交集；所有入口使用同一授权实现。不能仅对管理员做布尔判断，也不能把空有效权限集合再次解释为无限权限。

**验收：** viewer/contributor/editor/admin × 空／受限 scopes × 各模式及 restore/rollback/upgrade 的真实 HTTP 测试；拒绝请求零业务写入。

### R02 · P0 · 完整渲染器未接入生产发布，普通文本可变为可执行 HTML

**位置：** `cmd/server/publication_runtime.go:91`、`internal/product/publication/service.go:179`、`internal/product/publication/saga.go:160`。

生产构造未提供 Renderer，默认选择 `DefaultRenderer`。该渲染器将所有字符串直接转换为 `template.HTML`，没有按字段类型区分转义文本与消毒后的富文本；`ScriptPolicy=none` 只搜索 `<script`，无法阻止事件属性等向量。完整处理 Markdown、Snippet、Theme、Wikilink、TOC 的 `render.go` 存在，但未被生产 saga 接入。

**实测：** 将 `<img src=x onerror=alert(1)>` 填入 text 类型 headline，经 Generation → Publication → canonical 文件后，`onerror` 原样保留；简化渲染器也保留 `**bold**` 字符串而未生成 Markdown HTML。

**影响：** 内容字段可注入存储型 XSS，且已声明的 Markdown 等能力与实际发布结果不一致；`admin_only`、`none` 策略没有按设计执行。

**修复：** 给生产 saga 接入完整快照渲染适配器；普通文本使用正常转义值，只有经字段类型和脚本策略消毒的 richtext/markdown 才标记可信 HTML；渲染必须携带可信作者权限。依赖快照与 renderer version 应来自实际执行的渲染结果。

**验收：** 通过真实发布入口验证 text 中的标签被转义、事件属性和危险协议被移除／拒绝，Markdown 正常转换，脚本策略与作者权限组合正确。只测试独立 Render 函数不足以验证生产接线。

### R03 · P1 · Republish 的 canonical 切换存在页面消失窗口

**位置：** `internal/product/storage/filesystem.go:321`。

先将 canonical rename 到 previous，再读 immutable、写 next、fsync、核对 hash，最后 rename next 到 canonical。读写与校验期间公开文件不存在；单个 rename 原子不代表两次 rename 与中间复制过程整体原子。

**实测：** 在上述 12 MiB 切换诊断中观察到大量 canonical 不存在事件。读取者不获取 saga lock，因此进程内锁也不能消除公开读取窗口。

**修复：** 旧 canonical 保持服务时先完整准备并校验 next，另行建立可恢复的旧字节备份，最后以一次覆盖 rename 完成公开切换。补偿同样需避免先删文件再恢复的窗口。必要时同步修订设计 §17.4 中会误导实施的文件步骤。

**验收：** 持续公开 GET／Open 与 republish 并发，任何时点只能读到完整旧版或新版，不能得到缺文件或部分字节；覆盖复制、fsync、rename 和进程终止故障。

### R04 · P1 · 外部 Generation 崩溃后不能接管过期幂等租约

**位置：** `internal/product/generation/service.go:483`；同类问题在 `generation/schema_pub.go`、`generation/restore.go` 的直接 Begin 路径。

`beginForPublish` 明确不调用 TakeOver。Begin 遇到过期 processing record 返回 LeaseExpired，上层只转换成 REQUEST_IN_PROGRESS，下一次重试仍走同一分支。现有恢复 scanner 不会把这条 HTTP 请求自动接管并恢复缓存响应。

**实测：** 建立 operation 并将 processing_expires_at 调到过去，相同 key 的 Generation 请求仍返回 REQUEST_IN_PROGRESS。

**修复：** CAS 接管过期租约，读取 durable execution snapshot，根据 Content/Publication 当前阶段继续或重放；不能重新无条件执行 Content 写入。若 Publication 已经 active，需要完成响应缓存而非新建 Publication。

**验收：** 分别在 Content commit、snapshot freeze、Stage、cutover、activation commit、response cache 前终止 worker，相同 key 重试均能收敛且不重复发布。

### R05 · P1 · 403 请求占用幂等键，授权纠正后仍无法立即重试

**位置：** `internal/product/generation/service.go:225`。

权限与 sandbox 检查之前调用 Begin，Begin 插入 processing record；后续 403 直接 return，没有释放租约。类似的 upsert/path/precondition 早退也需全面检查。

**实测：** create-only Actor 发 publish，首次 PERMISSION_DENIED；补齐权限后用相同 key 重试得到 REQUEST_IN_PROGRESS。

**修复：** 将当前授权与必要的无副作用检查放在新 operation 写入之前；缓存查询和创建／接管操作分开，所有非缓存错误明确释放租约。重放也必须先验证当前身份、角色、scope 与 sandbox 边界。

**验收：** 403 不产生新 processing 租约；权限变化后的同 key 合法重试可执行；409/429/5xx 路径具有相同释放约束。

### R06 · P1 · 已成功请求重放依赖当前模板版本

**位置：** `internal/product/generation/service.go:151`、`:227`。

当前模板状态／expected_template_version 校验、数据验证、限流与存储检查先于 completed 缓存读取。成功发布后只要模板升级，相同 key、相同请求无法重放原结果。

**实测：** 成功发布模板 v1，升级到 v2，重试原请求返回 TEMPLATE_VERSION_CHANGED。

**修复：** 对已完成请求，在当前调用授权有效后按稳定请求 hash 读取原 status/body，不重新执行模板当前版本、存储可用性等新操作条件；新操作仍执行全部前置条件。

**验收：** 模板升级／deprecated、发布存储暂时不可用、目标状态改变后，同 key 与同 body 仍重放原成功结果；不同 body 返回冲突。

### R07 · P1 · 续租接口存在，但执行路径未启动 heartbeat

**位置：** `internal/product/idempotency/service.go:355`、生产调用链。

静态扫描生产 Go 源码，RenewLease 只有定义，没有调用；也没有在发布副作用前验证 lease generation。设计 §21.5 要求每 60 秒续租，失去租约后立即停止，单步 timeout 小于剩余租期。

**影响：** 长操作过期后，后台接管 worker 与旧 worker 可能并行执行；部分幂等更新只校验 attempt 而非 lease generation，不能有效阻止旧 worker 写入。

**修复：** 所有执行入口接入统一 heartbeat／取消机制，Bound/Freeze/Complete 与副作用核对 ownership；丢失租约停止后续步骤，文件副作用需要配套锁和 fencing 策略。

**验收：** 短租约、长 Stage／Render、续租失败和旧 worker 恢复的故障测试，证明只能一个有效执行者切换文件。

### R08 · P1 · Rename 的 redirect 写入在 activation 事务之后

**位置：** `internal/product/publication/saga.go:791`、`repository.go:245`。

ActivateCAS 提交 Publication/Content/Outbox 后，finishCommit 才单独 upsertRedirect。与设计 §18.3“redirect 与 activation 在同一 Mongo transaction”不符。

**影响：** commit 后崩溃或 redirect 写失败会留下新路径 active、旧路径无 redirect；旧 canonical 后续可能被 scanner 作为 orphan 隔离，导致旧链接失效。post-commit 错误还会进入外部幂等未完成路径。

**修复：** 将 redirect 记录与 activation 同事务提交；文件旧路径清理和 CDN purge 作为可恢复后置步骤。事务不成功时保留旧链接。

**验收：** 在 activation commit 后立即终止进程，重启时旧路径已经有可用 redirect；不存在 active 新版但 redirect 缺失的持久状态。

### R09 · P1 · 生产 sandbox Actor 没有绑定 active Fork

**位置：** `cmd/server/publication_runtime.go:118`、`internal/product/generation/service.go:248`。

生产 extractor 只传 SandboxOnly/AgentSession，没有提供 SandboxForkID；请求 body 也不允许客户端直接传该字段。Generation sandbox 分支要求非空 SandboxForkID，因此当前新接口即使已有有效 sandbox，也不能按设计使用它。

**修复：** 通过经过授权的 Agent session／sandbox 服务查询 active Fork，验证归属、状态与有效期，再注入 Actor；不能直接信任 X-Agent-Session 字符串。

**验收：** 经真实认证中间件调用 sandbox API，有效 sandbox 成功且只写 Fork；无效、过期或其他用户的 sandbox 被拒绝。

### R10 · P1（扩容条件）· Publish／Recovery 只有进程内锁

**位置：** `internal/product/publication/saga.go:38`、恢复与发布共享的锁注册表。

map + channel 锁只作用于当前进程，没有设计 §12.8 要求的 Mongo lease path/content lock。数据库唯一索引能阻止重复记录，不能序列化不同实例对同一 filesystem 的 rename／补偿，也不能让不同本地卷自动共享正确 canonical。

**修复：** 支持 scale-out 前落实跨实例 lease／fencing 与存储拓扑；或者在配置与部署中强制单实例，并将多实例明确列为未支持能力。

**验收：** 两个应用进程与 scanner 同时操作相同 content/path，不出现覆盖已提交新版、错误补偿、重复任务或实例间不同页面。

### R11 · P1 · Activation commit 不确定且读回失败时仍执行回滚补偿

**位置：** `internal/product/publication/saga.go:724`。

ActivateCAS 返回错误后仅尝试一次 GetActive。如果读回也因网络故障失败，代码继续 failCommitted 并补偿文件。但“无法确认已提交”不等于“确认未提交”。若事务已经提交而响应丢失，会恢复旧文件，与数据库新 active／已提交 Outbox 不一致。

**修复：** 将未知提交结果作为独立恢复状态保留，不在状态不明时执行破坏性补偿或标记 terminal；在能够确认数据库状态后收敛。Unpublish 也需要对称处理这一类错误。

**验收：** 故障注入模拟事务实际提交、commit 响应丢失、紧接的读回也失败；新 active 文件不能被恢复成旧版。

### R12 · P2 · 错误的生产公开 URL 配置被静默降级

**位置：** `cmd/server/publication_runtime.go:80`、`config/config.go:92`、`internal/product/publication/saga.go:605`。

URL resolver 构造失败只记 Warning；nil resolver 时 publication 返回相对 FullPath。启动配置校验没有执行生产 HTTPS origin 校验，因此坏地址可导致上线成功但返回 URL、Webhook 与模板 public_url 不符合契约。

**修复／验收：** 生产启动调用 ValidateProductionBaseURL 并 fail-fast；空值、非 HTTPS、无 host 等拒绝启动，有效配置返回完整公开 URL。开发环境的降级规则单独明确。

## 已核对的正确实现

- 模块均在同一应用内，未拆成独立 Generation／Publication 后端。
- TemplateContract 有版本与 hash 模型、严格字段校验与 Schema 转换。
- active Publication 的 CAS、唯一索引和生命周期已实现。
- 普通 activation、Unpublish 的状态与对应 Outbox 插入处于同一 Mongo transaction。
- immutable 对象、保留与 GC、scanner、legacy_unverified 迁移保护有相应代码与核心测试。

这些正确实现不能抵消生产接线和跨模块流程的问题。当前诊断说明，单模块全部 green 仍不足以证明真实入口符合设计。

## 建议修复顺序与交付门槛

1. R01/R02：先关闭越权与 XSS，使用真实生产 wiring 的安全回归。
2. R03/R04/R05/R06/R07：修复公开切换与幂等状态机，完成 crash／lost-response／lease 故障矩阵。
3. R08/R09/R11：补齐 Rename、Sandbox 与不确定提交处理。
4. R10/R12：明确部署拓扑和公开 URL 强制配置。
5. 在隔离 replica-set 实例执行完整 E2E、race 和真实路由回归；修复后更新发布证据，而不是沿用历史测试结论。

上述门槛完成前，本轮审查结论为“主体结构已实现，但存在生产阻断问题”。本轮尚未覆盖每个后台页面、CLI/MCP 全工具、外部 CDN/Webhook 的实际网络环境，不能承诺除此之外没有其他问题。
