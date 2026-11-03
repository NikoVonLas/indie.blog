package blog

import (
	"net/http"
	"net/url"
	"strings"
	"time"
)

func (a *Server) comment(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		http.Error(w, "Войдите, чтобы комментировать", 401)
		return
	}
	if a.limited(w, r, "comment", 30, 24*time.Hour) {
		return
	}
	p, e := a.Store.PostID(r.Context(), r.PostForm.Get("post_id"))
	if e != nil || !p.Readable(u) {
		http.NotFound(w, r)
		return
	}
	if !p.Commentable {
		http.Error(w, "Комментарии закрыты", 403)
		return
	}
	body := strings.TrimSpace(r.PostForm.Get("body"))
	block := r.PostForm.Get("block")
	reply := r.PostForm.Get("reply_to")
	if body == "" || len(body) > 10000 || len(block) > 128 {
		http.Error(w, "Комментарий пустой или слишком длинный", 400)
		return
	}
	if reply != "" {
		var valid bool
		e = a.Store.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM comments WHERE id=$1 AND post_id=$2 AND NOT deleted)`, reply, p.ID).Scan(&valid)
		if e != nil {
			a.fail(w, e)
			return
		}
		if !valid {
			http.Error(w, "Ответ относится к другой публикации", 400)
			return
		}
	}
	id := randomToken()
	_, e = a.Store.DB.Exec(r.Context(), `INSERT INTO comments(id,post_id,user_id,author_name,body,block,reply_to,author_url) VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8)`, id, p.ID, u.ID, u.Name, body, block, reply, u.ProfileURL)
	if e != nil {
		a.fail(w, e)
		return
	}
	http.Redirect(w, r, p.URL()+"#comment-"+id, 303)
}
func (a *Server) moderate(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	column := "deleted"
	if strings.HasSuffix(r.URL.Path, "/pin/") {
		column = "pinned"
	}
	var postID string
	e := a.Store.DB.QueryRow(r.Context(), `UPDATE comments SET `+column+`=NOT `+column+` WHERE id=$1 RETURNING post_id`, id).Scan(&postID)
	if e != nil {
		http.NotFound(w, r)
		return
	}
	p, e := a.Store.PostID(r.Context(), postID)
	if e != nil {
		a.fail(w, e)
		return
	}
	http.Redirect(w, r, p.URL()+"#comments", 303)
}
func (a *Server) react(w http.ResponseWriter, r *http.Request) {
	if a.limited(w, r, "reaction", 100, time.Hour) {
		return
	}
	var p Post
	var e error
	target := ""
	if id := r.PathValue("id"); id != "" {
		var postID string
		e = a.Store.DB.QueryRow(r.Context(), `SELECT post_id FROM comments WHERE id=$1 AND NOT deleted`, id).Scan(&postID)
		if e != nil {
			http.NotFound(w, r)
			return
		}
		p, e = a.Store.PostID(r.Context(), postID)
		target = "comment:" + id
	} else {
		p, e = a.Store.Post(r.Context(), r.PathValue("slug"), a.lang(r))
		target = "block:" + r.PathValue("block")
	}
	if e != nil || !p.Readable(currentUser(r)) || len(target) > 256 {
		http.NotFound(w, r)
		return
	}
	actor := ""
	if u := currentUser(r); u != nil {
		actor = "user:" + u.ID
	} else {
		c, e := r.Cookie("csrf")
		if e != nil {
			http.Error(w, "Обновите страницу", 400)
			return
		}
		actor = "visitor:" + hash(c.Value)
	}
	_, e = a.Store.DB.Exec(r.Context(), `INSERT INTO reactions(post_id,target,actor) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, p.ID, target, actor)
	if e != nil {
		a.fail(w, e)
		return
	}
	anchor := "comments"
	if strings.HasPrefix(target, "comment:") {
		anchor = "comment-" + strings.TrimPrefix(target, "comment:")
	}
	http.Redirect(w, r, p.URL()+"#"+url.PathEscape(anchor), 303)
}
