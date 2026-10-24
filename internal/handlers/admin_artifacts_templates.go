package handlers

import (
	_ "embed"
	"time"
)

//go:embed admin_artifacts.js
var artifactManagementScript string

func artifactManagementText(lang string) map[string]string {
	english := lang == "en"
	result := make(map[string]string, len(artifactManagementLabels))
	for key, values := range artifactManagementLabels {
		result[key] = values[0]
		if english {
			result[key] = values[1]
		}
	}
	return result
}

var artifactManagementLabels = map[string][2]string{
	"title":          {"静态产物管理", "Artifact management"},
	"intro":          {"管理静态发布和导出的私有产物。当前发布、未决任务和人工固定保留的产物受引用保护；清理前会再次检查。", "Manage private artifacts used by static releases and exports. Current releases, pending jobs and pinned artifacts stay protected; cleanup checks references again."},
	"retention_help": {"默认保留正常产物90天、失败产物7天，实际以服务器配置为准。旧记录可能未保存文件类型。", "Default retention is 90 days for regular artifacts and 7 days for failed artifacts, subject to server configuration. Older records may have no saved file type."},
	"preview":        {"清理预览", "Preview cleanup"}, "resume": {"继续中断清理", "Resume interrupted cleanup"},
	"hash": {"对象哈希", "Object hash"}, "type": {"类型", "Type"}, "size": {"大小", "Size"}, "state": {"状态", "State"}, "created": {"创建时间", "Created"},
	"protection": {"引用与保留", "References and retention"}, "actions": {"操作", "Actions"}, "details": {"详情", "Details"},
	"search": {"哈希搜索", "Search hash"}, "search_hint": {"输入哈希前缀", "Enter a hash prefix"}, "all_states": {"全部状态", "All states"},
	"all_pins": {"全部固定状态", "All pin states"}, "pinned": {"固定保留", "Pinned"}, "unpinned": {"未固定", "Unpinned"},
	"filter": {"筛选", "Filter"}, "clear": {"重置", "Reset"},
	"available": {"可用", "Available"}, "deleting": {"删除中", "Deleting"}, "deleted": {"已清理", "Deleted"}, "failed": {"失败产物", "Failed artifact"},
	"not_recorded": {"未记录", "Not recorded"}, "refs": {"活跃引用", "Active references"}, "not_referenced": {"无活跃引用", "No active references"},
	"references_more": {"引用较多，仅显示前50条；计数最多显示100+。全部活跃引用仍受清理保护。", "Only the first 50 references are shown; counts are capped at 100+. All active references remain protected from cleanup."},
	"retention":       {"仍在保留期", "Within retention period"}, "retention_until": {"保留期至", "Retention until"},
	"eligible": {"可进入清理预览", "Eligible for cleanup preview"}, "retained": {"保留中", "Retained"},
	"current_release": {"当前发布", "Current release"}, "pending_job": {"未决发布任务", "Pending deployment job"},
	"release_history": {"历史发布", "Release history"}, "export_history": {"导出记录", "Export history"},
	"recovery_preview": {"恢复预览", "Recovery preview"}, "other_reference": {"其他受保护引用", "Other protected reference"},
	"referenced": {"有活跃引用，禁止清理", "Active references prevent cleanup"},
	"close":      {"关闭", "Close"}, "loading": {"正在读取产物详情…", "Loading artifact details…"},
	"unavailable":           {"产物管理暂不可用", "Artifact management is temporarily unavailable"},
	"list_unavailable":      {"当前服务尚不支持产物列表，可继续使用清理预览和恢复功能。", "This service does not support artifact browsing yet. Cleanup preview and recovery remain available."},
	"list_error":            {"无法读取产物列表。请检查筛选条件，或稍后重新加载。", "Could not load artifacts. Check the filters or reload this page later."},
	"detail_error":          {"无法读取产物详情，请关闭后重试。", "Could not load artifact details. Close this dialog and try again."},
	"not_found":             {"未找到此产物，列表可能已变化。", "This artifact was not found. The list may have changed."},
	"invalid_hash":          {"请输入64位小写十六进制对象哈希。", "Enter a 64-character lowercase hexadecimal object hash."},
	"filter_error":          {"筛选条件或页码无效。", "The filter or page number is invalid."},
	"form_error":            {"表单格式或大小不正确。", "The submitted form is invalid or too large."},
	"method_error":          {"只允许 GET 或 POST。", "Only GET and POST are allowed."},
	"confirmation_required": {"请先明确确认本次解除固定或清理操作。", "Explicit confirmation is required before unpinning or cleanup."},
	"invalid_plan":          {"请输入原始的64位清理计划编号。", "Enter the original 64-character cleanup plan ID."},
	"pin_unavailable":       {"固定保留功能暂不可用。", "Pinning is temporarily unavailable."},
	"pin_saved":             {"固定状态已更新。解除固定不会立即删除，也不会解除发布或任务引用。", "Pin state updated. Unpinning does not delete the object or remove release and job references."},
	"cleanup_done":          {"本次候选清理已完成。", "Cleanup of the confirmed candidates is complete."},
	"resume_done":           {"已继续原计划中已开始的删除；未开始的候选需要重新预览。", "Resumed deletions already started by that plan. Preview again for candidates that had not started."},
	"unknown_action":        {"未知操作。", "Unknown action."},
	"action_error":          {"操作未完成。计划可能已过期，或产物已被新任务引用。中断的删除可使用原计划编号继续；其他情况请重新预览。", "The operation did not finish. The plan may have expired or new jobs may reference an artifact. Resume an interrupted deletion with its original plan ID; otherwise create a new preview."},
	"empty_title":           {"没有匹配的产物", "No matching artifacts"},
	"empty_body":            {"静态发布或导出后，私有产物会显示在这里。也可以调整筛选条件。", "Private artifacts appear here after a static release or export. You can also adjust the filters."},
	"detail_title":          {"产物详情", "Artifact details"}, "pin": {"固定保留", "Pin artifact"}, "unpin": {"解除固定", "Unpin artifact"},
	"pin_help":      {"固定保留没有自动过期时间。解除固定只移除人工保留，现有发布和任务引用继续受保护。", "Pins have no automatic expiry. Unpinning removes the manual hold; existing release and job references remain protected."},
	"unpin_confirm": {"确认解除人工固定；该产物可能进入以后手动确认的清理候选。", "Remove the manual pin. This artifact may become eligible for a future confirmed cleanup."},
	"preview_title": {"清理预览", "Cleanup preview"}, "plan": {"计划编号", "Plan ID"}, "expires": {"有效期至", "Expires"},
	"cleanup_help":    {"每批最多100个对象。确认时重新检查引用、固定状态和保留期；有变化会停止。", "Each batch contains at most 100 objects. Confirmation rechecks references, pins and retention; changes stop cleanup."},
	"cleanup_scope":   {"只清理此预览中的无引用私有对象，不删除文章、模板、旧版快照或 Cloudflare 资源。", "Cleanup removes only the unreferenced private objects in this preview. Articles, templates, legacy snapshots and Cloudflare resources remain unchanged."},
	"cleanup_confirm": {"确认永久删除以上无引用对象，且已单独验证需要保留的备份。", "Permanently delete these unreferenced objects. I have separately verified any backups I need."},
	"confirm_cleanup": {"确认清理", "Confirm cleanup"}, "no_candidates": {"没有符合清理条件的产物。", "No artifacts are eligible for cleanup."},
	"resume_title":  {"继续中断的清理", "Resume interrupted cleanup"},
	"resume_help":   {"仅继续原计划已标记为删除中的对象。不会扩大范围，也不处理尚未开始删除的候选。", "Resume only objects already marked for deletion by the original plan. This does not expand the scope or start other candidates."},
	"resume_digest": {"原计划编号", "Original plan ID"}, "resume_confirm": {"确认继续此计划中已开始的删除。", "Resume deletions already started by this plan."},
	"resume_button": {"继续清理", "Resume cleanup"}, "no_expiry": {"无到期时间", "No expiry"},
	"reference_identifier": {"关联标识", "Related ID"}, "previous": {"上一页", "Previous"}, "next": {"下一页", "Next"}, "page": {"页", "Page"},
	"manual_pin": {"按哈希固定保留", "Pin by hash"},
}

