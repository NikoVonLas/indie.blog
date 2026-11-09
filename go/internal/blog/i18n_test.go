package blog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func languageGet(b *browser, path, accept string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", b.app.Config.BaseURL+path, nil)
	r.Header.Set("Accept-Language", accept)
	for _, cookie := range b.cookies {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	b.app.ServeHTTP(w, r)
	for _, cookie := range w.Result().Cookies() {
		b.cookies[cookie.Name] = cookie
	}
	return w
}

func TestBrowserLanguagePreferenceAndCache(t *testing.T) {
	a := testApp(t)
	seedPost(t, a, "untranslated", true, false, time.Now().Add(-time.Hour))
	b := newBrowser(a)
	for _, tc := range []struct{ header, lang, menu string }{
		{"en-US,en;q=0.9,ru;q=0.5", "en", "📰 Articles"},
		{"ru-RU,en;q=0.9", "ru", "📰 Статьи"},
		{"fr-FR,ru;q=0.4,en;q=0.8", "en", "📰 Articles"},
		{"en;q=0,ru;q=0.8", "ru", "📰 Статьи"},
		{"de-DE", "ru", "📰 Статьи"},
		{"invalid!", "ru", "📰 Статьи"},
	} {
		w := languageGet(b, "/", tc.header)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `<html lang="`+tc.lang+`">`) || !strings.Contains(w.Body.String(), tc.menu) || !strings.Contains(w.Body.String(), "untranslated") {
			t.Fatalf("%s: %d %s", tc.header, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Header().Get("Vary"), "Accept-Language") {
			t.Fatal("missing language negotiation header")
		}
	}
	before := a.Store.DB.queries.Load()
	for _, header := range []string{"en-GB", "ru-RU", "en-US", "ru"} {
		if w := languageGet(b, "/", header); w.Header().Get("X-Blog-Cache") != "HIT" {
			t.Fatal("language variants did not retain independent warm caches", header)
		}
	}
	if a.Store.DB.queries.Load() != before {
		t.Fatal("warm language variants queried the database")
	}
	// Fonts, scripts and images have one representation shared by all locales.
	languageGet(b, "/static/site.css", "en")
	if w := languageGet(b, "/static/site.css", "ru"); w.Header().Get("X-Blog-Cache") != "HIT" || w.Header().Get("Content-Language") != "" {
		t.Fatal("language-neutral assets were duplicated or labeled with a visitor's language")
	}
	if w := b.request("POST", "/language/", url.Values{"language": {"en"}, "next": {"/notes/?lang=ru&page=2&q=test#comments"}}); w.Code != 303 || w.Header().Get("Location") != "/notes/?q=test#comments" {
		t.Fatal("language switch did not preserve the page/search", w.Code, w.Header())
	}
	cookie := b.cookies[languageCookie]
	if cookie == nil || cookie.Value != "en" || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.MaxAge <= 0 {
		t.Fatal("language choice not persisted safely", cookie)
	}
	if w := languageGet(b, "/", "ru-RU"); !strings.Contains(w.Body.String(), "📰 Articles") || !strings.Contains(w.Body.String(), `value="en" lang="en" selected`) {
		t.Fatal("manual selection did not override the browser")
	}
	if w := b.request("POST", "/language/", url.Values{"language": {"auto"}, "next": {"/"}}); w.Code != 303 || b.cookies[languageCookie] != nil {
		t.Fatal("automatic mode did not clear the preference")
	}
	if w := languageGet(b, "/", "ru-RU"); w.Header().Get("X-Blog-Cache") != "HIT" || !strings.Contains(w.Body.String(), "📰 Статьи") {
		t.Fatal("switching language flushed a cached variant or failed to resume detection")
	}
}

func TestLanguagePreferenceCSRFAndReturnURL(t *testing.T) {
	a := testApp(t)
	b := newBrowser(a)
	languageGet(b, "/", "en")
	if w := b.request("POST", "/language/", url.Values{"language": {"ru"}, "csrf": {"forged"}}); w.Code != 403 {
		t.Fatal("language mutation bypassed CSRF", w.Code)
	}
	if w := b.request("POST", "/language/", url.Values{"language": {"unsupported"}}); w.Code != 400 || b.cookies[languageCookie] != nil {
		t.Fatal("unsupported choice was persisted", w.Code)
	}
	for _, next := range []string{"https://example.org/", "//example.org/", "/%2fexample.org/", "/%5cexample.org/", "?next=https://example.org/"} {
		if w := b.request("POST", "/language/", url.Values{"language": {"ru"}, "next": {next}}); w.Code != 303 || w.Header().Get("Location") != "/" {
			t.Fatal("unsafe return URL", next, w.Header())
		}
	}
}

func TestTranslatedPostsAndFallback(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	ru := seedPost(t, a, "translated", true, false, time.Now().Add(-time.Hour))
	ru.Kind, ru.Title, ru.Body = "blog", "Русский заголовок", "Русский авторский текст"
	if err := a.Store.SavePost(ctx, &ru); err != nil {
		t.Fatal(err)
	}
	en := ru
	en.ID, en.Lang, en.Title, en.Body = "", "en", "Статьи", "English author text"
	if err := a.Store.SavePost(ctx, &en); err != nil {
		t.Fatal(err)
	}
	onlyRU := seedPost(t, a, "only-russian", true, false, time.Now().Add(-2*time.Hour))
	b := newBrowser(a)
	path := "/blog/translated/"
	w := languageGet(b, path, "en-US")
	if w.Code != 200 || !strings.Contains(w.Body.String(), en.Body) || strings.Contains(w.Body.String(), ru.Body) || !strings.Contains(w.Body.String(), `<html lang="en">`) || !strings.Contains(w.Body.String(), `<article class="h-entry" lang="en">`) {
		t.Fatal("browser did not select the existing translation", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `<title>Статьи ·`) || !strings.Contains(w.Body.String(), `hreflang="ru"`) || !strings.Contains(w.Body.String(), "Permalink") {
		t.Fatal("author text was translated or translation links/UI are missing")
	}
	if w = languageGet(b, ru.URL(), "en-US"); w.Code != 200 || !strings.Contains(w.Body.String(), ru.Body) || !strings.Contains(w.Body.String(), `<html lang="en">`) || w.Header().Get("Content-Language") != "en, ru" {
		t.Fatal("explicit permalink lost its language or changed UI preference", w.Code)
	}
	if w = languageGet(b, "/notes/only-russian/", "en"); w.Code != 200 || !strings.Contains(w.Body.String(), onlyRU.Body) || !strings.Contains(w.Body.String(), "There is no translation") || !strings.Contains(w.Body.String(), `<article class="h-entry" lang="ru">`) {
		t.Fatal("untranslated entry failed to fall back", w.Code, w.Body.String())
	}
	if w = languageGet(b, "/blog/translated/?lang=de", "en"); w.Code != 404 {
		t.Fatal("nonexistent explicit translation must stay 404", w.Code)
	}
	if w = languageGet(b, "/", "en"); strings.Count(w.Body.String(), `href="`+en.URL()+`"`) != 1 || strings.Contains(w.Body.String(), `href="`+ru.URL()+`"`) || !strings.Contains(w.Body.String(), onlyRU.Title) {
		t.Fatal("home duplicated the hero translation or hid an untranslated note", w.Body.String())
	}
	for _, feed := range []string{"/rss/", "/feed.json", "/llms.txt"} {
		w = languageGet(b, feed, "en")
		if w.Code != 200 || !strings.Contains(w.Body.String(), en.Title) || !strings.Contains(w.Body.String(), onlyRU.Title) || strings.Contains(w.Body.String(), ru.Title) {
			t.Fatal("feed did not select one preferred variant with fallback", feed, w.Code, w.Body.String())
		}
	}
	w = languageGet(b, "/blog/translated/index.md", "en")
	if w.Code != 200 || !strings.Contains(w.Body.String(), en.Body) || !strings.Contains(w.Body.String(), "Author:") || w.Header().Get("Content-Language") != "en" {
		t.Fatal("Markdown did not select the translation", w.Code)
	}
	// Publication changes invalidate both language variants and trigger fallback.
	en.Visible = false
	if err := a.Store.SavePost(ctx, &en); err != nil {
		t.Fatal(err)
	}
	if w = languageGet(b, path, "en"); w.Code != 200 || !strings.Contains(w.Body.String(), ru.Body) || strings.Contains(w.Body.String(), en.Body) {
		t.Fatal("draft translation leaked or cached publication remained", w.Code)
	}
	if w = languageGet(b, en.URL(), "en"); w.Code != 404 {
		t.Fatal("explicit draft became accessible", w.Code)
	}
	var feed struct {
		Items []struct {
			Language string `json:"language"`
		} `json:"items"`
	}
	w = languageGet(b, "/feed.json", "en")
	if err := json.Unmarshal(w.Body.Bytes(), &feed); err != nil || len(feed.Items) != 2 || feed.Items[0].Language != "ru" {
		t.Fatal("fallback feed has wrong languages or duplicates", err, w.Body.String())
	}
}

func TestRegionalAndProtectedLanguageFallback(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	ru := seedPost(t, a, "regional", true, false, time.Now().Add(-time.Hour))
	en := ru
	en.ID, en.Lang, en.Body = "", "en-GB", "regional English text"
	if err := a.Store.SavePost(ctx, &en); err != nil {
		t.Fatal(err)
	}
	b := newBrowser(a)
	if w := languageGet(b, "/notes/regional/", "en-US"); w.Code != 200 || !strings.Contains(w.Body.String(), en.Body) || strings.Contains(w.Body.String(), "There is no translation") {
		t.Fatal("regional English was ignored or incorrectly marked as unavailable", w.Code)
	}
	en.MembersOnly = true
	if err := a.Store.SavePost(ctx, &en); err != nil {
		t.Fatal(err)
	}
	if w := languageGet(b, "/notes/regional/", "en"); w.Code != 200 || strings.Contains(w.Body.String(), en.Body) || !strings.Contains(w.Body.String(), "available to blog members") {
		t.Fatal("protected translation leaked", w.Code, w.Body.String())
	}
	if w := languageGet(b, "/search/?q=regional", "en"); w.Code != 200 || strings.Contains(w.Body.String(), en.Body) || !strings.Contains(w.Body.String(), ru.Title) {
		t.Fatal("public search lost its accessible fallback", w.Code)
	}
	if w := languageGet(b, "/rss/", "en"); w.Code != 200 || strings.Contains(w.Body.String(), en.Body) || !strings.Contains(w.Body.String(), ru.Body) {
		t.Fatal("public feed exposed a protected translation or lost its fallback", w.Code)
	}
}
