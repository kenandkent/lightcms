# 日常运维（Operations）

> 配套文档：[DEPLOYMENT-PRODUCTION.md](DEPLOYMENT-PRODUCTION.md)（生产部署）·
> [PUBLICATION-RUNBOOK.md](PUBLICATION-RUNBOOK.md)（发布运维）·
> [BACKUP-RESTORE.md](BACKUP-RESTORE.md)（备份恢复）·
> [SECURITY.md](SECURITY.md)（安全）· [API.md](API.md)（HTTP 契约）·
> [AGENT-INTEGRATION.md](AGENT-INTEGRATION.md)（Agent 集成）

本文档面向日常值班：看什么日志、盯哪些计数器、P0 如何接、outbox 积压与幂等/租约参数怎么调、GC 与 quarantine 如何管理。发布专项处置（迁移、回滚、stale stage）见 [PUBLICATION-RUNBOOK.md](PUBLICATION-RUNBOOK.md)。

## 1. 日志

新增 Facade 与 Publication 沿用现有 JSON Logger（规范 §37.1）。结构化字段定义在 `internal/observe/observe.go:117-135`（零值省略）：

```text
request_id / actor / user_id / agent_session
template_slug / template_version
content_id / content_version / publication_id
full_path / storage_provider / stage
duration_ms / status_code / error_code
```

日志入口（`observe.go:186-196`）：`LogPublication("..."）`（生命周期事件：收到请求、stage/activation 成功或失败，含 plan freeze 起算的 duration）与 `LogGeneration(...)`（page-generation 请求结果，含限流拒绝）。rollback 单独记 `rollback` / `rollback_failed`（`internal/product/httpapi/publications.go:119-129`）。

日常 grep 姿势：

- 按页追踪：`full_path="<path>"` 或 `publication_id="<id>"`；
- 按模板：`template_slug="<slug>" template_version=<n>`；
- 失败聚合：`error_code="<CODE>"`（如 `TEMPLATE_VERSION_CHANGED`、`PUBLICATION_CONFLICT`，契约见 [API.md](API.md)）；
- P0：`[P0-ALERT]` 前缀（进程日志可 grep，`observe.go:7-9`；scanner 回调格式见 `cmd/server/publication_runtime.go:138-141`）。

注意 `duration_ms` 恒输出（含 0 值，`observe.go:176`），聚合 P95（目标见规范 §38：Draft Create < 1s、Preview < 1.5s、Publish < 3s，不含远程 asset 下载）时不要把限流拒绝（`Retry-After` 60s 的 429）混入成功样本。

## 2. 计数器

计数器名即规范 §37.2 指标名，常量在 `internal/observe/observe.go:21-34`，`Snapshot()` 按指标名返回（`observe.go:82-97`）：

| 计数器 | 含义 | 盯法 |
|---|---|---|
| `page_generation_requests_total` | page-generation 请求数 | 分母 |
| `page_generation_errors_total` | page-generation 错误数 | 错误率分子之一 |
| `publication_stage_failed_total` | stage/verify/cutover/commit 失败（saga 内计数） | 突增 → 查磁盘/Mongo/渲染 |
| `publication_activate_failed_total` | 激活失败 | 同上 |
| `idempotency_replay_total` | 同 key 同 body 重放缓存响应 | 高是好事（客户端重试生效） |
| `idempotency_conflict_total` | 同 key 不同 body 409 | 突增 → 客户端 key 复用 bug |
| `outbox_delivered_total` / `outbox_failed_total` | outbox 投递成功/失败（worker 每次 poll 更新 backlog gauge，`internal/product/publication/outbox.go:250-254`） | 失败见 §3 |
| `outbox_backlog` | pending + delivering 行数（`outbox.go:268-272` 定义：due 或 scheduled 的未投递行） | 持续非零见 §3 |
| `scanner_repairs_total` / `scanner_alerts_total` | scanner 修复/告警数 | 修复数长期非零 → 有慢性病 |

（规范 §37.2 另列 `page_generation_duration_seconds`、`publication_stage_total`、`publication_activate_total`、`publication_failed_total`、`publication_duration_seconds`、`publication_rollback_total`、`static_store_operations_total`、`static_store_errors_total`、`asset_import_total`、`asset_import_failed_total` 等直方图/明细；在当前进程内实现中以结构化日志字段 + 上表计数器承载，Dashboard 按日志聚合补齐对应视图，不要假设存在 Prometheus exposition 端点。）

