package blog

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
	"time"
)

type rssDocument struct {
	XMLName xml.Name   `xml:"rss"`
	Version string     `xml:"version,attr"`
	Channel rssChannel `xml:"channel"`
}
type rssChannel struct {
	Title       string    `xml:"title"`
	Link        string    `xml:"link"`
	Description string    `xml:"description"`
	Language    string    `xml:"language"`
	Items       []rssItem `xml:"item"`
}
type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	GUID        string `xml:"guid"`
	Date        string `xml:"pubDate"`
	Description string `xml:"description"`
}

func (a *Server) feed(w http.ResponseWriter, r *http.Request) {
	scope := r.PathValue("scope")
	if scope != "" && scope != "public" && scope != "private" && scope != "blog" {
		http.NotFound(w, r)
		return
	}
	private := scope == "private"
	u := currentUser(r)
	if private && (u == nil || !u.Member && !u.Admin) {
		http.Error(w, "Требуется вход", 401)
		return
	}
	posts, e := a.Store.Posts(r.Context(), Filter{Lang: a.lang(r), PublicOnly: !private, Home: true, Limit: 30})
	if e != nil {
		a.fail(w, e)
		return
	}
	feed := rssDocument{Version: "2.0", Channel: rssChannel{Title: a.Config.Title, Link: a.Config.BaseURL, Description: a.Config.Title, Language: a.lang(r)}}
	for _, p := range posts {
		body := string(a.md.RenderContent(p))
		if p.ExternalURL != "" {
			body = p.Summary
		}
		feed.Channel.Items = append(feed.Channel.Items, rssItem{Title: p.Title, Link: a.Config.BaseURL + p.URL(), GUID: a.Config.BaseURL + "/#" + p.ID, Date: p.PublishedAt.Format(time.RFC1123Z), Description: body})
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(feed)
}
func (a *Server) jsonFeed(w http.ResponseWriter, r *http.Request) {
	posts, e := a.Store.Posts(r.Context(), Filter{Lang: a.lang(r), PublicOnly: true, Home: true, Limit: 30})
	if e != nil {
		a.fail(w, e)
		return
	}
	items := []map[string]any{}
	for _, p := range posts {
		items = append(items, map[string]any{"id": a.Config.BaseURL + "/#" + p.ID, "url": a.Config.BaseURL + p.URL(), "title": p.Title, "content_html": string(a.md.RenderContent(p)), "date_published": p.PublishedAt.Format(time.RFC3339), "date_modified": p.UpdatedAt.Format(time.RFC3339), "language": p.Lang})
	}
	w.Header().Set("Content-Type", "application/feed+json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"version": "https://jsonfeed.org/version/1.1", "title": a.Config.Title, "home_page_url": a.Config.BaseURL, "feed_url": a.Config.BaseURL + "/feed.json", "items": items})
}

type sitemapDoc struct {
	XMLName xml.Name     `xml:"urlset"`
	NS      string       `xml:"xmlns,attr"`
	URLs    []sitemapURL `xml:"url"`
}
type sitemapURL struct {
	Location string `xml:"loc"`
	Modified string `xml:"lastmod"`
}

func (a *Server) sitemap(w http.ResponseWriter, r *http.Request) {
	doc := sitemapDoc{NS: "http://www.sitemaps.org/schemas/sitemap/0.9"}
	for offset := 0; ; offset += 1000 {
		ps, e := a.Store.Posts(r.Context(), Filter{PublicOnly: true, Limit: 1000, Offset: offset})
		if e != nil {
			a.fail(w, e)
			return
		}
		for _, p := range ps {
			doc.URLs = append(doc.URLs, sitemapURL{a.Config.BaseURL + p.URL(), p.UpdatedAt.Format(time.RFC3339)})
		}
		if len(ps) < 1000 {
			break
		}
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(doc)
}
