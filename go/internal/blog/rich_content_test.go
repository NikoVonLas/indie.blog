package blog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"
)

func TestHomeLatestArticleAndPagination(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	now := time.Now().Add(-time.Hour)
	article := seedPost(t, a, "latest-article", true, false, now.Add(-time.Minute))
	article.Kind = "blog"
	_ = a.Store.SavePost(ctx, &article)
	for i := 0; i < 34; i++ {
		p := seedPost(t, a, fmt.Sprintf("entry-%02d", i), true, false, now.Add(-time.Duration(i)*time.Hour))
		if i%5 == 4 {
			p.Kind = "blog"
			_ = a.Store.SavePost(ctx, &p)
		}
	}
	for _, slug := range []string{"future-article", "draft-article"} {
		p := seedPost(t, a, slug, false, false, now)
		p.Kind = "blog"
		if slug == "future-article" {
			p.Visible = true
			p.PublishedAt = time.Now().Add(time.Hour)
		}
		_ = a.Store.SavePost(ctx, &p)
	}
	b := newBrowser(a)
	first := b.request("GET", "/", nil).Body.String()
	second := b.request("GET", "/?page=2", nil).Body.String()
	cards := func(source string) []string {
		doc, _ := html.Parse(strings.NewReader(source))
		var out []string
		walkHTML(doc, func(n *html.Node) {
			if n.Data == "a" && hasClass(n, "card") {
				out = append(out, attr(n, "href"))
			}
		})
		return out
	}
	firstCards, secondCards := cards(first), cards(second)
	if len(firstCards) != 31 || len(secondCards) != 4 || firstCards[0] != article.URL() {
		t.Fatalf("bad home pages: %v %v", firstCards, secondCards)
	}
	if strings.Index(first, article.URL()) > strings.Index(first, `class="about-card`) || strings.Index(first, `class="about-card`) > strings.Index(first, "entry-00/") {
		t.Fatal("hero/about/feed order")
	}
	all := append(firstCards, secondCards...)
	seen := map[string]bool{}
	for _, address := range all {
		if seen[address] {
			t.Fatal("duplicate", address)
		}
		seen[address] = true
		if strings.Contains(address, "future-") || strings.Contains(address, "draft-") {
			t.Fatal("unpublished content")
		}
	}
	if strings.Contains(second, `class="about-card`) {
		t.Fatal("about repeated on page 2")
	}
	if firstCards[1] != "/notes/entry-00/?lang=ru" {
		t.Fatal("newer note must follow the hero and about")
	}
}

func uploadRequest(b *browser, purpose string, data []byte, csrf bool) *httptest.ResponseRecorder {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, _ := writer.CreateFormFile("image", "image.png")
	_, _ = file.Write(data)
	_ = writer.Close()
	r := httptest.NewRequest("POST", "http://blog.test/uploads/?purpose="+purpose, &body)
	r.Header.Set("Content-Type", writer.FormDataContentType())
	r.Header.Set("Origin", "http://blog.test")
	r.RemoteAddr = "127.0.0.1:1234"
	for _, c := range b.cookies {
		r.AddCookie(c)
	}
	if csrf && b.cookies["csrf"] != nil {
		r.Header.Set("X-CSRF-Token", b.cookies["csrf"].Value)
	}
	w := httptest.NewRecorder()
	b.app.ServeHTTP(w, r)
	return w
}
func TestImageUploads(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a)
	owner.login(t, "owner")
	owner.request("GET", "/profile/", nil)
	member := newBrowser(a)
	member.login(t, "member")
	member.request("GET", "/profile/", nil)
	anon := newBrowser(a)
	anon.request("GET", "/login/", nil)
	var imageData bytes.Buffer
	_ = png.Encode(&imageData, image.NewRGBA(image.Rect(0, 0, 12, 8)))
	data := imageData.Bytes()
	for _, tc := range []struct {
		b       *browser
		purpose string
		body    []byte
		csrf    bool
		status  int
	}{
		{anon, "avatar", data, true, 401}, {member, "post", data, true, 403}, {owner, "post", data, false, 403},
		{owner, "post", []byte(`<svg onload="alert(1)"></svg>`), true, 400}, {owner, "post", bytes.Repeat([]byte("x"), 11<<20), true, 413},
	} {
		if w := uploadRequest(tc.b, tc.purpose, tc.body, tc.csrf); w.Code != tc.status {
			t.Fatalf("upload status %d want %d: %s", w.Code, tc.status, w.Body.String())
		}
	}
	for _, b := range []*browser{owner, member} {
		w := uploadRequest(b, "avatar", data, true)
		if w.Code != 201 {
			t.Fatal(w.Code, w.Body.String())
		}
		var result struct{ URL string }
		_ = json.Unmarshal(w.Body.Bytes(), &result)
		media := anon.request("GET", result.URL, nil)
		if media.Code != 200 || media.Header().Get("Content-Type") != "image/png" || !bytes.Equal(media.Body.Bytes(), data) {
			t.Fatal("upload roundtrip")
		}
		saved := b.request("POST", "/profile/", url.Values{"name": {"Updated"}, "avatar": {result.URL}})
		if saved.Code != 303 {
			t.Fatal("avatar save", saved.Code)
		}
		page := b.request("GET", "/profile/", nil)
		if !strings.Contains(page.Body.String(), result.URL) {
			t.Fatal("avatar lost")
		}
	}
}

