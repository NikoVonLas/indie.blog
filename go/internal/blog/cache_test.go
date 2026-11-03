package blog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func cachedGet(t *testing.T, b *browser, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := b.request("GET", path, nil)
	if w.Code != 200 {
		t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
	}
	return w
}
func TestResponseCacheNoReadsAndInvalidation(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	p := seedPost(t, a, "cached", true, false, time.Now().Add(-time.Hour))
	b := newBrowser(a)
	paths := []string{"/", "/all/", "/notes/", p.URL(), "/notes/cached/index.md", "/rss/", "/feed.json", "/llms.txt", "/sitemap.xml", "/search/?q=cached", "/post-style/" + p.ID, "/static/site.css"}
	for _, path := range paths {
		cachedGet(t, b, path)
	}
	before := a.Store.DB.queries.Load()
	for _, path := range paths {
		if got := cachedGet(t, b, path).Header().Get("X-Blog-Cache"); got != "HIT" {
			t.Errorf("%s: %s", path, got)
		}
	}
	if n := a.Store.DB.queries.Load() - before; n != 0 {
		t.Fatalf("warm cache ran %d SQL statements", n)
	}
	p.Title = "Edited article"
	p.Body = "replacement body"
	if e := a.Store.SavePost(ctx, &p); e != nil {
		t.Fatal(e)
	}
	for _, path := range []string{p.URL(), "/notes/cached/index.md", "/rss/", "/feed.json"} {
		w := cachedGet(t, b, path)
		if w.Header().Get("X-Blog-Cache") != "MISS" || !strings.Contains(w.Body.String(), p.Title) {
			t.Fatalf("stale %s", path)
		}
	}
	// No-op/rolled-back writes and statistics must not evict content.
	cachedGet(t, b, p.URL())
	beforeEpoch := a.Store.DB.cacheEpoch()
	tx, e := a.Store.DB.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, "UPDATE posts SET title='rolled back' WHERE id=$1", p.ID); e != nil {
		t.Fatal(e)
	}
	if e = tx.Rollback(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e = a.Store.DB.Exec(ctx, "UPDATE posts SET title=title WHERE id=$1", p.ID); e != nil {
		t.Fatal(e)
	}
	if e = a.FlushViews(ctx); e != nil {
		t.Fatal(e)
	}
	if a.Store.DB.cacheEpoch() != beforeEpoch {
		t.Fatal("rollback/no-op/statistics evicted cache")
	}
	if cachedGet(t, b, p.URL()).Header().Get("X-Blog-Cache") != "HIT" {
		t.Fatal("cache lost")
	}
	var views int64
	if e = a.Store.DB.QueryRow(ctx, "SELECT views FROM posts WHERE id=$1", p.ID).Scan(&views); e != nil || views < 3 {
		t.Fatalf("views not batched: %d %v", views, e)
	}
}

func TestCacheCommentsMentionsProfileAndPrivacy(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	p := seedPost(t, a, "discussion", true, false, time.Now().Add(-time.Hour))
	guest := newBrowser(a)
	owner := newBrowser(a)
	owner.login(t, "owner")
	member := newBrowser(a)
	member.login(t, "member")
	cachedGet(t, guest, p.URL())
	cachedGet(t, member, p.URL())
	w := member.request("POST", "/comments/create/", url.Values{"post_id": {p.ID}, "body": {"fresh comment"}})
	if w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	if !strings.Contains(cachedGet(t, guest, p.URL()).Body.String(), "fresh comment") {
		t.Fatal("comment not invalidated")
	}
	cs, e := a.Store.Comments(ctx, p.ID)
	if e != nil || len(cs) != 1 {
		t.Fatal(e)
	}
	if w = owner.request("POST", "/comments/"+cs[0].ID+"/delete/", nil); w.Code != 303 {
		t.Fatal(w.Code)
	}
	if strings.Contains(cachedGet(t, guest, p.URL()).Body.String(), "fresh comment") {
		t.Fatal("deleted comment cached")
	}
	_, e = a.Store.DB.Exec(ctx, `INSERT INTO webmentions(id,direction,post_id,source,target,state,moderation,title) VALUES('cache-mention','in',$1,'https://external.example/entry',$2,'verified','approved','external reply')`, p.ID, a.Config.BaseURL+p.URL())
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(cachedGet(t, guest, p.URL()).Body.String(), "external reply") {
		t.Fatal("mention not invalidated")
	}
	if w = owner.request("POST", "/godmode/webmentions/cache-mention/", url.Values{"action": {"reject"}}); w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	if strings.Contains(cachedGet(t, guest, p.URL()).Body.String(), "external reply") {
		t.Fatal("rejected mention cached")
	}
	card, revision, e := a.Store.About(ctx)
	if e != nil {
		t.Fatal(e)
	}
	cachedGet(t, guest, "/")
	card.Name = "New author"
	if ok, e := a.Store.SaveAbout(ctx, card, revision); e != nil || !ok {
		t.Fatal(e)
	}
	if !strings.Contains(cachedGet(t, guest, "/").Body.String(), card.Name) {
		t.Fatal("about cached")
	}
	p.MembersOnly = true
	if e = a.Store.SavePost(ctx, &p); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(cachedGet(t, guest, p.URL()).Body.String(), p.Body) {
		t.Fatal("privacy change leaked cached body")
	}
	if !strings.Contains(cachedGet(t, member, p.URL()).Body.String(), p.Body) {
		t.Fatal("member lost access")
	}
	if _, e = a.Store.DB.Exec(ctx, "UPDATE users SET is_member=false WHERE email='member@example.test'"); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(cachedGet(t, member, p.URL()).Body.String(), p.Body) {
		t.Fatal("revoked membership survived cached identity")
	}
	p.Visible = false
	if e = a.Store.SavePost(ctx, &p); e != nil {
		t.Fatal(e)
	}
	if w = guest.request("GET", p.URL(), nil); w.Code != 404 {
		t.Fatal("draft cached", w.Code)
	}
}

