# 静态导出与 Cloudflare Workers 多账号部署 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. 本项目用户已指定由主 agent 直接处理，禁止使用子 agent；不要采用 subagent-driven-development。

**Goal:** 在不改变当前系统部署、运行及原发布行为的前提下，增加完整HTML导出、多Cloudflare账号/Worker管理、独立部署与恢复。

**Architecture:** `staticexport` 只读取既有Publication及资源，生成私有不可变导出包。`staticdelivery` 管理账号、目标、站点release、durable jobs；独立Cloudflare REST适配器上传版本后再激活。后台和REST共用同一应用服务，原Publication runtime只提供只读适配器。

**Tech Stack:** 既有Go 1.25、Mongo driver v1、Gorilla mux/session/CSRF、golang.org/x/net/html、现有CSS解析依赖；精简ES module Worker；Node仅用于Worker测试，不成为CMS运行依赖。

**Spec:** [2026-10-19-static-export-cloudflare-workers-design.md](../specs/2026-10-19-static-export-cloudflare-workers-design.md)。执行前完整阅读该文及[修正记录](../../reviews/2026-10-19-static-export-cloudflare-workers-review.md)。设计为行为依据，本计划固定文件归属、接口、执行顺序与验证方式。

## Global Constraints

- 项目边界 `/Users/ken/workspace/newsPage`；项目外只读，除非用户明确授权具体路径。所有新临时文件、测试数据库辅助文件、浏览器截图和构建输出保存在项目内。
- 原部署脚本、CI、provider、BASE_URL/PUBLIC_BASE_URL、清缓存服务、Publication状态、旧webhook及原publish/unpublish调用链行为不变。
- 不自动部署内容、不自动删除远端Worker、不接管未知Worker、不自动改DNS；一次选择一个目标。
- 用户已同意进入实现准备，但本文需先经用户确认后才执行产品代码；执行方式固定为当前会话主agent直接实施。
- 缺Cloudflare凭证不影响静态导出和原服务启动；缺真实账号不能把mock通过称为远端验收通过。
- 完整HTML、已验证Publication、固定版本、资源闭包；不使用当前mutable模板重渲染补导出，不修改原pin或GC。
- 所有幂等/并发/恢复规则以设计§11–13为准：未知激活不自动重试或释放目标；单篇部署合并完整目标站点。
- 默认导出根 `var/static-delivery`、全局并发2、每账号上传并发2、历史90天、失败临时文件7天、退出资源保留7天、任务30分钟、HTTP每请求30秒、最多5次有界重试。
- 测试使用独立名字包含 `test` 的数据库，不调用清空开发或生产库的helper；数据库用例必须串行 `-p 1`，集成门槛不得skip-green。
- 用户未要求本次提交、推送或真实生产部署。各任务完成后可形成独立提交清单，只有取得相应指示才提交/推送；不得把用户其他变更混进提交。

## Review Focus

1. API重放时CMS已经出现新Publication：必须返回首次固定版本，不重新选择最新；Task 2/5/15增加明确断言。
2. `news/a`不是目录、资源可能包含中文与query：导出下载包和Worker地址须按各自规则可用，不能错解相对图片；Task 3/4/9增加输入矩阵。
3. Token能读某个Worker但不能列全部Worker：允许有诊断的手工目标核验，不将其误报成无效账号；Task 6/7增加403-list/200-detail分支。
4. CF已切换但本地落库前断电，随后账号被停用：只读恢复记账，不再发激活；Task 10/11增加故障注入。
5. 不断发布会不断刷新旧资源保留时间：首次退出时间必须继承，最终可回收而非永久累积；Task 8/13使用固定时钟测试。

## 0. 文件与接口地图

所有路径相对于项目根。新文件只在所属任务实现时创建，禁止先生成大量空壳后标记完成。

