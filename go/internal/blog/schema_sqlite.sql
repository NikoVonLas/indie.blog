CREATE TABLE IF NOT EXISTS schema_versions (version integer PRIMARY KEY);
CREATE TABLE IF NOT EXISTS settings (key text PRIMARY KEY, value text NOT NULL);
CREATE TABLE IF NOT EXISTS users (
 id text PRIMARY KEY, email text UNIQUE, name text NOT NULL,
 password_hash text NOT NULL, avatar text NOT NULL DEFAULT '',
 is_admin boolean NOT NULL DEFAULT false, is_banned boolean NOT NULL DEFAULT false,
 is_member boolean NOT NULL DEFAULT true, profile_url text NOT NULL DEFAULT '',
 created_at text NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f000Z','now'))
);
CREATE TABLE IF NOT EXISTS sessions (
 token_hash text PRIMARY KEY, user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 expires_at text NOT NULL,
 passkey_id text REFERENCES admin_passkeys(id) ON DELETE CASCADE, created_at text NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f000Z','now'))
);
CREATE TABLE IF NOT EXISTS posts (
 id text PRIMARY KEY, slug text NOT NULL, kind text NOT NULL, lang text NOT NULL,
 title text NOT NULL, subtitle text NOT NULL DEFAULT '', body text NOT NULL DEFAULT '',
 summary text NOT NULL DEFAULT '', image text NOT NULL DEFAULT '', author text NOT NULL DEFAULT '',
 external_url text NOT NULL DEFAULT '', css text NOT NULL DEFAULT '',
 raw_html boolean NOT NULL DEFAULT false, visible boolean NOT NULL DEFAULT false,
 members_only boolean NOT NULL DEFAULT false, commentable boolean NOT NULL DEFAULT true,
 on_home boolean NOT NULL DEFAULT true, published_at text NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f000Z','now')),
 created_at text NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f000Z','now')), updated_at text NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f000Z','now')),
 deleted boolean NOT NULL DEFAULT false, views bigint NOT NULL DEFAULT 0, data text NOT NULL DEFAULT '{}',
 UNIQUE(slug, lang), CHECK (kind IN ('blog','notes','world','gallery','inside','365','reply','bookmark','repost'))
);
CREATE INDEX IF NOT EXISTS posts_listing ON posts(lang, kind, published_at DESC) WHERE visible;
CREATE TABLE IF NOT EXISTS comments (
 id text PRIMARY KEY, post_id text NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
 user_id text REFERENCES users(id) ON DELETE SET NULL, author_name text NOT NULL, author_url text NOT NULL DEFAULT '',
 body text NOT NULL, block text NOT NULL DEFAULT '',
 reply_to text REFERENCES comments(id), deleted boolean NOT NULL DEFAULT false,
 pinned boolean NOT NULL DEFAULT false, created_at text NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f000Z','now'))
);
CREATE INDEX IF NOT EXISTS comments_post ON comments(post_id, created_at);
CREATE TABLE IF NOT EXISTS reactions (
 post_id text NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
 target text NOT NULL, actor text NOT NULL, created_at text NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f000Z','now')),
 PRIMARY KEY(post_id,target,actor)
);
CREATE TABLE IF NOT EXISTS subscribers (
 id text PRIMARY KEY, email text UNIQUE, lang text NOT NULL,
 token_hash text UNIQUE NOT NULL, confirmed boolean NOT NULL DEFAULT false,
 created_at text NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f000Z','now'))
);
CREATE TABLE IF NOT EXISTS rate_limits (
 key text PRIMARY KEY, window_start text NOT NULL, hits integer NOT NULL
);
CREATE TABLE IF NOT EXISTS outbox (
 id text PRIMARY KEY, dedup_key text UNIQUE NOT NULL, recipient text NOT NULL,
 subject text NOT NULL, body text NOT NULL, attempts integer NOT NULL DEFAULT 0,
 available_at text NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f000Z','now')), locked_until text,
 delivered_at text, created_at text NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f000Z','now'))
);
INSERT INTO schema_versions(version) VALUES(1) ON CONFLICT DO NOTHING;
CREATE TABLE IF NOT EXISTS uploads (
 id text PRIMARY KEY, user_id text NOT NULL REFERENCES users(id),
 content_type text NOT NULL, body blob NOT NULL,
 created_at text NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f000Z','now'))
);
INSERT INTO schema_versions(version) VALUES(2) ON CONFLICT DO NOTHING;
CREATE TABLE IF NOT EXISTS webmentions (
 id text PRIMARY KEY, direction text NOT NULL CHECK(direction IN ('in','out')),
 post_id text NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
 source text NOT NULL, target text NOT NULL,
 state text NOT NULL DEFAULT 'queued', moderation text NOT NULL DEFAULT 'pending',
 title text NOT NULL DEFAULT '', kind text NOT NULL DEFAULT 'mention',
 attempts integer NOT NULL DEFAULT 0, available_at text NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f000Z','now')),
 locked_until text, claim text NOT NULL DEFAULT '',
 detail text NOT NULL DEFAULT '', created_at text NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f000Z','now')), updated_at text NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f000Z','now'))
);
CREATE INDEX IF NOT EXISTS webmentions_queue ON webmentions(available_at) WHERE state='queued';
CREATE INDEX IF NOT EXISTS webmentions_post ON webmentions(post_id,created_at);
INSERT INTO schema_versions(version) VALUES(3) ON CONFLICT DO NOTHING;
CREATE TABLE IF NOT EXISTS indieauth_requests (
 id text PRIMARY KEY, user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 session_hash text NOT NULL, payload text NOT NULL, expires_at text NOT NULL
);
CREATE TABLE IF NOT EXISTS indieauth_codes (
 code_hash text PRIMARY KEY, user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 client_id text NOT NULL, redirect_uri text NOT NULL, challenge text NOT NULL,
 scope text NOT NULL, me text NOT NULL, expires_at text NOT NULL
);
CREATE TABLE IF NOT EXISTS indieauth_tokens (
 id text PRIMARY KEY, token_hash text UNIQUE NOT NULL, user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 client_id text NOT NULL, scope text NOT NULL, me text NOT NULL,
 created_at text NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f000Z','now')), expires_at text NOT NULL
);
CREATE INDEX IF NOT EXISTS indieauth_tokens_user ON indieauth_tokens(user_id,created_at);
INSERT INTO schema_versions(version) VALUES(4) ON CONFLICT DO NOTHING;

