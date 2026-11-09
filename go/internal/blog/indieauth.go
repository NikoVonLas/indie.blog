package blog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
)

var indieScopes = []string{"profile", "create", "update", "delete", "undelete", "media", "read"}
var scopeLabels = map[string]string{
	"profile": "Имя и фото автора", "create": "Создавать записи и загружать изображения",
	"update": "Редактировать записи", "delete": "Удалять записи в корзину",
	"undelete": "Восстанавливать записи", "media": "Загружать изображения",
	"read": "Читать исходники, включая черновики и закрытые записи",
}
var pkceVerifier = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)
var pkceChallenge = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
var indieHostname = regexp.MustCompile(`^[A-Za-z0-9.-]+$`)

type IndieRequest struct {
	ID, ClientID, RedirectURI, State, Challenge, Scope, Me string
	Scopes                                                 []IndieScope
}
type IndieScope struct{ Name, Label string }
type IndieToken struct {
	ID, UserID, ClientID, Scope, Me string
	CreatedAt, ExpiresAt            time.Time
}

func (t IndieToken) Has(scope string) bool { return slices.Contains(strings.Fields(t.Scope), scope) }
func (t IndieToken) Permissions() string {
	labels := []string{}
	for _, scope := range strings.Fields(t.Scope) {
		if label := scopeLabels[scope]; label != "" {
			labels = append(labels, label)
		}
	}
	return strings.Join(labels, "; ")
}

func protocolJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func protocolError(w http.ResponseWriter, status int, code, description string) {
	if status == 401 {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
	}
	if status == 403 {
		w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope"`)
	}
	protocolJSON(w, status, map[string]string{"error": code, "error_description": description})
}
func protocolPath(path string) bool {
	switch path {
	case "/indieauth/token", "/indieauth/revoke", "/indieauth/introspect", "/micropub", "/micropub/media":
		return true
	}
	return false
}
func protocolRequest(r *http.Request) bool {
	return protocolPath(r.URL.Path) || r.URL.Path == "/indieauth/auth" && r.Method == "POST"
}
func protocolCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	w.Header().Set("Access-Control-Expose-Headers", "Location, WWW-Authenticate")
}
func (a *Server) indieMetadata(w http.ResponseWriter, r *http.Request) {
	b := a.Config.BaseURL
	protocolJSON(w, 200, map[string]any{
		"issuer": b + "/", "authorization_endpoint": b + "/indieauth/auth", "token_endpoint": b + "/indieauth/token",
		"revocation_endpoint": b + "/indieauth/revoke", "revocation_endpoint_auth_methods_supported": []string{"none"},
		"introspection_endpoint": b + "/indieauth/introspect", "introspection_endpoint_auth_methods_supported": []string{"bearer"},
		"scopes_supported": indieScopes, "response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code"},
		"code_challenge_methods_supported": []string{"S256"}, "authorization_response_iss_parameter_supported": true,
	})
}
func indieURL(raw string) (*url.URL, error) {
	u, e := url.Parse(raw)
	if e != nil || len(raw) > 2048 || u.User != nil || u.Opaque != "" || u.Hostname() == "" || u.Fragment != "" || strings.Contains(raw, "#") || (u.Scheme != "https" && u.Scheme != "http") || strings.ContainsAny(raw, "\\\r\n\t ") {
		return nil, errors.New("invalid URL")
	}
	for _, p := range strings.Split(u.Path, "/") {
		if p == "." || p == ".." {
			return nil, errors.New("dot segment")
		}
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		if u.Hostname() != "127.0.0.1" && u.Hostname() != "::1" {
			return nil, errors.New("invalid client address")
		}
	} else if !indieHostname.MatchString(u.Hostname()) {
		return nil, errors.New("invalid client hostname")
	}
	u.Host = strings.ToLower(u.Host)
	if u.Path == "" {
		u.Path = "/"
	}
	return u, nil
}
func sameOrigin(a, b *url.URL) bool { return a.Scheme == b.Scheme && strings.EqualFold(a.Host, b.Host) }

