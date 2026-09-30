# 上游同步说明（Upstream Sync）

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
