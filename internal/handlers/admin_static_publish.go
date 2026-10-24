package handlers

import (
	"github.com/gorilla/mux"
	"github.com/metavert-io/lightcms/v7/internal/auth"
	"github.com/metavert-io/lightcms/v7/internal/i18n"
	"github.com/metavert-io/lightcms/v7/internal/models"
	deliveryapi "github.com/metavert-io/lightcms/v7/internal/product/staticdelivery/httpapi"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"net/http"
)

func (h *Handler) RegisterStaticDeliveryContentRoutes(r *mux.Router) {
	var api deliveryapi.Handlers
	if h.staticDelivery != nil && h.staticDelivery.ContentAPI != nil {
		api = *h.staticDelivery.ContentAPI
	}
	api.ActorExtractor = func(r *http.Request) (*auth.SessionUser, bool) { return h.auth.GetCurrentUser(r) }
	deliveryapi.Register(r, &api)
}

// RegisterStaticDeliveryAdminAPI mounts the session-authenticated delivery API.
// Its shared handlers enforce per-operation permissions; the parent /cm
// router supplies the same CSRF middleware as the rest of the admin UI.
func (h *Handler) RegisterStaticDeliveryAdminAPI(r *mux.Router) {
	h.RegisterStaticDeliveryContentRoutes(r.PathPrefix("/static-delivery/api").Subrouter())
}
func (h *Handler) AdminStaticPublishPage(w http.ResponseWriter, r *http.Request) {
	if !h.adminRequirePerm(w, r, auth.PermContentView) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	data := map[string]any{}
	if h.staticDelivery == nil || h.staticDelivery.Commands == nil {
		data["ModuleError"] = staticPublishText(i18n.LangFromRequest(r))["moduleError"]
		h.renderAdmin(w, r, "static_delivery_publish", data)
		return
	}
	ids := r.URL.Query()["content_id"]
	seen := map[string]bool{}
	selected := []string{}
	rows := []models.Content{}
	if len(ids) > 100 {
		http.Error(w, "每次最多选择100篇文章", 400)
		return
	}
	for _, raw := range ids {
		id, e := primitive.ObjectIDFromHex(raw)
		if e != nil || seen[raw] {
			http.Error(w, "文章ID不正确或重复", 400)
			return
		}
		seen[raw] = true
		var c models.Content
		if e = h.db.Content().FindOne(r.Context(), bson.M{"_id": id}).Decode(&c); e != nil {
			http.Error(w, "文章不存在", 404)
			return
		}
		if !c.Published || c.Deleted || c.DeletedAt != nil || c.ForkID != nil {
			http.Error(w, "仅可选择未删除、非分支的已发布文章", 409)
			return
		}
		selected = append(selected, raw)
		rows = append(rows, c)
	}
	data["Selection"] = rows
	data["SelectedIDs"] = selected
	if u, ok := h.auth.GetCurrentUser(r); ok {
		data["CanPublish"] = auth.UserHasPermission(u, auth.PermContentPublish)
	}
	h.renderAdmin(w, r, "static_delivery_publish", data)
}
