package handlers

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/jonradoff/lightcms/v7/internal/i18n"
)

// HandleLangSwitch persists the admin UI language (?lang=zh|en) and
// redirects back to the referring page (fallback /cm). GET only, so no
// CSRF token is required.
func (h *Handler) HandleLangSwitch(w http.ResponseWriter, r *http.Request) {
	lang := r.URL.Query().Get("lang")
	if lang != i18n.LangZh && lang != i18n.LangEn {
		lang = i18n.LangFromRequest(r)
	}
	i18n.SetLang(w, lang)

	back := "/cm"
	if ref := r.Referer(); ref != "" {
		switch {
		case strings.HasPrefix(ref, "/") && !strings.HasPrefix(ref, "//"):
			// Same-origin path reference.
			back = ref
		default:
			// Absolute Referer: only follow it when it points back
			// at this host (avoids open redirects).
			if u, err := url.Parse(ref); err == nil && u.Host != "" && u.Host == r.Host {
				if uri := u.RequestURI(); uri != "" {
					back = uri
				}
			}
		}
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}
