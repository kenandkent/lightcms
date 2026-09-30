# 发布运维手册（Publication Runbook）

> 配套文档：[DEPLOYMENT-PRODUCTION.md](DEPLOYMENT-PRODUCTION.md)（生产部署）·
> [OPERATIONS.md](OPERATIONS.md)（日常运维）·
> [BACKUP-RESTORE.md](BACKUP-RESTORE.md)（备份恢复）·
> [UPGRADE.md](UPGRADE.md)（升级）· [API.md](API.md)（HTTP 契约）·
> [AGENT-INTEGRATION.md](AGENT-INTEGRATION.md)（Agent 集成）·
> [TEMPLATE-GUIDE.md](TEMPLATE-GUIDE.md)（模板指南）

本手册面向值班与发布管理员，覆盖迁移、verified/legacy 处置、scanner 告警、stale stage 清理、发布回滚与存储保留。日常指标与 outbox 积压见 [OPERATIONS.md](OPERATIONS.md)。

权威顺序：Mongo active Publication → immutable object → canonical 投影（可重建）。**文件永远不能选举 active 记录**（`internal/product/publication/recovery.go:4-7`）。所有修复先记审计、再动手；先备份再删文件。

## 1. Migration dry-run / apply

命令（同一 server 二进制内，无第二二进制，`cmd/server/publication_runtime.go:206-210`）：

```bash
lightcms migrate-publications --dry-run   # 只读，JSON 报告打 stdout，exit 0
lightcms migrate-publications --apply     # 执行；未完成则 exit 2 并打印报告
```

规则（`publication_runtime.go:211-254`）：`--dry-run` 与 `--apply` 恰选其一；未知 flag、两者同给/同缺 → exit 2。`--apply` 结束时 `completed=false` → exit 2（"migration: not completed — resolve the reported items and re-run"）。

报告字段（`internal/product/migration/report.go:8-61`）：`dry_run`、`migration_from`/`migration_to`（`""`/`not_started`/`running`/`completed`）、`completed`、阻塞类（`invalid_slugs`、`canonical_collisions`、`version_duplicates`、`invalid_paths`、`missing_files`、`render_errors`，`BlockingCount` 见 `report.go:99-106`）、候选类（`verified`、`legacy_unverified`、`skipped_active`）、索引证据（`new_indexes_verified`、`legacy_index_dropped`，后者仅在前者为真后才置位）。

操作流程：

1. `--dry-run` → 修阻塞项 → 再 `--dry-run`，直到 `HasBlocking()==false`（`report.go:108`）。
2. `--apply` → 确认 `completed=true` 且 `migration_to=completed`。
3. 中断续跑安全：已有 active 的页面进入 `skipped_active`，不建重复 active。

迁移 flag 门控（`recovery.go:48-64`）：`system_migrations.publication_model_v1` 为 `running`（或缺失）时，scanner 对"canonical 存在但无 active"**只报告（`RECOVERY_ORPHAN_REPORTED` WARN 级），不隔离**；仅 `completed` 后才允许 orphan quarantine。`running` 未完成时不要手动删 canonical 文件——scanner 不会自动下线这些页面（规范 §35.3 规则 6），无 active 且迁移失败的页面会阻止上线 Gate，必须人工处理。

## 2. verified / legacy_unverified 处理

对账矩阵（规范 §35.3，`migration/report.go:43-50`）：

| 结果 | 行为 |
|---|---|
| hash 相同 | 创建 verified active Publication |
| canonical 不存在 | **不创建 active**，记 `missing_files` 阻塞项 |
| hash 不同 | 旧 canonical 字节拷贝为 immutable legacy object，创建 `verification_status=legacy_unverified` 的 active Publication，canonical 继续服务 |
| render 失败 | 保留旧 canonical 服务，记 `render_errors` 阻塞项 |

`legacy_unverified` 页面三选一（规范 §35.3，**禁止静默标 verified**，**禁止自动 template upgrade**）：

1. 接受当前重渲染结果，发布新 verified Publication（旧 legacy object 按 retention 保留，可分阶段切）；
2. 人工确认已导入的 legacy immutable object，保持 `legacy_unverified` 并 `pin`（pinned 对象永不 GC，精确回滚期延长，见 §6）；
3. 下线页面（走正常 Unpublish 流程）。

`missing_files` 页面：先恢复 canonical 文件（从备份或重渲染），再重跑 `--apply`；不要手工插 active 记录。`render_errors` 页面：修模板/数据后重跑；旧 canonical 在此期间继续服务是预期行为。

