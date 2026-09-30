# LightCMS V3 本地联调部署文档（Task 18C）

> 范围：单机 Docker + 本地构建的**联调/演练环境**，不是生产部署手册。
> 生产拓扑、配置全表与日常运维见兄弟文档 `docs/DEPLOYMENT.md`、`docs/OPERATIONS.md`；
> 发布操作细节见 `docs/PUBLICATION-RUNBOOK.md`；API 契约见 `docs/API.md` 与
> `docs/openapi/page-generation-v1.yaml`；备份恢复见 `docs/BACKUP-RESTORE.md`；
> 模板升级见 `docs/UPGRADE.md`。
> 本文件中的每一条命令都在本 worktree（分支 `task/18c-localdeploy`，基线 `5c59494`）
> 真实跑通过，实测输出见 §7。**跑不通的命令没有写进本文。**

## 1. 环境要求（实测版本）

| 依赖 | 要求 | 本次实测 |
|---|---|---|
| Go | `go.mod` 声明 `go 1.25.0` | `go version go1.25.0 darwin/arm64` |
| Docker | daemon 可用即可 | `Docker version 29.4.3` |
| MongoDB | 镜像 `mongo:7.0.14`（`docker-compose.test.yml`  pinned） | `db.version()` → `7.0.14`，`rs.status().myState` → `1`（PRIMARY） |
| 传输 | server=HTTP，MCP=stdio（无端口），CLI=HTTPS/HTTP 客户端 | 见 §4 |

说明：

- V3 发布链（激活、unpublish、幂等绑定、outbox 落库）全部使用**多文档事务**，
  standalone `mongod` 跑不起来。本地必须用单节点副本集（`rs0`），生产环境启动时
  程序会直接拒绝 standalone（development 只告警、继续服务）。
  副本集 fixture 的完整说明见 `docs/implementation/test-environment.md`。
- `mongodump` / `mongorestore`（100.10.0）已包含在 `mongo:7.0.14` 镜像内，
  备份恢复用法见 `docs/BACKUP-RESTORE.md`。

## 2. 一步步启动

### 2.1 启动 Mongo 副本集栈

```bash
docker compose -f docker-compose.test.yml up -d
docker compose -f docker-compose.test.yml logs mongo-init
# 期望：首次 "RS initiate issued" 随后 "RS ready (myState=1)"；
#       重启 "RS already initialized" 随后 "RS ready (myState=1)"。
docker exec lightcms-mongo-test mongosh --quiet --eval 'db.version(); rs.status().myState'
# 期望：7.0.14 / 1
```

实测记录：本机 27017 已被同一镜像的健康栈占用（`Up 2 hours (healthy)`，
`myState=1`），`up -d` 因容器名冲突无法重复创建——这是**预期行为**，说明栈已就绪，
直接复用即可。后文所有“写验证”在**隔离栈**（27018，容器名 `lightcms-mongo-18c`，
同镜像同 `--replSet rs0` 参数）上完成，初始化命令为：

```bash
docker run -d --name lightcms-mongo-18c -p 27018:27017 mongo:7.0.14 \
  --replSet rs0 --bind_ip_all --port 27017
docker exec lightcms-mongo-18c mongosh --quiet --eval \
  'rs.initiate({_id:"rs0",members:[{_id:0,host:"127.0.0.1:27017"}]})'
# 期望：{ ok: 1 }；随后 rs.status().myState → 1
```

注意：`rs.initiate` 的 member host 在本机端口映射场景下必须写
`127.0.0.1:27017`（容器名形式在本镜像实测报
`No host described ... maps to this node`，故文档采用已跑通的写法）。

停止/清理：

```bash
docker compose -f docker-compose.test.yml down      # 保留数据卷
docker compose -f docker-compose.test.yml down -v   # 连测试数据一起删
```

### 2.2 配置 `.env.test`（给 `go test` 用，不是给 server 用）

```bash
cp .env.test.example .env.test   # gitignored，绝不提交
```

内容（已验证可直接用）：`MONGODB_URI=mongodb://127.0.0.1:27017/lightcms-test?replicaSet=rs0&directConnection=true`，
`DATABASE_NAME=lightcms-test`。库名必须含 `test`，否则测试安全 guard 拒绝运行。
server 本体**不读** `.env.test`，它读 `config.dev.json`（下一步）。

