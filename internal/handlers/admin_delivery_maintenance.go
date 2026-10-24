package handlers

import "net/http"

func (h *Handler) AdminDeliveryMaintenance(w http.ResponseWriter, r *http.Request) {
	if !h.deliveryAdminAllowed(w, r) {
		return
	}
	if h.staticDelivery == nil || h.staticDelivery.Commands == nil || h.staticDelivery.Targets == nil {
		http.Error(w, "静态发布暂不可用", 503)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	targets, err := h.staticDelivery.Targets.List(r.Context())
	if err != nil {
		http.Error(w, "无法读取发布目标", 503)
		return
	}
	h.renderAdmin(w, r, "delivery_maintenance", map[string]any{"MaintenanceTargets": targets})
}
