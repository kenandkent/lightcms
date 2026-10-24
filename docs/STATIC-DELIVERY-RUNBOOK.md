# 静态报告发布运维说明

## 启动与持久化

原 Go CMS 和 MongoDB 的启动方式不变。新增发布 worker 随原服务进程启动、随后台 context 停止；MongoDB 是持久队列，不能换成仅内存队列。事务需要 Mongo replica set（沿用原 Publication 的数据库条件）。

默认私有目录为项目内 `var/static-delivery`，不通过 HTTP 静态目录公开。`docker-compose.prod.yml` 为该目录增加专用命名卷 `lightcms-static-delivery-data`，不改变原 content/uploads 卷、端口、DNS 或 CI。若设置 `STATIC_DELIVERY_ROOT`，必须同步提供相应持久卷，不能仍挂默认路径而把真实产物写进容器临时层。

**首次升级检查：**如果旧容器已有 `var/static-delivery` 文件，管理员应在替换容器/挂载新卷之前备份并迁移到专用卷，不能让空卷遮住已有导出或 release。没有产物时不需要复制。不要仅恢复 Mongo 而不恢复配套私有文件。

安装身份改由 `static_delivery_settings` 的 `installation` 文档持久保存。升级时，若数据库已有且只有一种 target installation_id，则沿用，不生成新身份导致已有 Worker 不再被识别。存在多个身份或空配置时停止新模块，要求人工核实，不覆盖远端。

## 数据备份

一致备份所有 `static_delivery_*` 集合和整个私有目录。包括新增 `static_delivery_previews`、`static_delivery_settings`，以及 accounts、credentials、targets、exports、releases、jobs、operations。Token 按用户要求是数据库明文，备份也包含 Token。不需要为新凭证配置服务器 master key；历史加密 revision 如仍被任务引用，保留其原有可选解密条件或在管理页轮换，不能伪称可读取。

预览过期只禁止新提交，不自动删除已排队任务所引用的包。当前没有自动清理工具；不得用通配符清空私有目录或 TTL 删除 operations。磁盘告警和备份空间应纳入部署检查。

## 发布前检查

1. 构建版本包含内容管理新入口，并且新模块没有启动错误。
2. 账号 enabled、连接可读、专用目标 ready、公开 origin 与该 Worker 匹配。
3. 生产使用有效 HTTPS origin；Cloudflare 访问策略不能拦住公开报告、资源或 health 端点。
4. 报告静态资源能采集，脚本已人工审阅；旧未验证迁移记录先完成原迁移/发布。
5. 私有目录是持久卷；安装身份沿用；数据库与文件备份成对。
6. 通过管理界面选一篇指定测试报告，预览后发布到专用测试 Worker，验证最终链接、HEAD/GET、分享信息、手机布局，再发布正式报告。

## 未决激活恢复

`activation_unknown` 和 `active_unverified` 不能通过清空 active_job_id、删除任务或重新点发布解决。后台读取实际 deployments/version 及公开页面，只有确认候选版本与身份后才能完成记账。旧版本仍在响应并不能证明超时请求以后不会生效。若域名/权限问题持续，先修复这些条件；不得手改任务为 succeeded。

账号/目标停用或归档不删除远端页面。此版本没有远端撤下/回滚按钮，不应把归档或取消当作已发布内容的下线操作。
