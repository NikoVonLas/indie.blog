package blog

import (
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/text/language"
)

const languageCookie = "blog_language"

func localizedResponse(path string) bool {
	return !strings.HasPrefix(path, "/static/") && !strings.HasPrefix(path, "/media/") && !strings.HasPrefix(path, "/post-style/") && !strings.HasPrefix(path, "/embeds/") && path != "/sitemap.xml" && path != "/robots.txt"
}

func languageBase(tag string) string {
	base, _, _ := strings.Cut(strings.ToLower(tag), "-")
	return base
}

func interfaceLanguage(tag string) string {
	base := languageBase(tag)
	if base == "ru" || base == "en" {
		return base
	}
	return ""
}

func (a *Server) languageChoice(r *http.Request) string {
	if c, err := r.Cookie(languageCookie); err == nil && (c.Value == "ru" || c.Value == "en") {
		return c.Value
	}
	return "auto"
}

func (a *Server) preferredLanguage(r *http.Request) string {
	if selected := a.languageChoice(r); selected != "auto" {
		return selected
	}
	// ParseAcceptLanguage sorts by quality and ignores languages with q=0.
	if tags, _, err := language.ParseAcceptLanguage(r.Header.Get("Accept-Language")); err == nil {
		for _, tag := range tags {
			if supported := interfaceLanguage(tag.String()); supported != "" {
				return supported
			}
		}
	}
	return a.Config.Language
}

func (a *Server) uiLang(r *http.Request) string {
	if lang := interfaceLanguage(a.preferredLanguage(r)); lang != "" {
		return lang
	}
	return "ru"
}

func (a *Server) lang(r *http.Request) string {
	if lang := r.URL.Query().Get("lang"); langPattern.MatchString(lang) {
		return lang
	}
	return a.preferredLanguage(r)
}

func (a *Server) translate(lang, text string) string {
	if interfaceLanguage(lang) == "en" {
		if translated, ok := a.english[text]; ok {
			return translated
		}
	}
	return text
}

func (a *Server) text(r *http.Request, text string) string {
	return a.translate(a.uiLang(r), text)
}

func languageName(lang string) string {
	switch lang {
	case "ru":
		return "Русский"
	case "en":
		return "English"
	}
	return lang
}

func (a *Server) chooseLanguage(w http.ResponseWriter, r *http.Request) {
	selected := r.PostFormValue("language")
	if selected != "auto" && selected != "ru" && selected != "en" {
		http.Error(w, a.text(r, "Выберите поддерживаемый язык."), http.StatusBadRequest)
		return
	}
	if selected == "auto" {
		a.cookie(w, languageCookie, "", -1)
	} else {
		a.cookie(w, languageCookie, selected, 365*24*60*60)
	}
	next, err := url.Parse(r.PostFormValue("next"))
	if err != nil || next.IsAbs() || next.Host != "" || !strings.HasPrefix(next.Path, "/") || strings.HasPrefix(next.Path, "//") || strings.ContainsAny(next.Path, "\\\r\n") || strings.ContainsAny(next.String(), "\r\n") || len(next.String()) > 4096 {
		next = &url.URL{Path: "/"}
	}
	query := next.Query()
	query.Del("lang") // Re-negotiate the article translation after a global switch.
	query.Del("page")
	query.Del("wm_page")
	// Rebuild the destination with the configured origin. Form data controls
	// only the path/query/fragment, never the redirect's scheme or authority.
	origin, _ := url.Parse(a.Config.BaseURL)
	destination := &url.URL{Scheme: origin.Scheme, Host: origin.Host, Path: next.Path, RawQuery: query.Encode(), Fragment: next.Fragment}
	http.Redirect(w, r, destination.String(), http.StatusSeeOther)
}

// Explicit content permalinks keep their language. Unpinned article URLs choose
// a published translation, falling back to the default or another available one.
func (a *Server) requestedPost(r *http.Request) (Post, error) {
	if lang := r.URL.Query().Get("lang"); langPattern.MatchString(lang) {
		return a.Store.Post(r.Context(), r.PathValue("slug"), lang)
	}
	admin := currentUser(r) != nil && currentUser(r).Admin
	baseLang, _, _ := strings.Cut(a.lang(r), "-")
	return scanPost(a.Store.DB.QueryRow(r.Context(), `SELECT `+postCols+` FROM posts
 WHERE slug=$1 AND NOT deleted AND ((visible AND published_at<=now()) OR ($4 AND lang=$2))
 ORDER BY CASE WHEN lang=$2 THEN 0 WHEN lang=$5 OR lang LIKE $6 THEN 1 WHEN lang=$3 THEN 2 ELSE 3 END,lang LIMIT 1`, r.PathValue("slug"), a.lang(r), a.Config.Language, admin, baseLang, baseLang+"-%"))
}
