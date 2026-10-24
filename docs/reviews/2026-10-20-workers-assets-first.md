# Workers 静态资源优先调整与验证

范围：基于 `28a480cfabb266b8222dbc341dc71d42873e2743` 的现有发布链路，用户批准改成独立静态HTML页面优先响应。分支 `codex/worker-assets-first`。不改变原CMS运行、账号管理、发布队列、DNS或线上Worker；未授权自动提交/推送。

## 实现

- 新候选release冻结 `assets-first-v1`，上传时将HTML文件映射为精确公开路径，并提供HTML MIME；私有文件布局和下载包不变。
- `html_handling=none`，避免 `/a` 与 `/a.html` 被平台自动合并；根路径通过唯一静态200内部映射提供，正常访问不执行用户Worker。
- assets.config只对health指定worker-first。原fallback继续处理旧别名、404与必要错误响应，不把这些请求说成无限免费静态请求。
- 使用原生静态响应头保存release标识、测试noindex、hash资源immutable。两条header规则，不随文章数突破平台100规则上限。HTML每次复验缓存，平台原生ETag保留。
- verifier对新模式比对实际响应字节SHA-256、MIME、版本和缓存，接受原生ETag而不是错误假定其等于SHA-256。旧模式继续原校验。
- 新预览响应包含delivery_mode。旧模式release和已冻结任务不重写；只有用户新预览并确认后，才把包含原有文章的完整候选站点发布为新模式。

## 测试与review覆盖

1. 先观察到原实现缺少native配置、上传metadata仍全量worker-first的失败；实现后通过。
2. 原生ETag与项目SHA不同的响应最初被旧verifier拒绝；调整后通过，同时仍拒绝实际字节错误。
3. 公开路径映射测试覆盖首页、无后缀、`.html`、index名称和中文路径；legacy模式仍保持原始上传布局。
4. Mongo命令链测试验证runner实际传递canonical上传路径和HTML MIME，重复提交不重复激活、A→B→更新A保留B；权限/取消/恢复测试继续运行。
5. 官方本地运行时：Wrangler4.151.2（Miniflare5.20261019.0-beta、workerd1.20261019.1），工具仅安装在项目忽略的runtime目录，未加入CMS依赖。探针Worker证明普通文章和图片不调用用户Worker；再换入项目实际handler，检查health、别名、404及`.html`独立文章。首页/中文/query逐字节对应正确文章，图片缓存/MIME、HEAD和304均验证。
6. 发现的本地测试适配问题：Miniflare v5的v4转换不传入预解析headers/redirects，且默认hasUserWorker=false。通过读取其配置转换源码定位；本地测试改为实际控制文件和显式hasUserWorker。生产legacy version-upload metadata仍按官方Wrangler同一结构发送assets.config._headers/_redirects，不混用新的beta API。

新测试：`assets_first_test.go`、`verify_assets_first_test.go`、`cloudflare/assets_first_test.go`、`workers/report-static/test/native-assets.test.mjs`。旧Worker测试仍单独覆盖fallback；native测试需要显式运行，缺少工具/fixture会失败而不是skip-green。

## 本地native门槛复现

在项目根目录、Go1.25与Node24可用时执行；所有生成文件留在项目内。`NATIVE_ROOT`仅为本任务变量。

```sh
NATIVE_ROOT="$PWD/.superpowers/sdd/2026-10-19-static-export-cloudflare-workers/runtime"
npm install --prefix "$NATIVE_ROOT/assets-probe" --cache "$NATIVE_ROOT/npm-cache" wrangler@4.151.2
STATIC_DELIVERY_NATIVE_FIXTURE="$NATIVE_ROOT/native-assets.json" go test ./internal/product/staticdelivery -run TestAssetsFirstReleaseCarriesNativeRoutingConfig -count=1
STATIC_DELIVERY_MINIFLARE_ROOT="$NATIVE_ROOT/assets-probe" STATIC_DELIVERY_NATIVE_FIXTURE="$NATIVE_ROOT/native-assets.json" node --test workers/report-static/test/native-assets.test.mjs
```

最终验证（2026-10-20）：新模块完整race+独立Mongo、server/handler静态发布过滤测试均通过；Worker原10项和官方本地native路由1项均通过、零跳过；项目 `go build ./...`、`go vet ./...`、`go test ./... -run '^$'` 编译门槛与 `git diff --check` 均通过。编译门槛不代表全项目单元测试已执行：旧全量DB helper仍可能清空共享开发库，不在本checkout运行。

真实Cloudflare版本上传、证书/自定义域名及公开网络传播不由本地workerd证明，须部署新代码后在专用测试目标验收。本轮未提交、推送或部署线上，也未修改现有Cloudflare站点。
