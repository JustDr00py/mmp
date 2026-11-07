//go:build integration

package web

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/JustDr00py/mmp/internal/auth"
	"github.com/JustDr00py/mmp/internal/channel"
	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/testpg"
)

// pgUI is the UI over a fresh Postgres database with a signed-in admin.
func pgUI(t *testing.T, opts ...func(*Deps, *pgxpool.Pool)) (*browser, *db.Queries) {
	t.Helper()
	dsn := testpg.DSN("mmp_web_test")
	cfg, _ := pgx.ParseConfig(dsn)
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
			t.Fatal(err)
		}
	}
	mconn.Close(context.Background())
	if err := db.Migrate(context.Background(), dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Open(context.Background(), db.PoolConfig{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	q := db.New(pool)
	auth.BcryptCost = bcrypt.MinCost
	hash, _ := auth.HashPassword("boss-pass-1")
	if _, err := q.CreateUser(context.Background(), db.CreateUserParams{Username: "boss", PasswordHash: hash, Role: auth.RoleAdmin,
		Enabled: true, StreamToken: "tok-boss"}); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	deps := Deps{Auth: auth.NewAuthenticator(q, log, time.Hour, false), Store: q, Refresh: &fakeRefresher{store: q},
		Match: channel.NewService(log, pool), Log: log, Version: "test", BaseURL: "http://mmp.example:8080",
		InTx: func(ctx context.Context, fn func(Store) error) error {
			return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { return fn(db.New(tx)) })
		}}
	for _, o := range opts {
		o(&deps, pool)
	}
	ui, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	ui.Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	b := newBrowser(t, srv.URL)
	if resp, _ := b.post("/admin/login", url.Values{"username": {"boss"}, "password": {"boss-pass-1"}}, srv.URL); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login = %d", resp.StatusCode)
	}
	return b, q
}

// seedStreams creates providers A (priority 1) and B (priority 2) with
// streams; returns stream ids by name@provider.
func seedStreams(t *testing.T, q *db.Queries) map[string]int64 {
	t.Helper()
	ctx := context.Background()
	ids := map[string]int64{}
	for _, p := range []struct {
		name string
		prio int32
	}{{"A", 1}, {"B", 2}} {
		prov, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: p.name, Type: "m3u", Url: "http://" + p.name + "/", MaxConnections: 1,
			RefreshIntervalSecs: 3600, Priority: p.prio, Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []struct{ name, tvg string }{{"BBC One HD", "bbc1.uk"}, {"CNN", "cnn.us"}, {"Only " + p.name, ""}} {
			id, err := q.UpsertProviderStream(ctx, db.UpsertProviderStreamParams{ProviderID: prov.ID, ExternalID: s.name,
				Name: s.name, TvgID: s.tvg, StreamUrl: "http://" + p.name + "/live/u/pw/" + s.name})
			if err != nil {
				t.Fatal(err)
			}
			ids[s.name+"@"+p.name] = id
		}
	}
	return ids
}

func order(t *testing.T, q *db.Queries, channelID int64) []int64 {
	t.Helper()
	rows, err := q.ListChannelStreams(context.Background(), channelID)
	if err != nil {
		t.Fatal(err)
	}
	return currentOrder(rows)
}

