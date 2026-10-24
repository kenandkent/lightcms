# 九套模板移动端视觉差异化

用户确认九套移动端方向后，由主 agent 直接实施，未使用子 agent。范围仅为模板样式、移动目录呈现和对应模板库更新，不改变后端接口或文章字段。

## 设计与实现

| 模板 | 移动端结构 |
|---|---|
| crypto-analysis | 居中品牌、分类胶囊、盒式分析摘要、单列署名、圆形操作按钮 |
| cybersecurity-report | 紧凑技术页头、等宽标题、档案分隔、正文前原生折叠目录 |
| fund-research | 居中封面、衬线标题、研究概览、表式发布信息、衬线正文 |
| venture-research | 杂志式斜体大标题、偏置引言、编辑式章节与引用 |
| binance-announcement-style | 紧凑深色页头、黄色分类条、克制标题、全宽按钮 |
| okx-announcement-style | 黑白粗线、强字重标题、横向章节导航、方形操作按钮 |
| editorial-news | 居中报头、双线分隔、左对齐新闻标题、记者式署名、衬线正文 |
| financial-daily | 日期前置、紧凑日报标题、小字号高密度表格、财经简报节奏 |
| technology-report | 数字封面、大幅轻字重标题、横向导航、浅色正文阅读区 |

移动样式独立维护于 `examples/templates/report-mobile-styles.css` 和 `crypto-mobile-styles.css`，以 screen/max-width:780px 限定，不影响桌面或打印。内嵌到HTML和template.json，无新外部依赖。复用单个TOC，移动/桌面切换时重新定位，避免重复ID；禁用JavaScript时原目录仍在正文后可用。

## 验证

- 同一篇文章九套对比：先观察原模板仅有7种非颜色布局特征的失败，再验证九种不同字体/布局特征；九套改前桌面关键样式指标保持一致。
- 截图人工检查九套手机样式；发现科技模板浅色正文区按钮对比度不足，新增失败测试后修正。
- 验证折叠目录点击、横向章节导航、切回桌面后目录恢复。
- 原有40项语言/加载延迟/无JS检查，HTML与导入payload同步检查，实际本地/生产9套×4宽度检查，以及全项目Go测试。
- 未替换字段定义、中文元信息、文章data或URL。API仅更新html_layout，并对现有已发布文章显式Publish。模板旧版本和Publication历史保留。

运行证据、数据库备份、更新前清单及同内容对比截图位于忽略目录 `bin/mobile-identities-20261010/`。本地/生产API写入结果分别为 `local.result.json`、`production.result.json`；实际页面检查记录位于对应 `*-after/results.json`。
