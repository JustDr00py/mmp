//go:build integration

package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/sysadmin/mmp/internal/auth"
	"github.com/sysadmin/mmp/internal/channel"
	"github.com/sysadmin/mmp/internal/db"
	"github.com/sysadmin/mmp/internal/proxy"
)

func newAPITestEnv(t *testing.T) *Env {
	t.Helper()
	dsn := "postgres://mmp:mmp@localhost:5432/mmp_api_test?sslmode=disable"
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
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	auth.BcryptCost = bcrypt.MinCost // fast hashing under -race
	log := slog.New(slog.NewTextHandler(nilDiscard{}, nil))
	return &Env{Log: log, Store: db.New(pool), Match: channel.NewService(log, pool),
		InTx: func(ctx context.Context, fn func(Store) error) error {
			return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { return fn(db.New(tx)) })
		}}
}

type nilDiscard struct{}

func (nilDiscard) Write(p []byte) (int, error) { return len(p), nil }

func apiServer(env *Env) http.Handler {
	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) { Mount(r, env) })
	return r
}

func call(t *testing.T, h http.Handler, method, path string, body any) (int, map[string]any, []any) {
	t.Helper()
	var rd *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rd)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var obj map[string]any
	var arr []any
	if rec.Body.Len() > 0 {
		if strings.HasPrefix(strings.TrimSpace(rec.Body.String()), "[") {
			_ = json.Unmarshal(rec.Body.Bytes(), &arr)
		} else {
			_ = json.Unmarshal(rec.Body.Bytes(), &obj)
		}
	}
	return rec.Code, obj, arr
}

func TestChannelsGroupsBouquetsCRUD(t *testing.T) {
	env := newAPITestEnv(t)
	h := apiServer(env)

	// Groups.
	code, g, _ := call(t, h, http.MethodPost, "/api/v1/channel-groups", map[string]any{"name": "News", "sort_order": 5})
	if code != http.StatusCreated {
		t.Fatalf("create group = %d %v", code, g)
	}
	groupID := int64(g["id"].(float64))
	code, _, groups := call(t, h, http.MethodGet, "/api/v1/channel-groups", nil)
	if code != 200 || len(groups) != 1 {
		t.Fatalf("list groups = %d %v", code, groups)
	}
	code, _, _ = call(t, h, http.MethodPut, "/api/v1/channel-groups/"+strconv.FormatInt(groupID, 10), map[string]any{"name": "News & Docs", "sort_order": 6})
	if code != 200 {
		t.Fatalf("update group = %d", code)
	}

	// Channels.
	code, c, _ := call(t, h, http.MethodPost, "/api/v1/channels", map[string]any{
		"name": "ABC News", "tvg_id": "us.abc.news", "group_id": groupID,
	})
	if code != http.StatusCreated {
		t.Fatalf("create channel = %d %v", code, c)
	}
	if c["number"].(float64) != 101 { // first auto number is 101 (max(100)+1)
		t.Errorf("auto number = %v, want 101", c["number"])
	}
	channelID := int64(c["id"].(float64))
	code, c2, _ := call(t, h, http.MethodPost, "/api/v1/channels", map[string]any{"name": "News 24"})
	if code != http.StatusCreated || c2["number"].(float64) != 102 {
		t.Fatalf("second channel = %d %v", code, c2)
	}
	// Search filter.
	code, _, arr := call(t, h, http.MethodGet, "/api/v1/channels?q=news%2024", nil)
	if code != 200 || len(arr) != 1 {
		t.Fatalf("search = %d %v", code, arr)
	}
	// Group filter.
	code, _, arr = call(t, h, http.MethodGet, "/api/v1/channels?group_id="+strconv.FormatInt(groupID, 10), nil)
	if code != 200 || len(arr) != 1 {
		t.Fatalf("group filter = %d %v", code, arr)
	}
	// Update + 404 paths.
	code, _, _ = call(t, h, http.MethodPut, "/api/v1/channels/"+strconv.FormatInt(channelID, 10), map[string]any{"name": "ABC News Now", "group_id": groupID, "enabled": false})
	if code != 200 {
		t.Fatalf("update channel = %d", code)
	}
	code, _, _ = call(t, h, http.MethodGet, "/api/v1/channels/999", nil)
	if code != http.StatusNotFound {
		t.Errorf("missing channel = %d, want 404", code)
	}

	// Bouquets.
	code, b, _ := call(t, h, http.MethodPost, "/api/v1/bouquets", map[string]any{"name": "Basic"})
	if code != http.StatusCreated {
		t.Fatalf("create bouquet = %d", code)
	}
	bouquetID := int64(b["id"].(float64))
	code, _, _ = call(t, h, http.MethodPost, "/api/v1/bouquets/"+strconv.FormatInt(bouquetID, 10)+"/channels", map[string]any{"channel_id": channelID})
	if code != http.StatusCreated {
		t.Fatalf("add to bouquet = %d", code)
	}
	code, _, arr = call(t, h, http.MethodGet, "/api/v1/bouquets/"+strconv.FormatInt(bouquetID, 10)+"/channels", nil)
	if code != 200 || len(arr) != 1 || arr[0].(map[string]any)["name"] != "ABC News Now" {
		t.Fatalf("bouquet channels = %d %v", code, arr)
	}
	// Adding the same channel twice is idempotent.
	code, _, _ = call(t, h, http.MethodPost, "/api/v1/bouquets/"+strconv.FormatInt(bouquetID, 10)+"/channels", map[string]any{"channel_id": channelID})
	if code != http.StatusCreated {
		t.Fatalf("re-add = %d", code)
	}
	code, _, arr = call(t, h, http.MethodGet, "/api/v1/bouquets/"+strconv.FormatInt(bouquetID, 10)+"/channels", nil)
	if len(arr) != 1 {
		t.Fatalf("bouquet channels after re-add = %v", arr)
	}
	code, _, _ = call(t, h, http.MethodDelete, "/api/v1/bouquets/"+strconv.FormatInt(bouquetID, 10)+"/channels/"+strconv.FormatInt(channelID, 10), nil)
	if code != http.StatusNoContent {
		t.Fatalf("remove from bouquet = %d", code)
	}
}

