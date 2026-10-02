# LightCMS V3 生产部署指南

> 配套文档：[README-product.md](README-product.md)（产品总览）·
> [PUBLICATION-RUNBOOK.md](PUBLICATION-RUNBOOK.md)（发布运维）·
> [OPERATIONS.md](OPERATIONS.md)（日常运维）·
> [SECURITY.md](SECURITY.md)（安全）·
> [UPSTREAM-SYNC.md](UPSTREAM-SYNC.md)（上游同步）·
> [API.md](API.md)（HTTP 契约）· [USER-MANUAL.md](USER-MANUAL.md)（用户手册）·
> [DEPLOYMENT-LOCAL.md](DEPLOYMENT-LOCAL.md)（本地部署）·
> [BACKUP-RESTORE.md](BACKUP-RESTORE.md)（备份恢复）·
> [UPGRADE.md](UPGRADE.md)（升级）·
> [AGENT-INTEGRATION.md](AGENT-INTEGRATION.md)（Agent 集成）·
> [TEMPLATE-GUIDE.md](TEMPLATE-GUIDE.md)（模板指南）

本文档描述当前 Fly.io 生产目标的部署方式。V3 发布系统是**单二进制、单进程**拓扑：除 MongoDB 与 filesystem 存储外，不需要任何二级服务（见 Task 19 验收口径：one backend executable, one Admin UI, no required secondary services beyond MongoDB and configured static storage）。

## 1. 生产拓扑

```text
                HTTPS
                  │
            ┌─────▼──────┐
            │  Fly.io    │  app = metavert-cms（fly.toml:1）
            │  1× machine │  region ord，1 vCPU / 1GB（fly.toml:36-40）
            │  单进程     │
            │  ./lightcms │  Dockerfile:18 由 cmd/server 构建，start.sh 启动
            └─┬────────┬─┘
              │        │
   ┌──────────▼──┐  ┌──▼──────────────────────────┐
   │ MongoDB     │  │ Filesystem 存储（volume）     │
   │ Replica Set │  │ lightcms_data → /app/content │
   │ （必需）     │  │ （fly.toml:41-44）            │
   └─────────────┘  └─────────────────────────────┘
```

单进程内装配的运行时（`cmd/server/publication_runtime.go:64` `buildPublicationRuntime`，`main.go` 仅调用一次）：

- filesystem immutable store（根目录 `"content"`，见 `publication_runtime.go:72`）
- publication saga（`publication.Service`）
- idempotency service
- public URL resolver
- generation orchestrator（`generation.Service` + 发布限流器）
- product HTTP handlers
- durable outbox worker（webhook 投递）与 recovery/GC scanner

两个后台 worker 与主服务**同进程**运行，随 server context 取消而停止（`cmd/server/main.go:469-470`：`go rt.Outbox.Run(bgCtx)` / `go rt.Scanner.Run(bgCtx)`）。没有独立的 worker 进程、队列服务或 cron 容器。

同一站点数据库当前只允许运行一个应用进程。Mongo 唯一 writer lease slot 在启动时原子认领；心跳失效、owner/incarnation 不匹配或独立 watchdog 到期，应用直接停止。部署更新必须先停旧进程再启新进程，不要开启两个副本的重叠滚动更新。该限制不是多实例分布式锁的替代实现，Scale-out 仍是后续工作。

发布时完整渲染输入快照（含 Snippet/Wikilink/query 依赖）限制为 JSON 编码后 8 MiB。超过限制的请求在文件发布前失败；管理员需缩小输入/依赖范围，不能靠重试绕过。同一幂等 attempt 的恢复使用原快照，不受依赖后续修改影响。

当前 Fly.io 现状（按仓库现状如实记录，见 `CLAUDE.md` Deploy 章节、`fly.toml`、`Dockerfile`、`start.sh`）：

