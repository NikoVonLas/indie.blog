package blog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
)

type linkMetadata struct{ URL, Host, Title, Description, Image string }

func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, block := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32", "2001::/32", "2002::/16", "64:ff9b::/96"} {
		if netip.MustParsePrefix(block).Contains(ip) {
			return false
		}
	}
	return true
}

func publicURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, errors.New("invalid public URL")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return nil, errors.New("local URL")
	}
	if ip, err := netip.ParseAddr(host); err == nil && !publicIP(ip) {
		return nil, errors.New("non-public address")
	}
	if port := u.Port(); port != "" && port != "80" && port != "443" {
		return nil, errors.New("unsupported port")
	}
	return u, nil
}

// Resolve once and dial the checked address directly, including after redirects.
// Do not inherit HTTP_PROXY, which could bypass this address policy.
func publicPreviewClient() *http.Client {
	transport := &http.Transport{ResponseHeaderTimeout: 5 * time.Second, TLSHandshakeTimeout: 5 * time.Second, MaxResponseHeaderBytes: 64 << 10}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil || len(ips) == 0 {
			return nil, errors.New("host resolution failed")
		}
		for _, ip := range ips {
			if !publicIP(ip) {
				return nil, errors.New("non-public address")
			}
		}
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
	}
	return &http.Client{Transport: transport, Timeout: 8 * time.Second, CheckRedirect: func(r *http.Request, previous []*http.Request) error {
		if len(previous) >= 4 {
			return errors.New("too many redirects")
		}
		_, err := publicURL(r.URL.String())
		return err
	}}
}

func shortText(s string, limit int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) > limit {
		return string(r[:limit]) + "…"
	}
	return string(r)
}
func parseLinkMetadata(body []byte, base *url.URL) (linkMetadata, error) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return linkMetadata{}, err
	}
	meta := map[string]string{}
	title := ""
	walkHTML(doc, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		if n.Data == "title" && title == "" {
			title = nodeText(n)
		}
		if n.Data == "meta" {
			key := strings.ToLower(attr(n, "property"))
			if key == "" {
				key = strings.ToLower(attr(n, "name"))
			}
			if meta[key] == "" {
				meta[key] = attr(n, "content")
			}
		}
	})
	first := func(values ...string) string {
		for _, s := range values {
			if strings.TrimSpace(s) != "" {
				return s
			}
		}
		return ""
	}
	result := linkMetadata{URL: base.String(), Host: base.Hostname(), Title: shortText(first(meta["og:title"], meta["twitter:title"], title, base.Hostname()), 200), Description: shortText(first(meta["og:description"], meta["twitter:description"], meta["description"]), 400)}
	if raw := first(meta["og:image"], meta["twitter:image"]); raw != "" {
		if image, err := base.Parse(raw); err == nil {
			if valid, err := publicURL(image.String()); err == nil {
				result.Image = valid.String()
			}
		}
	}
	return result, nil
}
func fetchLinkMetadata(ctx context.Context, raw string, client *http.Client) (linkMetadata, error) {
	u, err := publicURL(raw)
	if err != nil {
		return linkMetadata{}, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return linkMetadata{}, err
	}
	req.Header.Set("User-Agent", "PersonalBlog-LinkPreview/1.0")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	response, err := client.Do(req)
	if err != nil {
		return linkMetadata{}, err
	}
	defer response.Body.Close()
	contentType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != 200 || (contentType != "text/html" && contentType != "application/xhtml+xml") {
		return linkMetadata{}, errors.New("not an HTML page")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil || len(body) > 2<<20 {
		return linkMetadata{}, errors.New("page too large")
	}
	return parseLinkMetadata(body, response.Request.URL)
}

var linkCardTemplate = template.Must(template.New("link-card").Parse(`<figure class="link-preview"><a href="{{.URL}}" rel="noopener noreferrer">{{if .Image}}<img src="{{.Image}}" alt="" loading="lazy">{{end}}<figcaption><span class="link-preview-host">{{.Host}}</span><strong class="link-preview-title">{{.Title}}</strong>{{if .Description}}<span class="link-preview-description">{{.Description}}</span>{{end}}</figcaption></a></figure>`))

func (a *Server) linkPreview(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	if a.limited(w, r, "link-preview", 30, time.Hour) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Проверьте адрес страницы", 400)
		return
	}
	client := publicPreviewClient()
	defer client.CloseIdleConnections()
	meta, err := fetchLinkMetadata(r.Context(), r.PostForm.Get("url"), client)
	if err != nil {
		http.Error(w, "Не удалось получить превью. Нужна открытая HTML-страница по http/https; можно вставить обычную ссылку.", 400)
		return
	}
	var snippet bytes.Buffer
	if err = linkCardTemplate.Execute(&snippet, meta); err != nil {
		a.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"html": fmt.Sprintf("\n\n%s\n\n", snippet.String()), "title": meta.Title})
}