func TestChannelStreamPriorityAndReorder(t *testing.T) {
	env := newAPITestEnv(t)
	h := apiServer(env)
	ctx := context.Background()

	// Two providers with streams, one channel.
	q := env.Store.(*db.Queries) // provider-stream seeding is not on the API Store interface
	p1, _ := q.CreateProvider(ctx, db.CreateProviderParams{Name: "A", Type: "m3u", Url: "http://a/", MaxConnections: 1, RefreshIntervalSecs: 3600, Enabled: true})
	p2, _ := q.CreateProvider(ctx, db.CreateProviderParams{Name: "B", Type: "m3u", Url: "http://b/", MaxConnections: 1, RefreshIntervalSecs: 3600, Enabled: true})
	s1, _ := q.UpsertProviderStream(ctx, db.UpsertProviderStreamParams{ProviderID: p1.ID, ExternalID: "1", Name: "News HD", StreamUrl: "http://a/1"})
	s2, _ := q.UpsertProviderStream(ctx, db.UpsertProviderStreamParams{ProviderID: p1.ID, ExternalID: "2", Name: "News FHD", StreamUrl: "http://a/2"})
	s3, _ := q.UpsertProviderStream(ctx, db.UpsertProviderStreamParams{ProviderID: p2.ID, ExternalID: "3", Name: "US: News", StreamUrl: "http://b/3"})

	code, c, _ := call(t, h, http.MethodPost, "/api/v1/channels", map[string]any{"name": "News"})
	if code != http.StatusCreated {
		t.Fatalf("create channel = %d", code)
	}
	chID := int64(c["id"].(float64))

	// Add streams; default priority appends in order.
	for _, sid := range []int64{s1, s2, s3} {
		code, _, _ = call(t, h, http.MethodPost, "/api/v1/channels/"+strconv.FormatInt(chID, 10)+"/streams", map[string]any{"provider_stream_id": sid})
		if code != http.StatusCreated {
			t.Fatalf("add stream %d = %d", sid, code)
		}
	}
	code, _, arr := call(t, h, http.MethodGet, "/api/v1/channels/"+strconv.FormatInt(chID, 10)+"/streams", nil)
	if code != 200 || len(arr) != 3 {
		t.Fatalf("streams = %d %v", code, arr)
	}
	if arr[0].(map[string]any)["stream_name"] != "News HD" {
		t.Errorf("order before = %v", arr)
	}
	// Joined provider data is present.
	if arr[0].(map[string]any)["provider_name"] != "A" {
		t.Errorf("provider join missing: %v", arr[0])
	}

	// Reorder: B's stream first.
	code, _, arr = call(t, h, http.MethodPut, "/api/v1/channels/"+strconv.FormatInt(chID, 10)+"/streams/order",
		map[string]any{"provider_stream_ids": []int64{s3, s1, s2}})
	if code != 200 {
		t.Fatalf("reorder = %d", code)
	}
	if arr[0].(map[string]any)["provider_name"] != "B" {
		t.Errorf("reorder response = %v", arr)
	}

	// AC: reorder persists.
	fresh, err := q.ListChannelStreams(ctx, chID)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 3 || fresh[0].ProviderStreamID != s3 || fresh[1].ProviderStreamID != s1 || fresh[2].ProviderStreamID != s2 {
		t.Errorf("persisted order wrong: %+v", fresh)
	}

	// Remove one.
	code, _, _ = call(t, h, http.MethodDelete, "/api/v1/channels/"+strconv.FormatInt(chID, 10)+"/streams/"+strconv.FormatInt(s2, 10), nil)
	if code != http.StatusNoContent {
		t.Fatalf("remove stream = %d", code)
	}
	fresh, _ = q.ListChannelStreams(ctx, chID)
	if len(fresh) != 2 {
		t.Errorf("after remove = %+v", fresh)
	}
}

