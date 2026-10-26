//go:build integration

package backup

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/testpg"
)

// freshDB recreates the test database at migration version v (0 = latest).
func freshDB(t *testing.T, v int64) (*pgxpool.Pool, *db.Queries, string) {
	t.Helper()
	ctx := context.Background()
	dsn := testpg.DSN("mmp_backup_test")
	cfg, _ := pgx.ParseConfig(dsn)
	target := cfg.Database
	cfg.Database = "postgres"
	mconn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		testpg.Unreachable(t, err)
	}
	for _, q := range []string{`DROP DATABASE IF EXISTS ` + pgx.Identifier{target}.Sanitize() + ` WITH (FORCE)`,
		`CREATE DATABASE ` + pgx.Identifier{target}.Sanitize()} {
		if _, err := mconn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	mconn.Close(ctx)
	if err := db.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	if v > 0 {
		if err := db.MigrateDownTo(ctx, dsn, v); err != nil {
			t.Fatal(err)
		}
	}
	pool, err := db.Open(ctx, db.PoolConfig{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, db.New(pool), dsn
}

func count(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func seed(t *testing.T, q *db.Queries) {
	t.Helper()
	ctx := context.Background()
	p, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: "Panel", Type: "xtream", Url: "http://panel/", Username: "u",
		Password: "p@ss,\"quoted\"\nnewline", MaxConnections: 2, RefreshIntervalSecs: 3600, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"ESPN", "CNN", "Ünïcødé ✓"} {
		sid, err := q.UpsertProviderStream(ctx, db.UpsertProviderStreamParams{ProviderID: p.ID, ExternalID: n, Name: n, StreamUrl: "http://panel/" + n})
		if err != nil {
			t.Fatal(err)
		}
		c, err := q.CreateChannel(ctx, db.CreateChannelParams{Name: n})
		if err != nil {
			t.Fatal(err)
		}
		if err := q.AddStreamToChannel(ctx, db.AddStreamToChannelParams{ChannelID: c.ID, ProviderStreamID: sid, Priority: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.CreateBouquet(ctx, db.CreateBouquetParams{Name: "Basic"}); err != nil {
		t.Fatal(err)
	}
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	pool, q, dsn := freshDB(t, 0)
	ctx := context.Background()
	seed(t, q)
	// Guide data and sessions are left out of backups.
	var srcID int64
	if err := pool.QueryRow(ctx, "INSERT INTO epg_sources (name, url, last_refreshed_at) VALUES ('g', 'http://g/', now()) RETURNING id").Scan(&srcID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertEpgPrograms(ctx, []db.InsertEpgProgramsParams{{EpgSourceID: srcID, ChannelTvgID: "x", Title: "Show",
		StartTime: time.Now(), StopTime: time.Now().Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	store := &Store{Dir: dir, Pool: pool, Version: "v-test"}
	info, err := store.Create(ctx, KindManual)
	if err != nil {
		t.Fatal(err)
	}
	if info.Manifest.Schema == 0 || info.Manifest.Version != "v-test" || !strings.HasPrefix(info.Name, "mmp-manual-") {
		t.Fatalf("manifest = %+v", info)
	}
	for _, tb := range info.Manifest.Tables {
		if tb.Name == "epg_programs" || tb.Name == "epg_channels" || tb.Name == "admin_sessions" || tb.Name == "goose_db_version" {
			t.Errorf("excluded table %s in backup", tb.Name)
		}
	}
	if st, _ := os.Stat(filepath.Join(dir, info.Name)); st.Mode().Perm() != 0o600 {
		t.Errorf("backup mode = %v, want 0600", st.Mode().Perm())
	}

	// Change everything, then restore.
	if _, err := pool.Exec(ctx, "DELETE FROM channels; DELETE FROM bouquets; UPDATE providers SET name = 'changed'"); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateChannel(ctx, db.CreateChannelParams{Name: "Extra 1"}); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(filepath.Join(dir, info.Name))
	defer f.Close()
	latest, _ := db.LatestMigration()
	if _, err := Restore(ctx, pool, f, db.Migrator{DSN: dsn}, latest, dir); err != nil {
		t.Fatal(err)
	}
	if count(t, pool, "channels") != 3 || count(t, pool, "channel_streams") != 3 || count(t, pool, "bouquets") != 1 {
		t.Errorf("after restore: channels %d, channel_streams %d, bouquets %d",
			count(t, pool, "channels"), count(t, pool, "channel_streams"), count(t, pool, "bouquets"))
	}
	provs, _ := q.ListProviders(ctx)
	if len(provs) != 1 || provs[0].Name != "Panel" || provs[0].Password != "p@ss,\"quoted\"\nnewline" {
		t.Errorf("provider = %+v", provs)
	}
	chans, _ := q.ListChannels(ctx, db.ListChannelsParams{Limit: 10})
	names := map[string]bool{}
	for _, c := range chans {
		names[c.Name] = true
	}
	if !names["Ünïcødé ✓"] || names["Extra 1"] {
		t.Errorf("channels = %v", names)
	}
	// Ids continue after the restored ones; guides re-download.
	if _, err := q.CreateChannel(ctx, db.CreateChannelParams{Name: "After restore"}); err != nil {
		t.Errorf("insert after restore: %v", err)
	}
	if count(t, pool, "epg_programs") != 0 {
		t.Error("guide programs survived the restore")
	}
	var refreshed *time.Time
	pool.QueryRow(ctx, "SELECT last_refreshed_at FROM epg_sources").Scan(&refreshed)
	if refreshed != nil {
		t.Error("epg source not reset for re-download")
	}

	// Prune keeps the newest automatic backups only.
	for range 3 {
		if _, err := store.Create(ctx, KindAuto); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := store.Prune(2); n != 1 {
		t.Errorf("pruned %d, want 1", n)
	}
	all, _ := store.List()
	kinds := map[string]int{}
	for _, b := range all {
		kinds[b.Kind]++
	}
	if kinds[KindAuto] != 2 || kinds[KindManual] != 1 {
		t.Errorf("after prune: %v", kinds)
	}
}

// TestRestoreOlderBackup: a backup from an older schema is restored into
// that schema and migrated up; a newer one is refused.
func TestRestoreOlderBackup(t *testing.T) {
	ctx := context.Background()
	latest, _ := db.LatestMigration()
	old := latest - 2
	pool, q, dsn := freshDB(t, old)
	if _, err := q.CreateBouquet(ctx, db.CreateBouquetParams{Name: "From the old version"}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	m, err := Create(ctx, pool, &buf, "v-old", KindManual, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if m.Schema != old {
		t.Fatalf("schema = %d, want %d", m.Schema, old)
	}
	pool.Close()

	pool3, _, _ := freshDB(t, 0) // a newer MMP, empty
	if _, err := Restore(ctx, pool3, bytes.NewReader(buf.Bytes()), db.Migrator{DSN: dsn}, latest, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if count(t, pool3, "bouquets") != 1 {
		t.Error("old data not restored")
	}
	var v int64
	pool3.QueryRow(ctx, "SELECT max(version_id) FROM goose_db_version WHERE is_applied").Scan(&v)
	if v != latest {
		t.Errorf("schema after restore = %d, want %d", v, latest)
	}
	// Newer than this MMP: refused, nothing changed.
	if _, err := Restore(ctx, pool3, bytes.NewReader(buf.Bytes()), db.Migrator{DSN: dsn}, old-1, t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "newer MMP") {
		t.Errorf("newer backup: %v", err)
	}
	// Not a backup at all.
	if _, err := Restore(ctx, pool3, strings.NewReader("hello"), db.Migrator{DSN: dsn}, latest, t.TempDir()); err == nil {
		t.Error("garbage accepted")
	}
	if _, err := ReadManifest(io.LimitReader(bytes.NewReader(buf.Bytes()), 10)); err == nil {
		t.Error("truncated manifest accepted")
	}
}
