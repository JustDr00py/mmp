//go:build integration

package source

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sysadmin/mmp/internal/db"
)

type pgxpoolWrapper struct{ pool *pgxpool.Pool }

func newTestDB(t *testing.T) (*pgxpoolWrapper, *db.Queries) {
	t.Helper()
	// Dedicated database per package: concurrent package tests must not
	// recreate each other's database.
	dsn := "postgres://mmp:mmp@localhost:5432/mmp_source_test?sslmode=disable"
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	target := cfg.Database
	cfg.Database = "postgres"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	mconn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Skipf("postgres not reachable (%v); start it with `make up`", err)
	}
	for _, q := range []string{
		`DROP DATABASE IF EXISTS ` + pgx.Identifier{target}.Sanitize() + ` WITH (FORCE)`,
		`CREATE DATABASE ` + pgx.Identifier{target}.Sanitize(),
	} {
		if _, err := mconn.Exec(ctx, q); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	mconn.Close(context.Background())

	if err := db.Migrate(context.Background(), dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := db.Open(context.Background(), db.PoolConfig{DSN: dsn})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pool.Close)
	return &pgxpoolWrapper{pool}, db.New(pool)
}

// newM3UServer serves a playlist that tests can swap between refreshes.
func newM3UServer(t *testing.T) (*httptest.Server, *atomic.Value) {
	t.Helper()
	body := &atomic.Value{}
	body.Store("#EXTM3U\n" +
		"#EXTINF:-1 tvg-id=\"us.abc\" tvg-logo=\"http://l/abc.png\" group-title=\"US\",ABC HD\n" +
		"http://fake.tv/abc.ts\n" +
		"#EXTINF:-1 tvg-id=\"us.nbc\" group-title=\"US\",NBC\n" +
		"http://fake.tv/nbc.ts\n" +
		"#EXTINF:-1 group-title=\"News\",News 24\n" +
		"http://fake.tv/news.ts\n")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "MMP-Test/1.0" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = io.WriteString(w, body.Load().(string))
	}))
	t.Cleanup(ts.Close)
	return ts, body
}

