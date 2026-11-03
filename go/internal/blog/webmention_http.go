package blog

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

type mentionDocument struct {
	URL    *url.URL
	Header http.Header
	Body   []byte
	Status int
	HTML   bool
}

func fetchMentionDocument(ctx context.Context, client *http.Client, raw string) (mentionDocument, error) {
	return fetchProtocolDocument(ctx, client, raw, "text/html,application/xhtml+xml")
}

func fetchProtocolDocument(ctx context.Context, client *http.Client, raw, accept string) (mentionDocument, error) {
	u, err := publicURL(raw)
	if err != nil {
		return mentionDocument{}, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return mentionDocument{}, err
	}
	req.Header.Set("User-Agent", "PersonalBlog-IndieWeb/1.0")
	req.Header.Set("Accept", accept)
	response, err := client.Do(req)
	if err != nil {
		return mentionDocument{}, err
	}
	defer response.Body.Close()
	doc := mentionDocument{URL: response.Request.URL, Header: response.Header, Status: response.StatusCode}
	ct, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	doc.HTML = ct == "text/html" || ct == "application/xhtml+xml"
	if doc.Status != 200 {
		return doc, nil
	}
	doc.Body, err = io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if len(doc.Body) > 2<<20 {
		return doc, errors.New("document exceeds limit")
	}
	return doc, err
}
func documentBase(doc *html.Node, base *url.URL) *url.URL {
	result := base
	found := false
	walkHTML(doc, func(n *html.Node) {
		if !found && n.Type == html.ElementNode && n.Data == "base" {
			if value := attr(n, "href"); hasHTMLAttr(n, "href") {
				if u, err := base.Parse(value); err == nil {
					result = u
				}
				found = true
			}
		}
	})
	return result
}
func hasHTMLAttr(n *html.Node, key string) bool {
	for _, attribute := range n.Attr {
		if attribute.Key == key {
			return true
		}
	}
	return false
}
func visitLinks(n *html.Node, visit func(*html.Node, string)) {
	if n.Type == html.ElementNode {
		switch n.Data {
		case "script", "style", "template":
			return
		}
		key := ""
		switch n.Data {
		case "a", "area", "link":
			key = "href"
		case "img", "video", "audio", "source", "iframe":
			key = "src"
		case "object":
			key = "data"
		}
		if key != "" && hasHTMLAttr(n, key) {
			visit(n, attr(n, key))
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		visitLinks(c, visit)
	}
}
func verifyMention(ctx context.Context, client *http.Client, source, target string) (state, title, kind, detail string) {
	kind = "mention"
	d, err := fetchMentionDocument(ctx, client, source)
	if err != nil {
		return "retry", "", kind, "Не удалось загрузить источник"
	}
	if d.Status == 410 || d.Status == 404 {
		return "removed", "", kind, "Источник удалён"
	}
	if d.Status == 429 || d.Status >= 500 {
		return "retry", "", kind, "Источник временно недоступен"
	}
	if d.Status != 200 || !d.HTML {
		return "failed", "", kind, "Источник должен быть открытой HTML-страницей"
	}
	doc, err := html.Parse(bytes.NewReader(d.Body))
	if err != nil {
		return "failed", "", kind, "Не удалось разобрать источник"
	}
	base := documentBase(doc, d.URL)
	found := false
	visitLinks(doc, func(n *html.Node, raw string) {
		u, err := base.Parse(raw)
		if err != nil || u.String() != target {
			return
		}
		found = true
		if hasClass(n, "u-in-reply-to") {
			kind = "reply"
		} else if hasClass(n, "u-like-of") && kind == "mention" {
			kind = "like"
		} else if hasClass(n, "u-repost-of") && kind == "mention" {
			kind = "repost"
		}
	})
	if !found {
		return "removed", "", kind, "В источнике нет точной ссылки на запись"
	}
	meta, err := parseLinkMetadata(d.Body, d.URL)
	if err != nil {
		return "failed", "", kind, "Не удалось прочитать название"
	}
	return "verified", meta.Title, kind, ""
}
func outgoingTargets(sourceHTML, source string) []string {
	doc, _ := html.Parse(strings.NewReader(sourceHTML))
	base, _ := url.Parse(source)
	seen := map[string]bool{}
	result := []string{}
	// Only author-written hyperlinks, not navigation, comments, images or embed URLs.
	visitLinks(doc, func(n *html.Node, raw string) {
		if n.Data != "a" {
			return
		}
		u, err := base.Parse(raw)
		if err != nil {
			return
		}
		if _, err = publicURL(u.String()); err != nil || strings.EqualFold(u.Host, base.Host) {
			return
		}
		if !seen[u.String()] {
			seen[u.String()] = true
			result = append(result, u.String())
		}
	})
	return result
}

// HTTP Link values may contain commas/semicolons inside quoted parameters or URI references.
func splitLinkHeader(s string, separator rune) []string {
	var parts []string
	start := 0
	quoted, escaped, angle := false, false, false
	for i, c := range s {
		if escaped {
			escaped = false
			continue
		}
		if quoted && c == '\\' {
			escaped = true
			continue
		}
		if c == '"' {
			quoted = !quoted
			continue
		}
		if !quoted {
			if c == '<' {
				angle = true
			}
			if c == '>' {
				angle = false
			}
			if c == separator && !angle {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, s[start:])
}
func relContains(raw, want string) bool {
	for _, v := range strings.Fields(raw) {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}
func discoverMentionEndpoint(d mentionDocument) (string, error) {
	resolve := func(raw string, base *url.URL) (string, error) {
		u, err := base.Parse(raw)
		if err != nil {
			return "", err
		}
		u.Fragment = ""
		valid, err := publicURL(u.String())
		if err != nil {
			return "", err
		}
		return valid.String(), nil
	}
	for _, header := range d.Header.Values("Link") {
		for _, value := range splitLinkHeader(header, ',') {
			value = strings.TrimSpace(value)
			end := strings.Index(value, ">")
			if !strings.HasPrefix(value, "<") || end < 1 {
				continue
			}
			for _, param := range splitLinkHeader(value[end+1:], ';') {
				k, v, ok := strings.Cut(strings.TrimSpace(param), "=")
				if !ok || !strings.EqualFold(k, "rel") {
					continue
				}
				v = strings.TrimSpace(v)
				if strings.HasPrefix(v, "\"") {
					unquoted, err := strconv.Unquote(v)
					if err != nil {
						continue
					}
					v = unquoted
				}
				if relContains(v, "webmention") {
					return resolve(value[1:end], d.URL)
				}
			}
		}
	}
	if !d.HTML {
		return "", nil
	}
	doc, err := html.Parse(bytes.NewReader(d.Body))
	if err != nil {
		return "", err
	}
	base := documentBase(doc, d.URL)
	var endpoint string
	var resolveErr error
	found := false
	visitLinks(doc, func(n *html.Node, raw string) {
		if !found && (n.Data == "a" || n.Data == "link") && relContains(attr(n, "rel"), "webmention") {
			found = true
			endpoint, resolveErr = resolve(raw, base)
		}
	})
	return endpoint, resolveErr
}
func deliverMention(ctx context.Context, client *http.Client, source, target string) (string, string) {
	if _, err := publicURL(source); err != nil {
		return "cancelled", "Адрес записи не является публичным"
	}
	d, err := fetchMentionDocument(ctx, client, target)
	if err != nil {
		return "retry", "Не удалось загрузить страницу получателя"
	}
	if d.Status == 429 || d.Status >= 500 {
		return "retry", "Сайт получателя временно недоступен"
	}
	if d.Status != 200 {
		return "failed", "Страница получателя недоступна"
	}
	endpoint, err := discoverMentionEndpoint(d)
	if err != nil {
		return "failed", "Недопустимый адрес Webmention у получателя"
	}
	if endpoint == "" {
		return "unsupported", ""
	}
	body := url.Values{"source": {source}, "target": {target}}.Encode()
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(body))
	if err != nil {
		return "failed", "Недопустимый адрес получателя"
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "PersonalBlog-Webmention/1.0")
	sender := *client
	sender.CheckRedirect = func(r *http.Request, previous []*http.Request) error {
		if r.Method != "POST" {
			return http.ErrUseLastResponse
		}
		if len(previous) >= 4 {
			return errors.New("too many redirects")
		}
		if _, err := publicURL(r.URL.String()); err != nil {
			return err
		}
		if client.CheckRedirect != nil {
			return client.CheckRedirect(r, previous)
		}
		return nil
	}
	response, err := sender.Do(req)
	if err != nil {
		return "retry", "Не удалось отправить уведомление"
	}
	defer response.Body.Close()
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return "sent", ""
	}
	if response.StatusCode == 429 || response.StatusCode >= 500 {
		return "retry", "Получатель временно отклонил уведомление"
	}
	return "failed", "Получатель отклонил уведомление"
}
