# 设计方案与实施计划代码复核

- 日期：2026-10-02，Asia/Shanghai。
- 审查提交：`ed50e73`，包含 `53c0fbc` 的上一轮修复。
- 设计基准：`docs/LightCMS_V3_模板化页面生成与静态发布系统_最终设计方案.md`，SHA-256 `60222a89251cd6e15fc1d9e10d8ad7ca11f45bf6bf44fdb9f7a3c16100704f86`，与实施计划记录一致。
- 范围：Task 0–19 的实现文件、生产接线、核心状态机、旧入口迁移、Admin/REST/MCP/CLI 相关接线、测试与交付证据。
- 本轮为审查，未修改应用实现；临时故障诊断只使用独立测试数据库与项目根目录内的临时文件。
- 按本轮用户要求，结论聚焦既定功能契约、数据一致性和故障恢复，不新增额外安全加固要求。

## 审查结论

主体功能与模块均已建立，上轮若干关键问题确已修改。当前完整数据库测试、E2E、vet、入口构建均通过，Product 整体覆盖率为 85.7%。但补充诊断仍复现了字段类型呈现、幂等恢复、执行者所有权、模板迁移和批量升级遗漏等问题。因此，不能将“代码与测试齐全”认定为“所有功能已按实施计划完善实现”。

本报告列的是 `ed50e73` 当前问题，不是重发上次 `de943b7` 的问题清单。下文标注“实测”的问题已执行隔离诊断；“静态确认”有明确代码路径，但未执行对应网络／跨进程故障实验。

## 1. 当前验证结果

| 验证 | 结果与边界 |
| --- | --- |
| `go test -p 1 ./... -count=1` | 本轮配置 replica-set MongoDB 后 exit 0，包括 `internal/product/e2e` |
| `go vet ./...` | exit 0 |
| `go build ./cmd/server ./cmd/mcp ./cmd/cli` | exit 0 |
| Product 覆盖率 | `go test -p 1 ./internal/product/... -coverprofile=...` 后 `go tool cover -func`，整体 85.7% |
| 单模块覆盖率 | Generation 85.3%、HTTPAPI 92.1%、Idempotency 89.8%、Migration 84.5%、Pathkey 97.4%、Publication 81.7%、PublicURL 96.8%、Storage 84.0%、TemplateContract 95.7% |
| 生产接线补充诊断 | 下列独立失败场景证明现有全部 green 测试仍存在覆盖缺口 |

测试使用现有 `lightcms-mongo-test` 的单节点 replica set，普通 suite 数据库为 `lightcms-test-final-review-20261002`，覆盖率 suite 为 `lightcms-test-final-coverage-20261002`，诊断为 `lightcms-test-final-probe-20261002`。E2E 使用代码固定的 `lightcms-test-e2e`，运行前确认其 collection 列表为空；测试在该独立库中生成并清理数据。没有连接开发 Mongo 容器或生产库。

关键诊断输出：

```text
unchanged_create_publish_key_replay: PERMISSION_DENIED
text_type_contract: markup_interpreted=true literal_text_escaped=false
warning_replay: first=1 repeat=0
same_render_input_after_snippet_edit: bytes_changed=true dependency_hash_changed=false
old_worker_after_lease_takeover: response=REQUEST_IN_PROGRESS active_publication_committed=true
crash_recovery_default_upsert: PATH_CONFLICT
rollback_activation_before_cache_recovery: PUBLICATION_CONFLICT
restore_retry: PUBLICATION_CONFLICT
restore_retry_content_version: before=2 after=3
completed_replay_after_deprecated: TEMPLATE_NOT_ACTIVE
slug_migration_requested=migrated-review current_version_slug=final-review
upgrade_preview: actual_pages=501 returned_pages=500
```

## 2. 存在的问题

### F01 · P1 · 普通 text 字段被按富文本呈现，与字段契约不一致

- 对应计划：Tasks 4、7、13、17；设计字段类型契约。
- 位置：`internal/product/publication/render.go:378`、`:398`；生产适配器 `internal/services/saga_render.go:124`。
- 证据：**实测**。生产同款 SnapshotRender 适配器下，字段 `headline` 类型为 text、ScriptPolicy=none；输入 `<strong>literal text</strong>` 后，输出解释了 strong 标签，没有把它作为字面文本呈现。
- 原因：按字段类型处理 Markdown／richtext 后，所有字符串仍统一转换成 `template.HTML`。text／textarea 没有保持设计表格规定的“转义文本”语义，新闻标题中包含 HTML 示例时显示结果也会失真。
- 改进：text、textarea、date、select 等保持正常字符串转义；富文本只在对应字段类型的处理路径输出 HTML。
- 验收：普通文本中的标签、尖括号和实体按字面文本显示；Markdown、richtext、rawhtml 各按自身字段契约输出。

