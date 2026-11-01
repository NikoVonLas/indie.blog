package blog

import (
	"context"
	"crypto/hmac"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

func (a *Server) subscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		a.render(w, r, page{Mode: "subscribe", Title: "Подписаться"})
		return
	}
	if a.Config.SMTPAddr == "" {
		http.Error(w, "Рассылка пока не подключена. Используйте RSS.", 503)
		return
	}
	if a.limited(w, r, "subscribe", 5, time.Hour) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.PostForm.Get("email")))
	addr, e := mail.ParseAddress(email)
	if e != nil || addr.Address != email || len(email) > 254 || r.PostForm.Get("website") != "" {
		http.Error(w, "Проверьте адрес почты", 400)
		return
	}
	ok, e := a.Store.Allow(r.Context(), "subscribe-email:"+a.sign(email), 3, time.Hour)
	if e != nil {
		a.fail(w, e)
		return
	}
	if !ok {
		http.Error(w, "Проверьте почту или попробуйте позже", 429)
		return
	}
	token := randomToken()
	id := randomToken()
	lang := a.lang(r)
	tx, e := a.Store.DB.Begin(r.Context())
	if e != nil {
		a.fail(w, e)
		return
	}
	defer tx.Rollback(r.Context())
	var confirmed bool
	e = tx.QueryRow(r.Context(), `INSERT INTO subscribers(id,email,lang,token_hash) VALUES($1,$2,$3,$4)
 ON CONFLICT(email) DO UPDATE SET token_hash=CASE WHEN subscribers.confirmed THEN subscribers.token_hash ELSE excluded.token_hash END RETURNING confirmed`, id, email, lang, hash(token)).Scan(&confirmed)
	if e != nil {
		a.fail(w, e)
		return
	}
	if !confirmed {
		_, e = tx.Exec(r.Context(), `INSERT INTO outbox(id,dedup_key,recipient,subject,body) VALUES($1,$2,$3,$4,$5)`, randomToken(), "confirm:"+hash(token), email, "Подтверждение подписки — "+a.Config.Title, "Подтвердите подписку: "+a.Config.BaseURL+"/subscribe/confirm/"+token+"/\n\nЕсли вы не подписывались, просто проигнорируйте это письмо.")
		if e != nil {
			a.fail(w, e)
			return
		}
	}
	if e = tx.Commit(r.Context()); e != nil {
		a.fail(w, e)
		return
	}
	a.render(w, r, page{Mode: "message", Title: "Проверьте почту", Message: "Если адрес ещё не подтверждён, мы отправим письмо со ссылкой."})
}
func (a *Server) confirm(w http.ResponseWriter, r *http.Request) {
	tag, e := a.Store.DB.Exec(r.Context(), `UPDATE subscribers SET confirmed=true WHERE token_hash=$1 AND created_at>now()-interval '30 days'`, hash(r.PathValue("token")))
	if e != nil {
		a.fail(w, e)
		return
	}
	if tag.RowsAffected() == 0 {
		http.Error(w, "Ссылка недействительна или истекла", 400)
		return
	}
	a.render(w, r, page{Mode: "message", Title: "Подписка подтверждена"})
}
func (a *Server) unsubscribe(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.PathValue("token"), ".")
	if len(parts) != 2 || !hmac.Equal([]byte(parts[1]), []byte(a.sign("unsubscribe:"+parts[0]))) {
		http.NotFound(w, r)
		return
	}
	if r.Method == "GET" {
		a.render(w, r, page{Mode: "unsubscribe", Title: "Отписаться"})
		return
	}
	tx, e := a.Store.DB.Begin(r.Context())
	if e != nil {
		a.fail(w, e)
		return
	}
	defer tx.Rollback(r.Context())
	if _, e = tx.Exec(r.Context(), `DELETE FROM outbox WHERE delivered_at IS NULL AND recipient IN (SELECT email FROM subscribers WHERE id=$1)`, parts[0]); e != nil {
		a.fail(w, e)
		return
	}
	if _, e = tx.Exec(r.Context(), `DELETE FROM subscribers WHERE id=$1`, parts[0]); e != nil {
		a.fail(w, e)
		return
	}
	if e = tx.Commit(r.Context()); e != nil {
		a.fail(w, e)
		return
	}
	a.render(w, r, page{Mode: "message", Title: "Вы отписались"})
}
func (a *Server) announce(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	if a.Config.SMTPAddr == "" {
		http.Error(w, "Рассылка не подключена", 503)
		return
	}
	p, e := a.Store.PostID(r.Context(), r.PathValue("id"))
	if e != nil || !p.Public(time.Now()) || p.MembersOnly {
		http.Error(w, "Можно рассылать только опубликованные открытые записи", 400)
		return
	}
	// Materialise recipients before starting a transaction, avoiding pool starvation.
	rows, e := a.Store.DB.Query(r.Context(), `SELECT id,email FROM subscribers WHERE confirmed AND lang=$1`, p.Lang)
	if e != nil {
		a.fail(w, e)
		return
	}
	type subscriber struct{ id, email string }
	subs := []subscriber{}
	for rows.Next() {
		var sub subscriber
		if e = rows.Scan(&sub.id, &sub.email); e != nil {
			rows.Close()
			a.fail(w, e)
			return
		}
		subs = append(subs, sub)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		a.fail(w, e)
		return
	}
	tx, e := a.Store.DB.Begin(r.Context())
	if e != nil {
		a.fail(w, e)
		return
	}
	defer tx.Rollback(r.Context())
	for _, sub := range subs {
		body := p.Title + "\n\n" + p.Summary + "\n\n" + a.Config.BaseURL + p.URL() + "\n\nОтписаться: " + a.Config.BaseURL + "/unsubscribe/" + sub.id + "." + a.sign("unsubscribe:"+sub.id) + "/"
		if _, e = tx.Exec(r.Context(), `INSERT INTO outbox(id,dedup_key,recipient,subject,body) VALUES($1,$2,$3,$4,$5) ON CONFLICT(dedup_key) DO NOTHING`, randomToken(), "post:"+p.ID+":"+sub.id, sub.email, p.Title, body); e != nil {
			a.fail(w, e)
			return
		}
	}
	if e = tx.Commit(r.Context()); e != nil {
		a.fail(w, e)
		return
	}
	a.render(w, r, page{Mode: "message", Title: "Анонс поставлен в очередь"})
}
func (a *Server) RunMailWorker(ctx context.Context) {
	if a.Config.SMTPAddr == "" {
		return
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for i := 0; i < 20; i++ {
				worked, _ := a.deliverOne(ctx)
				if !worked {
					break
				}
			}
		}
	}
}
func (a *Server) deliverOne(ctx context.Context) (bool, error) {
	var id, to, subject, body string
	var attempt int
	e := a.Store.DB.QueryRow(ctx, `UPDATE outbox SET locked_until=now()+interval '2 minutes',attempts=attempts+1
 WHERE id=(SELECT id FROM outbox WHERE delivered_at IS NULL AND attempts<8 AND available_at<=now() AND (locked_until IS NULL OR locked_until<now()) ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1)
 RETURNING id,recipient,subject,body,attempts`).Scan(&id, &to, &subject, &body, &attempt)
	if errors.Is(e, ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	if e = a.sendMail(ctx, id, to, subject, body); e != nil {
		_, dbErr := a.Store.DB.Exec(ctx, `UPDATE outbox SET locked_until=NULL,available_at=now()+$2::interval WHERE id=$1`, id, fmt.Sprintf("%d seconds", attempt*attempt*30))
		return true, dbErr
	}
	_, e = a.Store.DB.Exec(ctx, `UPDATE outbox SET delivered_at=now(),locked_until=NULL WHERE id=$1`, id)
	return true, e
}
func (a *Server) sendMail(ctx context.Context, id, to, subject, body string) error {
	host, _, e := net.SplitHostPort(a.Config.SMTPAddr)
	if e != nil {
		return e
	}
	from, e := mail.ParseAddress(a.Config.MailFrom)
	if e != nil {
		return e
	}
	recipient, e := mail.ParseAddress(to)
	if e != nil {
		return e
	}
	conn, e := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", a.Config.SMTPAddr)
	if e != nil {
		return e
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	c, e := smtp.NewClient(conn, host)
	if e != nil {
		return e
	}
	defer c.Close()
	if ok, _ := c.Extension("STARTTLS"); ok {
		if e = c.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); e != nil {
			return e
		}
	} else {
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return errors.New("SMTP server must support STARTTLS")
		}
	}
	if a.Config.SMTPUser != "" {
		if e = c.Auth(smtp.PlainAuth("", a.Config.SMTPUser, a.Config.SMTPPassword, host)); e != nil {
			return e
		}
	}
	if e = c.Mail(from.Address); e != nil {
		return e
	}
	if e = c.Rcpt(recipient.Address); e != nil {
		return e
	}
	out, e := c.Data()
	if e != nil {
		return e
	}
	message := "From: " + from.String() + "\r\nTo: " + recipient.String() + "\r\nSubject: " + mime.QEncoding.Encode("utf-8", strings.ReplaceAll(strings.ReplaceAll(subject, "\r", ""), "\n", " ")) + "\r\nMessage-ID: <" + id + "@" + strings.TrimPrefix(strings.TrimPrefix(a.Config.BaseURL, "https://"), "http://") + ">\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n" + strings.ReplaceAll(body, "\n", "\r\n")
	if _, e = io.WriteString(out, message); e != nil {
		return e
	}
	if e = out.Close(); e != nil {
		return e
	}
	return c.Quit()
}
