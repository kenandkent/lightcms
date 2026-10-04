# 报告与公告模板验证记录

日期：2026-10-03。范围：新增 8 套 HTML 模板、导入 JSON、示例数据、分享图片和图标；未修改业务后端、原有加密分析模板或已发布内容。

## 交付清单

网络安全报告、基金研报、风投研报、币安公告视觉参考、OKX 公告视觉参考、新闻编辑报道、财经日报、科技报告。文件入口和导入步骤见 [REPORT-TEMPLATES.md](../../examples/templates/REPORT-TEMPLATES.md)。

frontend-design / refactoring-ui 的设计指导落实为不同字体体系、栏位结构、标题比例、报头与正文表现；并非仅更换配色。交易所款使用独立品牌和原创图标，明确非官方发布。

## 通过的验证

- `go test ./examples/templates/... -count=1`：通过。每套模板由实际 LightCMS 字段校验器及 frozen publication renderer 验证；校验示例数据、英文默认数据和可选中文数据，核对标题转义、Markdown、分享元数据以及 HTML 与导入载荷完全一致。
- `go vet ./examples/templates/...`：通过。
- 浏览器自动化：24 项语言／布局场景（8 套 × 桌面英文、手机中文、非支持语言回退英文）通过；另有 8 项缺失中文正文回退测试和 8 项禁用 JavaScript 测试通过，共 40 项场景。
- 浏览器场景覆盖 500ms 初始隐藏、reduced-motion、正文显现、英文服务端分享元数据、目录链接唯一性、保留原有正文片段锚点、复制反馈、无页面级横向溢出；禁用 JS 时仍可看到英文正文。
- 已目视检查全部 8 套实际渲染截图，包含桌面和手机截图。
- 8 套生成器 `--check` 全部通过；8 张社交预览图实际为 1200×630，8 张 touch icon 实际为 180×180。
- `git diff --check`：通过。

本地实际渲染 HTML、16 张截图和浏览器验证结果保存在忽略目录 `bin/template-previews/`，可用附带工具重新生成。它们不是生产发布物，包含开发环境 URL。

## 验证边界

- 曾扩展运行 `internal/product/publication` 与 `internal/product/templatecontract` 测试；其中需要 MongoDB 的集成测试因 `127.0.0.1:27017` 连接拒绝失败，当前 Docker daemon 也未启动。因此不声称全项目或后台数据库集成测试通过。本次模板的无数据库实际渲染与字段校验测试已通过；无需修改后端解决此环境限制。
- 未通过管理 API 向实际数据库导入模板，未创建或更新已发布页面。
- 未使用真实生产 URL 在 Telegram、WhatsApp、LINE、LinkedIn、Facebook 验证抓取与卡片显示；这些验收仍属于 [PROD-001](../TODO.md)。文案与元数据已准备好，但示例域名必须替换。
- 文件保存于项目工作区，本次未自动提交或推送 Git。
