package blog

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Webmention struct {
	ID, Direction, PostID, Source, Target, State, Moderation, Title, Kind, Detail string
	Attempts                                                                      int
	CreatedAt                                                                     time.Time
	Claim                                                                         string
}

const mentionCols = `id,direction,post_id,source,target,state,moderation,title,kind,detail,attempts,created_at,claim`

func scanMention(row interface{ Scan(...any) error }) (Webmention, error) {
	var m Webmention
	err := row.Scan(&m.ID, &m.Direction, &m.PostID, &m.Source, &m.Target, &m.State, &m.Moderation, &m.Title, &m.Kind, &m.Detail, &m.Attempts, &m.CreatedAt, &m.Claim)
	return m, err
}
func (m Webmention) StateLabel() string {
	return map[string]string{"queued": "В очереди", "verified": "Ссылка подтверждена", "removed": "Упоминание удалено", "failed": "Ошибка проверки или отправки", "sent": "Отправлено", "unsupported": "Сайт не принимает Webmention", "cancelled": "Отправка отменена"}[m.State]
}
func (m Webmention) KindLabel() string {
	return map[string]string{"mention": "Упоминание", "reply": "Ответ", "like": "Отметка «нравится»", "repost": "Репост"}[m.Kind]
}
func (m Webmention) ModerationLabel() string {
	return map[string]string{"pending": "Ожидает модерации", "approved": "Опубликовано", "rejected": "Скрыто"}[m.Moderation]
}
func acceptsMentions(p Post) bool {
	return p.Public(time.Now()) && !p.MembersOnly && p.ExternalURL == "" && p.Commentable
}
func (a *Server) mentionTarget(ctx context.Context, raw string) (Post, error) {
	u, err := url.Parse(raw)
	origin, _ := url.Parse(a.Config.BaseURL)
	if err != nil || u.User != nil || u.Scheme != origin.Scheme || !strings.EqualFold(u.Host, origin.Host) {
		return Post{}, errors.New("invalid target")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return Post{}, err
	}
	for key := range query {
		if key != "lang" {
			return Post{}, errors.New("unsupported target query")
		}
	}
	lang := query.Get("lang")
	if lang == "" {
		lang = a.Config.Language
	}
	if !langPattern.MatchString(lang) {
		return Post{}, errors.New("invalid language")
	}
	path := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(path) == 3 && path[2] == "index.html" {
		path = path[:2]
	} else if !strings.HasSuffix(u.Path, "/") {
		return Post{}, errors.New("invalid target path")
	}
	if len(path) != 2 {
		return Post{}, errors.New("invalid target path")
	}
	p, err := a.Store.Post(ctx, path[1], lang)
	if err != nil {
		return Post{}, err
	}
	if p.Kind != path[0] || !acceptsMentions(p) {
		return Post{}, errors.New("target does not accept mentions")
	}
	return p, nil
}
func mentionID(direction, source, target string) string {
	return hash(direction + "\n" + source + "\n" + target)
}

const enqueueMentionSQL = `INSERT INTO webmentions(id,direction,post_id,source,target) VALUES($1,$2,$3,$4,$5)
 ON CONFLICT(id) DO UPDATE SET state='queued',attempts=0,available_at=CASE WHEN $6 THEN now() ELSE GREATEST(now(),webmentions.updated_at+interval '5 seconds') END,locked_until=NULL,claim='',detail='',updated_at=now()`

