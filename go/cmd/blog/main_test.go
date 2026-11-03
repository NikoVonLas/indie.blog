package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NikoVonLas/indie.blog/go/internal/blog"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdminSetupProtectedFile(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	ctx := context.Background()
	if dsn == "" || dsn == "sqlite" {
		dsn = "sqlite:" + filepath.Join(t.TempDir(), "blog.db")
	} else {
		root, err := pgxpool.New(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		suffix := make([]byte, 16)
		if _, err = rand.Read(suffix); err != nil {
			t.Fatal(err)
		}
		schema := "setup_" + hex.EncodeToString(suffix)
		if _, err = root.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
			t.Fatal(err)
		}
		defer root.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		parsed, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		q := parsed.Query()
		q.Set("search_path", schema)
		parsed.RawQuery = q.Encode()
		dsn = parsed.String()
	}
	store, err := blog.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB.Close()
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("BASE_URL", "http://localhost:8080")
	t.Setenv("OWNER_EMAIL", "owner@example.test")
	t.Setenv("OWNER_NAME", "Owner")
	file := filepath.Join(t.TempDir(), "setup.url")
	t.Setenv("ADMIN_SETUP_FILE", file)
	previousArgs := os.Args
	os.Args = []string{"blog", "admin-setup"}
	defer func() { os.Args = previousArgs }()
	if err = run(); err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(file)
	if err != nil || stat.Mode().Perm() != 0600 {
		t.Fatal("setup file must be 0600")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse(strings.TrimSpace(string(raw)))
	if err != nil || target.Scheme != "http" || target.Host != "localhost:8080" || target.Path != "/admin/setup" || len(target.Fragment) != 64 || target.RawQuery != "" {
		t.Fatal("invalid setup URL")
	}
	digest := sha256.Sum256([]byte(target.Fragment))
	var count int
	if err = store.DB.QueryRow(ctx, `SELECT count(*) FROM admin_setup_grants g JOIN users u ON u.id=g.user_id WHERE g.token_hash=$1 AND g.expires_at>now() AND u.is_admin AND u.password_hash=''`, hex.EncodeToString(digest[:])).Scan(&count); err != nil || count != 1 {
		t.Fatal("setup grant missing or stored incorrectly")
	}
	if err = run(); err == nil {
		t.Fatal("existing file overwritten")
	}
	again, _ := os.ReadFile(file)
	if string(raw) != string(again) {
		t.Fatal("existing setup file changed")
	}
	t.Setenv("ADMIN_SETUP_FILE", filepath.Join(t.TempDir(), "new.url"))
	if err = run(); err != nil {
		t.Fatal(err)
	}
	store.DB.QueryRow(ctx, `SELECT count(*) FROM admin_setup_grants WHERE token_hash=$1`, hex.EncodeToString(digest[:])).Scan(&count)
	if count != 0 {
		t.Fatal("old setup link survives new setup")
	}
	t.Setenv("BASE_URL", "http://production.example")
	if err = run(); err == nil {
		t.Fatal("insecure production origin accepted")
	}
	os.Args = []string{"blog", "create-owner"}
	if err = run(); err == nil {
		t.Fatal("old password command still active")
	}
}
