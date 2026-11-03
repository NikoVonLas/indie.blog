package blog

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

//go:embed schema_sqlite.sql
var sqliteSchema string

var contentTables = []string{"posts", "comments", "reactions", "webmentions", "settings", "users", "sessions", "admin_passkeys", "uploads"}

func (d *Database) migrate(ctx context.Context) error {
	tx, e := d.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	source := schema
	if d.dialect == "sqlite" {
		source = sqliteSchema
	}
	if e = tx.Lock(ctx, 81241937); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, source); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS cache_revision (id integer PRIMARY KEY, revision bigint NOT NULL);
 INSERT INTO cache_revision(id,revision) VALUES(1,0) ON CONFLICT DO NOTHING;`); e != nil {
		return e
	}
	if d.dialect == "postgres" {
		_, e = tx.Exec(ctx, `CREATE OR REPLACE FUNCTION blog_content_changed() RETURNS trigger LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
  BEGIN
   IF TG_OP = 'UPDATE' THEN
    IF TG_TABLE_NAME='posts' AND (to_jsonb(OLD)-'views') IS NOT DISTINCT FROM (to_jsonb(NEW)-'views') THEN RETURN NEW; END IF;
    IF OLD IS NOT DISTINCT FROM NEW THEN RETURN NEW; END IF;
   END IF;
   UPDATE cache_revision SET revision=revision+1 WHERE id=1;
   PERFORM pg_notify('blog_content_changed',current_schema());
   RETURN NULL;
  END $$;`)
		if e != nil {
			return e
		}
		for _, table := range contentTables {
			if _, e = tx.Exec(ctx, fmt.Sprintf(`DROP TRIGGER IF EXISTS blog_content_changed ON %s;
   CREATE TRIGGER blog_content_changed AFTER INSERT OR UPDATE OR DELETE ON %s FOR EACH ROW EXECUTE FUNCTION blog_content_changed();`, table, table)); e != nil {
				return e
			}
			if _, e = tx.Exec(ctx, fmt.Sprintf(`DROP TRIGGER IF EXISTS blog_content_truncated ON %s;
   CREATE TRIGGER blog_content_truncated AFTER TRUNCATE ON %s FOR EACH STATEMENT EXECUTE FUNCTION blog_content_changed();`, table, table)); e != nil {
				return e
			}
		}
	} else {
		for _, table := range contentTables {
			rows, err := tx.Query(ctx, "PRAGMA table_info("+table+")")
			if err != nil {
				return err
			}
			var comparisons []string
			for rows.Next() {
				var id, notnull, pk int
				var name, typ string
				var def any
				if err = rows.Scan(&id, &name, &typ, &notnull, &def, &pk); err != nil {
					rows.Close()
					return err
				}
				if table == "posts" && name == "views" {
					continue
				}
				comparisons = append(comparisons, fmt.Sprintf("OLD.\"%s\" IS NOT NEW.\"%s\"", name, name))
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			for _, op := range []string{"INSERT", "UPDATE", "DELETE"} {
				when := ""
				if op == "UPDATE" {
					when = " WHEN " + strings.Join(comparisons, " OR ")
				}
				q := fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS cache_%s_%s AFTER %s ON %s%s BEGIN UPDATE cache_revision SET revision=revision+1 WHERE id=1; END;`, table, op, op, table, when)
				if _, e = tx.Exec(ctx, q); e != nil {
					return e
				}
			}
		}
	}
	if _, e = tx.Exec(ctx, "INSERT INTO schema_versions(version) VALUES(8) ON CONFLICT DO NOTHING"); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

