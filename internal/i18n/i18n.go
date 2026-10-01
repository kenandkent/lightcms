package i18n

import (
	"net/http"
	"strings"
	"time"
)

// Supported admin UI languages.
const (
	LangZh = "zh"
	LangEn = "en"
)

// CookieName carries the admin UI language preference.
const CookieName = "lc_lang"

// langCookieMaxAge is one year in seconds (persistent cookie, Path=/).
const langCookieMaxAge = 365 * 24 * 60 * 60

// normalize returns lang when it is a supported language, else "".
func normalize(lang string) string {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case LangZh:
		return LangZh
	case LangEn:
		return LangEn
	default:
		return ""
	}
}

// Dict returns the translation dictionary for lang.
// Unknown languages fall back to the Chinese (default) dictionary.
// The returned map must be treated as read-only.
func Dict(lang string) map[string]string {
	if normalize(lang) == LangEn {
		return enDict
	}
	return zhDict
}

// LangFromRequest resolves the request language with the following
// precedence: ?lang=zh|en > cookie lc_lang > Accept-Language (zh?zh:en) >
// default zh.
func LangFromRequest(r *http.Request) string {
	if r != nil {
		if q := normalize(r.URL.Query().Get("lang")); q != "" {
			return q
		}
		if c, err := r.Cookie(CookieName); err == nil {
			if v := normalize(c.Value); v != "" {
				return v
			}
		}
		if al := r.Header.Get("Accept-Language"); al != "" {
			if strings.Contains(strings.ToLower(al), "zh") {
				return LangZh
			}
			return LangEn
		}
	}
	return LangZh
}

// SetLang persists the language preference in a long-lived cookie (Path=/).
// Unsupported values fall back to the default language.
func SetLang(w http.ResponseWriter, lang string) {
	if normalize(lang) == "" {
		lang = LangZh
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    lang,
		Path:     "/",
		MaxAge:   langCookieMaxAge,
		Expires:  time.Now().Add(langCookieMaxAge * time.Second),
		SameSite: http.SameSiteLaxMode,
	})
}

// T returns the translation for key in lang, or fallback when the key is
// missing. Fallback is the Chinese default text, so rendering is always
// correct even for keys that have no translation yet.
func T(key, fallback, lang string) string {
	if v, ok := Dict(lang)[key]; ok && v != "" {
		return v
	}
	return fallback
}
