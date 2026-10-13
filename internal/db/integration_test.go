//go:build integration

package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// testDSN returns the DSN under test. The round-trip test is destructive, so
// a dedicated database (mmp_test) is recreated from scratch each run.
func testDSN(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("MMP_TEST_DSN"); v != "" {
		return v
	}
	return "postgres://mmp:mmp@localhost:5432/mmp_test?sslmode=disable"
}

// recreate drops and re-creates the target database using the maintenance
// connection on the same server.
func recreate(t *testing.T, dsn string) {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	target := cfg.Database
	cfg.Database = "postgres"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Skipf("postgres not reachable (%v); start it with `make up`", err)
	}
	defer conn.Close(context.Background())
	for _, q := range []string{
		`DROP DATABASE IF EXISTS ` + pgx.Identifier{target}.Sanitize() + ` WITH (FORCE)`,
		`CREATE DATABASE ` + pgx.Identifier{target}.Sanitize(),
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("setup %q: %v", q, err)
		}
	}
}

func TestMigrateRoundTrip(t *testing.T) {
	dsn := testDSN(t)
	recreate(t, dsn)
	ctx := context.Background()

	up := func() {
		if err := Migrate(ctx, dsn); err != nil {
			t.Fatalf("migrate up: %v", err)
		}
	}
	down := func() {
		if err := MigrateDownTo(ctx, dsn, 0); err != nil {
			t.Fatalf("migrate down: %v", err)
		}
	}

	up()
	tables := []string{
		"providers", "provider_streams", "channels", "channel_streams",
		"channel_groups", "bouquets", "bouquet_channels", "stream_profiles",
		"epg_sources", "epg_programmes", "users", "user_bouquets", "events",
	}
	pool, err := Open(ctx, PoolConfig{DSN: dsn})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	for _, tbl := range tables {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name=$1`,
			tbl).Scan(&n); err != nil || n != 1 {
			t.Errorf("table %s missing after up (n=%d err=%v)", tbl, n, err)
		}
	}

	down()
	var total int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema='public'`).Scan(&total); err != nil {
		t.Fatalf("count after down: %v", err)
	}
	// goose_db_version remains after a full down.
	if total > 1 {
		t.Errorf("expected only goose_db_version after down, found %d tables", total)
	}

	// And up again from empty.
	up()
}

func TestProviderQueriesSmoke(t *testing.T) {
	dsn := testDSN(t)
	recreate(t, dsn)
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := Open(ctx, PoolConfig{DSN: dsn})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	q := New(pool)

	p, err := q.CreateProvider(ctx, CreateProviderParams{
		Name: "Fake Panel", Type: "xtream",
		Url: "http://panel.example.tv:8080", Username: "u", Password: "p",
		MaxConnections: 2, RefreshIntervalSecs: 3600, Priority: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if p.ID == 0 || p.Type != "xtream" {
		t.Errorf("unexpected provider %+v", p)
	}
	got, err := q.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if got.Name != "Fake Panel" {
		t.Errorf("name = %q", got.Name)
	}
	list, err := q.ListProviders(ctx)
	if err != nil || len(list) != 1 {
		t.Errorf("ListProviders = %v, %v", list, err)
	}
	if err := q.DeleteProvider(ctx, p.ID); err != nil {
		t.Fatalf("DeleteProvider: %v", err)
	}
	if list, err = q.ListProviders(ctx); err != nil || len(list) != 0 {
		t.Errorf("after delete ListProviders = %v, %v", list, err)
	}
}