| 任务 | 归属文件（新建，除注明修改） | 对外契约 |
| --- | --- | --- |
| 1 | `internal/product/staticdelivery/model.go`, `errors.go`, `config.go`；`internal/product/staticexport/model.go` | 设计§4.1模型、§13错误；`LoadConfig(projectRoot string) (Config,error)` |
| 2 | `staticdelivery/repository.go`, `operations.go`, `indexes.go`, `repository_test.go` | `NewRepository(db *database.DB) *Repository`；单文档CAS及幂等 |
| 3 | `internal/product/staticexport/source.go`, `source_test.go`；`cmd/server/static_delivery_source.go` | `Source.Resolve(ctx,Item)(PageSource,error)`、`Source.Open(ctx,PageSource)(io.ReadCloser,error)` |
| 4 | `staticexport/dependencies.go`, `html.go`, `css.go`, 对应测试 | `Collect(ctx,PageSource,[]byte,Resolver)(Document,error)` |
| 5 | `staticexport/archive.go`, `service.go`, `archive_test.go` | `NewService(Source,Resolver,Options) *Service`；`Service.Build(ctx,FrozenExport)(Artifact,error)` |
| 6 | `staticdelivery/cloudflare/client.go`, `assets.go`, `versions.go`, `client_test.go`；父包`cloudflare_adapter.go` | 每任务独立`NewClient(token string,opts Options) *Client`；父包适配器实现Remote |
| 7 | `staticdelivery/accounts.go`, `targets.go`、对应测试；`vault.go`仅兼容历史revision | AccountService/TargetService不依赖密钥；新Token明文存库且不回显 |
| 8 | `staticdelivery/release.go`, `rewrite.go`, `preview.go`, 对应测试 | `BuildRelease(ctx,BuildInput)(ReleaseManifest,error)`、`PreviewDigest(Preview) string` |
| 9 | `workers/report-static/bundle.go`, `src/index.js`, `src/handler.mjs`, `test/handler.test.mjs` | `reportstatic.Modules(routeJSON []byte) ([]Module,error)`；JS `createHandler(routes)` |
| 10 | `staticdelivery/runner.go`, `activation.go`, `job_test.go` | `Runner.RunOnce(ctx) error`、`Runner.Run(ctx)`；激活前durable intent |
| 11 | `staticdelivery/verify.go`, `reconcile.go`, `recovery_test.go` | `Verifier.Check(ctx,ReleaseManifest)(Verification,error)`；`Reconcile(ctx,jobID) error` |
| 12 | `staticdelivery/commands.go`, `rollback.go`, `origin.go`, `commands_test.go` | 预览与命令提交、撤下、回滚、retry/cancel、地址迁移 |
| 13 | `staticdelivery/retention.go`, `backup.go`, `retention_test.go` | `Sweep(ctx,now time.Time)(SweepReport,error)`；备份清单与校验 |
| 14 | `staticdelivery/httpapi/handler.go`, `accounts.go`, `exports.go`, `deployments.go`, `handler_test.go` | `Register(r *mux.Router,h *Handlers)`；全部设计§13路由 |
| 15 | 新 `cmd/server/static_delivery_runtime.go`, `_test.go`；修改`cmd/server/main.go`的新增接线 | `buildStaticDeliveryRuntime(ctx,db,publicationRuntime,Config)`；故障隔离 |
| 16 | 新`internal/handlers/admin_static_delivery.go`, `admin_static_delivery_templates.go`, `_test.go`；修改既有Handler字段/导航/内容页入口 | 复用原renderAdmin与CSRF；管理/导出/部署/历史界面 |
| 17 | `internal/product/staticdelivery/e2e/*_test.go`, `testdata/`；`workers/report-static/package.json` | 本地全链路、九模板、可选真实CF契约门槛 |
| 18 | `docs/openapi/static-delivery-v1.yaml`, `docs/STATIC-DELIVERY.md`, `docs/STATIC-DELIVERY-RUNBOOK.md`；追加`docs/API.md`索引 | API/使用/配置/备份/验收证据 |

表中 `staticdelivery/...` 和 `staticexport/...` 分别展开为 `internal/product/staticdelivery/...` 和 `internal/product/staticexport/...`。每个实现文件须保持单一职责，不把所有REST、队列和HTML处理塞入一个service.go。

### 核心共享类型与依赖方向

`staticexport` 不依赖 `staticdelivery`；`cloudflare` 不依赖其父包，避免循环导入。`staticdelivery` 使用接口适配Cloudflare返回值。`httpapi`依赖应用服务；`cmd/server`负责旧项目只读适配器。

```go
// internal/product/staticexport/model.go
type Item struct {
    ContentID primitive.ObjectID `json:"content_id"`
    PublicationID primitive.ObjectID `json:"publication_id,omitempty"`
}
type PageSource struct {
    ContentID, PublicationID, TemplateVersionID primitive.ObjectID
    ContentVersion, TemplateVersion int64
    FullPath, PublicURL, TemplateSlug, SourceSHA256 string
    LogicalPublishedAt time.Time
    StoragePath string // 私有字段，不进入公共manifest
}
type Source interface {
    Resolve(context.Context, Item) (PageSource, error)
    Open(context.Context, PageSource) (io.ReadCloser, error)
}
type Resolver interface {
    Resolve(context.Context, string) (Resource, error)
}
type Resource struct {
    URL, MIME, SHA256, SourceRevision string
    Data []byte
}
```

`FrozenExport`固定ID、PageSource切片、捕获时间；`Artifact`固定ID、manifest、私有相对路径和portability。`Document`包含保留原script/style的HTML、资源引用图、external与诊断；具体字段与JSON/BSON标签在Task 1一并声明，不能后续任务各自定义同名结构。

`ReleaseManifest`是完整页面/文件/alias表，ID、TargetID、Origin、Revision、ConfigRevision、SourceExports、WorkerProtocol、文件hash与retained_until齐全。`Job`、`Operation`及账号模型逐字段来自设计§4.1，不复用原Publication作为远端部署记录。