func eq(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestChannelPages(t *testing.T) {
	b, q := pgUI(t)
	ids := seedStreams(t, q)
	ctx := context.Background()
	g, _ := q.CreateChannelGroup(ctx, db.CreateChannelGroupParams{Name: "News"})

	_, body := b.get("/admin/channels")
	if !strings.Contains(body, "No channels yet.") {
		t.Fatalf("empty list:\n%s", body)
	}
	_, body = b.get("/admin/channels/new")
	if !strings.Contains(body, `name="number" type="number" min="1" max="99999" value="101"`) || !strings.Contains(body, ">News</option>") {
		t.Errorf("new form lacks next number or groups:\n%s", body)
	}
	resp, _ := b.post("/admin/channels/new", url.Values{"name": {"BBC One"}, "number": {"101"}, "group_id": {strconv.FormatInt(g.ID, 10)},
		"tvg_id": {"bbc1.uk"}, "enabled": {"on"}}, b.base)
	loc := resp.Header.Get("Location")
	m := regexp.MustCompile(`^/admin/channels/(\d+)\?msg=created$`).FindStringSubmatch(loc)
	if resp.StatusCode != http.StatusSeeOther || m == nil {
		t.Fatalf("create = %d %q", resp.StatusCode, loc)
	}
	cid, _ := strconv.ParseInt(m[1], 10, 64)
	base := "/admin/channels/" + m[1]

	// Duplicate number: a form error, not a 500.
	resp, body = b.post("/admin/channels/new", url.Values{"name": {"Dup"}, "number": {"101"}}, b.base)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "already in use") || !strings.Contains(body, `value="Dup"`) {
		t.Errorf("duplicate number = %d", resp.StatusCode)
	}

	_, body = b.get(loc)
	if !strings.Contains(body, "Channel created.") || !strings.Contains(body, "No streams yet") {
		t.Errorf("channel page:\n%s", body)
	}

	// Picker: search across providers, add three streams.
	_, body = b.htmx(http.MethodGet, base+"/picker?q=bbc", nil, "picker")
	if strings.Count(body, "<li>") != 2 || !strings.Contains(body, "BBC One HD") || strings.Contains(body, "<html") {
		t.Fatalf("picker:\n%s", body)
	}
	for _, s := range []string{"BBC One HD@B", "BBC One HD@A", "Only A@A"} {
		resp, body = b.htmx(http.MethodPost, base+"/streams", url.Values{"stream": {strconv.FormatInt(ids[s], 10)}}, "stream-list")
		if resp.StatusCode != 200 || !strings.HasPrefix(strings.TrimSpace(body), `<div id="stream-list">`) ||
			!strings.Contains(body, `<span id="pick-`+strconv.FormatInt(ids[s], 10)+`" hx-swap-oob="true">`) {
			t.Fatalf("add %s = %d\n%s", s, resp.StatusCode, body)
		}
	}
	b.htmx(http.MethodPost, base+"/streams", url.Values{"stream": {strconv.FormatInt(ids["Only A@A"], 10)}}, "stream-list") // duplicate: no-op
	want := []int64{ids["BBC One HD@B"], ids["BBC One HD@A"], ids["Only A@A"]}
	if got := order(t, q, cid); !eq(got, want) {
		t.Fatalf("order after adds = %v, want %v", got, want)
	}
	_, body = b.htmx(http.MethodGet, base+"/picker?q=bbc", nil, "picker")
	if strings.Count(body, "in list") != 2 {
		t.Errorf("picker does not mark listed streams:\n%s", body)
	}

	// Drag-and-drop order (full permutation).
	perm := url.Values{"stream": {strconv.FormatInt(ids["Only A@A"], 10), strconv.FormatInt(ids["BBC One HD@A"], 10), strconv.FormatInt(ids["BBC One HD@B"], 10)}}
	if resp, _ := b.htmx(http.MethodPost, base+"/streams/order", perm, "stream-list"); resp.StatusCode != 200 {
		t.Fatalf("reorder = %d", resp.StatusCode)
	}
	want = []int64{ids["Only A@A"], ids["BBC One HD@A"], ids["BBC One HD@B"]}
	if got := order(t, q, cid); !eq(got, want) {
		t.Fatalf("order after drag = %v, want %v", got, want)
	}
	// A stale page (missing, foreign or duplicated ids) is refused.
	for _, bad := range []url.Values{
		{"stream": perm["stream"][:2]},
		{"stream": append(perm["stream"][:2:2], strconv.FormatInt(ids["CNN@A"], 10))},
		{"stream": {perm["stream"][0], perm["stream"][0], perm["stream"][1]}},
	} {
		if resp, _ := b.htmx(http.MethodPost, base+"/streams/order", bad, "stream-list"); resp.StatusCode != http.StatusConflict {
			t.Errorf("bad order %v = %d, want 409", bad["stream"], resp.StatusCode)
		}
	}
	if got := order(t, q, cid); !eq(got, want) {
		t.Fatalf("refused reorders changed the order: %v", got)
	}

	// Arrow buttons (no-JS path: plain form posts) and remove.
	if resp, _ := b.post(base+"/streams/"+strconv.FormatInt(ids["BBC One HD@B"], 10)+"/move?dir=up", url.Values{}, b.base); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("move = %d", resp.StatusCode)
	}
	want = []int64{ids["Only A@A"], ids["BBC One HD@B"], ids["BBC One HD@A"]}
	if got := order(t, q, cid); !eq(got, want) {
		t.Fatalf("order after move up = %v, want %v", got, want)
	}
	b.htmx(http.MethodPost, base+"/streams/"+strconv.FormatInt(ids["Only A@A"], 10)+"/move?dir=up", nil, "stream-list") // already first
	b.htmx(http.MethodPost, base+"/streams/"+strconv.FormatInt(ids["Only A@A"], 10)+"/remove", nil, "stream-list")
	want = []int64{ids["BBC One HD@B"], ids["BBC One HD@A"]}
	if got := order(t, q, cid); !eq(got, want) {
		t.Fatalf("order after remove = %v, want %v", got, want)
	}

	// Sort by provider priority (A=1 before B=2), from the channel page...
	if _, body := b.get(base); !strings.Contains(body, "Sort by provider priority") {
		t.Errorf("channel page lacks the sort button")
	}
	if resp, _ := b.htmx(http.MethodPost, base+"/streams/sort", nil, "stream-list"); resp.StatusCode != 200 {
		t.Fatalf("sort = %d", resp.StatusCode)
	}
	want = []int64{ids["BBC One HD@A"], ids["BBC One HD@B"]}
	if got := order(t, q, cid); !eq(got, want) {
		t.Fatalf("order after sort = %v, want %v", got, want)
	}
	// ...as a bulk action on the channel list...
	b.post(base+"/streams/"+strconv.FormatInt(ids["BBC One HD@B"], 10)+"/move?dir=up", url.Values{}, b.base)
	resp, _ = b.post("/admin/channels/sort-streams", url.Values{"channel": {strconv.FormatInt(cid, 10)}, "q": {"bbc"}}, b.base)
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || !strings.Contains(loc, "msg=sorted-n") || !strings.Contains(loc, "n=1") {
		t.Fatalf("bulk sort = %d %q", resp.StatusCode, loc)
	}
	if got := order(t, q, cid); !eq(got, want) {
		t.Fatalf("order after bulk sort = %v, want %v", got, want)
	}
	if _, body := b.get("/admin/channels?msg=sorted-n&n=1"); !strings.Contains(body, "Sorted the failover lists of 1 channel") {
		t.Errorf("bulk sort flash missing")
	}
	// ...and when a provider's priority changes, only with the checkbox.
	provs, _ := q.ListProviders(ctx)
	var provB int64
	for _, p := range provs {
		if p.Name == "B" {
			provB = p.ID
		}
	}
	bulkPrio := func(resort bool) {
		v := url.Values{"action": {"priority"}, "priority_to": {"0"}, "provider": {strconv.FormatInt(provB, 10)}}
		if resort {
			v.Set("resort_channels", "on")
		}
		if resp, _ := b.post("/admin/providers/bulk", v, b.base); resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("bulk priority = %d", resp.StatusCode)
		}
	}
	bulkPrio(false)
	if got := order(t, q, cid); !eq(got, want) {
		t.Fatalf("priority change without re-sort moved streams: %v", got)
	}
	bulkPrio(true)
	want = []int64{ids["BBC One HD@B"], ids["BBC One HD@A"]}
	if got := order(t, q, cid); !eq(got, want) {
		t.Fatalf("order after re-sort = %v, want %v", got, want)
	}

	// Settings form.
	resp, _ = b.post(base, url.Values{"name": {"BBC One (UK)"}, "number": {"105"}, "tvg_id": {"bbc1.uk"}}, b.base)
	if resp.Header.Get("Location") != base+"?msg=saved" {
		t.Fatalf("update = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	c, _ := q.GetChannel(ctx, cid)
	if c.Name != "BBC One (UK)" || *c.Number != 105 || c.Enabled || c.GroupID != nil {
		t.Errorf("after update = %+v", c)
	}
	if resp, body := b.post(base, url.Values{"name": {"X"}, "logo_url": {"javascript:alert(1)"}}, b.base); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Logo URL must be") {
		t.Errorf("bad logo = %d", resp.StatusCode)
	}

	// List: search and group filter (htmx fragment).
	_, body = b.htmx(http.MethodGet, "/admin/channels?q=bbc", nil, "channels")
	if !strings.HasPrefix(strings.TrimSpace(body), `<div id="channels">`) || !strings.Contains(body, "BBC One (UK)") || !strings.Contains(body, ">2</td>") {
		t.Errorf("search fragment:\n%s", body)
	}
	if _, body := b.htmx(http.MethodGet, "/admin/channels?group="+strconv.FormatInt(g.ID, 10), nil, "channels"); !strings.Contains(body, "No channels match.") {
		t.Errorf("group filter (channel left the group):\n%s", body)
	}

	if resp, _ := b.htmx(http.MethodPost, base+"/delete", nil, ""); resp.Header.Get("HX-Redirect") != "/admin/channels?msg=deleted" {
		t.Errorf("delete = %d", resp.StatusCode)
	}
	if resp, _ := b.get(base); resp.StatusCode != http.StatusNotFound {
		t.Errorf("deleted channel = %d", resp.StatusCode)
	}
}

