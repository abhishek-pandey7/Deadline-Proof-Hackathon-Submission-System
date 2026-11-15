// Package db opens the Postgres pool and applies schema migrations.
package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationLockKey is a fixed advisory-lock key so that only one server
// instance migrates at a time.
const migrationLockKey int64 = 0x48344850_53320001 // "H4HP" "S2" 0001

// Open connects to Postgres and checks the connection.
func Open(ctx context.Context, url string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("db: parse url: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	cfg.MaxConnIdleTime = 5 * time.Minute
	if server := os.Getenv("DB_DNS_SERVER"); server != "" {
		// For laptop tools on networks whose DNS refuses the database host
		// (some ISP resolvers refuse Neon's): resolve it through server instead.
		r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "udp", server)
		}}
		cfg.ConnConfig.LookupFunc = r.LookupHost
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: open pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}

// Migrate applies the app's SQL migrations in file-name order, then River's
// own migrations. It holds an advisory lock for the whole run, so concurrent
// server starts wait for each other instead of racing.
func Migrate(ctx context.Context, pool *pgxpool.Pool) (applied []string, err error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("db: acquire for migrate: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "select pg_advisory_lock($1)", migrationLockKey); err != nil {
		return nil, fmt.Errorf("db: migration lock: %w", err)
	}
	defer func() {
		// Use a fresh context: the caller's may already be cancelled.
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, uerr := conn.Exec(unlockCtx, "select pg_advisory_unlock($1)", migrationLockKey); uerr != nil && err == nil {
			err = fmt.Errorf("db: migration unlock: %w", uerr)
		}
	}()

	if _, err := conn.Exec(ctx, `create table if not exists schema_migrations (
		version    text primary key,
		applied_at timestamptz not null default now()
	)`); err != nil {
		return nil, fmt.Errorf("db: create schema_migrations: %w", err)
	}

	names, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return nil, fmt.Errorf("db: list migrations: %w", err)
	}
	sort.Strings(names)

	for _, name := range names {
		version := strings.TrimSuffix(strings.TrimPrefix(name, "migrations/"), ".sql")
		did, err := applyOne(ctx, conn.Conn(), name, version)
		if err != nil {
			return applied, err
		}
		if did {
			applied = append(applied, version)
		}
	}

	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return applied, fmt.Errorf("db: river migrator: %w", err)
	}
	res, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil)
	if err != nil {
		return applied, fmt.Errorf("db: river migrate: %w", err)
	}
	for _, v := range res.Versions {
		applied = append(applied, fmt.Sprintf("river_%03d", v.Version))
	}
	return applied, nil
}

func applyOne(ctx context.Context, conn *pgx.Conn, file, version string) (bool, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("db: begin %s: %w", version, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var exists bool
	if err := tx.QueryRow(ctx, "select exists (select 1 from schema_migrations where version = $1)", version).Scan(&exists); err != nil {
		return false, fmt.Errorf("db: check %s: %w", version, err)
	}
	if exists {
		return false, nil
	}

	sql, err := migrationFiles.ReadFile(file)
	if err != nil {
		return false, fmt.Errorf("db: read %s: %w", file, err)
	}
	if _, err := tx.Exec(ctx, string(sql)); err != nil {
		return false, fmt.Errorf("db: apply %s: %w", version, err)
	}
	if _, err := tx.Exec(ctx, "insert into schema_migrations (version) values ($1)", version); err != nil {
		return false, fmt.Errorf("db: record %s: %w", version, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("db: commit %s: %w", version, err)
	}
	return true, nil
}

// ErrNoRows is re-exported so callers need not import pgx for the common case.
var ErrNoRows = pgx.ErrNoRows

// IsNoRows reports whether err means a query matched nothing.
func IsNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