```go
// internal/product/staticdelivery/remote.go，随Task 1创建
type Remote interface {
    Inspect(context.Context, Target) (RemoteState, error)
    Initialize(context.Context, Target, []Module) (RemoteVersion, error)
    UploadAssets(context.Context, Target, []UploadFile) (CompletionToken, error)
    UploadVersion(context.Context, Target, CompletionToken, []Module) (RemoteVersion, error)
    Activate(context.Context, Target, string) (RemoteDeployment, error)
}
```

以上参数的RemoteState、RemoteVersion、Module、UploadFile、CompletionToken、RemoteDeployment由Task 1在父包定义：RemoteState含ownership、settings、100%版本与deployment ID；RemoteVersion/RemoteDeployment含远端字符串ID和annotations；Module含Name/MIME/Data；UploadFile含PublicPath/MIME/SHA256/Data；CompletionToken为不允许JSON序列化的私有值。Task 6通过父包适配器调用无父包依赖的cloudflare.Client，不能让cloudflare反向import父包。

### 每个任务的固定执行节奏

1. 先创建该任务列出的测试，使用下面给出的断言或输入矩阵。
2. 运行限定测试，记录真实失败原因；必须是目标行为缺失，而非导入路径或测试语法错误。
3. 实现使这些测试通过的代码；保留原行为，不让测试调用假返回值绕过真实逻辑。
4. 跑该任务测试、受影响旧包测试和`git diff --check`，在本计划勾选完成并写测试结果。
5. 自行review差异，仅标记已验证的交付面；未执行的CF真实测试保留未通过标记，不伪造。

---

## Task 1：冻结模型、配置、错误与状态机

**依赖：** 无。**验收：** C01、C08、C12基础。

- [ ] 创建地图中Task 1的模型、所有状态/错误码、ID序列化；新增`remote.go`中的契约类型。共用ObjectID/UTC时间/int64 revision，私有字段全部`json:"-"`。
- [ ] 写`config_test.go`验证默认值、项目外root、static/generated下root、无主密钥、错误主密钥。写`model_test.go`验证unknown不能直接failed/succeeded、cancel不能越过激活intent。
- [ ] 运行`go test ./internal/product/staticdelivery -run 'Test(Config|JobTransition)' -count=1`，确认红灯，再实现并跑至通过。
- [ ] `LoadConfig`读取新增模块可选环境，不改原`config.ValidatePublicationConfig`；无密钥只禁用凭证操作，不禁用导出。

核心红灯断言及实现规则：

```go
func TestJobTransitionRejectsUnknownAsFailure(t *testing.T) {
    if ValidJobTransition(JobActivationUnknown, JobFailed) {
        t.Fatal("uncertain remote activation cannot become ordinary failure")
    }
}
// ValidJobTransition(from,to JobState) bool 使用显式允许边；
// unknown仅通过reconcile进入verifying/active_unverified/blocked，
// 最终成功必须经过公开校验与target CAS，不能仅改一个枚举。
```

## Task 2：新增集合、幂等预留、单文档CAS与测试数据库

**依赖：** 1。**验收：** C01、C02、C12、C13。

- [ ] 创建`indexes.go`，准确创建设计§4.1七个集合的唯一/查询索引；不修改原EnsureProductIndexes。
- [ ] 创建`repository_test.go`的独立`connectDeliveryTestDB(t)`：读取MONGODB_URI，要求DATABASE_NAME包含test；使用`static-delivery-test-`命名空间/专用数据库；缺变量在release测试模式失败，而非skip。
- [ ] 定义并测试`ReserveOperation(ctx,Operation)(Operation,bool,error)`、`ClaimTarget(ctx,targetID,jobID,expectedRevision) error`、`CommitTarget(ctx,targetID,jobID,releaseID,expectedRevision) error`、`EnsureIndexes(ctx) error`。
- [ ] 运行`go test -p 1 ./internal/product/staticdelivery -run 'TestRepository|TestOperation' -count=1`先失败再通过。覆盖50个相同key并发、同key不同body、预分配ID后崩溃、另一个目标可独立领取。
- [ ] 初始化operations先写唯一身份，再由预分配ID补建export/job；retry operation引用原job，不插入第二job；target单文档提交即保护release，不依赖job状态补写。

```go
func TestOperationRequestHashDoesNotIncludeLatestPublication(t *testing.T) {
    a := OperationRequest{Owner:"user:1",Action:"export",Key:"same",Body:[]byte(`{"items":[{"content_id":"000000000000000000000001"}]}`)}
    b := a
    if OperationRequestHash(a) != OperationRequestHash(b) {
        t.Fatal("same client request must reserve same operation")
    }
}
// OperationRequest及OperationRequestHash在operations.go定义；
// 服务端冻结的Publication存operation.snapshot，不参与客户端request hash。
```

## Task 3：原Publication只读源适配器

**依赖：** 1–2。**验收：** C02、C03。

