package blog

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"
)

const visitorMetadata = `{"issuer":"https://auth.example/","authorization_endpoint":"https://auth.example/authorize","code_challenge_methods_supported":["S256"]}`

func TestVisitorDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name, me, link, body, metadata, want string
		fail                                 bool
	}{
		{name: "modern header", me: "person.example", link: `<https://auth.example/metadata>; rel="indieauth-metadata"`, metadata: visitorMetadata, want: "https://auth.example/authorize"},
		{name: "modern HTML", me: "https://person.example/", body: `<link rel="indieauth-metadata" href="https://auth.example/metadata">`, metadata: visitorMetadata, want: "https://auth.example/authorize"},
		{name: "legacy", me: "person.example", body: `<link rel=authorization_endpoint href=/auth>`, want: "https://person.example/auth"},
		{name: "header wins", me: "person.example", link: `</first>; rel="authorization_endpoint"`, body: `<link rel=authorization_endpoint href=/second>`, want: "https://person.example/first"},
		{name: "no endpoint", me: "person.example", body: `<a rel=authorization_endpoint href=/wrong></a><template><link rel=authorization_endpoint href=/wrong></template>`, fail: true},
		{name: "no downgrade on bad metadata", me: "person.example", link: `<https://auth.example/metadata>; rel="indieauth-metadata"`, body: `<link rel=authorization_endpoint href=/auth>`, metadata: `{}`, fail: true},
		{name: "wrong issuer", me: "person.example", link: `<https://auth.example/metadata>; rel="indieauth-metadata"`, metadata: strings.Replace(visitorMetadata, `"issuer":"https://auth.example/"`, `"issuer":"https://else.example/"`, 1), fail: true},
		{name: "no PKCE", me: "person.example", link: `<https://auth.example/metadata>; rel="indieauth-metadata"`, metadata: strings.Replace(visitorMetadata, "S256", "plain", 1), fail: true},
		{name: "HTTP endpoint", me: "person.example", body: `<link rel=authorization_endpoint href=http://auth.example/auth>`, fail: true},
		{name: "local endpoint", me: "person.example", body: `<link rel=authorization_endpoint href=https://localhost/auth>`, fail: true},
		{name: "IP profile", me: "https://8.8.8.8/", fail: true},
		{name: "port", me: "https://person.example:443/", fail: true},
		{name: "fragment", me: "https://person.example/#me", fail: true},
		{name: "credentials", me: "https://user:password@person.example/", fail: true},
		{name: "dot segments", me: "https://person.example/a/../", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := wmClient(func(r *http.Request) *http.Response {
				if r.URL.Host == "auth.example" {
					return wmResponse(r, 200, tc.metadata, nil)
				}
				return wmResponse(r, 200, tc.body, http.Header{"Link": {tc.link}})
			})
			p, err := discoverVisitor(context.Background(), client, tc.me)
			if (err != nil) != tc.fail || !tc.fail && p.Endpoint != tc.want {
				t.Fatalf("provider=%+v error=%v", p, err)
			}
		})
	}
	for _, raw := range []string{"https://evil.example/", "//evil.example/", "/%2fexample.com", "/\\evil.example/", "/\r\nfoo"} {
		if loginNext(raw) != "/profile/" {
			t.Fatalf("unsafe next: %q", raw)
		}
	}
	if loginNext("/notes/hello/?lang=ru#comments") != "/notes/hello/?lang=ru#comments" {
		t.Fatal("lost next")
	}
}

