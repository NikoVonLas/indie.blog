package blog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"image"
	"io"
	"mime"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/microcosm-cc/bluemonday"
)

type mpProperties map[string][]any
type mpRequest struct {
	Type       []string        `json:"type"`
	Properties mpProperties    `json:"properties"`
	Action     string          `json:"action"`
	URL        string          `json:"url"`
	Replace    mpProperties    `json:"replace"`
	Add        mpProperties    `json:"add"`
	Delete     json.RawMessage `json:"delete"`
}
type mpStored struct {
	Properties mpProperties `json:"properties"`
	Body       string       `json:"body"`
	RawHTML    bool         `json:"raw_html"`
}

var mpKnown = []string{"name", "summary", "content", "published", "category", "photo", "featured", "in-reply-to", "like-of", "repost-of", "bookmark-of", "syndication", "post-status", "visibility"}

func mpString(p mpProperties, k string) string {
	if len(p[k]) == 0 {
		return ""
	}
	s, _ := p[k][0].(string)
	return s
}
func mpData(p Post) mpStored {
	var data map[string]json.RawMessage
	_ = json.Unmarshal(p.Data, &data)
	var m mpStored
	_ = json.Unmarshal(data["micropub"], &m)
	return m
}
func (a *Server) mpSource(p Post) mpProperties {
	stored := mpData(p)
	props := stored.Properties
	if props == nil {
		props = mpProperties{}
	}
	props["syndication"] = p.syndicationLinks(props)
	props["name"] = []any{p.Title}
	props["summary"] = []any{p.Summary}
	props["published"] = []any{p.PublishedAt.Format(time.RFC3339)}
	props["updated"] = []any{p.UpdatedAt.Format(time.RFC3339)}
	props["post-status"] = []any{"draft"}
	if p.Visible {
		props["post-status"] = []any{"published"}
	}
	props["visibility"] = []any{"public"}
	if p.MembersOnly {
		props["visibility"] = []any{"private"}
	}
	props["featured"] = []any{}
	if p.Image != "" {
		props["featured"] = []any{p.Image}
	}
	if stored.Body != p.Body || stored.RawHTML != p.RawHTML || len(props["content"]) == 0 {
		// Normal-editor Markdown is returned as rendered HTML, so clients do not
		// mistake Markdown syntax for plain text and accidentally destroy formatting.
		props["content"] = []any{map[string]any{"html": string(a.md.Render(p.Body, p.RawHTML))}}
	}
	return props
}
func mpPhoto(v any) (string, string) {
	if s, ok := v.(string); ok {
		return s, ""
	}
	m, _ := v.(map[string]any)
	s, _ := m["value"].(string)
	alt, _ := m["alt"].(string)
	return s, alt
}
func mpURL(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && u.User == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && len(raw) <= 2048
}
func validateMP(props mpProperties) error {
	for k, vs := range props {
		if !slices.Contains(mpKnown, k) {
			continue
		}
		if len(vs) > 100 {
			return fmt.Errorf("Too many values: %s", k)
		}
		if slices.Contains([]string{"name", "summary", "content", "published", "featured", "post-status", "visibility"}, k) && len(vs) > 1 {
			return fmt.Errorf("Property accepts one value: %s", k)
		}
		for _, v := range vs {
			s, isString := v.(string)
			switch k {
			case "content":
				if !isString {
					m, ok := v.(map[string]any)
					if !ok {
						return errors.New("Invalid content")
					}
					_, ok = m["html"].(string)
					if !ok {
						return errors.New("Expected content.html")
					}
				}
			case "photo":
				u, alt := mpPhoto(v)
				if !mpURL(u) || len(alt) > 4096 {
					return errors.New("Invalid photo URL or alt text")
				}
			case "featured", "in-reply-to", "like-of", "repost-of", "bookmark-of", "syndication":
				if !isString || !mpURL(s) {
					return fmt.Errorf("Invalid URL: %s", k)
				}
			default:
				if !isString || len(s) > 10000 {
					return fmt.Errorf("Invalid property: %s", k)
				}
			}
		}
	}
	return nil
}
func (a *Server) applyMP(p *Post, props mpProperties, changed map[string]bool) error {
	if e := validateMP(props); e != nil {
		return e
	}
	if changed["name"] {
		p.Title = mpString(props, "name")
	}
	if changed["summary"] {
		p.Summary = mpString(props, "summary")
		p.Subtitle = p.Summary
	}
	if changed["featured"] {
		p.Image = mpString(props, "featured")
	}
	if changed["published"] && len(props["published"]) > 0 {
		d, e := time.Parse(time.RFC3339, mpString(props, "published"))
		if e != nil {
			return errors.New("published must be RFC3339")
		}
		p.PublishedAt = d
	}
	if changed["post-status"] {
		switch mpString(props, "post-status") {
		case "published", "":
			p.Visible = true
		case "draft":
			p.Visible = false
		default:
			return errors.New("post-status must be draft or published")
		}
	}
	if changed["visibility"] {
		switch mpString(props, "visibility") {
		case "public", "":
			p.MembersOnly = false
		case "private":
			p.MembersOnly = true
		default:
			return errors.New("visibility must be public or private")
		}
	}
	if changed["content"] {
		p.Body = ""
		p.RawHTML = true
		if len(props["content"]) > 0 {
			if s, ok := props["content"][0].(string); ok {
				p.Body = `<p dir="auto">` + strings.ReplaceAll(html.EscapeString(s), "\n", "<br>\n") + "</p>"
			} else {
				v := props["content"][0].(map[string]any)["html"].(string)
				p.Body = bluemonday.UGCPolicy().Sanitize(v)
				props["content"] = []any{map[string]any{"html": p.Body}}
			}
		}
	}
	var data map[string]json.RawMessage
	_ = json.Unmarshal(p.Data, &data)
	if data == nil {
		data = map[string]json.RawMessage{}
	}
	stored := mpData(*p)
	// On non-content updates retain the original plain-text/HTML representation
	// only while it still describes the saved body.
	if !changed["content"] && stored.Body == p.Body && stored.RawHTML == p.RawHTML && len(stored.Properties["content"]) > 0 {
		props["content"] = stored.Properties["content"]
	}
	for k := range props {
		if !slices.Contains(mpKnown, k) {
			delete(props, k)
		}
	}
	data["micropub"], _ = json.Marshal(mpStored{props, p.Body, p.RawHTML})
	p.Data, _ = json.Marshal(data)
	if !p.Valid() {
		return errors.New("Invalid or oversized post")
	}
	return nil
}
func (p Post) micropubExtras() string {
	props := mpData(p).Properties
	if props == nil {
		props = mpProperties{}
	}
	props["syndication"] = p.syndicationLinks(props)
	var out strings.Builder
	for _, rel := range []struct{ key, label string }{{"in-reply-to", "Ответ на"}, {"like-of", "Нравится"}, {"repost-of", "Репост"}, {"bookmark-of", "Закладка"}, {"syndication", "Также опубликовано"}} {
		for _, v := range props[rel.key] {
			s, ok := v.(string)
			if !ok || !mpURL(s) {
				continue
			}
			fmt.Fprintf(&out, `<p>%s: <a class="u-%s" href="%s" rel="nofollow noopener">%s</a></p>`, rel.label, rel.key, html.EscapeString(s), html.EscapeString(s))
		}
	}
	for _, v := range props["photo"] {
		u, alt := mpPhoto(v)
		if mpURL(u) {
			fmt.Fprintf(&out, `<figure><img class="u-photo" src="%s" alt="%s" loading="lazy">`, html.EscapeString(u), html.EscapeString(alt))
			if alt != "" {
				fmt.Fprintf(&out, "<figcaption>%s</figcaption>", html.EscapeString(alt))
			}
			out.WriteString("</figure>")
		}
	}
	if len(props["category"]) > 0 {
		out.WriteString(`<p class="hint">`)
		for _, v := range props["category"] {
			if s, ok := v.(string); ok {
				fmt.Fprintf(&out, `<span class="p-category">#%s</span> `, html.EscapeString(s))
			}
		}
		out.WriteString("</p>")
	}
	return out.String()
}
func (m *Markdown) RenderContent(p Post) template.HTML {
	return template.HTML(string(m.Render(p.Body, p.RawHTML)) + p.micropubExtras())
}

