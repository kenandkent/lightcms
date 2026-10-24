# 静态导出与 Cloudflare Workers 代码审查

- 日期：2026-10-19
- 范围：新增 `staticexport`、`staticdelivery`、Worker、后台管理和服务器接线；对照附加设计及18项实施任务。
- 基线：`c73c1c8`，审查包括当前尚未提交的新文件，不仅是 `git diff` 中已跟踪的文件。
- 审查方式：主 agent 自审、行为回归测试、独立测试 MongoDB；没有使用子 agent。
- 结论：**不通过完整功能验收，当前不能宣称可生产发布。** 原因不只是缺少 Cloudflare 测试凭证，关键产品链路仍未实现/接线。

## 1. 完整性核对

| 设计能力 | 代码事实 | 结论 |
| --- | --- | --- |
| 不改变旧部署、Publication及清缓存 | 新包独立；旧文件变更主要为新增后台入口/接线 | 新代码检查符合；旧全套集成回归尚未执行 |
| 固定Publication导出 | 只读源、hash校验、资源闭包、ZIP/单HTML、私有快照已实现 | 组件可测；服务未构造实际Source/export应用入口 |
| 多Cloudflare账号 | 加密凭证、轮换、生命周期与中文后台表单 | 已接线；缺REST、分页、完整管理/审计接口 |
| 专用Worker初始化 | 持久初始化状态、归属检查、占位Worker、公开health | 已有管理入口；未完成设计要求的operation/job幂等接口和完整异步初始化 |
| 完整release合并 | ownership、内容寻址资源、私有来源、现有页面复用、资源保留 | 已实现组件；本轮补充磁盘存储，尚未接入服务器发布链路 |
| Worker访问契约 | 明确路由、GET/HEAD、404/405、ETag、测试noindex、无CMS回源 | 本地Worker测试；未在真实Workers运行时验收 |
| durable发布/恢复 | Mongo lease、target CAS、激活intent、只读恢复核心 | 组件实现；Runner未在服务启动，不存在生产Authorize/Prepare依赖接线 |
| 公开上线校验 | 本轮新增3轮health/GET/HEAD/MIME/hash/cache验证 | 组件实现；删除路径/差异样本、完整任务时限等仍需补齐 |
| 预览/部署/撤下/单篇及全站回滚/地址迁移 | 有PreviewDigest及release合并基础；无CommandService/对应REST和完整后台操作 | **缺失，阻塞交付** |
| retry/cancel | 有取消CAS底层；没有对外授权/幂等命令、attempt历史、凭证替换确认 | **缺失，阻塞交付** |
| 资源解析全部来源 | 磁盘解析已覆盖；legacy Asset DB适配器未注入 | **缺失，影响历史资源导出** |
| 九模板默认分享图与浏览器回归 | 只有Defaults回调；没有九模板默认资源映射及390/1440真实产物网络验收 | **未完成** |
| retention/GC/备份恢复 | 只有release资源保留计算；没有Sweep、引用保护、备份校验/恢复实现 | **缺失，阻塞运维验收** |
| 文档/API交付 | 设计/计划存在；无完整新OpenAPI、使用/恢复runbook、真实发布记录 | **未完成** |
| 真实Cloudflare发布 | 尚未进行真实账号/Worker版本上传、激活、离线访问及双账号验收 | **未验证，不能用mock替代** |

## 2. 已复现并修复的问题

下列修复均有行为测试，不以存在某个函数作为验收。

