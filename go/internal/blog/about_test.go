package blog

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

func TestAboutCardEditing(t *testing.T) {
	a := testApp(t)
	anon := newBrowser(a)
	owner := newBrowser(a)
	owner.login(t, "owner")
	member := newBrowser(a)
	member.login(t, "member")
	if w := anon.request("GET", "/godmode/about/", nil); w.Code != 303 {
		t.Fatal("anonymous editor", w.Code)
	}
	if w := member.request("GET", "/godmode/about/", nil); w.Code != 403 {
		t.Fatal("member editor", w.Code)
	}
	initial := anon.request("GET", "/", nil).Body.String()
	if !strings.Contains(initial, "Денис Николас") || strings.Contains(initial, "Редактировать карточку") {
		t.Fatal("default card / private edit link")
	}
	if w := owner.request("GET", "/godmode/about/", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "NikoVonLas") {
		t.Fatal("editor default", w.Code)
	}
	form := url.Values{"name": {"New Author"}, "nickname": {"new-nick"}, "description": {"Hello **world**.\n\n<script>alert(1)</script>"}, "avatar": {"/media/test-photo"}, "link_label": {"My profile", ""}, "link_url": {"https://example.org/me", ""}, "link_me": {"yes", "no"}, "revision": {hash("")}}
	invalidCSRF := form.Get("csrf")
	form.Set("csrf", "invalid")
	if w := owner.request("POST", "/godmode/about/", form); w.Code != 403 {
		t.Fatal("CSRF bypass", w.Code)
	}
	form.Set("csrf", invalidCSRF)
	if w := member.request("POST", "/godmode/about/", form); w.Code != 403 {
		t.Fatal("member write", w.Code)
	}
	form.Del("csrf")
	if w := owner.request("POST", "/godmode/about/", form); w.Code != 303 || w.Header().Get("Location") != "/#about-heading" {
		t.Fatal("save", w.Code, w.Body.String())
	}
	// A fresh server must read persisted settings without a restart-dependent cache.
	next, err := NewServer(context.Background(), a.Store, a.Config)
	if err != nil {
		t.Fatal(err)
	}
	page := newBrowser(next).request("GET", "/", nil).Body.String()
	for _, want := range []string{"New Author", "new-nick", "<strong>world</strong>", `class="u-photo about-photo"`, `href="https://example.org/me" rel="me"`} {
		if !strings.Contains(page, want) {
			t.Error("missing", want)
		}
	}
	if strings.Contains(page, "<script>alert(1)") || strings.Contains(page, "Денис Николас") {
		t.Fatal("unsafe or stale card")
	}
	card, raw, err := a.Store.About(context.Background())
	if err != nil || len(card.Links) != 1 {
		t.Fatal(card, err)
	}
	form.Set("name", "Stale Author")
	w := owner.request("POST", "/godmode/about/", form)
	if !strings.Contains(w.Body.String(), "другой вкладке") || !strings.Contains(w.Body.String(), "Stale Author") {
		t.Fatal("stale save not preserved/rejected")
	}
	if saved, err := a.Store.SaveAbout(context.Background(), card, ""); err != nil || saved {
		t.Fatal("concurrent save overwrites", err)
	}
	form.Set("revision", hash(raw))
	form["link_url"][0] = "javascript:alert(1)"
	w = owner.request("POST", "/godmode/about/", form)
	if !strings.Contains(w.Body.String(), "Проверьте ссылку") {
		t.Fatal("unsafe URL accepted")
	}
	form["link_url"][0] = "https://example.org/"
	form.Set("avatar", "data:image/svg+xml,unsafe")
	if w = owner.request("POST", "/godmode/about/", form); !strings.Contains(w.Body.String(), "адрес фотографии") {
		t.Fatal("unsafe avatar accepted")
	}
	// Empty optional fields remove the photo, nickname and links rather than restoring defaults.
	form.Set("avatar", "")
	form.Set("nickname", "")
	form.Set("description", "")
	form["link_label"] = []string{""}
	form["link_url"] = []string{""}
	form["link_me"] = []string{"no"}
	if w = owner.request("POST", "/godmode/about/", form); w.Code != 303 {
		t.Fatal("clear optional fields", w.Body.String())
	}
	card, _, _ = a.Store.About(context.Background())
	if card.Avatar != "" || card.Nickname != "" || len(card.Links) != 0 {
		t.Fatal("optional fields not cleared")
	}
}