## 3. P0 告警

P0 出口统一为进程日志 `[P0-ALERT]` + 一条结构化 error 行（`observe.go:198-204` `AlertP0`）。规范 §37.3 告警项与当前实现的对应关系：

- **Publication failure > 5% / 5min**：进程内 5 个分钟桶实现（`observe.go:206-249`）：最近 5 分钟样本 ≥20 且失败率 >5% 即 paging（每次观察调用评估一次，持续失败会重复 paging，这是故意的）。样本 <20 时不告警——低流量时段看绝对失败数，不要等 P0。
- **static store error / active pointer 不一致 / Mongo 不可用**：由 scanner P0（`RECOVERY_IMMUTABLE_MISSING` / `RECOVERY_IMMUTABLE_CORRUPT` / `RECOVERY_MULTIPLE_ACTIVE` / `RECOVERY_INSPECT_FAILED`）与 saga 内 `PUBLICATION_UNPUBLISH_STAGE_FAILED`（unpublish rename 失败，规范 §18.1）覆盖；unpublish 的 DB 失败 + backup 恢复失败为 P0（`internal/product/publication/service.go:297` 附近 "P0: scanner repair required"）。
- **webhook retry backlog（outbox 积压）**：看 `outbox_backlog` gauge + `RECOVERY_OUTBOX_POISONED`（毒化即 P0/行）。CDN purge 失败不恢复上线状态、进 retry（规范 §18.1），同样反映在 backlog。
- **API 5xx spike**：`page_generation_errors_total` + `publication_*_failed_total` 突增；结合 `status_code` 日志定位。
- **staged Publication 长时间未清理**：`ScanReport.StaleStagedFailed` 持续非零（处置见 [PUBLICATION-RUNBOOK.md](PUBLICATION-RUNBOOK.md) §4）。

值班接警后先看 `ScanReport` 全量字段（`recovery.go:139-163`），区分 P0（立即处理）与 WARN（预期修复，复核）。各 code 处置见 [PUBLICATION-RUNBOOK.md](PUBLICATION-RUNBOOK.md) §3。

## 4. Outbox 积压处理

Outbox worker 默认值（`internal/product/publication/outbox.go:169-234`；生产 wiring 用零值即默认，`publication_runtime.go:143-146` 未覆盖）：

```text
PollInterval   5s（无到期行时空闲睡眠；有积压则不睡连续排空，outbox.go:240-249）
Lease          5m（claim 租约；超期可被另一 replica 接管，crash takeover）
Backoff       attempt1 → 30s；attempt2 → 5m；attempt3 → 30m；之后 1h 封顶
              （对齐既有 WebhookService 重试节奏，outbox.go:195-209）
```

投递语义：at-least-once；每事件带稳定 event ID（`X-LightCMS-Event-ID` 头 + HMAC 签名，`outbox.go:161-165`），接收端按 event ID 去重（见 [AGENT-INTEGRATION.md](AGENT-INTEGRATION.md)）。同事务内插入唯一 outbox 行，重试不产生第二行（`repository.go:33-34`）。

积压处理流程：

1. 看 `outbox_backlog`：短暂非零正常（poll 排水）；**单调增长或数小时不回零** → 查 `outbox_failed_total` 与接收端状态（5xx？超时？DNS？WARP 代理？见 [DEPLOYMENT-PRODUCTION.md](DEPLOYMENT-PRODUCTION.md) §1）。
2. 投递失败自动按 Backoff 重排（`next_attempt_at` 将来时间），行状态保持 pending——这是预期，不要手工改行状态“催单”。
3. 毒化线（`internal/product/publication/gc.go:49-57`）：pending 行达 **10 次**（`DefaultOutboxMaxAttempts`）或 **超 7 天**（`DefaultOutboxMaxAge`）→ scanner 转 terminal failed + P0/行（`gc.go:139-154`）；delivering 中的行归 worker 租约接管路径，**不被毒化**（`gc.go:318-322`）。terminal 行 90 天后删除（message TTL，`gc.go:55-57`）。
4. 毒化后：先修接收端，再与下游按稳定 event ID 对账、手工补发，确认后才清理 failed 行。禁止“删行了事”——at-least-once 下删行 = 丢事件。