| 编号 | 级别 | 问题及影响 | 修复/证据 |
| --- | --- | --- | --- |
| F01 | P1 | export包错误进入delivery后丢失业务code，410变500 | `TestReviewExportErrorsPreserveHTTPCode` |
| F02 | P1 | 退出资源默认保留90天，与设计固定7天不一致 | 默认改为7天；`TestReviewRetiredAssetsDefaultToSevenDays` |
| F03 | P1 | verifying遇到凭证错误尝试非法转换为unknown，恢复失败 | 保留原恢复状态和owner并延后只读核实；`TestReviewRecoveryCredentialFailureStaysUncertain` |
| F04 | P1 | 发布合并重做未变页面，可能读取新的默认图等mutable值 | 同origin/environment未变页面直接复制原字节/依赖；`TestReviewMergeKeepsUnchangedPageBytes` |
| F05 | P1 | 当前release丢文件时可被静默重建，掩盖不可变产物丢失 | 先验证当前完整文件闭包；`TestReviewMissingCurrentFileCannotBeRebuiltAsEmpty` |
| F06 | P1 | JSON-LD将publisher/author身份地址改为文章地址 | 保留身份/来源节点，仅重写文章/图像URL；`TestReviewJSONLDDoesNotRewritePublisherIdentity` |
| F07 | P2 | JSON-LD尾随第二个JSON值被悄悄忽略 | 必须单JSON值；`TestReviewJSONLDRejectsTrailingJSON` |
| F08 | P1 | 生产分享图接受data URL，社交爬虫不可使用 | 明确422拒绝；`TestReviewDataSocialImageRejected` |
| F09 | P1 | 激活前仅检查version，忽略同version配置/代码漂移 | 保存并比较settings/code hash，标记target drifted；`TestReviewRunnerBlocksSettingsDriftBeforeUpload` |
| F10 | P1 | remote检查期间账号停用后仍可能写activation intent并激活 | account/target/job同事务建立intent，作为停用/取消竞争的线性化点；`TestReviewDisabledAccountCannotCrossActivationIntent` |
| F11 | P1 | Worker绑定身份没有release ID，无法核对新版本归属 | 从冻结routes.json核实target/release并生成binding marker；`TestReviewVersionIdentityIncludesFrozenRelease` |
| F12 | P2 | 读取接口网络错误不执行约定重试 | 只对允许重试请求按上限重试，激活仍不重发；`TestReviewReadsRetryTransportFailures` |
| F13 | P2 | HTTP默认45秒，与设计30秒不一致 | `TestReviewDefaultTimeoutMatchesSpec` |
| F14 | P1 | 只有内存ReleaseStore，重启无法读取部署及回滚快照 | 新增私有磁盘Store，完整来源/公开文件hash验证、临时目录提交、重放冲突检测；`TestReviewReleaseStoreSurvivesRestartAndRejectsTamper` |
| F15 | P1 | 损坏的prepared release会再次调用Prepare重建 | 仅不存在允许构建，损坏必须阻塞；`TestReviewCorruptPreparedReleaseIsNotRebuilt` |
| F16 | P1 | 没有真实公开响应校验器 | 新增ReleaseVerifier，连续3轮/2秒、health、所有文件GET及页面HEAD、hash/MIME/ETag/cache；`TestReviewVerifierChecksThreeRoundsAndRejectsWrongBytes` |
| F17 | P1 | Worker过宽前缀拒绝 `/_lc-assets-report` 等合法页面，可能导致Worker启动失败 | 保留路径按完整路径/子路径判断；Worker用例 `a similar name is not a reserved path prefix` |
| F18 | P2 | 仅有og:title时仍拒绝合法报告，不符合标题回退规则 | `TestReviewMetadataUsesExistingOGTitle` |
| F19 | P1 | 已存在的release目录缺少completion/私有快照时返回NotExist，Runner可能误当首次构建 | 仅目录不存在允许准备，已安装包不完整返回410；`TestReviewIncompleteReleaseIsNotMissing` |
| F20 | P1 | 不确定激活的恢复发现外部版本覆盖时，目标仍显示ready | 非原版本/候选版本标记drifted且保留owner，不重复激活；`TestReviewRecoveryMarksForeignVersionDrifted` |
| F21 | P1 | uploading恢复时缺少Store会直接panic，可能中断工作进程 | 上传/激活前缺少持久存储明确blocked；`TestReviewMissingStoreBlocksBeforeUpload` |

