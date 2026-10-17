//go:build integration

package source

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/JustDr00py/mmp/internal/channel"
	"github.com/JustDr00py/mmp/internal/db"
)

// TestRefreshAutoMatch is the 3.5 AC: with auto-match on, refreshing a
// provider attaches its streams to matching existing channels, and creates
// new channels only for groups seen at min_providers providers.
func TestRefreshAutoMatch(t *testing.T) {
	w, q := newTestDB(t)
	tsA, _ := newM3UServer(t) // ABC HD (us.abc), NBC (us.nbc), News 24
	tsB := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(rw, "#EXTM3U\n"+
			"#EXTINF:-1 tvg-id=\"US.NBC\",NBC FHD\nhttp://b.tv/nbc.ts\n"+
			"#EXTINF:-1,Cartoons\nhttp://b.tv/toons.ts\n")
	}))
	t.Cleanup(tsB.Close)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	abc, err := q.CreateChannel(ctx, db.CreateChannelParams{Name: "ABC", TvgID: "us.abc"})
	if err != nil {
		t.Fatal(err)
	}
	ref := NewRefresher(log, w.pool, 80)
	ref.AfterRefresh = channel.NewService(log, w.pool).AutoMatchHook(channel.AutoMatch{MinProviders: 2, NewChannels: true})

	mk := func(name, url, ua string, prio int32) db.Provider {
		p, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: name, Type: "m3u", Url: url, UserAgent: ua,
			MaxConnections: 1, RefreshIntervalSecs: 3600, Priority: prio, Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	pA := mk("A", tsA.URL, "MMP-Test/1.0", 0)
	pB := mk("B", tsB.URL, "", 1)

	channelStreams := func(id int64) []string {
		t.Helper()
		cs, err := q.ListChannelStreams(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, len(cs))
		for i, s := range cs {
			out[i] = s.StreamUrl
		}
		return out
	}
	channelNames := func() []string {
		t.Helper()
		chans, err := q.ListChannels(ctx, db.ListChannelsParams{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, len(chans))
		for i, c := range chans {
			out[i] = c.Name
		}
		return out
	}

	// A alone: ABC joins the existing channel; single-provider groups wait.
	if _, err := ref.RefreshProvider(ctx, pA, false); err != nil {
		t.Fatal(err)
	}
	if got := channelStreams(abc.ID); len(got) != 1 || got[0] != "http://fake.tv/abc.ts" {
		t.Errorf("ABC streams after A = %v", got)
	}
	if got := channelNames(); len(got) != 1 {
		t.Errorf("channels after A = %v, want only ABC", got)
	}

	// B brings a second NBC: that group now spans 2 providers and is created
	// with A (priority 0) first. Cartoons stays unassigned.
	if _, err := ref.RefreshProvider(ctx, pB, false); err != nil {
		t.Fatal(err)
	}
	chans, _ := q.ListChannels(ctx, db.ListChannelsParams{Limit: 100})
	if len(chans) != 2 {
		t.Fatalf("channels after B = %v, want ABC + NBC", channelNames())
	}
	var nbc db.Channel
	for _, c := range chans {
		if c.ID != abc.ID {
			nbc = c
		}
	}
	if got := channelStreams(nbc.ID); len(got) != 2 || got[0] != "http://fake.tv/nbc.ts" || got[1] != "http://b.tv/nbc.ts" {
		t.Errorf("NBC (%q) streams = %v", nbc.Name, got)
	}

	// Re-refreshing is a no-op for channel membership.
	if _, err := ref.RefreshProvider(ctx, pA, false); err != nil {
		t.Fatal(err)
	}
	if got := channelStreams(abc.ID); len(got) != 1 {
		t.Errorf("ABC streams after re-refresh = %v", got)
	}
}

// TestRefreshAutoMatchAttachOnly: with auto_new_channels off, a refresh only
// adds streams to channels that exist, and placeholder entries named like
// a channel ("##### ABC #####") are never attached.
func TestRefreshAutoMatchAttachOnly(t *testing.T) {
	w, q := newTestDB(t)
	tsA, _ := newM3UServer(t) // ABC HD (us.abc), NBC (us.nbc), News 24
	tsB := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(rw, "#EXTM3U\n"+
			"#EXTINF:-1 group-title=\"US\",##### ABC #####\nhttp://b.tv/sep.ts\n"+
			"#EXTINF:-1 tvg-id=\"US.NBC\",NBC FHD\nhttp://b.tv/nbc.ts\n"+
			"#EXTINF:-1,ABC FHD\nhttp://b.tv/abc.ts\n")
	}))
	t.Cleanup(tsB.Close)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	abc, err := q.CreateChannel(ctx, db.CreateChannelParams{Name: "ABC", TvgID: "us.abc"})
	if err != nil {
		t.Fatal(err)
	}
	ref := NewRefresher(log, w.pool, 80)
	ref.AfterRefresh = channel.NewService(log, w.pool).AutoMatchHook(channel.AutoMatch{MinProviders: 2})
	for i, url := range []string{tsA.URL, tsB.URL} {
		p, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: string(rune('A' + i)), Type: "m3u", Url: url, UserAgent: "MMP-Test/1.0",
			MaxConnections: 1, RefreshIntervalSecs: 3600, Priority: int32(i), Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ref.RefreshProvider(ctx, p, false); err != nil {
			t.Fatal(err)
		}
	}
	if chans, _ := q.ListChannels(ctx, db.ListChannelsParams{Limit: 100}); len(chans) != 1 {
		t.Errorf("channels = %d, want only ABC (NBC at 2 providers must not be created)", len(chans))
	}
	cs, _ := q.ListChannelStreams(ctx, abc.ID)
	var urls []string
	for _, s := range cs {
		urls = append(urls, s.StreamUrl)
	}
	if len(urls) != 2 || urls[0] != "http://fake.tv/abc.ts" || urls[1] != "http://b.tv/abc.ts" {
		t.Errorf("ABC streams = %v, want A's ABC HD and B's ABC FHD, not the placeholder", urls)
	}
}