### 2.3 三件套构建

```bash
go build -o bin/lightcms ./cmd/server
go build -o bin/lightcms-mcp ./cmd/mcp
go build -o bin/lightcms-cli ./cmd/cli
```

实测：三条全部 exit 0，`bin/` 产出 `lightcms` / `lightcms-mcp` / `lightcms-cli`
（`bin/` 已 gitignore，不入库）。

### 2.4 写 `config.dev.json`（gitignored，绝不提交）

server 启动只认 `config.dev.json`（或环境变量 `MONGO_URI` 等，见 `config/config.go`）。
联调最小配置（27018 为隔离栈端口；标准栈用 27017）：

```json
{
  "port": "18082",
  "mongo_uri": "mongodb://127.0.0.1:27018/lightcms?replicaSet=rs0&directConnection=true",
  "env": "development",
  "session_secret": "dev-session-secret-change-in-prod-18c-local",
  "base_url": "http://localhost:18082",
  "secure_cookies": false,
  "public_base_url": "http://localhost:18082"
}
```

V3 新增配置键（`config/config.go`，零值自动填 spec §36 默认，可不写）：

| 键 | 默认 | 含义 |
|---|---|---|
| `PUBLIC_BASE_URL`（`public_base_url`） | = `base_url` | 公网 URL 起源，匿名访问与 `public_url` 以它为准 |
| `STATIC_STORAGE_PROVIDER` | `filesystem` | MVP 只允许 `filesystem`，其它值启动拒绝 |
| `PUBLICATION_STAGE_TIMEOUT_MINUTES` | `15` | 残留 stage / `.next-*` 的清理期限 |
| `PUBLICATION_RETENTION_DAYS` | `90` | superseded/unpublished 不可变对象的保留期 |
| `PUBLICATION_FAILED_RETENTION_DAYS` | `7` | failed/staged 对象的保留期 |
| `PUBLICATION_QUARANTINE_RETENTION_DAYS` | `30` | 隔离区文件的保留期 |
| `PUBLICATION_SCAN_INTERVAL_MINUTES` | `10` | 恢复扫描间隔（启动时先扫一次） |
| `IDEMPOTENCY_TTL_HOURS` | `24`（允许 1–72） | 幂等记录保留期，超范围启动拒绝 |
| `IDEMPOTENCY_LEASE_MINUTES` | `5` | 处理租约 |
| `PAGE_GENERATION_RATE_LIMIT` | `60`/分钟 | 发布生成限流 |

### 2.5 启动 server / MCP / CLI

```bash
./bin/lightcms &                                   # 前台会占用终端，建议后台+日志
curl -s http://localhost:18082/health              # 期望：OK（HTTP 200）
```

实测：日志 `Starting LightCMS in development mode` →
`Connected to MongoDB successfully` →
`LightCMS starting on http://localhost:18082`，`/health` 返回 `OK`。

MCP（stdio，无端口）：

```bash
LIGHTCMS_API_KEY='<key>' LIGHTCMS_CONFIG_DIR=. ./bin/lightcms-mcp
```

实测行为：**不带** `LIGHTCMS_API_KEY` 时直接报错退出
（`Error: LIGHTCMS_API_KEY environment variable is required`）；
带上联调 key + config 后进程正常进入 MCP 主循环（stdin EOF 时才退出）。
完整 `tools/list` 联调留待 §8（Task 19）。

CLI：

```bash
./bin/lightcms-cli --help    # exit 0，打印用法
./bin/lightcms-cli           # exit 1：Error: API key is required
```

### 2.6 端口说明

| 进程 | 端口 | 说明 |
|---|---|---|
| `lightcms` server | `config.dev.json` 的 `port`（开发默认 `8082`；本次联调 `18082`） | Admin `/cm`、REST `/api/v1`、公网页面同端口 |
| Mongo 标准栈 | `27017` | `docker-compose.test.yml` 映射 |
| Mongo 隔离栈（仅本次验证） | `27018` | 写验证专用，避免污染共享栈 |
| `lightcms-mcp` | 无（stdio） | 通过管道与宿主通信 |
| Task 19 全栈复验 | 随机 50k+ 端口 | 见 §8，不在本轮固定 |

