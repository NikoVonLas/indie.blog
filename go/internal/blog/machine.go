package blog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/strikethrough"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"

	"golang.org/x/net/html"
)

func (p Post) MarkdownURL() string {
	return "/" + p.Kind + "/" + url.PathEscape(p.Slug) + "/index.md?lang=" + url.QueryEscape(p.Lang)
}
func markdownLabel(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return strings.NewReplacer(`\`, `\\`, "[", `\[`, "]", `\]`, "*", `\*`, "_", `\_`, "<", "&lt;", ">", "&gt;", "`", "&#96;", "#", `\#`).Replace(s)
}
func absoluteURL(origin, ref string) string {
	base, _ := url.Parse(origin)
	u, err := url.Parse(ref)
	if err != nil {
		return origin
	}
	return base.ResolveReference(u).String()
}
func (a *Server) structuredPost(p Post) template.JS {
	if !p.Public(time.Now()) || p.MembersOnly {
		return ""
	}
	author := p.Author
	if author == "" {
		author = a.Config.Author
	}
	canonical := a.Config.BaseURL + p.URL()
	description := p.Summary
	if description == "" {
		description = p.Subtitle
	}
	value := map[string]any{
		"@context": "https://schema.org", "@type": "BlogPosting", "@id": canonical + "#article",
		"url": canonical, "mainEntityOfPage": canonical, "headline": p.Title,
		"description": description, "author": map[string]string{"@type": "Person", "name": author},
		"datePublished": p.PublishedAt.UTC().Format(time.RFC3339),
		"dateModified":  p.UpdatedAt.UTC().Format(time.RFC3339), "inLanguage": p.Lang,
		"isPartOf": map[string]string{"@type": "Blog", "name": a.Config.Title, "url": a.Config.BaseURL},
	}
	if p.Image != "" {
		value["image"] = absoluteURL(canonical, p.Image)
	}
	// encoding/json escapes <, > and &, preventing a title from closing the script tag.
	data, _ := json.Marshal(value)
	return template.JS(data)
}
func (a *Server) markdownPost(w http.ResponseWriter, r *http.Request) {
	p, err := a.requestedPost(r)
	if errors.Is(err, ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	if !p.Readable(currentUser(r)) {
		http.NotFound(w, r)
		return
	}
	if p.Kind != r.PathValue("kind") {
		http.Redirect(w, r, p.MarkdownURL(), http.StatusMovedPermanently)
		return
	}
	canonical := a.Config.BaseURL + p.URL()
	body, err := a.markdownBody(p, canonical)
	if err != nil {
		a.fail(w, err)
		return
	}
	author := p.Author
	if author == "" {
		author = a.Config.Author
	}
	var out strings.Builder
	fmt.Fprintf(&out, "# %s\n\n", markdownLabel(p.Title))
	if p.Subtitle != "" {
		fmt.Fprintf(&out, "> %s\n\n", markdownLabel(p.Subtitle))
	}
	fmt.Fprintf(&out, a.translate(p.Lang, "Автор: %s\n\nОпубликовано: %s\n\nОбновлено: %s\n\nЯзык: %s\n\nОригинал: <%s>\n\n"), markdownLabel(author), p.PublishedAt.UTC().Format(time.RFC3339), p.UpdatedAt.UTC().Format(time.RFC3339), p.Lang, canonical)
	if p.Image != "" {
		fmt.Fprintf(&out, a.translate(p.Lang, "![Обложка](<%s>)\n\n"), absoluteURL(canonical, p.Image))
	}
	if p.ExternalURL != "" {
		fmt.Fprintf(&out, a.translate(p.Lang, "Внешняя публикация: <%s>\n\n"), p.ExternalURL)
	}
	out.WriteString(body)
	out.WriteByte('\n')
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Language", p.Lang)
	w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="canonical", <%s>; rel="alternate"; type="text/html", <%s/llms.txt?lang=%s>; rel="describedby"`, canonical, canonical, a.Config.BaseURL, p.Lang))
	if p.MembersOnly || !p.Public(time.Now()) {
		w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	}
	fmt.Fprint(w, out.String())
}
func (a *Server) markdownBody(p Post, canonical string) (string, error) {
	doc, err := html.Parse(strings.NewReader(string(a.md.RenderContent(p))))
	if err != nil {
		return "", err
	}
	// Export the article alone. UI controls become plain text; media retain their URLs.
	var visit func(*html.Node)
	visit = func(n *html.Node) {
		attrs := map[string]string{}
		for _, v := range n.Attr {
			attrs[v.Key] = v.Val
		}
		if n.Type == html.ElementNode && strings.Contains(" "+attrs["class"]+" ", " block-discuss ") {
			n.Parent.RemoveChild(n)
			return
		}
		if n.Type == html.ElementNode {
			switch n.Data {
			case "aside":
				n.Data = "blockquote"
			case "video", "iframe":
				src := attrs["src"]
				n.Data = "a"
				n.Attr = []html.Attribute{{Key: "href", Val: src}}
				if n.FirstChild == nil {
					n.AppendChild(&html.Node{Type: html.TextNode, Data: "Видео"})
				}
			case "input":
				label := "☐ "
				if _, ok := attrs["checked"]; ok {
					label = "☑ "
				}
				n.Type = html.TextNode
				n.Data = label
				n.Attr = nil
			}
		}
		for child := n.FirstChild; child != nil; {
			next := child.NextSibling
			visit(child)
			child = next
		}
	}
	visit(doc)
	var clean bytes.Buffer
	if err = html.Render(&clean, doc); err != nil {
		return "", err
	}
	conv := converter.NewConverter(converter.WithPlugins(base.NewBasePlugin(), commonmark.NewCommonmarkPlugin(), table.NewTablePlugin(), strikethrough.NewStrikethroughPlugin()))
	return conv.ConvertString(clean.String(), converter.WithDomain(canonical))
}
func (a *Server) llms(w http.ResponseWriter, r *http.Request) {
	lang := a.lang(r)
	posts, err := a.Store.Posts(r.Context(), Filter{Lang: lang, FallbackLang: a.Config.Language, PublicOnly: true, Limit: 20})
	if err != nil {
		a.fail(w, err)
		return
	}
	var out strings.Builder
	fmt.Fprintf(&out, a.translate(lang, "# %s\n\n> Личный блог. Автор: %s. Язык этой подборки: %s.\n\n"), markdownLabel(a.Config.Title), markdownLabel(a.Config.Author), lang)
	out.WriteString(a.translate(lang, "Статьи, короткие заметки и другие публикации автора. Ниже — последние открытые записи; полный список находится в архиве. HTML-страницы содержат ссылку rel=alternate на Markdown и метаданные BlogPosting в JSON-LD. В Markdown сохранены текст, ссылки и адреса медиа; содержимое спойлеров раскрыто.\n\n"))
	fmt.Fprintf(&out, a.translate(lang, "## Навигация\n\n- [Архив](%s/all/?lang=%s): все опубликованные записи.\n- [RSS](%s/rss/?lang=%s): обновления.\n- [JSON Feed](%s/feed.json?lang=%s): машиночитаемая лента.\n- [Карта сайта](%s/sitemap.xml): адреса открытых записей на всех языках.\n\n## Записи в Markdown\n\n"), a.Config.BaseURL, lang, a.Config.BaseURL, lang, a.Config.BaseURL, lang, a.Config.BaseURL)
	for _, p := range posts {
		title := p.Title
		if title == "" {
			title = a.translate(lang, "Заметка")
		}
		fmt.Fprintf(&out, "- [%s](%s%s): %s, %s, %s.\n", markdownLabel(title), a.Config.BaseURL, p.MarkdownURL(), a.translate(lang, kindLabel(p.Kind)), p.PublishedAt.UTC().Format("2006-01-02"), p.Lang)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, out.String())
}