### F02 · P1 · 默认 `upsert=false` 的创建请求崩溃后无法恢复

- 对应计划：Tasks 8、11、12、17；设计 §21.5。
- 位置：`internal/product/generation/service.go:311`；`service_publish.go:34`。
- 证据：**实测**。首次创建已写 Content 并进入 cutover，使用已有 `ErrStopAfterRename` 故障点终止；同 key 重试已取得 operation 绑定，却仍因 path 已存在返回 PATH_CONFLICT。
- 原因：Upsert 冲突判定先于绑定恢复分支，没有区分“本 operation 已创建的 Content”和“其他请求创建的 Content”。
- 改进：优先按 durable operation binding 识别恢复操作，合法恢复不走新建的 path/upsert gate；不能要求客户端改变 body 为 upsert=true，因为相同 key 改 body 会产生幂等冲突。
- 验收：默认创建在 Content commit、Render、Stage、cutover 各阶段崩溃后，原 key/body 均能恢复同一资源。

### F03 · P1 · Rollback／Revert／Restore 的恢复仍没有闭合

- 对应计划：Tasks 8、11、12、15、17。
- 位置：`generation/schema_pub.go:108`、`restore.go:97`、`:106`、`:224`；`publication/repository.go:195`。
- 证据：**实测**两种情况：
  - 模拟 rollback 已 activation、但成功响应缓存尚未完成的合法崩溃状态；同 key 重试再次尝试插入相同 ID 的 active Publication，返回 PUBLICATION_CONFLICT。
  - RestoreAndPublish 在首次 cutover 崩溃后重试，又创建 Main Content Version，版本从 2 增加到 3，然后返回 PUBLICATION_CONFLICT。
- 原因：这些命令虽然接入了 Begin/TakeOver，但没有像 Generation 的部分路径那样，根据已绑定 Content Version／Publication 状态继续或重放。Restore 每次仍无条件计算 next version。
- 改进：统一命令恢复协议；active／superseded 的已提交历史结果需识别，绑定版本已提交时不得再次写版本。不能简单放宽 InsertStaged 让 active 重新 stage。
- 验收：所有 live-changing 命令在 activation→cache 窗口和 Content→Render 窗口崩溃后均只生成一次目标版本／Publication。

### F04 · P1 · 已成功 create 的合法重试被重新按 edit 权限判定

- 对应计划：Tasks 11、12、17。
- 位置：`generation/service.go:216`、`:627`。
- 证据：**实测**。editor key 仅含 content.create+content.publish，首次创建发布成功；同 key/body、权限未变化，第二次因目标已经存在要求 content.edit，返回 PERMISSION_DENIED。
- 改进：先做无写入的缓存定位与身份验证；已完成操作按原命令要求检查当前角色／scope，不能按现有目标状态把 create 变成 edit。仍需禁止权限真正被撤回后的越权重放。
- 验收：create-only+publish key 可重放原创建；仍不能用新 key 编辑已存在页面。

### F05 · P1 · Completed 重放仍被当前模板状态等可变条件阻止

- 对应计划：Tasks 11、12、17；上轮 R06 的残余。
- 位置：`generation/service.go:132`、`:154`。
- 证据：**实测**。成功发布后把模板变为 deprecated，相同 key/body 返回 TEMPLATE_NOT_ACTIVE。当前 limiter／store availability 检查同样在缓存读取之前，具有同类静态偏差。
- 改进：经当前身份授权后优先读取 completed 结果，不再为已经完成的操作检查当前模板状态／存储 availability；新 operation 仍遵守全部条件。
- 验收：模板升级／deprecated、存储短暂故障、目标状态变化均不丢失原成功响应。

### F06 · P1 · 模板 slug 迁移后，当前不可变版本与新机器标识不一致

