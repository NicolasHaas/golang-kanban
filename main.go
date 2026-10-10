package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	_ "github.com/ncruces/go-sqlite3/driver"
)

const usage = `usage:
  kanban [serve]                          run the web server (the default)
  kanban export [-o file] [-board slug]   write boards as JSON, to stdout unless -o is given
  kanban import [-replace] [-dry-run] file|-
                                          read boards from an export; - reads stdin
`

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd, args := "serve", os.Args[1:]
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(ctx)
	case "export":
		err = exportCmd(ctx, args)
	case "import":
		err = importCmd(ctx, args)
	case "help", "-h", "-help", "--help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		slog.Error(cmd+" failed", "err", err)
		os.Exit(1)
	}
}

func getEnv(key, def string) string {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v
}

// dsnFromEnv builds the connection URL with net/url, so a password containing
// '@', '/' or '?' is escaped instead of silently producing a different URL.
func dsnFromEnv() string {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(getEnv("DB_USER", "user"), getEnv("DB_PASS", "password")),
		Host:     net.JoinHostPort(getEnv("DB_HOST", "postgres"), getEnv("DB_PORT", "5432")),
		Path:     "/" + getEnv("DB_NAME", "kanban"),
		RawQuery: url.Values{"sslmode": {getEnv("DB_SSLMODE", "disable")}}.Encode(),
	}
	return u.String()
}

// sqliteDSN switches on foreign keys and WAL, makes a writer wait for a lock
// instead of failing at once, and takes the write lock when a transaction
// begins, so a `kanban import` next to the running server queues rather than
// failing halfway.
//
// The path goes in as a URL path, so a '#', '?' or '%' in it is escaped
// rather than ending the file name early and opening a different file.
func sqliteDSN(path string) string {
	u := url.URL{Scheme: "file", OmitHost: true, Path: path, RawQuery: "_txlock=immediate" +
		"&_pragma=foreign_keys(1)&_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)"}
	return u.String()
}

// openStore connects to the database STORAGE names, waits for it if it is
// still starting, and brings the schema up to date.
func openStore(ctx context.Context) (*store, error) {
	s := &store{dialect: getEnv("STORAGE", "postgres")}
	var err error
	switch s.dialect {
	case "postgres":
		s.db, err = sql.Open("postgres", dsnFromEnv())
		if err != nil {
			return nil, err
		}
		s.db.SetMaxOpenConns(5)
		s.db.SetMaxIdleConns(2)
		s.db.SetConnMaxIdleTime(5 * time.Minute)
	case "sqlite":
		s.db, err = sql.Open("sqlite3", sqliteDSN(getEnv("SQLITE_PATH", "kanban.db")))
		if err != nil {
			return nil, err
		}
		// One connection: SQLite takes one writer at a time anyway, and this
		// way writes queue in Go instead of failing with "database is locked".
		s.db.SetMaxOpenConns(1)
	default:
		return nil, fmt.Errorf("STORAGE=%q: use postgres or sqlite", s.dialect)
	}

	// With docker compose the database is often still starting when we are.
	if err := waitForDB(ctx, s.db, 30*time.Second); err != nil {
		_ = s.db.Close()
		return nil, err
	}
	if err := s.migrate(ctx); err != nil {
		_ = s.db.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	return s, nil
}

func waitForDB(ctx context.Context, db *sql.DB, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		err := db.PingContext(ctx)
		if err == nil {
			return nil
		}
		slog.Warn("waiting for database", "err", err)
		select {
		case <-ctx.Done():
			return fmt.Errorf("database not reachable: %w", err)
		case <-time.After(time.Second):
		}
	}
}

func serve(ctx context.Context) error {
	s, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = s.db.Close() }()

	// AUTH_HEADER names the header a sign-in proxy such as oauth2-proxy puts
	// the user's email in. Setting it turns on assignees and refuses every
	// request that did not come through the proxy.
	authHeader := os.Getenv("AUTH_HEADER")
	a, err := newApp(s, authHeader)
	if err != nil {
		return err
	}
	if authHeader != "" {
		slog.Info("sign-in required", "header", authHeader)
	}
	srv := &http.Server{
		Addr:              ":" + getEnv("SERVER_PORT", "17808"),
		Handler:           a.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	errc := make(chan error, 1)
	go func() {
		slog.Info("server started", "addr", srv.Addr, "storage", s.dialect)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func exportCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	out := fs.String("o", "", "write to this file instead of stdout")
	board := fs.String("board", "", "export only the board with this slug")
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	s, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = s.db.Close() }()

	if *out == "" {
		return s.export(ctx, os.Stdout, *board)
	}
	// Written next to the target and renamed, so a failed export never
	// leaves a truncated file where the last good one was. CreateTemp makes
	// it 0600, which is right for a file that holds the whole board.
	f, err := os.CreateTemp(filepath.Dir(*out), ".kanban-export-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if err := s.export(ctx, f, *board); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), *out)
}

func importCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	replace := fs.Bool("replace", false, "delete boards that are in the file and already exist, then import them")
	dryRun := fs.Bool("dry-run", false, "check the file and say what it holds, write nothing")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("import needs exactly one file, or - for stdin")
	}

	var r io.Reader = os.Stdin
	if name := fs.Arg(0); name != "-" {
		f, err := os.Open(name)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	snap, err := readSnapshot(r)
	if err != nil {
		return err
	}
	for _, b := range snap.Boards {
		cards := 0
		for _, l := range b.Columns {
			cards += len(l.Cards)
		}
		fmt.Fprintf(os.Stderr, "board %-20s %2d columns %4d cards  %s\n", b.Slug, len(b.Columns), cards, b.Name)
	}
	if *dryRun {
		fmt.Fprintln(os.Stderr, "dry run: nothing written")
		return nil
	}

	s, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = s.db.Close() }()
	if err := s.importSnapshot(ctx, snap, *replace); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "imported %d boards\n", len(snap.Boards))
	return nil
}
