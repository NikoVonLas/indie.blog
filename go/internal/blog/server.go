package blog

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

//go:embed templates/*.html static/* locales/*.json
var assets embed.FS

type Config struct{ BaseURL, Title, Author, Language, Listen, SMTPAddr, SMTPUser, SMTPPassword, MailFrom, OwnerEmail, TelegramChannel, TelegramBotToken string }
type Server struct {
	cache       pageCache
	viewsMu     sync.Mutex
	flushMu     sync.Mutex
	views       map[string]int64
	Store       *Store
	Config      Config
	md          *Markdown
	templates   map[string]*template.Template
	english     map[string]string
	secret      string
	mux         *http.ServeMux
	visitorHTTP *http.Client
	posseHTTP   *http.Client
	passkeys    *webauthn.WebAuthn
}
type identityKey struct{}
type commentView struct {
	Comment
	HTML template.HTML
}
type page struct {
	POSSECanSend                                               bool
	Syndications                                               []Syndication
	POSSETarget                                                SyndicationTarget
	POSSEPreview, POSSERevision, POSSEReason                   string
	PasskeyKeys                                                []adminKey
	FirstPasskey                                               bool
	Status                                                     int
	Title, Mode, Message, CSRF, Lang, Query, Block, ReplyTo    string
	ContentLang, PreferredLang, LanguageChoice, LanguageReturn string
	ContentFallback                                            bool
	ClientMessages                                             template.JS
	LoginNext, LoginRedirect                                   string
	Site                                                       Config
	User                                                       *User
	Post                                                       Post
	Posts                                                      []Post
	Comments                                                   []commentView
	HTML                                                       template.HTML
	StructuredData                                             template.JS
	Kinds                                                      []string
	Page, Next, Previous                                       int
	Translations                                               []Post
	Home                                                       bool
	Hero                                                       *Post
	About                                                      AboutCard
	AboutHTML                                                  template.HTML
	AboutRevision                                              string
	Mentions                                                   []Webmention
	AcceptsMentions                                            bool
	IndieRequest                                               IndieRequest
	IndieTokens                                                []IndieToken
}