CREATE UNIQUE INDEX IF NOT EXISTS users_profile_url ON users(profile_url) WHERE profile_url<>'';
CREATE TABLE IF NOT EXISTS indieauth_logins (
 state_hash text PRIMARY KEY, browser_hash text NOT NULL, payload text NOT NULL,
 expires_at text NOT NULL
);
CREATE INDEX IF NOT EXISTS indieauth_logins_expiry ON indieauth_logins(expires_at);
INSERT INTO schema_versions(version) VALUES(5) ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS admin_passkeys (
 id text PRIMARY KEY, user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 credential text NOT NULL, label text NOT NULL, created_at text NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f000Z','now')), last_used_at text
);
CREATE TABLE IF NOT EXISTS admin_setup_grants (
 token_hash text PRIMARY KEY, user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 expires_at text NOT NULL
);
CREATE TABLE IF NOT EXISTS passkey_ceremonies (
 browser_hash text PRIMARY KEY, kind text NOT NULL, payload text NOT NULL, expires_at text NOT NULL
);
INSERT INTO schema_versions(version) VALUES(6) ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS syndications (
 id text PRIMARY KEY, post_id text NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
 target text NOT NULL, state text NOT NULL DEFAULT 'queued' CHECK(state IN ('queued','sending','sent','failed','uncertain','cancelled')),
 body text NOT NULL, revision text NOT NULL, url text NOT NULL DEFAULT '', detail text NOT NULL DEFAULT '',
 attempts integer NOT NULL DEFAULT 0, available_at text NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f000Z','now')),
 created_at text NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f000Z','now')), updated_at text NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f000Z','now')),
 UNIQUE(post_id,target)
);
CREATE INDEX IF NOT EXISTS syndications_queue ON syndications(available_at) WHERE state='queued';
INSERT INTO schema_versions(version) VALUES(7) ON CONFLICT DO NOTHING;
CREATE VIRTUAL TABLE IF NOT EXISTS posts_search USING fts5(title,body,content='posts',content_rowid='rowid',tokenize='unicode61');
CREATE TRIGGER IF NOT EXISTS posts_fts_insert AFTER INSERT ON posts BEGIN
 INSERT INTO posts_search(rowid,title,body) VALUES(new.rowid,new.title,new.body);
END;
CREATE TRIGGER IF NOT EXISTS posts_fts_delete AFTER DELETE ON posts BEGIN
 INSERT INTO posts_search(posts_search,rowid,title,body) VALUES('delete',old.rowid,old.title,old.body);
END;
CREATE TRIGGER IF NOT EXISTS posts_fts_update AFTER UPDATE OF title,body ON posts
 WHEN old.title IS NOT new.title OR old.body IS NOT new.body BEGIN
 INSERT INTO posts_search(posts_search,rowid,title,body) VALUES('delete',old.rowid,old.title,old.body);
 INSERT INTO posts_search(rowid,title,body) VALUES(new.rowid,new.title,new.body);
END;
