# LightCMS V3（模板化页面生成与静态发布）产品总览

> 文档索引：[API.md](API.md)（HTTP 契约与 curl 示例）·
> [USER-MANUAL.md](USER-MANUAL.md)（用户手册）·
> [TEMPLATE-GUIDE.md](TEMPLATE-GUIDE.md)（模板指南）·
> [AGENT-INTEGRATION.md](AGENT-INTEGRATION.md)（Agent 集成）·
> [DEPLOYMENT-LOCAL.md](DEPLOYMENT-LOCAL.md)（本地部署）·
> [DEPLOYMENT-PRODUCTION.md](DEPLOYMENT-PRODUCTION.md)（生产部署）·
> [PUBLICATION-RUNBOOK.md](PUBLICATION-RUNBOOK.md)（发布运维）·
> [OPERATIONS.md](OPERATIONS.md)（日常运维）·
> [BACKUP-RESTORE.md](BACKUP-RESTORE.md)（备份恢复）·
> [UPGRADE.md](UPGRADE.md)（升级）·
> [SECURITY.md](SECURITY.md)（安全）· [UPSTREAM-SYNC.md](UPSTREAM-SYNC.md)（上游同步）

LightCMS V3 在原有轻量 CMS（MongoDB + Admin UI + 静态输出）之上，增加**模板化页面生成与静态发布系统**：模板不可变版本、草稿→预览→发布、filesystem 不可变存储 + saga 切流、幂等发布、outbox webhook、recovery scanner。单 Go 应用、单 Admin UI，不引入二级服务。

## 1. 安装

```bash
go build -o bin/lightcms ./cmd/server   # 主服务（含 migrate-publications，见 §6）
go build -o bin/lightcms-mcp ./cmd/mcp   # MCP 服务（改 cmd/mcp 或 internal/mcp 后重编 + 重启宿主）
```

- 本地：复制 `config.dev.json.example` 为 `config.dev.json`，填 `mongo_uri`；`./bin/lightcms`；Admin `http://localhost:8082/cm`（默认 `admin@localhost / admin123`，首次登录强制改密）。详见 [DEPLOYMENT-LOCAL.md](DEPLOYMENT-LOCAL.md)。
- 生产（Fly.io，`metavert-cms`）：环境变量配置 + `./deploy.sh`（勿用裸 `fly deploy`），见 [DEPLOYMENT-PRODUCTION.md](DEPLOYMENT-PRODUCTION.md)。
- 发布前提：**MongoDB 必须是 replica set**（生产 standalone 拒绝启动；开发仅警告）。DB 测试需要 `.env.test`（库名须含 "test"），`go test -p 1` 运行。

## 2. 单进程拓扑

```text
./lightcms（单二进制、单进程）
 ├── Admin UI（/cm）+ REST API（/api/v1）+ 公共站点（/）
 ├── generation facade（POST /api/v1/page-generation）
 ├── publication saga（stage → verify → cutover → commit，失败补偿）
 ├── idempotency service（TTL 24h / lease 5m）
 ├── outbox worker（同进程，webhook at-least-once 投递）
 └── recovery/GC scanner（同进程；启动扫一次 + 每 10 分钟）
         │
         ├── MongoDB（replica set，必需）
         └── filesystem（volume /app/content，MVP 唯一生产 provider）
```

一次装配：`buildPublicationRuntime`（`cmd/server/publication_runtime.go:64`）在 `main.go` 中只调一次；worker 随 server context 启停（`cmd/server/main.go:469-470`）。R2/S3 不在 MVP（`STATIC_STORAGE_PROVIDER` 仅接受 `filesystem`，见 [DEPLOYMENT-PRODUCTION.md](DEPLOYMENT-PRODUCTION.md) §3）。

## 3. 配置

环境变量（完整表见 [DEPLOYMENT-PRODUCTION.md](DEPLOYMENT-PRODUCTION.md) §2；零值 = 规范默认，由 `ApplyPublicationDefaults` 填充，`config/config.go:55-86`）：

```text
MONGO_URI / SESSION_SECRET                       必填
BASE_URL（生产 HTTPS）/ PUBLIC_BASE_URL（跨域时，默认回退 BASE_URL）
STATIC_STORAGE_PROVIDER=filesystem（MVP 唯一合法值）
PAGE_GENERATION_RATE_LIMIT=60/min
IDEMPOTENCY_TTL_HOURS=24（1–72）/ IDEMPOTENCY_LEASE_MINUTES=5（≥1）
PUBLICATION_RETENTION_DAYS=90 / PUBLICATION_FAILED_RETENTION_DAYS=7
PUBLICATION_QUARANTINE_RETENTION_DAYS=30
PUBLICATION_SCAN_INTERVAL_MINUTES=10 / PUBLICATION_STAGE_TIMEOUT_MINUTES=15
```