func NewServer(ctx context.Context, s *Store, c Config) (*Server, error) {
	u, e := url.Parse(c.BaseURL)
	if e != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return nil, errors.New("BASE_URL must be an absolute origin without a path")
	}
	c.BaseURL = strings.TrimSuffix(c.BaseURL, "/")
	if c.Language == "" {
		c.Language = "ru"
	}
	if !langPattern.MatchString(c.Language) {
		return nil, errors.New("invalid language")
	}
	if c.Title == "" {
		c.Title = "Garbage M1ndstr3am"
	}
	if c.Author == "" {
		c.Author = "Автор"
	}
	_, e = s.DB.Exec(ctx, `INSERT INTO settings(key,value) VALUES('csrf_secret',$1) ON CONFLICT DO NOTHING`, randomToken())
	if e != nil {
		return nil, e
	}
	app := &Server{Store: s, Config: c, md: NewMarkdown(), mux: http.NewServeMux(), visitorHTTP: publicPreviewClient()}
	if e = s.DB.QueryRow(ctx, `SELECT value FROM settings WHERE key='csrf_secret'`).Scan(&app.secret); e != nil {
		return nil, e
	}
	translations, e := assets.ReadFile("locales/en.json")
	if e != nil {
		return nil, e
	}
	if e = json.Unmarshal(translations, &app.english); e != nil {
		return nil, e
	}
	app.templates = make(map[string]*template.Template)
	for _, lang := range []string{"ru", "en"} {
		funcs := template.FuncMap{"date": func(t time.Time) string { return t.Format("02.01.2006") }, "iso": func(t time.Time) string { return t.UTC().Format(time.RFC3339) }, "inputDate": func(t time.Time) string { return t.UTC().Format("2006-01-02T15:04") }, "label": func(kind string) string { return app.translate(lang, kindLabel(kind)) }, "t": func(text string) string { return app.translate(lang, text) }, "languageName": languageName}
		app.templates[lang], e = template.New("site").Funcs(funcs).ParseFS(assets, "templates/*.html")
		if e != nil {
			return nil, e
		}
	}
	if e = app.initPasskeys(); e != nil {
		return nil, e
	}
	if e = app.initPOSSE(); e != nil {
		return nil, e
	}
	s.DB.startChanges(ctx)
	app.routes()
	return app, nil
}
func kindLabel(s string) string {
	m := map[string]string{"blog": "Статьи", "notes": "Заметки", "world": "Путешествия", "gallery": "Фотографии", "inside": "Личное", "365": "Дневник", "reply": "Ответы", "bookmark": "Закладки", "repost": "Репосты"}
	if v, ok := m[s]; ok {
		return v
	}
	return s
}
func (a *Server) routes() {
	m := a.mux
	handle := func(pattern string, h http.HandlerFunc) {
		if strings.HasSuffix(pattern, "/") {
			pattern += "{$}"
		}
		m.HandleFunc(pattern, h)
	}
	static, _ := fs.Sub(assets, "static")
	handle("POST /language/", a.chooseLanguage)
	// Exact asset routes cannot collide with legacy /{kind}/{slug}/index.html.
	fileServer := http.StripPrefix("/static/", http.FileServerFS(static))
	_ = fs.WalkDir(static, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			m.Handle("GET /static/"+path, fileServer)
		}
		return nil
	})

	a.indieRoutes(handle)
	a.visitorRoutes(handle)
	a.passkeyRoutes(handle)
	handle("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if e := a.Store.DB.Ping(r.Context()); e != nil {
			http.Error(w, "unavailable", 503)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	handle("GET /{$}", a.list)
	handle("GET /all/", a.list)
	handle("GET /search/", a.list)
	handle("GET /login/", a.login)

	handle("POST /logout/", a.logout)
	handle("GET /profile/", a.profile)
	handle("POST /profile/", a.profile)
	handle("POST /uploads/", a.upload)
	handle("GET /media/{id}", a.media)
	handle("POST /godmode/link-preview/", a.linkPreview)
	handle("GET /embeds/{id}/{digest}/document", a.embed)
	handle("POST /webmention", a.receiveMention)
	handle("GET /godmode/webmentions/", a.mentionsAdmin)
	handle("POST /godmode/webmentions/{id}/", a.moderateMention)
	handle("POST /godmode/posts/{id}/webmentions/", a.sendMentions)
	handle("GET /godmode/posts/{id}/posse/", a.possePage)
	handle("POST /godmode/posts/{id}/posse/", a.possePage)
	handle("GET /godmode/", a.admin)
	handle("GET /godmode/about/", a.editAbout)
	handle("POST /godmode/about/", a.editAbout)
	handle("GET /godmode/posts/new/", a.edit)
	handle("POST /godmode/posts/new/", a.edit)
	handle("GET /godmode/posts/{id}/", a.edit)
	handle("POST /godmode/posts/{id}/", a.edit)
	handle("GET /godmode/posts/{id}/delete/", a.deletePost)
	handle("POST /godmode/posts/{id}/delete/", a.deletePost)
	handle("GET /godmode/posts/{id}/restore/", a.restorePost)
	handle("POST /godmode/posts/{id}/restore/", a.restorePost)

	handle("GET /godmode/export/", a.export)
	handle("POST /godmode/posts/{id}/announce/", a.announce)
	handle("POST /comments/create/", a.comment)
	handle("POST /comments/{id}/delete/", a.moderate)
	handle("POST /comments/{id}/pin/", a.moderate)
	handle("POST /clickers/comments/{id}/", a.react)
	handle("POST /clickers/blocks/{slug}/{block}/", a.react)
	handle("GET /rss/", a.feed)
	handle("GET /rss/{scope}/", a.feed)
	handle("GET /feed.json", a.jsonFeed)
	handle("GET /llms.txt", a.llms)
	handle("GET /{kind}/{slug}/index.md", a.markdownPost)
	handle("GET /sitemap.xml", a.sitemap)
	handle("GET /robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "User-agent: *\nDisallow: /godmode/\nDisallow: /admin\nDisallow: /profile/\nSitemap: %s/sitemap.xml\n", a.Config.BaseURL)
	})
	handle("GET /subscribe/", a.subscribe)
	handle("POST /subscribe/", a.subscribe)
	handle("GET /subscribe/confirm/{token}/", a.confirm)
	handle("GET /unsubscribe/{token}/", a.unsubscribe)
	handle("POST /unsubscribe/{token}/", a.unsubscribe)
	handle("GET /donate/", func(w http.ResponseWriter, r *http.Request) {
		a.render(w, r, page{Mode: "message", Title: a.text(r, "Поддержать блог"), Message: "Контакты автора — на главной странице."})
	})
	handle("GET /post-style/{id}", a.postStyle)
	handle("GET /{kind}/", a.list)
	handle("GET /{kind}/{slug}/", a.show)
	handle("GET /{kind}/{slug}/index.html", a.show)
	handle("GET /{kind}/{slug}/{block}/", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("block") == "edit" {
			a.legacyEdit(w, r)
			return
		}
		p, e := a.Store.Post(r.Context(), r.PathValue("slug"), a.lang(r))
		if e != nil || !p.Readable(currentUser(r)) {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, p.URL()+"#"+url.PathEscape(r.PathValue("block")), http.StatusMovedPermanently)
	})
}
func (a *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' https: http:; media-src 'self' https:; style-src 'self' 'unsafe-inline'; script-src 'self'; frame-src 'self' https: http:; base-uri 'self'; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("Cache-Control", "private, no-store")
	if localizedResponse(r.URL.Path) {
		w.Header().Set("Content-Language", a.lang(r))
		w.Header().Add("Vary", "Accept-Language, Cookie")
	}
	limit := int64(2 << 20)
	if r.URL.Path == "/webmention" {
		limit = 64 << 10
	}
	if r.URL.Path == "/uploads/" || r.URL.Path == "/micropub/media" || r.URL.Path == "/micropub" {
		limit = 11 << 20
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if protocolPath(r.URL.Path) || r.URL.Path == "/indieauth/auth" && (r.Method == "POST" || r.Method == "OPTIONS") || r.URL.Path == "/.well-known/oauth-authorization-server" {
		protocolCORS(w)
		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
	}
	if c, e := r.Cookie("session"); e == nil && r.URL.Path != "/webmention" && !protocolRequest(r) {
		var u *User
		var e error
		if a.cacheable(r) {
			u, e = a.cachedSession(r.Context(), c.Value)
		} else {
			u, e = a.Store.Session(r.Context(), c.Value)
		}
		if e == nil {
			r = r.WithContext(context.WithValue(r.Context(), identityKey{}, u))
		} else if !errors.Is(e, ErrNoRows) {
			a.fail(w, e)
			return
		}
	}
	if r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS" {
		// Protocol endpoints authenticate with codes/tokens, never session cookies.
		// Browser consent and all other browser mutations still require CSRF.
		if r.URL.Path != "/webmention" && !protocolRequest(r) && !a.validCSRF(r) {
			http.Error(w, a.text(r, "Обновите страницу и повторите действие"), http.StatusForbidden)
			return
		}
	}
	a.serveCached(w, r)
}
func currentUser(r *http.Request) *User { u, _ := r.Context().Value(identityKey{}).(*User); return u }
func (a *Server) sign(s string) string {
	h := hmac.New(sha256.New, []byte(a.secret))
	h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}