- [ ] 实现cmd/server只读适配器，使用既有PubRepo.GetActive/GetByID和rt.Store.Open。Content只检查fork/软删除，template slug来自不可变TemplateVersion，不用当前模板冒充旧slug。
- [ ] 为source_test.go建立内存Source及Spy：原Publish/Stage/Activate方法若被调用则立即失败；只读适配器集成测试检查原集合/文件hash不变。
- [ ] 测试矩阵：active/superseded verified present允许；failed/staged/unpublished/legacy_unverified/fork/deleted拒绝；pub不属于content拒绝；源缺失410；hash不一致失败。
- [ ] 运行`go test ./internal/product/staticexport -run TestSource -count=1`及`go test -p 1 ./cmd/server -run TestStaticDeliverySource -count=1`先红再绿。
- [ ] API预留operation后固定PageSource，异步执行不再GetActive；资源GC竞态必须失败，不fallback canonical。

```go
func TestSourceSourceHashIsFrozen(t *testing.T) {
    p := PageSource{SourceSHA256:strings.Repeat("a",64),FullPath:"/news/a"}
    frozen := p
    p.SourceSHA256 = strings.Repeat("b",64)
    if frozen.SourceSHA256 == p.SourceSHA256 { t.Fatal("snapshot changed") }
}
// 另一个Mongo测试必须真正创建新Publication后重放API，确认返回旧ID；
// 不能仅凭此值复制单测声称C02完成。
```

## Task 4：HTML/CSS依赖闭包、资源路径与原字节保留

**依赖：** 3。**验收：** C03、C04、C07。

- [ ] 实现文档级检测、HTML tokenizer定位资源属性并保留script/style原始字节；不使用Parse+Render重新序列化所有脚本。
- [ ] Resolver按设计§7.3解析static、uploads、assets两种磁盘候选及legacy DB Data；拒绝越界symlink和原sidecar；读取前后变更重试有上限。
- [ ] 收集src/srcset/poster、resource href、CSS url/@import、JSON-LD/OG/icon；普通a链接不下载；data保留；external渲染依赖列为incomplete。
- [ ] 红绿运行`go test ./internal/product/staticexport -run 'Test(Document|Dependency|Relative|Resolver|CSS)' -count=1`；覆盖UTF-8路径、空src、协议相对URL、CSS环、JS动态依赖警告。

```go
func TestRelativeURLUsesOriginalPageNotDirectory(t *testing.T) {
    got,err := ResolveReference("https://cms.example/news/a","chart.png")
    if err != nil || got != "https://cms.example/news/chart.png" {
        t.Fatalf("resolved=%q err=%v",got,err)
    }
}
// ResolveReference(base,ref string)(string,error) 在dependencies.go定义；
// 使用net/url.ResolveReference，不用filepath.Join处理URL。
```

## Task 5：不可变export包、ZIP下载与导出恢复

**依赖：** 2–4。**验收：** C02、C05、C06。

- [ ] Build逐篇流式捕获到项目内staging；保存source副本、公用本地路径副本、manifest、README；整批成功rename后ready，失败保留诊断，不下载半包。
- [ ] 内部字节hash与local_export_sha分开；下载manifest剔除StoragePath、actor、凭证、绝对路径。单HTML仅对内联完整依赖允许；外部依赖ZIP可诊断下载但不可部署。
- [ ] 写ZIP allowlist/目录穿越/重复条目测试和HTTP解压浏览测试；创建文件完成标记后故障，重启可补记ready。
- [ ] 红绿运行`go test ./internal/product/staticexport -run 'TestArchive|TestExport' -count=1`；实际testdata使用九模板渲染后的文档，不能直接上传带Go占位符的源模板。

```go
func TestArchiveRejectsPrivateEntries(t *testing.T) {
    for _,p := range []string{"../token","private/provenance.json","/etc/passwd"} {
        if IsPublicArchivePath(p) { t.Fatalf("unsafe entry %q",p) }
    }
}
// IsPublicArchivePath(string) bool仅接受manifest.json、README.txt、
// public/下allowlist文件；不靠zip整个root实现下载。
```

## Task 6：Cloudflare REST协议与测试专用Worker契约探针

**依赖：** 1。**验收：** C08、C09、C10。

- [ ] 新客户端固定生产API origin，Options允许注入httptest Transport/BaseURL仅在测试构造器中；不修改旧services.CloudflareService。
- [ ] httptest严格断言账号path、Bearer隔离、HTTP+success、bucket上传JWT、base64/MIME、空buckets、JWT过期、429 Retry-After和5次退避上限。
- [ ] 实现资源hash算法、版本multipart、显式assets.config/binding，UploadVersion不能Activate；Initialize PUT与普通更新POST函数分离。
- [ ] 红绿运行`go test ./internal/product/staticdelivery/cloudflare -count=1`。增加`contract_integration_test.go`，只有用户提供专用测试账号、Worker名并明确允许远端写入后才运行；不得扫描或打印.env中的Token。
- [ ] 真实探针：新建专用占位Worker→上传一HTML一PNG→POST版本→确认原占位仍active→POST100%deployment→公开验证；保存脱敏fixture。如果API不接受metadata，记录阻塞并修正契约，不fallback PUT直接上线。