`config.dev.json` / `config.prod.json` / `.env*` 绝不提交（见 [SECURITY.md](SECURITY.md) §5）。

## 4. Quickstart

1. 选模板建草稿：`POST /api/v1/page-generation`（`mode=draft`，模板 schema 见 `GET /api/v1/templates/{slug}/schema`，`ETag: "template-version-3"`）。
2. 预览：`mode=preview`（不落发布记录）。
3. 发布：`mode=publish`，**必须带 `expected_template_version` + `Idempotency-Key`**；模板版本漂移返回 `409 TEMPLATE_VERSION_CHANGED` 且零副作用（换新版本号重试，不要换 key）。
4. 查发布：`GET /api/v1/content/{id}/publications`；回滚：`POST .../publications/{publication_id}/rollback`（新 Publication，不改旧记录）。
5. 可执行示例（含 201/200/409/422/428）见 [API.md](API.md)；模板字段写法见 [TEMPLATE-GUIDE.md](TEMPLATE-GUIDE.md)；Agent（sandbox key、MCP、outbox 去重）见 [AGENT-INTEGRATION.md](AGENT-INTEGRATION.md)。

## 5. 迁移警告（Breaking Change，必读）

- **已发布 Content 的更新默认只改草稿，不再直写线上**：此前依赖 `PUT` 即改线上的客户端，必须显式调用 Publish（`mode=publish`），否则线上纹丝不动。
- 模板 slug 建后**不可变**（普通 update 改 slug 被拒，须走 `migrate-slug`，见 §6）；模板每次保存生成不可变版本，旧页面不受影响，按需走 Upgrade Job 重发。
- 首次上线必须跑迁移（`migrate-publications --dry-run` → 修阻塞 → `--apply`，见 [DEPLOYMENT-PRODUCTION.md](DEPLOYMENT-PRODUCTION.md) §4）：canonical 字节与 fresh render 不一致的页面建成 `legacy_unverified` active（继续服务、禁自动升级），管理员三选一处置（见 [PUBLICATION-RUNBOOK.md](PUBLICATION-RUNBOOK.md) §2）；无 canonical / render 失败的页面**不建 active**，必须先修好。
- 迁移完成前 scanner 只报告 orphan、不隔离；`completed` 后才开 quarantine（误删找回见 [PUBLICATION-RUNBOOK.md](PUBLICATION-RUNBOOK.md) §3）。

## 6. 命令索引

```text
# 迁移（同一 server 二进制，无第二二进制）
lightcms migrate-publications --dry-run    # 只读诊断（JSON 报告，exit 0）
lightcms migrate-publications --apply      # 执行（未完成 exit 2）

# 模板 slug 迁移（admin + template.edit；历史版本/页面 URL/Publication 不变）
POST /api/v1/templates/{id}/migrate-slug

# 模板升级（先 Preview 看影响，再起 Job 执行；Job 可查询/运行）
GET  /api/v1/templates/{slug}/upgrade-preview
POST /api/v1/templates/{slug}/upgrade-jobs
GET  /api/v1/templates/upgrade-jobs/{job_id}
POST /api/v1/templates/upgrade-jobs/{job_id}/run

# 回滚 / 恢复（两个 restore 命令：精确回滚 vs 重渲染恢复）
POST /api/v1/content/{id}/publications/{publication_id}/rollback   # 精确：immutable 仍在，字节一致
POST /api/v1/content/{id}/restore-and-publish                      # 重渲染：历史版本 + 模板版本，不承诺字节一致
```

Admin 对应面：`/cm/.../content/{id}/publications`（历史）、`revert_live`（精确回滚）、`upgrade-preview` / `upgrade-jobs/{jobID}/run`（升级）。升级细节见 [UPGRADE.md](UPGRADE.md)，备份恢复见 [BACKUP-RESTORE.md](BACKUP-RESTORE.md)，日常值班见 [OPERATIONS.md](OPERATIONS.md)，生产部署见 [DEPLOYMENT-PRODUCTION.md](DEPLOYMENT-PRODUCTION.md)。
