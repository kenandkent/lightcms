# 静态导出与 Workers 多账号方案 review / 修正记录

日期：2026-10-19。范围：本次会话提出的静态导出、Workers Static Assets 和多账号方案。基线：`c73c1c8a778857f0f79e7a6f71e86df6d9081b99` 加既存模板工作区。没有运行远端写入，没有修改产品代码、现有配置、模板或部署脚本。

正式修正版：[静态内容导出与 Cloudflare Workers 多账号部署设计](../superpowers/specs/2026-10-19-static-export-cloudflare-workers-design.md)。本记录的“已修正”均指设计规则已修正，不代表代码已实现。

## 发现的问题及修正

| 编号 | 级别 | 原方案缺口 / 当前项目事实 | 修正与验证位置 |
| --- | --- | --- | --- |
| R01 | P1 | 原 CF 服务只有 Zone 清缓存，账号/Token 和 Workers Account 不同 | 新部署适配器与旧服务隔离；设计 §1–2、§4、§9；C01 |
| R02 | P1 | 当前 canonical 是可变投影；简单复制或重新render不能锁定版本 | 创建任务时固定Publication，通过Store.Open校验不可变字节；§7；C02–C03 |
| R03 | P1 | 原服务运行时会给片段套主题，immutable片段不等于完整网页 | v1明确仅支持完整文档，拒绝片段、不用mutable主题补齐；§7.2；C03 |
| R04 | P1 | 图片/icon不是都在同一目录，且有legacy数据库资源 | 定义static/uploads/generated/assets别名与DB解析优先级；§7.3；C04 |
| R05 | P1 | “关联已有Worker并确认覆盖”会破坏已有代码/文件，无法可靠导入私有清单 | 只新建专用Worker或验证归属后恢复；未知Worker不可接管；§6.1；C09 |
| R06 | P1 | 上传文件/上传version不等于已经激活，PUT还可能立即影响线上 | 初始化PUT与普通更新POST version/POST deployment严格分开；§6、§9；C10 |
| R07 | P1 | 单篇上传如果仅传增量manifest会丢失其他报告 | 每次构建完整目标站点release，默认merge，路径ownership冲突409；§8；C11 |
| R08 | P1 | 只依赖本地lease不能阻止超时的旧激活请求稍后覆盖新版本 | durable intent、unknown冻结目标、不盲重试/接管；§11；C12–C14 |
| R09 | P1 | CF激活与Mongo记账非原子；激活成功但公开验证失败不等于旧版本仍在服务 | active_unverified与activation_unknown分开，读回恢复；§11；C13–C14 |
| R10 | P1 | 全局可变Token、仅按Worker名或只按邮箱标识会串账号 | Account ID身份、account+worker唯一、凭证revision/target冻结；§5–6、§11；C09/C12/C17 |
| R11 | P1 | 同一HTML给多个域名会保留错误canonical、OG、JSON-LD及私有资源链接 | export与target化release分离、结构化重写副本；§8；C07 |
| R12 | P1 | mutable资源不能声称已经保存历史截图/图片 | 明确导出时资源快照，exact rollback只针对保留完整包；§7.3、§12；C18 |
| R13 | P2 | 自动HTML handling、同名文件/路由、SPA fallback可能改变URL或把不存在路径返回200 | Worker-first、显式canonical/alias表、关闭自动HTML处理和SPA；§10；C15 |
| R14 | P2 | 账号“验证通过”容易误读为具备写权限；workers.dev子域不能凭空拼接 | 分项只读检查、实际初始化验证写能力、查询子域与显式启用；§5.2、§6.2；C09–C10 |
| R15 | P2 | 凭证存储/轮换/停用未细化；现有API key密文依赖session secret | CF专用主密钥、独立密文revision、不提供reveal、停用不删除远端；§5；C08/C20 |
| R16 | P2 | 既有系统有role/scope/sandbox边界，单纯管理员判断或隐式放大editor会漂移 | 复用UserHasPermission及已有权限组合，403先于副作用；§13；C08 |
| R17 | P2 | 私有包保留、原GC、备份丢失后合并更新可能互相冲突 | 独立私有根和GC、引用保护、丢包阻止更新、恢复需完整备份；§4、§12；C18–C20 |
| R18 | P2 | 免费静态访问的说法不适用于所有Worker-first流量；文件限制未含保留文件 | 明确调用计费/429和每版本完整资源配额；§3、§12；C19 |
| R19 | P2 | 前文称九模板都浏览器自动中英适配，但工作区crypto脚本固定英文 | 撤销已全支持的断言；导出保留当前行为，不将模板修复混入部署；§7.2；C06 |
| R20 | P2 | 账号/目标默认值变更、原CMS撤下、源路径改名容易被误解成自动迁移/撤下 | 身份冻结，双生命周期提示，路径删除显式确认；§1、§8、§12、§14；C11/C17 |
| R21 | P2 | 回滚、重试、幂等、接口/错误码缺少可交付契约 | 独立operations、preview digest、revision、错误码、单篇/全站回滚与retry区别；§11–13；C12–C18 |
| R22 | P2 | “不改运行”可能误读为不能接新路由，或新模块故障必须导致旧系统停机 | 允许附加接线、不改变原行为；新模块故障隔离；§1、§4、§15；C01 |
| R23 | P1 | 官方Direct Uploads指南与自动生成Version API字段清单不完全对应，SDK可能过滤assets | 显式REST metadata、专用Worker先做契约实测、禁止偷偷降级PUT激活；§9；C10；尚需开发后真实验证 |
| R24 | P2 | 只有概念模型，没有数据库唯一索引/大清单限制；retry易被实现为第二任务 | 固定集合字段、索引、单文档CAS、私有清单引用和retry operation/job关系；§4.1、§11、§13；C12–C13 |
| R25 | P2 | 下载ZIP的无扩展名路径/源站绝对资源、异步409及接口缩写仍可误读 | 明确下载包目录/资源重写，202后错误仅在job中，完整列出各preview和提交路径；§7.4、§11、§13；C05/C15 |

