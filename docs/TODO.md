# 项目待办

## PROD-001：生产域名确定后，完成所有模板的社交分享预览配置

- 状态：待办，等待生产域名确定。
- 创建日期：2026-10-02。
- 负责人：Ken 确定域名；部署负责人完成配置与验证。
- 优先级：生产部署必备，未完成不得通过 Go-live Gate。
- 范围：本项目所有生成公开页面的 HTML 模板，包括文件中的模板、默认／主题模板、数据库中已启用的 Template HTMLLayout，以及已发布页面。管理后台内部视图不属于公开分享页面范围。
- 模板清单补充：新增 8 套报告／公告模板的文件与静态分享素材见 [REPORT-TEMPLATES.md](../examples/templates/REPORT-TEMPLATES.md)。全部纳入此生产门槛；示例 JSON 中的 `publisher.example` 地址必须替换，不能直接上线。

### 执行步骤

- [ ] 确定生产 HTTPS 域名与公开页面路径，配置 `BASE_URL`／`PUBLIC_BASE_URL`。
- [ ] 盘点所有公开页面模板，记录模板路径或 ID、版本、检查结果和负责人；覆盖率必须为 100%。
- [ ] 每个模板的初始 HTML `<head>` 均输出 `og:title`、`og:description`、`og:type`、`og:site_name`、`og:url`、`og:image` 与图片 alt，以及 canonical、X/Twitter 卡片、favicon 和 Apple touch icon。标题与简介随文章数据替换，不能依赖浏览器 JS 注入。
- [ ] 页面 URL、`og:url`、canonical、`og:image` 和 `twitter:image` 均使用真实生产绝对 HTTPS 地址；清除示例域名、未替换占位符和预览图片的相对路径。
- [ ] 所有模板配置预览图来源，允许文章专属图片或默认图片；图片尺寸声明与实际文件一致。默认图为 1200×630 PNG：`static/images/chain-lens-share.png`。
- [ ] 核对 favicon 与 Apple touch icon 已部署且公开可访问，确认品牌、图片说明与文章语言一致。
- [ ] 更新 TemplateVersion 并通过正式发布流程更新受影响页面；已发布页面不会因编辑模板而自动更新。
- [ ] 对所有模板类型生成实际生产测试 URL，确认页面及图片无需登录即可返回 HTTP 200，HTTPS、Content-Type、robots 与 WAF 配置允许分享抓取程序读取。
- [ ] 在 Telegram、WhatsApp、LINE、LinkedIn、Facebook 实测链接卡片；记录实际显示的标题、简介、图片，必要时刷新平台缓存。平台可能裁剪文案或不展示小图标，验收以支持的字段正确与抓取成功为准。
- [ ] 将全量模板清单、生产 URL、验证日期及结果归档到 `docs/implementation/social-preview-production-report.md`，由部署负责人签字关闭此待办。

### 完成标准

所有公开页面模板均具备完整元数据，使用真实生产地址，无未处理的抓取失败；上述平台验证结果已归档。此项在 [生产部署清单](DEPLOYMENT-PRODUCTION.md) 和 [发布清单](implementation/release-checklist.md) 中同步作为强制门槛。

实施参考：[模板分享预览配置](../examples/templates/SOCIAL-PREVIEW.md)。当前新闻模板和 AGT 示例页已增加元数据；AGT 示例中的图片相对地址与缺失的生产 canonical／og:url 仍须在此任务中补齐。
