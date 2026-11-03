package blog

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestPostTrashEditorFlow(t *testing.T) {
	for _, kind := range []string{"blog", "notes"} {
		t.Run(kind, func(t *testing.T) {
			a := testApp(t)
			ctx := context.Background()
			p := seedPost(t, a, "trash-"+kind, true, false, time.Now().Add(-time.Hour).Truncate(time.Minute))
			p.Kind = kind
			if err := a.Store.SavePost(ctx, &p); err != nil {
				t.Fatal(err)
			}
			if _, err := a.Store.DB.Exec(ctx, `INSERT INTO comments(id,post_id,author_name,body) VALUES('keep-comment',$1,'Reader','Keep this comment')`, p.ID); err != nil {
				t.Fatal(err)
			}
			owner, member, anon := newBrowser(a), newBrowser(a), newBrowser(a)
			owner.login(t, "owner")
			member.login(t, "member")
			base := "/godmode/posts/" + p.ID + "/"
			if w := owner.request("GET", base, nil); w.Code != 200 || !strings.Contains(w.Body.String(), base+"delete/") {
				t.Fatal("editor has no delete action", w.Code)
			}
			if w := owner.request("GET", "/godmode/posts/new/", nil); strings.Contains(w.Body.String(), ">Удалить запись</a>") {
				t.Fatal("unsaved post offers deletion")
			}
			paths := []string{"/", "/all/", "/" + kind + "/", "/search/?q=" + p.Slug, "/rss/", "/feed.json", "/sitemap.xml", "/llms.txt"}
			for _, path := range append(paths, p.URL(), p.MarkdownURL()) {
				cachedGet(t, anon, path)
				cachedGet(t, anon, path)
			}
			if w := owner.request("GET", base+"delete/", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "Переместить в корзину") {
				t.Fatal("no confirmation page", w.Code)
			}
			for _, action := range []string{"delete/", "restore/"} {
				for _, method := range []string{"GET", "POST"} {
					if w := member.request(method, base+action, nil); w.Code != 403 {
						t.Fatal("member can manage trash", method, action, w.Code)
					}
					if w := anon.request(method, base+action, nil); w.Code != 303 || !strings.HasPrefix(w.Header().Get("Location"), "/admin?") {
						t.Fatal("anonymous trash access", method, action, w.Code)
					}
				}
				if w := owner.request("POST", base+action, url.Values{"csrf": {"invalid"}}); w.Code != 403 {
					t.Fatal("trash action bypasses CSRF", action, w.Code)
				}
			}
			before, err := a.Store.PostID(ctx, p.ID)
			if err != nil || before.Deleted {
				t.Fatal("confirmation GET or denied request changed the post", err)
			}
			if w := owner.request("POST", base+"delete/", nil); w.Code != 303 || w.Header().Get("Location") != "/godmode/" {
				t.Fatal("delete failed", w.Code, w.Body.String())
			}
			deleted, err := a.Store.PostID(ctx, p.ID)
			if err != nil || !deleted.Deleted || deleted.Body != before.Body || deleted.Visible != before.Visible {
				t.Fatal("delete destroyed source or publication settings", err)
			}
			for _, path := range []string{p.URL(), p.MarkdownURL()} {
				if w := anon.request("GET", path, nil); w.Code != 404 {
					t.Fatal("deleted page is still accessible", path, w.Code)
				}
			}
			for _, path := range paths {
				if w := anon.request("GET", path, nil); strings.Contains(w.Body.String(), "/"+kind+"/"+p.Slug+"/") {
					t.Fatal("deleted post remains cached in listing", path)
				}
			}
			var comments int
			if err := a.Store.DB.QueryRow(ctx, `SELECT count(*) FROM comments WHERE post_id=$1 AND body='Keep this comment'`, p.ID).Scan(&comments); err != nil || comments != 1 {
				t.Fatal("delete lost comments", comments, err)
			}
			for _, path := range []string{"/godmode/", base} {
				if w := owner.request("GET", path, nil); w.Code != 200 || !strings.Contains(w.Body.String(), base+"restore/") {
					t.Fatal("no restore action", path, w.Code)
				}
			}
			// A repeated request must not write another revision or flush other pages.
			cachedGet(t, anon, "/")
			if w := owner.request("POST", base+"delete/", nil); w.Code != 303 {
				t.Fatal("repeated delete failed", w.Code)
			}
			if w := cachedGet(t, anon, "/"); w.Header().Get("X-Blog-Cache") != "HIT" {
				t.Fatal("repeated delete invalidated content")
			}
			// Simulate saving an editor snapshot loaded before deletion.
			before.Body = "Updated while in trash"
			if err := a.Store.SavePostEdit(ctx, &before); err != nil || !before.Deleted {
				t.Fatal("stale edit resurrected post", err)
			}
			before.Body = "Updated from the browser editor while in trash"
			form := url.Values{"title": {p.Title}, "slug": {p.Slug}, "kind": {p.Kind}, "lang": {p.Lang}, "body": {before.Body}, "author": {p.Author}, "published_at": {before.PublishedAt.UTC().Format("2006-01-02T15:04")}, "visible": {"on"}, "commentable": {"on"}, "on_home": {"on"}}
			if w := owner.request("POST", base, form); w.Code != 303 || w.Header().Get("Location") != base {
				t.Fatal("saving trashed post did not return to its editor", w.Code)
			}
			if w := owner.request("GET", base+"restore/", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "Восстановить запись") {
				t.Fatal("restore confirmation failed", w.Code)
			}
			if unchanged, _ := a.Store.PostID(ctx, p.ID); !unchanged.Deleted {
				t.Fatal("restore GET changed state")
			}
			if w := owner.request("POST", base+"restore/", nil); w.Code != 303 {
				t.Fatal("restore failed", w.Code, w.Body.String())
			}
			restored, err := a.Store.PostID(ctx, p.ID)
			if err != nil || restored.Deleted || restored.Body != before.Body || restored.Visible != p.Visible || restored.MembersOnly != p.MembersOnly || !restored.PublishedAt.Equal(before.PublishedAt) {
				t.Fatal("restore changed source or visibility", err)
			}
			for _, path := range []string{p.URL(), p.MarkdownURL()} {
				if w := cachedGet(t, anon, path); !strings.Contains(w.Body.String(), before.Body) {
					t.Fatal("restored content missing", path)
				}
			}
			if _, err := a.Store.PostID(ctx, "missing"); !errors.Is(err, ErrNoRows) {
				t.Fatal(err)
			}
			if w := owner.request("POST", "/godmode/posts/missing/delete/", nil); w.Code != 404 {
				t.Fatal("missing post", w.Code)
			}
		})
	}
}