// TestMatcherThreeProviders is the 3.4 AC end to end: three providers name
// the same channels differently; the matcher groups them, apply creates one
// channel per group with a provider-priority failover list, and a second
// apply is a no-op.
func TestMatcherThreeProviders(t *testing.T) {
	env := newAPITestEnv(t)
	h := apiServer(env)
	ctx := context.Background()
	q := env.Store.(*db.Queries)

	type st struct{ ext, name, tvg string }
	seed := map[string][]st{ // provider name -> streams; priority = index
		"A": {{"a1", "US: ABC East HD", "abc.us"}, {"a2", "ESPN2", "espn2.us"}, {"a3", "Local Only", ""}},
		"B": {{"b1", "ABC EAST FHD", "abc.us"}, {"b2", "Sky Sports PL HD", ""}, {"b3", "ESPN 2 SD", ""}},
		"C": {{"c1", "abc-east (us)", ""}, {"c2", "ESPN 2 FHD", "ESPN2.us"}, {"c3", "|EN| Sky Sport Premier League", ""}},
	}
	ids := map[string]int64{}
	for i, name := range []string{"A", "B", "C"} {
		p, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: name, Type: "m3u", Url: "http://" + name + "/",
			MaxConnections: 1, RefreshIntervalSecs: 3600, Priority: int32(i), Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range seed[name] {
			id, err := q.UpsertProviderStream(ctx, db.UpsertProviderStreamParams{
				ProviderID: p.ID, ExternalID: s.ext, Name: s.name, TvgID: s.tvg, StreamUrl: "http://x/" + s.ext})
			if err != nil {
				t.Fatal(err)
			}
			ids[s.ext] = id
		}
	}

	// Invalid regex is rejected; a valid one is stored.
	code, _, _ := call(t, h, http.MethodPost, "/api/v1/match/rules", map[string]any{"pattern": "(", "channel_name": "X"})
	if code != http.StatusBadRequest {
		t.Errorf("bad pattern = %d, want 400", code)
	}
	code, _, _ = call(t, h, http.MethodPost, "/api/v1/match/rules", map[string]any{
		"pattern": `sky ?sports? ?(pl|premier league)`, "channel_name": "Sky Sports Premier League"})
	if code != http.StatusCreated {
		t.Fatalf("create rule = %d", code)
	}

	// Preview: 3 multi-provider groups + the singleton.
	code, _, all := call(t, h, http.MethodGet, "/api/v1/match/proposals", nil)
	if code != 200 || len(all) != 4 {
		t.Fatalf("proposals = %d %v", code, all)
	}
	code, _, multi := call(t, h, http.MethodGet, "/api/v1/match/proposals?min_providers=2", nil)
	if code != 200 || len(multi) != 3 {
		t.Fatalf("proposals min 2 = %d %v", code, multi)
	}

	// Auto-create only cross-provider groups.
	code, res, _ := call(t, h, http.MethodPost, "/api/v1/match/apply", map[string]any{"min_providers": 2})
	if code != 200 || res["created"] != float64(3) || res["attached"] != float64(8) {
		t.Fatalf("apply = %d %v", code, res)
	}

	chans, err := q.ListChannels(ctx, db.ListChannelsParams{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]int64{
		"ABC East":                  {ids["a1"], ids["b1"], ids["c1"]},
		"ESPN2":                     {ids["a2"], ids["b3"], ids["c2"]},
		"Sky Sports Premier League": {ids["b2"], ids["c3"]},
	}
	if len(chans) != len(want) {
		t.Fatalf("channels = %+v", chans)
	}
	for _, c := range chans {
		order, ok := want[c.Name]
		if !ok {
			t.Errorf("unexpected channel %q", c.Name)
			continue
		}
		cs, err := q.ListChannelStreams(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]int64, len(cs))
		for i, s := range cs {
			got[i] = s.ProviderStreamID
		}
		if !slices.Equal(got, order) {
			t.Errorf("%s failover order = %v, want %v", c.Name, got, order)
		}
	}

	// Remaining: only the singleton. Confirm it by key; it gets a channel too.
	code, _, rest := call(t, h, http.MethodGet, "/api/v1/match/proposals", nil)
	if code != 200 || len(rest) != 1 {
		t.Fatalf("remaining proposals = %v", rest)
	}
	key := rest[0].(map[string]any)["key"].(string)
	code, res, _ = call(t, h, http.MethodPost, "/api/v1/match/apply", map[string]any{"keys": []string{key}})
	if code != 200 || res["created"] != float64(1) || res["attached"] != float64(1) {
		t.Fatalf("apply by key = %d %v", code, res)
	}
	code, res, _ = call(t, h, http.MethodPost, "/api/v1/match/apply", map[string]any{})
	if code != 200 || res["created"] != float64(0) || res["attached"] != float64(0) {
		t.Fatalf("second apply should be a no-op: %d %v", code, res)
	}

	// A new provider stream for an existing channel attaches, appended last.
	d, _ := q.CreateProvider(ctx, db.CreateProviderParams{Name: "D", Type: "m3u", Url: "http://d/",
		MaxConnections: 1, RefreshIntervalSecs: 3600, Priority: 0, Enabled: true})
	d1, _ := q.UpsertProviderStream(ctx, db.UpsertProviderStreamParams{ProviderID: d.ID, ExternalID: "d1", Name: "ABC East 4K", StreamUrl: "http://d/1"})
	code, res, _ = call(t, h, http.MethodPost, "/api/v1/match/apply", map[string]any{})
	if code != 200 || res["created"] != float64(0) || res["attached"] != float64(1) {
		t.Fatalf("attach to existing = %d %v", code, res)
	}
	abc, _ := q.ListChannels(ctx, db.ListChannelsParams{Limit: 10, Search: ptr("ABC East")})
	cs, _ := q.ListChannelStreams(ctx, abc[0].ID)
	if len(cs) != 4 || cs[3].ProviderStreamID != d1 {
		t.Errorf("existing channel after attach = %+v", cs)
	}
}

