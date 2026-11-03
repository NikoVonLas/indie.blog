package blog

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func mpJSON(a *Server, token string, value any) *httptest.ResponseRecorder {
	body, _ := json.Marshal(value)
	return apiRequest(a, "POST", "/micropub", "application/json", string(body), token)
}
func mpRead(t *testing.T, a *Server, token, location string) mpProperties {
	t.Helper()
	w := apiRequest(a, "GET", "/micropub?q=source&url="+url.QueryEscape(location), "", "", token)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var v struct {
		Properties mpProperties `json:"properties"`
	}
	if e := json.Unmarshal(w.Body.Bytes(), &v); e != nil {
		t.Fatal(e)
	}
	return v.Properties
}
func TestMicropubLifecycleAndScopes(t *testing.T) {
	a := testApp(t)
	token := issueTestToken(t, a, "create update delete undelete media read")
	create := issueTestToken(t, a, "create")
	if w := apiRequest(a, "GET", "/micropub?q=config&access_token="+token, "", "", ""); w.Code != 401 {
		t.Fatal("query-string token accepted")
	}
	// Browser cookies do not authenticate the protocol endpoint.
	owner := newBrowser(a)
	owner.login(t, "owner")
	if w := owner.request("POST", "/micropub", url.Values{"content": {"cookie post"}}); w.Code != 401 {
		t.Fatal("session authorized Micropub")
	}
	form := url.Values{"h": {"entry"}, "content": {"Hello **literal** <script>alert(1)</script>\nSecond line"}, "category[]": {"go", "indieweb"}, "mp-slug": {"from-client"}, "ignored-property": {"ignored"}}
	w := apiRequest(a, "POST", "/micropub", "application/x-www-form-urlencoded", form.Encode(), create)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	location := w.Header().Get("Location")
	if location != "http://blog.test/notes/from-client/?lang=ru" {
		t.Fatal(location)
	}
	props := mpRead(t, a, token, location)
	if mpString(props, "content") != form.Get("content") || len(props["category"]) != 2 || props["access_token"] != nil || props["ignored-property"] != nil {
		t.Fatal("source mismatch", props)
	}
	public := newBrowser(a).request("GET", strings.TrimPrefix(location, a.Config.BaseURL), nil).Body.String()
	if strings.Contains(public, "<script>alert(1)</script>") || strings.Contains(public, "<strong>literal") || !strings.Contains(public, "**literal**") || !strings.Contains(public, "p-category") {
		t.Fatal("plaintext interpreted as executable HTML/Markdown or categories absent")
	}
	update := map[string]any{"action": "update", "url": location, "replace": mpProperties{"name": {"New name"}, "content": {map[string]any{"html": "<b>Rich text</b><script>bad()</script>"}}}, "add": mpProperties{"category": {"new", "go"}}, "delete": mpProperties{"category": {"indieweb"}}}
	if w = mpJSON(a, create, update); w.Code != 403 {
		t.Fatal("create token updated")
	}
	if w = apiRequest(a, "GET", "/micropub?q=source&url="+url.QueryEscape(location), "", "", create); w.Code != 403 {
		t.Fatal("create token read source")
	}
	if w = mpJSON(a, token, update); w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	props = mpRead(t, a, token, location)
	if mpString(props, "name") != "New name" || len(props["category"]) != 2 {
		t.Fatal(props)
	}
	content := props["content"][0].(map[string]any)["html"].(string)
	if !strings.Contains(content, "<b>Rich text</b>") || strings.Contains(content, "script") {
		t.Fatal("HTML not sanitized", content)
	}
	if w = mpJSON(a, token, map[string]any{"action": "update", "url": location, "delete": []string{"category"}}); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if props = mpRead(t, a, token, location); len(props["category"]) != 0 {
		t.Fatal("delete property failed")
	}
	if w = mpJSON(a, token, map[string]any{"action": "update", "url": location, "replace": mpProperties{"post-status": {"draft"}}}); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if w = newBrowser(a).request("GET", "/notes/from-client/", nil); w.Code != 404 {
		t.Fatal("draft publicly visible")
	}
	if w = mpJSON(a, token, map[string]any{"action": "update", "url": location, "replace": mpProperties{"post-status": {"published"}}}); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if w = mpJSON(a, create, map[string]any{"action": "delete", "url": location}); w.Code != 403 {
		t.Fatal("create token deleted")
	}
	if w = mpJSON(a, token, map[string]any{"action": "delete", "url": location}); w.Code != 204 {
		t.Fatal(w.Code)
	}
	for _, path := range []string{"/notes/from-client/", "/notes/from-client/index.md"} {
		if w = newBrowser(a).request("GET", path, nil); w.Code != 404 {
			t.Fatal("deleted post exposed", path, w.Code)
		}
	}
	for _, path := range []string{"/", "/all/", "/rss/", "/feed.json", "/sitemap.xml", "/llms.txt"} {
		if strings.Contains(newBrowser(a).request("GET", path, nil).Body.String(), "from-client") {
			t.Fatal("deleted post listed", path)
		}
	}
	p, e := a.mpPost(context.Background(), location)
	if e != nil || !p.Deleted || p.Body == "" {
		t.Fatal("delete destroyed source", e)
	}
	if w = mpJSON(a, token, map[string]any{"action": "undelete", "url": location}); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if w = newBrowser(a).request("GET", "/notes/from-client/", nil); w.Code != 200 {
		t.Fatal("restore failed")
	}
	// Published date, private access and endpoint limits are validated before writes.
	for _, props := range []mpProperties{{"published": {"not-a-date"}}, {"photo": {"javascript:alert(1)"}}, {"content": {true}}, {"post-status": {"other"}}, {"name": {"a", "b"}}} {
		props["content"] = append(props["content"], "test")
		if w = mpJSON(a, token, map[string]any{"properties": props}); w.Code != 400 {
			t.Fatal("invalid properties accepted", props, w.Code)
		}
	}
	if w = mpJSON(a, token, map[string]any{"action": "update", "url": "https://other.example/notes/from-client/", "replace": mpProperties{"name": {"bad"}}}); w.Code != 400 {
		t.Fatal("foreign URL accepted")
	}
	if w = apiRequest(a, "POST", "/micropub", "application/json", `{"properties":{"content":["first"]}} {"second":true}`, token); w.Code != 400 {
		t.Fatal("trailing JSON accepted")
	}
	// Form token transport is supported without persisting it in the post.
	form = url.Values{"access_token": {create}, "content": {"form token"}, "mp-slug": {"form-token"}}
	if w = apiRequest(a, "POST", "/micropub", "application/x-www-form-urlencoded", form.Encode(), ""); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	p, e = a.Store.Post(context.Background(), "form-token", "ru")
	if e != nil || strings.Contains(string(p.Data), create) {
		t.Fatal("token persisted")
	}
	if w = apiRequest(a, "POST", "/micropub", "application/x-www-form-urlencoded", form.Encode(), create); w.Code != 400 {
		t.Fatal("ambiguous token transport")
	}
}
func TestMicropubPhotosRelationsAndEditorRoundtrip(t *testing.T) {
	a := testApp(t)
	token := issueTestToken(t, a, "create update read")
	props := mpProperties{"name": {"Photo article"}, "content": {"Caption"}, "photo": {map[string]any{"value": "https://images.example/pic.jpg", "alt": "A <photo>"}}, "in-reply-to": {"https://other.example/post"}, "featured": {"https://images.example/cover.jpg"}, "post-status": {"draft"}}
	w := mpJSON(a, token, map[string]any{"type": []string{"h-entry"}, "properties": props})
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	location := w.Header().Get("Location")
	p, e := a.mpPost(context.Background(), location)
	if e != nil || p.Kind != "blog" || p.Visible || p.Image != "https://images.example/cover.jpg" {
		t.Fatal(p, e)
	}
	rendered := string(a.md.RenderPost(p))
	if !strings.Contains(rendered, "u-in-reply-to") || !strings.Contains(rendered, "u-photo") || !strings.Contains(rendered, "A &lt;photo&gt;") {
		t.Fatal("missing rich properties", rendered)
	}
	subset := apiRequest(a, "GET", "/micropub?q=source&url="+url.QueryEscape(location)+"&properties[]=photo&properties[]=name", "", "", token)
	obj := readObject(t, subset)
	if obj["type"] != nil || len(obj["properties"].(map[string]any)) != 2 {
		t.Fatal("source filter", obj)
	}
	// Edits through the local editor are authoritative, not stale Micropub metadata.
	p.Body = "Updated **Markdown**"
	p.RawHTML = false
	p.Title = "Local edit"
	if e = a.Store.SavePost(context.Background(), &p); e != nil {
		t.Fatal(e)
	}
	source := mpRead(t, a, token, location)
	if mpString(source, "name") != "Local edit" || !strings.Contains(source["content"][0].(map[string]any)["html"].(string), "<strong>Markdown</strong>") {
		t.Fatal("stale source", source)
	}
	w = mpJSON(a, token, map[string]any{"action": "update", "url": location, "add": mpProperties{"category": {"new"}}})
	if w.Code != 204 {
		t.Fatal(w.Code)
	}
	p, _ = a.mpPost(context.Background(), location)
	if p.Body != "Updated **Markdown**" || p.RawHTML {
		t.Fatal("metadata edit changed Markdown")
	}
	// Visibility cannot be changed as a side effect of another property update.
	p.MembersOnly = true
	_ = a.Store.SavePost(context.Background(), &p)
	w = mpJSON(a, token, map[string]any{"action": "update", "url": location, "replace": mpProperties{"summary": {"Updated summary"}}})
	if w.Code != 204 {
		t.Fatal(w.Code)
	}
	p, _ = a.mpPost(context.Background(), location)
	if !p.MembersOnly || p.Visible {
		t.Fatal("privacy changed")
	}
	// A future publication stays out of public feeds.
	w = mpJSON(a, token, map[string]any{"properties": mpProperties{"content": {"Scheduled"}, "published": {time.Now().Add(time.Hour).Format(time.RFC3339)}}})
	if w.Code != 201 {
		t.Fatal(w.Code)
	}
	p, _ = a.mpPost(context.Background(), w.Header().Get("Location"))
	if p.Public(time.Now()) {
		t.Fatal("scheduled post visible")
	}
}
func TestMicropubMedia(t *testing.T) {
	a := testApp(t)
	token := issueTestToken(t, a, "media")
	noMedia := issueTestToken(t, a, "read")
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 200, A: 255})
	var pngData bytes.Buffer
	_ = png.Encode(&pngData, img)
	upload := func(token string, data []byte) *httptest.ResponseRecorder {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		f, _ := writer.CreateFormFile("file", "image.png")
		_, _ = f.Write(data)
		_ = writer.Close()
		r := httptest.NewRequest("POST", "http://blog.test/micropub/media", &body)
		r.Header.Set("Content-Type", writer.FormDataContentType())
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		return w
	}
	if w := upload(noMedia, pngData.Bytes()); w.Code != 403 {
		t.Fatal("missing scope accepted")
	}
	if w := upload(token, []byte("<svg onload=alert(1)></svg>")); w.Code != 400 {
		t.Fatal("SVG accepted")
	}
	w := upload(token, pngData.Bytes())
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	location := w.Header().Get("Location")
	if !strings.HasPrefix(location, "http://blog.test/media/") {
		t.Fatal(location)
	}
	w = newBrowser(a).request("GET", strings.TrimPrefix(location, a.Config.BaseURL), nil)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || !bytes.Equal(w.Body.Bytes(), pngData.Bytes()) {
		t.Fatal("uploaded bytes unavailable")
	}
	if w = mpJSON(a, token, map[string]any{"properties": mpProperties{"content": {"bad"}}}); w.Code != 403 {
		t.Fatal("media token created post")
	}
}