func TestPostTrashRestorationPreservesAccess(t *testing.T) {
	a := testApp(t)
	owner, anon := newBrowser(a), newBrowser(a)
	owner.login(t, "owner")
	for _, state := range []struct {
		name             string
		visible, private bool
		at               time.Time
	}{
		{"draft", false, false, time.Now().Add(-time.Hour)},
		{"private", true, true, time.Now().Add(-time.Hour)},
		{"scheduled", true, false, time.Now().Add(time.Hour)},
	} {
		p := seedPost(t, a, state.name, state.visible, state.private, state.at)
		p, err := a.Store.PostID(context.Background(), p.ID)
		if err != nil {
			t.Fatal(err)
		}
		base := "/godmode/posts/" + p.ID + "/"
		for _, action := range []string{"delete/", "restore/"} {
			if w := owner.request("POST", base+action, nil); w.Code != 303 {
				t.Fatal(state.name, action, w.Code)
			}
		}
		if w := anon.request("GET", p.URL(), nil); strings.Contains(w.Body.String(), p.Body) {
			t.Fatal("restoration exposed protected content", state.name)
		}
		if restored, err := a.Store.PostID(context.Background(), p.ID); err != nil || restored.Deleted || restored.Visible != p.Visible || restored.MembersOnly != p.MembersOnly || !restored.PublishedAt.Equal(p.PublishedAt) {
			t.Fatal("access settings changed", state.name, err)
		}
	}
}