func ptr[T any](v T) *T { return &v }

// fakeTSUpstream serves an endless paced TS (a PAT then 9 payload packets,
// repeated) and counts requests and open connections.
func fakeTSUpstream(t *testing.T) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	return fakeTSUpstreamN(t, 0)
}

// fakeTSUpstreamN ends the response after groups packet groups (0 = never).
func fakeTSUpstreamN(t *testing.T, groups int) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var hits, open atomic.Int32
	pkt := func(pid uint16, pusi bool, cc byte) []byte {
		b := make([]byte, 188)
		b[0], b[1], b[2], b[3] = 0x47, byte(pid>>8)&0x1F, byte(pid), 0x10|cc&0x0F
		if pusi {
			b[1] |= 0x40
		}
		return b
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		open.Add(1)
		defer open.Add(-1)
		w.Header().Set("Content-Type", "video/mp2t")
		var cc byte
		for sent := 0; groups == 0 || sent < groups; sent++ {
			var group []byte
			group = append(group, pkt(0, true, cc)...)
			for range 9 {
				group = append(group, pkt(0x100, false, cc)...)
			}
			cc++
			if _, err := w.Write(group); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits, &open
}

// TestChannelStreamEndToEnd is the 4.7 path through the real stack:
// channel row -> DBSource -> proxy manager -> /channels/{id}/stream. Two
// clients share one upstream request; it is released after idle grace.
func TestChannelStreamEndToEnd(t *testing.T) {
	env := newAPITestEnv(t)
	ctx := context.Background()
	q := env.Store.(*db.Queries)
	up, hits, open := fakeTSUpstream(t)

	capacity := proxy.NewCapacity()
	mgr := proxy.NewManager(ctx, proxy.ManagerConfig{
		Source: proxy.DBSource(q), Opener: proxy.NewHTTPOpener(2 * time.Second),
		Capacity: capacity, Cooldowns: proxy.NewCooldowns(time.Second, 10*time.Second),
		RingSize: 4 << 20, IdleGrace: 200 * time.Millisecond, MaxRetryWindow: 5 * time.Second,
		Thresholds: proxy.Thresholds{StallTimeout: 2 * time.Second, Window: 5 * time.Second},
	})
	t.Cleanup(mgr.Shutdown)
	env.Streams = mgr
	srv := httptest.NewServer(apiServer(env))
	t.Cleanup(srv.Close)

	p, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: "P", Type: "m3u", Url: "http://p/",
		MaxConnections: 1, RefreshIntervalSecs: 3600, Enabled: true, UserAgent: "TestUA/1"})
	if err != nil {
		t.Fatal(err)
	}
	sid, err := q.UpsertProviderStream(ctx, db.UpsertProviderStreamParams{ProviderID: p.ID, ExternalID: "1",
		Name: "News", StreamUrl: up.URL + "/live/user/secret/1.ts"})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := q.CreateChannel(ctx, db.CreateChannelParams{Name: "News"})
	if err != nil {
		t.Fatal(err)
	}
	empty, err := q.CreateChannel(ctx, db.CreateChannelParams{Name: "Empty"})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.AddStreamToChannel(ctx, db.AddStreamToChannelParams{ChannelID: ch.ID, ProviderStreamID: sid, Priority: 1}); err != nil {
		t.Fatal(err)
	}
	streamURL := func(id int64) string { return srv.URL + "/api/v1/channels/" + strconv.FormatInt(id, 10) + "/stream" }

	for id, want := range map[int64]int{999999: http.StatusNotFound, empty.ID: http.StatusConflict} {
		resp, err := http.Get(streamURL(id))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("channel %d: status %d, want %d", id, resp.StatusCode, want)
		}
	}

	// Two clients at once: both get aligned TS, one upstream request.
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Go(func() {
			resp, err := http.Get(streamURL(ch.ID))
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "video/mp2t" {
				t.Errorf("client %d: %d %s", i, resp.StatusCode, resp.Header.Get("Content-Type"))
				return
			}
			buf := make([]byte, 188*500)
			if _, err := io.ReadFull(resp.Body, buf); err != nil {
				t.Errorf("client %d read: %v", i, err)
				return
			}
			for off := 0; off < len(buf); off += 188 {
				if buf[off] != 0x47 {
					t.Errorf("client %d: unaligned at %d", i, off)
					return
				}
			}
		})
	}
	wg.Wait()
	if h := hits.Load(); h != 1 {
		t.Errorf("upstream requests = %d, want 1", h)
	}

	// Clients gone: after idle grace the slot and upstream are released.
	deadline := time.Now().Add(5 * time.Second)
	for capacity.InUse(p.ID) != 0 || open.Load() != 0 || len(mgr.Channels()) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("not torn down: slots=%d open=%d channels=%v", capacity.InUse(p.ID), open.Load(), mgr.Channels())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestFailoverEventPersisted is the 4.8 AC: a provider switch writes a
