package blog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testApp(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()
	dsn := os.Getenv("TEST_DATABASE_URL")
	var store *Store
	var e error
	if dsn == "" || dsn == "sqlite" {
		store, e = Open(ctx, "sqlite:"+filepath.Join(t.TempDir(), "blog.db"))
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(store.DB.Close)
	} else {
		root, err := pgxpool.New(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		schema := "test_" + randomToken()[:16]
		if _, err = root.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
			t.Fatal(err)
		}
		parsed, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		q := parsed.Query()
		q.Set("search_path", schema)
		parsed.RawQuery = q.Encode()
		store, e = Open(ctx, parsed.String())
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() {
			store.DB.Close()
			_, _ = root.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
			root.Close()
		})
	}
	if e = store.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	if e = store.Migrate(ctx); e != nil {
		t.Fatal("migration is not idempotent:", e)
	}
	app, e := NewServer(ctx, store, Config{BaseURL: "http://blog.test", Title: "Test blog", Author: "Author", Language: "ru", SMTPAddr: "127.0.0.1:1", MailFrom: "blog@example.test"})
	if e != nil {
		t.Fatal(e)
	}
	for _, role := range []string{"owner", "member"} {
		if _, e = store.DB.Exec(ctx, `INSERT INTO users(id,email,name,password_hash,is_admin) VALUES($1,$2,$3,'',$4)`, randomToken(), role+"@example.test", role, role == "owner"); e != nil {
			t.Fatal(e)
		}
	}
	return app
}

type browser struct {
	app     *Server
	cookies map[string]*http.Cookie
}

