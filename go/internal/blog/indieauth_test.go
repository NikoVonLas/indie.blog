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
	"sync"
	"testing"
	"time"

	"golang.org/x/net/html"
)

func apiRequest(a *Server, method, path, ct, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://blog.test"+path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:4321"
	if ct != "" {
		r.Header.Set("Content-Type", ct)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w
}
func readObject(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var v map[string]any
	if e := json.Unmarshal(w.Body.Bytes(), &v); e != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	return v
}
func consentFormID(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	doc, e := html.Parse(strings.NewReader(w.Body.String()))
	if e != nil {
		t.Fatal(e)
	}
	id := ""
	walkHTML(doc, func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "input" && attr(n, "name") == "request" {
			id = attr(n, "value")
		}
	})
	if id == "" {
		t.Fatal("no consent form")
	}
	return id
}
func authParams(scope, verifier string) url.Values {
	h := sha256.Sum256([]byte(verifier))
	return url.Values{"response_type": {"code"}, "client_id": {"https://editor.example/"}, "redirect_uri": {"https://editor.example/callback?keep=1"}, "state": {"test state & +"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(h[:])}, "code_challenge_method": {"S256"}, "scope": {scope}, "me": {"http://blog.test/"}}
}
func authCode(t *testing.T, b *browser, scope string) (url.Values, string) {
	t.Helper()
	v := randomToken()
	q := authParams(scope, v)
	w := b.request("GET", "/indieauth/auth?"+q.Encode(), nil)
	id := consentFormID(t, w)
	w = b.request("POST", "/indieauth/approve", url.Values{"request": {id}, "decision": {"allow"}, "scope": strings.Fields(scope)})
	if w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	dest, e := url.Parse(w.Header().Get("Location"))
	if e != nil {
		t.Fatal(e)
	}
	if dest.Query().Get("state") != q.Get("state") || dest.Query().Get("keep") != "1" || dest.Query().Get("iss") != "http://blog.test/" {
		t.Fatal("redirect state lost")
	}
	return url.Values{"grant_type": {"authorization_code"}, "code": {dest.Query().Get("code")}, "client_id": {q.Get("client_id")}, "redirect_uri": {q.Get("redirect_uri")}, "code_verifier": {v}}, id
}
func issueTestToken(t *testing.T, a *Server, scope string) string {
	t.Helper()
	b := newBrowser(a)
	b.login(t, "owner")
	form, _ := authCode(t, b, scope)
	w := apiRequest(a, "POST", "/indieauth/token", "application/x-www-form-urlencoded", form.Encode(), "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	return readObject(t, w)["access_token"].(string)
}
func TestIndieAuthAuthorizationAndTokens(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a)
	owner.login(t, "owner")
	q := authParams("create update", randomToken())
	path := "/indieauth/auth?" + q.Encode()
	w := newBrowser(a).request("GET", path, nil)
	if w.Code != 303 || !strings.HasPrefix(w.Header().Get("Location"), "/admin?next=") {
		t.Fatal("login continuation missing")
	}
	member := newBrowser(a)
	member.login(t, "member")
	if w = member.request("GET", path, nil); w.Code != 403 {
		t.Fatal("member authorized client")
	}
	for _, key := range []string{"state", "code_challenge", "code_challenge_method"} {
		bad := authParams("create", randomToken())
		bad.Del(key)
		if w = owner.request("GET", "/indieauth/auth?"+bad.Encode(), nil); w.Code != 400 {
			t.Fatal("missing parameter accepted", key)
		}
	}
	for _, field := range []struct{ k, v string }{{"scope", "admin"}, {"me", "https://other.example/"}, {"redirect_uri", "javascript:alert(1)"}, {"client_id", "https://example.com/../evil"}} {
		bad := authParams("create", randomToken())
		bad.Set(field.k, field.v)
		if w = owner.request("GET", "/indieauth/auth?"+bad.Encode(), nil); w.Code != 400 {
			t.Fatal("unsafe request accepted", field)
		}
	}
	id := consentFormID(t, owner.request("GET", path, nil))
	if w = owner.request("POST", "/indieauth/approve", url.Values{"request": {id}, "decision": {"allow"}, "csrf": {"bad"}}); w.Code != 403 {
		t.Fatal("consent CSRF bypass")
	}
	other := newBrowser(a)
	other.login(t, "owner")
	if w = other.request("POST", "/indieauth/approve", url.Values{"request": {id}, "decision": {"allow"}}); w.Code != 400 {
		t.Fatal("consent not session-bound")
	}
	w = owner.request("POST", "/indieauth/approve", url.Values{"request": {id}, "decision": {"deny"}})
	if w.Code != 303 || !strings.Contains(w.Header().Get("Location"), "error=access_denied") {
		t.Fatal("denial failed")
	}
	if w = owner.request("POST", "/indieauth/approve", url.Values{"request": {id}, "decision": {"allow"}}); w.Code != 400 {
		t.Fatal("consent replay")
	}
	form, _ := authCode(t, owner, "create update profile")
	for _, key := range []string{"code_verifier", "client_id", "redirect_uri"} {
		bad := url.Values{}
		for k, v := range form {
			bad[k] = append([]string{}, v...)
		}
		bad.Set(key, "bad")
		if w = apiRequest(a, "POST", "/indieauth/token", "application/x-www-form-urlencoded", bad.Encode(), ""); w.Code != 400 {
			t.Fatal("invalid code binding", key)
		}
	}
	var wg sync.WaitGroup
	responses := make(chan *httptest.ResponseRecorder, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			responses <- apiRequest(a, "POST", "/indieauth/token", "application/x-www-form-urlencoded", form.Encode(), "")
		}()
	}
	wg.Wait()
	close(responses)
	token := ""
	success := 0
	for response := range responses {
		if response.Code == 200 {
			success++
			token = readObject(t, response)["access_token"].(string)
		} else if response.Code != 400 {
			t.Fatal(response.Code)
		}
	}
	if success != 1 {
		t.Fatal("code consumed more than once", success)
	}
	inspected := readObject(t, apiRequest(a, "GET", "/indieauth/token", "", "", token))
	if inspected["me"] != "http://blog.test/" || inspected["scope"] != "create update profile" {
		t.Fatal(inspected)
	}
	var stored string
	_ = a.Store.DB.QueryRow(context.Background(), `SELECT token_hash FROM indieauth_tokens`).Scan(&stored)
	if stored != hash(token) || stored == token {
		t.Fatal("raw token persisted")
	}
	if w = apiRequest(a, "POST", "/indieauth/introspect", "application/x-www-form-urlencoded", url.Values{"token": {token}}.Encode(), ""); w.Code != 401 {
		t.Fatal("unprotected introspection")
	}
	v := readObject(t, apiRequest(a, "POST", "/indieauth/introspect", "application/x-www-form-urlencoded", url.Values{"token": {token}}.Encode(), token))
	if v["active"] != true {
		t.Fatal(v)
	}
	w = apiRequest(a, "POST", "/indieauth/revoke", "application/x-www-form-urlencoded", url.Values{"token": {token}}.Encode(), "")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w = apiRequest(a, "GET", "/micropub?q=config", "", "", token); w.Code != 401 {
		t.Fatal("revoked token works")
	}
	form, _ = authCode(t, owner, "")
	if w = apiRequest(a, "POST", "/indieauth/token", "application/x-www-form-urlencoded", form.Encode(), ""); w.Code != 400 {
		t.Fatal("scope-less access token")
	}
	v = readObject(t, apiRequest(a, "POST", "/indieauth/auth", "application/x-www-form-urlencoded", form.Encode(), ""))
	if v["me"] != "http://blog.test/" || v["access_token"] != nil {
		t.Fatal("identity response", v)
	}
	// An expired code never becomes an access token.
	form, _ = authCode(t, owner, "create")
	_, _ = a.Store.DB.Exec(context.Background(), `UPDATE indieauth_codes SET expires_at=now()-interval '1 second'`)
	if w = apiRequest(a, "POST", "/indieauth/token", "application/x-www-form-urlencoded", form.Encode(), ""); w.Code != 400 {
		t.Fatal("expired code accepted")
	}
	if indieNext("//evil.example/") != "/profile/" || indieNext("https://evil.example/") != "/profile/" || indieNext(path) != path {
		t.Fatal("login open redirect")
	}
}
func TestIndieAuthRedirectMetadata(t *testing.T) {
	ctx := context.Background()
	hits := 0
	client := wmClient(func(r *http.Request) *http.Response {
		hits++
		return wmResponse(r, 200, `{"client_id":"https://client.example/meta","client_uri":"https://client.example/","redirect_uris":["https://other.example/callback"]}`, http.Header{"Content-Type": {"application/json"}})
	})
	if !validIndieRedirect(ctx, client, "https://client.example/meta", "https://other.example/callback") {
		t.Fatal("registered JSON callback rejected")
	}
	if validIndieRedirect(ctx, client, "https://client.example/meta", "https://other.example/callback/extra") {
		t.Fatal("callback prefix accepted")
	}
	before := hits
	if !validIndieRedirect(ctx, client, "http://127.0.0.1:8082/", "http://127.0.0.1:8082/callback") || hits != before {
		t.Fatal("loopback same-origin client fetched")
	}
	if validIndieRedirect(ctx, client, "http://127.0.0.1:8082/", "https://other.example/callback") || hits != before {
		t.Fatal("loopback client fetched")
	}
	client = wmClient(func(r *http.Request) *http.Response {
		return wmResponse(r, 200, `<link rel="redirect_uri" href="https://other.example/callback"><script>'<link rel=redirect_uri href=https://evil.example/>'</script>`, nil)
	})
	if !validIndieRedirect(ctx, client, "https://client.example/", "https://other.example/callback") || validIndieRedirect(ctx, client, "https://client.example/", "https://evil.example/") {
		t.Fatal("HTML callback validation")
	}
}
func TestIndieAuthRevocationExpiryAndDiscovery(t *testing.T) {
	a := testApp(t)
	raw := issueTestToken(t, a, "create")
	owner := newBrowser(a)
	owner.login(t, "owner")
	w := owner.request("GET", "/godmode/apps/", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "https://editor.example/") || strings.Contains(w.Body.String(), raw) {
		t.Fatal("apps list missing or token leaked")
	}
	var id string
	_ = a.Store.DB.QueryRow(context.Background(), `SELECT id FROM indieauth_tokens WHERE token_hash=$1`, hash(raw)).Scan(&id)
	if w = owner.request("POST", "/godmode/apps/", url.Values{"id": {id}, "csrf": {"bad"}}); w.Code != 403 {
		t.Fatal("revocation CSRF bypass")
	}
	owner.request("POST", "/godmode/apps/", url.Values{"id": {id}})
	if _, e := a.accessToken(context.Background(), raw); e == nil {
		t.Fatal("owner revocation failed")
	}
	raw = issueTestToken(t, a, "create")
	_, _ = a.Store.DB.Exec(context.Background(), `UPDATE indieauth_tokens SET expires_at=$1`, time.Now().Add(-time.Second))
	if _, e := a.accessToken(context.Background(), raw); e == nil {
		t.Fatal("expired token accepted")
	}
	raw = issueTestToken(t, a, "create")
	_, _ = a.Store.DB.Exec(context.Background(), `UPDATE users SET is_banned=true WHERE is_admin`)
	if _, e := a.accessToken(context.Background(), raw); e == nil {
		t.Fatal("banned owner token accepted")
	}
	w = newBrowser(a).request("GET", "/", nil)
	for _, rel := range []string{"indieauth-metadata", "authorization_endpoint", "token_endpoint", "micropub"} {
		if !strings.Contains(w.Header().Get("Link"), rel) || !strings.Contains(w.Body.String(), `rel="`+rel+`"`) {
			t.Fatal("discovery", rel)
		}
	}
	metadata := readObject(t, apiRequest(a, "GET", "/.well-known/oauth-authorization-server", "", "", ""))
	if metadata["issuer"] != "http://blog.test/" {
		t.Fatal(metadata)
	}
	w = apiRequest(a, "OPTIONS", "/micropub", "", "", "")
	if w.Code != 204 || w.Header().Get("Access-Control-Allow-Origin") != "*" || w.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Fatal("CORS")
	}
}

func TestIndieAuthConsentCannotExpandPermissions(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a)
	owner.login(t, "owner")
	q := authParams("create update", randomToken())
	w := owner.request("GET", "/indieauth/auth?"+q.Encode(), nil)
	if w.Header().Get("Referrer-Policy") != "same-origin" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "form-action 'self' https://editor.example") {
		t.Fatal("consent must retain same-origin CSRF metadata and allow validated callback")
	}
	id := consentFormID(t, w)
	w = owner.request("POST", "/indieauth/approve", url.Values{"request": {id}, "decision": {"allow"}, "scope": {"delete"}})
	if w.Code != 400 {
		t.Fatal("client expanded requested scopes")
	}
	w = owner.request("POST", "/indieauth/approve", url.Values{"request": {id}, "decision": {"allow"}, "scope": {"create"}})
	if w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	dest, _ := url.Parse(w.Header().Get("Location"))
	var scope string
	if e := a.Store.DB.QueryRow(context.Background(), `SELECT scope FROM indieauth_codes WHERE code_hash=$1`, hash(dest.Query().Get("code"))).Scan(&scope); e != nil || scope != "create" {
		t.Fatal("consent was not narrowed", scope, e)
	}
}