- 应用名 `metavert-cms`，实际运行的是一台 legacy 非 Launch machine（`d890122a371528`）。`fly deploy` 看不到它，会另建 stuck orphan machine，因此**部署必须使用 `./deploy.sh`**（构建镜像 → 提取 image ref → 删除 stuck 新机 → `fly machines update` 直接更新运行机）。machine ID 硬编码在 `deploy.sh` 中。
- 健康检查：`GET /health`（`fly.toml:26-30`，10s 间隔 / 5s 超时 / 10s grace）。
- volume `lightcms_data`（初始 1GB）挂载到 `/app/content`；filesystem store 根目录 `"content"` 即该 volume 下的相对路径。volume 容量与 immutable 对象保留策略（§17.6，默认 superseded 90 天）共同决定磁盘水位，见 [OPERATIONS.md](OPERATIONS.md)。
- 镜像构建时通过 `--build-arg GIT_SHA=$(git rev-parse --short HEAD)` 把 git SHA 注入 `main.ProductBuildSHA`（`Dockerfile:14-18`），写入每条 Publication 记录（`publication_runtime.go:98` `BuildSHA`）；未注入时回退到 build 版本字符串。**发版时务必传递该参数**，否则 Exact Rollback 审计链断裂（见 [PUBLICATION-RUNBOOK.md](PUBLICATION-RUNBOOK.md) 回滚节）。
- 运行时先起 Cloudflare WARP（proxy 模式，`start.sh`），再 `exec ./lightcms`。WARP 启动失败不阻塞主进程（脚本中均为 `|| true`），但出站 webhook 走代理，排障时注意区分。
- `fly deploy` 上传约 185MB 构建上下文（`content/` + `static/`），无进度显示，约 8 分钟，**不是 hung**。
- `fly secrets set` 不会重启 legacy machine（它在 Fly release 管理之外）；secret 处于 staged 状态，直到执行 `fly machines update d890122a371528 -a metavert-cms --yes` 或下一次 `./deploy.sh`。注意：普通 `fly machines restart` 复用旧 machine 配置，**不会**注入新 secret，修改后用 `fly ssh console -C 'sh -c env'` 验证。

## 2. 全部环境变量表

配置加载优先级：环境变量 > `config.prod.json` > `config.dev.json`（`config/config.go:131-132`）；只要设置了 `MONGO_URI` 即走环境变量路径（`config.go:135`）。Fly.io 生产走环境变量路径。`LIGHTCMS_CONFIG_DIR` 可指定 JSON 配置目录（`config.go:144-147`）。

零值语义：V3 发布类变量未设置时保持零值，随后 `ApplyPublicationDefaults` 填入规范默认值（`config.go:55-86`，`loadFromEnv` 显式注释见 `config.go:208-220`）。因此**留空 = 默认值**，不是报错（除必填项外）。