## 5. Idempotency TTL / Lease 调参

默认值（`internal/product/idempotency/model.go:34-46`；生产值来自 config，`publication_runtime.go:74-76`）：

```text
TTL      24h（允许 1–72h，越界启动拒绝）
Lease    5m（必须 ≥1 分钟）
Heartbeat 60s（worker 心跳 CAS 续租，lease 全长延续）
```

语义速览（`internal/product/idempotency/service.go:64-78`）：无记录 → 建 attempt 1；同 key 换 body → 409 `IDEMPOTENCY_CONFLICT` 零副作用；completed 同 body → 重放缓存响应；processing + 活租约 → 409 `REQUEST_IN_PROGRESS`；processing + 过期租约 → `IDEMPOTENCY_LEASE_EXPIRED`（调用方可 TakeOver 同 attempt）；terminal（pre-activation 失败）+ 同 body → CAS attempt++ 并分配**新 Publication ID**。

调参指南：

- **TTL**：覆盖客户端最大重试窗口即可。24h 默认适配“当天重试”；拉到 72h 上限会等比放大 `idempotency_records` 集合（TTL 索引自动过期）与重放缓存命中。低于 1h 会导致正常重试变新 attempt（重复建 Publication），除非写入量极大否则不要调小。
- **Lease**：必须大于单次 publish 最长耗时（含 render + stage + verify + cutover）。默认 5m；超大页面/慢盘导致频繁 `IDEMPOTENCY_LEASE_EXPIRED` 时，先优化耗时（见规范 §38 性能目标），再考虑放宽到 10m。Lease 过大则 crash 后接管等待变长（接管只能等租约过期）。
- **心跳 60s** 是代码常量（`model.go:44-46`），不可配置；长任务 worker 必须保持心跳，否则同 attempt 被 TakeOver 后出现双写竞争（CAS 会挡住，但会产生困惑的 409）。
- 同 key 在 terminal CREATE 失败后**字节相同重试不可达**（upsert 门与幂等冲突的已知夹角，E2E 已 pin）：换新 key 重试（见 [API.md](API.md) 幂等节）。

## 6. GC 与 quarantine 管理

保留默认值（`gc.go:39-58`，生产值来自 `PUBLICATION_*` 环境变量，见 [DEPLOYMENT-PRODUCTION.md](DEPLOYMENT-PRODUCTION.md) §2）：superseded/unpublished 90 天、failed/staged 7 天、quarantine 30 天（按文件 modtime）、outbox terminal 90 天。`ScannerOptions` 零值回退到同样默认值（`recovery.go:165-197`），所以留空 env = 规范值。

日常事项：

- GC 是 scanner 每周期的一个 phase（`ScanOnce` 内运行，`gc.go:17-18`）；看 `GCReport`（`gc.go:62-71`）：`ObjectsDeleted`（删 object + 翻 `storage_state=deleted`）、`ObjectDeleteErrors`（存删失败，metadata 不动、下周期重试）、`QuarantineFilesDeleted`（过期隔离文件）。
- `ObjectDeleteErrors` 持续非零 → 查 volume 权限/只读/满盘；修好后自动追赶，无需手工补删。
- quarantine 目录（默认 filesystem 根下 `quarantine/`）只删过期文件（递归走子目录、只删 regular file，`gc.go:268-302`）；**不要手工清空**——30 天是取证窗口（误删 canonical 的唯一找回可能是这里 + 备份）。
- `pinned=true` 的对象永不 GC（`gc.go:210-213`）。pin 用于延长 Exact Rollback SLA（`legacy_unverified` 接受现状分支等），定期评审 pinned 清单，防止 volume 无限增长；Fly.io volume 初始仅 1GB（`fly.toml:41-44`），先扩容、后 pin。
- metadata 行永久保留（删 object 只翻 `storage_state=deleted`，`gc.go:247-257`），审计链不断；不要手工删 `content_publications` 行——`MultipleActive` 处置也是改状态而非删行（[PUBLICATION-RUNBOOK.md](PUBLICATION-RUNBOOK.md) §3）。
