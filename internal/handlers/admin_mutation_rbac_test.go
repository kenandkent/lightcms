package handlers

// Wave-1a scope completion (B2): every state-changing Admin UI handler must
// enforce its RBAC permission, mirroring the REST API matrix. Before this
// change ~22 mutating handlers (asset/folder/collection/redirect deletes,
// uploads, search-replace execute, …) accepted any authenticated session —
// including viewer.
//
// Matrix (mirrors api_settings/api_assets/api_search/api_apikeys):
//   content.delete      UndeleteContent, DeleteContactMessage
//   content.edit        ViewContactMessage (=marks read), MarkAllMessagesRead,
//                       BrokenLinkScan, FixBrokenLink
//   template.edit       ConfirmChangeTemplate (admin-only)
//   settings.edit       Create/Update/Delete Collection/Folder/Redirect,
//                       UpdateSiteConfiguration (admin-only)
//   asset.upload        UploadFile, AssetUpload
//   asset.delete        DeleteAsset
//   apikey.manage       DeleteAPIKey
//   search_replace      ReplaceExecute (admin-only)
//
// viewer holds only *.view (403 everywhere); contributor holds
// asset.upload + apikey.manage (passes the two upload/delete-key gates only);
// editor holds content.* + asset.* but not template.edit/settings.edit/
// search_replace; admin passes everything. Allowed roles run against ghost
// IDs / empty bodies so they fail downstream (404/400/redirect), never 403.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestAdminMutationRBACGates(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	ghost := primitive.NewObjectID().Hex()
	withID := map[string]string{"id": ghost}

	type call struct {
		name string
		fn   http.HandlerFunc
		vars map[string]string
		// get issues a GET instead of a POST (view-message renders + marks read).
		get bool
		// allowed lists roles expected to pass the RBAC gate (downstream
		// outcome is irrelevant, only that it is not 403).
		allowed []string
	}
	editor := []string{"editor", "admin"}
	all := []string{"contributor", "editor", "admin"}
	adminOnly := []string{"admin"}
	calls := []call{
		{"UndeleteContent", h.UndeleteContent, withID, false, editor},
		{"ConfirmChangeTemplate", h.ConfirmChangeTemplate, map[string]string{"id": ghost, "template_id": ghost}, false, adminOnly},
		{"CreateCollection", h.CreateCollection, nil, false, adminOnly},
		{"UpdateCollection", h.UpdateCollection, withID, false, adminOnly},
		{"DeleteCollection", h.DeleteCollection, withID, false, adminOnly},
		{"CreateFolder", h.CreateFolder, nil, false, adminOnly},
		{"UpdateFolder", h.UpdateFolder, withID, false, adminOnly},
		{"DeleteFolder", h.DeleteFolder, withID, false, adminOnly},
		{"CreateRedirect", h.CreateRedirect, nil, false, adminOnly},
		{"UpdateRedirect", h.UpdateRedirect, withID, false, adminOnly},
		{"DeleteRedirect", h.DeleteRedirect, withID, false, adminOnly},
		{"UpdateSiteConfiguration", h.UpdateSiteConfiguration, nil, false, adminOnly},
		{"UploadFile", h.UploadFile, nil, false, all},
		{"AssetUpload", h.AssetUpload, nil, false, all},
		{"DeleteAsset", h.DeleteAsset, withID, false, editor},
		{"DeleteAPIKey", h.DeleteAPIKey, withID, false, all},
		{"ViewContactMessage", h.ViewContactMessage, withID, true, editor},
		{"MarkAllMessagesRead", h.MarkAllMessagesRead, nil, false, editor},
		{"DeleteContactMessage", h.DeleteContactMessage, withID, false, editor},
		{"BrokenLinkScan", h.BrokenLinkScan, nil, true, editor},
		{"FixBrokenLink", h.FixBrokenLink, nil, false, editor},
		{"ReplaceExecute", h.ReplaceExecute, nil, false, adminOnly},
	}

	invoke := func(c call, role string) *httptest.ResponseRecorder {
		if c.get {
			req := rbacSessionReq(t, role, http.MethodGet, "/cm/x", nil, c.vars)
			rr := httptest.NewRecorder()
			c.fn(rr, req)
			return rr
		}
		return rbacPost(t, c.fn, url.Values{}, c.vars, role)
	}

	for _, c := range calls {
		if rr := invoke(c, "viewer"); rr.Code != http.StatusForbidden {
			t.Errorf("%s as viewer: got %d, want 403", c.name, rr.Code)
		}
		// contributor only holds asset.upload + apikey.manage among these.
		wantContributor := http.StatusForbidden
		for _, a := range c.allowed {
			if a == "contributor" {
				wantContributor = -1 // must NOT be 403
			}
		}
		if rr := invoke(c, "contributor"); (rr.Code == http.StatusForbidden) != (wantContributor == http.StatusForbidden) {
			t.Errorf("%s as contributor: got %d, want 403=%v", c.name, rr.Code, wantContributor == http.StatusForbidden)
		}
		for _, role := range c.allowed {
			if role == "contributor" {
				continue // covered above
			}
			if rr := invoke(c, role); rr.Code == http.StatusForbidden {
				t.Errorf("%s as %s: got 403, want allowed past RBAC (got %d: %s)",
					c.name, role, rr.Code, strings.TrimSpace(rr.Body.String()))
			}
		}
	}
}
