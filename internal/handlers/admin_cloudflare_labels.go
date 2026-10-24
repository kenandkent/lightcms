package handlers

import sd "github.com/metavert-io/lightcms/v7/internal/product/staticdelivery"

type cloudflareAccountView struct {
	sd.Account
	Workers []sd.Target `json:"workers"`
}

func cloudflareLabels(lang string) map[string]string {
	words := map[string][2]string{
		"title": {"Cloudflare 管理", "Cloudflare management"}, "intro": {"管理 Cloudflare 账号及其专用 Worker。", "Manage Cloudflare accounts and their dedicated Workers."},
		"addAccount": {"添加账号", "Add account"}, "account": {"账号", "Account"}, "accountName": {"账号名称", "Account name"}, "remark": {"备注", "Notes"}, "workers": {"Worker 数量", "Workers"}, "connection": {"连接验证", "Connection"}, "status": {"状态", "Status"}, "actions": {"操作", "Actions"}, "details": {"详情", "Details"}, "delete": {"删除", "Delete"}, "close": {"关闭", "Close"}, "cancel": {"取消", "Cancel"}, "save": {"保存", "Save"}, "search": {"搜索账号名称或 Account ID", "Search account name or Account ID"},
		"emptyAccounts": {"尚未添加 Cloudflare 账号。点击右上角“添加账号”开始配置。", "No Cloudflare accounts yet. Select Add account to get started."}, "noMatch": {"没有匹配的记录。", "No matching records."}, "saved": {"操作已保存。", "Changes saved."}, "readable": {"账号读取已验证", "Account access verified"}, "unverified": {"账号读取尚未验证", "Account access not verified"}, "writeVerified": {"部署写入已验证", "Deployment access verified"}, "writeUnverified": {"部署写入尚未验证", "Deployment access not yet verified"},
		"tokenHelp": {"使用该账号的 Workers 编辑权限 API Token，保存后不再显示。", "Use an API Token with Workers edit permission for this account. The saved token is not displayed again."}, "storageHelp": {"Token 保存在本系统数据库中，数据库备份也包含 Token。", "Tokens are stored in the CMS database and included in its backups."},
		"accountSettings": {"账号连接与凭证", "Account connection and credentials"}, "verifyAccount": {"检查连接", "Check connection"}, "rotate": {"验证并更新 Token", "Verify and update token"}, "newToken": {"新的 API Token", "New API Token"}, "workerScope": {"限定 Worker（可选）", "Scoped Worker (optional)"}, "revision": {"凭证版本", "Credential revision"},
		"enabled": {"已启用", "Enabled"}, "disabled": {"已停用", "Disabled"}, "archived": {"已归档", "Archived"}, "draft": {"待初始化", "Not initialized"}, "initializing": {"初始化待核实", "Initialization pending"}, "ready": {"就绪", "Ready"}, "drifted": {"需核实远端状态", "Remote verification required"}, "enable": {"核实并启用", "Verify and enable"}, "disable": {"停用", "Disable"}, "restore": {"恢复为停用", "Restore as disabled"},
		"workerList": {"Worker 列表", "Workers"}, "addWorker": {"添加 Worker", "Add Worker"}, "emptyWorkers": {"该账号尚无 Worker 配置。", "This account has no Worker configurations yet."}, "workerName": {"Worker 名称", "Worker name"}, "displayName": {"显示名称", "Display name"}, "origin": {"公开 HTTPS 地址", "Public HTTPS origin"}, "optionalOrigin": {"可选，留空使用 workers.dev 地址", "Optional; leave blank to use workers.dev"}, "environment": {"环境", "Environment"}, "test": {"测试", "Test"}, "production": {"生产", "Production"}, "siteRevision": {"站点版本", "Site revision"}, "edit": {"编辑", "Edit"}, "more": {"更多操作", "More actions"}, "maintenance": {"页面维护", "Manage pages"}, "initialize": {"初始化 Worker", "Initialize Worker"}, "verifyWorker": {"核实 Worker", "Verify Worker"},
		"initializeHelp": {"确认仅初始化本系统专用 Worker，不覆盖其他站点。", "Initialize only a dedicated Worker for this CMS; do not overwrite another site."}, "saveWorker": {"保存 Worker 配置", "Save Worker configuration"}, "editWorker": {"编辑 Worker", "Edit Worker"}, "draftHelp": {"保存后在列表中初始化专用 Worker，核实成功后即可发布。", "After saving, initialize the dedicated Worker from its row. Publishing is available after verification."}, "editHelp": {"初始化后 Worker 名称固定。已有页面的域名与环境变更请使用“页面维护”，以同步更新静态页面。修改未发布 Worker 的地址或环境后，需要重新核实并启用。", "An initialized Worker's name is fixed. Use Manage pages to change the origin or environment of a published site and update its static pages together. Changing an unpublished Worker's origin or environment requires verification before enabling."},
		"deleteAccountTitle": {"删除账号配置", "Delete account configuration"}, "deleteAccountHelp": {"将移除此账号、其全部 Worker 的本地配置及保存的 Token。Cloudflare 上的 Worker 和已发布站点继续保留；历史任务及产物按原保留策略管理。", "Remove this account, all its local Worker configurations and saved tokens. Cloudflare Workers and published sites remain online. Historical jobs and artifacts retain their existing retention policy."},
		"deleteWorkerTitle": {"删除 Worker 配置", "Delete Worker configuration"}, "deleteWorkerHelp": {"仅删除本系统配置，Cloudflare Worker 和已发布站点继续保留。", "Remove only the local configuration. The Cloudflare Worker and its published site remain online."}, "deleteConsent": {"确认删除上述本地配置", "Delete the local configuration shown above"}, "busy": {"正在发布或初始化，完成后可编辑或删除。", "Publishing or initialization is in progress. Edit or delete after it completes."}, "archiveHelp": {"先恢复账号后再添加 Worker 或更新 Token。", "Restore this account before adding Workers or updating its token."},
		"publishHistory": {"报告导出与发布记录", "Report exports and publishing history"}, "workerSearch": {"搜索名称、Worker 或域名", "Search name, Worker or origin"}, "deleteConfirm": {"确认删除", "Confirm deletion"},
	}
	out := make(map[string]string, len(words))
	for k, v := range words {
		if lang == "en" {
			out[k] = v[1]
		} else {
			out[k] = v[0]
		}
	}
	return out
}