## 3. Scanner 告警处置

Scanner 每个周期输出 `ScanReport`（`recovery.go:139-163`）并经 `Alert` 回调打进程日志：`[P0-ALERT] scanner severity=... code=... path=... msg=...`（`publication_runtime.go:138-141`）。生产接警方式就是 grep 该前缀（`internal/observe/observe.go:7-9`），P0 同时附一条结构化 jsonlog error 行。

P0（立即人工介入，`recovery.go:66-101`）：

| code | 含义 | 处置 |
|---|---|---|
| `RECOVERY_MULTIPLE_ACTIVE` | 同一 content 多条 active，自动修复已停止 | 查 `content_publications` 同 content 多 active 行，确认哪条是真 active（发布时间、hash、canonical 对照），手工把多余行置 superseded；不要删 metadata 行。修完等下一周期自愈验证 |
| `RECOVERY_IMMUTABLE_MISSING` | active 无 immutable object 可重建，不猜测，站点标 degraded | 从备份恢复 immutable 对象（[BACKUP-RESTORE.md](BACKUP-RESTORE.md)）；备份也没有则按“canonical 还在”走 mismatch 重建（scanner 自动），canonical 也不在则该页下线并公告 |
| `RECOVERY_IMMUTABLE_CORRUPT` | immutable 对象存在但 hash 不符 | 同上：以备份为准；备份恢复前不要删 canonical（当前服务字节可能是唯一正确副本，先拷贝留存） |
| `RECOVERY_INSPECT_FAILED` | Inspect 出错（如 sidecar 不可读），该 content 被跳过、**不视为干净** | 修文件权限/磁盘，确认 `InspectErrors` 回零；跳过期间该页不在保护之下，优先处理 |
| `RECOVERY_OUTBOX_POISONED` | outbox 行达投递上限/超期，转 terminal failed，不再重试 | 见 [OPERATIONS.md](OPERATIONS.md) outbox 节：查接收端、手工补发，确认下游已对账后再清行 |

WARN（预期内修复，复核即可）：`RECOVERY_CANONICAL_MISMATCH`（坏 canonical 已先 quarantine 再用 immutable 重建——顺序是故意的，因为 Restore 会覆盖字节不留证，`recovery.go:13-14`）、`RECOVERY_ORPHAN_QUARANTINED`（completed 后孤儿已隔离）、`RECOVERY_ORPHAN_REPORTED`（迁移中仅报告）、`RECOVERY_REPAIR_ERROR`（瞬态 IO/CAS 竞争，下周期重试；持续增长才介入）。

处置 scanner  quarantined 文件：quarantine 目录默认在 filesystem 根下 `quarantine/`（`recovery.go:188-190`）。核对 `ScanReport.Quarantined`（原路径 → 隔离路径 → 原因 → 时间，`recovery.go:129-135`）后，误删可从 quarantine 拷回；确认无用则等 30 天自动过期（见 §6），不要手动 `rm -rf` 整个目录。

Crash 恢复（Task 19 必演练）：进程在 Publish/Unpublish cutover 中崩溃后重启，同二进制 scanner 会修复 canonical 文件并投递 pending outbox 事件。重启后检查 `PreviousRestored` / `BackupRestored` / `CanonicalRebuilt` 计数与 P0 是否清零；`ImmutableMissingP0` 非零则按上表处理。

## 4. Stale stage 清理

Stale 判定：staged 对象或孤儿 `.next` 文件超过 `PUBLICATION_STAGE_TIMEOUT_MINUTES`（默认 15 分钟，`config/config.go:50,84-85`；store 层常量 `internal/product/storage/store.go:33-40`，`DefaultStageTimeoutMinutes = 15`）即过期。scanner 自动清理：stale `.next` → 删除（`recovery.stale_next_cleaned`），stale staged publication → 标 failed（`recovery.stale_staged_failed`），对应 `ScanReport.StaleNextCleaned` / `StaleStagedFailed`（`recovery.go:152-154`）。

人工介入时机：

