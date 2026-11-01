package blog

// The application uses this small SQL interface, not driver-specific rows,
// transactions or errors. Dialect-specific behavior lives in database_*.go.
import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"modernc.org/sqlite"
)

var ErrNoRows = sql.ErrNoRows
var ErrConflict = errors.New("unique constraint conflict")

type Row interface{ Scan(...any) error }
type Rows interface {
	Row
	Next() bool
	Close()
	Err() error
}
type Result int64

func (r Result) RowsAffected() int64 { return int64(r) }

type Transaction interface {
	executor
	Lock(context.Context, int64) error
	Query(context.Context, string, ...any) (Rows, error)
	QueryRow(context.Context, string, ...any) Row
	Commit(context.Context) error
	Rollback(context.Context) error
}
type Database struct {
	pool    *sql.DB
	dialect string
	address string
	// Commit and cache-hit barriers make local writes visible before a successful
	// mutation returns. Triggers also catch writes from CLI/other processes.
	gate              sync.RWMutex
	writeMu           sync.Mutex
	changeMu          sync.Mutex
	revision          int64
	scheduledRevision int64
	epoch             atomic.Uint64
	nextPublication   atomic.Int64
	startOnce         sync.Once
	ready             atomic.Bool
	queries           atomic.Int64
	stop              context.CancelFunc
	stopped           chan struct{}
}

func openDatabase(ctx context.Context, dsn string) (*Database, error) {
	dialect, driver, address := "postgres", "pgx", dsn
	if strings.HasPrefix(dsn, "sqlite:") {
		dialect, driver = "sqlite", "sqlite"
		var err error
		address, err = sqliteAddress(strings.TrimPrefix(dsn, "sqlite:"))
		if err != nil {
			return nil, err
		}
	} else if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") {
		return nil, errors.New("DATABASE_URL must use postgres://, postgresql:// or sqlite:/absolute/path.db")
	}
	pool, err := sql.Open(driver, address)
	if err != nil {
		return nil, err
	}
	pool.SetMaxOpenConns(8)
	pool.SetMaxIdleConns(4)
	if err = pool.PingContext(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("cannot connect to %s database", dialect)
	}
	return &Database{pool: pool, dialect: dialect, address: address}, nil
}
func (d *Database) Close() {
	if d.stop != nil {
		d.stop()
		<-d.stopped
	}
	d.pool.Close()
}
func (d *Database) Ping(ctx context.Context) error { return d.pool.PingContext(ctx) }
func (d *Database) prepare(q string, args []any) (string, []any) {
	d.queries.Add(1)
	if d.dialect == "sqlite" {
		return sqliteQuery(q, args)
	}
	return q, args
}
func dbError(err error) error {
	var pg *pgconn.PgError
	var sq *sqlite.Error
	if errors.As(err, &pg) && pg.Code == "23505" || errors.As(err, &sq) && (sq.Code() == 1555 || sq.Code() == 2067) {
		return fmt.Errorf("%w: %w", ErrConflict, err)
	}
	return err
}
func isWrite(q string) bool {
	q = strings.ToUpper(strings.TrimSpace(q))
	return !strings.HasPrefix(q, "SELECT ") && !strings.HasPrefix(q, "PRAGMA ") && !strings.HasPrefix(q, "EXPLAIN ")
}
func (d *Database) afterWrite() {
	if !d.ready.Load() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d.refreshRevision(ctx)
}
func (d *Database) Exec(ctx context.Context, q string, args ...any) (Result, error) {
	write := isWrite(q)
	if write {
		d.writeMu.Lock()
		defer d.writeMu.Unlock()
		d.gate.Lock()
		defer d.gate.Unlock()
	}
	q, args = d.prepare(q, args)
	r, e := d.pool.ExecContext(ctx, q, args...)
	if write {
		d.afterWrite()
	}
	if e != nil {
		return 0, dbError(e)
	}
	n, e := r.RowsAffected()
	return Result(n), e
}
func (d *Database) Query(ctx context.Context, q string, args ...any) (Rows, error) {
	write := isWrite(q)
	if write {
		d.writeMu.Lock()
		d.gate.Lock()
	}
	q, args = d.prepare(q, args)
	r, e := d.pool.QueryContext(ctx, q, args...)
	if e != nil {
		if write {
			d.afterWrite()
			d.gate.Unlock()
			d.writeMu.Unlock()
		}
		return nil, dbError(e)
	}
	rows := &dbRows{Rows: r}
	if write {
		rows.done = func() { d.afterWrite(); d.gate.Unlock(); d.writeMu.Unlock() }
	}
	return rows, nil
}
func (d *Database) QueryRow(ctx context.Context, q string, args ...any) Row {
	// Lazy execution lets Scan close RETURNING rows before notifying caches.
	return rowFunc(func(dest ...any) error {
		write := isWrite(q)
		if write {
			d.writeMu.Lock()
			defer d.writeMu.Unlock()
			d.gate.Lock()
			defer d.gate.Unlock()
		}
		query, values := d.prepare(q, args)
		e := scanSQL(d.pool.QueryRowContext(ctx, query, values...), dest...)
		if write {
			d.afterWrite()
		}
		return dbError(e)
	})
}
func (d *Database) Begin(ctx context.Context) (Transaction, error) {
	d.writeMu.Lock()
	conn, e := d.pool.Conn(ctx)
	if e != nil {
		d.writeMu.Unlock()
		return nil, e
	}
	begin := "BEGIN"
	if d.dialect == "sqlite" {
		begin = "BEGIN IMMEDIATE"
	}
	if _, e = conn.ExecContext(ctx, begin); e != nil {
		conn.Close()
		d.writeMu.Unlock()
		return nil, e
	}
	return &dbTx{db: d, conn: conn}, nil
}