func TestCacheCSRFSessionAndHead(t *testing.T) {
	a := testApp(t)
	p := seedPost(t, a, "csrf-cache", true, false, time.Now().Add(-time.Hour))
	if _, e := a.Store.DB.Exec(context.Background(), `INSERT INTO comments(id,post_id,author_name,body) VALUES('csrf-comment',$1,'reader','A comment with a reaction form')`, p.ID); e != nil {
		t.Fatal(e)
	}
	first, second := newBrowser(a), newBrowser(a)
	one := cachedGet(t, first, p.URL())
	two := cachedGet(t, second, p.URL())
	if two.Header().Get("X-Blog-Cache") != "HIT" {
		t.Fatal("anonymous response not shared")
	}
	c1, c2 := first.cookies["csrf"], second.cookies["csrf"]
	if c1 == nil || c2 == nil || c1.Value == c2.Value || strings.Contains(two.Body.String(), c1.Value) || !strings.Contains(one.Body.String(), c1.Value) {
		t.Fatal("CSRF token shared between browsers")
	}
	// The second visitor's freshly injected token can actually submit a form.
	if w := second.request("POST", "/clickers/blocks/csrf-cache/10/", nil); w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	member := newBrowser(a)
	member.login(t, "member")
	cachedGet(t, member, p.URL())
	before := a.Store.DB.queries.Load()
	cachedGet(t, member, p.URL())
	if a.Store.DB.queries.Load() != before {
		t.Fatal("cached signed-in page queried database")
	}
	session := member.cookies["session"]
	if _, e := a.Store.DB.Exec(context.Background(), "UPDATE sessions SET expires_at=$2 WHERE token_hash=$1", hash(session.Value), time.Now().Add(150*time.Millisecond)); e != nil {
		t.Fatal(e)
	}
	cachedGet(t, member, p.URL())
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(cachedGet(t, member, p.URL()).Body.String(), "/profile/") {
		t.Fatal("expired session cached")
	}
	w := first.request("HEAD", p.URL(), nil)
	if w.Code != 200 || w.Body.Len() != 0 {
		t.Fatal("HEAD emitted body")
	}
	for _, path := range []string{"/admin", "/profile/", "/login/", "/indieauth/token"} {
		if w := first.request("GET", path, nil); w.Header().Get("X-Blog-Cache") != "" {
			t.Fatalf("sensitive route cached: %s", path)
		}
	}
}

func TestCacheExternalChangesAndScheduledPublication(t *testing.T) {
	a := testApp(t)
	p := seedPost(t, a, "external", true, false, time.Now().Add(-time.Hour))
	b := newBrowser(a)
	cachedGet(t, b, p.URL())
	// Bypass the application wrapper: the database trigger/watch must catch it.
	q, args := a.Store.DB.prepare("UPDATE posts SET title=$1 WHERE id=$2", []any{"Outside edit", p.ID})
	if _, e := a.Store.DB.pool.ExecContext(context.Background(), q, args...); e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(4 * time.Second)
	for {
		w := cachedGet(t, b, p.URL())
		if strings.Contains(w.Body.String(), "Outside edit") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("external SQL edit not observed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	future := seedPost(t, a, "scheduled", true, false, time.Now().Add(180*time.Millisecond))
	cachedGet(t, b, "/")
	time.Sleep(230 * time.Millisecond)
	if !strings.Contains(cachedGet(t, b, "/").Body.String(), future.Title) {
		t.Fatal("scheduled publication stuck in cache")
	}
}

func TestCacheOldFillAndConcurrentRequests(t *testing.T) {
	a := testApp(t)
	p := seedPost(t, a, "concurrent-cache", true, false, time.Now().Add(-time.Hour))
	old := a.Store.DB.cacheEpoch()
	a.cache.get("old", old)
	p.Body = "new body"
	if e := a.Store.SavePost(context.Background(), &p); e != nil {
		t.Fatal(e)
	}
	epoch := a.Store.DB.cacheEpoch()
	a.cache.get("old", epoch)
	a.cache.put("old", cachedResponse{body: []byte("stale")}, old)
	if _, ok := a.cache.get("old", epoch); ok {
		t.Fatal("late old render overwrote cache")
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b := newBrowser(a)
			w := b.request(http.MethodGet, p.URL(), nil)
			if w.Code != 200 || !strings.Contains(w.Body.String(), "new body") {
				t.Error("concurrent response")
			}
		}()
	}
	wg.Wait()
}
