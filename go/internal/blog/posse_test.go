package blog

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func posseApp(t *testing.T) *Server {
	a := testApp(t)
	a.Config.TelegramChannel = "@testchannel"
	a.Config.TelegramBotToken = "12345:test-token"
	if e := a.initPOSSE(); e != nil {
		t.Fatal(e)
	}
	a.Config.BaseURL = "https://blog.example"
	return a
}
func possePost(t *testing.T, a *Server, slug string) Post {
	p := seedPost(t, a, slug, true, false, time.Now().Add(-time.Hour))
	p, e := a.Store.PostID(context.Background(), p.ID)
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func queuePOSSE(t *testing.T, a *Server, p Post) {
	t.Helper()
	if e := enqueueSyndication(context.Background(), a.Store.DB, p, a.telegramTarget().UID, a.telegramText(p)); e != nil {
		t.Fatal(e)
	}
}
func jobPOSSE(t *testing.T, a *Server, p Post) Syndication {
	t.Helper()
	jobs, e := a.syndications(context.Background(), p.ID)
	if e != nil || len(jobs) != 1 {
		t.Fatalf("jobs %v %v", jobs, e)
	}
	return jobs[0]
}
func runPOSSE(t *testing.T, a *Server) {
	t.Helper()
	if e := a.processSyndication(context.Background()); e != nil {
		t.Fatal(e)
	}
}
func fakeTelegram(a *Server, body string, calls *atomic.Int32) {
	a.posseHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}
}

const telegramSuccess = `{"ok":true,"result":{"message_id":42,"chat":{"type":"channel","username":"testchannel"}}}`