- `StaleStagedFailed` 持续增长：说明 publish 卡在 stage（磁盘满、权限、Mongo 事务失败），查进程日志 `publication.* stage=... error_code=...`（[OPERATIONS.md](OPERATIONS.md)），修根因而非调大超时。
- 同一 key 重试被 `409 REQUEST_IN_PROGRESS` 挡住：是 idempotency 租约未过期（默认 5 分钟），等租约过期后同 key 同 body 可 TakeOver（见 [API.md](API.md)）；**不要删 `idempotency_records` 行来“解锁”**，会破坏去重语义。
- 超时调参：stage 耗时长期接近 15 分钟（如超大页面渲染），可上调 `PUBLICATION_STAGE_TIMEOUT_MINUTES`；这是 fishermen 式的最后手段，先优化渲染/磁盘（见 [OPERATIONS.md](OPERATIONS.md) 调参节）。

## 5. Rollback（发布回滚）操作

两级回滚（规范 §18.5，`internal/product/generation/restore.go:1-7`）：

```text
Exact Rollback（精确回滚）
    = immutable publication object 仍保留
    = 恢复历史字节完全一致的 HTML
    = 接口：POST /api/v1/content/{id}/publications/{publication_id}/rollback
      （Handler：internal/product/httpapi/publications.go:72-73）
      Admin：POST /cm/.../content/{id}/publications/{publicationID}/revert_live
      （cmd/server/main.go:413）

Re-render Rollback（重渲染回滚）
    = immutable object 已被 GC
    = 历史 Content Version + 准确 Template Version + 兼容 renderer/依赖快照
    = 不承诺字节完全一致
    = 接口：POST /api/v1/content/{id}/restore-and-publish（restore.go:41）
```

操作：

1. 在 Publication 历史中找到目标版本，确认其 immutable 对象仍在（`storage_state != deleted`）。默认 Exact Rollback SLA 为 superseded 后 **90 天**；需更长保证的 Publication 必须 `pinned=true`（pinned 永不 GC，见 §6）。
2. 调用 rollback（需 `content.edit + content.publish` 权限与 `Idempotency-Key`，`restore.go:47,51`），传 `expectedActiveID` 做 CAS（防止回滚到过期 active 上）。
3. 回滚创建的是**新 Publication**（引用历史 Content/Template Version），从不复用/篡改旧 Publication 记录（规范 §18.4）。
4. CDN purge 与 webhook 为异步 best-effort：失败不回滚已提交的状态，进入 outbox 重试（规范 §18.1）；用 `publication_rollback_total` 与 outbox backlog 确认最终一致（[OPERATIONS.md](OPERATIONS.md)）。

Unpublish 是独立操作（page lock 与 Publish 共用；先 atomic rename canonical 为 `.unpublish-backup-{publication_id}` 再提交 Mongo 事务，任一失败按 Saga 补偿，规范 §18.1）。unpublish 成功条件 = canonical 已不可公开访问 **且** 事务已提交；immutable object 的物理删除交给 retention/GC，不要手工删。

部署回滚（整个二进制回退，规范 §35.4）：schema 是 additive change——保留新 collections、不删历史 metadata、恢复旧 handler、static canonical path 兼容。禁止在回滚时“清理”新表。

## 6. 存储 retention

保留策略（规范 §17.6，`internal/product/publication/gc.go:37-58`；生产值由 config 注入，scanner 零值回退到同文件默认值）：

```text
active                    永不自动删除
pinned superseded         永不自动删除
pinned unpublished        永不自动删除
superseded                默认保留 90 天（PUBLICATION_RETENTION_DAYS）
unpublished               默认保留 90 天（同上）
failed / staged           默认保留 7 天（PUBLICATION_FAILED_RETENTION_DAYS）
quarantine 隔离文件       默认保留 30 天（按文件 modtime，PUBLICATION_QUARANTINE_RETENTION_DAYS）
Publication metadata      永久保留；object 删除后 storage_state=deleted
outbox terminal 行        90 天后删除（message TTL，gc.go:55-57）
outbox pending 毒化       达 10 次（DefaultOutboxMaxAttempts）或超 7 天（DefaultOutboxMaxAge）转 failed + P0
```

GC 铁律（`gc.go:4-9`）：**先删 storage object，成功后再翻 metadata**；失败记 `ObjectDeleteErrors` 重试，绝不先删审计记录。active 与 pinned 对象跳过（`gc.go:210-213`）。superseded/unpublished 计时用 `superseded_at`/`unpublished_at`（缺失回退 `created_at`）；failed/staged 用 `created_at`；quarantine 用文件 modtime（`gc.go:192-302`）。

需要延长精确回滚期的页面：置 `pinned=true`（如 `legacy_unverified` 接受现状的分支）。pin 是人工决策，不设自动 pin；定期评审 pinned 清单，避免 volume 被无限期占用。