| 环境变量 | JSON 键 | 默认值 | 必填 | 说明 |
|---|---|---|---|---|
| `MONGO_URI` | `mongo_uri` | 无 | **是** | MongoDB 连接串；缺失则启动失败（`config.go:176-178`） |
| `SESSION_SECRET` | `session_secret` | 无 | **是** | ≥32 字符 session 加密密钥；缺失则启动失败（`config.go:180-183`） |
| `BASE_URL` | `base_url` | `https://lightcms.fly.dev`（仅 env 路径） | 否 | 站点公开 URL；生产必须 HTTPS（规范 §19.3） |
| `PORT` | `port` | `80`（prod 默认；dev `8082`） | 否 | 监听端口；`fly.toml` 内外均用 `8082`，以 env/文件覆盖关系为准 |
| `ENV` | `env` | `production`（prod 默认） | 否 | `production`/`development`；决定 standalone Mongo 是拒绝还是警告（见 §3） |
| `SECURE_COOKIES` | `secure_cookies` | `true`（prod 默认；仅显式 `"false"` 关闭，`config.go:199-201`） | 否 | 生产 HTTPS 下保持 `true` |
| `VOYAGE_API_KEY` | `voyage_api_key` | 空 | 否 | 语义搜索 embedding |
| `ANTHROPIC_API_KEY` | `anthropic_api_key` | 空 | 否 | Admin copilot（`/cm/copilot`）与问答合成；未设置则 copilot 不可用 |
| `RESEND_API_KEY` | `resend_api_key` | 空 | 否 | CMS Agent 邮件摘要（Resend） |
| `EMAIL_FROM` | `email_from` | 空 | 否 | 发件地址，如 `LightCMS Agent <agent@example.com>` |
| `PUBLIC_BASE_URL` | `public_base_url` | 回退到 `BASE_URL`（`config.go:56-58`） | 否 | 公共站点与 Admin/API 不同域时设置（规范 §19.2）；同域留空 |
| `STATIC_STORAGE_PROVIDER` | `static_storage_provider` | `filesystem` | 否 | MVP 生产**仅接受 `filesystem`**；其它值启动拒绝（见 §3） |
| `PAGE_GENERATION_RATE_LIMIT` | `page_generation_rate_limit` | `60`（次/分钟，进程内固定窗口） | 否 | `POST /api/v1/page-generation` 发布吞吐上限（`publication_runtime.go:103,186-204`） |
| `IDEMPOTENCY_TTL_HOURS` | `idempotency_ttl_hours` | `24`（允许 1–72） | 否 | 幂等记录保留；越界启动拒绝（见 §3） |
| `IDEMPOTENCY_LEASE_MINUTES` | `idempotency_lease_minutes` | `5`（必须 ≥1） | 否 | 处理租约；`<1` 启动拒绝 |
| `PUBLICATION_RETENTION_DAYS` | `publication_retention_days` | `90` | 否 | superseded/unpublished immutable 对象保留（规范 §17.6） |
| `PUBLICATION_FAILED_RETENTION_DAYS` | `publication_failed_retention_days` | `7` | 否 | failed/staged 对象保留（规范 §17.6） |
| `PUBLICATION_QUARANTINE_RETENTION_DAYS` | `publication_quarantine_retention_days` | `30` | 否 | quarantine 隔离文件保留（规范 §17.6） |
| `PUBLICATION_SCAN_INTERVAL_MINUTES` | `publication_scan_interval_minutes` | `10` | 否 | recovery scanner 周期；启动时先扫一次，随后每 10 分钟单飞扫描 |
| `PUBLICATION_STAGE_TIMEOUT_MINUTES` | `publication_stage_timeout_minutes` | `15` | 否 | stale stage / stale `.next` 判定阈值 |

注意：

- 仓库自带的 `config.prod.json.example` **尚未包含上述 V3 发布键**（仅含 port/mongo_uri/env/session_secret/base_url/secure_cookies）。用 JSON 文件方式部署生产时，需手动追加 `public_base_url` 等键（JSON 键名见上表第二列）；缺失键同样由 `ApplyPublicationDefaults`（`config.go:166,220`）填默认值。走 Fly.io 环境变量路径不受此影响。
- 规范 §36 提到的 R2/S3 键（`R2_ACCOUNT_ID` / `R2_BUCKET` / … / `S3_REGION` / …）**不属于 MVP 生产配置**，当前代码不读取它们；在 Storage ADR 与完整集成测试通过前不要设置（设置了也无效果，且 `STATIC_STORAGE_PROVIDER` 仍只能是 `filesystem`）。

## 3. 启动拒绝项（Fail-fast）

以下检查在 serve 之前执行，失败即 `log.Fatalf` 退出，不对外服务：

1. **非 filesystem 存储 provider**（`config/config.go:92-104` `ValidatePublicationConfig`，`publication_runtime.go:65` 首先调用）：
   `STATIC_STORAGE_PROVIDER` 非 `filesystem` → 报错 `unsupported STATIC_STORAGE_PROVIDER ... MVP production supports only "filesystem" (R2/S3 require a Storage ADR and full integration suite)`。R2/S3 在 MVP 直接拒绝。