- 对应计划：Tasks 3、12；设计 §8.1 与 §9.3。
- 位置：`generation/migrate.go:67`；`templatecontract/service.go:266`。
- 证据：**实测**。迁移到 `migrated-review` 后，GetCurrent("migrated-review") 返回的 TemplateVersion.Slug 仍为 `final-review`。
- 原因：只更新 mutable Template.slug，没有创建带新 slug/ContractHash 的当前版本；当前版本仍引用历史旧标识。
- 影响：新 slug 的 Schema 响应、Generation DTO、模板系统变量可能输出旧 slug，外部集成迁移提示不对应实际契约。
- 改进：迁移以事务创建新的 TemplateVersion 并前移 current_version；历史版本保持原值。继续不改页面 URL、不建 redirect、不切换 Publication。
- 验收：新 slug 获取的 current version 与 Schema 标识一致，旧历史版本与线上字节不变。

### F07 · P1 · 可变依赖没有 durable 冻结，依赖 hash 也未覆盖实际依赖

- 对应计划：Tasks 7、8、11；设计 §18.5、§21.5。
- 位置：`services/saga_render.go:20`、`:129`；`publication/render.go:277`。
- 证据：**实测**。同一个 RenderInput（相同 Publication ID／logical time／TemplateVersion）中修改引用的 Snippet 后，输出字节变化，但 RenderDependenciesHash 不变。
- 原因：每次恢复重新读取 Snippet、Wikilink index 与查询结果；存入 Publication 的 DependencySnapshot 仅有处理器版本和策略，未包含实际 Snippet/Theme/查询依赖内容或 hash。
- 影响：同 attempt 重渲染不确定；Stage/InsertStaged 的同 ID/hash 检查可能阻止恢复；依赖 hash 不能用于可信的变更诊断。保留 immutable 字节期间的精确回滚仍可成立，但不能据此宣称输入快照已完整实现。
- 改进：首次 Render 前持久化有界依赖快照／版本引用，或禁止 Product Template 引用无法冻结的依赖；依赖 hash 必须覆盖实际引用内容。超大依赖需要明确产品限制与 ADR，不可仅在代码注释中放宽设计。
- 验收：依赖在崩溃窗口被修改，同一 attempt 仍可按原输入恢复，依赖变化有准确 hash。

### F08 · P1 · 被接管的旧 worker 仍能提交 active Publication

- 对应计划：Tasks 8、11、17；设计 §21.5。
- 位置：`publication/saga.go:456`、`:724`、`:733`。
- 证据：**实测**。在 Render 返回前使租约到期并通过 TakeOverByKey 增加 lease_generation；旧执行者继续进入 cutover，最终提交 active Publication，上层只在完成缓存时返回 REQUEST_IN_PROGRESS。
- 原因：heartbeat 在 Render 之后才启动；executeCutoverPlan 读到当前 generation 后，用当前 generation 启动 heartbeat，却未核对它是否属于本执行者。检查响应缓存时的 fencing 已晚于文件和 active 事务副作用。
- 改进：从开始执行时携带固定 owned generation，全程续租；Render 前及每次文件／DB 副作用前检查同一 ownership。不能从数据库读取新 owner 的 generation 并借给旧 worker。
- 验收：在 Render／Stage／cutover 前插入接管，旧 worker不能更新文件、active 状态、Outbox 或响应缓存；长步骤 timeout 小于剩余租期。

### F09 · P1 · 模板升级静默只处理前 500 个页面

- 对应计划：Tasks 12、15、16D、17。
- 位置：`generation/upgrade.go:118`、StartUpgradeJob。
- 证据：**实测**。模板关联 501 个 live Content，PreviewUpgrade.TotalPages 只返回 500；Job 从 preview.Items 建立，剩余页面不会加入。
- 原因：无排序／分页的 SetLimit(500)，且结果没有 total/truncated/cursor 提示；实施计划未约定 500 页上限。
- 改进：分页或流式构建完整任务；若采用产品上限，必须显式拒绝／提示，并修改设计与 API 契约，不能静默漏项。
- 验收：501 页及多页批次测试，真实总量一致，全部符合条件的页面可追踪到完成／失败／跳过结果。

### F10 · P2 · 成功重放没有保留原 warnings

- 对应计划：Tasks 11、12、18。
- 位置：`generation/service_publish.go:126`、`:404`。
- 证据：**实测**。首次 publish 因字段默认值返回一个 warning；相同请求重放 warnings 数量变为 0。
- 改进：缓存并还原完整响应，包括 warnings 和字段细节；审查 BSON round-trip。
- 验收：原 status/body/resource IDs 的重放一致性测试，而非仅断言 Publication ID 不变。

