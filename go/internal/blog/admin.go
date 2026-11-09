package blog

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

func (a *Server) login(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, page{Mode: "login", Title: a.text(r, "Войти")})
}
func (a *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, e := r.Cookie("session"); e == nil {
		if _, e = a.Store.DB.Exec(r.Context(), `DELETE FROM sessions WHERE token_hash=$1`, hash(c.Value)); e != nil {
			a.fail(w, e)
			return
		}
	}
	a.cookie(w, "session", "", -1)
	a.cookie(w, "csrf", "", -1)
	http.Redirect(w, r, "/", 303)
}
func (a *Server) profile(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/login/", 303)
		return
	}
	if r.Method == "POST" {
		name := strings.TrimSpace(r.PostForm.Get("name"))
		avatar := strings.TrimSpace(r.PostForm.Get("avatar"))
		if name == "" || len(name) > 128 || !safeURL(avatar) {
			http.Error(w, a.text(r, "Проверьте имя и адрес аватара"), 400)
			return
		}
		if _, e := a.Store.DB.Exec(r.Context(), `UPDATE users SET name=$1,avatar=$2 WHERE id=$3`, name, avatar, u.ID); e != nil {
			a.fail(w, e)
			return
		}
		http.Redirect(w, r, "/profile/", 303)
		return
	}
	a.render(w, r, page{Mode: "profile", Title: a.text(r, "Профиль")})
}
func (a *Server) admin(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	posts, e := a.Store.Posts(r.Context(), Filter{Admin: true, Limit: 1000})
	if e != nil {
		a.fail(w, e)
		return
	}
	a.render(w, r, page{Mode: "admin", Title: a.text(r, "Публикации"), Posts: posts})
}
func (a *Server) edit(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	p := Post{Kind: "blog", Lang: a.Config.Language, Author: a.Config.Author, Commentable: true, OnHome: true, PublishedAt: time.Now().UTC(), Data: json.RawMessage(`{}`)}
	if id != "" {
		var e error
		p, e = a.Store.PostID(r.Context(), id)
		if errors.Is(e, ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if e != nil {
			a.fail(w, e)
			return
		}
	}
	if r.Method == "POST" {
		f := r.PostForm
		p.Title = f.Get("title")
		p.Slug = f.Get("slug")
		p.Kind = f.Get("kind")
		p.Lang = f.Get("lang")
		p.Subtitle = f.Get("subtitle")
		p.Body = f.Get("body")
		p.Summary = f.Get("summary")
		p.Image = f.Get("image")
		p.Author = f.Get("author")
		p.ExternalURL = f.Get("external_url")
		p.CSS = f.Get("css")
		p.Visible = f.Get("visible") == "on"
		p.MembersOnly = f.Get("members_only") == "on"
		p.Commentable = f.Get("commentable") == "on"
		p.OnHome = f.Get("on_home") == "on"
		p.RawHTML = f.Get("raw_html") == "on"
		t, e := time.Parse("2006-01-02T15:04", f.Get("published_at"))
		p.PublishedAt = t
		if e != nil || !p.Valid() {
			a.render(w, r, page{Mode: "edit", Title: a.text(r, "Редактирование"), Post: p, Message: "Проверьте адрес, язык, ссылки и дату публикации (UTC)."})
			return
		}
		if e = a.Store.SavePostEdit(r.Context(), &p); e != nil {
			a.render(w, r, page{Mode: "edit", Title: a.text(r, "Редактирование"), Post: p, Message: "Не удалось сохранить. Возможно, этот адрес и язык уже заняты."})
			return
		}
		if p.Deleted {
			http.Redirect(w, r, "/godmode/posts/"+p.ID+"/", http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, p.URL(), 303)
		return
	}
	a.render(w, r, page{Mode: "edit", Title: a.text(r, "Редактирование"), Post: p})
}
func (a *Server) export(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	posts := []Post{}
	for offset := 0; ; offset += 1000 {
		batch, e := a.Store.Posts(r.Context(), Filter{Admin: true, Limit: 1000, Offset: offset})
		if e != nil {
			a.fail(w, e)
			return
		}
		posts = append(posts, batch...)
		if len(batch) < 1000 {
			break
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="posts.json"`)
	_ = json.NewEncoder(w).Encode(posts)
}

func (a *Server) deletePost(w http.ResponseWriter, r *http.Request) {
	a.postTrash(w, r, true)
}

func (a *Server) restorePost(w http.ResponseWriter, r *http.Request) {
	a.postTrash(w, r, false)
}

func (a *Server) postTrash(w http.ResponseWriter, r *http.Request, deleted bool) {
	if !a.requireAdmin(w, r) {
		return
	}
	p, err := a.Store.PostID(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	if r.Method == "POST" {
		if err = a.Store.SetPostDeleted(r.Context(), p.ID, deleted); errors.Is(err, ErrNoRows) {
			http.NotFound(w, r)
			return
		} else if err != nil {
			a.fail(w, err)
			return
		}
		http.Redirect(w, r, "/godmode/", http.StatusSeeOther)
		return
	}
	if p.Deleted == deleted {
		http.Redirect(w, r, "/godmode/posts/"+p.ID+"/", http.StatusSeeOther)
		return
	}
	mode, title := "post-delete", "Удалить запись?"
	if !deleted {
		mode, title = "post-restore", "Восстановить запись?"
	}
	a.render(w, r, page{Mode: mode, Title: title, Post: p})
}
