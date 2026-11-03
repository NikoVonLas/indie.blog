package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/NikoVonLas/indie.blog/go/internal/blog"
)

func main() {
	if e := run(); e != nil {
		slog.Error("blog stopped", "error", e)
		os.Exit(1)
	}
}
func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	store, e := blog.Open(ctx, dsn)
	if e != nil {
		return e
	}
	defer store.DB.Close()
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	switch command {
	case "migrate":
		return store.Migrate(ctx)
	case "admin-setup":
		email := strings.ToLower(strings.TrimSpace(os.Getenv("OWNER_EMAIL")))
		addr, e := mail.ParseAddress(email)
		if e != nil || addr.Address != email {
			return errors.New("valid OWNER_EMAIL is required")
		}
		base, e := url.Parse(env("BASE_URL", "http://localhost:8080"))
		if e != nil || base.User != nil || base.Host == "" || base.RawQuery != "" || base.Fragment != "" || (base.Path != "" && base.Path != "/") || (base.Scheme != "https" && (base.Scheme != "http" || base.Hostname() != "localhost")) {
			return errors.New("BASE_URL must be HTTPS, or HTTP localhost for development")
		}
		filename := os.Getenv("ADMIN_SETUP_FILE")
		if filename == "" {
			return errors.New("ADMIN_SETUP_FILE is required; use a protected file outside Git")
		}
		file, e := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return errors.New("cannot create setup file (must not exist)")
		}
		complete := false
		defer func() {
			file.Close()
			if !complete {
				os.Remove(filename)
			}
		}()
		token, e := store.PrepareAdminPasskey(ctx, email, env("OWNER_NAME", "Автор"))
		if e != nil {
			return errors.New("cannot prepare administrator; check account role and database")
		}
		base.Path = "/admin/setup"
		base.Fragment = token
		if _, e = file.WriteString(base.String() + "\n"); e != nil {
			return errors.New("cannot write setup file")
		}
		if e = file.Close(); e != nil {
			return errors.New("cannot close setup file")
		}
		complete = true
		fmt.Println("Одноразовая ссылка записана в ADMIN_SETUP_FILE (0600), действует 15 минут.")
		return nil
	case "import":
		var posts []blog.Post
		dec := json.NewDecoder(os.Stdin)
		if e = dec.Decode(&posts); e != nil {
			return e
		}
		for _, p := range posts {
			if !p.Valid() {
				return fmt.Errorf("invalid post %q", p.Slug)
			}
		}
		if e = store.ImportPosts(ctx, posts); e != nil {
			return errors.New("import failed and rolled back; check duplicate addresses and field values")
		}
		fmt.Printf("Imported %d posts\n", len(posts))
		return nil
	case "serve":
	default:
		return errors.New("usage: blog [serve|migrate|admin-setup|import]")
	}
	cfg := blog.Config{TelegramChannel: env("TELEGRAM_CHANNEL", "@m1ndstr3am"), TelegramBotToken: os.Getenv("TELEGRAM_BOT_TOKEN"), OwnerEmail: os.Getenv("OWNER_EMAIL"), BaseURL: env("BASE_URL", "http://localhost:8080"), Listen: env("LISTEN_ADDR", "127.0.0.1:8080"), Title: env("BLOG_TITLE", "Garbage M1ndstr3am"), Author: env("BLOG_AUTHOR", "Автор"), Language: env("BLOG_LANGUAGE", "ru"), SMTPAddr: os.Getenv("SMTP_ADDR"), SMTPUser: os.Getenv("SMTP_USER"), SMTPPassword: os.Getenv("SMTP_PASSWORD"), MailFrom: os.Getenv("MAIL_FROM")}
	app, e := blog.NewServer(ctx, store, cfg)
	if e != nil {
		return e
	}
	server := &http.Server{Addr: cfg.Listen, Handler: app, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 16}
	var workers sync.WaitGroup
	for _, work := range []func(context.Context){app.RunCacheWorker, app.RunMailWorker, app.RunWebmentionWorker, app.RunPOSSEWorker} {
		workers.Add(1)
		go func() { defer workers.Done(); work(ctx) }()
	}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(stop)
	}()
	slog.Info("blog listening", "address", cfg.Listen)
	e = server.ListenAndServe()
	cancel()
	<-shutdownDone
	workers.Wait()
	flush, done := context.WithTimeout(context.Background(), 5*time.Second)
	flushErr := app.FlushViews(flush)
	done()
	if flushErr != nil {
		return errors.New("could not persist final page-view batch")
	}
	if errors.Is(e, http.ErrServerClosed) {
		return nil
	}
	return e
}
