package handlers

import (
	"context"
	"github.com/metavert-io/lightcms/v7/internal/models"
	"github.com/metavert-io/lightcms/v7/internal/product/artifacts"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"net/http"
	"time"
)

type artifactMaintenance interface {
	PreviewCleanup(context.Context, string) (artifacts.CleanupPlan, error)
	ConfirmCleanup(context.Context, string, string, bool) error
	ResumeCleanup(context.Context, string, string, bool) error
}

type artifactPinning interface {
	PinObject(context.Context, string, bool) error
}

func (h *Handler) AdminArtifactsPage(w http.ResponseWriter, r *http.Request) {
	if !h.deliveryAdminAllowed(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if h.staticDelivery == nil || h.staticDelivery.Artifacts == nil {
		http.Error(w, "产物管理暂不可用", 503)
		return
	}
	data := map[string]any{}
	pinning, canPin := h.staticDelivery.Artifacts.(artifactPinning)
	data["CanPin"] = canPin
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "表单格式错误", 400)
			return
		}
		user, _ := h.auth.GetCurrentUser(r)
		owner := user.ID
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		var err error
		switch r.FormValue("action") {
		case "pin", "unpin":
			if !canPin {
				http.Error(w, "固定保留功能暂不可用", 503)
				return
			}
			if r.FormValue("action") == "unpin" && r.FormValue("confirmed") != "yes" {
				http.Error(w, "解除固定保留需要明确确认", 400)
				return
			}
			err = pinning.PinObject(ctx, r.FormValue("hash"), r.FormValue("action") == "pin")
			if err == nil {
				data["Notice"] = "固定保留状态已更新；解除固定不等于立即删除，清理仍须重新预览并确认。"
			}
		case "preview":
			var plan artifacts.CleanupPlan
			plan, err = h.staticDelivery.Artifacts.PreviewCleanup(ctx, owner)
			if err == nil {
				data["Plan"] = plan
			}
		case "confirm":
			err = h.staticDelivery.Artifacts.ConfirmCleanup(ctx, owner, r.FormValue("digest"), r.FormValue("confirmed") == "yes")
			if err == nil {
				data["Notice"] = "本次候选清理已完成。"
			}
		case "resume":
			err = h.staticDelivery.Artifacts.ResumeCleanup(ctx, owner, r.FormValue("digest"), r.FormValue("confirmed") == "yes")
			if err == nil {
				data["Notice"] = "已核实并继续该计划中已开始的删除；未开始的候选需要重新预览。"
			}
		default:
			http.Error(w, "未知操作", 400)
			return
		}
		if err != nil {
			data["Error"] = "操作未完成。确认是否已勾选、计划是否过期或文件是否已被新任务引用。中断的删除可使用原计划编号继续；其他情况请重新预览。"
			data["Digest"] = r.FormValue("digest")
		} else if action := r.FormValue("action"); action != "preview" && h.auditService != nil {
			uid, _ := primitive.ObjectIDFromHex(user.ID)
			h.auditService.LogAsync(models.AuditLog{UserID: uid, UserEmail: user.Email, Action: "artifact." + action, Resource: "artifact", ResourceID: r.FormValue("hash"), Details: map[string]any{"cleanup_plan": r.FormValue("digest")}, IPAddress: r.RemoteAddr})
		}
	}
	h.renderAdmin(w, r, "artifacts", data)
}
func init() {
	adminTemplates["artifacts"] = adminLayoutStart + `
<div class="page-header"><h1>静态产物管理</h1></div>
<p>只管理新对象存储中的私有产物，不删除文章、模板、旧版快照或 Cloudflare 资源。默认保留期：正常产物90天，失败产物7天，实际以服务器配置为准；当前发布、未决任务及固定保留的文件不会进入候选。</p>
{{if .CanPin}}<details class="card" style="padding:24px;margin:24px 0"><summary>手动固定保留 / 解除固定</summary><p>输入对象 SHA-256（清理预览中可查看）。固定无自动过期时间；解除固定不会移除当前发布或任务引用。</p><form method="POST" action="/cm/artifacts"><input type="hidden" name="gorilla.csrf.Token" value="{{.CSRFToken}}"><label>对象哈希 <input name="hash" required minlength="64" maxlength="64" pattern="[a-f0-9]{64}" style="width:100%"></label><label style="display:block;margin:20px 0"><input type="checkbox" name="confirmed" value="yes"> 确认解除固定后可进入后续清理候选</label><button name="action" value="pin" class="btn btn-secondary">固定保留</button> <button name="action" value="unpin" class="btn btn-secondary">解除固定</button></form></details>{{end}}
{{if .Notice}}<p role="status">{{.Notice}}</p>{{end}}{{if .Error}}<p role="alert">{{.Error}}</p>{{end}}
<form method="POST" action="/cm/artifacts" style="margin:24px 0"><input type="hidden" name="gorilla.csrf.Token" value="{{.CSRFToken}}"><input type="hidden" name="action" value="preview"><button class="btn btn-secondary">预览可清理文件（不删除）</button></form>
{{with .Plan}}<section class="card" style="padding:24px"><h2>清理预览</h2><p>计划编号：<code style="overflow-wrap:anywhere">{{.Digest}}</code></p><p>有效期至 {{.ExpiresAt}}。每批最多100个对象，确认时会再次检查引用。</p><ul>{{range .Items}}<li style="overflow-wrap:anywhere">{{.Hash}} · {{.Size}} bytes</li>{{else}}<li>没有符合清理条件的文件。</li>{{end}}</ul>
{{if .Items}}<form method="POST" action="/cm/artifacts"><input type="hidden" name="gorilla.csrf.Token" value="{{$.CSRFToken}}"><input type="hidden" name="action" value="confirm"><input type="hidden" name="digest" value="{{.Digest}}"><label><input type="checkbox" name="confirmed" value="yes" required> 确认永久删除以上无引用对象；需要保留的备份已单独验证</label><p><button class="btn btn-secondary">确认清理</button></p></form>{{end}}</section>{{end}}
<details style="margin-top:24px"><summary>继续中断的清理</summary><p>仅处理原计划已经标记为删除中的对象，不扩大范围，也不处理未开始的候选。</p><form method="POST" action="/cm/artifacts"><input type="hidden" name="gorilla.csrf.Token" value="{{.CSRFToken}}"><input type="hidden" name="action" value="resume"><label>原计划编号 <input name="digest" value="{{.Digest}}" required minlength="64" maxlength="64"></label><p><label><input type="checkbox" name="confirmed" value="yes" required> 确认继续已开始的删除</label></p><button class="btn btn-secondary">继续清理</button></form></details>
` + adminLayoutEnd
}
