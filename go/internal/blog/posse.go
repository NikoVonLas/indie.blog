package blog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"golang.org/x/net/html"
)

type SyndicationTarget struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
}
type Syndication struct {
	ID, PostID, Target, State, Body, URL, Detail, Revision string
	Attempts                                               int
}

func (s Syndication) StateLabel() string {
	return map[string]string{"queued": "В очереди", "sending": "Отправляется", "sent": "Опубликовано", "failed": "Не отправлено", "uncertain": "Нужно проверить канал", "cancelled": "Отменено"}[s.State]
}
func (s Syndication) Retryable() bool { return s.State == "failed" || s.State == "cancelled" }

var telegramChannelPattern = regexp.MustCompile(`^@[A-Za-z][A-Za-z0-9_]{4,31}$`)
var telegramTokenPattern = regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`)

func (a *Server) initPOSSE() error {
	if a.Config.TelegramChannel != "" && !telegramChannelPattern.MatchString(a.Config.TelegramChannel) {
		return errors.New("TELEGRAM_CHANNEL must be a public @username")
	}
	if a.Config.TelegramBotToken != "" && (!telegramTokenPattern.MatchString(a.Config.TelegramBotToken) || a.Config.TelegramChannel == "") {
		return errors.New("invalid Telegram configuration")
	}
	a.posseHTTP = publicPreviewClient()
	a.posseHTTP.Timeout = 20 * time.Second
	a.posseHTTP.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return nil
}
func (a *Server) telegramTarget() SyndicationTarget {
	if a.Config.TelegramChannel == "" {
		return SyndicationTarget{}
	}
	return SyndicationTarget{"https://t.me/" + strings.ToLower(strings.TrimPrefix(a.Config.TelegramChannel, "@")), "Telegram · " + a.Config.TelegramChannel}
}
func (a *Server) posseReady() string {
	if a.Config.TelegramBotToken == "" || a.Config.TelegramChannel == "" {
		return "Telegram-канал ещё не подключён."
	}
	u, e := publicURL(a.Config.BaseURL)
	if e != nil || u.Scheme != "https" {
		return "Для отправки нужна публичная HTTPS-ссылка на блог."
	}
	return ""
}
func (a *Server) syndicationTargets() []SyndicationTarget {
	targets := []SyndicationTarget{}
	if a.posseReady() == "" {
		targets = append(targets, a.telegramTarget())
	}
	return targets
}
func possePublic(p Post) bool { return p.Public(time.Now()) && !p.MembersOnly && p.ExternalURL == "" }

// Keep paragraph boundaries, but never copy script, embed or hidden spoiler contents.
func possePlain(raw string) string {
	doc, e := html.Parse(strings.NewReader(raw))
	if e != nil {
		return ""
	}
	var out strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "iframe", "template", "details":
				return
			}
			if strings.Contains(attr(n, "class"), "spoiler") {
				out.WriteString("[спойлер]")
				return
			}
			switch n.Data {
			case "p", "div", "br", "li", "h1", "h2", "h3", "blockquote", "pre":
				out.WriteByte('\n')
			}
		}
		if n.Type == html.TextNode {
			out.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode {
			switch n.Data {
			case "p", "div", "li", "h1", "h2", "h3", "blockquote", "pre":
				out.WriteByte('\n')
			}
		}
	}
	walk(doc)
	lines := []string{}
	for _, line := range strings.Split(out.String(), "\n") {
		if line = strings.Join(strings.Fields(line), " "); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n\n")
}
func utf16Length(s string) int { return len(utf16.Encode([]rune(s))) }
func truncateTelegram(s string, limit int) string {
	if utf16Length(s) <= limit {
		return s
	}
	var out strings.Builder
	used := 0
	for _, r := range s {
		n := 1
		if r > 0xffff {
			n = 2
		}
		if used+n > limit-1 {
			break
		}
		out.WriteRune(r)
		used += n
	}
	return strings.TrimSpace(out.String()) + "…"
}
func (a *Server) telegramText(p Post) string {
	body := possePlain(string(a.md.Render(p.Body, p.RawHTML)))
	if p.Kind == "blog" {
		switch {
		case p.Summary != "":
			body = p.Summary
		case p.Subtitle != "":
			body = p.Subtitle
		}
		body = truncateTelegram(body, 700)
	}
	if p.Title != "" {
		body = strings.TrimSpace(p.Title + "\n\n" + body)
	}
	suffix := "\n\n" + a.Config.BaseURL + p.URL()
	return truncateTelegram(body, 4096-utf16Length(suffix)) + suffix
}
func posseRevision(p Post, body, target string) string {
	return hash(p.UpdatedAt.Format(time.RFC3339Nano) + "\n" + body + "\n" + target)
}

const syndicationCols = `id,post_id,target,state,body,url,detail,revision,attempts`

func scanSyndication(row interface{ Scan(...any) error }) (Syndication, error) {
	var s Syndication
	e := row.Scan(&s.ID, &s.PostID, &s.Target, &s.State, &s.Body, &s.URL, &s.Detail, &s.Revision, &s.Attempts)
	return s, e
}
func (a *Server) syndications(ctx context.Context, id string) ([]Syndication, error) {
	rows, e := a.Store.DB.Query(ctx, `SELECT `+syndicationCols+` FROM syndications WHERE post_id=$1 ORDER BY created_at`, id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	result := []Syndication{}
	for rows.Next() {
		s, e := scanSyndication(rows)
		if e != nil {
			return nil, e
		}
		result = append(result, s)
	}
	return result, rows.Err()
}
func enqueueSyndication(ctx context.Context, db executor, p Post, target, body string) error {
	_, e := db.Exec(ctx, `INSERT INTO syndications(id,post_id,target,body,revision) VALUES($1,$2,$3,$4,$5) ON CONFLICT(post_id,target) DO NOTHING`, randomToken(), p.ID, target, body, posseRevision(p, body, target))
	return e
}
func (a *Server) possePage(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	p, e := a.Store.PostID(r.Context(), r.PathValue("id"))
	if e != nil {
		http.NotFound(w, r)
		return
	}
	target := a.telegramTarget()
	if r.Method == "POST" {
		tx, e := a.Store.DB.Begin(r.Context())
		if e != nil {
			a.fail(w, e)
			return
		}
		defer tx.Rollback(r.Context())
		p, e = scanPost(tx.QueryRow(r.Context(), `SELECT `+postCols+` FROM posts WHERE id=$1 FOR UPDATE`, p.ID))
		if e != nil {
			a.fail(w, e)
			return
		}
		switch r.PostForm.Get("action") {
		case "send", "retry", "retry-checked":
			if reason := a.posseReady(); reason != "" {
				http.Error(w, reason, 409)
				return
			}
			if !possePublic(p) {
				http.Error(w, a.text(r, "Можно отправить только опубликованную открытую запись."), 409)
				return
			}
			body := a.telegramText(p)
			if r.PostForm.Get("revision") != posseRevision(p, body, target.UID) {
				http.Error(w, a.text(r, "Запись или канал изменились. Обновите предпросмотр."), 409)
				return
			}
			if r.PostForm.Get("action") == "retry-checked" {
				if r.PostForm.Get("checked") != "yes" {
					http.Error(w, a.text(r, "Проверьте канал и подтвердите отсутствие сообщения."), 400)
					return
				}
				_, e = tx.Exec(r.Context(), `UPDATE syndications SET state='queued',body=$1,revision=$2,detail='',attempts=0,available_at=now(),updated_at=now() WHERE post_id=$3 AND target=$4 AND state='uncertain'`, body, posseRevision(p, body, target.UID), p.ID, target.UID)
			} else if r.PostForm.Get("action") == "retry" {
				_, e = tx.Exec(r.Context(), `UPDATE syndications SET state='queued',body=$1,revision=$2,detail='',attempts=0,available_at=now(),updated_at=now() WHERE post_id=$3 AND target=$4 AND state IN ('failed','cancelled')`, body, posseRevision(p, body, target.UID), p.ID, target.UID)
			} else {
				e = enqueueSyndication(r.Context(), tx, p, target.UID, body)
			}
		case "resolve":
			s, err := scanSyndication(tx.QueryRow(r.Context(), `SELECT `+syndicationCols+` FROM syndications WHERE id=$1 AND post_id=$2 AND state='uncertain' FOR UPDATE`, r.PostForm.Get("job"), p.ID))
			if err != nil {
				http.Error(w, a.text(r, "Отправка уже обработана."), 409)
				return
			}
			link := strings.TrimSpace(r.PostForm.Get("url"))
			if !telegramMessageURL(link, s.Target) {
				http.Error(w, a.text(r, "Нужна ссылка на сообщение в этом канале."), 400)
				return
			}
			if !possePublic(p) {
				http.Error(w, a.text(r, "Сначала восстановите публичную запись."), 409)
				return
			}
			e = saveSyndicationLink(r.Context(), tx, p, link)
			if e == nil {
				_, e = tx.Exec(r.Context(), `UPDATE syndications SET state='sent',url=$1,detail='',updated_at=now() WHERE id=$2`, link, s.ID)
			}
		case "cancel":
			_, e = tx.Exec(r.Context(), `UPDATE syndications SET state='cancelled',detail='',updated_at=now() WHERE post_id=$1 AND id=$2 AND state='queued'`, p.ID, r.PostForm.Get("job"))
		default:
			http.Error(w, a.text(r, "Неизвестное действие"), 400)
			return
		}
		if e != nil {
			a.fail(w, e)
			return
		}
		if e = tx.Commit(r.Context()); e != nil {
			a.fail(w, e)
			return
		}
		http.Redirect(w, r, r.URL.Path, 303)
		return
	}
	jobs, e := a.syndications(r.Context(), p.ID)
	if e != nil {
		a.fail(w, e)
		return
	}
	reason := a.posseReady()
	if !possePublic(p) {
		reason = "Сначала опубликуйте запись с открытым доступом."
	}
	canSend := true
	for _, job := range jobs {
		if job.Target == target.UID {
			canSend = false
		}
	}
	body := a.telegramText(p)
	a.render(w, r, page{Mode: "posse", Title: a.text(r, "Отправить в Telegram"), Post: p, Syndications: jobs, POSSETarget: target, POSSEPreview: body, POSSERevision: posseRevision(p, body, target.UID), POSSEReason: reason, POSSECanSend: canSend})
}
func telegramMessageURL(raw, target string) bool {
	if !strings.HasPrefix(raw, target+"/") {
		return false
	}
	id, e := strconv.ParseInt(strings.TrimPrefix(raw, target+"/"), 10, 64)
	return e == nil && id > 0
}
func (p Post) posseLinks() []string {
	var d struct {
		Links []string `json:"posse"`
	}
	_ = json.Unmarshal(p.Data, &d)
	return d.Links
}
func saveSyndicationLink(ctx context.Context, db executor, p Post, link string) error {
	links := p.posseLinks()
	if !slices.Contains(links, link) {
		links = append(links, link)
	}
	raw, _ := json.Marshal(links)
	_, e := db.Exec(ctx, `UPDATE posts SET data=jsonb_set(data,'{posse}',$2::jsonb),updated_at=now() WHERE id=$1`, p.ID, raw)
	return e
}

// sendMessage has no idempotency key. Ambiguous results must never be retried automatically.
func (a *Server) sendTelegram(ctx context.Context, body string) (state, link, detail string, retry int) {
	payload, _ := json.Marshal(map[string]any{"chat_id": a.Config.TelegramChannel, "text": body, "link_preview_options": map[string]any{"url": body[strings.LastIndex(body, "\n")+1:], "prefer_large_media": true}})
	req, e := http.NewRequestWithContext(ctx, "POST", "https://api.telegram.org/bot"+a.Config.TelegramBotToken+"/sendMessage", bytes.NewReader(payload))
	if e != nil {
		return "failed", "", "Не удалось подготовить отправку.", 0
	}
	req.Header.Set("Content-Type", "application/json")
	res, e := a.posseHTTP.Do(req)
	if e != nil {
		return "uncertain", "", "Нет подтверждения Telegram. Проверьте канал перед следующей отправкой.", 0
	}
	defer res.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	var reply struct {
		OK         bool `json:"ok"`
		ErrorCode  int  `json:"error_code"`
		Parameters struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
		Result struct {
			ID   int64                           `json:"message_id"`
			Chat struct{ Type, Username string } `json:"chat"`
		} `json:"result"`
	}
	if e != nil || len(raw) > 1<<20 || json.Unmarshal(raw, &reply) != nil {
		return "uncertain", "", "Не удалось прочитать подтверждение Telegram. Проверьте канал.", 0
	}
	if reply.OK && reply.Result.ID > 0 && reply.Result.Chat.Type == "channel" && strings.EqualFold(reply.Result.Chat.Username, strings.TrimPrefix(a.Config.TelegramChannel, "@")) {
		return "sent", a.telegramTarget().UID + "/" + strconv.FormatInt(reply.Result.ID, 10), "", 0
	}
	if !reply.OK && reply.ErrorCode == 429 && reply.Parameters.RetryAfter > 0 {
		if reply.Parameters.RetryAfter > 86400 {
			return "failed", "", "Telegram временно ограничил отправку. Повторите позже.", 0
		}
		return "queued", "", "Telegram попросил подождать.", reply.Parameters.RetryAfter
	}
	if !reply.OK && reply.ErrorCode >= 400 && reply.ErrorCode < 500 {
		return "failed", "", fmt.Sprintf("Telegram отклонил отправку (%d). Проверьте подключение бота и права публикации.", reply.ErrorCode), 0
	}
	return "uncertain", "", "Telegram не подтвердил результат. Проверьте канал.", 0
}
func (a *Server) RunPOSSEWorker(ctx context.Context) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if e := a.processSyndication(ctx); e != nil && ctx.Err() == nil {
				slog.Error("POSSE worker failed", "type", fmt.Sprintf("%T", e))
			}
		}
	}
}
func (a *Server) processSyndication(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	// Recovery after a crash is deliberately not a resend.
	_, e := a.Store.DB.Exec(ctx, `UPDATE syndications SET state='uncertain',detail='Отправка прервалась. Проверьте канал.',updated_at=now() WHERE state='sending' AND updated_at<now()-interval '2 minutes'`)
	if e != nil {
		return e
	}
	if a.posseReady() != "" {
		return nil
	}
	job, e := scanSyndication(a.Store.DB.QueryRow(ctx, `UPDATE syndications SET state='sending',attempts=attempts+1,updated_at=now() WHERE id=(SELECT id FROM syndications WHERE state='queued' AND available_at<=now() ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1) RETURNING `+syndicationCols))
	if errors.Is(e, ErrNoRows) {
		return nil
	}
	if e != nil {
		return e
	}
	tx, e := a.Store.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	// Hold the post lock through the request: a concurrent edit cannot make it private mid-send.
	p, e := scanPost(tx.QueryRow(ctx, `SELECT `+postCols+` FROM posts WHERE id=$1 FOR UPDATE`, job.PostID))
	if e != nil {
		return e
	}
	var stillSending string
	if e = tx.QueryRow(ctx, `SELECT state FROM syndications WHERE id=$1 FOR UPDATE`, job.ID).Scan(&stillSending); e != nil {
		return e
	}
	if stillSending != "sending" {
		return nil
	}
	body := a.telegramText(p)
	state, link, detail, retry := "cancelled", "", "Запись или канал изменились. Обновите предпросмотр перед отправкой.", 0
	if possePublic(p) && job.Target == a.telegramTarget().UID && job.Revision == posseRevision(p, body, job.Target) {
		state, link, detail, retry = a.sendTelegram(ctx, job.Body)
	}
	if state == "queued" && job.Attempts >= 5 {
		state = "failed"
		detail = "Telegram продолжает ограничивать отправку. Повторите позже."
	}
	if state == "sent" {
		if e = saveSyndicationLink(ctx, tx, p, link); e != nil {
			return e
		}
	}
	_, e = tx.Exec(ctx, `UPDATE syndications SET state=$1,url=$2,detail=$3,available_at=now()+($4 * interval '1 second'),updated_at=now() WHERE id=$5 AND state='sending'`, state, link, detail, retry, job.ID)
	if e != nil {
		return e
	}
	return tx.Commit(ctx)
}

// Atomically save a Micropub-created post and its explicitly requested syndication jobs.
func (a *Server) saveMicropubPOSSE(ctx context.Context, p *Post, requested []any) error {
	if len(requested) == 0 {
		return a.Store.SavePost(ctx, p)
	}
	target := a.telegramTarget()
	if a.posseReady() != "" || !possePublic(*p) {
		return errors.New("Syndication requires a connected target and an already public post")
	}
	for _, v := range requested {
		s, ok := v.(string)
		if !ok || s != target.UID {
			return errors.New("Unknown syndication target")
		}
	}
	tx, e := a.Store.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if e = savePost(ctx, tx, p); e != nil {
		return e
	}
	// Build the revision from the stored representation, including database timestamp precision.
	stored, e := scanPost(tx.QueryRow(ctx, `SELECT `+postCols+` FROM posts WHERE id=$1`, p.ID))
	if e != nil {
		return e
	}
	if e = enqueueSyndication(ctx, tx, stored, target.UID, a.telegramText(stored)); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

// Keep the imported Micropub links and successful POSSE links in one public view.
func (p Post) syndicationLinks(props mpProperties) []any {
	result := append([]any{}, props["syndication"]...)
	for _, link := range p.posseLinks() {
		if !slices.ContainsFunc(result, func(v any) bool { return v == link }) {
			result = append(result, link)
		}
	}
	return result
}