func init() {
	adminTemplateFuncMap["artifactText"] = artifactManagementText
	adminTemplateFuncMap["artifactTime"] = func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		return t.UTC().Format("2006-01-02 15:04 UTC")
	}
	adminTemplateFuncMap["artifactShortHash"] = func(hash string) string {
		if len(hash) > 16 {
			return hash[:12] + "…" + hash[len(hash)-4:]
		}
		return hash
	}
	adminTemplates["artifacts"] = adminLayoutStart + artifactManagementBody + `<script>` + artifactManagementScript + `</script>` + adminLayoutEnd
}

const artifactManagementBody = `
{{$t := artifactText .Lang}}
<style>
.artifact-management{max-width:1400px}.artifact-management .artifact-intro{color:var(--text-muted);max-width:85ch;line-height:1.65;margin:8px 0}.artifact-toolbar,.artifact-actions,.artifact-pagination{display:flex;align-items:center;gap:12px;flex-wrap:wrap}.artifact-management .page-header{gap:16px;flex-wrap:wrap}.artifact-toolbar label{display:flex;flex-direction:column;gap:8px;font-size:.875rem}.artifact-toolbar input,.artifact-toolbar select,.artifact-dialog input[type=text]{background:var(--bg-card);border:1px solid var(--border);border-radius:var(--radius);color:var(--text);font:inherit;min-height:44px;padding:8px 12px;max-width:100%;box-sizing:border-box}.artifact-toolbar .artifact-search{flex:1;min-width:180px}.artifact-toolbar{padding:20px;border-bottom:1px solid var(--border);align-items:flex-end}.artifact-table-wrap{overflow:auto}.artifact-table{border-collapse:collapse;width:100%;min-width:850px}.artifact-table th,.artifact-table td{text-align:left;padding:16px;vertical-align:top;border-bottom:1px solid var(--border)}.artifact-table th{font-size:.8rem;color:var(--text-muted);font-weight:600;white-space:nowrap}.artifact-table .artifact-size{text-align:right;white-space:nowrap}.artifact-table code{font-size:.85rem}.artifact-management code,.artifact-dialog code{overflow-wrap:anywhere;white-space:normal}.artifact-management small,.artifact-dialog small{display:block;color:var(--text-muted);margin-top:6px;line-height:1.5}.artifact-status{display:inline-block;padding:3px 9px;background:var(--bg-tertiary);border:1px solid var(--border);border-radius:99px;font-size:.8rem;line-height:1.5}.artifact-management .btn,.artifact-dialog .btn{min-height:44px;white-space:normal}.artifact-actions form{margin:0}.artifact-empty{padding:48px 24px;text-align:center}.artifact-empty p{color:var(--text-muted)}.artifact-pagination{justify-content:flex-end;padding:16px}.artifact-feedback{padding:16px;border:1px solid var(--border);border-radius:var(--radius);margin:20px 0;overflow-wrap:anywhere}.artifact-feedback[role=alert]{color:var(--danger)}.artifact-dialog{background:var(--bg-card);color:var(--text);border:1px solid var(--border);border-radius:var(--radius);padding:0;width:min(860px,calc(100% - 32px));max-height:90vh;max-height:90svh;overflow:auto;box-sizing:border-box;box-shadow:0 16px 60px rgba(0,0,0,.35)}.artifact-dialog::backdrop{background:rgba(0,0,0,.55)}.artifact-dialog header,.artifact-dialog footer{padding:20px 24px;display:flex;gap:16px;align-items:center;justify-content:space-between}.artifact-dialog header{border-bottom:1px solid var(--border)}.artifact-dialog h2{margin:0;font-size:1.25rem}.artifact-dialog-body{padding:24px}.artifact-dialog p{line-height:1.65;overflow-wrap:anywhere}.artifact-dialog dl{display:grid;grid-template-columns:minmax(110px,1fr) minmax(0,3fr);gap:16px;margin:0}.artifact-dialog dt{color:var(--text-muted)}.artifact-dialog dd{margin:0;overflow-wrap:anywhere}.artifact-dialog section+section{border-top:1px solid var(--border);padding-top:24px;margin-top:24px}.artifact-dialog ul{padding-left:20px;line-height:1.7}.artifact-dialog label{display:block;line-height:1.65}.artifact-dialog input[type=checkbox]{width:18px;height:18px;vertical-align:middle;margin-right:8px}.artifact-dialog .artifact-check{margin:20px 0}.artifact-dialog [hidden],.artifact-management [hidden]{display:none!important}.artifact-dialog .artifact-plan{font-size:.85rem}.artifact-dialog :focus-visible,.artifact-management :focus-visible{outline:2px solid var(--primary);outline-offset:3px}.artifact-resume{margin-top:20px}.artifact-dialog .artifact-table{min-width:520px}.artifact-dialog .artifact-pin-form{margin-top:20px}.artifact-dialog .artifact-full-input{display:block;width:100%;margin-top:8px}@media(max-width:640px){.artifact-toolbar label,.artifact-toolbar .btn{width:100%}.artifact-toolbar{padding:16px}.artifact-dialog header,.artifact-dialog-body{padding:20px}.artifact-dialog dl{grid-template-columns:1fr;gap:6px}.artifact-dialog dd{margin-bottom:14px}.artifact-dialog .artifact-actions .btn{flex:1}.artifact-management .page-header .artifact-actions{width:100%}}
</style>
<div class="artifact-management" id="artifact-management">
<div class="page-header"><h1>{{$t.title}}</h1><div class="artifact-actions"><button type="button" class="btn btn-secondary" data-artifact-open="artifact-resume-dialog">{{$t.resume}}</button><form method="POST" action="{{.ListURL}}" data-artifact-form><input type="hidden" name="gorilla.csrf.Token" value="{{.CSRFToken}}"><input type="hidden" name="action" value="preview"><button class="btn btn-primary" type="submit">{{$t.preview}}</button></form></div></div>
<p class="artifact-intro">{{$t.intro}}</p><p class="artifact-intro">{{$t.retention_help}}</p>
{{if .Notice}}<div class="artifact-feedback" role="status">{{.Notice}}</div>{{end}}{{if .Error}}<div class="artifact-feedback" role="alert">{{.Error}}</div>{{end}}
<section class="card" style="margin-top:24px">
<form class="artifact-toolbar" method="GET" action="/cm/artifacts"><input type="hidden" name="lang" value="{{.Lang}}"><label class="artifact-search">{{$t.search}}<input name="q" value="{{.Search}}" placeholder="{{$t.search_hint}}" maxlength="64" pattern="[a-fA-F0-9]{1,64}"></label><label>{{$t.state}}<select name="state"><option value="">{{$t.all_states}}</option><option value="available" {{if eq .State "available"}}selected{{end}}>{{$t.available}}</option><option value="deleting" {{if eq .State "deleting"}}selected{{end}}>{{$t.deleting}}</option><option value="deleted" {{if eq .State "deleted"}}selected{{end}}>{{$t.deleted}}</option><option value="failed" {{if eq .State "failed"}}selected{{end}}>{{$t.failed}}</option></select></label><label>{{$t.protection}}<select name="pin"><option value="">{{$t.all_pins}}</option><option value="pinned" {{if eq .Pin "pinned"}}selected{{end}}>{{$t.pinned}}</option><option value="unpinned" {{if eq .Pin "unpinned"}}selected{{end}}>{{$t.unpinned}}</option></select></label><button class="btn btn-secondary" type="submit">{{$t.filter}}</button><a class="btn btn-secondary" href="/cm/artifacts?lang={{.Lang}}">{{$t.clear}}</a></form>
<div class="artifact-table-wrap"><table class="artifact-table" id="artifact-list"><thead><tr><th scope="col">{{$t.hash}}</th><th scope="col">{{$t.type}}</th><th scope="col" class="artifact-size">{{$t.size}}</th><th scope="col">{{$t.state}}</th><th scope="col">{{$t.created}}</th><th scope="col">{{$t.protection}}</th><th scope="col">{{$t.actions}}</th></tr></thead><tbody>
{{range .Listing.Items}}<tr><td><code title="{{.Hash}}">{{artifactShortHash .Hash}}</code></td><td>{{if .MIME}}{{.MIME}}{{else}}{{$t.not_recorded}}{{end}}</td><td class="artifact-size">{{formatBytes .Size}}</td><td><span class="artifact-status">{{index $t .State}}</span>{{if .Failed}}<small>{{$t.failed}}</small>{{end}}</td><td>{{artifactTime .CreatedAt}}</td><td>{{if .Pinned}}<span class="artifact-status">{{$t.pinned}}</span>{{end}}{{if .ReferenceCount}}<small>{{$t.refs}}: {{.ReferenceCount}}{{if .ReferencesTruncated}}+{{end}}</small>{{else}}<small>{{$t.not_referenced}}</small>{{end}}{{if .CleanupEligible}}<small>{{$t.eligible}}</small>{{else if and (eq .State "available") (not .Protected)}}<small>{{$t.retained}}</small>{{end}}</td><td><div class="artifact-actions"><button type="button" class="btn btn-secondary" data-artifact-detail="{{.Hash}}">{{$t.details}}</button>{{if and $.CanPin (eq .State "available")}}{{if .Pinned}}<button type="button" class="btn btn-secondary" data-artifact-detail="{{.Hash}}" data-artifact-focus="pin">{{$t.unpin}}</button>{{else}}<form method="POST" action="{{$.ListURL}}" data-artifact-form><input type="hidden" name="gorilla.csrf.Token" value="{{$.CSRFToken}}"><input type="hidden" name="action" value="pin"><input type="hidden" name="hash" value="{{.Hash}}"><button type="submit" class="btn btn-secondary">{{$t.pin}}</button></form>{{end}}{{end}}</div></td></tr>
{{else}}<tr><td colspan="7"><div class="artifact-empty">{{if $.ListError}}<p role="alert">{{$.ListError}}</p>{{else if not $.CanBrowse}}<p>{{$t.list_unavailable}}</p>{{else}}<strong>{{$t.empty_title}}</strong><p>{{$t.empty_body}}</p>{{end}}</div></td></tr>{{end}}
</tbody></table></div><nav class="artifact-pagination" aria-label="{{$t.title}}">{{if .PreviousURL}}<a class="btn btn-secondary" href="{{.PreviousURL}}">{{$t.previous}}</a>{{end}}<span>{{$t.page}} {{with .Listing}}{{.Page}}{{else}}1{{end}}</span>{{if .NextURL}}<a class="btn btn-secondary" href="{{.NextURL}}">{{$t.next}}</a>{{end}}</nav>
</section>
{{if and .CanPin (not .CanBrowse)}}<details class="artifact-resume"><summary>{{$t.manual_pin}}</summary><p>{{$t.pin_help}}</p><form method="POST" action="{{.ListURL}}" data-artifact-form><input type="hidden" name="gorilla.csrf.Token" value="{{.CSRFToken}}"><label>{{$t.hash}} <input name="hash" required minlength="64" maxlength="64" pattern="[a-f0-9]{64}"></label><label><input type="checkbox" name="confirmed" value="yes">{{$t.unpin_confirm}}</label><button name="action" value="pin" class="btn btn-secondary">{{$t.pin}}</button> <button name="action" value="unpin" class="btn btn-secondary">{{$t.unpin}}</button></form></details>{{end}}
</div>
<dialog id="artifact-detail-dialog" class="artifact-dialog" aria-labelledby="artifact-detail-title"><header><h2 id="artifact-detail-title">{{$t.detail_title}}</h2><button type="button" class="btn btn-secondary" data-artifact-close>{{$t.close}}</button></header><div class="artifact-dialog-body"><p id="artifact-detail-status" role="status" aria-live="polite"></p><p id="artifact-detail-error" role="alert"></p><div id="artifact-detail-content" hidden><dl id="artifact-detail-fields"></dl><section><h3>{{$t.protection}}</h3><ul id="artifact-protection-reasons"></ul><p>{{$t.pin_help}}</p><div id="artifact-pin-section" hidden><form method="POST" action="{{.ListURL}}" id="artifact-pin-form" class="artifact-pin-form" data-artifact-form><input type="hidden" name="gorilla.csrf.Token" value="{{.CSRFToken}}"><input type="hidden" name="action" id="artifact-pin-action"><input type="hidden" name="hash" id="artifact-pin-hash"><label id="artifact-unpin-consent" class="artifact-check" hidden><input type="checkbox" id="artifact-unpin-confirm" name="confirmed" value="yes">{{$t.unpin_confirm}}</label><button type="submit" class="btn btn-secondary" id="artifact-pin-submit"></button></form></div></section><section><h3>{{$t.refs}}</h3><p id="artifact-reference-limit" hidden>{{$t.references_more}}</p><ul id="artifact-references"></ul></section></div></div></dialog>
<dialog id="artifact-cleanup-dialog" class="artifact-dialog" aria-labelledby="artifact-cleanup-title" {{if .Plan}}data-open-on-load open{{end}}><header><h2 id="artifact-cleanup-title">{{$t.preview_title}}</h2><button type="button" class="btn btn-secondary" data-artifact-close>{{$t.close}}</button></header><div class="artifact-dialog-body"><p>{{$t.cleanup_scope}}</p>{{with .Plan}}<p>{{$t.plan}}: <code class="artifact-plan">{{.Digest}}</code></p><p>{{$t.expires}}: {{artifactTime .ExpiresAt}}</p><p>{{$t.cleanup_help}}</p><div class="artifact-table-wrap"><table class="artifact-table"><thead><tr><th scope="col">{{$t.hash}}</th><th scope="col" class="artifact-size">{{$t.size}}</th></tr></thead><tbody>{{range .Items}}<tr><td><code>{{.Hash}}</code></td><td class="artifact-size">{{formatBytes .Size}}</td></tr>{{else}}<tr><td colspan="2">{{$t.no_candidates}}</td></tr>{{end}}</tbody></table></div>{{if .Items}}<form method="POST" action="{{$.ListURL}}" data-artifact-form><input type="hidden" name="gorilla.csrf.Token" value="{{$.CSRFToken}}"><input type="hidden" name="action" value="confirm"><input type="hidden" name="digest" value="{{.Digest}}"><label class="artifact-check"><input type="checkbox" name="confirmed" value="yes" required>{{$t.cleanup_confirm}}</label><button type="submit" class="btn btn-primary">{{$t.confirm_cleanup}}</button></form>{{end}}{{end}}</div></dialog>
<dialog id="artifact-resume-dialog" class="artifact-dialog" aria-labelledby="artifact-resume-title" {{if .ResumeOpen}}data-open-on-load open{{end}}><header><h2 id="artifact-resume-title">{{$t.resume_title}}</h2><button type="button" class="btn btn-secondary" data-artifact-close>{{$t.close}}</button></header><div class="artifact-dialog-body">{{if .Error}}<p role="alert">{{.Error}}</p>{{end}}<p>{{$t.resume_help}}</p><form method="POST" action="{{.ListURL}}" data-artifact-form><input type="hidden" name="gorilla.csrf.Token" value="{{.CSRFToken}}"><input type="hidden" name="action" value="resume"><label>{{$t.resume_digest}}<input class="artifact-full-input" type="text" name="digest" value="{{.Digest}}" required minlength="64" maxlength="64" pattern="[a-f0-9]{64}" autocomplete="off"></label><label class="artifact-check"><input type="checkbox" name="confirmed" value="yes" required>{{$t.resume_confirm}}</label><button type="submit" class="btn btn-primary">{{$t.resume_button}}</button></form></div></dialog>
`