## 审查证据

代码只读核对：`internal/services/cloudflare.go`、`cmd/server/publication_runtime.go`、`cmd/server/main.go`、`internal/product/publication/{model,repository,gc}.go`、`internal/product/storage/{store,filesystem}.go`、`internal/handlers/handlers.go`、`internal/services/{asset,apikey}.go`、`internal/database/mongo.go`、`internal/auth/permissions.go`、`config/config_publication_16e_test.go`、九模板生成器和crypto实际HTML、`static/images/`、`docs/TODO.md`。

平台核对来源（官方主文档，核对日2026-10-19）：

- [Direct Uploads](https://developers.cloudflare.com/workers/static-assets/direct-upload/)：manifest、上传session、资源上传、版本附加。
- [Upload Version](https://developers.cloudflare.com/api/resources/workers/subresources/scripts/subresources/versions/methods/create/) / [Create Deployment](https://developers.cloudflare.com/api/resources/workers/subresources/scripts/subresources/deployments/methods/create/)：上传与激活分开。
- [Multipart metadata](https://developers.cloudflare.com/workers/configuration/multipart-upload-metadata/)：assets binding及上传metadata。
- [Configuration and Bindings](https://developers.cloudflare.com/workers/static-assets/binding/) / [HTML handling](https://developers.cloudflare.com/workers/static-assets/routing/advanced/html-handling/)：执行顺序与URL行为。
- [Billing and Limitations](https://developers.cloudflare.com/workers/static-assets/billing-and-limitations/) / [Platform Limits](https://developers.cloudflare.com/workers/platform/limits/)：计费、429、文件数/大小限制。
- [Rollbacks](https://developers.cloudflare.com/workers/versions-and-deployments/rollbacks/)：整Worker影响与历史限制。

## 结论与未完成的验证

设计缺口已经在正式方案中修正；以独立附加模块接入当前项目可行，不需要迁移原服务或改变原发布语义。

没有Cloudflare新功能代码，因此本轮没有把既有测试通过当作新功能证据，也没有执行账号连接、Worker初始化、资源上传、DNS修改或实际激活。当前不能声称“已验证顺利发布”。开发后必须通过C01–C20，尤其真实双Account ID、workers.dev公开地址、CMS离线后可访问这三项。

下一阶段是用户确认此文档后编写逐步实施计划；不是在本轮直接改代码。
