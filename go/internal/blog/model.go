package blog

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type Post struct {
	ID          string          `json:"id"`
	Slug        string          `json:"slug"`
	Kind        string          `json:"kind"`
	Lang        string          `json:"lang"`
	Title       string          `json:"title"`
	Subtitle    string          `json:"subtitle"`
	Body        string          `json:"body"`
	Summary     string          `json:"summary"`
	Image       string          `json:"image"`
	Author      string          `json:"author"`
	ExternalURL string          `json:"external_url"`
	CSS         string          `json:"css"`
	RawHTML     bool            `json:"raw_html"`
	Deleted     bool            `json:"deleted"`
	Visible     bool            `json:"visible"`
	MembersOnly bool            `json:"members_only"`
	Commentable bool            `json:"commentable"`
	OnHome      bool            `json:"on_home"`
	PublishedAt time.Time       `json:"published_at"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	Views       int64           `json:"views"`
	Data        json.RawMessage `json:"data"`
}

var Kinds = []string{"blog", "notes", "world", "gallery", "inside", "365", "reply", "bookmark", "repost"}
var slugPattern = regexp.MustCompile(`^[\p{L}\p{N}_-]{1,128}$`)
var langPattern = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z0-9]{2,8})?$`)

func (p Post) URL() string {
	return "/" + p.Kind + "/" + url.PathEscape(p.Slug) + "/?lang=" + url.QueryEscape(p.Lang)
}
func (p Post) Public(now time.Time) bool { return !p.Deleted && p.Visible && !p.PublishedAt.After(now) }
func (p Post) Readable(u *User) bool {
	if p.Deleted {
		return false
	}
	if u != nil && u.Admin {
		return true
	}
	return p.Public(time.Now()) && (!p.MembersOnly || u != nil && (u.Member || u.Admin))
}
func (p Post) Valid() bool {
	validKind := false
	for _, k := range Kinds {
		if p.Kind == k {
			validKind = true
		}
	}
	return validKind && slugPattern.MatchString(p.Slug) && langPattern.MatchString(p.Lang) &&
		len(p.Title) <= 2048 && len(p.Body) <= 1000000 && p.PublishedAt.Year() >= 1970 && p.PublishedAt.Year() < 10000 &&
		safeURL(p.Image) && safeURL(p.ExternalURL)
}
func safeURL(s string) bool {
	if s == "" {
		return true
	}
	u, e := url.Parse(s)
	return e == nil && u.User == nil && ((u.Scheme == "https" || u.Scheme == "http") && u.Host != "" || u.Scheme == "" && strings.HasPrefix(s, "/") && !strings.HasPrefix(s, "//"))
}

type User struct {
	ID, Email, Name, PasswordHash, Avatar, ProfileURL string
	Admin, Banned, Member                             bool
}
type Comment struct {
	ID, PostID, UserID, Author, AuthorURL, Body, Block, ReplyTo string
	Deleted, Pinned                                             bool
	CreatedAt                                                   time.Time
	Votes                                                       int
}

func randomToken() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}
func hash(s string) string { v := sha256.Sum256([]byte(s)); return hex.EncodeToString(v[:]) }
