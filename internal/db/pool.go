// Package db owns the Postgres connection pool and embedded goose migrations,
// plus sqlc-generated queries for the whole schema.
package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"

	"github.com/JustDr00py/mmp/internal/redact"

	// Register the pgx stdlib driver so goose can drive migrations through
	// database/sql.
	_ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationsSub is the migration directory re-rooted at the FS top level,
// which is where goose expects to find versioned .sql files.
var migrationsSub fs.FS

func init() {
	var err error
	migrationsSub, err = fs.Sub(migrationsFS, "migrations")
	if err != nil {
		panic(fmt.Sprintf("db: embed migrations: %v", err))
	}
}

// PoolConfig is the subset of configuration the DB layer needs.
type PoolConfig struct {
	DSN      string
	MaxConns int32
}

// Open creates and pings a pgx connection pool.
func Open(ctx context.Context, cfg PoolConfig) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		// The parse error quotes the DSN, password included.
		return nil, errors.New("db: parse DSN: " + redact.Text(err.Error()))
	}
	if cfg.MaxConns > 0 {
		pc.MaxConns = cfg.MaxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("db: connect: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}

// Migrate applies all embedded goose migrations to the database at dsn.
// It uses a dedicated database/sql connection because goose drives DDL
// through the stdlib interface.
func Migrate(ctx context.Context, dsn string) error {
	goose.SetLogger(goose.NopLogger())
	sdb, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("db: open for migration: %w", err)
	}
	defer sdb.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, sdb, migrationsSub)
	if err != nil {
		return fmt.Errorf("db: goose provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		// The DSN is parsed lazily, so a malformed one surfaces here quoted.
		return errors.New("db: migrate up: " + redact.Text(err.Error()))
	}
	return nil
}

// MigrateDownTo rolls the schema back to version v (0 = all the way down).
// Used by tests and development only.
func MigrateDownTo(ctx context.Context, dsn string, v int64) error {
	goose.SetLogger(goose.NopLogger())
	sdb, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("db: open for migration: %w", err)
	}
	defer sdb.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, sdb, migrationsSub)
	if err != nil {
		return fmt.Errorf("db: goose provider: %w", err)
	}
	if _, err := provider.DownTo(ctx, v); err != nil {
		return fmt.Errorf("db: migrate down to %d: %s", v, redact.Text(err.Error()))
	}
	return nil
}
