# 上游同步说明（Upstream Sync）

## 2026-10-24 同步记录

本次以开源项目 `metavert-io/lightcms` 的 `main` 提交
`81707877bb8d29dd74cf79c8b919a91c0a3aa637`（7.4.5）为基线，处理
`cloudflare-worker` 提交 `f9d63ae9be9eeef72b94ef122f82038ce8b42581`
合入 `main` 的 squash 冲突。以下旧章节保留为历史记录，不再代表当前模块路径或冻结状态。

- Go 模块与所有内部导入统一使用 `github.com/metavert-io/lightcms/v7`。
- 保留上游的集中后台路由权限、会话 API CSRF、独立页面的 SEO、订阅源、
  IndexNow、暂缓发布以及分支副本隔离修复；二开后台入口加入同一权限表。
- 保留本项目模板版本、Publication、Cloudflare 多账号及 Worker 静态发布、
  页面维护、任务中心、运行状态、对象保留和清理能力。草稿保存、替换、重命名、
  删除及恢复不直接重写或删除 canonical 文件；正式运行时仍由 Publication 完成切换。
  上游测试中依赖隐式重新生成的断言已按该既有边界调整，保留副本隔离检查。
- 分支合并默认只形成草稿；显式 `publish_new` 仅发布本次新建且未暂缓的页面，
  发布选择、操作者及版本固定在持久化任务中，通过现有发布入口执行；临时失败重试同一
  发布操作，不改发后续草稿，业务拒绝返回未发布明细。仅清理与检查点一致的副本，
  提交后新增或修改的副本保留并报告部分完成，保留计数及任务检查点。
- 搜索引擎通知接在 Publication 提交后的回调，公开内容修改时间仅在 HTML 哈希
  改变时更新。模板和内容草稿变更不会借上游旧的重新生成路径绕过发布记录。
- 本项目 CI/部署继续使用 `.github/workflows/deploy.yml` 和现有 Compose 配置。
  不恢复向上游组织镜像仓库推送的 `publish-mcp-image.yml`，不启用 Fly.io 部署。
- 本次合并不改变任何生产内容、Cloudflare 资源、域名绑定或 Token，
  不在本机打开内容页面。回归仅使用本地独立测试库和测试夹具。

合并验证：`go test -p 1 ./... -count=1` 的 42 个测试包通过；
首页 SEO 用例因独立测试库没有首页夹具按原逻辑跳过。最后的收尾并发修复后，
`internal/services` 完整重跑通过（524 项测试及子测试，无跳过）。
`go build ./...`、`go vet ./...` 和 MCP 二进制重建通过。
Worker 的 10 项 JavaScript 测试及 legacy/assets-only 两种原生静态资源路由夹具均通过。

> 配套文档：[README-product.md](README-product.md)（产品总览）·
> [DEPLOYMENT-PRODUCTION.md](DEPLOYMENT-PRODUCTION.md)（生产部署）·
> [UPGRADE.md](UPGRADE.md)（升级）

本文档按仓库现状如实记录 fork/同步策略。本节为**占位说明**：当前仓库尚未配置上游同步机制，以下是现状盘点与推荐策略，待团队确认后细化成操作步骤。

## 1. 仓库现状（2026-09-30）

- 本地 remote 仅 `origin`：`ssh://git@ssh.github.com:443/kenandkent/lightcms.git`（fetch/push 同一地址），**没有配置 `upstream` remote**。
- Go module 路径为 `github.com/jonradoff/lightcms/v7`（`go.mod`），与 origin 的 `kenandkent/lightcms` 组织路径**不一致**——这是 fork 特征（上游疑似 `jonradoff/lightcms`，但尚未在 git remote 中声明，**不要假设**，以团队确认为准）。
- 当前分支 `task/18d-proddeploy` 基于 `main @ 5c59494`；Task 18 系列文档任务并行工作在多个 worktree/分支，最终由集成 owner 合并（plan §3 交接边界：并行 worker 移交已评审提交，集成 owner 独占共享装配文件）。
- V3 发布系统处于 MVP 收尾（Task 18 文档 + Task 19 发布演练），此时上游同步策略以**冻结基线、只进不出**为原则（见 §3）。

## 2. 推荐同步策略（待确认）

```bash
# 以下为推荐占位步骤，执行前需团队确认 upstream 地址
git remote add upstream <UPSTREAM_URL>   # 候选：上游 jonradoff/lightcms（待确认）
git fetch upstream
git log --oneline main..upstream/main    # 审阅上游增量
git checkout -b sync/upstream-<DATE> main
git merge upstream/main                  # 或 rebase，按团队既有习惯（二选一，固定下来）
go build ./... && go test -p 1 ./...     # 全量回归（DB 测试需 replica-set fixture）
```

- 同步频率：V3 MVP 发布前**不主动同步上游**（避免基线漂移影响 Task 19 验收）；发布后按固定节奏（如双周）同步一次，每次同步单独分支 + 全量回归 + 迁移 `--dry-run` 复核。
- 冲突面预期：`internal/product/**`（V3 新域，上游大概率无冲突）、`config/config.go`、`cmd/server/main.go`（装配区，冲突高发，交由集成 owner 裁决）、`internal/handlers/*`（Admin/legacy 面）。
- Module 路径注意：若 fork 改名（按 CLAUDE.md 要求 `sed` 替换 module path），同步上游时必须保留本 fork 的 module 路径，上游文件原样合入后重新替换，不得把上游 module 路径带进来。

## 3. 当前冻结事项

- 不要在 Task 19 signoff 之前引入上游功能变更；安全补丁例外（走 hotfix 分支，单独回归 + 提前演练迁移）。
- 同步操作本身不改变 `system_migrations.publication_model_v1` flag；但同步后若涉及 migration/索引代码，必须重跑 `--dry-run` 确认报告语义未变（见 [DEPLOYMENT-PRODUCTION.md](DEPLOYMENT-PRODUCTION.md) §4）。
- 每次同步记录：upstream SHA、product SHA、回归结果、迁移复核结论，写入发布报告（Task 19 口径）。
