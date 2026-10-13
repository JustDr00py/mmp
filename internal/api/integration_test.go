//go:build integration

package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/sysadmin/mmp/internal/channel"
	"github.com/sysadmin/mmp/internal/db"
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
	log := slog.New(slog.NewTextHandler(nilDiscard{}, nil))
	return &Env{Log: log, Store: db.New(pool), Match: channel.NewService(log, pool)}
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