```go
func TestAssetHashProtocol(t *testing.T) {
    body := []byte("asset fixture")
    sum := sha256.Sum256([]byte(base64.StdEncoding.EncodeToString(body)+"png"))
    want := hex.EncodeToString(sum[:])[:32]
    if got := AssetHash(body,"png"); got != want { t.Fatalf("%s != %s",got,want) }
}
// AssetHash([]byte,string) string在assets.go；测试另须解码multipart，
// 原size而非base64长度、API token不得混用于上传session。
```

## Task 7：凭证、多账号、目标初始化与域名核实

**依赖：** 2、6。**验收：** C08、C09、C10、C20。

- [ ] 按用户2026-10-20确认，新建/轮换凭证在独立credentials集合存储明文token，不要求服务器密钥；JSON/页面/日志/公开产物不得回显。保留Vault只用于历史加密revision可选读取，缺少旧密钥时要求重新填写Token，不静默替换历史任务凭证。
- [ ] 实现AccountService.Create/Rotate/Verify/SetEnabled/Archive/Restore；密码不回显，读取通过与写入验证分开。轮换旧revision保留给已排队任务，不静默换Token。
- [ ] 实现TargetService.Create/Verify/Initialize/SetEnabled/Archive/Restore；同account+worker唯一；未知Worker禁止接管，读取列表403但指定Worker200允许诊断继续。
- [ ] workers.dev查询真实账号subdomain；未开通提示手工开通；初始化intent先持久化，再PUT占位；超时保持initializing并只读核实。
- [ ] 红绿运行`go test -p 1 ./internal/product/staticdelivery -run 'TestVault|TestAccount|TestTarget' -count=1`，验证账号A/B同Worker名隔离、禁用不会调用CF删除/DNS。

必须覆盖 `TestAccountPlaintextCredentialsNeedNoMasterKey`：无密钥新增账号、明文入库、轮换、服务重建后读取固定revision、JSON不回显。另覆盖历史加密凭证不被当作明文使用，且管理员可不提供旧密钥直接轮换新Token。runtime不得以密钥缺失隐藏账号/目标表单。

## Task 8：目标化release、预览digest和完整站点合并

**依赖：** 5、7。**验收：** C07、C11、C19。

- [ ] 构建完整ReleaseManifest，current页面复用已有export，新增页面才来自本次包；同path不同content 409，同内容改path不隐式删除。
- [ ] HTML/CSS/JSON结构化重写canonical/OG/JSON-LD/资源，按依赖顺序hash CSS；默认图标/封面只按九模板显式表读取；不改正文品牌和JS。
- [ ] PreviewDigest覆盖target/config/base revision、全部export hash、显式remove_paths/操作类型；预览不触发CF写入。
- [ ] 旧hash资源首次失去引用时设置retained_until，后续部署原样继承不续期；包含保留资源做套餐和module大小检查。
- [ ] 红绿运行`go test ./internal/product/staticdelivery -run 'TestRelease|TestPreview|TestRetainedAsset' -count=1`。

```go
func TestPreviewDigestChangesWithBaseRevision(t *testing.T) {
    p := Preview{BaseRevision:7,ConfigRevision:2}
    before := PreviewDigest(p)
    p.BaseRevision++
    if before == PreviewDigest(p) { t.Fatal("revision omitted from confirmation") }
}
// Preview结构在Task1定义，字段int64；JSON稳定排序，数组规范化但保留有语义顺序。
```

## Task 9：独立Worker、Go embed包和路由测试

**依赖：** 8。**验收：** C06、C15、C16基础。

- [ ] `src/handler.mjs`导出`createHandler(routes)`，`src/index.js`只导入JSON路由模块并导出默认fetch；Go bundle用embed打包，不要求服务器有Node。
- [ ] Node内建test+Request/Response/ASSETS fake验证canonical、alias冲突、root404、保留路径、HEAD、405、ETag/304、query、UTF-8和路径编码拒绝。
- [ ] 设置最终cache、MIME、X-LC-Release及测试目标noindex；不加阻止现有inline JS/CSS的CSP，不代理CMS。
- [ ] 运行`node --test workers/report-static/test/handler.test.mjs`先红后绿；`go test ./workers/report-static -count=1`核对embed模块名/MIME与Remote multipart一致。

```js
import test from 'node:test';
import assert from 'node:assert/strict';
import { createHandler } from '../src/handler.mjs';
test('missing route never falls back to CMS', async () => {
  const handler=createHandler({pages:{},aliases:{},assets:{},release_id:'r1'});
  const response=await handler.fetch(new Request('https://reports.example/api/content'),{});
  assert.equal(response.status,404);
});
```

## Task 10：durable任务执行、账号并发和激活intent

**依赖：** 2、5–9。**验收：** C12、C13。