func (a *Server) micropubIdentity(w http.ResponseWriter, r *http.Request) (IndieToken, bool) {
	raw := bearer(r)
	// Query-string tokens are intentionally never accepted. Form tokens are
	// supported for clients that cannot set Authorization headers.
	if r.Method == "POST" {
		ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if ct == "application/x-www-form-urlencoded" || ct == "multipart/form-data" {
			var e error
			if ct == "multipart/form-data" {
				e = r.ParseMultipartForm(1 << 20)
			} else {
				e = r.ParseForm()
			}
			if e != nil {
				protocolError(w, 400, "invalid_request", "Cannot parse request")
				return IndieToken{}, false
			}
			f := r.PostForm.Get("access_token")
			if raw != "" && f != "" {
				protocolError(w, 400, "invalid_request", "Use one token transport")
				return IndieToken{}, false
			}
			if raw == "" {
				raw = f
			}
		}
	}
	t, e := a.accessToken(r.Context(), raw)
	if e != nil {
		protocolError(w, 401, "invalid_token", "Valid access token required")
		return t, false
	}
	if a.limited(w, r, "micropub:"+t.ID, 120, time.Minute) {
		return t, false
	}
	return t, true
}
func requireScope(w http.ResponseWriter, t IndieToken, s string) bool {
	if !t.Has(s) {
		protocolError(w, 403, "insufficient_scope", "Required scope: "+s)
		return false
	}
	return true
}
func parseMP(r *http.Request) (mpRequest, error) {
	var p mpRequest
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch ct {
	case "application/json":
		if e := decodeProtocolJSON(r.Body, &p); e != nil {
			return p, e
		}
	case "application/x-www-form-urlencoded", "multipart/form-data":
		p.Action = r.PostForm.Get("action")
		p.URL = r.PostForm.Get("url")
		p.Properties = mpProperties{}
		p.Type = []string{"h-entry"}
		if h := r.PostForm.Get("h"); h != "" {
			p.Type = []string{"h-" + h}
		}
		if p.Action == "update" {
			return p, errors.New("Updates require JSON")
		}
		for k, vs := range r.PostForm {
			k = strings.TrimSuffix(k, "[]")
			if k == "h" || k == "action" || k == "url" || k == "access_token" {
				continue
			}
			for _, v := range vs {
				p.Properties[k] = append(p.Properties[k], v)
			}
		}
		if r.MultipartForm != nil && len(r.MultipartForm.File) > 0 {
			return p, errors.New("Upload files to the media-endpoint first")
		}
	default:
		return p, errors.New("Use JSON, form-urlencoded or multipart")
	}
	if len(p.Type) == 0 {
		p.Type = []string{"h-entry"}
	}
	if p.Action == "" && (len(p.Type) != 1 || p.Type[0] != "h-entry") {
		return p, errors.New("Only h-entry is supported")
	}
	if p.Action == "" && p.Properties == nil {
		return p, errors.New("Missing properties")
	}
	return p, nil
}
func (a *Server) mpPost(ctx context.Context, raw string) (Post, error) {
	u, e := url.Parse(raw)
	base, _ := url.Parse(a.Config.BaseURL)
	if e != nil || u.User != nil || !sameOrigin(u, base) || u.Fragment != "" {
		return Post{}, ErrNoRows
	}
	q, e := url.ParseQuery(u.RawQuery)
	if e != nil {
		return Post{}, ErrNoRows
	}
	for k := range q {
		if k != "lang" {
			return Post{}, ErrNoRows
		}
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 3 && parts[2] == "index.html" {
		parts = parts[:2]
	}
	if len(parts) != 2 {
		return Post{}, ErrNoRows
	}
	lang := q.Get("lang")
	if lang == "" {
		lang = a.Config.Language
	}
	p, e := a.Store.Post(ctx, parts[1], lang)
	if e != nil || p.Kind != parts[0] {
		return Post{}, ErrNoRows
	}
	return p, nil
}
func (a *Server) micropub(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	t, ok := a.micropubIdentity(w, r)
	if !ok {
		return
	}
	if r.Method == "GET" {
		a.micropubQuery(w, r, t)
		return
	}
	input, e := parseMP(r)
	if e != nil {
		protocolError(w, 400, "invalid_request", e.Error())
		return
	}
	action := input.Action
	if action == "" {
		action = "create"
	}
	if !slices.Contains([]string{"create", "update", "delete", "undelete"}, action) {
		protocolError(w, 400, "invalid_request", "Unknown action")
		return
	}
	if !requireScope(w, t, action) {
		return
	}
	if action == "create" {
		p := Post{Kind: "notes", Lang: a.Config.Language, Slug: time.Now().UTC().Format("20060102-150405") + "-" + randomToken()[:8], Visible: true, Commentable: true, OnHome: true, Author: a.Config.Author, PublishedAt: time.Now().UTC()}
		if mpString(input.Properties, "name") != "" {
			p.Kind = "blog"
		}
		if slug := mpString(input.Properties, "mp-slug"); slug != "" {
			p.Slug = slug
		}
		requested := input.Properties["mp-syndicate-to"]
		delete(input.Properties, "mp-syndicate-to")
		if len(requested) > 0 {
			if a.posseReady() != "" {
				protocolError(w, 400, "invalid_request", "No syndication target connected")
				return
			}
			for _, v := range requested {
				s, ok := v.(string)
				if !ok || s != a.telegramTarget().UID {
					protocolError(w, 400, "invalid_request", "Unknown syndication target")
					return
				}
			}
		}
		changed := map[string]bool{}
		for k := range input.Properties {
			changed[k] = true
		}
		if e = a.applyMP(&p, input.Properties, changed); e != nil {
			protocolError(w, 400, "invalid_request", e.Error())
			return
		}
		if p.Body == "" && p.Title == "" && p.Summary == "" && len(p.micropubExtras()) == 0 {
			protocolError(w, 400, "invalid_request", "Empty entry")
			return
		}
		if len(requested) > 0 && !possePublic(p) {
			protocolError(w, 400, "invalid_request", "Syndication requires an already public post")
			return
		}
		if e = a.saveMicropubPOSSE(r.Context(), &p, requested); e != nil {
			a.mpSaveError(w, e)
			return
		}
		w.Header().Set("Location", a.Config.BaseURL+p.URL())
		w.WriteHeader(201)
		return
	}
	p, e := a.mpPost(r.Context(), input.URL)
	if e != nil {
		protocolError(w, 400, "invalid_request", "Unknown post URL")
		return
	}
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
	switch action {
	case "delete":
		p.Deleted = true
	case "undelete":
		p.Deleted = false
	case "update":
		if p.Deleted {
			protocolError(w, 400, "invalid_request", "Restore the deleted post before editing")
			return
		}
		props := a.mpSource(p)
		changed := map[string]bool{}
		if input.Replace == nil && input.Add == nil && len(input.Delete) == 0 {
			protocolError(w, 400, "invalid_request", "Missing update operations")
			return
		}
		for k, v := range input.Replace {
			if slices.Contains(mpKnown, k) {
				props[k] = v
				changed[k] = true
			}
		}
		for k, vs := range input.Add {
			if slices.Contains(mpKnown, k) {
				for _, v := range vs {
					if !slices.ContainsFunc(props[k], func(x any) bool { return reflect.DeepEqual(x, v) }) {
						props[k] = append(props[k], v)
					}
				}
				changed[k] = true
			}
		}
		if len(input.Delete) > 0 {
			var names []string
			var values mpProperties
			if json.Unmarshal(input.Delete, &names) == nil {
				for _, k := range names {
					if slices.Contains(mpKnown, k) {
						delete(props, k)
						changed[k] = true
					}
				}
			} else if json.Unmarshal(input.Delete, &values) == nil {
				for k, vs := range values {
					if slices.Contains(mpKnown, k) {
						props[k] = slices.DeleteFunc(props[k], func(x any) bool { return slices.ContainsFunc(vs, func(v any) bool { return reflect.DeepEqual(x, v) }) })
						changed[k] = true
					}
				}
			} else {
				protocolError(w, 400, "invalid_request", "Invalid delete operation")
				return
			}
		}
		if e = a.applyMP(&p, props, changed); e != nil {
			protocolError(w, 400, "invalid_request", e.Error())
			return
		}
	}
	if e = savePost(r.Context(), tx, &p); e != nil {
		a.mpSaveError(w, e)
		return
	}
	if e = tx.Commit(r.Context()); e != nil {
		a.fail(w, e)
		return
	}
	w.WriteHeader(204)
}
func (a *Server) mpSaveError(w http.ResponseWriter, e error) {
	if errors.Is(e, ErrConflict) {
		protocolError(w, 400, "invalid_request", "This slug is already in use")
		return
	}
	a.fail(w, e)
}
func (a *Server) micropubQuery(w http.ResponseWriter, r *http.Request, t IndieToken) {
	switch r.URL.Query().Get("q") {
	case "config":
		protocolJSON(w, 200, map[string]any{"media-endpoint": a.Config.BaseURL + "/micropub/media", "syndicate-to": a.syndicationTargets(), "post-types": []any{map[string]string{"type": "note", "name": "Заметка"}, map[string]string{"type": "article", "name": "Статья"}, map[string]string{"type": "photo", "name": "Фото"}, map[string]string{"type": "reply", "name": "Ответ"}, map[string]string{"type": "like", "name": "Нравится"}, map[string]string{"type": "repost", "name": "Репост"}, map[string]string{"type": "bookmark", "name": "Закладка"}}})
	case "syndicate-to":
		protocolJSON(w, 200, map[string]any{"syndicate-to": a.syndicationTargets()})
	case "source":
		if !t.Has("read") && !t.Has("update") {
			protocolError(w, 403, "insufficient_scope", "Source queries require read or update")
			return
		}
		p, e := a.mpPost(r.Context(), r.URL.Query().Get("url"))
		if e != nil || p.Deleted {
			protocolError(w, 400, "invalid_request", "Unknown post URL")
			return
		}
		props := a.mpSource(p)
		result := map[string]any{"type": []string{"h-entry"}, "properties": props}
		keys := append(r.URL.Query()["properties[]"], r.URL.Query()["properties"]...)
		if len(keys) > 0 {
			chosen := mpProperties{}
			for _, k := range keys {
				if vs, ok := props[k]; ok {
					chosen[k] = vs
				}
			}
			result = map[string]any{"properties": chosen}
		}
		protocolJSON(w, 200, result)
	default:
		protocolError(w, 400, "invalid_request", "Unknown query")
	}
}
func storeMPImage(ctx context.Context, db executor, userID string, r io.Reader) (string, error) {
	data, e := io.ReadAll(io.LimitReader(r, (10<<20)+1))
	if e != nil || len(data) > 10<<20 {
		return "", errors.New("Image limit is 10 MiB")
	}
	cfg, format, e := image.DecodeConfig(bytes.NewReader(data))
	if e != nil || cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 24_000_000 {
		return "", errors.New("Expected JPEG, PNG, GIF or WebP up to 24 megapixels")
	}
	ct := map[string]string{"jpeg": "image/jpeg", "png": "image/png", "gif": "image/gif", "webp": "image/webp"}[format]
	if ct == "" {
		return "", errors.New("Unsupported image format")
	}
	if _, _, e = image.Decode(bytes.NewReader(data)); e != nil {
		return "", errors.New("Invalid image")
	}
	id := randomToken()
	_, e = db.Exec(ctx, `INSERT INTO uploads(id,user_id,content_type,body) VALUES($1,$2,$3,$4)`, id, userID, ct, data)
	return id, e
}
func (a *Server) micropubMedia(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	t, ok := a.micropubIdentity(w, r)
	if !ok {
		return
	}
	if !t.Has("media") && !t.Has("create") {
		protocolError(w, 403, "insufficient_scope", "Media upload requires media or create")
		return
	}
	if a.limited(w, r, "micropub-media:"+t.UserID, 60, time.Hour) {
		return
	}
	if e := r.ParseMultipartForm(1 << 20); e != nil {
		protocolError(w, 400, "invalid_request", "Expected multipart file")
		return
	}
	if len(r.MultipartForm.File) != 1 || len(r.MultipartForm.File["file"]) != 1 {
		protocolError(w, 400, "invalid_request", "Send exactly one file")
		return
	}
	f, _, e := r.FormFile("file")
	if e != nil {
		protocolError(w, 400, "invalid_request", "Missing file")
		return
	}
	defer f.Close()
	id, e := storeMPImage(r.Context(), a.Store.DB, t.UserID, f)
	if e != nil {
		protocolError(w, 400, "invalid_request", e.Error())
		return
	}
	w.Header().Set("Location", a.Config.BaseURL+"/media/"+id)
	w.WriteHeader(201)
}
