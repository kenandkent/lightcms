package handlers

import (
	"context"
	"github.com/gorilla/mux"
	"github.com/metavert-io/lightcms/v7/internal/i18n"
	"github.com/metavert-io/lightcms/v7/internal/models"
	sd "github.com/metavert-io/lightcms/v7/internal/product/staticdelivery"
	deliveryapi "github.com/metavert-io/lightcms/v7/internal/product/staticdelivery/httpapi"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"net/http"
	"strconv"
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
	data := map[string]any{"Notice": message, "Saved": r.URL.Query().Get("saved") == "1", "CF": cloudflareLabels(i18n.LangFromRequest(r)), "OpenAccount": r.URL.Query().Get("account")}
	if r.FormValue("account_ref") != "" {
		data["OpenAccount"] = r.FormValue("account_ref")
	}
	service := h.staticDelivery
	if service == nil || service.Unavailable != "" {
		data["ModuleError"] = "静态发布模块暂不可用，原有内容服务不受影响。"
	} else {
		accounts, e := service.Accounts.List(r.Context())
		if e != nil {
			data["ModuleError"] = "无法读取 Cloudflare 账号列表。"
		}
		targets, e := service.Targets.List(r.Context())
		if e != nil {
			data["ModuleError"] = "无法读取 Worker 目标列表。"
		} else {
			data["Targets"] = targets
		}
		views := make([]cloudflareAccountView, 0, len(accounts))
		for _, account := range accounts {
			view := cloudflareAccountView{Account: account, Workers: []sd.Target{}}
			for _, target := range targets {
				if target.AccountRef == account.ID {
					view.Workers = append(view.Workers, target)
				}
			}
			views = append(views, view)
		}
		data["Accounts"] = views
	}
	h.renderAdmin(w, r, "static_delivery", data)
}
func (h *Handler) AdminStaticDeliveryAction(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
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
	accountRef := ""
	if kind == "accounts" {
		accountRef = idText
		switch action {
		case "create":
			var created sd.Account
			created, err = s.Accounts.Create(ctx, r.FormValue("name"), r.FormValue("account_id"), r.FormValue("token"), r.FormValue("remark"))
			if err == nil {
				accountRef = created.ID.Hex()
				auditResource(r, accountRef)
			}
		case "delete":
			if r.FormValue("confirm_delete") != "yes" {
				http.Error(w, "Explicit deletion confirmation required", 428)
				return
			}
			err = s.Accounts.Delete(ctx, id)
			accountRef = ""
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
		if idText != "" {
			var target sd.Target
			target, err = s.Targets.Get(ctx, id)
			if err == nil {
				accountRef = target.AccountRef.Hex()
			}
			if err != nil {
				http.Error(w, sd.CodeOf(err), sd.StatusForError(err))
				return
			}
			if scope := r.FormValue("account_ref"); scope != "" && scope != accountRef {
				http.Error(w, sd.CodeNotFound, 404)
				return
			}
		}
		switch action {
		case "create":
			var account primitive.ObjectID
			account, err = primitive.ObjectIDFromHex(r.FormValue("account_ref"))
			if err != nil {
				err = &sd.Error{Code: sd.CodeInvalidRequest, Message: "invalid account reference"}
			}
			if err == nil {
				accountRef = account.Hex()
				var created sd.Target
				created, err = s.Targets.Create(ctx, account, r.FormValue("name"), r.FormValue("worker_name"), r.FormValue("environment"), r.FormValue("public_origin"))
				if err == nil {
					auditResource(r, created.ID.Hex())
				}
			}
		case "update":
			var account primitive.ObjectID
			account, err = primitive.ObjectIDFromHex(r.FormValue("account_ref"))
			revision, parseErr := strconv.ParseInt(r.FormValue("expected_config_revision"), 10, 64)
			managementRevision, managementParseErr := strconv.ParseInt(r.FormValue("expected_management_revision"), 10, 64)
			if err != nil || parseErr != nil || revision < 1 || managementParseErr != nil || managementRevision < 0 {
				err = &sd.Error{Code: sd.CodeInvalidRequest, Message: "invalid Worker configuration"}
			} else {
				_, err = s.Targets.Update(ctx, id, sd.TargetUpdate{AccountRef: account, ExpectedConfigRevision: revision, ExpectedManagementRevision: managementRevision, Name: r.FormValue("name"), WorkerName: r.FormValue("worker_name"), Environment: r.FormValue("environment"), Origin: r.FormValue("public_origin")})
			}
		case "delete":
			if r.FormValue("confirm_delete") != "yes" {
				http.Error(w, "Explicit deletion confirmation required", 428)
				return
			}
			var account primitive.ObjectID
			account, err = primitive.ObjectIDFromHex(r.FormValue("account_ref"))
			if err != nil {
				err = &sd.Error{Code: sd.CodeInvalidRequest, Message: "invalid account reference"}
			}
			if err == nil {
				err = s.Targets.Delete(ctx, id, account)
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
		r.URL.RawQuery = "account=" + accountRef
		message := "操作未完成，错误码：" + sd.CodeOf(err) + "。请刷新后检查配置；若有未决任务，请先处理任务。"
		if i18n.LangFromRequest(r) == "en" {
			message = "The operation could not complete (" + sd.CodeOf(err) + "). Refresh and check the configuration or any pending jobs."
		}
		w.WriteHeader(sd.StatusForError(err))
		h.renderDeliveryPage(w, r, message)
		return
	}
	auditDetail(r, "operation", kind+"."+action)
	location := "/cm/static-delivery?saved=1"
	if accountRef != "" {
		location += "&account=" + accountRef
	}
	http.Redirect(w, r, location, http.StatusSeeOther)
}
