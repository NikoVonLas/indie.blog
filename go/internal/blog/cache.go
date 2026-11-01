package blog

import (
	"bytes"
	"container/list"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// There is no TTL. Entries live until a committed data change, a scheduled
// publication, a restart or memory pressure. Each generation is immutable.
const responseCacheBytes = 64 << 20

type cachedResponse struct {
	header http.Header
	body   []byte
	status int
	postID string
	csrf   string
}
type cacheItem struct {
	key      string
	response cachedResponse
	size     int
}
type sessionEntry struct {
	user    User
	expires time.Time
	epoch   uint64
}
type pageCache struct {
	mu       sync.Mutex
	epoch    uint64
	entries  map[string]*list.Element
	lru      list.List
	bytes    int
	sessions map[string]sessionEntry
}

func (c *pageCache) generation(epoch uint64) {
	if c.entries == nil || c.epoch != epoch {
		c.epoch = epoch
		c.entries = make(map[string]*list.Element)
		c.lru.Init()
		c.bytes = 0
		c.sessions = make(map[string]sessionEntry)
	}
}
func (c *pageCache) get(key string, epoch uint64) (cachedResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation(epoch)
	if el := c.entries[key]; el != nil {
		c.lru.MoveToFront(el)
		return el.Value.(cacheItem).response, true
	}
	return cachedResponse{}, false
}
func (c *pageCache) put(key string, v cachedResponse, epoch uint64) {
	size := len(key) + len(v.body) + 512
	for k, values := range v.header {
		size += len(k)
		for _, value := range values {
			size += len(value)
		}
	}
	if size > responseCacheBytes/2 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.epoch != epoch {
		return
	} // A render that raced a mutation cannot repopulate old data.
	if _, ok := c.entries[key]; ok {
		return
	}
	for c.bytes+size > responseCacheBytes || len(c.entries) >= 2048 {
		el := c.lru.Back()
		item := el.Value.(cacheItem)
		delete(c.entries, item.key)
		c.bytes -= item.size
		c.lru.Remove(el)
	}
	c.entries[key] = c.lru.PushFront(cacheItem{key, v, size})
	c.bytes += size
}

type renderCacheKey struct{}
type renderCacheInfo struct{ csrf, postID string }
type responseBuffer struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (b *responseBuffer) Header() http.Header { return b.header }
func (b *responseBuffer) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}
func (b *responseBuffer) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = 200
	}
	return b.body.Write(p)
}

func (a *Server) cacheable(r *http.Request) bool {
	if r.Method != "GET" && r.Method != "HEAD" || r.Header.Get("Authorization") != "" || r.Header.Get("Range") != "" {
		return false
	}
	_, pattern := a.mux.Handler(r)
	switch pattern {
	case "GET /{$}", "GET /all/{$}", "GET /search/{$}", "GET /{kind}/{$}", "GET /{kind}/{slug}/{$}", "GET /{kind}/{slug}/index.html",
		"GET /{kind}/{slug}/index.md", "GET /rss/{$}", "GET /rss/{scope}/{$}", "GET /feed.json", "GET /llms.txt", "GET /sitemap.xml", "GET /robots.txt",
		"GET /media/{id}", "GET /post-style/{id}", "GET /embeds/{id}/{digest}/document":
		return true
	}
	return strings.HasPrefix(pattern, "GET /static/")
}
func (a *Server) cachedSession(ctx context.Context, token string) (*User, error) {
	d := a.Store.DB
	key := hash(token)
	d.gate.RLock()
	epoch := d.cacheEpoch()
	a.cache.mu.Lock()
	a.cache.generation(epoch)
	entry, ok := a.cache.sessions[key]
	a.cache.mu.Unlock()
	healthy := d.ready.Load()
	d.gate.RUnlock()
	if healthy && ok && time.Now().Before(entry.expires) {
		u := entry.user
		return &u, nil
	}
	var u User
	var expires time.Time
	e := d.QueryRow(ctx, `SELECT u.id,coalesce(u.email,''),u.name,u.password_hash,u.avatar,u.is_admin,u.is_banned,u.is_member,u.profile_url,s.expires_at FROM users u JOIN sessions s ON s.user_id=u.id WHERE s.token_hash=$1 AND s.expires_at>now() AND NOT u.is_banned AND (NOT u.is_admin OR EXISTS(SELECT 1 FROM admin_passkeys k WHERE k.id=s.passkey_id AND k.user_id=u.id))`, key).Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash, &u.Avatar, &u.Admin, &u.Banned, &u.Member, &u.ProfileURL, &expires)
	if e != nil {
		return nil, e
	}
	d.gate.RLock()
	defer d.gate.RUnlock()
	if d.ready.Load() && d.cacheEpoch() == epoch {
		a.cache.mu.Lock()
		defer a.cache.mu.Unlock()
		a.cache.generation(epoch)
		if len(a.cache.sessions) < 2048 {
			a.cache.sessions[key] = sessionEntry{u, expires, epoch}
		}
	}
	return &u, nil
}

