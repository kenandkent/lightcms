# 报告模板本地导入记录

导入日期：2026-10-03，Asia/Shanghai。目标为本地开发环境，不是生产环境。

## 部署与导入

- 启动 Docker Desktop，复用并启动原有 `lightcms-dev-mongo` 容器及数据库，没有重建或清空数据。
- 启动项目已有开发二进制 `bin/lightcms`，配置为根目录 `config.dev.json`。
- 服务地址：`http://127.0.0.1:50491`；后台模板列表：`http://127.0.0.1:50491/cm/templates`。
- 导入前备份：`bin/local-dev-backups/pre-template-import-20261003-1702.archive.gz`（忽略文件，不包含在 Git 交付中）。
- 所有模板通过认证的 `POST /api/v1/templates` 创建，没有直接向数据库写入模板。使用现有应用认证凭证，未另建 API Key，未将凭证输出或保存到报告。
- 一次批量请求触发 HTTP 429 后，降低请求频率并重新核验已成功创建的模板；已存在且一致的模板跳过创建，没有产生重复模板。

## 导入结果

| 模板名称 | Slug | 状态 | 版本 | 字段数 |
| --- | --- | --- | --- | --- |
| 网络安全报告模板 | cybersecurity-report | active | 1 | 15 |
| 基金研报模板 | fund-research | active | 1 | 15 |
| 风投研报模板 | venture-research | active | 1 | 15 |
| 币安公告风格模板（非官方） | binance-announcement-style | active | 1 | 15 |
| OKX公告风格模板（非官方） | okx-announcement-style | active | 1 | 15 |
| 新闻媒体报道模板 | editorial-news | active | 1 | 15 |
| 财经日报模板 | financial-daily | active | 1 | 15 |
| 科技报告模板 | technology-report | active | 1 | 15 |

模板名称、描述及库分类为中文；页面默认英文和浏览器中英切换保持不变。导入文件与生成器同步更新，后续重新生成不会恢复英文库名称。

## 核验

- 每套通过 `GET /api/v1/templates/{slug}` 读回核对中文元信息与完整 HTML。
- 每套通过 `GET /api/v1/templates/{slug}/schema` 核对版本和 15 个字段；必填字段为 `headline`、`summary`、`body`、`author`、`category`。
- 模板数量由 8 增加到 16；用户 1、内容 3、Publication 0 的数量保持不变，没有创建测试文章或发布页面。
- 导入后 `/healthz` 返回 healthy，MongoDB healthy。
- 中文库元信息更新后 `go test ./examples/templates/... -count=1` 通过。
- API 导入结果明细位于 `bin/local-dev/template-import-20261003.json`（忽略文件，可本地查看）。

可登录后台新建内容，选择以上模板并填写文章数据。标题、正文等字段定义已随模板导入，不需要手动补建。生产分享域名仍未确定，[PROD-001](../TODO.md) 继续保留；正式发布前必须替换示例图片地址。本次没有 Git 提交或推送。
