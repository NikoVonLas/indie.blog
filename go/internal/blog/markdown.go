package blog

import (
	"bytes"
	"fmt"
	"html"
	"html/template"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	gmhtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

var spoilerKind = ast.NewNodeKind("Spoiler")

type spoilerNode struct {
	ast.BaseInline
	source []byte
}

func (n *spoilerNode) Kind() ast.NodeKind            { return spoilerKind }
func (n *spoilerNode) Dump(source []byte, level int) { ast.DumpHelper(n, source, level, nil, nil) }

type spoilerParser struct{}

func (spoilerParser) Trigger() []byte { return []byte{'['} }
func (spoilerParser) Parse(parent ast.Node, r text.Reader, pc parser.Context) ast.Node {
	line, _ := r.PeekLine()
	if !bytes.HasPrefix(line, []byte("[?")) {
		return nil
	}
	end := bytes.Index(line[2:], []byte("?]"))
	if end < 0 {
		return nil
	}
	n := &spoilerNode{source: append([]byte(nil), line[2:2+end]...)}
	r.Advance(end + 4)
	return n
}

type mediaRenderer struct{}

func (mediaRenderer) RegisterFuncs(r renderer.NodeRendererFuncRegisterer) {
	r.Register(spoilerKind, renderSpoiler)
	r.Register(ast.KindImage, renderImage)
}
func renderSpoiler(w util.BufWriter, source []byte, node ast.Node, enter bool) (ast.WalkStatus, error) {
	if enter {
		var inline bytes.Buffer
		md := goldmark.New(goldmark.WithExtensions(extension.GFM))
		_ = md.Convert(bytes.TrimSpace(node.(*spoilerNode).source), &inline)
		content := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(inline.String()), "<p>"), "</p>")
		fmt.Fprintf(w, `<span class="block-spoiler" tabindex="0">%s</span>`, content)
	}
	return ast.WalkSkipChildren, nil
}
func renderImage(w util.BufWriter, source []byte, node ast.Node, enter bool) (ast.WalkStatus, error) {
	if !enter {
		return ast.WalkContinue, nil
	}
	n := node.(*ast.Image)
	src := string(n.Destination)
	alt := string(n.Text(source))
	if !safeURL(src) {
		return ast.WalkSkipChildren, nil
	}
	u, _ := url.Parse(src)
	path := strings.ToLower(u.Path)
	caption := string(n.Title)
	if caption == "" {
		caption = alt
	}
	fmt.Fprint(w, "<figure>")
	switch {
	case strings.HasSuffix(path, ".mp4") || strings.HasSuffix(path, ".webm"):
		fmt.Fprintf(w, `<video controls preload="metadata" src="%s">%s</video>`, html.EscapeString(src), html.EscapeString(alt))
	case u.Hostname() == "youtu.be" || u.Hostname() == "www.youtube.com" || u.Hostname() == "youtube.com":
		id := u.Query().Get("v")
		if u.Hostname() == "youtu.be" {
			id = strings.TrimPrefix(u.Path, "/")
		}
		if regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`).MatchString(id) {
			fmt.Fprintf(w, `<iframe loading="lazy" title="YouTube" src="https://www.youtube-nocookie.com/embed/%s" allowfullscreen></iframe>`, id)
		} else {
			fmt.Fprintf(w, `<a href="%s">%s</a>`, html.EscapeString(src), html.EscapeString(alt))
		}
	default:
		fmt.Fprintf(w, `<img loading="lazy" src="%s" alt="%s">`, html.EscapeString(src), html.EscapeString(alt))
	}
	if caption != "" {
		fmt.Fprintf(w, "<figcaption>%s</figcaption>", html.EscapeString(caption))
	}
	fmt.Fprint(w, "</figure>")
	return ast.WalkSkipChildren, nil
}

type Markdown struct {
	md     goldmark.Markdown
	policy *bluemonday.Policy
}

func NewMarkdown() *Markdown {
	p := bluemonday.UGCPolicy()
	p.AllowElements("figure", "figcaption", "video", "details", "summary")
	p.AllowAttrs("type").Matching(regexp.MustCompile(`^checkbox$`)).OnElements("input")
	p.AllowAttrs("disabled", "checked").OnElements("input")
	p.AllowAttrs("class", "id").Matching(regexp.MustCompile(`^[\p{L}\p{N}_ .:-]+$`)).Globally()
	p.AllowAttrs("tabindex").Matching(regexp.MustCompile(`^0$`)).OnElements("span")
	p.AllowAttrs("controls", "preload", "src").OnElements("video")
	p.AllowAttrs("loading").OnElements("img", "iframe")
	p.AllowAttrs("src").Matching(regexp.MustCompile(`^https?://`)).OnElements("iframe")
	p.AllowAttrs("title", "allowfullscreen").OnElements("iframe")
	p.AllowAttrs("height").Matching(regexp.MustCompile(`^[0-9]{1,4}$`)).OnElements("iframe")
	md := goldmark.New(goldmark.WithExtensions(extension.GFM, extension.Footnote), goldmark.WithParserOptions(parser.WithAutoHeadingID(), parser.WithInlineParsers(util.Prioritized(spoilerParser{}, 150))), goldmark.WithRendererOptions(gmhtml.WithUnsafe(), gmhtml.WithHardWraps(), renderer.WithNodeRenderers(util.Prioritized(mediaRenderer{}, 100))))
	return &Markdown{md, p}
}

type headingIDs struct{ used map[string]bool }

func (h *headingIDs) Put(value []byte) { h.used[string(value)] = true }
func (h *headingIDs) Generate(value []byte, _ ast.NodeKind) []byte {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(string(value)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r)
			dash = false
		} else {
			dash = true
		}
	}
	base := b.String()
	if base == "" {
		base = "heading"
	}
	id := base
	for i := 2; h.used[id]; i++ {
		id = fmt.Sprintf("%s-%d", base, i)
	}
	h.used[id] = true
	return []byte(id)
}
func (m *Markdown) plain(source string, ids parser.IDs) string {
	var b bytes.Buffer
	if e := m.md.Convert([]byte(source), &b, parser.WithContext(parser.NewContext(parser.WithIDs(ids)))); e != nil {
		return html.EscapeString(source)
	}
	return b.String()
}
func (m *Markdown) Render(source string, raw bool) template.HTML {
	rendered := source
	if !raw {
		counter := 0
		rendered = m.blocks(source, &counter, 0, &headingIDs{used: map[string]bool{}})
	}
	return template.HTML(isolateFrames(m.policy.Sanitize(rendered)))
}
func (m *Markdown) Comment(source string) template.HTML {
	// Comments never interpret owner-only HTML or custom media blocks.
	md := goldmark.New(goldmark.WithExtensions(extension.GFM), goldmark.WithRendererOptions(gmhtml.WithHardWraps()))
	var b bytes.Buffer
	_ = md.Convert([]byte(source), &b)
	return template.HTML(bluemonday.UGCPolicy().Sanitize(b.String()))
}
func (m *Markdown) blocks(source string, counter *int, depth int, ids parser.IDs) string {
	if depth > 16 {
		return m.plain(source, ids)
	}
	lines := strings.Split(source, "\n")
	var out, chunk strings.Builder
	flush := func() {
		if chunk.Len() > 0 {
			out.WriteString(m.plain(chunk.String(), ids))
			chunk.Reset()
		}
	}
	fence := ""
	fenceLen := 0
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			char := t[:1]
			n := len(t) - len(strings.TrimLeft(t, char))
			if fence == "" {
				fence = char
				fenceLen = n
			} else if char == fence && n >= fenceLen && strings.Trim(t, char) == "" {
				fence = ""
			}
			chunk.WriteString(line + "\n")
			continue
		}
		if fence != "" || strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t") {
			chunk.WriteString(line + "\n")
			continue
		}
		open, close, klass := "", "", ""
		if strings.HasPrefix(line, "[[[") {
			open, close, klass = "[[[", "]]]", "block-text"
		}
		if strings.HasPrefix(line, "{{{") {
			open, close, klass = "{{{", "}}}", "block-media"
		}
		if open != "" {
			end := i + 1
			nest := 1
			innerFence := ""
			innerFenceLen := 0
			for ; end < len(lines); end++ {
				trimmed := strings.TrimSpace(lines[end])
				if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
					char := trimmed[:1]
					n := len(trimmed) - len(strings.TrimLeft(trimmed, char))
					if innerFence == "" {
						innerFence = char
						innerFenceLen = n
					} else if innerFence == char && n >= innerFenceLen && strings.Trim(trimmed, char) == "" {
						innerFence = ""
					}
					continue
				}
				if innerFence != "" {
					continue
				}
				if strings.HasPrefix(lines[end], open) {
					nest++
				}
				if strings.TrimSpace(lines[end]) == close {
					nest--
					if nest == 0 {
						break
					}
				}
			}
			if end < len(lines) {
				flush()
				attrs := strings.TrimSpace(strings.TrimPrefix(line, open))
				id := ""
				for _, a := range strings.Split(attrs, ".") {
					if strings.HasPrefix(a, "#") {
						id = strings.TrimPrefix(a, "#")
					} else if a != "" {
						klass += " " + a
					}
				}
				block := 0
				if open == "[[[" {
					*counter += 10
					block = *counter
					if id == "" {
						id = fmt.Sprintf("block-%d", block)
					}
				}
				fmt.Fprintf(&out, `<section class="%s" id="%s">`, html.EscapeString(klass), html.EscapeString(id))
				out.WriteString(m.blocks(strings.Join(lines[i+1:end], "\n"), counter, depth+1, ids))
				if block > 0 {
					fmt.Fprintf(&out, `<a class="block-discuss" href="?block=%d#comment-form">Обсудить фрагмент</a>`, block)
				}
				out.WriteString("</section>\n")
				i = end
				continue
			}
		}
		if strings.HasPrefix(line, "% ") {
			flush()
			out.WriteString(`<aside class="block-cite">` + m.plain(line[2:], ids) + "</aside>\n")
			continue
		}
		chunk.WriteString(line + "\n")
	}
	flush()
	return out.String()
}
