# 代码 Review 问题修复与验证记录

- 日期：2026-10-03，Asia/Shanghai。
- 项目根目录：`/Users/ken/workspace/newsPage`。
- 修复基线：`ed50e73`，工作分支 `main`；本记录随本轮修复提交交付，不代表已推送或部署到生产。
- 对应问题清单：[设计方案与实施计划代码复核](2026-10-02-final-plan-code-review.md) 的 F01–F13。
- 设计文件：`docs/LightCMS_V3_模板化页面生成与静态发布系统_最终设计方案.md`。
- 设计 SHA-256：`15db7eab7e7818906f56d156401b101c416a1b2caf48c0687d299da2e0659e48`，实施计划已同步。

## 1. 修复范围

本次直接修复既定功能契约、发布一致性与故障恢复问题，没有引入额外安全产品功能，没有新增前端、后端或独立 worker 服务。原复核报告保留为历史审查证据；当前状态以本记录及对应测试为准。

| 问题 | 修复内容 | 关键验证 |
| --- | --- | --- |
| F01 普通文本被当作 HTML | `text/textarea/date/select` 等保留字符串转义；只有 `markdown/richtext/rawhtml` 进入对应 HTML 处理路径。完整渲染器与默认渲染器同时修正。 | 普通标题中的标签和实体按字面呈现；旧 E2E 不再断言错误的原样标签透传。 |
| F02 创建崩溃恢复被 upsert 拦截 | 同 operation 的 durable Content binding 优先于新建路径冲突；`upsert=false` 不阻止恢复已经由本操作创建的资源。 | 默认创建在绑定后恢复；terminal stage failure 使用原 key/body 成功，不重复写 Content Version。 |
| F03 Rollback/Revert/Restore 恢复不闭合 | 识别带 `activated_at` 的已提交 Publication，即使之后已 superseded/unpublished，也恢复原响应。Restore 已绑定版本不再次写入。旧发布入口支持过期租约接管。后台发布和升级外围不再把不确定提交误标为 terminal。 | 三类历史命令 activation→cache 故障恢复；Restore cutover 前后不重复增版；后台与 Upgrade Job 保留原 attempt/Publication。 |
| F04 创建重放误要求 edit 权限 | 副作用前持久化 `command_kind`，重放按原 created/updated 命令检查当前角色和 scope，而非按今日资源存在状态重新分类。 | create+publish 凭据可重放原创建；新 key 不能编辑既有页面。 |
| F05 Completed 重放被可变状态阻止 | 只读定位和原命令授权完成后，优先重放或恢复已提交结果；不重新检查模板当前状态、发布 limiter 或存储 availability。执行中恢复使用已固定模板版本。 | 模板 deprecated、存储故障、限流和后续下线不破坏原结果；新命令仍受全部 gate 约束。 |
| F06 slug 迁移与当前版本不一致 | slug 与新 Template Version/current_version 在一个事务中更新；历史版本不变，不创建页面 redirect，也不切换 Publication。 | 新 slug 返回版本 2 的新契约；唯一冲突和 stale CAS 不产生半完成迁移；历史及在线字节不变。 |
| F07 可变依赖未 durable 冻结 | Render 前 CAS 保存完整 `render_snapshot`，恢复直接读取原快照。依赖 hash 覆盖实际 Snippet、Wikilink 和 query cache 内容。JSON 快照上限 8 MiB，超限明确拒绝，不回退到 live dependencies。 | 修改 Snippet 后同 attempt 字节不变；新尝试的依赖 hash 变化；快照不可覆盖、过期所有权不能读取/写入，超限不保存。 |
| F08 旧 worker 能借用新 generation 提交 | 从执行开始保留 owned generation，全程 heartbeat 与超时控制；阶段边界、最终文件 rename 和激活事务均校验所有权。事务写入 operation fence，接管与激活竞争产生 write conflict。失去所有权后不得 MarkFailed、Abort 或补偿新 owner 的候选。 | Render、Verify 和 Activate 接管后旧 worker 不能提交 active、写 Outbox 或把新 owner 的 Publication 标记 failed；final rename guard 阻止公开文件写入。 |
| F09 升级静默漏掉第 501 页 | 删除隐式 500 页限制，按 `_id` 稳定排序构造完整 preview/job。 | 501 个关联页面全部进入 Preview 和 Job，可追踪结果。 |
| F10 重放丢失 warnings | 完整缓存/还原 warnings，并在发布前持久化 `response_metadata`，覆盖 activation→cache 崩溃窗口。 | 普通 BSON 往返及后续下线后的恢复均保留原 warnings 和 Publication ID。 |
| F11 幂等 owner 不是凭据身份 | Actor 与 SessionUser 携带独立 CredentialOwner；API key 使用数据库 ID，OAuth 使用 client+resolved subject。MCP loopback 保留原 OAuth 身份，不折叠为共享 system key。用户 ID 仍用于用户/sandbox 归属。 | 同用户不同凭据不重放同一个 operation；同一凭据仍严格执行 key/body 冲突规则。 |
| F12 hash 遗漏并发前置条件 | Rollback/Restore/Revert 的 canonical 语义请求包含 `expected_active_id`；恢复先识别原结果，再应用新执行所需的 active precondition。 | 相同 key 只改变 expected_active_id 时，三种命令都返回幂等冲突。 |
| F13 单实例 gate 丢失后仍继续服务 | Mongo 唯一 writer slot 原子认领，heartbeat 校验 owner/incarnation/有效期与 matched count。独立 watchdog 避免续租 I/O 阻塞停止决策；生产丢失租约时 fail-stop。 | 第二实例拒绝、missing row 不可续租、所有权丢失通知终止路径；既有过期/重启/reap 场景回归。 |