2. **幂等 TTL 越界**（`config.go:97-99`）：`IDEMPOTENCY_TTL_HOURS` 不在 1–72 → 拒绝。对应 `idempotency.NewService` 同样校验（`internal/product/idempotency/service.go:37-43`）。
3. **幂等 lease 非法**（`config.go:100-102`）：`IDEMPOTENCY_LEASE_MINUTES < 1` → 拒绝。
4. **生产 standalone Mongo**（`cmd/server/publication_runtime.go:160-173` `requireReplicaSet`，`publication_runtime.go:68` 调用）：生产（`ENV=production`）检测到 standalone → 拒绝并提示 `--replSet` 初始化；**开发环境仅警告并继续服务**（非发布路径仍可用，发布事务会失败，须用 Task 0 replica-set fixture 跑发布路径）。
5. **Mongo 拓扑不可验证**（`requireReplicaSet` 首分支）：连不上 / `hello.setName` 失败 → 拒绝（"cannot verify MongoDB topology"）。
6. **产品索引集失败**（`cmd/server/main.go:457`）：`EnsureProductIndexes`（幂等唯一键、active pointer、outbox、template versions 索引）失败 → Fatal，并提示先跑 `migrate-publications --dry-run` 查 blocker。
7. **必填缺失**（env 路径，`config.go:176-183`）：`MONGO_URI` / `SESSION_SECRET` 缺失 → 拒绝。

非致命降级（注意区分，排障时不要误判为启动失败）：

- `PUBLIC_BASE_URL` 解析失败 → public URL resolver 被禁用并记一条 `Warning: public URL resolver disabled` 日志，服务继续（`publication_runtime.go:80-87`）。此时 Publication 激活成功后返回的 URL 为空，属配置错误，需修复而非重启能自愈。

## 4. 首次上线迁移演练步骤

迁移 CLI 由 Task 16 契约定义，实现在同一 server 二进制内（**没有第二个二进制**，`publication_runtime.go:206-210`）：

```bash
lightcms migrate-publications --dry-run   # 只读诊断，零写入
lightcms migrate-publications --apply     # 实际执行
```

准确用法（`publication_runtime.go:211-226`）：`--dry-run` 与 `--apply` **恰选其一**，否则 exit 2；未知 flag exit 2。`--apply` 未完成（仍有 blocker）同样 exit 2。`--dry-run` 即使报告未完成也 exit 0（报告以 JSON 打到 stdout，`publication_runtime.go:240-242`），`--apply` 成功且 `completed=true` 时 exit 0 并记录 build 版本。

演练步骤（对应规范 §35.1–§35.3，顺序不可颠倒）：

1. **备份**：按 [BACKUP-RESTORE.md](BACKUP-RESTORE.md) 对 MongoDB 与 `content/`（canonical + immutable + quarantine）做完整备份，先验证备份可恢复再继续。
2. **代表性数据拷贝**：在与生产同构的演练环境（replica set + filesystem volume 拷贝）上执行，覆盖 verified、mismatch、missing static、deleted、fork、大写冲突样本（Task 19 口径）。
3. **`lightcms migrate-publications --dry-run`**，审阅 JSON 报告（字段定义见 `internal/product/migration/report.go:8-61`）：
   - 阻塞类（任一非空即不可 `--apply`，`BlockingCount` 见 `report.go:99-106`）：`invalid_slugs`（空/非法/重复/大小写冲突，**绝不静默改名**，§34.1）、`canonical_collisions`（一 canonical 被多 live content 认领，先由管理员解）、`version_duplicates`（阻塞唯一索引）、`invalid_paths`（traversal/空段/query 等 canonicalization 失败）、`missing_files`（无 canonical 文件：**不创建 active**，§35.3 矩阵）、`render_errors`（fresh render 失败：保留旧 canonical 继续服务，不建 active）。
   - 候选类：`verified`（canonical 字节 == fresh render，将建成 verified active）、`legacy_unverified`（字节不一致：将旧字节拷贝为 immutable legacy object + `verification_status=legacy_unverified` 的 active，canonical **继续服务**，scanner 永不将其判为 orphan）、`skipped_active`（已有 active，中断续跑跳过）。
