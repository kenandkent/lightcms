package handlers

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/metavert-io/lightcms/v7/internal/i18n"
	"github.com/metavert-io/lightcms/v7/internal/models"
	"github.com/metavert-io/lightcms/v7/internal/product/artifacts"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

type artifactMaintenance interface {
	PreviewCleanup(context.Context, string) (artifacts.CleanupPlan, error)
	ConfirmCleanup(context.Context, string, string, bool) error
	ResumeCleanup(context.Context, string, string, bool) error
}

type artifactPinning interface {
	PinObject(context.Context, string, bool) error
}

// Optional to keep existing maintenance integrations and test doubles valid.
type artifactBrowser interface {
	ListObjects(context.Context, artifacts.ListQuery) (artifacts.ObjectPage, error)
	GetObject(context.Context, string) (artifacts.ObjectDetail, error)
}

func validArtifactHash(hash string) bool {
	if len(hash) != 64 {
		return false
	}
	_, err := hex.DecodeString(hash)
	return err == nil && hash == strings.ToLower(hash)
}

func artifactPageURL(r *http.Request, page int) string {
	query := url.Values{}
	for _, key := range []string{"q", "state", "pin", "lang"} {
		if value := r.URL.Query().Get(key); value != "" {
			query.Set(key, value)
		}
	}
	if page > 1 {
		query.Set("page", strconv.Itoa(page))
	}
	if len(query) == 0 {
		return "/cm/artifacts"
	}
	return "/cm/artifacts?" + query.Encode()
}

func (h *Handler) AdminArtifactsPage(w http.ResponseWriter, r *http.Request) {
	if !h.deliveryAdminAllowed(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	labels := artifactManagementText(i18n.LangFromRequest(r))
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, labels["method_error"], http.StatusMethodNotAllowed)
		return
	}
	if h.staticDelivery == nil || h.staticDelivery.Artifacts == nil {
		http.Error(w, labels["unavailable"], http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	browser, canBrowse := h.staticDelivery.Artifacts.(artifactBrowser)
	if r.Method == http.MethodGet && r.URL.Query().Get("format") == "json" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		fail := func(status int, message string) {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
		}
		if !canBrowse {
			fail(http.StatusServiceUnavailable, labels["list_unavailable"])
			return
		}
		hash := r.URL.Query().Get("hash")
		if !validArtifactHash(hash) {
			fail(http.StatusBadRequest, labels["invalid_hash"])
			return
		}
		detail, err := browser.GetObject(ctx, hash)
		if errors.Is(err, mongo.ErrNoDocuments) {
			fail(http.StatusNotFound, labels["not_found"])
			return
		}
		if err != nil {
			fail(http.StatusServiceUnavailable, labels["detail_error"])
			return
		}
		_ = json.NewEncoder(w).Encode(detail)
		return
	}
	page := 1
	if value := r.URL.Query().Get("page"); value != "" {
		var err error
		page, err = strconv.Atoi(value)
		if err != nil || page < 1 || page > 1000 {
			http.Error(w, labels["filter_error"], http.StatusBadRequest)
			return
		}
	}
	data := map[string]any{"CanBrowse": canBrowse, "Search": r.URL.Query().Get("q"), "State": r.URL.Query().Get("state"), "Pin": r.URL.Query().Get("pin"), "PageNumber": page, "ListURL": artifactPageURL(r, page)}
	pinning, canPin := h.staticDelivery.Artifacts.(artifactPinning)
	data["CanPin"] = canPin
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := r.ParseForm(); err != nil {
			http.Error(w, labels["form_error"], http.StatusBadRequest)
			return
		}
		user, _ := h.auth.GetCurrentUser(r)
		owner := user.ID
		var err error
		action := r.FormValue("action")
		if (action == "unpin" || action == "confirm" || action == "resume") && r.FormValue("confirmed") != "yes" {
			http.Error(w, labels["confirmation_required"], http.StatusBadRequest)
			return
		}
		if (action == "pin" || action == "unpin") && !validArtifactHash(r.FormValue("hash")) {
			http.Error(w, labels["invalid_hash"], http.StatusBadRequest)
			return
		}
		if (action == "confirm" || action == "resume") && !validArtifactHash(r.FormValue("digest")) {
			http.Error(w, labels["invalid_plan"], http.StatusBadRequest)
			return
		}
		switch action {
		case "pin", "unpin":
			if !canPin {
				http.Error(w, labels["pin_unavailable"], http.StatusServiceUnavailable)
				return
			}
			err = pinning.PinObject(ctx, r.FormValue("hash"), action == "pin")
			if err == nil {
				data["Notice"] = labels["pin_saved"]
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
				data["Notice"] = labels["cleanup_done"]
			}
		case "resume":
			err = h.staticDelivery.Artifacts.ResumeCleanup(ctx, owner, r.FormValue("digest"), r.FormValue("confirmed") == "yes")
			if err == nil {
				data["Notice"] = labels["resume_done"]
			}
		default:
			http.Error(w, labels["unknown_action"], http.StatusBadRequest)
			return
		}
		if err != nil {
			data["Error"] = labels["action_error"]
			data["Digest"] = r.FormValue("digest")
			data["ResumeOpen"] = action == "confirm" || action == "resume"
		} else if action := r.FormValue("action"); action != "preview" && h.auditService != nil {
			uid, _ := primitive.ObjectIDFromHex(user.ID)
			h.auditService.LogAsync(models.AuditLog{UserID: uid, UserEmail: user.Email, Action: "artifact." + action, Resource: "artifact", ResourceID: r.FormValue("hash"), Details: map[string]any{"cleanup_plan": r.FormValue("digest")}, IPAddress: r.RemoteAddr})
		}
	}
	if canBrowse {
		listing, err := browser.ListObjects(ctx, artifacts.ListQuery{Search: r.URL.Query().Get("q"), State: r.URL.Query().Get("state"), Pin: r.URL.Query().Get("pin"), Page: page, Limit: 25})
		if err != nil {
			data["ListError"] = labels["list_error"]
		} else {
			data["Listing"] = listing
			if page > 1 {
				data["PreviousURL"] = artifactPageURL(r, page-1)
			}
			if listing.HasNext {
				data["NextURL"] = artifactPageURL(r, page+1)
			}
		}
	}
	h.renderAdmin(w, r, "artifacts", data)
}