F03 的补偿检查还发现：同一 cutover 在崩溃后重试，会用已经可见的新 canonical 覆盖 `.previous-{oldID}`。现已保留已存在的旧备份，并用真实文件测试证明二次 cutover 后补偿仍恢复原始字节。

## 2. 回归测试

新增 23 个 `TestFinalReview*` 测试函数，包含多模式和多阶段子测试；另扩展后台发布测试，并纠正旧测试中“必须继续复现已知缺陷”的断言。主要文件：

- `internal/product/generation/final_review_test.go`：创建、重放、权限、历史命令、501 页升级、渲染/验证/激活接管、崩溃恢复。
- `internal/product/idempotency/command_authority_test.go`：原命令与响应 metadata、只读 lookup、凭据隔离、事务 fence。
- `internal/product/idempotency/render_snapshot_test.go`：快照持久化、不覆盖、8 MiB 上限、过期所有权。
- `internal/product/templatecontract/slug_migration_test.go`：slug 契约迁移、唯一冲突、CAS 与历史不可变。
- `internal/product/storage/cutover_recovery_test.go`：二次 cutover 补偿字节、最终 rename guard。
- `internal/services/saga_snapshot_recovery_test.go`：真实依赖变更与同 attempt 恢复。
- `internal/services/publish_internal_16d_test.go`：后台不确定 cutover 不能 terminalize。
- `cmd/server/instance_gate_loss_test.go`：missing heartbeat 与 gate ownership 丢失。
- `internal/handlers/render_snapshot_status_test.go`：旧入口渲染容量错误映射 422。

测试使用 `lightcms-mongo-test` 的单节点 replica set，不使用开发 Mongo 容器或生产数据库。普通测试数据库名称均以 `lightcms_test_fix_review_` 开头；E2E 使用代码固定的 `lightcms-test-e2e`，每次运行前确认 collection 列表为空。测试临时文件及最终阶段 Go build cache 放在项目根目录内。

## 3. 验证结果

| 验证 | 结果 |
| --- | --- |
| 最新完整 `go test -p 1 ./... -count=1`，配置 replica-set MongoDB | exit 0，包括数据库集成、后台、REST/MCP、OAuth 和 E2E |
| `go vet ./...` | exit 0 |
| `go build ./cmd/server ./cmd/mcp ./cmd/cli` | exit 0 |
| Product 九个实现包的数据库测试与 coverage profile | exit 0，整体 **85.1%** |
| `git diff --check` | exit 0 |

覆盖率按与原复核相同的按包 instrumentation 汇总，不使用跨包 instrumentation 改变口径；E2E 包无产品实现代码，单独包含在完整测试中。单包覆盖率：Generation 85.3%、HTTPAPI 92.1%、Idempotency 89.4%、Migration 84.5%、Pathkey 97.4%、Publication 80.3%、PublicURL 96.8%、Storage 83.6%、TemplateContract 95.6%。这里的 85.1% 是 Product 整体覆盖率，不声称每个包或每一条新增分支都达到 85%。

保留的验证输出：[完整测试](2026-10-03-validation/go-test-all.log)、[覆盖率测试](2026-10-03-validation/product-coverage-tests.log)、[逐函数覆盖率](2026-10-03-validation/product-coverage.txt)。vet/build 输出为空且 exit 0，退出状态以本表和执行记录为准。

## 4. 已明确的实现边界

1. **Filesystem MVP 只允许一个应用进程写同一站点数据库。** 部署必须先停旧实例再启新实例。门禁不是多副本分布式锁，Scale-out 仍需后续共享锁/任务租约/对象 pointer 验证。
2. **完整渲染快照 JSON 上限为 8 MiB**，与 data 5 MiB 上限同时成立。超限返回 `RENDER_VALIDATION_FAILED`，不缓存为“纯请求验证” completed，因为此前可能已写入草稿绑定。
3. **API key 新记录代表新幂等 namespace。** 更换请求体或更换 expected version 使用新 key；丢失响应使用原凭据、原 key、原 body 恢复。Legacy OAuth 没有持久化 subject 字段，其 resolved subject 是现有 system user，但 client 身份独立。
4. 本次没有部署生产、没有运行 GitHub 远端 CI、没有对真实生产数据做迁移演练、没有完成外部 Webhook 接收端或 600 秒双进程网络分区实验。ADR 仍保留人工签审状态，不虚构批准。
5. 原报告中 preview 产品契约、Unicode fold 范围及生产域名/部署验收等交付边界不是 F01–F13 的代码缺陷。本记录不把修复 13 项等同于这些事项也已完成。

## 5. 交付状态

代码、回归测试、设计约束、ADR、生产部署说明、OpenAPI 错误码及实施计划 spec hash 均在同一项目根目录管理。本记录的验证通过后，按用户明确要求提交本地代码并重启现有开发环境；不 push、不变更生产环境。