## 3. 迁移演练（`migrate-publications`）

**顺序铁律（实测血泪）：先启动 server 完成 seed，再跑迁移。**
启动 server 会自动 seed 7 个系统模板与示例页；若迁移已 `completed` 之后才 seed，
启动扫描会把无 active 记录的种子文件当孤儿隔离（实测
`[P0-ALERT] ... code=RECOVERY_ORPHAN_QUARANTINED path=/ ... (migration completed)`）。
Task 14 契约：`Run`（`--apply`）/ `DryRun`（`--dry-run`），同一二进制，无第二个迁移程序。

### 3.1 dry-run（零写入）

```bash
./bin/lightcms migrate-publications --dry-run
```

实测（空库）：exit 0，JSON 报告 `dry_run=true`，`migration_from=""`，
各阻塞清单全空。`--dry-run` 绝不写 DB、不碰文件、不动迁移旗标。

### 3.2 apply

```bash
./bin/lightcms migrate-publications --apply
```

实测（空库）：exit 0，`migration_to="completed"`，`completed=true`，
`new_indexes_verified=true`，`legacy_index_dropped=true`，
日志 `migration: completed (build 7.2.2)`。
中断后重跑是 resume 语义（已有 active 的页跳过、`skipped_active`）；
完成后重跑是 no-op（`migration_from=completed` → `completed=true`，exit 0，实测）。

### 3.3 误用（exit 2）

```bash
./bin/lightcms migrate-publications                 # 无 flag
./bin/lightcms migrate-publications --bogus         # 未知 flag
./bin/lightcms migrate-publications --dry-run --apply  # 双 flag
```

实测：三种全部 exit **2**，
stderr 为 `usage: lightcms migrate-publications --dry-run|--apply (exactly one)`
或 `unknown flag "--bogus": usage: ...`。注意必须且只能带一个 flag。

### 3.4 有种子数据的真实演练（本次实测完整链路）

1. 清库后启动 server（自动 seed 7 模板 + 示例页），`/health` 200。
2. `--dry-run` 报阻塞：`invalid_paths` 2 条——种子示例页 `full_path` 为空。
   管理员动作（二选一）：补齐 `full_path`，或删除演示行。本次联调删除演示行。
3. 另发现 backfill 缺口（已列入 §6.3，**本次只修测试库、不改代码**）：
   seed 模板缺 `current_version` 字段，`$lte: 0` 过滤器匹配不到，`--apply` 后仍为缺失；
   联调中用 `updateMany({current_version:{$exists:false}}, [{$set:{current_version:1}}])`
   修复 7 个模板后重跑 `--apply` → `completed=true`。
4. 种子模板 v1 状态为 `draft`，发布前需经模板更新接口激活为 `active`
   （`current_version` 变为 2），否则发布报 `409 TEMPLATE_NOT_ACTIVE`（实测）。

## 4. 冒烟验证：发一篇测试页并打开公网 URL（实测全链路）

联调用 admin/key 用 mongosh 写入（`api_keys.key_hash` = `sha256(rawKey)` 十六进制，
`prefix` = key 前 11 字符，`user_id` 指向 role 为 `admin` 的用户；`scopes` 为空
= 完整权限——均为 `internal/services/apikey.go` 已验证语义，**仅限联调库**）：

```bash
RAWKEY="lc_$(openssl rand -hex 16)"
HASH=$(printf '%s' "$RAWKEY" | shasum -a 256 | awk '{print $1}')
# mongosh：users 插 admin；api_keys 插 {name, prefix, key_hash: HASH, user_id}
```

步骤与实测：