### F11 · P2 · 幂等 owner 仍是用户 ID，而非凭据身份

- 对应计划：Task 11；设计 §21.5。
- 位置：`generation/types.go:161`、生产 ActorExtractor 与 HTTP IdempotencyParams。
- 证据：**静态确认**。Actor.Owner 返回用户 ID／Email，未携带 API key 数据库 ID 或 OAuth client+subject。相同用户的两个独立集成共用相同 key 字符串与端点时，会相互冲突／重放。
- 改进：认证中间件注入稳定凭据 owner，保留不同 API key、OAuth client 的隔离语义；与现有 key rotation 策略一起说明。
- 验收：同一用户、不同凭据、相同 Idempotency-Key 的不同请求可独立执行；同一凭据仍严格冲突／重放。

### F12 · P2 · Rollback/Restore/Revert 的请求 hash 遗漏 expected_active_id

- 对应计划：Tasks 11、12、17。
- 位置：`httpapi/publications.go:109`、`:169`、`:221`；`generation/schema_pub.go:103`、`restore.go:86`、`:211`。
- 证据：**静态确认**。Handler 传 Body:nil，命令内部 hash 只重建 source/version 等字段，没有 expected_active_id。相同 key 下改变该并发前置条件不会按“different body”统一返回 IDEMPOTENCY_CONFLICT；不同命令还可能先被当前 active 检查拦截。
- 改进：对完整语义请求体做统一 canonical hash，覆盖并发前置条件；稳定的 server-derived 字段另存 snapshot。
- 验收：只改变 expected_active_id 的同 key 请求，按约定冲突，所有命令行为一致。

### F13 · P1（故障条件）· 单实例 gate 在长期 heartbeat 丢失后仍可放行第二实例

- 对应计划：Task 16E、19；上轮 R10 后续风险。
- 位置：`cmd/server/instance_gate.go:86`、`:207`。
- 证据：**静态确认，未做 600 秒双进程故障实验**。heartbeat 失败只记录 Warning；TTL 清理旧记录后，UpdateOne 的 MatchedCount=0 也不被检查。旧实例继续服务，新实例可因没有 fresh rival 而启动。底层 path/content 锁仍是各进程独立。
- 改进：使用有唯一槽位与 ownership 的 lease，并在失去 ownership 时停止写入／停止服务；明确文件存储拓扑。若维持单实例限制，必须真正 fail-stop，不能仅在启动时检查。
- 验收：暂停 heartbeat 超过窗口、TTL 删除、实例恢复、并发启动，不能出现两个有效写入者。

## 3. Task 0–19 对照结果

“存在实现”表示文件和接线已核对；不表示所有边界已验收。“问题关联”指本报告的具体缺陷。

| Task | 要求 | 当前判断 |
| --- | --- | --- |
| 0 | replica-set 测试环境、ADR、CI门槛 | 本轮测试环境可用，ADR 文档存在；CI／签字证据见第 4 节 |
| 1 | 基线、入口清单 | 基线与入口矩阵已提供；审查固定当前提交，未以历史行号当作当前实现证据 |
| 2 | 事务、路径、模型、索引 | 实现存在、数据库测试通过；Unicode case-fold 仍使用 ToLower，若要求完整 Unicode fold 应补充范围与测试 |
| 3 | 模板 slug、版本与 CAS | 实现存在；F06 当前 slug 迁移与版本契约不一致 |
| 4 | 字段验证、Schema、新类型 | 校验／Schema 存在；最终输出的普通文本契约存在 F01 |
| 5 | Publication 模型、状态、CAS | 实现存在、测试通过；恢复命令与模型接缝仍有 F03 |
| 6 | immutable Stage/Verify/文件切换 | 上轮先移走旧文件的流程已改为准备后覆盖 rename；新测试通过 |
| 7 | 完整渲染快照与依赖 | 完整渲染器已接入；F01、F07，长 Render 的 ownership 见 F08 |
| 8 | Publish/Unpublish/Rename/Rollback Saga | 主流程存在，redirect 已同事务；F02、F03、F08 |
| 9 | transactional Outbox、签名、重试 | transaction/worker 接线存在；本轮 E2E 通过，实际外部接收端网络环境未验收 |
| 10 | scanner、GC、legacy保护 | 实现存在，本轮测试通过；不能替代 HTTP operation 的精确恢复 |
| 11 | execution snapshot、租约、重放 | 实现存在但 F02–F05、F07、F08、F10–F12 说明边界未完善 |
| 12 | Generation/Schema/迁移/升级/恢复API | 路由存在；F02–F06、F09；preview 分支目前只返回状态元数据，未执行渲染，需明确它与既有 authenticated Preview 的产品契约 |
| 13 | Public URL、远程资源导入 | URL／远程获取／大小校验存在，测试通过；真实生产域名尚未验收 |
| 14 | 迁移／回填／冲突处理 | 当前实现与测试存在；真实生产数据副本演练仍属部署前工作 |
| 15 | 单Admin、Fork编辑、升级、恢复UI | handler/route/控件已存在，相关 suite 通过；背后依赖 F03、F06、F09 |
| 16 | 所有旧入口接线、单进程、可观测性 | 接线已有，未发现重新拆服务；单实例替代设计中的 scale-out 应正式对齐方案／ADR，F13仍需处理 |
| 17 | E2E／故障矩阵／≥85%覆盖率 | 全量 DB/E2E green、整体85.7%；本报告新增失败场景证明故障矩阵不足 |
| 18 | API/OpenAPI/Agent/升级/运维文档 | 交付文件存在；需随 F02–F12 修复同步，不能把现有文档视为行为一致性证明 |
| 19 | release演练与签字 | 构建与本轮测试证据齐；第4节未完成门槛阻止“全部完成”的交付结论 |