func (a *Server) serveCached(w http.ResponseWriter, r *http.Request) {
	if !a.cacheable(r) {
		a.mux.ServeHTTP(w, r)
		return
	}
	d := a.Store.DB
	d.gate.RLock()
	epoch := d.cacheEpoch()
	healthy := d.ready.Load()
	identity := "anonymous"
	if u := currentUser(r); u != nil {
		identity = u.ID + fmt.Sprint(u.Admin, u.Member, u.Banned) + u.Name + u.Avatar + u.ProfileURL
	}
	key := identity + "\n" + r.URL.EscapedPath() + "?" + r.URL.Query().Encode()
	response, hit := a.cache.get(key, epoch)
	d.gate.RUnlock()
	if hit && healthy {
		w.Header().Set("X-Blog-Cache", "HIT")
		a.writeCached(w, r, response)
		return
	}
	info := &renderCacheInfo{csrf: "CACHE_CSRF_" + randomToken()}
	request := r.Clone(context.WithValue(r.Context(), renderCacheKey{}, info))
	request.Method = "GET"
	// Conditional requests must not turn the cached representation into a 304.
	request.Header = request.Header.Clone()
	request.Header.Del("If-None-Match")
	request.Header.Del("If-Modified-Since")
	out := &responseBuffer{header: w.Header().Clone()}
	a.mux.ServeHTTP(out, request)
	if out.status == 0 {
		out.status = 200
	}
	response = cachedResponse{out.header.Clone(), append([]byte(nil), out.body.Bytes()...), out.status, info.postID, info.csrf}
	response.header.Del("Set-Cookie")
	// Never retain errors, redirects, authentication forms or response cookies.
	if out.status == 200 && len(out.header.Values("Set-Cookie")) == 0 && healthy {
		d.gate.RLock()
		if d.ready.Load() && d.cacheEpoch() == epoch {
			a.cache.put(key, response, epoch)
		}
		d.gate.RUnlock()
	}
	for _, cookie := range out.header.Values("Set-Cookie") {
		w.Header().Add("Set-Cookie", cookie)
	}
	w.Header().Set("X-Blog-Cache", "MISS")
	a.writeCached(w, r, response)
}
func (a *Server) writeCached(w http.ResponseWriter, r *http.Request, v cachedResponse) {
	for k, values := range v.header {
		w.Header()[k] = append([]string(nil), values...)
	}
	body := v.body
	if v.csrf != "" && bytes.Contains(body, []byte(v.csrf)) {
		body = bytes.ReplaceAll(body, []byte(v.csrf), []byte(a.csrf(w, r)))
	}
	if v.postID != "" && r.Method == "GET" {
		a.recordView(v.postID)
	}
	// Browser/shared caches must revalidate mutable resources. Server RAM is the
	// persistent cache; privacy changes must never be hidden by a browser max-age.
	if strings.HasPrefix(r.URL.Path, "/media/") {
		w.Header().Set("Cache-Control", "public, no-cache")
	}
	if etag := w.Header().Get("ETag"); etag != "" && r.Header.Get("If-None-Match") == etag && v.status == 200 {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(v.status)
	if r.Method != "HEAD" {
		_, _ = w.Write(body)
	}
}

// Page views are statistics, not a content edit. Persisting a batch does not
// advance the content revision or evict pages. A hard crash may lose one batch.
func (a *Server) recordView(id string) {
	a.viewsMu.Lock()
	defer a.viewsMu.Unlock()
	if a.views == nil {
		a.views = map[string]int64{}
	}
	a.views[id]++
}
func (a *Server) FlushViews(ctx context.Context) error {
	a.flushMu.Lock()
	defer a.flushMu.Unlock()
	a.viewsMu.Lock()
	batch := a.views
	a.views = map[string]int64{}
	a.viewsMu.Unlock()
	for id, n := range batch {
		if _, e := a.Store.DB.Exec(ctx, "UPDATE posts SET views=views+$2 WHERE id=$1", id, n); e != nil {
			a.viewsMu.Lock()
			a.views[id] += n
			for other, count := range batch {
				if other != id {
					a.views[other] += count
				}
			}
			a.viewsMu.Unlock()
			return e
		}
		delete(batch, id)
	}
	return nil
}
func (a *Server) RunCacheWorker(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			stop, done := context.WithTimeout(context.Background(), 5*time.Second)
			_ = a.FlushViews(stop)
			done()
			return
		case <-ticker.C:
			_ = a.FlushViews(ctx)
		}
	}
}
