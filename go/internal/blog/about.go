package blog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"
)

type AboutLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
	Me    bool   `json:"me"`
}
type AboutCard struct {
	Name        string      `json:"name"`
	Nickname    string      `json:"nickname"`
	Description string      `json:"description"`
	Avatar      string      `json:"avatar"`
	Links       []AboutLink `json:"links"`
}

func defaultAboutCard() AboutCard {
	return AboutCard{
		Name: "Денис Николас", Nickname: "NikoVonLas",
		Description: "Делаю инструменты, которые можно запустить у себя: [платформу для Telegram-ботов](https://github.com/NikoVonLas/tg-bot-platform), [панель управления Jitsi](https://github.com/NikoVonLas/jitsi-admin-panel). Дорабатываю [Apple Notes MCP](https://github.com/NikoVonLas/apple-notes-mcp) для работы с заметками через ИИ.\n\nЗдесь собираю статьи, короткие заметки и опыт работы над своими проектами. Обживаю собственный уголок интернета — с постоянными ссылками, открытыми форматами и IndieWeb.",
		Links:       []AboutLink{{"GitHub", "https://github.com/NikoVonLas", true}, {"Telegram", "https://t.me/nikovonlas", true}, {"RSS блога", "/rss/", false}},
	}
}
func (s *Store) About(ctx context.Context) (AboutCard, string, error) {
	var raw string
	err := s.DB.QueryRow(ctx, `SELECT value FROM settings WHERE key='about_card'`).Scan(&raw)
	if errors.Is(err, ErrNoRows) {
		return defaultAboutCard(), "", nil
	}
	if err != nil {
		return AboutCard{}, "", err
	}
	var card AboutCard
	err = json.Unmarshal([]byte(raw), &card)
	return card, raw, err
}
func (s *Store) SaveAbout(ctx context.Context, card AboutCard, previous string) (bool, error) {
	data, err := json.Marshal(card)
	if err != nil {
		return false, err
	}
	result, err := s.DB.Exec(ctx, `INSERT INTO settings(key,value) VALUES('about_card',$1) ON CONFLICT(key) DO UPDATE SET value=excluded.value WHERE settings.value=$2`, string(data), previous)
	return err == nil && result.RowsAffected() == 1, err
}
func (c AboutCard) validate() string {
	if strings.TrimSpace(c.Name) == "" || utf8.RuneCountInString(c.Name) > 128 || utf8.RuneCountInString(c.Nickname) > 128 {
		return "Укажите имя до 128 символов; ник тоже должен быть не длиннее 128 символов."
	}
	if utf8.RuneCountInString(c.Description) > 10000 {
		return "Описание должно быть не длиннее 10 000 символов."
	}
	if len(c.Avatar) > 2048 || !safeURL(c.Avatar) {
		return "Проверьте адрес фотографии: нужен http/https или путь на этом сайте."
	}
	if len(c.Links) > 12 {
		return "Можно добавить не больше 12 ссылок."
	}
	for i, link := range c.Links {
		if strings.TrimSpace(link.Label) == "" || utf8.RuneCountInString(link.Label) > 80 || link.URL == "" || len(link.URL) > 2048 || !safeURL(link.URL) {
			return fmt.Sprintf("Проверьте ссылку №%d: нужны название до 80 символов и адрес http/https или путь на сайте.", i+1)
		}
	}
	return ""
}
func (a *Server) editAbout(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	card, previous, err := a.Store.About(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	revision := hash(previous)
	message := ""
	if r.Method == "POST" {
		f := r.PostForm
		card = AboutCard{Name: strings.TrimSpace(f.Get("name")), Nickname: strings.TrimSpace(f.Get("nickname")), Description: strings.TrimSpace(f.Get("description")), Avatar: strings.TrimSpace(f.Get("avatar"))}
		labels, urls, profiles := f["link_label"], f["link_url"], f["link_me"]
		if len(labels) != len(urls) || len(labels) != len(profiles) {
			http.Error(w, "Некорректные поля ссылок", 400)
			return
		}
		for i, label := range labels {
			label = strings.TrimSpace(label)
			address := strings.TrimSpace(urls[i])
			if label == "" && address == "" {
				continue
			}
			card.Links = append(card.Links, AboutLink{label, address, profiles[i] == "yes"})
		}
		message = card.validate()
		if message == "" {
			if f.Get("revision") != revision {
				message = "Карточка уже изменена в другой вкладке. Скопируйте свои правки и обновите страницу перед сохранением."
			} else {
				saved, err := a.Store.SaveAbout(r.Context(), card, previous)
				if err != nil {
					a.fail(w, err)
					return
				}
				if saved {
					http.Redirect(w, r, "/#about-heading", http.StatusSeeOther)
					return
				}
				message = "Карточка уже изменена в другой вкладке. Скопируйте свои правки и обновите страницу перед сохранением."
			}
		}
		// Keep the submitted revision: a second save must not silently overwrite newer changes.
		revision = f.Get("revision")
	}
	a.render(w, r, page{Mode: "about-edit", Title: "Карточка «Обо мне»", About: card, AboutRevision: revision, Message: message})
}
