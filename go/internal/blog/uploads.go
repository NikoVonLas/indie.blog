package blog

import (
	"bytes"
	"encoding/json"
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"regexp"
	"time"

	_ "golang.org/x/image/webp"
)

var mediaID = regexp.MustCompile(`^[a-f0-9]{64}$`)

func (a *Server) upload(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		http.Error(w, "Войдите, чтобы загрузить изображение", 401)
		return
	}
	purpose := r.URL.Query().Get("purpose")
	if purpose != "avatar" && !u.Admin {
		http.Error(w, "Недостаточно прав", 403)
		return
	}
	if a.limited(w, r, "upload:"+u.ID, 60, time.Hour) {
		return
	}
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		http.Error(w, "Размер изображения — не больше 10 МБ", 413)
		return
	}
	defer r.MultipartForm.RemoveAll()
	file, _, err := r.FormFile("image")
	if err != nil {
		http.Error(w, "Выберите изображение", 400)
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (10<<20)+1))
	if err != nil || len(data) > 10<<20 {
		http.Error(w, "Размер изображения — не больше 10 МБ", 413)
		return
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 24_000_000 {
		http.Error(w, "Нужен JPEG, PNG, GIF или WebP, не больше 24 мегапикселей", 400)
		return
	}
	mime := map[string]string{"jpeg": "image/jpeg", "png": "image/png", "gif": "image/gif", "webp": "image/webp"}[format]
	if mime == "" {
		http.Error(w, "Этот формат изображения не поддерживается", 400)
		return
	}
	if _, _, err = image.Decode(bytes.NewReader(data)); err != nil {
		http.Error(w, "Не удалось прочитать изображение", 400)
		return
	}
	id := randomToken()
	if _, err = a.Store.DB.Exec(r.Context(), `INSERT INTO uploads(id,user_id,content_type,body) VALUES($1,$2,$3,$4)`, id, u.ID, mime, data); err != nil {
		a.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"url": "/media/" + id, "width": cfg.Width, "height": cfg.Height})
}

func (a *Server) media(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !mediaID.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	var mime string
	var data []byte
	err := a.Store.DB.QueryRow(r.Context(), `SELECT content_type,body FROM uploads WHERE id=$1`, id).Scan(&mime, &data)
	if errors.Is(err, ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("ETag", `"`+id+`"`)
	if r.Header.Get("If-None-Match") == `"`+id+`"` {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(data)
}
