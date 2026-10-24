# LightCMS V3 备份与恢复（Task 18C）

> 关联文档：本地联调部署见 `docs/DEPLOYMENT-LOCAL.md`；
> 发布操作见 `docs/PUBLICATION-RUNBOOK.md`；日常运维见 `docs/OPERATIONS.md`；
> 模板升级与 restore/revert 选择见 `docs/UPGRADE.md`。
> 本文件命令均在 Task 18C 联调环境实测通过（`mongo:7.0.14` 容器内
> `mongodump/restore 100.10.0`，见 §5）。

## 1. 先分清三类东西

| 类别 | 位置 | 性质 | 能否直接改 |
|---|---|---|---|
| 不可变对象（immutable） | `content/publications/{contentID}/{publicationID}/index.html` | 一次写入、SHA-256 校验、**永不原地修改**；GC 只删过期且非 active/非 pinned 的 | 否。只能经发布链路新建 |
| canonical 文件（投影） | `content/generated/{路径}.html`（`/` → `index.html`） | 实际被 serve 的字节；是 active Publication 的投影，可由不可变对象**重建** | 否。只能经 saga 切流/恢复扫描重建 |
| Mongo 记录 | `content`、`content_versions`、`templates`、`template_versions`、`content_publications`、`webhook_outbox`、`template_upgrade_jobs`、`system_migrations` 等 | 控制面真相（active 指针、版本链、outbox 事件、迁移旗标） | 仅经 API/迁移程序；联调修数据除外 |

权威顺序（Task 10 恢复扫描同款）：**Mongo active 记录 → 不可变对象 → canonical 投影**。
文件永远选不出 active；canonical 丢了能重建，active 记录丢了必须从备份恢复。

切流 sidecar（同目录，与 canonical 同名后缀）：`.next-{新ID}`（在途）、
`.previous-{旧ID}`（提交前保留的老投影）、`.unpublish-backup-{ID}`（unpublish 在途）。
备份时把它们一起打包，恢复扫描会自行收敛；保留 30 天的隔离区
`content/quarantine/` 也建议一起打包（误删找回的最后机会）。

## 2. Mongo 备份与恢复（实测）

`mongodump`/`mongorestore` 在 `mongo:7.0.14` 镜像里自带，用 `docker exec` 调用：

```bash
# 备份（全库单文件归档；<container> 如 lightcms-mongo-test）
docker exec <container> mongodump --quiet \
  --uri 'mongodb://127.0.0.1:27017/lightcms?replicaSet=rs0&directConnection=true' \
  --archive=/tmp/lightcms-backup.archive
docker exec <container> ls -la /tmp/lightcms-backup.archive
```

```bash
# 恢复（覆盖式；先停服，见 §4）
docker exec <container> mongorestore --quiet \
  --uri 'mongodb://127.0.0.1:27017/lightcms?replicaSet=rs0&directConnection=true' \
  --archive=/tmp/lightcms-backup.archive
```

实测闭环（隔离栈，库 `lightcms`）：备份归档 21658 bytes；
`dropDatabase` 后 `system_migrations` count 1→0；
`mongorestore` 后回到 1，且旗标文档原样回来
（`_id=publication_model_v1`，`status=completed`，`completed=true`，
`new_indexes_verified/legacy_index_dropped=true`）。**恢复不重跑迁移、不重建索引**，
因为索引定义与旗标都在备份里。

要点：

- 用 `--archive` 单文件 + 事务库：dump/restore 走同一 replicaSet URI（含 `replicaSet=rs0`）。
- 全库归档是推荐姿势：`content_publications`（active 指针）、`webhook_outbox`
  （未投递事件，重启后 outbox worker 会续投）、`system_migrations`（迁移旗标，
  决定扫描仪是否允许隔离孤儿文件）缺一不可。**不要只备 `content` 表。**