func TestExecutableEmbedsAreIsolated(t *testing.T) {
	a := testApp(t)
	p := seedPost(t, a, "interactive", true, false, time.Now().Add(-time.Hour))
	source := "<button id=hello>Click</button><script>document.querySelector('#hello').onclick=()=>{document.body.dataset.clicked='yes'}</script>\n"
	p.Body = "```embed\n" + source + "```"
	_ = a.Store.SavePost(context.Background(), &p)
	b := newBrowser(a)
	page := b.request("GET", p.URL(), nil)
	address := "/embeds/" + p.ID + "/" + hash(source) + "/document"
	if !strings.Contains(page.Body.String(), address) || strings.Contains(page.Body.String(), "document.body.dataset.clicked") {
		t.Fatal("script leaked into parent or iframe absent")
	}
	embedded := b.request("GET", address, nil)
	if embedded.Code != 200 || !strings.Contains(embedded.Body.String(), source) || !strings.Contains(embedded.Header().Get("Content-Security-Policy"), "sandbox "+embedSandbox) || strings.Contains(embedded.Header().Get("Content-Security-Policy"), "allow-same-origin") {
		t.Fatal("embed isolation", embedded.Code)
	}
	p.MembersOnly = true
	_ = a.Store.SavePost(context.Background(), &p)
	if b.request("GET", address, nil).Code != 404 {
		t.Fatal("private embed leaked")
	}
	b.login(t, "member")
	if b.request("GET", address, nil).Code != 200 {
		t.Fatal("member embed denied")
	}
	p.Visible = false
	_ = a.Store.SavePost(context.Background(), &p)
	if b.request("GET", address, nil).Code != 404 {
		t.Fatal("draft embed leaked")
	}
	comment := string(a.md.Comment(p.Body + `<iframe src="https://example.org"></iframe>`))
	if strings.Contains(comment, "<script>") || strings.Contains(comment, "<iframe") {
		t.Fatal("comment executable")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestLinkPreviewMetadataAndSSRF(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "::1", "::ffff:127.0.0.1", "10.1.2.3", "169.254.169.254", "100.100.100.200", "192.168.1.1", "fd00::1", "224.0.0.1", "198.18.0.1"} {
		if publicIP(netip.MustParseAddr(ip)) {
			t.Fatal("private IP accepted", ip)
		}
	}
	for _, raw := range []string{"http://127.0.0.1/", "http://[::1]/", "http://localhost/", "http://service.local/", "file:///etc/passwd", "https://user:pass@example.org/", "http://example.org:8080"} {
		if _, err := publicURL(raw); err == nil {
			t.Fatal("invalid URL accepted", raw)
		}
	}
	client := publicPreviewClient()
	defer client.CloseIdleConnections()
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("internal server reached") }))
	defer local.Close()
	if _, err := fetchLinkMetadata(context.Background(), local.URL, client); err == nil {
		t.Fatal("local fetch allowed")
	}
	redirect, _ := http.NewRequest("GET", "http://169.254.169.254/", nil)
	if client.CheckRedirect(redirect, nil) == nil {
		t.Fatal("private redirect allowed")
	}
	htmlBody := `<html><head><title>Fallback</title><meta property="og:title" content="A &amp; B &lt;script&gt;"><meta name="description" content="Useful page"><meta property="og:image" content="/cover.png"></head></html>`
	fake := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader(htmlBody)), Request: r}, nil
	})}
	meta, err := fetchLinkMetadata(context.Background(), "https://example.org/article", fake)
	if err != nil || meta.Title != "A & B <script>" || meta.Image != "https://example.org/cover.png" {
		t.Fatal(meta, err)
	}
	var snippet bytes.Buffer
	_ = linkCardTemplate.Execute(&snippet, meta)
	if strings.Contains(snippet.String(), "<script>") || !strings.Contains(snippet.String(), "&lt;script&gt;") {
		t.Fatal("metadata injection")
	}
	clean := string(NewMarkdown().Render(snippet.String(), false))
	if !strings.Contains(clean, `class="link-preview"`) || !strings.Contains(clean, "Useful page") {
		t.Fatal("card lost during sanitization")
	}
}