// Same-origin callbacks need no metadata fetch. Cross-origin callbacks must be
// explicitly registered at the client URL, fetched with the public-address guard.
func validIndieRedirect(ctx context.Context, client *http.Client, id, target string) bool {
	cu, e := indieURL(id)
	if e != nil {
		return false
	}
	ru, e := indieURL(target)
	if e != nil {
		return false
	}
	if sameOrigin(cu, ru) {
		return true
	}
	d, e := fetchProtocolDocument(ctx, client, id, "application/json,application/oauth-client-metadata+json,text/html")
	if e != nil || d.Status != 200 {
		return false
	}
	ct, _, _ := mime.ParseMediaType(d.Header.Get("Content-Type"))
	if ct == "application/json" || ct == "application/oauth-client-metadata+json" {
		var m struct {
			ClientID  string   `json:"client_id"`
			ClientURI string   `json:"client_uri"`
			Redirects []string `json:"redirect_uris"`
		}
		if json.Unmarshal(d.Body, &m) != nil || m.ClientID != id || m.ClientURI == "" || !strings.HasPrefix(id, m.ClientURI) {
			return false
		}
		for _, raw := range m.Redirects {
			if u, e := d.URL.Parse(raw); e == nil && u.String() == target {
				return true
			}
		}
		return false
	}
	matches := func(raw string, base *url.URL) bool { u, e := base.Parse(raw); return e == nil && u.String() == target }
	for _, header := range d.Header.Values("Link") {
		for _, value := range splitLinkHeader(header, ',') {
			value = strings.TrimSpace(value)
			end := strings.Index(value, ">")
			if !strings.HasPrefix(value, "<") || end < 1 {
				continue
			}
			for _, param := range splitLinkHeader(value[end+1:], ';') {
				k, v, ok := strings.Cut(strings.TrimSpace(param), "=")
				if ok && strings.EqualFold(k, "rel") && relContains(strings.Trim(v, `"`), "redirect_uri") && matches(value[1:end], d.URL) {
					return true
				}
			}
		}
	}
	if !d.HTML {
		return false
	}
	doc, e := html.Parse(bytes.NewReader(d.Body))
	if e != nil {
		return false
	}
	base := documentBase(doc, d.URL)
	found := false
	visitLinks(doc, func(n *html.Node, raw string) {
		if n.Data == "link" && relContains(attr(n, "rel"), "redirect_uri") && matches(raw, base) {
			found = true
		}
	})
	return found
}
func indieNext(raw string) string {
	u, e := url.Parse(raw)
	if e == nil && !u.IsAbs() && u.Host == "" && u.Path == "/indieauth/auth" && !strings.ContainsAny(raw, "\\\r\n") {
		return u.String()
	}
	return "/profile/"
}
func (a *Server) indieAuthorize(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		a.indieRedeem(w, r)
		return
	}
	w.Header().Set("Referrer-Policy", "same-origin")
	if currentUser(r) == nil {
		http.Redirect(w, r, "/admin?next="+url.QueryEscape(r.URL.RequestURI()), 303)
		return
	}
	if !a.requireAdmin(w, r) {
		return
	}
	if a.limited(w, r, "indieauth-authorize", 30, time.Hour) {
		return
	}
	q := r.URL.Query()
	cu, e := indieURL(q.Get("client_id"))
	ru, re := indieURL(q.Get("redirect_uri"))
	if e != nil || re != nil || q.Get("response_type") != "code" || q.Get("state") == "" || len(q.Get("state")) > 2048 || q.Get("code_challenge_method") != "S256" || !pkceChallenge.MatchString(q.Get("code_challenge")) {
		protocolError(w, 400, "invalid_request", "Нужны client_id, redirect_uri, state и PKCE S256.")
		return
	}
	if !validIndieRedirect(r.Context(), publicPreviewClient(), cu.String(), ru.String()) {
		protocolError(w, 400, "invalid_request", "Адрес возврата не разрешён для этого приложения.")
		return
	}
	me := a.Config.BaseURL + "/"
	if q.Get("me") != "" {
		mu, e := url.Parse(q.Get("me"))
		if e != nil || strings.TrimSuffix(mu.String(), "/") != a.Config.BaseURL {
			protocolError(w, 400, "invalid_request", "Этот сервер подтверждает только адрес владельца блога.")
			return
		}
	}
	req := IndieRequest{ID: randomToken(), ClientID: cu.String(), RedirectURI: ru.String(), State: q.Get("state"), Challenge: q.Get("code_challenge"), Me: me}
	scopes := []string{}
	for _, s := range strings.Fields(q.Get("scope")) {
		if !slices.Contains(indieScopes, s) {
			protocolError(w, 400, "invalid_scope", "Неизвестное разрешение: "+shortText(s, 40))
			return
		}
		if !slices.Contains(scopes, s) {
			scopes = append(scopes, s)
			req.Scopes = append(req.Scopes, IndieScope{s, scopeLabels[s]})
		}
	}
	req.Scope = strings.Join(scopes, " ")
	data, _ := json.Marshal(req)
	session, _ := r.Cookie("session")
	// Expired transient data carries no usable credentials and can be discarded.
	_, e = a.Store.DB.Exec(r.Context(), `DELETE FROM indieauth_requests WHERE expires_at<now(); DELETE FROM indieauth_codes WHERE expires_at<now()`)
	if e != nil {
		a.fail(w, e)
		return
	}
	_, e = a.Store.DB.Exec(r.Context(), `INSERT INTO indieauth_requests(id,user_id,session_hash,payload,expires_at) VALUES($1,$2,$3,$4,now()+interval '10 minutes')`, hash(req.ID), currentUser(r).ID, hash(session.Value), data)
	if e != nil {
		a.fail(w, e)
		return
	}
	// Some browsers apply form-action to the final redirect after submitting.
	// Permit only the already-validated callback origin on this consent page.
	policy := w.Header().Get("Content-Security-Policy")
	w.Header().Set("Content-Security-Policy", strings.Replace(policy, "form-action 'self'", "form-action 'self' "+ru.Scheme+"://"+ru.Host, 1))
	a.render(w, r, page{Mode: "indie-consent", Title: a.text(r, "Подключить приложение"), IndieRequest: req})
}
func (a *Server) indieApprove(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	session, _ := r.Cookie("session")
	tx, e := a.Store.DB.Begin(r.Context())
	if e != nil {
		a.fail(w, e)
		return
	}
	defer tx.Rollback(r.Context())
	var data []byte
	e = tx.QueryRow(r.Context(), `DELETE FROM indieauth_requests WHERE id=$1 AND user_id=$2 AND session_hash=$3 AND expires_at>now() RETURNING payload`, hash(r.PostForm.Get("request")), currentUser(r).ID, hash(session.Value)).Scan(&data)
	if e != nil {
		protocolError(w, 400, "invalid_request", "Запрос истёк или уже обработан. Откройте подключение заново.")
		return
	}
	var req IndieRequest
	if json.Unmarshal(data, &req) != nil {
		protocolError(w, 400, "invalid_request", "Некорректный запрос")
		return
	}
	dest, _ := url.Parse(req.RedirectURI)
	q := dest.Query()
	q.Set("state", req.State)
	q.Set("iss", a.Config.BaseURL+"/")
	if r.PostForm.Get("decision") == "deny" {
		q.Set("error", "access_denied")
	} else if r.PostForm.Get("decision") == "allow" {
		selected := []string{}
		for _, s := range r.PostForm["scope"] {
			if !slices.Contains(strings.Fields(req.Scope), s) {
				protocolError(w, 400, "invalid_scope", "Разрешение не запрашивалось")
				return
			}
			if !slices.Contains(selected, s) {
				selected = append(selected, s)
			}
		}
		code := randomToken()
		q.Set("code", code)
		_, e = tx.Exec(r.Context(), `INSERT INTO indieauth_codes(code_hash,user_id,client_id,redirect_uri,challenge,scope,me,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,now()+interval '5 minutes')`, hash(code), currentUser(r).ID, req.ClientID, req.RedirectURI, req.Challenge, strings.Join(selected, " "), req.Me)
		if e != nil {
			a.fail(w, e)
			return
		}
	} else {
		protocolError(w, 400, "invalid_request", "Выберите действие")
		return
	}
	if e = tx.Commit(r.Context()); e != nil {
		a.fail(w, e)
		return
	}
	dest.RawQuery = q.Encode()
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, dest.String(), 303)
}
func formProtocol(r *http.Request) bool {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return ct == "application/x-www-form-urlencoded" && r.ParseForm() == nil
}
func (a *Server) indieRedeem(w http.ResponseWriter, r *http.Request) {
	if !formProtocol(r) {
		protocolError(w, 400, "invalid_request", "Expected form-encoded request")
		return
	}
	if r.PostForm.Get("action") == "revoke" {
		a.indieRevoke(w, r)
		return
	}
	if r.PostForm.Get("grant_type") != "authorization_code" {
		protocolError(w, 400, "unsupported_grant_type", "Use authorization_code")
		return
	}
	if a.limited(w, r, "indieauth-code", 60, time.Minute) {
		return
	}
	v := r.PostForm.Get("code_verifier")
	if !pkceVerifier.MatchString(v) {
		protocolError(w, 400, "invalid_grant", "Invalid code or verifier")
		return
	}
	digest := sha256.Sum256([]byte(v))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])
	tx, e := a.Store.DB.Begin(r.Context())
	if e != nil {
		a.fail(w, e)
		return
	}
	defer tx.Rollback(r.Context())
	var t IndieToken
	var expected, redirect string
	e = tx.QueryRow(r.Context(), `SELECT c.user_id,c.client_id,c.redirect_uri,c.challenge,c.scope,c.me FROM indieauth_codes c JOIN users u ON u.id=c.user_id WHERE c.code_hash=$1 AND c.expires_at>now() AND u.is_admin AND NOT u.is_banned FOR UPDATE OF c`, hash(r.PostForm.Get("code"))).Scan(&t.UserID, &t.ClientID, &redirect, &expected, &t.Scope, &t.Me)
	tokenEndpoint := r.URL.Path == "/indieauth/token"
	clientURL, clientErr := indieURL(r.PostForm.Get("client_id"))
	redirectURL, redirectErr := indieURL(r.PostForm.Get("redirect_uri"))
	if e != nil || clientErr != nil || redirectErr != nil || t.ClientID != clientURL.String() || redirect != redirectURL.String() || subtle.ConstantTimeCompare([]byte(expected), []byte(challenge)) != 1 || t.Me != a.Config.BaseURL+"/" || tokenEndpoint && t.Scope == "" {
		protocolError(w, 400, "invalid_grant", "Invalid code, client, redirect URI or verifier")
		return
	}
	_, e = tx.Exec(r.Context(), `DELETE FROM indieauth_codes WHERE code_hash=$1`, hash(r.PostForm.Get("code")))
	if e != nil {
		a.fail(w, e)
		return
	}
	result := map[string]any{"me": t.Me}
	if t.Has("profile") {
		card, _, e := a.Store.About(r.Context())
		if e != nil {
			a.fail(w, e)
			return
		}
		profile := map[string]string{"name": card.Name, "url": t.Me}
		if card.Avatar != "" {
			profile["photo"] = absoluteURL(a.Config.BaseURL, card.Avatar)
		}
		result["profile"] = profile
	}
	if tokenEndpoint {
		raw := randomToken()
		_, e = tx.Exec(r.Context(), `INSERT INTO indieauth_tokens(id,token_hash,user_id,client_id,scope,me,expires_at) VALUES($1,$2,$3,$4,$5,$6,now()+interval '30 days')`, randomToken(), hash(raw), t.UserID, t.ClientID, t.Scope, t.Me)
		if e != nil {
			a.fail(w, e)
			return
		}
		result["access_token"] = raw
		result["token_type"] = "Bearer"
		result["scope"] = t.Scope
		result["expires_in"] = 30 * 86400
	}
	if e = tx.Commit(r.Context()); e != nil {
		a.fail(w, e)
		return
	}
	protocolJSON(w, 200, result)
}
func bearer(r *http.Request) string {
	fields := strings.Fields(r.Header.Get("Authorization"))
	if len(fields) == 2 && strings.EqualFold(fields[0], "Bearer") {
		return fields[1]
	}
	return ""
}
func (a *Server) accessToken(ctx context.Context, raw string) (IndieToken, error) {
	var t IndieToken
	if len(raw) != 64 {
		return t, ErrNoRows
	}
	e := a.Store.DB.QueryRow(ctx, `SELECT t.id,t.user_id,t.client_id,t.scope,t.me,t.created_at,t.expires_at FROM indieauth_tokens t JOIN users u ON u.id=t.user_id WHERE token_hash=$1 AND expires_at>now() AND u.is_admin AND NOT u.is_banned AND me=$2`, hash(raw), a.Config.BaseURL+"/").Scan(&t.ID, &t.UserID, &t.ClientID, &t.Scope, &t.Me, &t.CreatedAt, &t.ExpiresAt)
	return t, e
}
func (a *Server) indieInspect(w http.ResponseWriter, r *http.Request) {
	caller, e := a.accessToken(r.Context(), bearer(r))
	if e != nil {
		protocolError(w, 401, "invalid_token", "Valid bearer token required")
		return
	}
	t := caller
	if r.Method == "POST" {
		if !formProtocol(r) {
			protocolError(w, 400, "invalid_request", "Expected form-encoded token")
			return
		}
		t, e = a.accessToken(r.Context(), r.PostForm.Get("token"))
		if e != nil || t.UserID != caller.UserID || t.ClientID != caller.ClientID {
			protocolJSON(w, 200, map[string]any{"active": false})
			return
		}
	}
	protocolJSON(w, 200, map[string]any{"active": true, "me": t.Me, "client_id": t.ClientID, "scope": t.Scope, "exp": t.ExpiresAt.Unix(), "iat": t.CreatedAt.Unix()})
}
func (a *Server) indieRevoke(w http.ResponseWriter, r *http.Request) {
	if !formProtocol(r) {
		protocolError(w, 400, "invalid_request", "Expected form-encoded token")
		return
	}
	_, e := a.Store.DB.Exec(r.Context(), `DELETE FROM indieauth_tokens WHERE token_hash=$1`, hash(r.PostForm.Get("token")))
	if e != nil {
		a.fail(w, e)
		return
	}
	w.WriteHeader(200)
}
func (a *Server) indieApps(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	if r.Method == "POST" {
		_, e := a.Store.DB.Exec(r.Context(), `DELETE FROM indieauth_tokens WHERE id=$1 AND user_id=$2`, r.PostForm.Get("id"), currentUser(r).ID)
		if e != nil {
			a.fail(w, e)
			return
		}
		http.Redirect(w, r, "/godmode/apps/", 303)
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if n < 1 || n > 100000 {
		n = 1
	}
	rows, e := a.Store.DB.Query(r.Context(), `SELECT id,client_id,scope,created_at,expires_at FROM indieauth_tokens WHERE user_id=$1 AND expires_at>now() ORDER BY created_at DESC,id LIMIT 51 OFFSET $2`, currentUser(r).ID, (n-1)*50)
	if e != nil {
		a.fail(w, e)
		return
	}
	defer rows.Close()
	ts := []IndieToken{}
	for rows.Next() {
		var t IndieToken
		if e = rows.Scan(&t.ID, &t.ClientID, &t.Scope, &t.CreatedAt, &t.ExpiresAt); e != nil {
			a.fail(w, e)
			return
		}
		ts = append(ts, t)
	}
	if e = rows.Err(); e != nil {
		a.fail(w, e)
		return
	}
	p := page{Mode: "indie-apps", Title: a.text(r, "Приложения"), IndieTokens: ts}
	if len(ts) > 50 {
		p.IndieTokens = ts[:50]
		p.Next = n + 1
	}
	if n > 1 {
		p.Previous = n - 1
	}
	a.render(w, r, p)
}

// Read exactly one JSON value; trailing payloads are rejected.
func decodeProtocolJSON(r io.Reader, v any) error {
	d := json.NewDecoder(r)
	if e := d.Decode(v); e != nil {
		return e
	}
	var extra any
	if e := d.Decode(&extra); e != io.EOF {
		return errors.New("unexpected trailing JSON")
	}
	return nil
}

func (a *Server) indieRoutes(handle func(string, http.HandlerFunc)) {
	handle("GET /.well-known/oauth-authorization-server", a.indieMetadata)
	handle("GET /indieauth/auth", a.indieAuthorize)
	handle("POST /indieauth/auth", a.indieAuthorize)
	handle("POST /indieauth/approve", a.indieApprove)
	handle("POST /indieauth/token", a.indieRedeem)
	handle("GET /indieauth/token", a.indieInspect)
	handle("POST /indieauth/introspect", a.indieInspect)
	handle("POST /indieauth/revoke", a.indieRevoke)
	handle("GET /godmode/apps/", a.indieApps)
	handle("POST /godmode/apps/", a.indieApps)
	handle("GET /micropub", a.micropub)
	handle("POST /micropub", a.micropub)
	handle("POST /micropub/media", a.micropubMedia)
}
func (a *Server) indieDiscovery() string {
	b := a.Config.BaseURL
	return `<` + b + `/.well-known/oauth-authorization-server>; rel="indieauth-metadata", <` + b + `/indieauth/auth>; rel="authorization_endpoint", <` + b + `/indieauth/token>; rel="token_endpoint", <` + b + `/micropub>; rel="micropub"`
}