- 备份文件拷出容器：`docker cp <container>:/tmp/lightcms-backup.archive ./`。
- 恢复后必查：`system_migrations.publication_model_v1.status` 应为 `completed`；
  若回到 `running`/缺失，扫描仪只会上报孤儿文件、不隔离，同时迁移必须重跑到完成。

## 3. 文件备份与恢复（实测）

```bash
tar -czf lightcms-content-backup.tgz content
tar -tzf lightcms-content-backup.tgz   # 验包
```

实测：包内含 `content/publications/`、`content/generated/`、`content/quarantine/`
三棵树。恢复就是解包覆盖，然后启动 server——启动扫描会自动：
active 在但 canonical 缺失/不一致 → 用不可变对象重建；
有 `.previous-*` 残留且 DB 仍是旧 active → 恢复老投影。
若 active 记录的不可变对象也丢了，扫描报 P0（`RECOVERY_IMMUTABLE_MISSING`），
此时只能按 §4 从双备份重建，不许“猜”一个字节顶上去。

## 4. 标准恢复流程（灾难场景）

1. 停服（停掉所有 server 副本；outbox/scanner 与 server 同进程，停服即停）。
2. `mongorestore` 恢复 Mongo（§2）。
3. 解包恢复 `content/`（§3）。
4. 启动一个 server，观察启动扫描日志：`canonical_rebuilt` / `previous_restored`
   为正常自愈；`RECOVERY_IMMUTABLE_MISSING` / `RECOVERY_MULTIPLE_ACTIVE` 必须人工介入。
5. 抽查公网 URL（对比备份前的 publication 清单）。
6. outbox 未投递事件会自动续投；下游用稳定 event ID 去重（见 `docs/API.md`）。

## 5. Publication 回滚 vs 备份恢复：区别与操作

这是两回事，选错会丢审计链：

| | 回滚（rollback / revert / restore_and_publish） | 备份恢复（restore） |
|---|---|---|
| 本质 | **新建一个 Publication**，旧记录全部保留，生命周期照常流转（rollback 生成的新记录也可再被回滚） | 用备份**覆盖**回旧状态，备份点之后的数据/发布都丢失 |
| 数据来源 | 站内保留：源 Publication 的不可变字节（精确）或历史 ContentVersion 重渲染 | 备份归档（Mongo + `content/` 双份） |
| 适用 | 发错版、模板升错级、内容事故——日常操作 | 删库、磁盘丢失、不可逆污染——灾难操作 |
| 入口 | REST：`POST /api/v1/content/{id}/publications/{pid}/rollback`、`.../revert-live`、`.../restore-and-publish`；Admin 同名按钮。三者如何选见 `docs/UPGRADE.md` §3 | 本文件 §2–§4；三者**不能**替代备份恢复 |
| 幂等 | 外部调用必须带 `Idempotency-Key`（缺则 428 零写入）；同 key 重放返回同一新 Publication | `mongorestore` 可重跑；`--drop` 语义自行确认 |

回滚的 CAS 语义：可带 `expected_active_id`，与别人并发时 stale 方拿
`PUBLICATION_CONFLICT` 且双方记录都不变——先查
`GET /api/v1/content/{id}/publications` 再动手。

## 6. 实测命令与结果总表（本 worktree，2026-09-30）

| # | 命令（容器内 `mongodump/restore 100.10.0`） | 结果 |
|---|---|---|
| 1 | `mongodump --uri ... --archive=/tmp/lightcms-18c-backup.archive` | 21658 bytes，`DUMP_OK` |
| 2 | `dropDatabase` 后 `system_migrations.countDocuments()` | 1 → 0 |
| 3 | `mongorestore --uri ... --archive=...` | `RESTORE_OK`，count 回到 1，旗标 `completed` 原样 |
| 4 | `tar -czf content` / `tar -tzf` | 三棵树完整，`FS_BACKUP_OK` |
| 5 | 同 `Idempotency-Key` 重发 publish | 同一 `publication_id`，无新 Publication（回滚类操作同理，见 `docs/UPGRADE.md`） |