func TestPOSSEFormatting(t *testing.T) {
	a := &Server{Config: Config{BaseURL: "https://blog.example"}, md: NewMarkdown()}
	p := Post{Kind: "notes", Slug: "unicode", Lang: "ru", Title: "Title <&>", Body: strings.Repeat("😀Ж", 4000)}
	text := a.telegramText(p)
	if utf16Length(text) > 4096 || !strings.HasSuffix(text, "https://blog.example"+p.URL()) || !strings.Contains(text, "…") {
		t.Fatal("bad truncation or lost canonical")
	}
	p.Kind = "blog"
	p.Summary = "Short summary"
	p.Body = "Secret full article"
	if text = a.telegramText(p); !strings.Contains(text, p.Summary) || strings.Contains(text, p.Body) {
		t.Fatal(text)
	}
	if plain := possePlain(`<p>Hello <b>world</b></p><script>script</script><iframe>frame</iframe><details>hidden</details><span class="spoiler">secret</span>`); strings.Contains(plain, "secret") || strings.Contains(plain, "hidden") || strings.Contains(plain, "script") || !strings.Contains(plain, "Hello world") {
		t.Fatal(plain)
	}
	for _, s := range []string{"https://t.me/testchannel/1?evil=1", "https://t.me/testchannel/0", "https://t.me/testchannel/../5", "https://t.me/other/1"} {
		if telegramMessageURL(s, "https://t.me/testchannel") {
			t.Fatal(s)
		}
	}
}
func TestPOSSEQueueDedupAndLinks(t *testing.T) {
	a := posseApp(t)
	p := possePost(t, a, "once")
	stale := p
	var calls atomic.Int32
	a.posseHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.Host != "api.telegram.org" || r.Method != "POST" {
			t.Error("wrong request")
		}
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if payload["chat_id"] != "@testchannel" || payload["text"] != a.telegramText(p) || payload["parse_mode"] != nil {
			t.Error("wrong payload")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(telegramSuccess)), Header: http.Header{}}, nil
	})}
	queuePOSSE(t, a, p)
	queuePOSSE(t, a, p)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e := a.processSyndication(context.Background()); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 || jobPOSSE(t, a, p).State != "sent" {
		t.Fatal("duplicated or not sent", calls.Load(), jobPOSSE(t, a, p))
	}
	stale.Title = "Edited later"
	if e := a.Store.SavePost(context.Background(), &stale); e != nil {
		t.Fatal(e)
	}
	p, _ = a.Store.PostID(context.Background(), p.ID)
	if !strings.Contains(p.micropubExtras(), `class="u-syndication"`) || !strings.Contains(string(a.md.RenderContent(p)), "https://t.me/testchannel/42") {
		t.Fatal("lost syndication link")
	}
	if len(a.mpSource(p)["syndication"]) != 1 {
		t.Fatal("missing source link")
	}
	markdown, e := a.markdownBody(p, a.Config.BaseURL+p.URL())
	if e != nil || !strings.Contains(markdown, "https://t.me/testchannel/42") {
		t.Fatal("missing Markdown link", e)
	}
	queuePOSSE(t, a, p)
	runPOSSE(t, a)
	if calls.Load() != 1 {
		t.Fatal("sent twice")
	}
}
func TestPOSSEFailureHandling(t *testing.T) {
	for _, tc := range []struct {
		name, reply, state string
		network            bool
	}{
		{"forbidden", `{"ok":false,"error_code":403,"description":"secret token"}`, "failed", false},
		{"rate", `{"ok":false,"error_code":429,"parameters":{"retry_after":120}}`, "queued", false},
		{"server", `{"ok":false,"error_code":500}`, "uncertain", false},
		{"malformed", `oops secret token`, "uncertain", false},
		{"timeout", "", "uncertain", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := posseApp(t)
			p := possePost(t, a, tc.name)
			var calls atomic.Int32
			fakeTelegram(a, tc.reply, &calls)
			if tc.network {
				a.posseHTTP = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, errors.New("secret token") })}
			}
			queuePOSSE(t, a, p)
			runPOSSE(t, a)
			runPOSSE(t, a)
			job := jobPOSSE(t, a, p)
			if job.State != tc.state || calls.Load() != 1 || strings.Contains(job.Detail, "secret") {
				t.Fatal(job, calls.Load())
			}
			if tc.name == "rate" {
				var seconds float64
				_ = a.Store.DB.QueryRow(context.Background(), `SELECT extract(epoch from available_at-now()) FROM syndications WHERE id=$1`, job.ID).Scan(&seconds)
				if seconds < 110 {
					t.Fatal("retry too soon", seconds)
				}
			}
		})
	}
}
func TestPOSSERechecksVisibilityRevisionAndChannel(t *testing.T) {
	for _, change := range []string{"private", "draft", "deleted", "future", "edit", "channel"} {
		t.Run(change, func(t *testing.T) {
			a := posseApp(t)
			p := possePost(t, a, change)
			var calls atomic.Int32
			fakeTelegram(a, telegramSuccess, &calls)
			queuePOSSE(t, a, p)
			switch change {
			case "private":
				p.MembersOnly = true
			case "draft":
				p.Visible = false
			case "deleted":
				p.Deleted = true
			case "future":
				p.PublishedAt = time.Now().Add(time.Hour)
			case "edit":
				p.Body = "Changed"
			case "channel":
				a.Config.TelegramChannel = "@anotherchannel"
			}
			if change != "channel" {
				if e := a.Store.SavePost(context.Background(), &p); e != nil {
					t.Fatal(e)
				}
			}
			runPOSSE(t, a)
			if calls.Load() != 0 || jobPOSSE(t, a, p).State != "cancelled" {
				t.Fatal("unsafe send")
			}
		})
	}
}
func TestPOSSECrashRecovery(t *testing.T) {
	a := posseApp(t)
	p := possePost(t, a, "crash")
	var calls atomic.Int32
	fakeTelegram(a, telegramSuccess, &calls)
	queuePOSSE(t, a, p)
	_, e := a.Store.DB.Exec(context.Background(), `UPDATE syndications SET state='sending',updated_at=now()-interval '3 minutes'`)
	if e != nil {
		t.Fatal(e)
	}
	runPOSSE(t, a)
	if jobPOSSE(t, a, p).State != "uncertain" || calls.Load() != 0 {
		t.Fatal("resent after crash")
	}
}
func posseBrowserRequest(b *browser, method, path string, form url.Values) *httptest.ResponseRecorder {
	if form == nil {
		form = url.Values{}
	}
	if c := b.cookies["csrf"]; c != nil {
		form.Set("csrf", c.Value)
	}
	r := httptest.NewRequest(method, b.app.Config.BaseURL+path, strings.NewReader(form.Encode()))
	r.RemoteAddr = "127.0.0.1:4321"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", b.app.Config.BaseURL)
	for _, c := range b.cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	b.app.ServeHTTP(w, r)
	for _, c := range w.Result().Cookies() {
		b.cookies[c.Name] = c
	}
	return w
}
func TestPOSSEAdminPreviewAndRetry(t *testing.T) {
	a := posseApp(t)
	p := possePost(t, a, "preview")
	b := newBrowser(a)
	b.login(t, "owner")
	path := "/godmode/posts/" + p.ID + "/posse/"
	if w := posseBrowserRequest(b, "GET", path, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "Предпросмотр сообщения") {
		t.Fatal(w.Code, w.Body.String())
	}
	member := newBrowser(a)
	member.login(t, "member")
	if w := posseBrowserRequest(member, "GET", path, nil); w.Code != 403 {
		t.Fatal("nonadmin access")
	}
	form := url.Values{"action": {"send"}, "revision": {"stale"}}
	if w := posseBrowserRequest(b, "POST", path, form); w.Code != 409 {
		t.Fatal("stale preview accepted")
	}
	form.Set("revision", posseRevision(p, a.telegramText(p), a.telegramTarget().UID))
	if w := posseBrowserRequest(b, "POST", path, form); w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	var calls atomic.Int32
	fakeTelegram(a, `{"ok":false,"error_code":403}`, &calls)
	runPOSSE(t, a)
	form.Set("action", "retry")
	if w := posseBrowserRequest(b, "POST", path, form); w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	fakeTelegram(a, telegramSuccess, &calls)
	runPOSSE(t, a)
	if calls.Load() != 2 || jobPOSSE(t, a, p).State != "sent" {
		t.Fatal("retry failed")
	}
}
func TestMicropubPOSSE(t *testing.T) {
	a := testApp(t)
	token := issueTestToken(t, a, "create read")
	a.Config.TelegramChannel = "@testchannel"
	a.Config.TelegramBotToken = "123:test"
	a.Config.BaseURL = "https://blog.example"
	_ = a.initPOSSE()
	if _, e := a.Store.DB.Exec(context.Background(), `UPDATE indieauth_tokens SET me=$1`, a.Config.BaseURL+"/"); e != nil {
		t.Fatal(e)
	}
	cfg := apiRequest(a, "GET", "/micropub?q=syndicate-to", "", "", token)
	if cfg.Code != 200 || !strings.Contains(cfg.Body.String(), "https://t.me/testchannel") {
		t.Fatal(cfg.Code, cfg.Body.String())
	}
	for _, props := range []mpProperties{
		{"content": {"no"}, "mp-syndicate-to": {"https://t.me/wrong"}},
		{"content": {"no"}, "post-status": {"draft"}, "mp-syndicate-to": {"https://t.me/testchannel"}},
		{"content": {"no"}, "visibility": {"private"}, "mp-syndicate-to": {"https://t.me/testchannel"}},
	} {
		w := mpJSON(a, token, map[string]any{"properties": props})
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	var count int
	_ = a.Store.DB.QueryRow(context.Background(), `SELECT count(*) FROM posts`).Scan(&count)
	if count != 0 {
		t.Fatal("invalid request saved post")
	}
	w := mpJSON(a, token, map[string]any{"properties": mpProperties{"content": {"Hello"}, "mp-syndicate-to": {"https://t.me/testchannel"}}})
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	posts, e := a.Store.Posts(context.Background(), Filter{Admin: true})
	if e != nil || len(posts) != 1 {
		t.Fatal(e, len(posts))
	}
	p := posts[0]
	if jobPOSSE(t, a, p).State != "queued" {
		t.Fatal("missing atomic job")
	}
	var calls atomic.Int32
	fakeTelegram(a, telegramSuccess, &calls)
	runPOSSE(t, a)
	if calls.Load() != 1 || jobPOSSE(t, a, p).State != "sent" {
		t.Fatal("MP post not sent", jobPOSSE(t, a, p))
	}
	// Form-encoded command is accepted as well.
	form := url.Values{"h": {"entry"}, "content": {"From form"}, "mp-syndicate-to[]": {"https://t.me/testchannel"}}
	w = apiRequest(a, "POST", "/micropub", "application/x-www-form-urlencoded", form.Encode(), token)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	runPOSSE(t, a)
	if calls.Load() != 2 {
		t.Fatal("form syndication failed")
	}
}

func TestPOSSEUncertainResolutionAndReadiness(t *testing.T) {
	a := posseApp(t)
	p := possePost(t, a, "uncertain")
	b := newBrowser(a)
	b.login(t, "owner")
	path := "/godmode/posts/" + p.ID + "/posse/"
	queuePOSSE(t, a, p)
	job := jobPOSSE(t, a, p)
	_, e := a.Store.DB.Exec(context.Background(), `UPDATE syndications SET state='uncertain' WHERE id=$1`, job.ID)
	if e != nil {
		t.Fatal(e)
	}
	form := url.Values{"action": {"retry-checked"}, "revision": {posseRevision(p, a.telegramText(p), a.telegramTarget().UID)}}
	if w := posseBrowserRequest(b, "POST", path, form); w.Code != 400 {
		t.Fatal("unconfirmed retry", w.Code)
	}
	form = url.Values{"action": {"resolve"}, "job": {job.ID}, "url": {"https://t.me/another/1"}}
	if w := posseBrowserRequest(b, "POST", path, form); w.Code != 400 {
		t.Fatal("wrong channel accepted")
	}
	form.Set("url", "https://t.me/testchannel/99")
	if w := posseBrowserRequest(b, "POST", path, form); w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	if jobPOSSE(t, a, p).State != "sent" {
		t.Fatal("not resolved")
	}
	p, _ = a.Store.PostID(context.Background(), p.ID)
	if !strings.Contains(p.micropubExtras(), "https://t.me/testchannel/99") {
		t.Fatal("missing resolved link")
	}
	a.Config.BaseURL = "http://localhost:8080"
	if a.posseReady() == "" || len(a.syndicationTargets()) != 0 {
		t.Fatal("localhost enabled")
	}
	a.Config.BaseURL = "https://blog.example"
	a.Config.TelegramBotToken = ""
	if a.posseReady() == "" || len(a.syndicationTargets()) != 0 {
		t.Fatal("missing token enabled")
	}
}