func TestMatchPage(t *testing.T) {
	b, q := pgUI(t)
	ids := seedStreams(t, q)

	_, body := b.get("/admin/match")
	// BBC One and CNN are at both providers; "Only A"/"Only B" at one.
	if !strings.Contains(body, "BBC One") || !strings.Contains(body, "CNN") || strings.Contains(body, ">Only A<") {
		t.Fatalf("proposals (min 2):\n%s", body)
	}
	if !strings.Contains(body, "2 more with “any source”") {
		t.Errorf("hidden count missing")
	}
	keys := regexp.MustCompile(`name="key" value="([^"]+)" aria-label="Apply ([^"]+)"`).FindAllStringSubmatch(body, -1)
	var bbc string
	for _, k := range keys {
		if strings.HasPrefix(k[2], "BBC One") {
			bbc = k[1]
		}
	}
	if bbc == "" {
		t.Fatalf("no BBC key in %v", keys)
	}
	resp, _ := b.post("/admin/match/apply", url.Values{"key": {bbc}, "min": {"2"}}, b.base)
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(loc, "/admin/match?applied=1-2") {
		t.Fatalf("apply = %d %q", resp.StatusCode, loc)
	}
	_, body = b.get(resp.Header.Get("Location"))
	if !strings.Contains(body, "Done: 1 channel created, 2 streams added.") || strings.Contains(body, `aria-label="Apply BBC One`) {
		t.Errorf("after apply:\n%s", body)
	}
	chans, _ := q.ListChannels(context.Background(), db.ListChannelsParams{Limit: 10})
	if len(chans) != 1 {
		t.Fatalf("channels = %+v", chans)
	}
	if got := order(t, q, chans[0].ID); !eq(got, []int64{ids["BBC One HD@A"], ids["BBC One HD@B"]}) {
		t.Errorf("failover order = %v (want provider priority A, B)", got)
	}

	// Rules: invalid regex is reported in the form; a valid one is listed.
	resp, body = b.post("/admin/match/rules", url.Values{"pattern": {"(unclosed"}, "channel_name": {"X"}}, b.base)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Invalid pattern") || !strings.Contains(body, `value="(unclosed"`) {
		t.Errorf("bad rule = %d", resp.StatusCode)
	}
	b.post("/admin/match/rules", url.Values{"pattern": {"(?i)^only "}, "channel_name": {"Only Channel"}}, b.base)
	_, body = b.get("/admin/match?min=1")
	if !strings.Contains(body, "(?i)^only ") || !strings.Contains(body, "Only Channel") {
		t.Errorf("rule listed=%v grouped=%v", strings.Contains(body, "(?i)^only "), strings.Contains(body, "Only Channel"))
	}
	rules, _ := q.ListMatchRules(context.Background())
	if len(rules) != 1 {
		t.Fatalf("rules = %v", rules)
	}
	b.post("/admin/match/rules/"+strconv.FormatInt(rules[0].ID, 10)+"/delete", url.Values{}, b.base)
	if rules, _ := q.ListMatchRules(context.Background()); len(rules) != 0 {
		t.Error("rule not deleted")
	}
}

