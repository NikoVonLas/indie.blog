package blog

import (
	"strings"
	"testing"
	"time"
)

func TestMarkdownBlocksAndSafety(t *testing.T) {
	m := NewMarkdown()
	cases := []struct {
		name, source string
		want, absent []string
	}{
		{"containers", "[[[wide.#intro\nHello **world**\n]]]\n\n{{{\n![Caption](https://example.org/a.jpg)\n}}}\n\n% Quote", []string{`id="intro"`, `<strong>world</strong>`, `block=10`, `block-media`, `<figure>`, `block-cite`}, nil},
		{"code is literal", "```text\n[[[\nnot a block\n]]]\n[? hidden ?]\n```", []string{"not a block", "[? hidden ?]"}, []string{`class="block-text`, `class="block-spoiler`}},
		{"inline code", "`[? literal ?]` [? secret ?]", []string{"<code>[? literal ?]</code>", `class="block-spoiler"`}, nil},
		{"unclosed", "[[[\nKeep this text", []string{"Keep this text"}, nil},
		{"unicode headings", "## Начало\n\n% quote\n\n## Начало", []string{`id="начало"`, `id="начало-2"`}, nil},
		{"fence inside container", "[[[\n```\n]]]\n```\ntext\n]]]", []string{"<code>", "text", "block=10"}, nil},
		{"unsafe", "<script>alert(1)</script><img src=x onerror=alert(1)>\n[x](javascript:alert(1))", nil, []string{"<script", "onerror=", "javascript:"}},
		{"video", "![Film](https://example.org/a.mp4)", []string{"<video", "controls"}, nil},
		{"youtube", "![Film](https://youtu.be/dQw4w9WgXcQ)", []string{"youtube-nocookie.com/embed/dQw4w9WgXcQ"}, nil},
		{"isolated external frame", `<iframe src="https://example.org/widget" sandbox="allow-same-origin allow-top-navigation" onload="alert(1)"></iframe>`, []string{`src="https://example.org/widget"`, `sandbox="allow-scripts allow-forms allow-popups"`}, []string{"allow-same-origin", "allow-top-navigation", "onload="}},
		{"unsafe frame", `<iframe src="javascript:alert(1)" srcdoc="<script>alert(1)</script>"></iframe>`, nil, []string{"javascript:", "srcdoc=", "<script"}},
		{"formatted spoiler", "[? **bold** *italic* ~~old~~ `code` [link](https://example.org) ?]", []string{"<strong>bold</strong>", "<em>italic</em>", "<del>old</del>", "<code>code</code>", `href="https://example.org"`}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := string(m.Render(tc.source, false))
			for _, s := range tc.want {
				if !strings.Contains(out, s) {
					t.Errorf("missing %q in %s", s, out)
				}
			}
			for _, s := range tc.absent {
				if strings.Contains(out, s) {
					t.Errorf("unexpected %q in %s", s, out)
				}
			}
		})
	}
	out := string(m.Comment(`<img src=x onerror=alert(1)><script>alert(1)</script>`))
	if strings.Contains(out, "<script") || strings.Contains(out, "<img") {
		t.Fatalf("unsafe comment: %s", out)
	}
}
func TestPostVisibility(t *testing.T) {
	now := time.Now()
	public := Post{Visible: true, PublishedAt: now.Add(-time.Hour)}
	if !public.Readable(nil) {
		t.Fatal("public post inaccessible")
	}
	public.MembersOnly = true
	if public.Readable(nil) || !public.Readable(&User{Member: true}) {
		t.Fatal("membership boundary")
	}
	public.PublishedAt = now.Add(time.Hour)
	if public.Readable(&User{Member: true}) || !public.Readable(&User{Admin: true}) {
		t.Fatal("scheduled post boundary")
	}
	public.Visible = false
	if public.Readable(nil) {
		t.Fatal("draft exposed")
	}
}