## 4. 尚未完成的验收与计划偏差

1. `.github/workflows` 当前为空，Git 中无 workflow 文件；实施计划要求的 CI 门槛尚无当前 release commit 的执行证据。这是用户先前删除后的已知状态，本轮不恢复文件。
2. `docs/implementation/release-checklist.md` 仍有外部签字等未勾选项；生产数据副本迁移、真实 CDN/Webhook、生产域名社交分享抓取属于实际部署验收，不能由本地 unit/E2E 代替。
3. 设计 §32 允许同一应用 scale-out，并要求 Mongo lease；当前选择单实例启动 gate。单服务与多副本并不矛盾，因此这是需要正式对齐文档的功能收缩，不能仅以“没有多个服务”判定全部架构要求已实现。
4. Generation mode=preview 是验证／元数据响应，当前没有渲染 HTML 或预览 URL。若它应承担设计 §14.2 所指的页面预览，需完善输出与调用路径；若仅用于验证，应修改命名和 API 文档避免误解。此项先列为契约确认，未作为已复现运行缺陷计数。

## 5. 上轮问题复核

- R01 角色越权：Actor.Role／Can 与生产 extractor 已接入，本轮没有重列原来的空 scope viewer 越权。
- R02 完整渲染器接线：已修，但普通 text 类型呈现偏差以 F01 留存。
- R03 公开文件缺失窗口：切换实现已改，新测试通过，不重列旧两次 rename 缺陷。
- R04 接管：已调用 TakeOver，但恢复不覆盖默认创建及 restore/rollback（F02/F03）。
- R05 403 占用租约：授权前置已改，原问题不重列；新的重放鉴权语义见 F04。
- R06 模板版本重放：version value gate 已后移，模板 deprecated 等前置仍阻断（F05）。
- R07 续租／fencing：代码已增加，旧执行者提交仍复现（F08）。
- R08 redirect 事务：已进入 ActivateCASWithRedirect，不重列旧 post-commit upsert。
- R09 sandbox：生产已接入 owned active Fork resolver，未重列无绑定问题。
- R10 多实例：增加单实例 gate，但条件性故障风险与架构偏差见 F13、第4节。
- R11 不确定提交：unknown commit 分支和 staged 恢复已增加；更广的命令恢复边界仍有 F03。
- R12 HTTPS 基址：production gate 已接入，不重列原静默启动降级。

## 6. 建议处理顺序

先统一所有外部命令的恢复／重放协议（F02–F05）并处理 F08 执行者所有权，再修 F01 字段类型呈现、F06 模板版本迁移、F07 依赖快照、F09 完整升级范围。F10–F12 纳入同一幂等契约回归；F13和单实例架构决策在生产发布前落实。最终重新执行全量数据库/E2E、故障矩阵与当前提交的 CI，并完成部署验收。

本轮结论：**主要模块已实现且现有测试全部通过，但仍有已复现缺陷和未完成交付门槛，不能确认所有功能已按计划完善实现。**