实现说明：磁盘release存储暂以256MiB公开字节和512MiB私有快照作为内存实现的防护上限，超出明确失败而非截断站点；这不等同支持Cloudflare套餐的全部最大容量，正式运维文档需说明，后续可换流式磁盘实现。

未作为bug修改：`srcset`中不带空白的逗号可能是URL本身的一部分，不将所有逗号机械切开；正常候选分隔仍有用例。不能以一个过宽的“修复”破坏合法URL。

## 3. 剩余阻塞项与处理顺序

1. 补齐CommandService、冻结operation→job/export的提交事务及全量REST/OpenAPI。复用现有认证链，禁止先写入后403。
2. 注入现有Publication Store、legacy Asset解析器、默认资源快照；将FileReleaseStore、ReleaseVerifier、原操作者重新授权、固定凭证revision、任务超时和Runner接入服务生命周期。
3. 补齐初始化幂等/异步命令、撤下、单篇及整站回滚、retry attempt历史、地址迁移、删除路径验证及drift的管理员处置/恢复入口。
4. 补齐后台内容入口、预览影响清单、确认、进度、历史、下载/访问链接、逐目标状态及“不联动CMS撤下”提示。
5. 完成新集合/私有文件的引用保护GC、备份恢复、主密钥轮换流程、审计、全部列表分页。大bucket当前仍构造内存multipart，需按设计改流式上传或明确限制。
6. 执行九模板真实发布产物浏览器/网络回归、旧全套集成测试、真实Workers运行时及两个账号的专用目标验收。

这些不是需要用户重新选择的产品需求，而是当前实现未完成项。现有代码不能因局部测试通过被标记为18项任务已完成。

## 4. 测试证据

- 测试Mongo：仅使用独立 `static-delivery-test-<ObjectID>` 数据库，测试后删除自己创建的库；未清空开发/生产库。
- 本轮每个缺陷的失败与修复后通过已在会话工具输出保留。
- 新包完整race测试通过：`go test -race -p 1 ./internal/product/staticdelivery/... ./internal/product/staticexport ./workers/report-static -count=1`，四个包分别8.606s、1.204s、1.406s、1.128s。Mongo集成使用强制环境标志，数据库不可用会失败，不会skip-green。
- Worker JavaScript路由测试10/10通过；无失败、无skip。
- server/handler新增功能相关筛选race测试通过。筛选不代表旧系统全套回归；Worker Go嵌入测试已在上面的完整新包测试中单独执行。
- 全项目 `go test -p 1 ./... -run '^$'` 编译检查及 `go vet ./...` 通过。编译检查没有执行旧测试，不能当作全量测试通过。
- 日志保存在项目内 `.superpowers/sdd/2026-10-19-static-export-cloudflare-workers/review-*.log`。
- 未执行旧全套数据库集成回归：旧测试辅助函数会无条件加载现有`.env.test`并清空其指向的开发库，尚未建立安全的隔离运行副本；不以覆盖环境变量冒险执行。浏览器/真实Cloudflare验证也不计入通过。
- 未提交、未推送、未修改Cloudflare远端、DNS、旧CI或旧部署脚本。

## 5. 提交前复验（用户后续授权提交及推送）

2026-10-19重新验证，不沿用第4节的旧结果：

- 新模块完整race测试通过：staticdelivery 7.573s、cloudflare 1.417s、staticexport 1.609s、Worker Go 1.343s；独立Mongo集成要求启用，不允许数据库缺失时跳过。
- server/handler的3项新增后台测试通过，race模式分别1.484s、1.559s。
- Worker JavaScript 10/10通过，零skip。
- 全项目 `go build ./...` 通过；`go vet ./...`、`git diff --check`通过。
- 旧数据库全套集成、浏览器和真实Cloudflare发布仍未执行，第1/3节的功能缺口仍然存在。此次提交不是完整功能验收或生产部署。
- 用户已明确授权提交并推送当前代码到自己的GitHub仓库；该授权不包含Cloudflare远端上传/激活、DNS修改或现有服务部署。