```bash
# 1. schema（模板版本与 ETag 同源，发布前先取 expected_template_version）
curl -H "Authorization: Bearer $RAWKEY" \
  "http://localhost:18082/api/v1/templates/blank-page/schema"
# 实测：{"template":"blank-page","template_version":2,"fields":[{"name":"content",...}]}

# 2. 发布（外部触发发布必须带 Idempotency-Key，否则 428 且零写入）
curl -X POST "http://localhost:18082/api/v1/page-generation" \
  -H "Authorization: Bearer $RAWKEY" -H "Content-Type: application/json" \
  -H "Idempotency-Key: smoke-18c-002" \
  -d '{"template":"blank-page","expected_template_version":2,"title":"Smoke 18C",
       "slug":"smoke-18c","folder_path":"/","mode":"publish",
       "data":{"content":"<p>hello local deploy 18c</p>"}}'
# 实测：HTTP 201
# {"action":"created","mode":"publish","full_path":"/smoke-18c","published":true,
#  "requires_publish":false,"template_version":2,
#  "public_url":"http://localhost:18082/smoke-18c",
#  "publication_id":"6abd260560a2f1e9cad7039c",...}

# 3. 匿名打开公网 URL
curl "http://localhost:18082/smoke-18c"
# 实测：HTTP 200，正文含 <p>hello local deploy 18c</p>

# 4. 磁盘双写验证
ls content/publications/<contentID>/<publicationID>/index.html  # 不可变对象
ls content/generated/smoke-18c.html                             # canonical 投影
# 实测：两处字节一致（grep 各命中 1 次）

# 5. 同 key 重放（幂等）
# 实测：同 Idempotency-Key 重发返回同一 publication_id，未产生新 Publication；
# content_publications 表 active 记录 verification_status=verified。
```

注意：联调从 `blank-page` 这类只需一个必填字段的种子模板开始最省事；
`expected_template_version` 取自 schema 返回的 `template_version`，
过期会报 `409 TEMPLATE_VERSION_CHANGED` 且零写入。

## 5. 冒烟后的清理（联调专用）

```bash
pkill -f "bin/lightcms"
docker stop lightcms-mongo-18c && docker rm lightcms-mongo-18c  # 仅隔离栈
rm -rf content config.dev.json   # 均为 gitignore，还原 worktree 干净状态
git status --short --branch      # 期望：仅 3 个新增 docs，无其它变更
```

另注意：server 启动会重写 `static/css/theme-vars.css`（EnsureThemeCSS）。
联调后若它变脏，用 `git checkout -- static/css/theme-vars.css` 还原——
**本文档任务禁止提交任何代码/生成文件变更**（本次实测已还原）。

## 6. 常见故障排查

| 现象 | 原因 | 动作 |
|---|---|---|
| 生产启动报 `production requires MongoDB replica set mode` | 用了 standalone Mongo，事务不可用 | 切到副本集（§2.1）；dev 下只是 WARNING |
| `STATIC_STORAGE_PROVIDER` / `IDEMPOTENCY_TTL_HOURS` 启动拒绝 | 配置超出 MVP 白名单（见 `config.ValidatePublicationConfig`） | 改回 `filesystem` / 1–72 |
| `--dry-run` 报 `invalid_slugs` | 模板 slug 空/非法/重复/大小写折叠冲突 | 人工改 slug 重跑；程序**永不**静默重命名 |
| `--dry-run` 报 `canonical_collisions` | 多个 live 内容折叠到同一 canonical key | 人工解决冲突前新唯一索引建不起来，`--apply` 会卡 `running` |
| `--dry-run` 报 `invalid_paths`（含 seed 行 `full_path:""`） | 路径非法/缺失 | 补 `full_path` 或删演示行（§3.4 实测）；修完重跑 |
| `--apply` 报 `missing_files` / `render_errors` | 缺 canonical 文件或重渲染失败 | 按报告逐页修；缺文件页**不建** active 记录，绝不静默丢失 URL |
| 发布 `409 TEMPLATE_NOT_ACTIVE` | 模板版本是 `draft`（迁移 backfill 的 v1 默认 draft） | 把模板更新到 `active` 再发布（§3.4 实测：blank-page v1→v2） |
| schema 报 `resolve template` / INTERNAL_ERROR | 模板缺 `current_version`（§3.4 的 backfill 缺口） | 联调可手动补 `current_version=1`；**正式修复归 Task 14/Task 3**，本文档不改代码 |
| 发布 `428` | 外部 publish/rollback/restore/revert 缺 `Idempotency-Key` | 加上重发；428 保证零写入 |
| 发布 `409 TEMPLATE_VERSION_CHANGED` | `expected_template_version` 过期 | 重取 schema 再发 |
| 同 key 重发报 `409`（body 变了） | 幂等键绑定 canonical body | 换新 key 发新请求 |
| 启动后种子页被隔离（`RECOVERY_ORPHAN_QUARANTINED`） | 迁移已 `completed` 之后才 seed（§3 顺序铁律） | 先 seed 后迁移；已被隔离的去 `content/quarantine/` 找回（保留 30 天，见 `docs/BACKUP-RESTORE.md`） |
| 同 key 并发一个成功一个 `409`（`/News/Foo` vs `/news/foo`） | canonical 唯一性预期行为 | 失败方换路径/key 重试 |
| 27017 端口/容器名冲突 | 本机已有同镜像栈 | 复用健康栈（`myState=1`），或另起隔离栈（§2.1 实测） |

