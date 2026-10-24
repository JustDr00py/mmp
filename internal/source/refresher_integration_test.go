//go:build integration

package source

import (
	"context"
	"errors"
	"fmt"
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

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/testpg"
)

type pgxpoolWrapper struct{ pool *pgxpool.Pool }

func newTestDB(t *testing.T) (*pgxpoolWrapper, *db.Queries) {
	t.Helper()
	// Dedicated database per package: concurrent package tests must not
	// recreate each other's database.
	dsn := testpg.DSN("mmp_source_test")
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
		testpg.Unreachable(t, err)
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
	ref := NewRefresher(slog.New(slog.NewTextHandler(io.Discard, nil)), w.pool, 80)

	// First refresh: three streams appear.
	res1, err := ref.RefreshProvider(ctx, p, false)
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
	res2, err := ref.RefreshProvider(ctx, p, false)
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
	res3, err := ref.RefreshProvider(ctx, p, false)
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
		if _, err := ref.RefreshProvider(ctx, p, false); !errors.Is(err, ErrNoStreams) {
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
	ref := NewRefresher(slog.New(slog.NewTextHandler(io.Discard, nil)), w.pool, 80)
	res, err := ref.RefreshProvider(ctx, p, false)
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
	ref := NewRefresher(slog.New(slog.NewTextHandler(io.Discard, nil)), w.pool, 80)

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
	n2, err := q.CountEpgPrograms(ctx, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 2 {
		t.Errorf("programs after replace = %d, want 2", n2)
	}
	if src.ProgramCount != 2 || src.ChannelCount != 2 {
		t.Errorf("stored counts = %d programs, %d channels; want 2, 2", src.ProgramCount, src.ChannelCount)
	}
	if src.LastRefreshedAt == nil {
		t.Error("epg source last_refreshed_at not set")
	}

	// Real guides repeat programs (here more than one COPY batch apart)
	// and carry zero-length ones: both are skipped, the rest imports.
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><tv>`)
	dup := `<programme start="20261013200000 +0000" stop="20261013210000 +0000" channel="us.abc"><title>Dup</title></programme>`
	b.WriteString(dup)
	start := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	for i := range programChunk + 10 {
		st := start.Add(time.Duration(i) * time.Minute)
		fmt.Fprintf(&b, `<programme start="%s" stop="%s" channel="us.nbc"><title>P%d</title></programme>`,
			st.Format("20060102150405 -0700"), st.Add(time.Minute).Format("20060102150405 -0700"), i)
	}
	b.WriteString(dup) // a repeat in a later batch
	b.WriteString(`<programme start="20261015000000 +0000" stop="20261015000000 +0000" channel="us.abc"><title>Empty</title></programme>`)
	b.WriteString(`</tv>`)
	guide.Store(b.String())
	if _, err := ref.RefreshEPG(ctx, p); err != nil {
		t.Fatalf("epg refresh with duplicates: %v", err)
	}
	if n, _ := q.CountEpgPrograms(ctx, src.ID); n != int64(programChunk+11) {
		t.Errorf("programs = %d, want %d (duplicate and empty skipped)", n, programChunk+11)
	}
}

// playlist builds an M3U with streams n0..n(count-1).
func playlist(count int) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	for i := range count {
		fmt.Fprintf(&b, "#EXTINF:-1 group-title=\"G\",Chan %d\nhttp://fake.tv/%d.ts\n", i, i)
	}
	return b.String()
}

// TestRefreshRefusesTruncatedList is the 8.2c AC: a provider answering
// with 10% of its list is refused (nothing disabled) unless forced, and
// the per-provider override moves the threshold either way.
func TestRefreshRefusesTruncatedList(t *testing.T) {
	w, q := newTestDB(t)
	ts, body := newM3UServer(t)
	ctx := context.Background()
	body.Store(playlist(20))
	p, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: "Big", Type: "m3u", Url: ts.URL, UserAgent: "MMP-Test/1.0",
		MaxConnections: 1, RefreshIntervalSecs: 3600, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	ref := NewRefresher(slog.New(slog.DiscardHandler), w.pool, 80)
	if _, err := ref.RefreshProvider(ctx, p, false); err != nil {
		t.Fatal(err)
	}
	active := func() int64 { n, _ := q.CountActiveProviderStreams(ctx, p.ID); return n }
	refreshedAt := func() time.Time { pr, _ := q.GetProvider(ctx, p.ID); return *pr.LastRefreshedAt }

	// 90% truncated: refused, rolled back.
	body.Store(playlist(2))
	before := refreshedAt()
	_, err = ref.RefreshProvider(ctx, p, false)
	var refused *DisableRefusedError
	if !errors.As(err, &refused) || refused.WouldDisable != 18 || refused.Active != 20 || refused.Pct() != 90 || refused.MaxPct != 80 {
		t.Fatalf("err = %v (%+v), want a refusal of 18/20", err, refused)
	}
	if n := active(); n != 20 || !refreshedAt().Equal(before) {
		t.Fatalf("refused refresh changed state: %d active", n)
	}

	// A normal drop (20%) still applies.
	body.Store(playlist(16))
	if res, err := ref.RefreshProvider(ctx, p, false); err != nil || res.Disabled != 4 {
		t.Fatalf("20%% drop = %+v, %v", res, err)
	}

	// Forced: the truncated list applies.
	body.Store(playlist(2))
	if res, err := ref.RefreshProvider(ctx, p, true); err != nil || res.Disabled != 14 || active() != 2 {
		t.Fatalf("forced = %+v, %v (active %d)", res, err, active())
	}

	// Per-provider override: 100 turns the guard off; 10 makes it strict.
	body.Store(playlist(20))
	if _, err := ref.RefreshProvider(ctx, p, false); err != nil { // re-enabling is never refused
		t.Fatal(err)
	}
	pct := func(v int32) {
		cur, _ := q.GetProvider(ctx, p.ID)
		upd, err := q.UpdateProvider(ctx, db.UpdateProviderParams{ID: cur.ID, Name: cur.Name, Type: cur.Type, Url: cur.Url,
			UserAgent: cur.UserAgent, MaxConnections: cur.MaxConnections, RefreshIntervalSecs: cur.RefreshIntervalSecs,
			Enabled: cur.Enabled, RefreshMaxDisablePct: &v})
		if err != nil {
			t.Fatal(err)
		}
		p = upd
	}
	pct(10)
	body.Store(playlist(16)) // 20% > 10%
	if _, err := ref.RefreshProvider(ctx, p, false); !errors.As(err, &refused) || refused.MaxPct != 10 {
		t.Errorf("strict override: err = %v", err)
	}
	pct(100)
	body.Store(playlist(2))
	if res, err := ref.RefreshProvider(ctx, p, false); err != nil || res.Disabled != 18 {
		t.Errorf("guard off: %+v, %v", res, err)
	}
}

// TestProviderHealth: a provider that stops answering is marked failing,
// retried every few minutes rather than every tick, and cleared once it
// answers again.
func TestProviderHealth(t *testing.T) {
	defer func(d time.Duration) { checkRetryAfter = d }(checkRetryAfter)
	checkRetryAfter = time.Millisecond
	w, q := newTestDB(t)
	ctx := context.Background()
	ts, _ := newM3UServer(t)
	r := NewRefresher(slog.New(slog.DiscardHandler), w.pool, 80)
	p, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: "P", Type: "m3u", Url: ts.URL + "/list.m3u",
		UserAgent: "MMP-Test/1.0", MaxConnections: 1, RefreshIntervalSecs: 3600, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	get := func() db.Provider {
		t.Helper()
		got, err := q.GetProvider(ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	p.Url = "http://mmp-no-such-host.invalid/list.m3u"
	if _, err := r.RefreshProvider(ctx, p, false); err == nil {
		t.Fatal("refresh of a dead host succeeded")
	}
	got := get()
	if !strings.Contains(got.LastError, "does not exist (DNS)") || got.FailingSince == nil {
		t.Fatalf("after failed refresh: error %q since %v", got.LastError, got.FailingSince)
	}
	first := *got.FailingSince
	due, err := q.ListDueProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Errorf("a provider that just failed is due again: %v", due)
	}

	// A failing health check keeps the first failure time.
	if err := r.CheckProvider(ctx, p); err == nil {
		t.Fatal("check of a dead host succeeded")
	}
	if got := get(); got.FailingSince == nil || !got.FailingSince.Equal(first) {
		t.Errorf("failing since moved: %v, want %v", got.FailingSince, first)
	}

	p.Url = ts.URL + "/list.m3u"
	if err := r.CheckProvider(ctx, p); err != nil {
		t.Fatalf("check: %v", err)
	}
	if got := get(); got.LastError != "" || got.FailingSince != nil || got.CheckedAt == nil {
		t.Errorf("after recovery: error %q since %v checked %v", got.LastError, got.FailingSince, got.CheckedAt)
	}
}

// TestProviderCheckRetries: a check refused once (a panel throttling a
// login that followed another) is asked again and not marked failing.
func TestProviderCheckRetries(t *testing.T) {
	defer func(d time.Duration) { checkRetryAfter = d }(checkRetryAfter)
	checkRetryAfter = time.Millisecond
	w, q := newTestDB(t)
	ctx := context.Background()
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			rw.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(rw, "#EXTM3U\n")
	}))
	t.Cleanup(ts.Close)
	r := NewRefresher(slog.New(slog.DiscardHandler), w.pool, 80)
	p, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: "P", Type: "m3u", Url: ts.URL + "/list.m3u",
		MaxConnections: 1, RefreshIntervalSecs: 3600, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.CheckProvider(ctx, p); err != nil {
		t.Fatalf("check: %v", err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("server asked %d times, want 2", n)
	}
	got, err := q.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastError != "" || got.FailingSince != nil {
		t.Errorf("marked failing after one refusal: %q since %v", got.LastError, got.FailingSince)
	}
}
