// Package i18n provides minimal public-site internationalization.
//
// Contract (shared with UI-A admin shell workstream — keep API identical):
//
//	consts LangZh / LangEn
//	funcs Dict(lang) map[string]string
//	      LangFromRequest(r) (query > cookie > Accept-Language, default zh)
//	      SetLang(w, lang)
//	      T(key, fallback, lang)
//
// Template usage: {{i18n "site.nav_home" "首页" .Lang}} — the Chinese
// fallback always renders correctly even when a key is missing.
package i18n

import (
	"net/http"
	"strings"
)

// Supported language codes.
const (
	LangZh = "zh"
	LangEn = "en"
)

// CookieName is the shared language cookie set by /cm/lang and read by
// LangFromRequest. Both admin and public renders use the same cookie.
const CookieName = "lc_lang"

// QueryParam is the ?lang=zh|en query override.
const QueryParam = "lang"

var zhDict = map[string]string{
	"site.nav_home":       "首页",
	"site.nav_blog":       "博客",
	"site.lang_zh":        "中文",
	"site.lang_en":        "EN",
	"site.footer_powered": "由 LightCMS 驱动",
	"site.error_404_msg":  "页面未找到",
	"site.error_404_back": "返回首页",
}

var enDict = map[string]string{
	"site.nav_home":       "Home",
	"site.nav_blog":       "Blog",
	"site.lang_zh":        "中文",
	"site.lang_en":        "EN",
	"site.footer_powered": "Powered by LightCMS.",
	"site.error_404_msg":  "Page not found",
	"site.error_404_back": "Go Home",
}

// Dict returns the translation dictionary for lang. Unknown languages fall
// back to Chinese. The returned map is a copy; callers may not mutate the
// package tables through it.
func Dict(lang string) map[string]string {
	src := zhDict
	if normalize(lang) == LangEn {
		src = enDict
	}
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// normalize maps a raw language tag to a supported code ("zh" or "en"),
// defaulting to Chinese.
func normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return LangZh
	}
	// Strip region suffixes: zh-CN / zh_TW -> zh, en-US -> en.
	if i := strings.IndexAny(s, "-_"); i >= 0 {
		s = s[:i]
	}
	// Also handle bare prefixes that slipped through (e.g. "zhcn").
	switch {
	case strings.HasPrefix(s, "zh"):
		return LangZh
	case strings.HasPrefix(s, "en"):
		return LangEn
	default:
		return LangZh
	}
}

// LangFromRequest resolves the request language with precedence:
//  1. ?lang= query parameter (only zh/en accepted)
//  2. lc_lang cookie (only zh/en accepted)
//  3. Accept-Language header (first supported tag wins)
//  4. default zh
func LangFromRequest(r *http.Request) string {
	if r != nil {
		if q := r.URL.Query().Get(QueryParam); q != "" {
			switch normalize(q) {
			case LangZh:
				return LangZh
			case LangEn:
				// normalize maps every en* to en; the switch documents intent.
				return LangEn
			}
		}
		if c, err := r.Cookie(CookieName); err == nil && c != nil && c.Value != "" {
			switch normalize(c.Value) {
			case LangZh:
				return LangZh
			case LangEn:
				return LangEn
			}
		}
		if al := r.Header.Get("Accept-Language"); al != "" {
			for _, part := range strings.Split(al, ",") {
				part = strings.TrimSpace(part)
				if i := strings.Index(part, ";"); i >= 0 {
					part = part[:i]
				}
				if part == "" || part == "*" {
					continue
				}
				switch normalize(part) {
				case LangEn:
					return LangEn
				case LangZh:
					return LangZh
				}
			}
		}
	}
	return LangZh
}

// SetLang persists the language choice in the shared lc_lang cookie
// (1-year expiry, path-scoped to the whole site so admin and public agree).
func SetLang(w http.ResponseWriter, lang string) {
	if normalize(lang) == LangEn {
		lang = LangEn
	} else {
		lang = LangZh
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    lang,
		Path:     "/",
		MaxAge:   365 * 24 * 60 * 60,
		HttpOnly: false,
		SameSite: http.SameSiteLaxMode,
	})
}

// T translates key for lang, returning fallback when the key is missing.
// lang is normalized, so raw tags (zh-CN, en-US) work directly.
func T(key, fallback, lang string) string {
	if v, ok := Dict(normalize(lang))[key]; ok && v != "" {
		return v
	}
	return fallback
}
