//go:build integration

package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/JustDr00py/mmp/internal/auth"
	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/output"
	"github.com/JustDr00py/mmp/internal/proxy"
)

// TestXtreamPlaybackEndToEnd is the 5.7 AC through the real stack: a
// customer logs in to player_api.php, lists live streams, builds the stream
// URL from server_info the way players do, and plays it through the proxy
// from a Postgres-backed channel. The playlist URLs play too, the
// connection allowance holds, and channels outside the bouquets stay hidden.
func TestXtreamPlaybackEndToEnd(t *testing.T) {
	env := newAPITestEnv(t)
	ctx := context.Background()
	q := env.Store.(*db.Queries)
	up, hits, _ := fakeTSUpstream(t)

	mgr := proxy.NewManager(ctx, proxy.ManagerConfig{
		Source: proxy.DBSource(q), Opener: proxy.NewHTTPOpener(2 * time.Second),
		Capacity: proxy.NewCapacity(), Cooldowns: proxy.NewCooldowns(time.Second, 10*time.Second),
		RingSize: 4 << 20, IdleGrace: 200 * time.Millisecond, MaxRetryWindow: 5 * time.Second,
		Thresholds: proxy.Thresholds{StallTimeout: 2 * time.Second, Window: 5 * time.Second},
	})
	t.Cleanup(mgr.Shutdown)
	outputs := &output.Handlers{Access: output.NewAccess(auth.NewCustomers(q, time.Minute)),
		Store: q, Streams: mgr, Log: env.Log}
	r := chi.NewRouter()
	outputs.Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	outputs.BaseURL = srv.URL

	// Provider stream -> channel "News" in bouquet "Basic"; "Premium" is not.
	p, _ := q.CreateProvider(ctx, db.CreateProviderParams{Name: "P", Type: "m3u", Url: "http://p/",
		MaxConnections: 1, RefreshIntervalSecs: 3600, Enabled: true})
	sid, _ := q.UpsertProviderStream(ctx, db.UpsertProviderStreamParams{ProviderID: p.ID, ExternalID: "1",
		Name: "News", StreamUrl: up.URL + "/live/user/secret/1.ts"})
	news, _ := q.CreateChannel(ctx, db.CreateChannelParams{Name: "News", TvgID: "news.us"})
	premium, _ := q.CreateChannel(ctx, db.CreateChannelParams{Name: "Premium"})
	for _, c := range []int64{news.ID, premium.ID} {
		if err := q.AddStreamToChannel(ctx, db.AddStreamToChannelParams{ChannelID: c, ProviderStreamID: sid, Priority: 1}); err != nil {
			t.Fatal(err)
		}
	}
	basic, _ := q.CreateBouquet(ctx, db.CreateBouquetParams{Name: "Basic"})
	if err := q.AddChannelToBouquet(ctx, db.AddChannelToBouquetParams{BouquetID: basic.ID, ChannelID: news.ID}); err != nil {
		t.Fatal(err)
	}
	hash, _ := auth.HashPassword("viewer-pw1")
	tok, _ := auth.NewStreamToken()
	u, err := q.CreateUser(ctx, db.CreateUserParams{Username: "viewer", PasswordHash: hash, Role: auth.RoleCustomer,
		MaxConnections: 1, Enabled: true, StreamToken: tok})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.AddUserBouquets(ctx, db.AddUserBouquetsParams{UserID: u.ID, BouquetIds: []int64{basic.ID}}); err != nil {
		t.Fatal(err)
	}

	getJSON := func(path string, out any) {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	const api = "/player_api.php?username=viewer&password=viewer-pw1"
	var login struct {
		UserInfo struct {
			Auth int `json:"auth"`
		} `json:"user_info"`
		ServerInfo struct {
			URL      string `json:"url"`
			Port     string `json:"port"`
			Protocol string `json:"server_protocol"`
		} `json:"server_info"`
	}
	getJSON(api, &login)
	if login.UserInfo.Auth != 1 {
		t.Fatalf("login = %+v", login)
	}
	var streams []struct {
		StreamID int64 `json:"stream_id"`
	}
	getJSON(api+"&action=get_live_streams", &streams)
	if len(streams) != 1 || streams[0].StreamID != news.ID {
		t.Fatalf("streams = %+v, want only News", streams)
	}

	// The URL a player builds from server_info + stream_id.
	si := login.ServerInfo
	playerURL := fmt.Sprintf("%s://%s:%s/live/viewer/viewer-pw1/%d.ts", si.Protocol, si.URL, si.Port, streams[0].StreamID)

	open := func(url string) (*http.Response, context.CancelFunc) {
		t.Helper()
		cctx, cancel := context.WithCancel(ctx)
		req, _ := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		return resp, func() { cancel(); resp.Body.Close() }
	}
	resp, stop := open(playerURL)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "video/mp2t" {
		t.Fatalf("play %s = %d", playerURL, resp.StatusCode)
	}
	buf := make([]byte, 188*500)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatal(err)
	}
	for off := 0; off < len(buf); off += 188 {
		if buf[off] != 0x47 {
			t.Fatalf("unaligned TS at %d", off)
		}
	}

	// Allowance is 1: a second stream (token form) is refused meanwhile.
	tokURL := fmt.Sprintf("%s/t/%s/%d.ts", srv.URL, tok, news.ID)
	r2, stop2 := open(tokURL)
	if r2.StatusCode != http.StatusForbidden {
		t.Errorf("second connection = %d, want 403", r2.StatusCode)
	}
	stop2()
	stop()

	// The token playlist's stream URL plays once the first viewer is gone.
	pl, stopPL := open(srv.URL + "/playlist/" + tok + ".m3u")
	var urls []string
	sc := bufio.NewScanner(pl.Body)
	for sc.Scan() {
		if l := sc.Text(); l != "" && !strings.HasPrefix(l, "#") {
			urls = append(urls, l)
		}
	}
	stopPL()
	if len(urls) != 1 || urls[0] != tokURL {
		t.Fatalf("playlist urls = %v, want [%s]", urls, tokURL)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		r3, stop3 := open(urls[0])
		code := r3.StatusCode
		stop3()
		if code == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("playlist url = %d", code)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// A channel outside the bouquets is not playable even by direct URL.
	r4, stop4 := open(fmt.Sprintf("%s/live/viewer/viewer-pw1/%d.ts", srv.URL, premium.ID))
	stop4()
	if r4.StatusCode != http.StatusNotFound {
		t.Errorf("premium = %d, want 404", r4.StatusCode)
	}
	if h := hits.Load(); h < 1 {
		t.Errorf("upstream never requested")
	}
}