// TestRefreshAutoMatchGroupsAndBouquets: channels auto-match creates go in
// the configured group and bouquets; an existing channel that only gains a
// stream keeps its own group and bouquets.
func TestRefreshAutoMatchGroupsAndBouquets(t *testing.T) {
	m3u := func(body string) string {
		ts := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(rw, "#EXTM3U\n"+body)
		}))
		t.Cleanup(ts.Close)
		return ts.URL
	}
	urlA := m3u("#EXTINF:-1 tvg-id=\"us.abc\" group-title=\"US\",ABC HD\nhttp://a.tv/abc.ts\n" +
		"#EXTINF:-1 tvg-id=\"us.nbc\" group-title=\"US| NEWS\",NBC\nhttp://a.tv/nbc.ts\n" +
		"#EXTINF:-1 tvg-id=\"us.cbs\" group-title=\"US| NEWS\",CBS\nhttp://a.tv/cbs.ts\n")
	urlB := m3u("#EXTINF:-1 tvg-id=\"us.abc\" group-title=\"Network\",ABC FHD\nhttp://b.tv/abc.ts\n" +
		"#EXTINF:-1 tvg-id=\"us.nbc\" group-title=\"USA NEWS\",NBC FHD\nhttp://b.tv/nbc.ts\n" +
		"#EXTINF:-1 tvg-id=\"us.cbs\" group-title=\"USA NEWS\",CBS\nhttp://b.tv/cbs.ts\n")

	for _, tt := range []struct {
		group     string
		wantGroup string // of the created NBC and CBS; "" = none
	}{
		{"source", "US| NEWS"}, // the first (priority 0) provider's group
		{"Auto added", "Auto added"},
		{"none", ""},
	} {
		t.Run(tt.group, func(t *testing.T) {
			w, q := newTestDB(t)
			ctx := context.Background()
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			mine, _ := q.CreateChannelGroup(ctx, db.CreateChannelGroupParams{Name: "My picks"})
			abc, err := q.CreateChannel(ctx, db.CreateChannelParams{Name: "ABC", TvgID: "us.abc", GroupID: &mine.ID})
			if err != nil {
				t.Fatal(err)
			}
			premium, _ := q.CreateBouquet(ctx, db.CreateBouquetParams{Name: "Premium"})
			basic, _ := q.CreateBouquet(ctx, db.CreateBouquetParams{Name: "Basic"})
			q.AddChannelsToBouquet(ctx, db.AddChannelsToBouquetParams{BouquetID: premium.ID, ChannelIds: []int64{abc.ID}})

			ref := NewRefresher(log, w.pool, 80)
			ref.AfterRefresh = channel.NewService(log, w.pool).AutoMatchHook(channel.AutoMatch{
				MinProviders: 2, NewChannels: true, Group: tt.group, Bouquets: []string{"basic", "No such bouquet"}})
			for i, url := range []string{urlA, urlB} {
				p, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: string(rune('A' + i)), Type: "m3u", Url: url,
					MaxConnections: 1, RefreshIntervalSecs: 3600, Priority: int32(i), Enabled: true})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := ref.RefreshProvider(ctx, p, false); err != nil {
					t.Fatal(err)
				}
			}

			groupName := func(c db.Channel) string {
				if c.GroupID == nil {
					return ""
				}
				g, _ := q.GetChannelGroup(ctx, *c.GroupID)
				return g.Name
			}
			chans, _ := q.ListChannels(ctx, db.ListChannelsParams{Limit: 100})
			created := map[string]bool{}
			for _, c := range chans {
				if c.ID == abc.ID {
					continue
				}
				created[c.Name] = true
				if got := groupName(c); got != tt.wantGroup {
					t.Errorf("created %q in group %q, want %q", c.Name, got, tt.wantGroup)
				}
			}
			if len(created) != 2 || !created["NBC"] || !created["CBS"] {
				t.Errorf("created channels = %v, want NBC and CBS", created)
			}

			// ABC only gained B's stream: same group, same bouquets.
			abcNow, _ := q.GetChannel(ctx, abc.ID)
			if cs, _ := q.ListChannelStreams(ctx, abc.ID); len(cs) != 2 {
				t.Errorf("ABC streams = %d, want 2", len(cs))
			}
			if got := groupName(abcNow); got != "My picks" {
				t.Errorf("ABC moved to group %q", got)
			}
			members := func(b int64) map[string]bool {
				out := map[string]bool{}
				cs, _ := q.ListBouquetChannels(ctx, b)
				for _, c := range cs {
					out[c.Name] = true
				}
				return out
			}
			if got := members(basic.ID); len(got) != 2 || !got["NBC"] || !got["CBS"] {
				t.Errorf("Basic = %v, want only the created NBC and CBS", got)
			}
			if got := members(premium.ID); len(got) != 1 || !got["ABC"] {
				t.Errorf("Premium = %v, want ABC alone", got)
			}
		})
	}
}