4. **人工修复阻塞项**：模板 slug、canonical 冲突、非法 path 逐一处理；每修完一批重跑 `--dry-run` 直到 `HasBlocking()==false`。
5. **`lightcms migrate-publications --apply`**：写入 `system_migrations.publication_model_v1=running` → 逐页对账建 active（verified 或 legacy_unverified）→ 校验新 canonical 唯一索引 → 删除 legacy `(full_path, fork_id)` 索引（仅在新索引确认后，`report.go:57-60`）→ blocker 清零后写 `completed`。**`completed` 之前 scanner 只报告 orphan、不隔离**（规范 §35.3 规则 2/5），这是故意的：迁移窗口内 canonical-without-active 不会被 quarantine。
6. **验证**：抽查 Public URL 可访问；确认“没有旧公开 URL 静默消失”（Task 19 口径）；检查 `migration_from`/`migration_to` 跃迁与 `completed=true`。
7. **切生产流量**：确认 `completed` 后再按 §5 清单放行。`legacy_unverified` 页面保持服务，但**禁止自动 template upgrade**，管理员三选一（接受重渲染结果建新 verified Publication / 人工确认 legacy 并 pin 延长精确回滚期 / 下线），详见 [PUBLICATION-RUNBOOK.md](PUBLICATION-RUNBOOK.md)。

`legacy_unverified` 是 `content_publications.verification_status` 字段，不是孤立 migration record（规范 §35.3）；它是 active Publication，保留旧 canonical 服务是设计行为，不是事故。

## 5. 上线检查清单（Go-live Gate）

- [ ] MongoDB 为 replica set（生产 standalone 会被拒绝启动；演练环境同样要求 replica set，否则发布事务不可用）。
- [ ] `MONGO_URI` / `SESSION_SECRET` 已通过 `fly secrets set` 设置，且已执行 machine update（或 `./deploy.sh`）使其生效，并用 `fly ssh console -C 'sh -c env'` 验证（见 §1 quirks）。
- [ ] `STATIC_STORAGE_PROVIDER=filesystem`（或留空）；`IDEMPOTENCY_TTL_HOURS` 在 1–72 内；`IDEMPOTENCY_LEASE_MINUTES ≥ 1`。
- [ ] `BASE_URL` 为生产 HTTPS 域名；跨域公共站点则另设 `PUBLIC_BASE_URL`（规范 §19.3：生产必须 HTTPS，启动时验证）。
- [ ] 镜像构建传入 `--build-arg GIT_SHA=$(git rev-parse --short HEAD)`（`Dockerfile:14-18`）。
- [ ] 迁移 `--dry-run` 无阻塞项，`--apply` 达到 `completed=true`；`legacy_unverified` 清单已评审并指定处置（三选一）。
- [ ] 无旧公开 URL 静默消失（抽查 + 全量对账二选一，至少抽查全部高流量页）。
- [ ] 发布冒烟：draft → preview → publish → 回滚一条测试页；同 `Idempotency-Key` 重试返回缓存响应（见 [API.md](API.md)）。
- [ ] 日志出现启动扫描与 worker 运行，无 `[P0-ALERT]`；`outbox_backlog` 回落到 0（见 [OPERATIONS.md](OPERATIONS.md)）。
- [ ] volume 磁盘水位：按页面量 × 保留期（90/7/30 天）估算，`lightcms_data` 初始 1GB 不够即提前扩容。
- [ ] 回滚预案就绪：schema 为 additive change，回滚部署保留新 collections、不删历史 metadata、static canonical path 兼容（规范 §35.4）；备份已验证可恢复（[BACKUP-RESTORE.md](BACKUP-RESTORE.md)）。
- [ ] secret 不在仓库：`config.dev.json` / `config.prod.json` / `.env*` 已进 `.gitignore`（见 [SECURITY.md](SECURITY.md)）。
