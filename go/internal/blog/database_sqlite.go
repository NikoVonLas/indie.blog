package blog

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"modernc.org/sqlite"
)

const dbTimeFormat = "2006-01-02T15:04:05.000000Z"

func init() {
	sqlite.MustRegisterScalarFunction("now", 0, func(_ *sqlite.FunctionContext, _ []driver.Value) (driver.Value, error) {
		return time.Now().UTC().Format(dbTimeFormat), nil
	})
	sqlite.MustRegisterDeterministicScalarFunction("blog_time_add", 2, func(_ *sqlite.FunctionContext, a []driver.Value) (driver.Value, error) {
		var at time.Time
		if e := (dbTime{&at}).Scan(a[0]); e != nil {
			return nil, e
		}
		interval := fmt.Sprint(a[1])
		parts := strings.Fields(interval)
		if len(parts) != 2 {
			return nil, errors.New("invalid database interval")
		}
		n, e := strconv.ParseFloat(parts[0], 64)
		if e != nil {
			return nil, e
		}
		unit := time.Second
		switch strings.TrimSuffix(parts[1], "s") {
		case "second":
		case "minute":
			unit = time.Minute
		case "hour":
			unit = time.Hour
		case "day":
			unit = 24 * time.Hour
		default:
			return nil, errors.New("unsupported database interval")
		}
		return at.Add(time.Duration(n * float64(unit))).UTC().Format(dbTimeFormat), nil
	})
}
func sqliteAddress(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) || strings.ContainsAny(path, "?\x00") {
		return "", errors.New("SQLite requires an absolute file path, e.g. sqlite:/srv/state/blog/blog.db")
	}
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return "", e
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return "", e
	}
	f.Close()
	if e = os.Chmod(path, 0600); e != nil {
		return "", e
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	for _, p := range []string{"busy_timeout(30000)", "journal_mode(WAL)", "foreign_keys(ON)", "synchronous(FULL)", "cache_size(-4096)"} {
		q.Add("_pragma", p)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

var (
	parameterRE       = regexp.MustCompile(`\$([0-9]+)`)
	literalIntervalRE = regexp.MustCompile(`(now\(\)|[a-z_]+\.[a-z_]+)([+-])interval '([0-9]+) ([a-z]+)'`)
	boundIntervalRE   = regexp.MustCompile(`now\(\)([+-])(\$[0-9]+)::interval`)
	rowLockRE         = regexp.MustCompile(` FOR UPDATE(?: OF [a-z]+| SKIP LOCKED)?`)
)

// Compile the application's fixed SQL dialect. This is deliberately not a
// general PostgreSQL-to-SQLite translator; values always remain bound parameters.
// Every application query is exercised by the shared integration suite.
func sqliteQuery(q string, args []any) (string, []any) {
	args = append([]any(nil), args...)
	for i, a := range args {
		switch v := a.(type) {
		case time.Time:
			args[i] = v.UTC().Format(dbTimeFormat)
		case json.RawMessage:
			args[i] = string(v)
		}
	}
	if strings.Contains(q, "to_tsvector('simple',title||' '||body) @@ plainto_tsquery('simple',$6)") {
		q = strings.ReplaceAll(q, "to_tsvector('simple',title||' '||body) @@ plainto_tsquery('simple',$6)", "rowid IN (SELECT rowid FROM posts_search WHERE posts_search MATCH $6)")
		if len(args) > 5 && fmt.Sprint(args[5]) != "" {
			args[5] = sqliteSearch(fmt.Sprint(args[5]))
		}
	}
	q = strings.ReplaceAll(q, "excluded.data || CASE WHEN posts.data ? 'posse' THEN jsonb_build_object('posse',posts.data->'posse') ELSE '{}'::jsonb END", "CASE WHEN json_type(posts.data,'$.posse') IS NOT NULL THEN json_set(excluded.data,'$.posse',json_extract(posts.data,'$.posse')) ELSE excluded.data END")
	q = strings.ReplaceAll(q, "jsonb_set(data,'{posse}',$2::jsonb)", "json_set(data,'$.posse',json($2))")
	q = strings.ReplaceAll(q, "now()+($4 * interval '1 second')", "blog_time_add(now(),$4 || ' seconds')")
	q = literalIntervalRE.ReplaceAllStringFunc(q, func(s string) string {
		m := literalIntervalRE.FindStringSubmatch(s)
		sign := ""
		if m[2] == "-" {
			sign = "-"
		}
		return "blog_time_add(" + m[1] + ",'" + sign + m[3] + " " + m[4] + "')"
	})
	q = boundIntervalRE.ReplaceAllStringFunc(q, func(s string) string {
		m := boundIntervalRE.FindStringSubmatch(s)
		prefix := ""
		if m[1] == "-" {
			prefix = "'-' || "
		}
		return "blog_time_add(now()," + prefix + m[2] + ")"
	})
	q = strings.ReplaceAll(q, "GREATEST(", "MAX(")
	q = rowLockRE.ReplaceAllString(q, "") // BEGIN IMMEDIATE serializes writers, including one-use auth codes.
	q = strings.ReplaceAll(q, "extract(epoch from available_at-now())", "((julianday(available_at)-julianday(now()))*86400)")
	q = parameterRE.ReplaceAllString(q, "?$1")
	return q, args
}
func sqliteSearch(s string) string {
	// Quote each token: FTS syntax in a user's query is never executed as syntax.
	words := strings.FieldsFunc(s, func(r rune) bool {
		return !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r > 127)
	})
	if len(words) == 0 {
		return `""`
	}
	for i, w := range words {
		words[i] = `"` + strings.ReplaceAll(w, `"`, `""`) + `"`
	}
	return strings.Join(words, " AND ")
}