// failover row to the events table.
func TestFailoverEventPersisted(t *testing.T) {
	env := newAPITestEnv(t)
	ctx := context.Background()
	q := env.Store.(*db.Queries)
	upA, _, _ := fakeTSUpstreamN(t, 50) // ends after 500 packets
	upB, _, _ := fakeTSUpstream(t)

	rec := proxy.NewEventRecorder(q, nil, 64)
	recCtx, stopRec := context.WithCancel(ctx)
	recDone := make(chan struct{})
	go func() { rec.Run(recCtx); close(recDone) }()
	mgr := proxy.NewManager(ctx, proxy.ManagerConfig{
		Source: proxy.DBSource(q), Opener: proxy.NewHTTPOpener(2 * time.Second),
		Capacity: proxy.NewCapacity(), Cooldowns: proxy.NewCooldowns(time.Minute, time.Hour),
		RingSize: 4 << 20, IdleGrace: time.Second, MaxRetryWindow: 5 * time.Second,
		Thresholds: proxy.Thresholds{StallTimeout: 2 * time.Second, Window: 5 * time.Second},
		OnEvent:    rec.Record,
	})

	var sids []int64
	for i, u := range []string{upA.URL, upB.URL} {
		p, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: "P" + strconv.Itoa(i), Type: "m3u",
			Url: "http://p/", MaxConnections: 1, RefreshIntervalSecs: 3600, Priority: int32(i), Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		sid, err := q.UpsertProviderStream(ctx, db.UpsertProviderStreamParams{ProviderID: p.ID,
			ExternalID: "1", Name: "News", StreamUrl: u})
		if err != nil {
			t.Fatal(err)
		}
		sids = append(sids, sid)
	}
	ch, err := q.CreateChannel(ctx, db.CreateChannelParams{Name: "News"})
	if err != nil {
		t.Fatal(err)
	}
	for i, sid := range sids {
		if err := q.AddStreamToChannel(ctx, db.AddStreamToChannelParams{ChannelID: ch.ID, ProviderStreamID: sid, Priority: int32(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}

	v, err := mgr.Attach(ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Watch until the session reports B playing, reading so the ring moves.
	readCtx, cancelRead := context.WithTimeout(ctx, 10*time.Second)
	defer cancelRead()
	buf := make([]byte, 64*1024)
	for {
		if _, err := v.Read(readCtx, buf); err != nil {
			t.Fatalf("viewer: %v", err)
		}
		if st := mgr.Channels(); len(st) == 1 && st[0].StreamID == sids[1] && st[0].State == "streaming" {
			break
		}
	}
	v.Close()
	mgr.Shutdown()
	stopRec()
	<-recDone

	evs, err := q.ListRecentEvents(ctx, db.ListRecentEventsParams{ChannelID: &ch.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range evs {
		if e.Type == proxy.EventFailover {
			found = true
			if e.FromStreamID == nil || *e.FromStreamID != sids[0] || e.ToStreamID == nil || *e.ToStreamID != sids[1] ||
				e.ChannelID == nil || *e.ChannelID != ch.ID {
				t.Errorf("failover row = %+v", e)
			}
		}
	}
	if !found {
		t.Fatalf("no failover event row; events = %+v", evs)
	}
}

// TestUsersAPI covers 5.1 through the API: CRUD, hash never exposed,
// password kept on empty update, validation, duplicates, bouquets, token
// rotation, and the last-admin guard.
func TestUsersAPI(t *testing.T) {
	env := newAPITestEnv(t)
	h := apiServer(env)
	ctx := context.Background()
	q := env.Store.(*db.Queries)
	b1, _ := q.CreateBouquet(ctx, db.CreateBouquetParams{Name: "Basic"})
	b2, _ := q.CreateBouquet(ctx, db.CreateBouquetParams{Name: "Sports"})

	// Validation.
	for name, body := range map[string]map[string]any{
		"bad username": {"username": "a/b", "password": "secret1"},
		"short pw":     {"username": "bob", "password": "x"},
		"slash pw":     {"username": "bob", "password": "sec/ret1"},
		"bad role":     {"username": "bob", "password": "secret1", "role": "root"},
		"neg conns":    {"username": "bob", "password": "secret1", "max_connections": -1},
	} {
		if code, _, _ := call(t, h, http.MethodPost, "/api/v1/users", body); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, code)
		}
	}

	// Create: defaults, bouquets, no hash in the response.
	exp := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second)
	code, u, _ := call(t, h, http.MethodPost, "/api/v1/users", map[string]any{
		"username": "alice", "password": "alice-pw1", "max_connections": 2,
		"expires_at": exp, "bouquet_ids": []int64{b1.ID, b2.ID},
	})
	if code != http.StatusCreated {
		t.Fatalf("create = %d %v", code, u)
	}
	if _, leaked := u["password_hash"]; leaked {
		t.Fatal("password_hash serialized")
	}
	if u["role"] != "customer" || u["enabled"] != true || u["max_connections"] != float64(2) ||
		len(u["stream_token"].(string)) != 43 || len(u["bouquet_ids"].([]any)) != 2 {
		t.Errorf("created user = %v", u)
	}
	uid := int64(u["id"].(float64))
	path := "/api/v1/users/" + strconv.FormatInt(uid, 10)
	stored, _ := q.GetUser(ctx, uid)
	if !auth.CheckPassword(stored.PasswordHash, "alice-pw1") {
		t.Error("stored hash does not verify")
	}

	if code, _, _ := call(t, h, http.MethodPost, "/api/v1/users", map[string]any{"username": "alice", "password": "other-pw1"}); code != http.StatusConflict {
		t.Errorf("duplicate username = %d, want 409", code)
	}
	if code, _, _ := call(t, h, http.MethodPost, "/api/v1/users", map[string]any{"username": "carol", "password": "carol-pw1", "bouquet_ids": []int64{99999}}); code != http.StatusBadRequest {
		t.Errorf("unknown bouquet = %d, want 400", code)
	}
	if _, err := q.GetUserByUsername(ctx, "carol"); !isNotFound(err) {
		t.Error("failed create left a half-created user (not atomic)")
	}

	// Update without password keeps it; bouquets replaced; expiry cleared.
	code, u, _ = call(t, h, http.MethodPut, path, map[string]any{"max_connections": 3, "bouquet_ids": []int64{b2.ID}, "clear_expiry": true})
	if code != 200 || u["max_connections"] != float64(3) || len(u["bouquet_ids"].([]any)) != 1 || u["expires_at"] != nil {
		t.Fatalf("update = %d %v", code, u)
	}
	stored, _ = q.GetUser(ctx, uid)
	if !auth.CheckPassword(stored.PasswordHash, "alice-pw1") {
		t.Error("empty password on update changed the hash")
	}
	if code, _, _ = call(t, h, http.MethodPut, path, map[string]any{"password": "alice-pw2"}); code != 200 {
		t.Fatalf("password change = %d", code)
	}
	stored, _ = q.GetUser(ctx, uid)
	if !auth.CheckPassword(stored.PasswordHash, "alice-pw2") || auth.CheckPassword(stored.PasswordHash, "alice-pw1") {
		t.Error("password not changed")
	}

	// A failing bouquet update rolls back the field change too.
	if code, _, _ = call(t, h, http.MethodPut, path, map[string]any{"max_connections": 9, "bouquet_ids": []int64{99999}}); code != http.StatusBadRequest {
		t.Errorf("bad bouquet update = %d, want 400", code)
	}
	if cur, _ := q.GetUser(ctx, uid); cur.MaxConnections != 3 {
		t.Errorf("partial update applied: max_connections = %d", cur.MaxConnections)
	}
	if ids, _ := q.ListUserBouquetIDs(ctx, uid); len(ids) != 1 || ids[0] != b2.ID {
		t.Errorf("bouquets after failed update = %v", ids)
	}

	// Token rotation.
	oldTok := stored.StreamToken
	code, u, _ = call(t, h, http.MethodPost, path+"/rotate-token", nil)
	if code != 200 || u["stream_token"] == oldTok {
		t.Errorf("rotate = %d, token unchanged=%v", code, u["stream_token"] == oldTok)
	}
	if _, err := q.GetUserByStreamToken(ctx, oldTok); !isNotFound(err) {
		t.Error("old token still resolves")
	}

	// Last-admin guard.
	code, adm, _ := call(t, h, http.MethodPost, "/api/v1/users", map[string]any{"username": "root", "password": "root-pw1", "role": "admin"})
	if code != http.StatusCreated {
		t.Fatalf("create admin = %d", code)
	}
	admPath := "/api/v1/users/" + strconv.FormatInt(int64(adm["id"].(float64)), 10)
	for name, req := range map[string][2]any{
		"delete":  {http.MethodDelete, nil},
		"disable": {http.MethodPut, map[string]any{"enabled": false}},
		"demote":  {http.MethodPut, map[string]any{"role": "customer"}},
	} {
		if code, _, _ := call(t, h, req[0].(string), admPath, req[1]); code != http.StatusConflict {
			t.Errorf("%s last admin = %d, want 409", name, code)
		}
	}
	// With a second admin, the first can be demoted.
	if code, _, _ := call(t, h, http.MethodPost, "/api/v1/users", map[string]any{"username": "root2", "password": "root-pw2", "role": "admin"}); code != http.StatusCreated {
		t.Fatal("second admin")
	}
	if code, _, _ := call(t, h, http.MethodPut, admPath, map[string]any{"role": "customer"}); code != 200 {
		t.Errorf("demote with another admin = %d", code)
	}

	// List and delete.
	code, _, list := call(t, h, http.MethodGet, "/api/v1/users", nil)
	if code != 200 || len(list) != 3 {
		t.Errorf("list = %d %d users", code, len(list))
	}
	for _, it := range list {
		if _, leaked := it.(map[string]any)["password_hash"]; leaked {
			t.Error("password_hash in list")
		}
	}
	if code, _, _ := call(t, h, http.MethodDelete, path, nil); code != http.StatusNoContent {
		t.Errorf("delete = %d", code)
	}
	if code, _, _ := call(t, h, http.MethodGet, path, nil); code != http.StatusNotFound {
		t.Errorf("get deleted = %d", code)
	}
}

// TestAdminAuth is the 5.2 AC: with auth on, every admin route answers 401
// without a valid admin credential; sessions and API tokens work.
func TestAdminAuth(t *testing.T) {
	env := newAPITestEnv(t)
	ctx := context.Background()
	q := env.Store.(*db.Queries)
	env.Auth = auth.NewAuthenticator(q, nil, time.Hour, false)

	// Bootstrap creates the first admin once.
	if err := auth.EnsureAdmin(ctx, q, slog.New(slog.DiscardHandler), "root", "root-pw1"); err != nil {
		t.Fatal(err)
	}
	if err := auth.EnsureAdmin(ctx, q, slog.New(slog.DiscardHandler), "other", "other-pw1"); err != nil {
		t.Fatal(err)
	}
	if n, _ := q.CountAdmins(ctx); n != 1 {
		t.Fatalf("admins after double bootstrap = %d", n)
	}
	hash, _ := auth.HashPassword("cust-pw1")
	if _, err := q.CreateUser(ctx, db.CreateUserParams{Username: "cust", PasswordHash: hash, Role: auth.RoleCustomer,
		MaxConnections: 1, Enabled: true, StreamToken: "tok-cust"}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(apiServer(env))
	t.Cleanup(srv.Close)
	do := func(c *http.Client, method, path, bearer string, body any) (*http.Response, map[string]any) {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = strings.NewReader(string(b))
		}
		req, _ := http.NewRequest(method, srv.URL+path, rd)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp, m
	}
	anon := &http.Client{}

	// No credentials: 401 everywhere, including the stream endpoint.
	for _, p := range []string{"/api/v1/providers", "/api/v1/channels", "/api/v1/users", "/api/v1/channels/1/stream", "/api/v1/auth/me", "/api/v1/tokens"} {
		if resp, _ := do(anon, http.MethodGet, p, "", nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s anonymous = %d, want 401", p, resp.StatusCode)
		}
	}
	if resp, _ := do(anon, http.MethodPost, "/api/v1/providers", "", map[string]any{"name": "x"}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous POST = %d", resp.StatusCode)
	}
	if resp, _ := do(anon, http.MethodGet, "/api/v1/providers", "not-a-token", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad bearer = %d", resp.StatusCode)
	}

	// Bad logins all look the same.
	for name, body := range map[string]map[string]any{
		"wrong password": {"username": "root", "password": "nope-nope"},
		"unknown user":   {"username": "ghost", "password": "root-pw1"},
		"customer":       {"username": "cust", "password": "cust-pw1"},
	} {
		resp, m := do(anon, http.MethodPost, "/api/v1/auth/login", "", body)
		if resp.StatusCode != http.StatusUnauthorized || m["error"] != "invalid credentials" {
			t.Errorf("%s login = %d %v", name, resp.StatusCode, m)
		}
	}

	// Good login: hardened cookie, then cookie-authenticated requests work.
	jar, _ := cookiejar.New(nil)
	admin := &http.Client{Jar: jar}
	resp, m := do(admin, http.MethodPost, "/api/v1/auth/login", "", map[string]any{"username": "root", "password": "root-pw1"})
	if resp.StatusCode != 200 || m["username"] != "root" {
		t.Fatalf("login = %d %v", resp.StatusCode, m)
	}
	if _, leaked := m["password_hash"]; leaked {
		t.Error("login leaked password_hash")
	}
	var sc *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == auth.SessionCookie {
			sc = c
		}
	}
	if sc == nil || !sc.HttpOnly || sc.SameSite != http.SameSiteStrictMode || len(sc.Value) != 43 {
		t.Fatalf("session cookie = %+v", sc)
	}
	if resp, m := do(admin, http.MethodGet, "/api/v1/auth/me", "", nil); resp.StatusCode != 200 || m["username"] != "root" {
		t.Errorf("me = %d %v", resp.StatusCode, m)
	}
	if resp, _ := do(admin, http.MethodGet, "/api/v1/providers", "", nil); resp.StatusCode != 200 {
		t.Errorf("providers with session = %d", resp.StatusCode)
	}

	// API token: shown once, works as Bearer, tracks last use, revocable.
	resp, tok := do(admin, http.MethodPost, "/api/v1/tokens", "", map[string]any{"name": "ci"})
	if resp.StatusCode != http.StatusCreated || len(tok["token"].(string)) != 43 {
		t.Fatalf("create token = %d %v", resp.StatusCode, tok)
	}
	secret := tok["token"].(string)
	if resp, _ := do(anon, http.MethodGet, "/api/v1/channels", secret, nil); resp.StatusCode != 200 {
		t.Errorf("bearer = %d", resp.StatusCode)
	}
	toks, _ := q.ListAPITokens(ctx)
	if len(toks) != 1 || toks[0].LastUsedAt == nil || toks[0].Username != "root" {
		t.Errorf("tokens = %+v", toks)
	}
	if resp, _ := do(admin, http.MethodGet, "/api/v1/tokens", "", nil); resp.StatusCode != 200 {
		t.Errorf("list tokens = %d", resp.StatusCode)
	}
	tokPath := "/api/v1/tokens/" + strconv.FormatInt(int64(tok["id"].(float64)), 10)
	if resp, _ := do(admin, http.MethodDelete, tokPath, "", nil); resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete token = %d", resp.StatusCode)
	}
	if resp, _ := do(anon, http.MethodGet, "/api/v1/channels", secret, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("revoked bearer = %d, want 401", resp.StatusCode)
	}

	// Disabling the admin cuts off its live session immediately.
	resp, second := do(admin, http.MethodPost, "/api/v1/users", "", map[string]any{"username": "root2", "password": "root-pw2", "role": "admin"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("second admin = %d", resp.StatusCode)
	}
	jar2, _ := cookiejar.New(nil)
	admin2 := &http.Client{Jar: jar2}
	if resp, _ := do(admin2, http.MethodPost, "/api/v1/auth/login", "", map[string]any{"username": "root2", "password": "root-pw2"}); resp.StatusCode != 200 {
		t.Fatal("root2 login")
	}
	root2 := "/api/v1/users/" + strconv.FormatInt(int64(second["id"].(float64)), 10)
	if resp, _ := do(admin, http.MethodPut, root2, "", map[string]any{"enabled": false}); resp.StatusCode != 200 {
		t.Fatal("disable root2")
	}
	if resp, _ := do(admin2, http.MethodGet, "/api/v1/auth/me", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("disabled admin session = %d, want 401", resp.StatusCode)
	}

	// Expired sessions are rejected.
	if err := q.CreateSession(ctx, db.CreateSessionParams{TokenHash: auth.HashToken("old-session"), UserID: 1,
		ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: "old-session"})
	if resp, err := anon.Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expired session = %v %v", resp.StatusCode, err)
	} else {
		resp.Body.Close()
	}
	if n, _ := q.DeleteExpiredSessions(ctx); n != 1 {
		t.Errorf("purged %d expired sessions, want 1", n)
	}

	// Logout invalidates the session server-side, not just the cookie.
	cookie := jar.Cookies(mustURL(t, srv.URL))[0]
	if resp, _ := do(admin, http.MethodPost, "/api/v1/auth/logout", "", nil); resp.StatusCode != http.StatusNoContent {
		t.Errorf("logout = %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/api/v1/auth/me", nil)
	req.AddCookie(cookie) // replay the old cookie value
	if resp, err := anon.Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("replayed logged-out cookie = %v %v", resp.StatusCode, err)
	} else {
		resp.Body.Close()
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