func TestRefreshProviderM3UTwice(t *testing.T) {
	w, q := newTestDB(t)
	ts, body := newM3UServer(t)
	ctx := context.Background()

	p, err := q.CreateProvider(ctx, db.CreateProviderParams{
		Name: "Fake M3U", Type: "m3u", Url: ts.URL, UserAgent: "MMP-Test/1.0",
		MaxConnections: 2, RefreshIntervalSecs: 3600, Priority: 1, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ref := NewRefresher(slog.New(slog.NewTextHandler(io.Discard, nil)), w.pool)

	// First refresh: three streams appear.
	res1, err := ref.RefreshProvider(ctx, p)
	if err != nil {
		t.Fatalf("refresh 1: %v", err)
	}
	if res1.Seen != 3 || res1.Upserted != 3 || res1.Disabled != 0 {
		t.Fatalf("res1 = %+v", res1)
	}
	streams, err := q.ListProviderStreams(ctx, db.ListProviderStreamsParams{ProviderID: p.ID, Limit: 100, Offset: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(streams) != 3 {
		t.Fatalf("streams after refresh 1 = %d, want 3", len(streams))
	}
	byURL := map[string]db.ProviderStream{}
	for _, s := range streams {
		byURL[s.StreamUrl] = s
	}
	if s := byURL["http://fake.tv/abc.ts"]; s.Name != "ABC HD" || s.TvgID != "us.abc" || s.GroupTitle != "US" || s.LogoUrl != "http://l/abc.png" || s.Disabled {
		t.Errorf("abc stream = %+v", s)
	}

	// Second refresh: NBC dropped, ABC renamed; row stays but is soft-disabled.
	body.Store("#EXTM3U\n" +
		"#EXTINF:-1 tvg-id=\"us.abc\" group-title=\"US\",ABC HD West\n" +
		"http://fake.tv/abc.ts\n" +
		"#EXTINF:-1 group-title=\"News\",News 24\n" +
		"http://fake.tv/news.ts\n")
	res2, err := ref.RefreshProvider(ctx, p)
	if err != nil {
		t.Fatalf("refresh 2: %v", err)
	}
	if res2.Seen != 2 || res2.Upserted != 2 || res2.Disabled != 1 {
		t.Fatalf("res2 = %+v", res2)
	}
	streams, _ = q.ListProviderStreams(ctx, db.ListProviderStreamsParams{ProviderID: p.ID, Limit: 100, Offset: 0})
	if len(streams) != 3 {
		t.Fatalf("row count changed to %d; stale rows must persist (soft-disable)", len(streams))
	}
	for _, s := range streams {
		switch s.StreamUrl {
		case "http://fake.tv/nbc.ts":
			if !s.Disabled {
				t.Error("nbc should be soft-disabled")
			}
		case "http://fake.tv/abc.ts":
			if s.Disabled || s.Name != "ABC HD West" {
				t.Errorf("abc = %+v", s)
			}
		case "http://fake.tv/news.ts":
			if s.Disabled {
				t.Errorf("news = %+v", s)
			}
		}
	}

	// Third refresh, identical content: nothing gets disabled.
	res3, err := ref.RefreshProvider(ctx, p)
	if err != nil || res3.Disabled != 0 {
		t.Fatalf("refresh 3 = %+v, %v", res3, err)
	}

	// The provider breaks: an empty body, a header-only playlist, or video
	// bytes instead of a playlist. Each refresh is refused and nothing is
	// disabled or marked refreshed.
	before, _ := q.GetProvider(ctx, p.ID)
	for name, broken := range map[string]string{
		"empty":       "",
		"header only": "#EXTM3U\n",
		"not m3u":     strings.Repeat("\x47\x40\x00\x10", 400),
	} {
		body.Store(broken)
		if _, err := ref.RefreshProvider(ctx, p); !errors.Is(err, ErrNoStreams) {
			t.Errorf("%s: err = %v, want ErrNoStreams", name, err)
		}
	}
	streams, _ = q.ListProviderStreams(ctx, db.ListProviderStreamsParams{ProviderID: p.ID, Limit: 100, Offset: 0})
	for _, s := range streams {
		if s.StreamUrl != "http://fake.tv/nbc.ts" && s.Disabled {
			t.Errorf("%s disabled by a broken refresh", s.StreamUrl)
		}
	}
	if after, _ := q.GetProvider(ctx, p.ID); !after.LastRefreshedAt.Equal(*before.LastRefreshedAt) {
		t.Error("a refused refresh marked the provider refreshed")
	}
}

func TestRefreshProviderXtream(t *testing.T) {
	w, q := newTestDB(t)
	ctx := context.Background()

	// Minimal fake panel.
	mux := http.NewServeMux()
	mux.HandleFunc("/player_api.php", func(w2 http.ResponseWriter, r *http.Request) {
		v := r.URL.Query()
		w2.Header().Set("Content-Type", "application/json")
		switch v.Get("action") {
		case "":
			_, _ = io.WriteString(w2, `{"user_info":{"auth":1},"server_info":{"url":"x"}}`)
		case "get_live_categories":
			_, _ = io.WriteString(w2, `[{"category_id":"7","category_name":"Sports","parent_id":0}]`)
		case "get_live_streams":
			_, _ = io.WriteString(w2, `[
			 {"num":1,"name":"Sport 1","stream_type":"live","stream_id":501,"stream_icon":"http://l/s1.png","epg_channel_id":"SP1","category_id":"7"},
			 {"num":2,"name":"A Movie","stream_type":"movie","stream_id":502,"category_id":"7"}
			]`)
		}
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	p, err := q.CreateProvider(ctx, db.CreateProviderParams{
		Name: "Fake Xtream", Type: "xtream", Url: ts.URL, Username: "u", Password: "p",
		MaxConnections: 1, RefreshIntervalSecs: 3600, Priority: 2, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ref := NewRefresher(slog.New(slog.NewTextHandler(io.Discard, nil)), w.pool)
	res, err := ref.RefreshProvider(ctx, p)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if res.Seen != 1 || res.Upserted != 1 {
		t.Fatalf("non-live streams must be skipped: %+v", res)
	}
	streams, _ := q.ListProviderStreams(ctx, db.ListProviderStreamsParams{ProviderID: p.ID, Limit: 10, Offset: 0})
	if len(streams) != 1 {
		t.Fatalf("streams = %d", len(streams))
	}
	s := streams[0]
	if s.ExternalID != "501" || s.Name != "Sport 1" || s.TvgID != "SP1" || s.GroupTitle != "Sports" {
		t.Errorf("stream = %+v", s)
	}
	if s.StreamUrl != ts.URL+"/live/u/p/501.ts" {
		t.Errorf("stream url = %q", s.StreamUrl)
	}
}

func TestRefreshEPGReplaces(t *testing.T) {
	w, q := newTestDB(t)
	ctx := context.Background()

	guide := &atomic.Value{}
	guide.Store(`<?xml version="1.0"?><tv>
<programme start="20261013180000 +0000" stop="20261013190000 +0000" channel="us.abc"><title>A</title></programme>
</tv>`)
	ts := httptest.NewServer(http.HandlerFunc(func(w2 http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w2, guide.Load().(string))
	}))
	t.Cleanup(ts.Close)

	p, err := q.CreateProvider(ctx, db.CreateProviderParams{
		Name: "EPG Source", Type: "m3u", Url: "http://unused.example/", EpgUrl: ts.URL,
		MaxConnections: 1, RefreshIntervalSecs: 3600, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ref := NewRefresher(slog.New(slog.NewTextHandler(io.Discard, nil)), w.pool)

	n, err := ref.RefreshEPG(ctx, p)
	if err != nil {
		t.Fatalf("epg refresh 1: %v", err)
	}
	if n != 1 {
		t.Fatalf("n = %d", n)
	}

	// Second refresh with different content replaces, not appends.
	guide.Store(`<?xml version="1.0"?><tv>
<programme start="20261013200000 +0000" stop="20261013210000 +0000" channel="us.abc"><title>B</title></programme>
<programme start="20261013210000 +0000" stop="20261013220000 +0000" channel="us.nbc"><title>C</title></programme>
</tv>`)
	if _, err := ref.RefreshEPG(ctx, p); err != nil {
		t.Fatalf("epg refresh 2: %v", err)
	}
	src, err := q.GetEpgSourceByProvider(ctx, &p.ID)
	if err != nil {
		t.Fatalf("get epg source: %v", err)
	}
	n2, err := q.CountEpgProgrammes(ctx, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 2 {
		t.Errorf("programmes after replace = %d, want 2", n2)
	}
	if src.LastRefreshedAt == nil {
		t.Error("epg source last_refreshed_at not set")
	}
}
