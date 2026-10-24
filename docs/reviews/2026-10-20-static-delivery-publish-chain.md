# 已发布内容 → Cloudflare Worker 发布链路实现与 review

日期：2026-10-20。基线：`8528881773123a484712f7ce42fb2d82dcbc7600`。实现分支：`codex/cloudflare-content-publish`；本文是本地工作区验收记录，不是已推送/已部署证明。

## 本次目标与结论

补齐已有已发布文章的用户可操作发布主链路，而非继续仅展示账号配置。主链路的服务、接口、后台入口和持久队列已连接；本地集成及浏览器交互测试通过。没有发现本次主链路中尚未修复的已知代码阻断项。**真实 Cloudflare 发布仍需在部署新版服务后，用管理员指定的测试报告进行实测；不能据本地结果宣称远端发布成功。**

原设计18项任务并未因此全部完成：远端撤下、回滚、手动 retry、origin migration、自动私有文件 GC、完整审计/备份工具及九模板真实远端验收仍不应标为完成。本次未改变原 CMS 生命周期，也未修改 DNS、原 Cloudflare 清缓存服务或原端口。

## 覆盖检查

| 环节 | 实现与证据 |
| --- | --- |
| 已有文章选择 | 内容列表勾选已发布文章、编辑页单篇入口、最多100篇、非分支/未删除检查 |
| 固定版本 | CommandService CAS 冻结 Publication 元数据；同键重试不重新选择最新版本；BSON 保留私有 actor snapshot |
| 资源和下载 | 原 Publication store 只读；资源采集、legacy Asset adapter、完整包校验；ZIP与受限单HTML下载 |
| 账号/Worker选择 | enabled账号、ready目标、安全DTO；繁忙目标可看记录但不可并发发布 |
| 预览和确认 | 完整站点候选包先冻结；摘要绑定owner/target/精确请求，一小时有效；人工脚本确认；预览不触发远端激活 |
| 持久提交 | 幂等 operation/job、事务 ClaimTarget、固定凭证 revision；重复提交和成功后重放返回同一job |
| 远端发布 | 独立Remote/client，完整资源→未激活version→durable activation intent→激活→公开校验；原CF清缓存服务不参与 |
| 更新不丢文 | A→B→更新A的命令集成测试确认B保留；不同内容抢占路径按原release规则拒绝 |
| 状态和恢复 | 任务轮询、刷新后恢复、目标历史、成功链接；unknown不重复激活；公开验证通过才succeeded |
| 取消与权限 | 激活前CAS取消、幂等取消；不支持把cancel当撤下；当前user/role/key scopes执行前再检查；unknown只读恢复 |
| 启动接线 | 真实Publication repo/store、source lookup、后台worker集成测试成功导出，原内容published保持不变 |
| 持久性 | Mongo保存安装身份并沿用已有唯一target身份；私有目录专用生产卷；不要求新Token加密密钥 |

## review 发现并已修复

1. **原主链路未连接：**补 CommandService、REST和session/CSRF接口、runner export分支、server启动接线和内容管理入口。
2. **执行身份可能丢失：**执行快照使用BSON而非会省略 `json:"-"` actor 的JSON；后台在上传/激活边缘重新核实权限。
3. **容器重建可能丢失安装身份/候选包：**Mongo身份权威、沿用已有target identity；新增私有数据卷，文档明确首次迁移注意事项。
4. **预览后重新构建可能漂移：**预览即冻结完整release，确认后只读取该release，丢包即阻塞，不从mutable源重新生成。
5. **遗漏 revision 会被当0：**HTTP显式区分 missing/null 与合法0，缺前置条件返回428。
6. **繁忙目标无法查看/取消任务：**目标允许选择查看历史，禁用的是新发布，不是历史入口；增加无文章选择时的记录入口。
7. **成功release补写不完整：**target提交为权威，随后记录release committed；崩溃后可从已提交target幂等补写。
8. **并发/超时配置未进入runner：**接入配置、每账号最多2个执行slot；激活前超时阻塞，激活后保留不确定性并只读恢复。
9. **底层错误直接返回：**未知内部错误返回统一消息，原始数据库/私有文件错误不直接泄漏到操作页。

## 实际验证结果

使用缓存Go1.25.13，测试临时文件/cache均在项目内。Mongo仅使用随机名 `static-delivery-test-<ObjectID>` 专用库，测试后删除自己创建的库；未清理开发或生产数据库。

- `go test -race -p 1 ./internal/product/staticdelivery/... ./internal/product/staticexport ./workers/report-static -count=1`：通过，真实隔离Mongo门槛开启（不是skip-green）。包括冻结/重放、篡改/过期确认、完整站点保留、取消、超时、安装身份升级、CF协议模拟和公开TLS校验。
- `go test -race -p 1 ./cmd/server ./internal/handlers -run 'TestStaticDelivery|TestAdminStatic' -count=1`：通过，包含真实启动接线导出、权限撤销、后台模板和渲染后JS语法检查。
- `TestHTTPPublishedContentToDeploymentAndDownload`：真实Mongo+真实命令服务+router请求→导出ZIP/HTML→预览→确认→worker执行→状态/链接→同键重放，通过；此测试的Cloudflare管理接口和verify回调为模拟。
- `TestCommandsFrozenExportPreviewAndDurableDeployment`：Cloudflare管理接口模拟，公开验证走真实TLS测试服务，校验三轮health/GET/HEAD/MIME/ETag/缓存/字节；通过。独立Worker JS测试验证实际路由逻辑。
- 浏览器测试：渲染后的后台页面执行“导出→账号/Worker→预览→确认→链接”，无pageerror，仅一次部署提交，390px无横向溢出；另测繁忙目标查看记录→取消→状态恢复通过。浏览器测试接口为模拟，不冒充生产服务。
- Worker Node测试：10/10通过。
- `go build ./...`、`go vet ./...`、全项目 `go test -p 1 ./... -run '^$'` 编译门槛：通过。最后一项只编译，不代表全项目所有测试执行。
- OpenAPI及生产Compose YAML解析、`git diff --check`：通过。

## 未执行/尚未证明

- 未操作真实Cloudflare API发布报告；未读取数据库中的Token。真实账号权限、平台当前返回格式、实际workers.dev传播和社交抓取效果须由专用测试目标发布验证。
- 未执行旧项目整套数据库测试：旧helper会从 `.env.test` 加载并清空共享开发测试库。本次没有为凑全绿运行可能删除现有数据的命令。
- 未提交/推送/替换本地运行进程或生产服务。生产需先发布本分支代码、确认私有卷和身份迁移，再按操作指南做真实报告验收。

操作步骤：[STATIC-DELIVERY.md](../STATIC-DELIVERY.md)。运维边界：[STATIC-DELIVERY-RUNBOOK.md](../STATIC-DELIVERY-RUNBOOK.md)。
