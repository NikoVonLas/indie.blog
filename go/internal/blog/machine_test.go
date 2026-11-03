package blog

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestFormattingGuide(t *testing.T) {
	source, err := os.ReadFile("../../examples/formatting.md")
	if err != nil {
		t.Fatal(err)
	}
	m := NewMarkdown()
	rendered := string(m.Render(string(source), false))
	for _, want := range []string{"<del>", "<strong>", "<em>", "<u>", "<sub>", "<sup>", "<mark>", "class=\"block-spoiler\"", "<details>", "<summary>", "<table>", "<video", "type=\"checkbox\"", "checked", "disabled", "footnote-ref", "block-media", "block-discuss"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("guide does not render %s", want)
		}
	}
	a := &Server{md: m}
	md, err := a.markdownBody(Post{Body: string(source)}, "http://blog.test/blog/guide/?lang=ru")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"~~зачёркнутый текст~~", "сорок два", "http://blog.test/static/demo-cover.svg", "http://blog.test/static/demo-video.mp4", "☑", "☐", "Текст сноски", "|"} {
		if !strings.Contains(md, want) {
			t.Errorf("Markdown missing %q", want)
		}
	}
	if strings.Contains(md, "?block=") {
		t.Error("UI discussion controls leaked into Markdown")
	}
}
func TestMachineFormats(t *testing.T) {
	a := testApp(t)
	now := time.Now().Add(-time.Hour)
	public := seedPost(t, a, "public-machine", true, false, now)
	public.Title = `Quoted "title" </script><script>alert(1)</script>`
	public.Author = "An author"
	public.Image = "/static/demo-cover.svg"
	public.Body = "[[[\nHello **reader**.\n]]]\n\n% A quote\n\n[? A secret ?]\n\n![Cover](/static/demo-cover.svg)"
	if err := a.Store.SavePost(context.Background(), &public); err != nil {
		t.Fatal(err)
	}
	seedPost(t, a, "private-machine", true, true, now)
	seedPost(t, a, "draft-machine", false, false, now)
	seedPost(t, a, "future-machine", true, false, time.Now().Add(time.Hour))
	en := seedPost(t, a, "english-machine", true, false, now)
	en.Lang = "en"
	if err := a.Store.SavePost(context.Background(), &en); err != nil {
		t.Fatal(err)
	}
	anon := newBrowser(a)
	owner := newBrowser(a)
	owner.login(t, "owner")
	member := newBrowser(a)
	member.login(t, "member")
	for _, slug := range []string{"private-machine", "draft-machine", "future-machine"} {
		w := anon.request("GET", "/notes/"+slug+"/index.md", nil)
		if w.Code != 404 || strings.Contains(w.Body.String(), "body-secret") {
			t.Fatalf("Markdown access leak for %s", slug)
		}
	}
	for _, entry := range []struct {
		b    *browser
		slug string
	}{{member, "private-machine"}, {owner, "draft-machine"}, {owner, "future-machine"}} {
		w := entry.b.request("GET", "/notes/"+entry.slug+"/index.md", nil)
		if w.Code != 200 || w.Header().Get("X-Robots-Tag") == "" || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
			t.Fatalf("authorized export failed: %s %d", entry.slug, w.Code)
		}
		page := entry.b.request("GET", "/notes/"+entry.slug+"/", nil)
		if strings.Contains(page.Body.String(), "application/ld+json") {
			t.Error("protected preview has public structured metadata")
		}
	}
	w := anon.request("GET", public.MarkdownURL(), nil)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/markdown") {
		t.Fatal("Markdown response", w.Code, w.Body.String())
	}
	for _, want := range []string{"Автор: An author", "Опубликовано:", "Обновлено:", "http://blog.test" + public.URL(), "**reader**", "> A quote", "A secret"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("Markdown missing %q: %s", want, w.Body.String())
		}
	}
	for _, bad := range []string{"[[[", "block-discuss", "<nav", "<script"} {
		if strings.Contains(w.Body.String(), bad) {
			t.Errorf("Markdown contains %q", bad)
		}
	}
	if !strings.Contains(w.Header().Get("Link"), "rel=\"canonical\"") {
		t.Error("missing canonical header")
	}
	redirected := anon.request("GET", "/blog/public-machine/index.md?lang=ru", nil)
	if redirected.Code != 301 || redirected.Header().Get("Location") != public.MarkdownURL() {
		t.Error("Markdown canonical redirect")
	}
	w = anon.request("GET", public.URL(), nil)
	match := regexp.MustCompile(`<script type="application/ld\+json">(.*?)</script>`).FindStringSubmatch(w.Body.String())
	if len(match) != 2 {
		t.Fatal("missing JSON-LD", w.Body.String())
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(match[1]), &data); err != nil {
		t.Fatal(err)
	}
	if data["@type"] != "BlogPosting" || data["headline"] != public.Title || data["inLanguage"] != "ru" || data["image"] != "http://blog.test/static/demo-cover.svg" || data["url"] != "http://blog.test"+public.URL() {
		t.Fatal("incorrect structured metadata", data)
	}
	if !strings.Contains(w.Body.String(), `rel="alternate" type="text/markdown"`) || !strings.Contains(w.Header().Get("Link"), "text/markdown") {
		t.Error("missing Markdown discovery")
	}
	if strings.Contains(w.Body.String(), "</script><script>alert") {
		t.Error("JSON-LD allows script breakout")
	}
	for _, b := range []*browser{anon, owner} {
		guide := b.request("GET", "/llms.txt", nil)
		if guide.Code != 200 || !strings.Contains(guide.Body.String(), public.MarkdownURL()) {
			t.Fatal("llms guide missing public article")
		}
		for _, slug := range []string{"private-machine", "draft-machine", "future-machine", "english-machine"} {
			if strings.Contains(guide.Body.String(), slug) {
				t.Error("llms guide leaks or mixes languages", slug)
			}
		}
	}
	english := anon.request("GET", "/llms.txt?lang=en", nil)
	if !strings.Contains(english.Body.String(), en.MarkdownURL()) || strings.Contains(english.Body.String(), public.MarkdownURL()) {
		t.Error("llms language selection failed")
	}
	// HTML-mode articles are sanitized before conversion, just like the visible page.
	raw := Post{RawHTML: true, Body: `<h2>Raw heading</h2><p>safe <strong>text</strong></p><script>evil()</script>`}
	markdown, err := a.markdownBody(raw, "http://blog.test/")
	if err != nil || !strings.Contains(markdown, "## Raw heading") || strings.Contains(markdown, "evil()") {
		t.Fatal("raw HTML export", markdown, err)
	}
}
