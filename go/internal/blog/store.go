package blog

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"time"
)

//go:embed schema.sql
var schema string

type Store struct{ DB *Database }

func Open(ctx context.Context, dsn string) (*Store, error) {
	db, err := openDatabase(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return &Store{DB: db}, nil
}
func (s *Store) Migrate(ctx context.Context) error { return s.DB.migrate(ctx) }

const postCols = `id,slug,kind,lang,title,subtitle,body,summary,image,author,external_url,css,raw_html,visible,members_only,commentable,on_home,published_at,created_at,updated_at,views,data,deleted`

func scanPost(row Row) (Post, error) {
	var p Post
	e := row.Scan(&p.ID, &p.Slug, &p.Kind, &p.Lang, &p.Title, &p.Subtitle, &p.Body, &p.Summary, &p.Image, &p.Author, &p.ExternalURL, &p.CSS, &p.RawHTML, &p.Visible, &p.MembersOnly, &p.Commentable, &p.OnHome, &p.PublishedAt, &p.CreatedAt, &p.UpdatedAt, &p.Views, &p.Data, &p.Deleted)
	return p, e
}
func (s *Store) Post(ctx context.Context, slug, lang string) (Post, error) {
	return scanPost(s.DB.QueryRow(ctx, "SELECT "+postCols+" FROM posts WHERE slug=$1 AND lang=$2", slug, lang))
}
func (s *Store) PostID(ctx context.Context, id string) (Post, error) {
	return scanPost(s.DB.QueryRow(ctx, "SELECT "+postCols+" FROM posts WHERE id=$1", id))
}

type Filter struct {
	Lang, Kind, Search                    string
	ExcludeID                             string
	Admin, PublicOnly, Home, HomeTimeline bool
	Limit, Offset                         int
}

func (s *Store) Posts(ctx context.Context, f Filter) ([]Post, error) {
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 30
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	rows, e := s.DB.Query(ctx, `SELECT `+postCols+` FROM posts WHERE
 ($1='' OR lang=$1) AND ($2='' OR kind=$2) AND ($3 OR (visible AND NOT deleted AND published_at<=now()))
 AND (NOT $4 OR NOT members_only) AND (NOT $5 OR on_home) AND (NOT $10 OR kind IN ('blog','notes'))
 AND ($6='' OR to_tsvector('simple',title||' '||body) @@ plainto_tsquery('simple',$6))
 AND ($9='' OR id<>$9)
 ORDER BY published_at DESC,id LIMIT $7 OFFSET $8`, f.Lang, f.Kind, f.Admin, f.PublicOnly, f.Home, f.Search, f.Limit, f.Offset, f.ExcludeID, f.HomeTimeline)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Post{}
	for rows.Next() {
		p, e := scanPost(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func (s *Store) SavePost(ctx context.Context, p *Post) error {
	return savePost(ctx, s.DB, p)
}

// SavePostEdit preserves the current trash state even when the editor submitted
// a page opened before another request deleted or restored the publication.
func (s *Store) SavePostEdit(ctx context.Context, p *Post) error {
	if p.ID == "" {
		return s.SavePost(ctx, p)
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = tx.QueryRow(ctx, `SELECT deleted FROM posts WHERE id=$1 FOR UPDATE`, p.ID).Scan(&p.Deleted); err != nil {
		return err
	}
	if err = savePost(ctx, tx, p); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SetPostDeleted changes only the trash state, preserving edits made in another
// browser tab. The row lock also serializes this action with Micropub and POSSE.
func (s *Store) SetPostDeleted(ctx context.Context, id string, deleted bool) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var current bool
	if err = tx.QueryRow(ctx, `SELECT deleted FROM posts WHERE id=$1 FOR UPDATE`, id).Scan(&current); err != nil {
		return err
	}
	if current != deleted {
		if _, err = tx.Exec(ctx, `UPDATE posts SET deleted=$1,updated_at=now() WHERE id=$2`, deleted, id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

type executor interface {
	Exec(context.Context, string, ...any) (Result, error)
}

func savePost(ctx context.Context, db executor, p *Post) error {
	if !p.Valid() {
		return errors.New("invalid post")
	}
	if p.ID == "" {
		p.ID = randomToken()
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	p.UpdatedAt = time.Now().UTC()
	if len(p.Data) == 0 {
		p.Data = []byte(`{}`)
	}
	_, e := db.Exec(ctx, `INSERT INTO posts (`+postCols+`) VALUES
 ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)
 ON CONFLICT(id) DO UPDATE SET slug=excluded.slug,kind=excluded.kind,lang=excluded.lang,title=excluded.title,
 subtitle=excluded.subtitle,body=excluded.body,summary=excluded.summary,image=excluded.image,author=excluded.author,
 external_url=excluded.external_url,css=excluded.css,raw_html=excluded.raw_html,visible=excluded.visible,
 members_only=excluded.members_only,commentable=excluded.commentable,on_home=excluded.on_home,
 published_at=excluded.published_at,updated_at=excluded.updated_at,data=excluded.data || CASE WHEN posts.data ? 'posse' THEN jsonb_build_object('posse',posts.data->'posse') ELSE '{}'::jsonb END,deleted=excluded.deleted`,
		p.ID, p.Slug, p.Kind, p.Lang, p.Title, p.Subtitle, p.Body, p.Summary, p.Image, p.Author, p.ExternalURL, p.CSS, p.RawHTML, p.Visible, p.MembersOnly, p.Commentable, p.OnHome, p.PublishedAt, p.CreatedAt, p.UpdatedAt, p.Views, p.Data, p.Deleted)
	return e
}
func (s *Store) ImportPosts(ctx context.Context, posts []Post) error {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	for i := range posts {
		if e = savePost(ctx, tx, &posts[i]); e != nil {
			return fmt.Errorf("post %q: %w", posts[i].Slug, e)
		}
	}
	return tx.Commit(ctx)
}
func scanUser(row Row) (*User, error) {
	var u User
	e := row.Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash, &u.Avatar, &u.Admin, &u.Banned, &u.Member, &u.ProfileURL)
	return &u, e
}

const userCols = `id,coalesce(email,''),name,password_hash,avatar,is_admin,is_banned,is_member,profile_url`

func (s *Store) UserEmail(ctx context.Context, email string) (*User, error) {
	return scanUser(s.DB.QueryRow(ctx, "SELECT "+userCols+" FROM users WHERE email=$1", email))
}
func (s *Store) Session(ctx context.Context, token string) (*User, error) {
	return scanUser(s.DB.QueryRow(ctx, `SELECT u.id,coalesce(u.email,''),u.name,u.password_hash,u.avatar,u.is_admin,u.is_banned,u.is_member,u.profile_url FROM users u JOIN sessions s ON s.user_id=u.id WHERE s.token_hash=$1 AND s.expires_at>now() AND NOT u.is_banned AND (NOT u.is_admin OR EXISTS(SELECT 1 FROM admin_passkeys k WHERE k.id=s.passkey_id AND k.user_id=u.id))`, hash(token)))
}
func (s *Store) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, error) {
	var n int
	e := s.DB.QueryRow(ctx, `INSERT INTO rate_limits(key,window_start,hits) VALUES($1,now(),1)
 ON CONFLICT(key) DO UPDATE SET hits=CASE WHEN rate_limits.window_start < now()-$2::interval THEN 1 ELSE rate_limits.hits+1 END,
 window_start=CASE WHEN rate_limits.window_start < now()-$2::interval THEN now() ELSE rate_limits.window_start END RETURNING hits`, key, fmt.Sprintf("%f seconds", window.Seconds())).Scan(&n)
	return n <= limit, e
}
func (s *Store) Comments(ctx context.Context, post string) ([]Comment, error) {
	rows, e := s.DB.Query(ctx, `SELECT c.id,c.post_id,coalesce(c.user_id,''),c.author_name,c.author_url,c.body,c.block,coalesce(c.reply_to,''),c.deleted,c.pinned,c.created_at,
 (SELECT count(*) FROM reactions r WHERE r.post_id=c.post_id AND r.target='comment:'||c.id) FROM comments c WHERE c.post_id=$1 ORDER BY c.pinned DESC,c.created_at`, post)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Comment{}
	for rows.Next() {
		var c Comment
		if e = rows.Scan(&c.ID, &c.PostID, &c.UserID, &c.Author, &c.AuthorURL, &c.Body, &c.Block, &c.ReplyTo, &c.Deleted, &c.Pinned, &c.CreatedAt, &c.Votes); e != nil {
			return nil, e
		}
		if c.Deleted {
			c.Body = "Комментарий удалён"
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
