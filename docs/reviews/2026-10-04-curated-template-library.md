# 九套自有模板库清理与迁移记录

日期：2026-10-04，Asia/Shanghai。目标：本地开发库 `59351/lightcms`，不涉及生产环境。用户确认保留首页与404页面，迁移后清理系统模板。

## 处理结果

- 七套系统模板已从当前模板库移除：Blog Post、Press Release、Explanatory Page、Blank Page、Homepage、Concept Page、Standard Page。
- 当前只保留九套自有模板，全部启用，当前版本为2。保留原模板ID，通过API重新导入中文元信息、中文字段标签和完整HTML／字段定义，避免已有文章引用失效。
- 加密货币分析模板使用富文本正文，定义8个字段；另外八套报告／公告模板使用Markdown正文，分别定义15个字段。字段标识保持英文，与HTML占位符一致。
- 标题、摘要、正文、作者、分类以及所用分享图／图标字段均已声明；`public_url`、`published_at`属于系统注入值，不作为用户字段声明。

## 页面保留与迁移

首页 `Welcome to LightCMS` 与404页面 `Page Not Found` 均保留原内容ID，迁移到“新闻媒体报道模板”。保留原英文文字和原有的进入后台／返回首页链接，补充对应中文内容，使用正常内容更新API保存并显式发布。

首页路径保持 `/`；404内容记录旧的空 `full_path` 经正常更新路径规范化为 `/404`。未知路径仍返回HTTP 404并显示404页面。原有“BTC 链上数据周报”仍使用原加密分析模板ID，其文章内容没有修改或重新发布。

## 操作边界与可恢复性

模板配置、页面更新、页面发布和模板删除均调用已有认证API。普通删除API禁止删除系统模板，因此在迁移并确认没有当前内容引用之后，仅对明确的七个系统模板ID移除 `is_system` 保护标志，再调用删除API；没有直接绕过引用检查删除模板。相关历史版本不清空，旧页面数据及原库可从备份恢复。

为避免重启重新补回系统模板，新增持久化设置：

```json
{"type":"template_library_policy","custom_only":true}
```

`Handler.SeedDefaults` 遵守此标志：跳过系统模板、默认首页和默认404的初始化，其他初始化不变。未配置／显式false时保留原有默认安装行为。该设置已写入本地开发库；旧二进制不认识该设置，回退部署时须注意。

备份均位于项目内忽略目录：

- `bin/local-dev-backups/pre-curated-library-20261004.archive.gz`：迁移前完整开发库。
- `bin/local-dev-backups/pre-curated-files-20261004.tar.gz`：迁移前content文件。
- `bin/local-dev-backups/lightcms-before-curated-20261004`：旧开发二进制。
- `bin/local-dev/curated-library-20261004.json`：操作结果、模板ID、页面ID与Publication ID。

删除的七套系统模板可通过数据库备份恢复；恢复时需同步考虑页面、文件与种子策略。未删除任何文章、账号或自有模板历史版本。本次没有Git提交或推送。

## 验证

- 清理后、实际重启后分别核对：模板9、系统模板0、内容3，当前内容不存在失效模板引用。
- 九套模板的名称、描述、分类及字段显示名称均为中文，字段定义覆盖全部业务占位符。
- 首页HTTP 200；未知地址HTTP 404；原有 `/btc-weekly` HTTP 200；服务及MongoDB健康。
- 真实服务浏览器核验：首页和404页面的英文／中文手机场景通过，正文可见，无JS异常与页面级横向溢出；截图保存在忽略目录 `bin/local-dev/`。
- `TestSeedDefaultsHonorsCustomOnlyLibrary`：先观察到custom-only场景错误恢复系统模板的失败，再实现策略并通过；同测显式false保留默认安装行为，重复初始化不增加模板。
- 完整 `go test -p 1 ./... -count=1` 通过，使用独立测试Mongo及测试库，未在开发库执行测试清理。日志：`bin/local-dev/curated-library-tests-20261004.log`。
- 针对模板与种子逻辑的测试／静态检查通过。新增加密模板测试核对富文本、标题转义及分享元数据。
- 八套报告模板的40项语言／布局／无JS场景再次通过；模板导入文件与HTML一致。已恢复完整测试修改的测试用主题CSS，未保留该无关生成改动。

生产域名与社交平台实测仍属于[PROD-001](../TODO.md)，不因本地模板清理而视为完成。
