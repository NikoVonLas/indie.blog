package blog

import (
	"bytes"
	"html/template"
	"net/http"
	"strings"

	"golang.org/x/net/html"
)

const embedSandbox = "allow-scripts allow-forms allow-popups"

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}
func hasClass(n *html.Node, class string) bool {
	return strings.Contains(" "+attr(n, "class")+" ", " "+class+" ")
}
func nodeText(n *html.Node) string {
	if n.Type == html.TextNode {
		return n.Data
	}
	var text strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		text.WriteString(nodeText(c))
	}
	return text.String()
}
func walkHTML(n *html.Node, visit func(*html.Node)) {
	visit(n)
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		walkHTML(c, visit)
		c = next
	}
}
func bodyHTML(doc *html.Node) string {
	var out bytes.Buffer
	walkHTML(doc, func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "body" {
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				_ = html.Render(&out, c)
			}
		}
	})
	return out.String()
}
func isolateFrames(source string) string {
	doc, err := html.Parse(strings.NewReader(source))
	if err != nil {
		return source
	}
	walkHTML(doc, func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "iframe" {
			n.Attr = append(n.Attr, html.Attribute{Key: "sandbox", Val: embedSandbox}, html.Attribute{Key: "referrerpolicy", Val: "no-referrer"})
		}
	})
	return bodyHTML(doc)
}

// Executable author content is kept out of the parent document and feed exports.
// Only explicit fenced `embed` blocks become separately served sandbox documents.
func (m *Markdown) RenderPost(p Post) template.HTML {
	doc, _ := html.Parse(strings.NewReader(string(m.RenderContent(p))))
	walkHTML(doc, func(n *html.Node) {
		if n.Type != html.ElementNode || n.Data != "code" || !hasClass(n, "language-embed") || n.Parent == nil || n.Parent.Data != "pre" {
			return
		}
		source := nodeText(n)
		frame := &html.Node{Type: html.ElementNode, Data: "iframe", Attr: []html.Attribute{
			{Key: "class", Val: "script-embed"}, {Key: "src", Val: "/embeds/" + p.ID + "/" + hash(source) + "/document"},
			{Key: "sandbox", Val: embedSandbox}, {Key: "loading", Val: "lazy"},
			{Key: "title", Val: "Интерактивный блок"}, {Key: "referrerpolicy", Val: "no-referrer"},
		}}
		pre := n.Parent
		if pre.Parent != nil {
			pre.Parent.InsertBefore(frame, pre)
			pre.Parent.RemoveChild(pre)
		}
	})
	return template.HTML(bodyHTML(doc))
}

func (a *Server) embed(w http.ResponseWriter, r *http.Request) {
	p, err := a.Store.PostID(r.Context(), r.PathValue("id"))
	if err != nil || !p.Readable(currentUser(r)) {
		http.NotFound(w, r)
		return
	}
	doc, _ := html.Parse(strings.NewReader(string(a.md.Render(p.Body, p.RawHTML))))
	source := ""
	walkHTML(doc, func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "code" && hasClass(n, "language-embed") && n.Parent != nil && n.Parent.Data == "pre" {
			candidate := nodeText(n)
			if hash(candidate) == r.PathValue("digest") {
				source = candidate
			}
		}
	})
	if source == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("Content-Security-Policy", "sandbox "+embedSandbox+"; default-src 'none'; script-src 'unsafe-inline' https: http:; style-src 'unsafe-inline' https: http:; img-src data: https: http:; font-src https: http:; media-src https: http:; connect-src https: http:; frame-src https: http:; form-action https: http:; base-uri 'none'; frame-ancestors 'self'")
	_, _ = w.Write([]byte("<!doctype html><html><head><meta charset=utf-8><meta name=viewport content='width=device-width,initial-scale=1'><style>html{color-scheme:dark}body{margin:20px;color:#ddd;background:#1b1b1c;font:16px system-ui}</style></head><body>" + source + "</body></html>"))
}