type dbTx struct {
	db   *Database
	conn *sql.Conn
	done bool
}

func (t *dbTx) Lock(ctx context.Context, key int64) error {
	if t.db.dialect == "sqlite" {
		return nil
	} // BEGIN IMMEDIATE already owns the writer lock.
	_, err := t.conn.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", key)
	return err
}

func (t *dbTx) Exec(ctx context.Context, q string, args ...any) (Result, error) {
	q, args = t.db.prepare(q, args)
	r, e := t.conn.ExecContext(ctx, q, args...)
	if e != nil {
		return 0, dbError(e)
	}
	n, e := r.RowsAffected()
	return Result(n), e
}
func (t *dbTx) Query(ctx context.Context, q string, args ...any) (Rows, error) {
	q, args = t.db.prepare(q, args)
	r, e := t.conn.QueryContext(ctx, q, args...)
	if e != nil {
		return nil, dbError(e)
	}
	return &dbRows{Rows: r}, nil
}
func (t *dbTx) QueryRow(ctx context.Context, q string, args ...any) Row {
	return rowFunc(func(dest ...any) error {
		q, args := t.db.prepare(q, args)
		return dbError(scanSQL(t.conn.QueryRowContext(ctx, q, args...), dest...))
	})
}
func (t *dbTx) Commit(ctx context.Context) error {
	if t.done {
		return sql.ErrTxDone
	}
	t.done = true
	defer t.db.writeMu.Unlock()
	t.db.gate.Lock()
	defer t.db.gate.Unlock()
	_, e := t.conn.ExecContext(ctx, "COMMIT")
	// A failed COMMIT must never return a live transaction to the pool.
	if e != nil {
		_, _ = t.conn.ExecContext(context.Background(), "ROLLBACK")
	}
	t.conn.Close()
	t.db.afterWrite()
	return dbError(e)
}
func (t *dbTx) Rollback(ctx context.Context) error {
	if t.done {
		return sql.ErrTxDone
	}
	t.done = true
	defer t.db.writeMu.Unlock()
	cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, e := t.conn.ExecContext(cleanup, "ROLLBACK")
	t.conn.Close()
	return e
}

type rowFunc func(...any) error

func (r rowFunc) Scan(dest ...any) error { return r(dest...) }

type dbRows struct {
	*sql.Rows
	done func()
	once sync.Once
}

func (r *dbRows) Close() {
	_ = r.Rows.Close()
	r.once.Do(func() {
		if r.done != nil {
			r.done()
		}
	})
}
func (r *dbRows) Next() bool {
	ok := r.Rows.Next()
	if !ok {
		r.Close()
	}
	return ok
}
func (r *dbRows) Scan(dest ...any) error { return scanSQL(r.Rows, dest...) }
func scanSQL(row Row, dest ...any) error {
	args := append([]any(nil), dest...)
	for i, v := range args {
		switch p := v.(type) {
		case *time.Time:
			args[i] = dbTime{p}
		case *json.RawMessage:
			args[i] = jsonScan{p}
		}
	}
	return row.Scan(args...)
}

type jsonScan struct{ dst *json.RawMessage }

func (v jsonScan) Scan(src any) error {
	switch x := src.(type) {
	case string:
		*v.dst = append((*v.dst)[:0], x...)
	case []byte:
		*v.dst = append((*v.dst)[:0], x...)
	default:
		return fmt.Errorf("invalid JSON value %T", src)
	}
	return nil
}

type dbTime struct{ dst *time.Time }

func (v dbTime) Scan(src any) error {
	if t, ok := src.(time.Time); ok {
		*v.dst = t
		return nil
	}
	var s string
	switch x := src.(type) {
	case string:
		s = x
	case []byte:
		s = string(x)
	default:
		return fmt.Errorf("invalid database time %T", src)
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999+00:00", "2006-01-02 15:04:05.999999999"} {
		if t, e := time.Parse(layout, s); e == nil {
			*v.dst = t
			return nil
		}
	}
	return errors.New("invalid database timestamp")
}
