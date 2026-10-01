package i18n

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLangFromRequest_Precedence(t *testing.T) {
	// ?lang wins over everything.
	r := httptest.NewRequest("GET", "/cm?lang=en", nil)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: "zh"})
	r.Header.Set("Accept-Language", "zh-CN")
	if got := LangFromRequest(r); got != LangEn {
		t.Fatalf("query lang: got %q, want en", got)
	}

	// Cookie beats Accept-Language.
	r = httptest.NewRequest("GET", "/cm", nil)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: "en"})
	r.Header.Set("Accept-Language", "zh-CN")
	if got := LangFromRequest(r); got != LangEn {
		t.Fatalf("cookie lang: got %q, want en", got)
	}

	// Invalid ?lang falls through to cookie.
	r = httptest.NewRequest("GET", "/cm?lang=fr", nil)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: "en"})
	if got := LangFromRequest(r); got != LangEn {
		t.Fatalf("invalid query falls to cookie: got %q, want en", got)
	}

	// Accept-Language with zh -> zh.
	r = httptest.NewRequest("GET", "/cm", nil)
	r.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	if got := LangFromRequest(r); got != LangZh {
		t.Fatalf("accept zh: got %q, want zh", got)
	}

	// Accept-Language without zh -> en.
	r = httptest.NewRequest("GET", "/cm", nil)
	r.Header.Set("Accept-Language", "en-US,en;q=0.9")
	if got := LangFromRequest(r); got != LangEn {
		t.Fatalf("accept en: got %q, want en", got)
	}

	// No signals -> default zh.
	r = httptest.NewRequest("GET", "/cm", nil)
	if got := LangFromRequest(r); got != LangZh {
		t.Fatalf("default: got %q, want zh", got)
	}
}

func TestSetLang_PersistentCookie(t *testing.T) {
	w := httptest.NewRecorder()
	SetLang(w, "en")
	found := false
	for _, c := range w.Result().Cookies() {
		if c.Name == CookieName && c.Value == "en" && c.Path == "/" && c.MaxAge > 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("expected persistent lc_lang=en cookie with Path=/")
	}

	// Invalid input falls back to default.
	w = httptest.NewRecorder()
	SetLang(w, "fr")
	if got := w.Result().Cookies()[0].Value; got != LangZh {
		t.Fatalf("invalid lang cookie: got %q, want zh", got)
	}
}

func TestT_Fallback(t *testing.T) {
	if got := T("nav.dashboard", "仪表盘", "zh"); got != "仪表盘" {
		t.Fatalf("zh dict hit: got %q", got)
	}
	if got := T("nav.dashboard", "仪表盘", "en"); got != "Dashboard" {
		t.Fatalf("en dict hit: got %q", got)
	}
	// Unknown key -> Chinese fallback text, always renders correctly.
	if got := T("sibling.future.key", "未来功能", "en"); got != "未来功能" {
		t.Fatalf("fallback: got %q", got)
	}
	if got := T("sibling.future.key", "未来功能", "fr"); got != "未来功能" {
		t.Fatalf("fallback unknown lang: got %q", got)
	}
}

func TestDicts_Parity(t *testing.T) {
	for k := range zhDict {
		if _, ok := enDict[k]; !ok {
			t.Errorf("en dict missing key %q", k)
		}
	}
	for k := range enDict {
		if _, ok := zhDict[k]; !ok {
			t.Errorf("zh dict missing key %q", k)
		}
	}
}
