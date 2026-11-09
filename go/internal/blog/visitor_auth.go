package blog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// Visitor identities are verified profile URLs, never email addresses or names.
// These accounts can comment but are neither members nor administrators.
type visitorProvider struct{ Me, Endpoint, Issuer string }
type visitorLogin struct {
	Provider visitorProvider
	Next     string
}

func (a *Server) visitorRoutes(handle func(string, http.HandlerFunc)) {
	handle("POST /login/indieauth/", a.visitorStart)
	handle("GET /login/indieauth/callback/", a.visitorCallback)
	handle("GET /indieauth/client", a.visitorClientMetadata)
}
func (a *Server) visitorClientMetadata(w http.ResponseWriter, r *http.Request) {
	protocolJSON(w, 200, map[string]any{
		"client_id": a.Config.BaseURL + "/indieauth/client", "client_name": a.Config.Title,
		"client_uri": a.Config.BaseURL + "/", "redirect_uris": []string{a.Config.BaseURL + "/login/indieauth/callback/"},
	})
}
func loginNext(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 4096 || u.IsAbs() || u.Host != "" || !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") || strings.ContainsAny(u.Path, "\\\r\n\x00") || strings.ContainsAny(raw, "\\\r\n") {
		return "/profile/"
	}
	return u.String()
}
func visitorProfile(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := indieURL(raw)
	if err != nil {
		return nil, errors.New("invalid profile URL")
	}
	if len(raw) > 2048 || u.Port() != "" || net.ParseIP(u.Hostname()) != nil {
		return nil, errors.New("invalid profile URL")
	}
	if _, err = publicURL(u.String()); err != nil {
		return nil, err
	}
	return u, nil
}
func visitorEndpoint(raw string) (*url.URL, error) {
	u, err := indieURL(raw)
	if err != nil || u.Scheme != "https" {
		return nil, errors.New("authentication endpoint must use HTTPS")
	}
	return publicURL(u.String())
}

