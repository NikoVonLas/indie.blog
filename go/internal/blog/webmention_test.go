package blog

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func wmResponse(r *http.Request, status int, body string, headers http.Header) *http.Response {
	if headers == nil {
		headers = http.Header{}
	}
	if headers.Get("Content-Type") == "" {
		headers.Set("Content-Type", "text/html")
	}
	return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}
func wmClient(fn func(*http.Request) *http.Response) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { return fn(r), nil })}
}
func TestWebmentionDiscovery(t *testing.T) {
	base, _ := url.Parse("https://target.example/redirected/post")
	for _, tc := range []struct {
		name, header, body, want string
		invalid                  bool
	}{
		{"header priority", `</wm?x=1>; rel="webmention"`, `<link rel=webmention href=/wrong>`, "https://target.example/wm?x=1", false},
		{"header list", `</other>; rel=alternate; title="one, two; three", <../mention>; rel="other webmention"`, "", "https://target.example/mention", false},
		{"a before link", "", `<a href="../first" rel="webmention"></a><link rel=webmention href=/second>`, "https://target.example/first", false},
		{"base", "", `<base href="https://target.example/api/"><link rel=webmention href="send?token=hello&amp;x=1">`, "https://target.example/api/send?token=hello&x=1", false},
		{"empty endpoint", "", `<a rel=webmention href="">send</a>`, "https://target.example/redirected/post", false},
		{"none", "", `<script>"<link rel=webmention href=/bad>"</script><template><a rel=webmention href=/bad></a></template>`, "", false},
		{"private", `<http://127.0.0.1/wm>; rel=webmention`, "", "", true},
		{"unsafe", `<javascript:alert(1)>; rel=webmention`, "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpoint, err := discoverMentionEndpoint(mentionDocument{URL: base, HTML: true, Header: http.Header{"Link": {tc.header}}, Body: []byte(tc.body)})
			if (err != nil) != tc.invalid || endpoint != tc.want {
				t.Fatalf("got %q %v want %q", endpoint, err, tc.want)
			}
		})
	}
}
func TestWebmentionVerification(t *testing.T) {
	target := "https://blog.example/notes/a/?lang=ru#block-1"
	for _, tc := range []struct {
		name, body, want, kind string
		status                 int
	}{
		{"exact", `<title>A &amp; B</title><a class=u-in-reply-to href="` + target + `">reply</a>`, "verified", "reply", 200},
		{"wrong fragment", `<a href="https://blog.example/notes/a/?lang=ru#other">x</a>`, "removed", "mention", 200},
		{"plain text", `<p>` + target + `</p><!-- <a href="` + target + `"> --><script>"<a href='` + target + `'>"</script>`, "removed", "mention", 200},
		{"relative base", `<base href=https://blog.example/><a class=u-like-of href="notes/a/?lang=ru#block-1">like</a>`, "verified", "like", 200},
		{"gone", "", "removed", "mention", 410},
		{"unavailable", "", "retry", "mention", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := wmClient(func(r *http.Request) *http.Response { return wmResponse(r, tc.status, tc.body, nil) })
			state, _, kind, _ := verifyMention(context.Background(), client, "https://source.example/post", target)
			if state != tc.want || kind != tc.kind {
				t.Fatalf("%s %s", state, kind)
			}
		})
	}
	targets := outgoingTargets(`<a href=https://target.example/a>A</a><a href=https://target.example/a>B</a><a href=/local>x</a><a href=http://127.0.0.1/>x</a><img src=https://images.example/a><pre>&lt;a href='https://code.example/'&gt;</pre>`, "https://blog.example/notes/a/")
	if len(targets) != 1 || targets[0] != "https://target.example/a" {
		t.Fatal(targets)
	}
}
func TestWebmentionDelivery(t *testing.T) {
	source, target := "https://blog.example/notes/a/?lang=ru", "https://target.example/post#part"
	var posted bool
	client := wmClient(func(r *http.Request) *http.Response {
		if r.Method == "GET" {
			return wmResponse(r, 200, "", http.Header{"Link": {`</wm?token=keep>; rel=webmention`}})
		}
		posted = true
		if r.URL.String() != "https://target.example/wm?token=keep" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Error("endpoint or content type", r.URL)
		}
		_ = r.ParseForm()
		if r.PostForm.Get("source") != source || r.PostForm.Get("target") != target || r.PostForm.Get("token") != "" {
			t.Error("wrong form", r.PostForm)
		}
		return wmResponse(r, 202, "", nil)
	})
	if state, detail := deliverMention(context.Background(), client, source, target); state != "sent" || !posted {
		t.Fatal(state, detail)
	}
	client = wmClient(func(r *http.Request) *http.Response {
		if r.Method == "POST" {
			t.Fatal("private endpoint reached")
		}
		return wmResponse(r, 200, `<link rel=webmention href="http://169.254.169.254/">`, nil)
	})
	if state, _ := deliverMention(context.Background(), client, source, target); state != "failed" {
		t.Fatal(state)
	}
	// A 302 must not be followed as GET and mistaken for successful delivery.
	client = wmClient(func(r *http.Request) *http.Response {
		if r.Method == "POST" {
			return wmResponse(r, 302, "", http.Header{"Location": {"/redirect"}})
		}
		if r.URL.Path == "/redirect" {
			t.Fatal("POST converted to GET")
		}
		return wmResponse(r, 200, `<link rel=webmention href=/wm>`, nil)
	})
	if state, _ := deliverMention(context.Background(), client, source, target); state != "failed" {
		t.Fatal(state)
	}
}
func TestWebmentionInboxModerationAndUpdates(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	p := seedPost(t, a, "mention-target", true, false, time.Now().Add(-time.Hour))
	target := a.Config.BaseURL + p.URL() + "#block-1"
	source := "https://source.example/post"
	receive := func(source, target string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "http://blog.test/webmention", strings.NewReader(url.Values{"source": {source}, "target": {target}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.RemoteAddr = "127.0.0.1:1234"
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		return w
	}
	for _, bad := range []string{"http://127.0.0.1/", "file:///tmp/test", target} {
		if w := receive(bad, target); w.Code != 400 {
			t.Fatal("invalid source", w.Code)
		}
	}
	for _, bad := range []string{"http://other.example/notes/mention-target/", "http://blog.test/", "http://blog.test/notes/missing/"} {
		if w := receive(source, bad); w.Code != 400 {
			t.Fatal("invalid target", w.Code)
		}
	}
	for _, flags := range []struct{ visible, private, comments bool }{{false, false, true}, {true, true, true}, {true, false, false}} {
		p.Visible, p.MembersOnly, p.Commentable = flags.visible, flags.private, flags.comments
		_ = a.Store.SavePost(ctx, &p)
		if w := receive(source, target); w.Code != 400 {
			t.Fatal("protected target accepted")
		}
	}
	p.Visible, p.MembersOnly, p.Commentable = true, false, true
	_ = a.Store.SavePost(ctx, &p)
	w := newBrowser(a).request("GET", p.URL(), nil)
	if !strings.Contains(w.Header().Get("Link"), `rel="webmention"`) || !strings.Contains(w.Body.String(), `<link rel="webmention"`) {
		t.Fatal("missing discovery")
	}
	for i := 0; i < 2; i++ {
		if w = receive(source, target); w.Code != 202 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	rows, _ := a.Store.Mentions(ctx, "", false, 0)
	if len(rows) != 1 {
		t.Fatal("duplicate", rows)
	}
	id := rows[0].ID
	_, _ = a.Store.DB.Exec(ctx, `UPDATE webmentions SET available_at=now() WHERE id=$1`, id)
	title := "External response"
	client := wmClient(func(r *http.Request) *http.Response {
		return wmResponse(r, 200, `<title>`+title+`</title><a class=u-in-reply-to href="`+target+`">reply</a>`, nil)
	})
	if worked, err := a.processMention(ctx, client); !worked || err != nil {
		t.Fatal(worked, err)
	}
	anon := newBrowser(a)
	owner := newBrowser(a)
	owner.login(t, "owner")
	member := newBrowser(a)
	member.login(t, "member")
	if strings.Contains(anon.request("GET", p.URL(), nil).Body.String(), title) {
		t.Fatal("unmoderated mention visible")
	}
	if w = member.request("POST", "/godmode/webmentions/"+id+"/", url.Values{"action": {"approve"}}); w.Code != 403 {
		t.Fatal("member moderation")
	}
	if w = owner.request("POST", "/godmode/webmentions/"+id+"/", url.Values{"action": {"approve"}, "csrf": {"bad"}}); w.Code != 403 {
		t.Fatal("CSRF moderation")
	}
	owner.request("POST", "/godmode/webmentions/"+id+"/", url.Values{"action": {"approve"}})
	if !strings.Contains(anon.request("GET", p.URL(), nil).Body.String(), title) {
		t.Fatal("approved mention missing")
	}
	// Reverification with unchanged content preserves approval, changed titles require review again.
	for _, changed := range []bool{false, true} {
		if changed {
			title = "Changed response"
		}
		owner.request("POST", "/godmode/webmentions/"+id+"/", url.Values{"action": {"recheck"}})
		_, err := a.processMention(ctx, client)
		if err != nil {
			t.Fatal(err)
		}
		public, _ := a.Store.Mentions(ctx, p.ID, true, 0)
		if (len(public) == 0) != changed {
			t.Fatal("moderation update", changed, public)
		}
	}
	owner.request("POST", "/godmode/webmentions/"+id+"/", url.Values{"action": {"approve"}})
	owner.request("POST", "/godmode/webmentions/"+id+"/", url.Values{"action": {"recheck"}})
	gone := wmClient(func(r *http.Request) *http.Response { return wmResponse(r, 410, "", nil) })
	_, _ = a.processMention(ctx, gone)
	public, _ := a.Store.Mentions(ctx, p.ID, true, 0)
	if len(public) != 0 {
		t.Fatal("gone source retained")
	}
	// The protocol exception does not exempt other POST endpoints from CSRF.
	w = anon.request("POST", "/profile/", url.Values{"name": {"bad"}, "csrf": {"bad"}})
	if w.Code != 403 {
		t.Fatal("CSRF exception widened")
	}
}
func TestWebmentionPublicPagination(t *testing.T) {
	a := testApp(t)
	p := seedPost(t, a, "mention-pages", true, false, time.Now().Add(-time.Hour))
	for n := 1; n <= 51; n++ {
		value := strconv.Itoa(n)
		_, err := a.Store.DB.Exec(context.Background(), `INSERT INTO webmentions(id,direction,post_id,source,target,state,moderation,title) VALUES($1,'in',$2,$3,$4,'verified','approved',$5)`, "page-"+value, p.ID, "https://source.example/"+value, a.Config.BaseURL+p.URL(), "Mention "+value)
		if err != nil {
			t.Fatal(err)
		}
	}
	b := newBrowser(a)
	for _, page := range []struct {
		query string
		count int
		link  string
	}{
		{"", 50, "&amp;wm_page=2#webmentions-heading"},
		{"&wm_page=2", 1, "&amp;wm_page=1#webmentions-heading"},
	} {
		w := b.request("GET", p.URL()+page.query, nil)
		if w.Code != 200 || strings.Count(w.Body.String(), `<article class="webmention">`) != page.count || !strings.Contains(w.Body.String(), page.link) {
			t.Fatalf("wrong mention page %q: %d %s", page.query, w.Code, w.Body.String())
		}
	}
}

func TestWebmentionQueueConcurrentAndRetry(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	p := seedPost(t, a, "target", true, false, time.Now().Add(-time.Hour))
	target := a.Config.BaseURL + p.URL()
	source := "https://source.example/p"
	id := mentionID("in", source, target)
	_, err := a.Store.DB.Exec(ctx, enqueueMentionSQL, id, "in", p.ID, source, target, true)
	if err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	client := wmClient(func(r *http.Request) *http.Response {
		hits.Add(1)
		time.Sleep(30 * time.Millisecond)
		return wmResponse(r, 503, "", nil)
	})
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := a.processMention(ctx, client)
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if hits.Load() != 1 {
		t.Fatal("concurrent delivery", hits.Load())
	}
	for i := 0; i < 4; i++ {
		_, _ = a.Store.DB.Exec(ctx, `UPDATE webmentions SET available_at=now() WHERE id=$1`, id)
		if _, err = a.processMention(ctx, client); err != nil {
			t.Fatal(err)
		}
	}
	rows, _ := a.Store.Mentions(ctx, "", false, 0)
	if rows[0].State != "failed" || rows[0].Attempts != 5 {
		t.Fatal(rows)
	}
	// A claim superseded by a new queue request must not publish stale content.
	_, _ = a.Store.DB.Exec(ctx, enqueueMentionSQL, id, "in", p.ID, source, target, true)
	client = wmClient(func(r *http.Request) *http.Response {
		_, err := a.Store.DB.Exec(ctx, enqueueMentionSQL, id, "in", p.ID, source, target, true)
		if err != nil {
			t.Error(err)
		}
		return wmResponse(r, 200, fmt.Sprintf(`<a href="%s">x</a>`, target), nil)
	})
	_, _ = a.processMention(ctx, client)
	rows, _ = a.Store.Mentions(ctx, "", false, 0)
	if rows[0].State != "queued" {
		t.Fatal("stale worker result", rows)
	}
}
func TestWebmentionOutgoingQueueAndPrivacy(t *testing.T) {
	a := testApp(t)
	a.Config.BaseURL = "http://blog.test"
	ctx := context.Background()
	p := seedPost(t, a, "out", true, false, time.Now().Add(-time.Hour))
	p.Body = "[External](https://target.example/post)"
	_ = a.Store.SavePost(ctx, &p)
	owner := newBrowser(a)
	owner.login(t, "owner")
	send := func() {
		t.Helper()
		w := owner.request("POST", "/godmode/posts/"+p.ID+"/webmentions/", nil)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	send()
	jobs, _ := a.Store.Mentions(ctx, "", false, 0)
	if len(jobs) != 1 {
		t.Fatal(jobs)
	}
	p.Body = "[Another](https://target.example/new)"
	_ = a.Store.SavePost(ctx, &p)
	send()
	jobs, _ = a.Store.Mentions(ctx, "", false, 0)
	if len(jobs) != 2 {
		t.Fatal("removed link not retained", jobs)
	}
	p.MembersOnly = true
	_ = a.Store.SavePost(ctx, &p)
	never := wmClient(func(r *http.Request) *http.Response { t.Error("private post sent"); return wmResponse(r, 200, "", nil) })
	for i := 0; i < 2; i++ {
		_, err := a.processMention(ctx, never)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range mustMentions(t, a) {
		if m.State != "cancelled" {
			t.Fatal(m)
		}
	}
	if w := owner.request("POST", "/godmode/posts/"+p.ID+"/webmentions/", nil); w.Code != 400 {
		t.Fatal("private queue accepted")
	}
}
func mustMentions(t *testing.T, a *Server) []Webmention {
	t.Helper()
	m, err := a.Store.Mentions(context.Background(), "", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