func (a *Server) csrf(w http.ResponseWriter, r *http.Request) string {
	if info, ok := r.Context().Value(renderCacheKey{}).(*renderCacheInfo); ok {
		return info.csrf
	}
	if c, e := r.Cookie("csrf"); e == nil && a.validToken(c.Value) {
		return c.Value
	}
	nonce := randomToken()
	token := nonce + "." + a.sign(nonce)
	a.cookie(w, "csrf", token, 86400)
	return token
}
func (a *Server) validToken(t string) bool {
	p := strings.Split(t, ".")
	return len(p) == 2 && len(p[0]) == 64 && hmac.Equal([]byte(p[1]), []byte(a.sign(p[0])))
}
func (a *Server) validCSRF(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		if u, e := url.Parse(r.Referer()); e == nil && u.Host != "" {
			origin = u.Scheme + "://" + u.Host
		}
	}
	if origin != a.Config.BaseURL {
		return false
	}
	token := r.Header.Get("X-CSRF-Token")
	if token == "" {
		if e := r.ParseForm(); e != nil {
			return false
		}
		token = r.PostForm.Get("csrf")
	}
	c, e := r.Cookie("csrf")
	if e != nil || !a.validToken(c.Value) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(token)) == 1
}
func (a *Server) cookie(w http.ResponseWriter, name, value string, age int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: age, HttpOnly: true, Secure: strings.HasPrefix(a.Config.BaseURL, "https://"), SameSite: http.SameSiteLaxMode})
}
func (a *Server) render(w http.ResponseWriter, r *http.Request, p page) {
	if p.LoginNext == "" {
		p.LoginNext = loginNext(r.URL.Query().Get("next"))
	}
	if r.URL.Path == "/" {
		w.Header().Add("Link", a.indieDiscovery())
	}
	p.Site = a.Config
	p.User = currentUser(r)
	p.CSRF = a.csrf(w, r)
	p.Lang = a.uiLang(r)
	p.ContentLang = a.lang(r)
	p.PreferredLang = a.preferredLanguage(r)
	p.LanguageChoice = a.languageChoice(r)
	p.LanguageReturn = r.URL.RequestURI()
	p.Message = a.translate(p.Lang, p.Message)
	if p.Mode == "post-delete" || p.Mode == "post-restore" {
		p.Title = a.translate(p.Lang, p.Title)
	}
	if p.Lang == "en" && p.Mode != "list" && p.Mode != "post" {
		messages, _ := json.Marshal(a.english)
		p.ClientMessages = template.JS(messages)
	}
	p.Kinds = Kinds
	if p.Home && p.Page == 1 {
		card, _, err := a.Store.About(r.Context())
		if err != nil {
			a.fail(w, err)
			return
		}
		p.About = card
		p.AboutHTML = a.md.Comment(card.Description)
	}
	if p.Mode == "post" {
		p.ContentFallback = languageBase(p.Post.Lang) != languageBase(p.ContentLang)
		p.StructuredData = a.structuredPost(p.Post)
		p.AcceptsMentions = acceptsMentions(p.Post)
		discovery := fmt.Sprintf(`<%s%s>; rel="alternate"; type="text/markdown", <%s/llms.txt?lang=%s>; rel="describedby"`, a.Config.BaseURL, p.Post.MarkdownURL(), a.Config.BaseURL, p.Post.Lang)
		if p.AcceptsMentions {
			n, _ := strconv.Atoi(r.URL.Query().Get("wm_page"))
			if n < 1 || n > 100000 {
				n = 1
			}
			mentions, err := a.Store.Mentions(r.Context(), p.Post.ID, true, (n-1)*50)
			if err != nil {
				a.fail(w, err)
				return
			}
			p.Mentions = mentions
			if len(mentions) > 50 {
				p.Mentions = mentions[:50]
				p.Next = n + 1
			}
			if n > 1 {
				p.Previous = n - 1
			}
			discovery += fmt.Sprintf(`, <%s/webmention>; rel="webmention"`, a.Config.BaseURL)
		}
		w.Header().Set("Link", discovery)
	}
	var b bytes.Buffer
	if e := a.templates[p.Lang].ExecuteTemplate(&b, "layout", p); e != nil {
		a.fail(w, e)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Language", p.Lang)
	if p.Mode == "post" && p.Post.Lang != p.Lang {
		w.Header().Set("Content-Language", p.Lang+", "+p.Post.Lang)
	}
	if p.Status != 0 {
		w.WriteHeader(p.Status)
	}
	_, _ = w.Write(b.Bytes())
}
func (a *Server) fail(w http.ResponseWriter, e error) {
	slog.Error("request failed", "type", fmt.Sprintf("%T", e))
	http.Error(w, "Не удалось выполнить запрос", 500)
}
func (a *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	u := currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/admin?next="+url.QueryEscape(r.URL.RequestURI()), 303)
		return false
	}
	if !u.Admin {
		http.Error(w, a.text(r, "Недостаточно прав"), 403)
		return false
	}
	return true
}
func (a *Server) limited(w http.ResponseWriter, r *http.Request, key string, n int, d time.Duration) bool {
	ip, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		ip = r.RemoteAddr
	}
	ok, e := a.Store.Allow(r.Context(), key+":"+a.sign(ip), n, d)
	if e != nil {
		a.fail(w, e)
		return true
	}
	if !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())))
		http.Error(w, a.text(r, "Слишком много запросов. Попробуйте позже."), 429)
		return true
	}
	return false
}
func (a *Server) list(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	if kind != "" {
		found := false
		for _, k := range Kinds {
			if kind == k {
				found = true
			}
		}
		if !found {
			http.NotFound(w, r)
			return
		}
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if n < 1 {
		n = 1
	}
	if n > 100000 {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query().Get("q")
	if len(q) > 300 {
		http.Error(w, a.text(r, "Слишком длинный запрос"), 400)
		return
	}
	// Search never examines protected text, even if only its title would be returned.
	home := r.URL.Path == "/" && q == ""
	var hero *Post
	exclude := ""
	if home {
		latest, err := a.Store.Posts(r.Context(), Filter{Lang: a.lang(r), FallbackLang: a.Config.Language, Kind: "blog", Limit: 1})
		if err != nil {
			a.fail(w, err)
			return
		}
		if len(latest) > 0 {
			hero = &latest[0]
			exclude = hero.ID
		}
	}
	posts, e := a.Store.Posts(r.Context(), Filter{Lang: a.lang(r), FallbackLang: a.Config.Language, Kind: kind, Search: q, PublicOnly: q != "", Home: home, HomeTimeline: home, ExcludeID: exclude, Limit: 31, Offset: (n - 1) * 30})
	if e != nil {
		a.fail(w, e)
		return
	}
	p := page{Mode: "list", Title: a.text(r, kindLabel(kind)), Posts: posts, Page: n, Query: q}
	if kind == "" {
		p.Title = a.text(r, "Архив")
	}
	if r.URL.Path == "/" {
		p.Title = a.Config.Title
		p.Home = q == ""
		if n == 1 {
			p.Hero = hero
		}
	}
	if q != "" {
		p.Title = a.text(r, "Поиск:") + " " + q
	}
	if len(posts) > 30 {
		p.Next = n + 1
		p.Posts = posts[:30]
	}
	if n > 1 {
		p.Previous = n - 1
	}
	a.render(w, r, p)
}
func (a *Server) show(w http.ResponseWriter, r *http.Request) {
	p, e := a.requestedPost(r)
	if errors.Is(e, ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if e != nil {
		a.fail(w, e)
		return
	}
	u := currentUser(r)
	if p.Deleted || !p.Public(time.Now()) && (u == nil || !u.Admin) {
		http.NotFound(w, r)
		return
	}
	if p.Kind != r.PathValue("kind") || strings.HasSuffix(r.URL.Path, "index.html") {
		http.Redirect(w, r, p.URL(), 301)
		return
	}
	if !p.Readable(u) {
		a.render(w, r, page{Title: p.Title, Mode: "locked", Post: p})
		return
	}
	if p.ExternalURL != "" {
		http.Redirect(w, r, p.ExternalURL, 302)
		return
	}
	if info, ok := r.Context().Value(renderCacheKey{}).(*renderCacheInfo); ok {
		info.postID = p.ID
	} else if r.Method == "GET" {
		a.recordView(p.ID)
	}
	cs, e := a.Store.Comments(r.Context(), p.ID)
	if e != nil {
		a.fail(w, e)
		return
	}
	cv := []commentView{}
	for _, c := range cs {
		cv = append(cv, commentView{c, a.md.Comment(c.Body)})
	}
	body := strings.ReplaceAll(string(a.md.RenderPost(p)), `href="?block=`, `href="?lang=`+url.QueryEscape(p.Lang)+`&amp;block=`)
	if u == nil {
		body = strings.ReplaceAll(body, "#comment-form", "#comments")
	}
	pageData := page{Title: p.Title, Mode: "post", Post: p, HTML: template.HTML(body), Comments: cv, Block: r.URL.Query().Get("block"), ReplyTo: r.URL.Query().Get("reply")}
	rows, e := a.Store.DB.Query(r.Context(), `SELECT `+postCols+` FROM posts WHERE slug=$1 AND lang<>$2 AND visible AND NOT deleted AND published_at<=now()`, p.Slug, p.Lang)
	if e != nil {
		a.fail(w, e)
		return
	}
	defer rows.Close()
	for rows.Next() {
		other, e := scanPost(rows)
		if e != nil {
			a.fail(w, e)
			return
		}
		pageData.Translations = append(pageData.Translations, other)
	}
	if e = rows.Err(); e != nil {
		a.fail(w, e)
		return
	}
	a.render(w, r, pageData)
}
func (a *Server) postStyle(w http.ResponseWriter, r *http.Request) {
	p, e := a.Store.PostID(r.Context(), r.PathValue("id"))
	if e != nil || !p.Readable(currentUser(r)) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	fmt.Fprint(w, p.CSS)
}
func (a *Server) legacyEdit(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	p, e := a.Store.Post(r.Context(), r.PathValue("slug"), a.lang(r))
	if e != nil {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/godmode/posts/"+p.ID+"/", 302)
}