func (a *Server) receiveMention(w http.ResponseWriter, r *http.Request) {
	contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if contentType != "application/x-www-form-urlencoded" || r.ParseForm() != nil {
		http.Error(w, "Expected form-encoded source and target", 400)
		return
	}
	source, target := strings.TrimSpace(r.PostForm.Get("source")), strings.TrimSpace(r.PostForm.Get("target"))
	if len(source) > 2048 || len(target) > 2048 || source == target || source == "" || target == "" {
		http.Error(w, "Invalid source or target", 400)
		return
	}
	su, err := publicURL(source)
	if err != nil {
		http.Error(w, "Source must be a public HTTP/HTTPS URL", 400)
		return
	}
	p, err := a.mentionTarget(r.Context(), target)
	if err != nil {
		http.Error(w, "Target does not accept Webmention", 400)
		return
	}
	if a.limited(w, r, "webmention-in", 30, time.Hour) {
		return
	}
	for _, key := range []string{"webmention-global", "webmention-source:" + hash(strings.ToLower(su.Host))} {
		limit := 300
		if key != "webmention-global" {
			limit = 60
		}
		ok, err := a.Store.Allow(r.Context(), key, limit, time.Hour)
		if err != nil {
			a.fail(w, err)
			return
		}
		if !ok {
			http.Error(w, "Too many requests", 429)
			return
		}
	}
	_, err = a.Store.DB.Exec(r.Context(), enqueueMentionSQL, mentionID("in", source, target), "in", p.ID, source, target, false)
	if err != nil {
		a.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintln(w, "Webmention queued for verification and moderation.")
}
func (s *Store) Mentions(ctx context.Context, postID string, public bool, offset int) ([]Webmention, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+mentionCols+` FROM webmentions WHERE ($1='' OR post_id=$1) AND (NOT $2 OR (direction='in' AND state='verified' AND moderation='approved')) ORDER BY created_at DESC,id LIMIT 51 OFFSET $3`, postID, public, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Webmention{}
	for rows.Next() {
		m, err := scanMention(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}
func (a *Server) mentionsAdmin(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	n := 1
	fmt.Sscan(r.URL.Query().Get("page"), &n)
	if n < 1 {
		n = 1
	}
	if n > 100000 {
		http.NotFound(w, r)
		return
	}
	mentions, err := a.Store.Mentions(r.Context(), "", false, (n-1)*50)
	if err != nil {
		a.fail(w, err)
		return
	}
	p := page{Mode: "webmentions", Title: "Упоминания", Mentions: mentions, Page: n}
	if len(mentions) > 50 {
		p.Mentions = mentions[:50]
		p.Next = n + 1
	}
	if n > 1 {
		p.Previous = n - 1
	}
	a.render(w, r, p)
}
func (a *Server) moderateMention(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	action := r.PostForm.Get("action")
	var err error
	switch action {
	case "approve", "reject":
		status := "rejected"
		if action == "approve" {
			status = "approved"
		}
		_, err = a.Store.DB.Exec(r.Context(), `UPDATE webmentions SET moderation=$2 WHERE id=$1 AND direction='in' AND ($2='rejected' OR state='verified')`, r.PathValue("id"), status)
	case "recheck":
		_, err = a.Store.DB.Exec(r.Context(), `UPDATE webmentions SET state='queued',attempts=0,available_at=now(),locked_until=NULL,claim='',detail='' WHERE id=$1 AND direction='in'`, r.PathValue("id"))
	default:
		http.Error(w, "Неизвестное действие", 400)
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	http.Redirect(w, r, "/godmode/webmentions/", 303)
}
func (a *Server) sendMentions(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	p, err := a.Store.PostID(r.Context(), r.PathValue("id"))
	if err != nil || !p.Public(time.Now()) || p.MembersOnly || p.ExternalURL != "" {
		http.Error(w, "Нужна опубликованная открытая запись", 400)
		return
	}
	source := a.Config.BaseURL + p.URL()
	if _, err := publicURL(source); err != nil {
		http.Error(w, "Для отправки нужен публичный адрес блога в BASE_URL. С localhost другие сайты не смогут проверить запись.", 400)
		return
	}
	if a.limited(w, r, "webmention-send:"+p.ID, 10, time.Hour) {
		return
	}
	targets := outgoingTargets(string(a.md.RenderPost(p)), source)
	if len(targets) > 100 {
		http.Error(w, "В записи больше 100 внешних ссылок; сократите их число для отправки Webmention.", 400)
		return
	}
	tx, err := a.Store.DB.Begin(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	// Re-send to previous targets too, including links removed from the current text.
	rows, err := tx.Query(r.Context(), `SELECT source,target FROM webmentions WHERE direction='out' AND post_id=$1`, p.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	type pair struct{ source, target string }
	pairs := map[pair]bool{}
	for rows.Next() {
		var v pair
		if err = rows.Scan(&v.source, &v.target); err != nil {
			break
		}
		pairs[v] = true
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		a.fail(w, err)
		return
	}
	for _, target := range targets {
		pairs[pair{source, target}] = true
	}
	if len(pairs) > 300 {
		http.Error(w, "Для этой записи накопилось больше 300 адресов отправки.", 400)
		return
	}
	for v := range pairs {
		if _, err = tx.Exec(r.Context(), enqueueMentionSQL, mentionID("out", v.source, v.target), "out", p.ID, v.source, v.target, true); err != nil {
			a.fail(w, err)
			return
		}
	}
	if err = tx.Commit(r.Context()); err != nil {
		a.fail(w, err)
		return
	}
	a.render(w, r, page{Mode: "message", Title: "Webmention", Message: fmt.Sprintf("В очередь поставлено уведомлений: %d. Результаты — в разделе «Упоминания» редактора.", len(pairs))})
}
func (a *Server) RunWebmentionWorker(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	client := publicPreviewClient()
	defer client.CloseIdleConnections()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for i := 0; i < 5; i++ {
				worked, err := a.processMention(ctx, client)
				if err != nil || !worked {
					break
				}
			}
		}
	}
}
func (a *Server) processMention(ctx context.Context, client *http.Client) (bool, error) {
	// A crash during the final attempt must not leave a job queued forever.
	if _, err := a.Store.DB.Exec(ctx, `UPDATE webmentions SET state='failed',detail='Проверка прервалась после последней попытки',claim='',locked_until=NULL WHERE state='queued' AND attempts>=5 AND (locked_until IS NULL OR locked_until<now())`); err != nil {
		return false, err
	}
	token := randomToken()
	m, err := scanMention(a.Store.DB.QueryRow(ctx, `UPDATE webmentions SET locked_until=now()+interval '2 minutes',claim=$1,attempts=attempts+1 WHERE id=(SELECT id FROM webmentions WHERE state='queued' AND attempts<5 AND available_at<=now() AND (locked_until IS NULL OR locked_until<now()) ORDER BY available_at FOR UPDATE SKIP LOCKED LIMIT 1) RETURNING `+mentionCols, token))
	if errors.Is(err, ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	state, title, kind, detail := "", "", "mention", ""
	p, err := a.Store.PostID(ctx, m.PostID)
	if err != nil {
		return true, err
	}
	if m.Direction == "in" {
		if _, e := a.mentionTarget(ctx, m.Target); e != nil {
			state, detail = "removed", "Запись больше не принимает упоминания"
		} else {
			state, title, kind, detail = verifyMention(ctx, client, m.Source, m.Target)
		}
	} else {
		if !p.Public(time.Now()) || p.MembersOnly || p.ExternalURL != "" || a.Config.BaseURL+p.URL() != m.Source {
			state, detail = "cancelled", "Запись скрыта или её адрес изменился"
		} else {
			state, detail = deliverMention(ctx, client, m.Source, m.Target)
		}
	}
	if state == "retry" {
		state = "queued"
		if m.Attempts >= 5 {
			state = "failed"
		}
		_, err = a.Store.DB.Exec(ctx, `UPDATE webmentions SET state=$3,detail=$4,locked_until=NULL,claim='',available_at=now()+$5::interval WHERE id=$1 AND claim=$2`, m.ID, token, state, detail, fmt.Sprintf("%d seconds", m.Attempts*m.Attempts*30))
	} else {
		_, err = a.Store.DB.Exec(ctx, `UPDATE webmentions SET state=$3,title=$4,kind=$5,detail=$6,locked_until=NULL,claim='',moderation=CASE WHEN direction='in' AND moderation='approved' AND (title<>$4 OR kind<>$5) THEN 'pending' ELSE moderation END WHERE id=$1 AND claim=$2`, m.ID, token, state, title, kind, detail)
	}
	return true, err
}