- [ ] Runner领任务前校验原actor有效/当时scope与当前角色、target/config/base revision、account状态；global2/account2有界，target CAS只能一个owner。
- [ ] Job按queued/preparing/uploading/version_ready/activating/verifying推进，每步durable checkpoint；先写激活intent再调用Remote.Activate。
- [ ] 取消只在intent前CAS获胜，初始化PUT同样按外部变更边界处理；停用账号只允许只读恢复，不能借后台服务身份绕过用户权限。
- [ ] 注入fake Remote：所有方法记录调用次数及账号；DB写失败模拟每个checkpoint，重启Runner确保未决激活不重发。测试文件完整定义fakeRemote，实现Task1的Remote接口，不使用空泛mock默认成功。
- [ ] 红绿运行`go test -p 1 ./internal/product/staticdelivery -run 'TestRunner|TestActivation|TestCancel' -count=1`。

```go
func TestTransitionDoesNotReleaseUnknown(t *testing.T) {
    if CanReleaseTarget(JobActivationUnknown) || CanReleaseTarget(JobActiveUnverified) {
        t.Fatal("uncertain target released")
    }
}
// CanReleaseTarget(JobState) bool在runner.go；真实Mongo测试另断言active_job_id未清空。
```

## Task 11：公开校验、超时读回、drift与本地记账恢复

**依赖：** 10。**验收：** C13、C14、C16。

- [ ] Verifier核对远端100%版本、health、全部变化HTML/meta/hash、变化资源MIME/hash、删除路径、HEAD与不变页面样本；连续3轮/间隔2秒。
- [ ] 读取超时、激活超时进入activation_unknown；确认已激活但访问检查失败进入active_unverified，禁止通用retry；30分钟超时不释放目标。
- [ ] Reconcile只读远端，确认后targets单文档提交release/revision，再幂等补写job和release状态；旧请求可能仍执行时不能根据一次旧版GET决定重发。
- [ ] 外部改版本/配置/分流→drifted；账号禁用后仍能核实已发出的请求但不做新激活。公开GET使用目标已核验HTTPS origin，不接受任意客户端探测地址。
- [ ] 红绿运行`go test -p 1 ./internal/product/staticdelivery -run 'TestVerify|TestReconcile|TestDrift' -count=1`。

```go
func TestActiveUnverifiedCannotReturnToQueued(t *testing.T) {
    if ValidJobTransition(JobActiveUnverified,JobQueued) { t.Fatal("would redeploy an active release") }
}
// ValidJobTransition由Task1提供；Task12另测RetryAllowed与提交接口。
```

## Task 12：命令、撤下、回滚、retry/cancel和origin迁移

**依赖：** 8、10–11。**验收：** C11、C17、C18。

- [ ] 定义CommandService与Submit/Preview入口：kind与请求字段严格对应设计§13；submit核实digest/revisions，不使用请求中的account_id/worker_name覆盖target。
- [ ] 单篇rollback合并当前其他页面，全站rollback使用同target完整历史manifest；不存在的withdraw幂等成功，不新增空版本。
- [ ] retry新operation引用原job、expected_attempt CAS追加；旧key重放旧结果。cancel需要专用kind权限，initializing/unknown不能直接取消释放。
- [ ] origin迁移检查新域名指向同Worker，整体重写保留源包，成功target CAS提交新origin/config；失败不提交新配置，未知状态对用户显示实际远端状态。
- [ ] 红绿运行`go test -p 1 ./internal/product/staticdelivery -run 'TestCommand|TestRollback|TestWithdrawal|TestOrigin|TestRetry' -count=1`。

```go
func TestRetryOnlyBeforeConfirmedActivation(t *testing.T) {
    for _,s := range []JobState{JobActivating,JobActivationUnknown,JobActiveUnverified,JobSucceeded} {
        if RetryAllowed(s) { t.Fatalf("retry allowed in %s",s) }
    }
}
```

## Task 13：新增存储GC、引用保护与备份恢复

**依赖：** 2、5、8、11–12。**验收：** C18–C20。

- [ ] Sweep读取targets当前release、active/unknown jobs、pin、历史引用，保护未补记committed的current release；不调用旧Publication GC。
- [ ] 固定时钟验证90天历史/7天失败临时文件/7天退出资源，无引用才删除；目标完整资源清单丢失返回unavailable，不能空站点覆盖。
- [ ] 备份manifest包含新集合、私有artifact、installation ID和key_id依赖（不含明文key）；恢复前校验字节hash/ownership，缺项不允许覆盖远端。
- [ ] 红绿运行`go test -p 1 ./internal/product/staticdelivery -run 'TestRetention|TestBackup|TestRestore' -count=1`。

```go
func TestRetainedUntilDoesNotSlide(t *testing.T) {
    first := time.Date(2026,10,19,0,0,0,0,time.UTC)
    expires := RetainedUntil(first,time.Time{})
    later := RetainedUntil(first.Add(24*time.Hour),expires)
    if !later.Equal(expires) { t.Fatal("old resource retention was extended") }
}
// RetainedUntil(now,existing time.Time) time.Time：existing非零原样返回，否则now+7天。
```

## Task 14：全量REST、鉴权与OpenAPI契约测试