func newBrowser(a *Server) *browser { return &browser{a, map[string]*http.Cookie{}} }
func (b *browser) request(method, path string, form url.Values) *httptest.ResponseRecorder {
	if form == nil {
		form = url.Values{}
	}
	if c := b.cookies["csrf"]; c != nil && form.Get("csrf") == "" {
		form.Set("csrf", c.Value)
	}
	r := httptest.NewRequest(method, "http://blog.test"+path, strings.NewReader(form.Encode()))
	r.RemoteAddr = "127.0.0.1:12345"
	if method == "POST" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "http://blog.test")
	}
	for _, c := range b.cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	b.app.ServeHTTP(w, r)
	for _, c := range w.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c
		}
	}
	return w
}
func (b *browser) login(t *testing.T, role string) {
	t.Helper()
	u, err := b.app.Store.UserEmail(context.Background(), role+"@example.test")
	if err != nil {
		t.Fatal(err)
	}
	token := randomToken()
	var key any
	if u.Admin {
		key = randomToken()
		_, err = b.app.Store.DB.Exec(context.Background(), `INSERT INTO admin_passkeys(id,user_id,credential,label) VALUES($1,$2,'{}','Test fixture')`, key, u.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = b.app.Store.DB.Exec(context.Background(), `INSERT INTO sessions(token_hash,user_id,passkey_id,expires_at) VALUES($1,$2,$3,now()+interval '1 day')`, hash(token), u.ID, key)
	if err != nil {
		t.Fatal(err)
	}
	b.cookies["session"] = &http.Cookie{Name: "session", Value: token}
	b.request("GET", "/profile/", nil)
}
func seedPost(t *testing.T, a *Server, slug string, visible, private bool, at time.Time) Post {
	t.Helper()
	p := Post{Slug: slug, Kind: "notes", Lang: "ru", Title: slug, Body: "body-secret-" + slug, Visible: visible, MembersOnly: private, Commentable: true, OnHome: true, PublishedAt: at}
	if e := a.Store.SavePost(context.Background(), &p); e != nil {
		t.Fatal(e)
	}
	return p
}
func TestHTTPIntegration(t *testing.T) {
	a := testApp(t)
	now := time.Now().Add(-time.Hour)
	public := seedPost(t, a, "public", true, false, now)
	seedPost(t, a, "private", true, true, now)
	seedPost(t, a, "draft", false, false, now)
	seedPost(t, a, "future", true, false, time.Now().Add(time.Hour))
	anonymous := newBrowser(a)
	owner := newBrowser(a)
	owner.login(t, "owner")
	member := newBrowser(a)
	member.login(t, "member")
	t.Run("public and protected routes", func(t *testing.T) {
		for _, path := range []string{"/", "/all/", "/notes/", "/notes/public/", "/rss/", "/feed.json", "/sitemap.xml", "/robots.txt", "/static/site.css", "/static/navigation.js", "/static/css/theme.css", "/static/fonts/Ubuntu-Regular.woff2", "/healthz"} {
			w := anonymous.request("GET", path, nil)
			if w.Code != 200 {
				t.Errorf("%s: %d %s", path, w.Code, w.Body.String())
			}
		}
		for _, path := range []string{"/notes/draft/", "/notes/draft/?preview=1", "/notes/future/"} {
			w := anonymous.request("GET", path, nil)
			if w.Code != 404 {
				t.Errorf("%s leaks: %d", path, w.Code)
			}
		}
		w := anonymous.request("GET", "/notes/private/", nil)
		if strings.Contains(w.Body.String(), "body-secret-private") {
			t.Fatal("private body exposed")
		}
		for _, path := range []string{"/rss/", "/rss/public/", "/rss/blog/", "/feed.json", "/sitemap.xml", "/search/?q=private"} {
			w := anonymous.request("GET", path, nil)
			if strings.Contains(w.Body.String(), "body-secret-private") || strings.Contains(w.Body.String(), "/notes/private/") {
				t.Errorf("private data exposed at %s", path)
			}
		}
		if w = anonymous.request("GET", "/rss/private/", nil); w.Code != 401 {
			t.Fatal("private feed open")
		}
		if w = member.request("GET", "/notes/private/", nil); !strings.Contains(w.Body.String(), "body-secret-private") {
			t.Fatal("member cannot read")
		}
		if w = member.request("GET", "/notes/draft/", nil); w.Code != 404 {
			t.Fatal("member reads draft")
		}
		if w = owner.request("GET", "/notes/draft/", nil); w.Code != 200 {
			t.Fatal("owner cannot preview")
		}
		if w = anonymous.request("GET", "/blog/public/", nil); w.Code != 301 || w.Header().Get("Location") != public.URL() {
			t.Fatal("canonical redirect failed")
		}
	})
	t.Run("csrf and admin boundary", func(t *testing.T) {
		r := httptest.NewRequest("POST", "http://blog.test/godmode/posts/new/", strings.NewReader("csrf=forged"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "http://evil.test")
		r.AddCookie(&http.Cookie{Name: "csrf", Value: "forged"})
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal("forged csrf accepted")
		}
		if w = member.request("GET", "/godmode/", nil); w.Code != 403 {
			t.Fatal("member is admin")
		}
		for _, path := range []string{"/godmode/", "/godmode/posts/new/", "/godmode/posts/" + public.ID + "/", "/godmode/export/"} {
			if w = owner.request("GET", path, nil); w.Code != 200 {
				t.Errorf("admin page %s: %d %s", path, w.Code, w.Body.String())
			}
		}
		form := url.Values{"slug": {"created"}, "kind": {"blog"}, "lang": {"en"}, "title": {"Created via editor"}, "body": {"Hello **world**"}, "published_at": {"2026-01-01T10:00"}, "visible": {"on"}, "on_home": {"on"}, "commentable": {"on"}}
		if w = owner.request("POST", "/godmode/posts/new/", form); w.Code != 303 {
			t.Fatalf("save: %d %s", w.Code, w.Body.String())
		}
		if w = anonymous.request("GET", "/blog/created/?lang=en", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "<strong>world</strong>") {
			t.Fatal("published post not rendered")
		}
	})
	t.Run("comments and concurrent reactions", func(t *testing.T) {
		w := member.request("POST", "/comments/create/", url.Values{"post_id": {public.ID}, "body": {"Hello <script>alert(1)</script>"}, "block": {"10"}})
		if w.Code != 303 {
			t.Fatalf("comment: %d %s", w.Code, w.Body.String())
		}
		comments, e := a.Store.Comments(context.Background(), public.ID)
		if e != nil || len(comments) != 1 {
			t.Fatal("comment not saved", e)
		}
		id := comments[0].ID
		w = anonymous.request("GET", public.URL(), nil)
		if strings.Contains(w.Body.String(), "<script>alert") {
			t.Fatal("comment XSS")
		}
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				b := newBrowser(a)
				for k, c := range member.cookies {
					copy := *c
					b.cookies[k] = &copy
				}
				w := b.request("POST", "/clickers/comments/"+id+"/", nil)
				if w.Code != 303 {
					t.Errorf("vote: %d", w.Code)
				}
			}()
		}
		wg.Wait()
		comments, e = a.Store.Comments(context.Background(), public.ID)
		if e != nil || comments[0].Votes != 1 {
			t.Fatalf("duplicate votes: %+v %v", comments, e)
		}
		if w = member.request("POST", "/comments/"+id+"/delete/", nil); w.Code != 403 {
			t.Fatal("member can moderate")
		}
		if w = owner.request("POST", "/comments/"+id+"/delete/", nil); w.Code != 303 {
			t.Fatal("owner cannot delete")
		}
	})
	t.Run("subscriptions and deduplicated announcement", func(t *testing.T) {
		b := newBrowser(a)
		b.request("GET", "/subscribe/", nil)
		w := b.request("POST", "/subscribe/", url.Values{"email": {"reader@example.test"}})
		if w.Code != 200 {
			t.Fatalf("subscribe: %d %s", w.Code, w.Body.String())
		}
		var body string
		if e := a.Store.DB.QueryRow(context.Background(), `SELECT body FROM outbox WHERE recipient='reader@example.test'`).Scan(&body); e != nil {
			t.Fatal(e)
		}
		prefix := "http://blog.test/subscribe/confirm/"
		start := strings.Index(body, prefix)
		token := strings.Split(body[start+len(prefix):], "/")[0]
		w = b.request("GET", "/subscribe/confirm/"+token+"/", nil)
		if w.Code != 200 {
			t.Fatal("confirm failed")
		}
		for i := 0; i < 2; i++ {
			w = owner.request("POST", "/godmode/posts/"+public.ID+"/announce/", nil)
			if w.Code != 200 {
				t.Fatalf("announce: %d %s", w.Code, w.Body.String())
			}
		}
		var count int
		if e := a.Store.DB.QueryRow(context.Background(), `SELECT count(*) FROM outbox WHERE dedup_key LIKE 'post:%'`).Scan(&count); e != nil || count != 1 {
			t.Fatal("announcement duplicated", count, e)
		}
	})
	t.Run("survives new server instance", func(t *testing.T) {
		second, e := NewServer(context.Background(), a.Store, a.Config)
		if e != nil {
			t.Fatal(e)
		}
		owner.app = second
		w := owner.request("GET", "/godmode/", nil)
		if w.Code != 200 {
			t.Fatal("session lost after restart")
		}
	})
	t.Run("database constraints", func(t *testing.T) {
		duplicate := public
		duplicate.ID = ""
		if e := a.Store.SavePost(context.Background(), &duplicate); e == nil {
			t.Fatal("duplicate slug allowed")
		}
	})
	t.Run("import rolls back all records on conflict", func(t *testing.T) {
		p := Post{ID: "import-new", Slug: "import-new", Kind: "blog", Lang: "ru", PublishedAt: now}
		conflict := public
		conflict.ID = "different-id"
		if e := a.Store.ImportPosts(context.Background(), []Post{p, conflict}); e == nil {
			t.Fatal("expected duplicate-address error")
		}
		var count int
		if e := a.Store.DB.QueryRow(context.Background(), `SELECT count(*) FROM posts WHERE id='import-new'`).Scan(&count); e != nil || count != 0 {
			t.Fatal("partial import committed", e)
		}
	})
}