## 7. 实测命令与结果总表（本 worktree，2026-09-30）

| # | 命令 | 结果 |
|---|---|---|
| 1 | `go version` / `docker --version` | `go1.25.0 darwin/arm64` / `Docker 29.4.3` |
| 2 | `go build -o bin/lightcms ./cmd/server && go build -o bin/lightcms-mcp ./cmd/mcp && go build -o bin/lightcms-cli ./cmd/cli` | exit 0，三二进制产出 |
| 3 | `docker compose -f docker-compose.test.yml up -d`（27017 被占） | 容器名冲突，复用已有健康栈（预期行为） |
| 4 | `docker exec lightcms-mongo-test mongosh --eval db.version()/myState` | `7.0.14` / `1` |
| 5 | 隔离栈 `docker run ... mongo:7.0.14 --replSet rs0` + `rs.initiate(127.0.0.1:27017)` | `{ok:1}`，`myState=1` |
| 6 | `./bin/lightcms migrate-publications --dry-run`（空库） | exit 0，`dry_run=true`，阻塞清单全空 |
| 7 | `./bin/lightcms migrate-publications --apply`（空库） | exit 0，`completed=true`，索引双确认 |
| 8 | 误用（无 flag / `--bogus` / 双 flag） | exit **2** + usage（`>/dev/null` 外重定向取码，管道会掩盖退出码） |
| 9 | `--apply` 完成后重跑 | no-op，`completed=true`，exit 0 |
| 10 | `./bin/lightcms &` + `curl /health` | `OK`，HTTP 200 |
| 11 | `./bin/lightcms-mcp`（无 key） | 报错退出（key 必需） |
| 12 | `./bin/lightcms-cli --help` / 无参 | exit 0 / exit 1（API key 必需） |
| 13 | `mongodump --archive` / `mongorestore --archive`（容器内） | dump 21658 bytes；drop 后 count 0→restore 后 1，`status=completed`（详见 `docs/BACKUP-RESTORE.md`） |
| 14 | `tar -czf content` | 含 `publications/`、`generated/`、`quarantine/`，解包验证通过 |
| 15 | seed→迁移→schema→publish 201→匿名 GET 200→磁盘双写→同 key 重放同 ID | 全链路通过（§4） |

## 8. 附录：Task 19 全栈复验（待填写——本节由 Task 19 团队更新）

> ***以下为 Task 19 预留位，本文档正文已完备。本节在全量本地全栈部署验证时填写，
> 只追加、不改写正文结论。***

- [ ] 复验端口（随机 50k+）：server 端口 `________`，Mongo 端口 `________`
- [ ] 三件套构建输出（粘贴 `ls -la bin/` 与 Go 版本）：
- [ ] 副本集状态（粘贴 `db.version()` / `myState`）：
- [ ] 迁移 `--dry-run` / `--apply` 报告摘要（completed / blocker 计数）：
- [ ] 冒烟页 URL 与 HTTP 状态（发布状态码 / 匿名 GET 状态码 / publication_id）：
- [ ] MCP `tools/list` 联调结果：
- [ ] CLI 主要子命令抽查结果：
- [ ] 与 §7 的差异说明（如有）：
- [ ] 复验结论与签字：