**依赖：** 5、7、12–13。**验收：** C08、C12、C17。

- [ ] 按设计§13逐行注册全部route，设计表即路由覆盖清单；包括archive/restore、所有preview、origin迁移、cancel，不只实现happy path。
- [ ] 每handler先UserHasPermission/拒绝sandbox，再strict JSON；禁止先预留operation再返回403。GET下载流式allowlist，列表分页20/100。
- [ ] handler_test.go用httptest认证actor注入运行权限矩阵、unknown fields、跨target release、428缺key、409 body冲突、202任务状态、敏感字段不泄漏。
- [ ] 写独立OpenAPI所有request/response/error枚举，测试路由方法/路径与OpenAPI一致；不修改旧generation StatusForCode。
- [ ] 红绿运行`go test ./internal/product/staticdelivery/httpapi -count=1`。

```go
func TestStrictDTORejectsAccountOverride(t *testing.T) {
    var req DeployRequest
    dec:=json.NewDecoder(strings.NewReader(`{"account_id":"other","export_ids":[]}`))
    dec.DisallowUnknownFields()
    if dec.Decode(&req)==nil { t.Fatal("account override was accepted") }
}
// DeployRequest仅含设计定义的export_ids/remove_paths/revisions/confirmation_digest。
```

## Task 15：服务接线、故障隔离与旧功能回归

**依赖：** 3、14。**验收：** C01、C02、C16基础。

- [ ] 新建static_delivery_runtime.go构造附加服务，复用rt.Store/rt.PubRepo，只读源适配器注入；新增Register调用位于同一apiv1中间件之后，原路由不改。
- [ ] 原Handler/APIHandler通过新增SetStaticDeliveryRuntime接入，不创建第二Publication runtime，不修改全局services.SetPublicationPublisher行为。
- [ ] 新模块初始化错误只在新API返回503/可读原因；CF key缺失仍能export；使用已有关闭context取消runner，不新增必须外部部署的进程。
- [ ] 为旧source文件只加所需字段/构造参数/导航接线，不拆改旧publish/unpublish；`.gitignore`只追加新私有artifact与项目内测试runtime路径。
- [ ] `go test -p 1 ./cmd/server ./internal/handlers ./internal/services ./config -count=1`与原publication/e2e串行回归；缺Mongo不得把DB skip算通过。

```go
func TestDeliveryInitFailureDoesNotChangeProvider(t *testing.T) {
    cfg:=config.DefaultDev()
    cfg.ApplyPublicationDefaults()
    if cfg.StaticStorageProvider!="filesystem" { t.Fatal("old provider changed") }
}
// runtime测试还必须注入新增EnsureIndexes失败，确认旧HTTP入口仍可请求；
// 此默认值断言不是故障隔离的全部证据。
```

## Task 16：后台账号/Worker管理与内容部署交互

**依赖：** 14–15。**验收：** C08、C09、C14、C17。

- [ ] 沿用原renderAdmin/layout、session与CSRF；新增独立模板常量到既有adminTemplates，不引入第二前端项目。全部管理提示使用清晰中文。
- [ ] 提供账号增改/读取检查/凭证轮换/停用/归档恢复，区分读通过与写验证；提供专用Worker初始化和origin核实，未知Worker显示不可接管。
- [ ] 内容列表/详情新增导出、账号→目标联动、preview影响清单、确认提交、阶段进度、下载/访问链接；切换账号立即清空worker，原publish按钮与状态不变。
- [ ] 提供目标历史、逐篇远端状态、withdraw/page-site rollback、retry/cancel、origin迁移确认；unknown/active_unverified展示实情，不提供危险重发按钮。
- [ ] `go test -p 1 ./internal/handlers -run TestAdminStaticDelivery -count=1`；浏览器验证390/1440宽度、表单错误、CSRF与键盘操作，截图只保存在项目内。

```go
func TestAccountOptionsNeverRenderToken(t *testing.T) {
    option:=deliveryhttpapi.AccountOption{ID:"000000000000000000000001",Name:"生产账号"}
    b,err:=json.Marshal(option)
    if err!=nil || bytes.Contains(b,[]byte("token")) { t.Fatalf("option=%s err=%v",b,err) }
}
// AccountOption在httpapi DTO中定义，仅ID/Name；deliveryhttpapi为该包import别名。
// 完整后台测试还检查Token值不在HTML/脚本。
```

## Task 17：端到端与真实Cloudflare验收

**依赖：** 1–16。**验收：** C01–C20。

