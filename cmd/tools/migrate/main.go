package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const createTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
  version    TEXT PRIMARY KEY,
  applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
`

var fileNamePattern = regexp.MustCompile(`^(\d+)_[a-zA-Z0-9_\-]+\.sql$`)

type migration struct {
	version  string
	name     string
	path     string
	contents string
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	dir := flag.String("dir", "migrations", "directory containing .sql migration files")
	flag.Parse()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		slog.Error("DATABASE_URL is not set")
		os.Exit(1)
	}

	if err := run(*dir, dsn); err != nil {
		slog.Error("migrate failed", "err", err)
		os.Exit(1)
	}
}

func run(dir, dsn string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	migrations, err := loadMigrations(dir)
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	if len(migrations) == 0 {
		slog.Warn("no migrations found", "dir", dir)
		return nil
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, createTable); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := loadApplied(ctx, conn)
	if err != nil {
		return fmt.Errorf("load applied: %w", err)
	}

	for _, m := range migrations {
		if _, ok := applied[m.version]; ok {
			slog.Info("skip applied", "version", m.version, "name", m.name)
			continue
		}
		slog.Info("applying", "version", m.version, "name", m.name)
		if err := apply(ctx, conn, m); err != nil {
			return fmt.Errorf("apply %s: %w", m.version, err)
		}
	}
	slog.Info("migrate complete", "count", len(migrations))
	return nil
}

func loadMigrations(dir string) ([]migration, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		match := fileNamePattern.FindStringSubmatch(e.Name())
		if match == nil {
			continue
		}
		path := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		out = append(out, migration{
			version:  match[1],
			name:     strings.TrimSuffix(e.Name(), ".sql"),
			path:     path,
			contents: string(b),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func loadApplied(ctx context.Context, conn *pgx.Conn) (map[string]struct{}, error) {
	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = struct{}{}
	}
	return out, rows.Err()
}

func apply(ctx context.Context, conn *pgx.Conn, m migration) error {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, m.contents); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, m.version); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return err
	}
	return nil
}
