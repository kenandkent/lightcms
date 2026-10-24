package handlers

import (
	"context"
	"github.com/gorilla/mux"
	"github.com/metavert-io/lightcms/v7/internal/models"
	sd "github.com/metavert-io/lightcms/v7/internal/product/staticdelivery"
	deliveryapi "github.com/metavert-io/lightcms/v7/internal/product/staticdelivery/httpapi"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"net/http"
	"time"
)

type StaticDeliveryAdmin struct {
	Capacity    capacityReporter
	Artifacts   artifactMaintenance
	Accounts    *sd.AccountService
	Targets     *sd.TargetService
	Unavailable string
	Commands    *sd.CommandService
	ContentAPI  *deliveryapi.Handlers
}

func (h *Handler) SetStaticDeliveryAdmin(service *StaticDeliveryAdmin) { h.staticDelivery = service }
func (h *Handler) deliveryAdminAllowed(w http.ResponseWriter, r *http.Request) bool {
	if h.auth == nil {
		http.Error(w, "需要管理员权限", 403)
		return false
	}
	user, ok := h.auth.GetCurrentUser(r)
	if !ok || user.Role != models.RoleAdmin {
		http.Error(w, "需要管理员权限", 403)
		return false
	}
	return true
}
func (h *Handler) AdminStaticDeliveryPage(w http.ResponseWriter, r *http.Request) {
	if !h.deliveryAdminAllowed(w, r) {
		return
	}
	h.renderDeliveryPage(w, r, "")
}
func (h *Handler) renderDeliveryPage(w http.ResponseWriter, r *http.Request, message string) {
	w.Header().Set("Cache-Control", "no-store")
	data := map[string]any{"Notice": message, "Saved": r.URL.Query().Get("saved") == "1"}
	service := h.staticDelivery
	if service == nil || service.Unavailable != "" {
		data["ModuleError"] = "静态发布模块暂不可用，原有内容服务不受影响。"
	} else {
		accounts, e := service.Accounts.List(r.Context())
		if e != nil {
			data["ModuleError"] = "无法读取 Cloudflare 账号列表。"
		} else {
			data["Accounts"] = accounts
		}
		targets, e := service.Targets.List(r.Context())
		if e != nil {
			data["ModuleError"] = "无法读取 Worker 目标列表。"
		} else {
			data["Targets"] = targets
		}
	}
	h.renderAdmin(w, r, "static_delivery", data)
}
func (h *Handler) AdminStaticDeliveryAction(w http.ResponseWriter, r *http.Request) {
	if !h.deliveryAdminAllowed(w, r) {
		return
	}
	if r.Method != "POST" {
		http.Error(w, "只允许 POST", 405)
		return
	}
	if h.staticDelivery == nil || h.staticDelivery.Unavailable != "" {
		http.Error(w, "静态发布模块暂不可用", 503)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 24<<10)
	if e := r.ParseForm(); e != nil {
		http.Error(w, "表单格式或大小不正确", 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	vars := mux.Vars(r)
	action := vars["action"]
	kind := vars["kind"]
	idText := vars["id"]
	var id primitive.ObjectID
	var err error
	if idText != "" {
		id, err = primitive.ObjectIDFromHex(idText)
		if err != nil {
			http.Error(w, "记录标识不正确", 400)
			return
		}
	}
	s := h.staticDelivery
	if kind == "accounts" {
		switch action {
		case "create":
			_, err = s.Accounts.Create(ctx, r.FormValue("name"), r.FormValue("account_id"), r.FormValue("token"), r.FormValue("remark"))
		case "verify":
			_, err = s.Accounts.Verify(ctx, id, r.FormValue("worker_name"))
		case "rotate":
			_, err = s.Accounts.Rotate(ctx, id, r.FormValue("token"), r.FormValue("worker_name"))
		case "enable":
			err = s.Accounts.SetEnabled(ctx, id, true)
		case "disable":
			err = s.Accounts.SetEnabled(ctx, id, false)
		case "archive":
			err = s.Accounts.Archive(ctx, id)
		case "restore":
			err = s.Accounts.Restore(ctx, id)
		default:
			http.Error(w, "不支持的账号操作", 400)
			return
		}
	} else if kind == "targets" {
		switch action {
		case "create":
			var account primitive.ObjectID
			account, err = primitive.ObjectIDFromHex(r.FormValue("account_ref"))
			if err == nil {
				_, err = s.Targets.Create(ctx, account, r.FormValue("name"), r.FormValue("worker_name"), r.FormValue("environment"), r.FormValue("public_origin"))
			}
		case "verify":
			_, err = s.Targets.Verify(ctx, id)
		case "initialize":
			if r.FormValue("confirm_initialize") != "yes" {
				http.Error(w, "请明确确认创建专用 Worker", 428)
				return
			}
			_, err = s.Targets.Initialize(ctx, id)
		case "enable":
			err = s.Targets.SetEnabled(ctx, id, true)
		case "disable":
			err = s.Targets.SetEnabled(ctx, id, false)
		case "archive":
			err = s.Targets.Archive(ctx, id)
		case "restore":
			err = s.Targets.Restore(ctx, id)
		default:
			http.Error(w, "不支持的目标操作", 400)
			return
		}
	} else {
		http.Error(w, "不支持的操作", 400)
		return
	}
	// Never echo submitted form data, tokens, or upstream response bodies.
	r.Form.Del("token")
	r.PostForm.Del("token")
	if err != nil {
		message := "操作未完成，错误码：" + sd.CodeOf(err) + "。请检查账号权限、目标状态和连接检查结果。"
		w.WriteHeader(sd.StatusForError(err))
		h.renderDeliveryPage(w, r, message)
		return
	}
	http.Redirect(w, r, "/cm/static-delivery?saved=1", http.StatusSeeOther)
}