// TestChannelPageGameOrder: while game routing is on, the failover list
// shows what plays: the slot's streams first, then the channel's own, a
// stream both have only once (in the slot's place).
func TestChannelPageGameOrder(t *testing.T) {
	b, q := pgUI(t)
	ids := seedStreams(t, q)
	ctx := context.Background()
	channel := func(name string, streams ...int64) int64 {
		c, err := q.CreateChannel(ctx, db.CreateChannelParams{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		for i, s := range streams {
			if err := q.AddStreamToChannel(ctx, db.AddStreamToChannelParams{ChannelID: c.ID, ProviderStreamID: s,
				Priority: int32(i + 1)}); err != nil {
				t.Fatal(err)
			}
		}
		return c.ID
	}
	slot := channel("NBA 06", ids["BBC One HD@A"], ids["CNN@A"])
	team := channel("Toronto Raptors", ids["Only B@B"], ids["CNN@A"], ids["CNN@B"])
	page := "/admin/channels/" + strconv.FormatInt(team, 10)
	rank := regexp.MustCompile(`<span class="rank">([^<]*)</span>\s*<span class="what">\s*<span class="what-name"><strong>([^<]*)</strong>`)
	ranks := func(body string) string {
		var out []string
		for _, m := range rank.FindAllStringSubmatch(body, -1) {
			out = append(out, m[1]+" "+m[2])
		}
		return strings.Join(out, ", ")
	}

	_, body := b.get(page)
	if got, want := ranks(body), "1 Only B, 2 CNN, 3 CNN"; got != want || strings.Contains(body, "Game on") {
		t.Errorf("no game: ranks %q, want %q", got, want)
	}

	now := time.Now()
	if err := q.UpsertEventOverride(ctx, db.UpsertEventOverrideParams{ChannelID: team, SourceChannelID: slot,
		Title: "Milwaukee Bucks vs Toronto Raptors", StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	_, body = b.get(page)
	if got, want := ranks(body), "1 BBC One HD, 2 CNN, 3 Only B, – CNN, 4 CNN"; got != want {
		t.Errorf("game on: ranks %q, want %q", got, want)
	}
	if !strings.Contains(body, "NBA 06</a>'s 2 streams in order, then this channel's own") || !strings.Contains(body, ">in NBA 06</span>") {
		t.Errorf("game on: banner or slot pill missing:\n%s", body)
	}
	// An htmx edit re-renders the fragment in the same order.
	_, body = b.htmx(http.MethodPost, page+"/streams/"+strconv.FormatInt(ids["CNN@B"], 10)+"/move?dir=up", nil, "stream-list")
	if got, want := ranks(body), "1 BBC One HD, 2 CNN, 3 Only B, 4 CNN, – CNN"; got != want {
		t.Errorf("after move: ranks %q, want %q", got, want)
	}
}