// HTTP Link takes precedence over HTML, and the first matching link wins.
func visitorRel(d mentionDocument, rel string) (string, error) {
	resolve := func(raw string, base *url.URL) (string, error) {
		u, err := base.Parse(raw)
		if err != nil {
			return "", err
		}
		v, err := visitorEndpoint(u.String())
		if err != nil {
			return "", err
		}
		return v.String(), nil
	}
	for _, header := range d.Header.Values("Link") {
		for _, value := range splitLinkHeader(header, ',') {
			value = strings.TrimSpace(value)
			end := strings.Index(value, ">")
			if !strings.HasPrefix(value, "<") || end < 1 {
				continue
			}
			for _, param := range splitLinkHeader(value[end+1:], ';') {
				k, v, ok := strings.Cut(strings.TrimSpace(param), "=")
				if ok && strings.EqualFold(k, "rel") && relContains(strings.Trim(v, `"`), rel) {
					return resolve(value[1:end], d.URL)
				}
			}
		}
	}
	if !d.HTML {
		return "", nil
	}
	doc, err := html.Parse(bytes.NewReader(d.Body))
	if err != nil {
		return "", err
	}
	base := documentBase(doc, d.URL)
	raw := ""
	found := false
	visitLinks(doc, func(n *html.Node, value string) {
		if !found && n.Data == "link" && relContains(attr(n, "rel"), rel) {
			found = true
			raw = value
		}
	})
	if found {
		return resolve(raw, base)
	}
	return "", nil
}
func discoverVisitor(ctx context.Context, client *http.Client, raw string) (visitorProvider, error) {
	u, err := visitorProfile(raw)
	if err != nil {
		return visitorProvider{}, err
	}
	d, err := fetchMentionDocument(ctx, client, u.String())
	if err != nil || d.Status != 200 {
		return visitorProvider{}, errors.New("profile unavailable")
	}
	me, err := visitorProfile(d.URL.String())
	if err != nil {
		return visitorProvider{}, err
	}
	p := visitorProvider{Me: me.String()}
	metadata, err := visitorRel(d, "indieauth-metadata")
	if err != nil {
		return p, err
	}
	if metadata == "" {
		p.Endpoint, err = visitorRel(d, "authorization_endpoint")
		if err != nil || p.Endpoint == "" {
			return p, errors.New("no IndieAuth endpoint")
		}
		return p, nil
	}
	m, err := fetchProtocolDocument(ctx, client, metadata, "application/json")
	if err != nil || m.Status != 200 {
		return p, errors.New("metadata unavailable")
	}
	var data struct {
		Issuer   string   `json:"issuer"`
		Endpoint string   `json:"authorization_endpoint"`
		Methods  []string `json:"code_challenge_methods_supported"`
	}
	if decodeProtocolJSON(bytes.NewReader(m.Body), &data) != nil {
		return p, errors.New("invalid metadata")
	}
	issuer, err := visitorEndpoint(data.Issuer)
	if err != nil || issuer.RawQuery != "" || !strings.HasPrefix(metadata, data.Issuer) || !strings.HasPrefix(m.URL.String(), data.Issuer) {
		return p, errors.New("invalid issuer")
	}
	endpoint, err := visitorEndpoint(data.Endpoint)
	if err != nil {
		return p, err
	}
	pkce := false
	for _, method := range data.Methods {
		if method == "S256" {
			pkce = true
		}
	}
	if !pkce {
		return p, errors.New("S256 is required")
	}
	p.Endpoint = endpoint.String()
	p.Issuer = data.Issuer
	return p, nil
}
func (a *Server) visitorError(w http.ResponseWriter, r *http.Request, message string) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	a.render(w, r, page{Status: 400, Mode: "login", Title: a.text(r, "Войти"), Message: message, LoginNext: loginNext(r.PostForm.Get("next"))})
}
func (a *Server) visitorVerifier(state string) string {
	// Derive the PKCE secret without persisting it or exposing it to the browser.
	return a.sign("visitor-pkce:" + state)
}
func (a *Server) visitorStart(w http.ResponseWriter, r *http.Request) {
	if a.limited(w, r, "visitor-login", 10, 15*time.Minute) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	provider, err := discoverVisitor(ctx, a.visitorHTTP, r.PostForm.Get("me"))
	if err != nil {
		a.visitorError(w, r, "Не удалось найти IndieAuth на этом сайте. Проверьте адрес и настройку сервиса входа.")
		return
	}
	state, nonce := randomToken(), randomToken()
	payload, _ := json.Marshal(visitorLogin{Provider: provider, Next: loginNext(r.PostForm.Get("next"))})
	if _, err = a.Store.DB.Exec(ctx, `DELETE FROM indieauth_logins WHERE expires_at<=now()`); err != nil {
		a.fail(w, err)
		return
	}
	_, err = a.Store.DB.Exec(ctx, `INSERT INTO indieauth_logins(state_hash,browser_hash,payload,expires_at) VALUES($1,$2,$3,now()+interval '10 minutes')`, hash(state), hash(nonce), payload)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.cookie(w, "indieauth_login", nonce, 600)
	endpoint, _ := url.Parse(provider.Endpoint)
	q := endpoint.Query()
	challenge := sha256.Sum256([]byte(a.visitorVerifier(state)))
	for k, v := range map[string]string{"response_type": "code", "client_id": a.Config.BaseURL + "/indieauth/client", "redirect_uri": a.Config.BaseURL + "/login/indieauth/callback/", "state": state, "me": provider.Me, "scope": "profile", "code_challenge": base64.RawURLEncoding.EncodeToString(challenge[:]), "code_challenge_method": "S256"} {
		q.Set(k, v)
	}
	endpoint.RawQuery = q.Encode()
	w.Header().Set("Referrer-Policy", "no-referrer")
	a.render(w, r, page{Mode: "login-redirect", Title: a.text(r, "Подтвердите вход"), LoginRedirect: endpoint.String()})
}
func (a *Server) visitorCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	if a.limited(w, r, "visitor-callback", 30, 15*time.Minute) {
		return
	}
	q := r.URL.Query()
	state := q.Get("state")
	nonce, err := r.Cookie("indieauth_login")
	if err != nil || len(state) != 64 || len(q["state"]) != 1 {
		a.visitorError(w, r, "Попытка входа устарела. Начните заново.")
		return
	}
	var payload []byte
	err = a.Store.DB.QueryRow(r.Context(), `DELETE FROM indieauth_logins WHERE state_hash=$1 AND browser_hash=$2 AND expires_at>now() RETURNING payload`, hash(state), hash(nonce.Value)).Scan(&payload)
	if err != nil {
		if !errors.Is(err, ErrNoRows) {
			a.fail(w, err)
			return
		}
		a.visitorError(w, r, "Попытка входа устарела или открыта в другом браузере. Начните заново.")
		return
	}
	a.cookie(w, "indieauth_login", "", -1)
	var flow visitorLogin
	if json.Unmarshal(payload, &flow) != nil {
		a.visitorError(w, r, "Не удалось восстановить попытку входа.")
		return
	}
	if q.Get("error") != "" {
		a.visitorError(w, r, "Вход отменён на вашем сайте. Можно попробовать ещё раз.")
		return
	}
	if len(q["code"]) != 1 || len(q.Get("code")) > 4096 || q.Get("code") == "" || len(q["iss"]) > 1 || flow.Provider.Issuer != "" && q.Get("iss") != flow.Provider.Issuer {
		a.visitorError(w, r, "Ответ сервиса входа не прошёл проверку. Начните заново.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	form := url.Values{"grant_type": {"authorization_code"}, "code": {q.Get("code")}, "client_id": {a.Config.BaseURL + "/indieauth/client"}, "redirect_uri": {a.Config.BaseURL + "/login/indieauth/callback/"}, "code_verifier": {a.visitorVerifier(state)}}
	req, err := http.NewRequestWithContext(ctx, "POST", flow.Provider.Endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		a.fail(w, err)
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// Never forward the authorization code or verifier through an HTTP redirect.
	client := *a.visitorHTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		a.visitorError(w, r, "Сервис входа не ответил. Попробуйте ещё раз.")
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	var identity struct {
		Me      string `json:"me"`
		Profile struct {
			Name  string `json:"name"`
			Photo string `json:"photo"`
		} `json:"profile"`
	}
	if err != nil || resp.StatusCode != 200 || len(body) > 64<<10 || decodeProtocolJSON(bytes.NewReader(body), &identity) != nil || (!strings.HasPrefix(identity.Me, "https://") && !strings.HasPrefix(identity.Me, "http://")) {
		a.visitorError(w, r, "Сервис не подтвердил ваш сайт. Попробуйте ещё раз.")
		return
	}
	// An endpoint must not be able to assert another person's site as its identity.
	verified, err := discoverVisitor(ctx, a.visitorHTTP, identity.Me)
	me, meErr := visitorProfile(identity.Me)
	if err != nil || meErr != nil || verified.Endpoint != flow.Provider.Endpoint || verified.Issuer != flow.Provider.Issuer {
		a.visitorError(w, r, "Не удалось подтвердить, что сервис входа принадлежит вашему сайту.")
		return
	}
	name := shortText(identity.Profile.Name, 128)
	if name == "" {
		name = me.Hostname()
	}
	photo := ""
	if u, e := publicURL(identity.Profile.Photo); e == nil {
		photo = u.String()
	}
	// Do not merge by supplied email/name; do not overwrite edited profile fields.
	u, err := scanUser(a.Store.DB.QueryRow(ctx, `INSERT INTO users(id,email,name,password_hash,avatar,is_member,profile_url) VALUES($1,NULL,$2,'',$3,false,$4) ON CONFLICT(profile_url) WHERE profile_url<>'' DO UPDATE SET profile_url=EXCLUDED.profile_url RETURNING `+userCols, randomToken(), name, photo, me.String()))
	if err != nil {
		a.fail(w, err)
		return
	}
	if u.Admin {
		http.Error(w, a.text(r, "Администратор входит через /admin с Passkey"), 403)
		return
	}
	if u.Banned {
		http.Error(w, a.text(r, "Этот профиль заблокирован"), 403)
		return
	}
	token := randomToken()
	_, err = a.Store.DB.Exec(ctx, `INSERT INTO sessions(token_hash,user_id,expires_at) VALUES($1,$2,now()+interval '30 days')`, hash(token), u.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.cookie(w, "session", token, 30*86400)
	a.cookie(w, "csrf", "", -1)
	http.Redirect(w, r, loginNext(flow.Next), 303)
}
