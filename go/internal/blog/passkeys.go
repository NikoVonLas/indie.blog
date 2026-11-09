package blog

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

type passkeyUser struct {
	*User
	Credentials []webauthn.Credential
}

func (u passkeyUser) WebAuthnID() []byte { return []byte(u.ID) }
func (u passkeyUser) WebAuthnName() string {
	if u.Email != "" {
		return u.Email
	}
	return u.Name
}
func (u passkeyUser) WebAuthnDisplayName() string                { return u.Name }
func (u passkeyUser) WebAuthnCredentials() []webauthn.Credential { return u.Credentials }

type adminKey struct {
	ID, Label string
	CreatedAt time.Time
}
type passkeyFlow struct {
	First                                       bool
	Session                                     webauthn.SessionData
	UserID, GrantHash, SessionHash, Label, Next string
}

func (a *Server) initPasskeys() error {
	u, _ := url.Parse(a.Config.BaseURL)
	var err error
	a.passkeys, err = webauthn.New(&webauthn.Config{
		RPID: u.Hostname(), RPDisplayName: a.Config.Title, RPOrigins: []string{a.Config.BaseURL},
		AuthenticatorSelection: protocol.AuthenticatorSelection{ResidentKey: protocol.ResidentKeyRequirementRequired, UserVerification: protocol.VerificationRequired},
		AttestationPreference:  protocol.PreferNoAttestation,
	})
	return err
}
func (a *Server) passkeyRoutes(handle func(string, http.HandlerFunc)) {
	handle("GET /admin", a.adminLoginPage)
	handle("GET /admin/", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/admin", 303) })
	handle("GET /admin/setup", a.adminSetupPage)
	handle("GET /admin/keys", a.adminKeys)
	handle("POST /admin/keys/{id}/delete", a.deleteAdminKey)
	handle("POST /admin/passkey/login/begin", a.passkeyLoginBegin)
	handle("POST /admin/passkey/login/finish", a.passkeyLoginFinish)
	handle("POST /admin/passkey/register/begin", a.passkeyRegisterBegin)
	handle("POST /admin/passkey/register/finish", a.passkeyRegisterFinish)
}
func adminNext(raw string) string {
	next := loginNext(raw)
	if next == "/profile/" || strings.HasPrefix(next, "/admin?") || next == "/admin" {
		return "/godmode/"
	}
	return next
}
func (a *Server) adminLoginPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	if u := currentUser(r); u != nil && u.Admin && r.URL.Query().Get("reauth") != "1" {
		http.Redirect(w, r, adminNext(r.URL.Query().Get("next")), 303)
		return
	}
	var hasKeys bool
	if err := a.Store.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM admin_passkeys)`).Scan(&hasKeys); err != nil {
		a.fail(w, err)
		return
	}
	a.render(w, r, page{Mode: "admin-login", Title: a.text(r, "Вход администратора"), FirstPasskey: !hasKeys, LoginNext: adminNext(r.URL.Query().Get("next"))})
}
func (a *Server) adminSetupPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	a.render(w, r, page{Mode: "admin-setup", Title: a.text(r, "Регистрация ключа")})
}
func (a *Server) adminKeys(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	rows, err := a.Store.DB.Query(r.Context(), `SELECT id,label,created_at FROM admin_passkeys WHERE user_id=$1 ORDER BY created_at`, currentUser(r).ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	defer rows.Close()
	keys := []adminKey{}
	for rows.Next() {
		var k adminKey
		if err = rows.Scan(&k.ID, &k.Label, &k.CreatedAt); err != nil {
			a.fail(w, err)
			return
		}
		keys = append(keys, k)
	}
	if err = rows.Err(); err != nil {
		a.fail(w, err)
		return
	}
	a.render(w, r, page{Mode: "admin-keys", Title: a.text(r, "Ключи администратора"), PasskeyKeys: keys})
}
func passkeyError(w http.ResponseWriter, status int, message string) {
	protocolJSON(w, status, map[string]string{"error": message})
}
func (a *Server) savePasskeyFlow(w http.ResponseWriter, r *http.Request, kind string, flow passkeyFlow) error {
	nonce := randomToken()
	payload, err := json.Marshal(flow)
	if err != nil {
		return err
	}
	_, err = a.Store.DB.Exec(r.Context(), `DELETE FROM passkey_ceremonies WHERE expires_at<=now()`)
	if err != nil {
		return err
	}
	_, err = a.Store.DB.Exec(r.Context(), `INSERT INTO passkey_ceremonies(browser_hash,kind,payload,expires_at) VALUES($1,$2,$3,now()+interval '5 minutes')`, hash(nonce), kind, payload)
	if err == nil {
		a.cookie(w, "passkey_flow", nonce, 300)
	}
	return err
}
func (a *Server) takePasskeyFlow(w http.ResponseWriter, r *http.Request, kind string) (passkeyFlow, error) {
	var flow passkeyFlow
	cookie, err := r.Cookie("passkey_flow")
	if err != nil {
		return flow, err
	}
	var payload []byte
	err = a.Store.DB.QueryRow(r.Context(), `DELETE FROM passkey_ceremonies WHERE browser_hash=$1 AND kind=$2 AND expires_at>now() RETURNING payload`, hash(cookie.Value), kind).Scan(&payload)
	if err != nil {
		return flow, err
	}
	a.cookie(w, "passkey_flow", "", -1)
	err = json.Unmarshal(payload, &flow)
	return flow, err
}
func (a *Server) passkeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	if a.limited(w, r, "passkey-login", 20, 15*time.Minute) {
		return
	}
	var input struct {
		Next string `json:"next"`
	}
	if decodeProtocolJSON(r.Body, &input) != nil {
		passkeyError(w, 400, "Неверный запрос")
		return
	}
	options, session, err := a.passkeys.BeginDiscoverableLogin()
	if err != nil {
		a.fail(w, err)
		return
	}
	if err = a.savePasskeyFlow(w, r, "login", passkeyFlow{Session: *session, Next: adminNext(input.Next)}); err != nil {
		a.fail(w, err)
		return
	}
	protocolJSON(w, 200, options)
}
func loadPasskeyUser(ctx context.Context, tx Transaction, id string) (passkeyUser, error) {
	u, err := scanUser(tx.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id=$1 AND is_admin AND NOT is_banned FOR UPDATE`, id))
	if err != nil {
		return passkeyUser{}, err
	}
	rows, err := tx.Query(ctx, `SELECT credential FROM admin_passkeys WHERE user_id=$1 ORDER BY id`, id)
	if err != nil {
		return passkeyUser{}, err
	}
	defer rows.Close()
	result := passkeyUser{User: u}
	for rows.Next() {
		var raw []byte
		var c webauthn.Credential
		if err = rows.Scan(&raw); err != nil {
			return result, err
		}
		if err = json.Unmarshal(raw, &c); err != nil {
			return result, err
		}
		result.Credentials = append(result.Credentials, c)
	}
	return result, rows.Err()
}
func (a *Server) passkeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	if a.limited(w, r, "passkey-finish", 30, 15*time.Minute) {
		return
	}
	flow, err := a.takePasskeyFlow(w, r, "login")
	if err != nil {
		passkeyError(w, 400, "Попытка входа устарела. Нажмите «Войти с ключом» ещё раз.")
		return
	}
	tx, err := a.Store.DB.Begin(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		u, err := loadPasskeyUser(r.Context(), tx, string(userHandle))
		if err != nil {
			return nil, err
		}
		return u, nil
	}
	user, credential, err := a.passkeys.FinishPasskeyLogin(handler, flow.Session, r)
	if err != nil || credential.Authenticator.CloneWarning {
		passkeyError(w, 401, "Ключ не прошёл проверку. Попробуйте ещё раз.")
		return
	}
	keyID := base64.RawURLEncoding.EncodeToString(credential.ID)
	raw, _ := json.Marshal(credential)
	result, err := tx.Exec(r.Context(), `UPDATE admin_passkeys SET credential=$1,last_used_at=now() WHERE id=$2 AND user_id=$3`, raw, keyID, string(user.WebAuthnID()))
	if err != nil || result.RowsAffected() != 1 {
		passkeyError(w, 401, "Ключ больше недоступен")
		return
	}
	token := randomToken()
	_, err = tx.Exec(r.Context(), `INSERT INTO sessions(token_hash,user_id,passkey_id,expires_at) VALUES($1,$2,$3,now()+interval '12 hours')`, hash(token), string(user.WebAuthnID()), keyID)
	if err != nil {
		a.fail(w, err)
		return
	}
	if old, e := r.Cookie("session"); e == nil {
		if _, err = tx.Exec(r.Context(), `DELETE FROM sessions WHERE token_hash=$1`, hash(old.Value)); err != nil {
			a.fail(w, err)
			return
		}
	}
	if err = tx.Commit(r.Context()); err != nil {
		a.fail(w, err)
		return
	}
	a.cookie(w, "session", token, 12*3600)
	a.cookie(w, "csrf", "", -1)
	protocolJSON(w, 200, map[string]string{"redirect": adminNext(flow.Next)})
}
func freshAdminSession(ctx context.Context, tx Transaction, userID, sessionHash string) bool {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sessions s JOIN admin_passkeys k ON k.id=s.passkey_id AND k.user_id=s.user_id WHERE s.token_hash=$1 AND s.user_id=$2 AND s.expires_at>now() AND s.created_at>now()-interval '10 minutes')`, sessionHash, userID).Scan(&ok)
	return err == nil && ok
}
func (a *Server) passkeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	if a.limited(w, r, "passkey-register", 10, 15*time.Minute) {
		return
	}
	var input struct {
		First bool   `json:"first"`
		Token string `json:"token"`
		Label string `json:"label"`
	}
	if decodeProtocolJSON(r.Body, &input) != nil || len(input.Token) > 128 {
		passkeyError(w, 400, "Неверный запрос")
		return
	}
	flow := passkeyFlow{Label: shortText(input.Label, 80)}
	if flow.Label == "" {
		flow.Label = "Passkey"
	}
	tx, err := a.Store.DB.Begin(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	if input.Token != "" {
		flow.GrantHash = hash(input.Token)
		err = tx.QueryRow(r.Context(), `SELECT user_id FROM admin_setup_grants WHERE token_hash=$1 AND expires_at>now()`, flow.GrantHash).Scan(&flow.UserID)
		if err != nil {
			passkeyError(w, 403, "Одноразовая ссылка недействительна или истекла. Создайте новую через скрипт настройки.")
			return
		}
	} else if input.First {
		if !a.localFirstKeyRequest(r) {
			passkeyError(w, 403, "Для первичной настройки откройте блог на localhost или используйте защищённую ссылку настройки.")
			return
		}
		if err = tx.Lock(r.Context(), 81241938); err != nil {
			a.fail(w, err)
			return
		}
		if !noAdminKeys(r.Context(), tx) {
			passkeyError(w, 409, "Первый ключ уже зарегистрирован. Обновите страницу и войдите с ключом.")
			return
		}
		email := strings.ToLower(strings.TrimSpace(a.Config.OwnerEmail))
		addr, e := mail.ParseAddress(email)
		if e != nil || addr.Address != email {
			passkeyError(w, 403, "Для первичной настройки укажите OWNER_EMAIL в окружении сервера.")
			return
		}
		_, err = tx.Exec(r.Context(), `INSERT INTO users(id,email,name,password_hash,is_admin) VALUES($1,$2,$3,'',true) ON CONFLICT(email) DO NOTHING`, randomToken(), email, a.Config.Author)
		if err != nil {
			a.fail(w, err)
			return
		}
		err = tx.QueryRow(r.Context(), `SELECT id FROM users WHERE email=$1 AND is_admin AND NOT is_banned FOR UPDATE`, email).Scan(&flow.UserID)
		if err != nil {
			passkeyError(w, 403, "Учётная запись администратора недоступна")
			return
		}
		flow.First = true
	} else {
		u := currentUser(r)
		c, e := r.Cookie("session")
		if u == nil || !u.Admin || e != nil {
			passkeyError(w, 403, "Нужна одноразовая ссылка настройки или вход администратора")
			return
		}
		flow.UserID = u.ID
		flow.SessionHash = hash(c.Value)
		if !freshAdminSession(r.Context(), tx, u.ID, flow.SessionHash) {
			passkeyError(w, 403, "Для добавления ключа войдите заново через /admin?reauth=1&next=/admin/keys")
			return
		}
	}
	u, err := loadPasskeyUser(r.Context(), tx, flow.UserID)
	if err != nil {
		passkeyError(w, 403, "Администратор недоступен")
		return
	}
	exclusions := []protocol.CredentialDescriptor{}
	for _, c := range u.Credentials {
		exclusions = append(exclusions, c.Descriptor())
	}
	options, session, err := a.passkeys.BeginRegistration(u, webauthn.WithExclusions(exclusions), webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired))
	if err != nil {
		a.fail(w, err)
		return
	}
	flow.Session = *session
	if err = tx.Commit(r.Context()); err != nil {
		a.fail(w, err)
		return
	}
	if err = a.savePasskeyFlow(w, r, "register", flow); err != nil {
		a.fail(w, err)
		return
	}
	protocolJSON(w, 200, options)
}
func (a *Server) passkeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	if a.limited(w, r, "passkey-register-finish", 20, 15*time.Minute) {
		return
	}
	flow, err := a.takePasskeyFlow(w, r, "register")
	if err != nil {
		passkeyError(w, 400, "Регистрация устарела. Начните её заново.")
		return
	}
	tx, err := a.Store.DB.Begin(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	// Serialize every registration against first-key enrollment, including CLI grants.
	if err = tx.Lock(r.Context(), 81241938); err != nil {
		a.fail(w, err)
		return
	}
	if flow.First && (!a.localFirstKeyRequest(r) || !noAdminKeys(r.Context(), tx)) {
		passkeyError(w, 409, "Первый ключ уже зарегистрирован или первичная настройка недоступна. Обновите страницу.")
		return
	}
	u, err := loadPasskeyUser(r.Context(), tx, flow.UserID)
	if err != nil {
		passkeyError(w, 403, "Администратор недоступен")
		return
	}
	if flow.First {
		// The direct loopback request and empty key store were checked under the lock.
	} else if flow.GrantHash != "" {
		result, e := tx.Exec(r.Context(), `DELETE FROM admin_setup_grants WHERE token_hash=$1 AND user_id=$2 AND expires_at>now()`, flow.GrantHash, u.ID)
		if e != nil || result.RowsAffected() != 1 {
			passkeyError(w, 403, "Ссылка настройки больше не действует")
			return
		}
	} else {
		c, e := r.Cookie("session")
		if e != nil || hash(c.Value) != flow.SessionHash || !freshAdminSession(r.Context(), tx, u.ID, flow.SessionHash) {
			passkeyError(w, 403, "Для добавления ключа войдите заново")
			return
		}
	}
	credential, err := a.passkeys.FinishRegistration(u, flow.Session, r)
	if err != nil {
		passkeyError(w, 400, "Не удалось проверить ключ. Повторите регистрацию.")
		return
	}
	raw, _ := json.Marshal(credential)
	_, err = tx.Exec(r.Context(), `INSERT INTO admin_passkeys(id,user_id,credential,label) VALUES($1,$2,$3,$4)`, base64.RawURLEncoding.EncodeToString(credential.ID), u.ID, raw, flow.Label)
	if err != nil {
		passkeyError(w, 400, "Этот ключ уже зарегистрирован")
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		a.fail(w, err)
		return
	}
	// A setup grant can only register a key; it never creates an authenticated session.
	protocolJSON(w, 200, map[string]string{"redirect": "/admin", "message": "Ключ зарегистрирован. Теперь войдите с ним."})
}
func (a *Server) deleteAdminKey(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	u := currentUser(r)
	tx, err := a.Store.DB.Begin(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err = loadPasskeyUser(r.Context(), tx, u.ID); err != nil {
		a.fail(w, err)
		return
	}
	c, _ := r.Cookie("session")
	if c == nil || !freshAdminSession(r.Context(), tx, u.ID, hash(c.Value)) {
		http.Redirect(w, r, "/admin?reauth=1&next=/admin/keys", 303)
		return
	}
	var count int
	if err = tx.QueryRow(r.Context(), `SELECT count(*) FROM admin_passkeys WHERE user_id=$1`, u.ID).Scan(&count); err != nil {
		a.fail(w, err)
		return
	}
	if count <= 1 {
		http.Error(w, a.text(r, "Сначала добавьте запасной ключ: последний удалить нельзя"), 400)
		return
	}
	if _, err = tx.Exec(r.Context(), `DELETE FROM admin_passkeys WHERE id=$1 AND user_id=$2`, r.PathValue("id"), u.ID); err != nil {
		a.fail(w, err)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		a.fail(w, err)
		return
	}
	http.Redirect(w, r, "/admin/keys", 303)
}

// Called only from the local CLI, never from an HTTP handler. A fresh grant
// revokes previous setup links, but does not delete existing passkeys.
func (s *Store) PrepareAdminPasskey(ctx context.Context, email, name string) (string, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `INSERT INTO users(id,email,name,password_hash,is_admin) VALUES($1,$2,$3,'',true) ON CONFLICT(email) DO NOTHING`, randomToken(), email, name)
	if err != nil {
		return "", err
	}
	var id string
	err = tx.QueryRow(ctx, `SELECT id FROM users WHERE email=$1 AND is_admin AND NOT is_banned FOR UPDATE`, email).Scan(&id)
	if err != nil {
		return "", errors.New("existing account is not an active administrator")
	}
	if _, err = tx.Exec(ctx, `DELETE FROM admin_setup_grants WHERE user_id=$1 OR expires_at<=now()`, id); err != nil {
		return "", err
	}
	token := randomToken()
	_, err = tx.Exec(ctx, `INSERT INTO admin_setup_grants(token_hash,user_id,expires_at) VALUES($1,$2,now()+interval '15 minutes')`, hash(token), id)
	if err != nil {
		return "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", err
	}
	return token, nil
}

// Direct local setup is available only on a loopback listener and localhost origin.
// Public/proxied deployments continue to require a CLI grant or recent admin login.
func (a *Server) localFirstKeyRequest(r *http.Request) bool {
	base, err := url.Parse(a.Config.BaseURL)
	if err != nil || base.Scheme != "http" || base.Hostname() != "localhost" || r.Host != base.Host {
		return false
	}
	listen, _, err := net.SplitHostPort(a.Config.Listen)
	if err != nil || !net.ParseIP(listen).IsLoopback() {
		return false
	}
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !net.ParseIP(peer).IsLoopback() {
		return false
	}
	for _, h := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Real-IP", "Via"} {
		if _, ok := r.Header[http.CanonicalHeaderKey(h)]; ok {
			return false
		}
	}
	return true
}
func noAdminKeys(ctx context.Context, tx Transaction) bool {
	var empty bool
	err := tx.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM admin_passkeys)`).Scan(&empty)
	return err == nil && empty
}