func (d *Database) refreshRevision(ctx context.Context) {
	d.changeMu.Lock()
	defer d.changeMu.Unlock()
	var revision int64
	err := d.pool.QueryRowContext(ctx, "SELECT revision FROM cache_revision WHERE id=1").Scan(&revision)
	if err != nil {
		d.ready.Store(false)
		return
	}
	if revision != d.revision || !d.ready.Load() {
		d.revision = revision
		d.epoch.Add(1)
	}
	// Publication at a future date is a content change, not a cache TTL.
	if revision != d.scheduledRevision || d.nextPublication.Load() <= time.Now().UnixNano() {
		var next any
		if err = d.pool.QueryRowContext(ctx, "SELECT min(published_at) FROM posts WHERE visible AND NOT deleted AND published_at>now()").Scan(&next); err != nil {
			d.ready.Store(false)
			return
		}
		var at time.Time
		if next != nil {
			if err = (dbTime{&at}).Scan(next); err != nil {
				d.ready.Store(false)
				return
			}
		}
		if previous := d.nextPublication.Load(); previous > 0 && previous <= time.Now().UnixNano() {
			d.epoch.Add(1)
		}
		deadline := int64(1<<63 - 1)
		if !at.IsZero() {
			deadline = at.UnixNano()
		}
		d.nextPublication.Store(deadline)
		d.scheduledRevision = revision
	}
	d.ready.Store(true)
}
func (d *Database) startChanges(ctx context.Context) {
	d.startOnce.Do(func() {
		watch, cancel := context.WithCancel(context.Background())
		d.stop = cancel
		d.stopped = make(chan struct{})
		if d.dialect == "postgres" {
			conn, scope := d.listenChanges(ctx)
			if conn != nil {
				d.refreshRevision(ctx)
			}
			go d.watchPostgres(watch, conn, scope)
			return
		}
		d.refreshRevision(ctx)
		go func() {
			defer close(d.stopped)
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-watch.Done():
					return
				case <-ticker.C:
					check, done := context.WithTimeout(watch, 3*time.Second)
					d.gate.Lock()
					d.refreshRevision(check)
					d.gate.Unlock()
					done()
				}
			}
		}()
	})
}

func (d *Database) listenChanges(ctx context.Context) (*pgx.Conn, string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, d.address)
	if err != nil {
		return nil, ""
	}
	var scope string
	if _, err = conn.Exec(ctx, "LISTEN blog_content_changed"); err == nil {
		err = conn.QueryRow(ctx, "SELECT current_schema()").Scan(&scope)
	}
	if err != nil {
		_ = conn.Close(ctx)
		return nil, ""
	}
	return conn, scope
}

func (d *Database) watchPostgres(ctx context.Context, conn *pgx.Conn, scope string) {
	defer close(d.stopped)
	defer func() {
		if conn != nil {
			closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = conn.Close(closeCtx)
			cancel()
		}
	}()
	for ctx.Err() == nil {
		if conn == nil {
			d.ready.Store(false)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			conn, scope = d.listenChanges(ctx)
			if conn == nil {
				continue
			}
			d.gate.Lock()
			d.refreshRevision(ctx)
			d.gate.Unlock()
		}
		deadline := time.Now().Add(time.Minute) // Connection-health check, not cache expiry.
		if next := d.nextPublication.Load(); next > 0 && next < deadline.UnixNano() {
			deadline = time.Unix(0, next)
		}
		wait, cancel := context.WithDeadline(ctx, deadline)
		notification, err := conn.WaitForNotification(wait)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			d.ready.Store(false)
			closeCtx, done := context.WithTimeout(context.Background(), 3*time.Second)
			_ = conn.Close(closeCtx)
			done()
			conn = nil
			continue
		}
		if notification != nil && notification.Payload != scope {
			continue
		}
		check, done := context.WithTimeout(ctx, 3*time.Second)
		d.gate.Lock()
		d.refreshRevision(check)
		d.gate.Unlock()
		done()
	}
}
func (d *Database) cacheEpoch() uint64 {
	if at := d.nextPublication.Load(); at > 0 && at <= time.Now().UnixNano() {
		if d.nextPublication.CompareAndSwap(at, 0) {
			d.epoch.Add(1)
		}
	}
	return d.epoch.Load()
}
