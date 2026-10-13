//go:build integration

package source

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sysadmin/mmp/internal/channel"
	"github.com/sysadmin/mmp/internal/db"
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
	ref := NewRefresher(log, w.pool)
	ref.AfterRefresh = channel.NewService(log, w.pool).AutoMatchHook(2)

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
	if _, err := ref.RefreshProvider(ctx, pA); err != nil {
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
	if _, err := ref.RefreshProvider(ctx, pB); err != nil {
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
	if _, err := ref.RefreshProvider(ctx, pA); err != nil {
		t.Fatal(err)
	}
	if got := channelStreams(abc.ID); len(got) != 1 {
		t.Errorf("ABC streams after re-refresh = %v", got)
	}
}