func visitorBegin(t *testing.T, b *browser, next string) url.Values {
	t.Helper()
	b.request("GET", "/login/", nil)
	w := b.request("POST", "/login/indieauth/", url.Values{"me": {"person.example"}, "next": {next}})
	if w.Code != 200 {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	doc, _ := html.Parse(strings.NewReader(w.Body.String()))
	target := ""
	walkHTML(doc, func(n *html.Node) {
		if attr(n, "id") == "indieauth-continue" {
			target = attr(n, "href")
		}
	})
	u, err := url.Parse(target)
	if err != nil || u.Host != "auth.example" {
		t.Fatalf("missing continuation: %s", w.Body.String())
	}
	q := u.Query()
	if q.Get("scope") != "profile" || q.Get("code_challenge_method") != "S256" || q.Get("redirect_uri") != b.app.Config.BaseURL+"/login/indieauth/callback/" {
		t.Fatalf("bad auth request %v", q)
	}
	return q
}
func visitorReturn(b *browser, q url.Values) *httptest.ResponseRecorder {
	return b.request("GET", "/login/indieauth/callback/?"+q.Encode(), nil)
}
func visitorReply(state string) url.Values {
	return url.Values{"state": {state}, "code": {"one-time-code"}, "iss": {"https://auth.example/"}}
}

func TestVisitorLoginAndComment(t *testing.T) {
	a := testApp(t)
	b := newBrowser(a)
	var challenge string
	exchanges := 0
	a.visitorHTTP = wmClient(func(r *http.Request) *http.Response {
		if r.Method == "POST" {
			exchanges++
			r.ParseForm()
			digest := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(digest[:]) != challenge || r.PostForm.Get("client_id") != a.Config.BaseURL+"/indieauth/client" {
				t.Error("bad PKCE/client binding")
			}
			return wmResponse(r, 200, `{"me":"https://person.example/","profile":{"name":"Visitor","email":"owner@example.test","url":"https://victim.example/","photo":"https://person.example/photo.jpg"}}`, nil)
		}
		if r.URL.Host == "auth.example" {
			return wmResponse(r, 200, visitorMetadata, nil)
		}
		return wmResponse(r, 200, `<link rel=indieauth-metadata href=https://auth.example/metadata>`, nil)
	})
	public := seedPost(t, a, "public", true, false, time.Now().Add(-time.Hour))
	private := seedPost(t, a, "private", true, true, time.Now().Add(-time.Hour))
	q := visitorBegin(t, b, public.URL()+"#comments")
	challenge = q.Get("code_challenge")
	callback := visitorReply(q.Get("state"))
	w := visitorReturn(b, callback)
	if w.Code != 303 || w.Header().Get("Location") != public.URL()+"#comments" {
		t.Fatalf("callback: %d %s", w.Code, w.Body.String())
	}
	u, err := a.Store.Session(context.Background(), b.cookies["session"].Value)
	if err != nil || u.Admin || u.Member || u.Email != "" || u.ProfileURL != "https://person.example/" || u.Name != "Visitor" {
		t.Fatalf("identity: %+v %v", u, err)
	}
	if w := visitorReturn(b, callback); w.Code != 400 || exchanges != 1 {
		t.Fatal("replayed flow")
	}
	profile := b.request("GET", "/profile/", nil)
	if !strings.Contains(profile.Body.String(), "https://person.example/") {
		t.Fatal("profile missing verified site")
	}
	if w := b.request("POST", "/comments/create/", url.Values{"post_id": {public.ID}, "body": {"Hello from my site"}}); w.Code != 303 {
		t.Fatalf("comment: %d %s", w.Code, w.Body.String())
	}
	page := b.request("GET", public.URL(), nil).Body.String()
	if !strings.Contains(page, `href="https://person.example/" rel="ugc nofollow">Visitor`) || !strings.Contains(page, "Hello from my site") {
		t.Fatal("comment/profile link missing")
	}
	for _, path := range []string{private.URL(), private.MarkdownURL(), "/rss/private/", "/godmode/", "/indieauth/auth"} {
		w := b.request("GET", path, nil)
		if strings.Contains(w.Body.String(), private.Body) {
			t.Fatalf("leak %s", path)
		}
		if path == "/rss/private/" && w.Code != 401 || path == "/godmode/" && w.Code != 403 {
			t.Fatalf("privilege escalation %s: %d", path, w.Code)
		}
	}
	if w := b.request("POST", "/comments/create/", url.Values{"post_id": {private.ID}, "body": {"should not work"}}); w.Code != 404 {
		t.Fatal("commented on locked post")
	}
	// Re-login must preserve locally edited display fields and identity.
	b.request("POST", "/profile/", url.Values{"name": {"Edited name"}, "avatar": {""}})
	b.request("POST", "/logout/", nil)
	q = visitorBegin(t, b, "//evil.example/")
	challenge = q.Get("code_challenge")
	w = visitorReturn(b, visitorReply(q.Get("state")))
	if w.Code != 303 || w.Header().Get("Location") != "/profile/" {
		t.Fatal("unsafe return URL")
	}
	again, err := a.Store.Session(context.Background(), b.cookies["session"].Value)
	if err != nil || again.ID != u.ID || again.Name != "Edited name" {
		t.Fatalf("identity changed: %+v %v", again, err)
	}
	owner, _ := a.Store.UserEmail(context.Background(), "owner@example.test")
	if owner.ID == again.ID || !owner.Admin {
		t.Fatal("owner merged")
	}
	// Ban is effective both for existing sessions and the next external login.
	a.Store.DB.Exec(context.Background(), `UPDATE users SET is_banned=true WHERE id=$1`, u.ID)
	if _, err = a.Store.Session(context.Background(), b.cookies["session"].Value); err == nil {
		t.Fatal("banned session alive")
	}
	q = visitorBegin(t, b, "/")
	challenge = q.Get("code_challenge")
	if w = visitorReturn(b, visitorReply(q.Get("state"))); w.Code != 403 {
		t.Fatal("banned login accepted")
	}
	member := newBrowser(a)
	member.login(t, "member")
	if !strings.Contains(member.request("GET", private.URL(), nil).Body.String(), private.Body) {
		t.Fatal("existing member lost access")
	}
}

func TestVisitorLoginRejection(t *testing.T) {
	for _, kind := range []string{"wrong browser", "wrong state", "expired", "wrong issuer", "missing issuer", "cancel", "forged identity", "redirect exchange", "duplicate code"} {
		t.Run(kind, func(t *testing.T) {
			a := testApp(t)
			b := newBrowser(a)
			exchanges := 0
			a.visitorHTTP = wmClient(func(r *http.Request) *http.Response {
				if r.Method == "POST" {
					exchanges++
					if kind == "redirect exchange" {
						return wmResponse(r, 307, "", http.Header{"Location": {"https://steal.example/"}})
					}
					return wmResponse(r, 200, `{"me":"https://victim.example/"}`, nil)
				}
				if r.URL.Host == "victim.example" {
					return wmResponse(r, 200, `<link rel=authorization_endpoint href=https://other.example/auth>`, nil)
				}
				if r.URL.Host == "auth.example" {
					return wmResponse(r, 200, visitorMetadata, nil)
				}
				if r.URL.Host == "steal.example" {
					t.Fatal("forwarded credentials")
				}
				return wmResponse(r, 200, `<link rel=indieauth-metadata href=https://auth.example/metadata>`, nil)
			})
			q := visitorBegin(t, b, "/")
			reply := visitorReply(q.Get("state"))
			switch kind {
			case "wrong browser":
				b = newBrowser(a)
			case "wrong state":
				reply.Set("state", randomToken())
			case "expired":
				a.Store.DB.Exec(context.Background(), `UPDATE indieauth_logins SET expires_at=now()-interval '1 second'`)
			case "wrong issuer":
				reply.Set("iss", "https://evil.example/")
			case "missing issuer":
				reply.Del("iss")
			case "cancel":
				reply.Del("code")
				reply.Set("error", "access_denied")
			case "duplicate code":
				reply.Add("code", "second")
			}
			w := visitorReturn(b, reply)
			if w.Code != 400 {
				t.Fatalf("accepted %s: %d", kind, w.Code)
			}
			var count int
			a.Store.DB.QueryRow(context.Background(), `SELECT count(*) FROM users WHERE profile_url<>''`).Scan(&count)
			if count != 0 {
				t.Fatal("created unverified account")
			}
			wantExchanges := 0
			if kind == "forged identity" || kind == "redirect exchange" {
				wantExchanges = 1
			}
			if exchanges != wantExchanges {
				t.Fatalf("exchanges %d want %d", exchanges, wantExchanges)
			}
		})
	}
}
func TestVisitorStartCSRF(t *testing.T) {
	a := testApp(t)
	w := apiRequest(a, "POST", "/login/indieauth/", "application/x-www-form-urlencoded", "me=https%3A%2F%2Fperson.example%2F", "")
	if w.Code != 403 {
		t.Fatalf("start without CSRF: %d", w.Code)
	}
}

func TestVisitorClientMetadata(t *testing.T) {
	a := testApp(t)
	w := newBrowser(a).request("GET", "/indieauth/client", nil)
	var m struct {
		ID        string   `json:"client_id"`
		Redirects []string `json:"redirect_uris"`
	}
	if json.Unmarshal(w.Body.Bytes(), &m) != nil || m.ID != a.Config.BaseURL+"/indieauth/client" || len(m.Redirects) != 1 || m.Redirects[0] != a.Config.BaseURL+"/login/indieauth/callback/" {
		t.Fatal("invalid client metadata")
	}
}

func TestVisitorLegacyLogin(t *testing.T) {
	a := testApp(t)
	b := newBrowser(a)
	a.visitorHTTP = wmClient(func(r *http.Request) *http.Response {
		if r.Method == "POST" {
			return wmResponse(r, 200, `{"me":"https://person.example/"}`, nil)
		}
		return wmResponse(r, 200, `<link rel=authorization_endpoint href=https://auth.example/authorize>`, nil)
	})
	q := visitorBegin(t, b, "/")
	reply := visitorReply(q.Get("state"))
	reply.Del("iss")
	if w := visitorReturn(b, reply); w.Code != 303 {
		t.Fatalf("legacy login: %d %s", w.Code, w.Body.String())
	}
	u, err := a.Store.Session(context.Background(), b.cookies["session"].Value)
	if err != nil || u.Name != "person.example" || u.Member || u.Admin {
		t.Fatal("legacy identity invalid")
	}
}