- [ ] 新e2e测试fixture使用独立test数据库、项目内临时root、真实Mongo repository、httptest Cloudflare及独立HTTP ASSETS模拟器；不只测内存fake仓库。
- [ ] 全链路发布A→导出A→部署A→部署B→更新A→核对B→单篇回滚→撤下→跨账号重复部署；验证原Content/Publication数量与值不因CF操作变化。
- [ ] 故障注入覆盖激活前/后、落库前/后、换Token、禁用账号、原资源被GC；恢复后不重复激活、不清空其他页面。
- [ ] 九模板由既有模板引擎生成真实fixture后进行截图、浏览器网络请求检查、500ms显示与语言现状对比；测试UA读取原始英文meta不依赖JS。
- [ ] `node --test workers/report-static/test/handler.test.mjs`、新包`go test -race`、全项目`go test -p 1 ./... -count=1`、`go vet ./...`和`go build ./...`；所有输出证据保存在项目内，不提交凭证。
- [ ] 真实验收需要用户提供/指定两个Account ID下的专用测试Worker及相应Token，先说明将发生的远端写入并取得明确授权；不从聊天历史猜测账号或覆盖生产。真实CF门槛失败/缺凭证时停止远端阶段，报告本地完成范围和未通过项。

集成门槛命令（执行前Task2准备项目内临时/缓存目录；这是计划，不在本轮创建这些运行目录）：

```sh
: "${MONGODB_URI:?test database URI required}"
: "${DATABASE_NAME:?dedicated name containing test required}"
go test -p 1 ./internal/product/staticdelivery/... ./internal/product/staticexport/... -count=1
go test -p 1 ./... -count=1
go vet ./...
go build ./...
```

测试执行环境：`TMPDIR`和`GOCACHE`指向项目内`var/static-delivery-test-runtime/`对应目录；只读取现有module cache，若需下载依赖也设置项目内`GOMODCACHE`。不得为了跑测试在项目外生成脚本、截图或worktree。Mongo实例数据卷不得覆盖已有开发/生产数据；若需新外部Docker volume位置，先取得对应位置授权或使用项目内bind目录。

## Task 18：使用/运维文档、证据清单与最终自审

**依赖：** 17。**验收：** 全部；未执行的真实测试不能标已完成。

- [ ] 完成OpenAPI（Task14已建立）、STATIC-DELIVERY.md、STATIC-DELIVERY-RUNBOOK.md，追加docs/API.md新索引；说明导出与CMS/CF双生命周期、domain准备、权限、数据库备份包含明文Token、旧加密凭证重新录入、unknown恢复。
- [ ] 新建`docs/implementation/static-delivery-acceptance.md`逐行记录C01–C20对应测试名、命令、日期、结果及证据位置；每个非通过项写具体原因，不用“基本通过”。
- [ ] 建立`docs/implementation/static-delivery-release-report.md`，只填写真实Account/Worker的脱敏标识、remote IDs、公开URL、CMS离线后结果；不记录Token或JWT。
- [ ] 对照设计全部API路由和后台操作做覆盖检查，确认未偷偷改DNS、旧CI、旧部署脚本、原webhook或provider；最终git diff逐文件自审。
- [ ] 向用户交付已实现范围、测试结果、待提供的账号/真实验证条件；只有本地与远端门槛都通过才称可生产使用。没有提交/推送授权不自行推送。

## 依赖顺序与阶段出口

| 阶段 | 任务 | 出口 |
| --- | --- | --- |
| A：独立导出 | 1→2→3→4→5 | 不配置Cloudflare也能下载完整ZIP/符合条件的HTML |
| B：多账号/平台协议 | 6→7（依赖1–2） | 账号/目标可管理；mock协议完整；真实探针是否通过单列 |
| C：完整站点部署 | 8→9→10→11→12→13 | 更新/恢复/回滚/GC在本地端到端成立 |
| D：接线与交付 | 14→15→16→17→18 | 现有系统兼容，后台完整，真实CF验收有证据 |

无需把A–D拆到不同工作目录并行；按用户要求由当前agent顺序处理。Task6真实协议探针可在获得测试凭证后提前执行，失败不妨碍独立导出，但阻止把部署功能宣布完成。

## 覆盖映射与自审

| 设计章节 | 实现任务 | 验收 |
| --- | --- | --- |
| §1–4兼容/配置/存储 | 1–2、15 | C01/C08 |
| §5–6多账号/目标 | 6–7、14、16 | C08–C10/C20 |
| §7导出 | 3–5、14–17 | C02–C06 |
| §8release合并 | 8、12 | C07/C11/C19 |
| §9实际CF协议 | 6、9–11、17 | C10/C13/C16 |
| §10Worker | 9、17 | C06/C15/C16 |
| §11任务恢复 | 2、10–11、15 | C12–C14 |
| §12撤下/回滚/保留 | 12–13、17 | C17–C20 |
| §13–14API/后台 | 14、16 | C08/C09/C14/C17 |
| §15运行/审计 | 1、10、15、18 | C01/C13/C20 |
| §16–18验收/交付限制 | 17–18 | C01–C20 |

- [ ] 实施前由用户确认本计划；当前尚未创建产品代码或执行真实Cloudflare写入。
- [ ] 每个任务的红灯/绿灯/回归结果写入对应验收记录，测试snippet不是替代完整输入矩阵的捷径。
- [ ] 平台API/设计行为若不匹配，先修正spec并同步计划与测试，不实现第二套隐含契约。
