//go:build integration

package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/JustDr00py/mmp/internal/testpg"
)

// testDSN returns the DSN under test. The round-trip test is destructive, so
// a dedicated database (mmp_test) is recreated from scratch each run.
func testDSN(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("MMP_TEST_DSN"); v != "" {
		return v
	}
	return testpg.DSN("mmp_test")
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
		testpg.Unreachable(t, err)
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
		"epg_sources", "epg_programs", "users", "user_bouquets", "events",
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

// TestListGuidePrograms covers the guide query: case-insensitive tvg-id
// match, one source per channel (best provider priority with data in the
// window, provider sources ahead of manual ones), the time window, and
// disabled sources.
func TestListGuidePrograms(t *testing.T) {
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

	source := func(name string, prio int32, withProvider bool) int64 {
		t.Helper()
		var pid *int64
		if withProvider {
			p, err := q.CreateProvider(ctx, CreateProviderParams{Name: name, Type: "m3u", Url: "http://" + name + "/",
				MaxConnections: 1, RefreshIntervalSecs: 3600, Priority: prio, Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			pid = &p.ID
		}
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO epg_sources (name, url, provider_id) VALUES ($1, $2, $3) RETURNING id`,
			name, "http://"+name+"/xmltv", pid).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	sBest, sWorse, sManual, sOff := source("best", 0, true), source("worse", 5, true), source("manual", 0, false), source("off", 0, true)
	if _, err := pool.Exec(ctx, `UPDATE epg_sources SET enabled = false WHERE id = $1`, sOff); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	h := func(n int) time.Time { return now.Add(time.Duration(n) * time.Hour) }
	var rows []InsertEpgProgramsParams
	add := func(src int64, tvg, title string, start, stop time.Time) {
		rows = append(rows, InsertEpgProgramsParams{EpgSourceID: src, ChannelTvgID: tvg, Title: title, StartTime: start, StopTime: stop})
	}
	add(sBest, "News.US", "best-news-2", h(1), h(2))
	add(sBest, "News.US", "best-news-1", h(0), h(1))
	add(sBest, "News.US", "best-news-old", h(-30), h(-29))     // before the window
	add(sBest, "News.US", "best-news-running", h(-25), h(-23)) // overlaps the window start
	add(sWorse, "news.us", "worse-news", h(0), h(3))
	add(sWorse, "sports", "worse-sports", h(0), h(1))
	add(sManual, "sports", "manual-sports", h(0), h(1))
	add(sBest, "movies", "best-movies-far", h(200), h(201)) // after the window
	add(sWorse, "movies", "worse-movies", h(0), h(2))
	add(sOff, "kids", "off-kids", h(0), h(1))
	if _, err := q.InsertEpgPrograms(ctx, rows); err != nil {
		t.Fatal(err)
	}

	got, err := q.ListGuidePrograms(ctx, ListGuideProgramsParams{
		TvgKeys: []string{"news.us", "sports", "movies", "kids", "missing"},
		Since:   h(-24), Until: h(7 * 24),
	})
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, r := range got {
		titles = append(titles, r.TvgKey+":"+r.Title)
	}
	want := []string{
		"movies:worse-movies",
		"news.us:best-news-running", "news.us:best-news-1", "news.us:best-news-2",
		"sports:worse-sports",
	}
	if len(titles) != len(want) {
		t.Fatalf("guide = %v, want %v", titles, want)
	}
	for i := range want {
		if titles[i] != want[i] {
			t.Errorf("guide[%d] = %s, want %s (all: %v)", i, titles[i], want[i], titles)
		}
	}
}

func TestProbeQueries(t *testing.T) {
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

	on, _ := q.CreateProvider(ctx, CreateProviderParams{Name: "On", Type: "m3u", Url: "http://a/", MaxConnections: 2, RefreshIntervalSecs: 3600, Enabled: true, UserAgent: "ProvUA"})
	off, _ := q.CreateProvider(ctx, CreateProviderParams{Name: "Off", Type: "m3u", Url: "http://b/", RefreshIntervalSecs: 3600, Enabled: false})
	stream := func(p int64, ext string) int64 {
		id, err := q.UpsertProviderStream(ctx, UpsertProviderStreamParams{ProviderID: p, ExternalID: ext, Name: ext, StreamUrl: "http://x/" + ext})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	sA, sB, sC, sD := stream(on.ID, "a"), stream(on.ID, "b"), stream(off.ID, "c"), stream(on.ID, "d")
	stream(on.ID, "unused") // on no channel
	prof, _ := q.CreateStreamProfile(ctx, CreateStreamProfileParams{Name: "ua", Mode: "passthrough", UserAgent: "ProfUA"})
	chOn, _ := q.CreateChannel(ctx, CreateChannelParams{Name: "On"})
	chProf, _ := q.CreateChannel(ctx, CreateChannelParams{Name: "Prof", StreamProfileID: &prof.ID})
	chOff, _ := q.CreateChannel(ctx, CreateChannelParams{Name: "Off"})
	if _, err := pool.Exec(ctx, `UPDATE channels SET enabled = false WHERE id = $1`, chOff.ID); err != nil {
		t.Fatal(err)
	}
	for _, l := range []struct{ ch, s int64 }{{chOn.ID, sA}, {chProf.ID, sA}, {chOn.ID, sB}, {chOn.ID, sC}, {chOff.ID, sD}} {
		if err := q.AddStreamToChannel(ctx, AddStreamToChannelParams{ChannelID: l.ch, ProviderStreamID: l.s, Priority: 1}); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := q.ListProbeTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// a (twice linked, listed once, profile UA), b; not c (provider off),
	// d (channel off) or unused (no channel).
	if len(rows) != 2 || rows[0].StreamID != sA || rows[1].StreamID != sB {
		t.Fatalf("targets = %+v", rows)
	}
	if rows[0].UserAgent != "ProfUA" || rows[1].UserAgent != "ProvUA" || rows[0].MaxConnections != 2 || rows[0].LastProbedAt != nil {
		t.Errorf("targets = %+v", rows)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	until := now.Add(time.Minute)
	if err := q.RecordProbe(ctx, RecordProbeParams{ID: sA, HealthScore: 0.5, LastError: "upstream status 503", CooldownUntil: &until, LastProbedAt: &now}); err != nil {
		t.Fatal(err)
	}
	rows, _ = q.ListProbeTargets(ctx)
	if r := rows[0]; r.HealthScore != 0.5 || r.CooldownUntil == nil || !r.CooldownUntil.Equal(until) || r.LastProbedAt == nil || !r.LastProbedAt.Equal(now) {
		t.Errorf("after record: %+v", r)
	}
}
