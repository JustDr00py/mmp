//go:build integration

package web

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JustDr00py/mmp/internal/channel"
	"github.com/JustDr00py/mmp/internal/db"
)

func TestCleanupSlotNames(t *testing.T) {
	var svc *channel.Service
	_, q := pgUI(t, func(d *Deps, _ *pgxpool.Pool) { svc = d.Match.(*channel.Service) })
	ctx := context.Background()
	prov, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: "P", Type: "m3u", Url: "http://p/", MaxConnections: 1,
		RefreshIntervalSecs: 3600, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	for _, s := range []struct{ name, tvg string }{
		{"US| ESPN 2 HD", "espn2.us"}, {"US ★ ESPN+ 2:", ""}, {"US - AMC+ HD", "amc.us"}, // same tvg-id: kept
		{"UFC: Press Conference @ Oct 22 9:00 AM :Paramount+  01", ""},
	} {
		id, err := q.UpsertProviderStream(ctx, db.UpsertProviderStreamParams{ProviderID: prov.ID, ExternalID: s.name, Name: s.name,
			TvgID: s.tvg, StreamUrl: "http://p/" + s.name})
		if err != nil {
			t.Fatal(err)
		}
		ids[s.name] = id
	}
	link := func(ch db.Channel, names ...string) {
		for i, n := range names {
			if err := q.AddStreamToChannel(ctx, db.AddStreamToChannelParams{ChannelID: ch.ID, ProviderStreamID: ids[n],
				Priority: int32(i + 1)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	espn2, _ := q.CreateChannel(ctx, db.CreateChannelParams{Name: "ESPN 2", TvgID: "espn2.us"})
	link(espn2, "US| ESPN 2 HD", "US ★ ESPN+ 2:")
	amc, _ := q.CreateChannel(ctx, db.CreateChannelParams{Name: "AMC", TvgID: "amc.us"})
	link(amc, "US - AMC+ HD")
	stale, _ := q.CreateChannel(ctx, db.CreateChannelParams{Name: "Serie A: Venezia vs Napoli @ Oct 17 8:50 AM :Paramount+ 01"})
	link(stale, "UFC: Press Conference @ Oct 22 9:00 AM :Paramount+  01")

	res, err := svc.CleanupSlotNames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ran || res.Unlinked != 1 || len(res.Renamed) != 1 {
		t.Fatalf("cleanup = %+v, want ESPN+ 2 unlinked and one rename", res)
	}
	streams := func(ch db.Channel) []string {
		rows, _ := q.ListChannelStreams(ctx, ch.ID)
		var out []string
		for _, r := range rows {
			out = append(out, r.StreamName)
		}
		return out
	}
	if got := streams(espn2); !slices.Equal(got, []string{"US| ESPN 2 HD"}) {
		t.Errorf("ESPN 2 streams = %v", got)
	}
	if got := streams(amc); len(got) != 1 {
		t.Errorf("AMC streams = %v, want the tvg-id link kept", got)
	}
	if c, _ := q.GetChannel(ctx, stale.ID); c.Name != "Paramount+ 01" {
		t.Errorf("stale channel name = %q", c.Name)
	}

	// It runs once: a stream the admin puts back stays.
	link(espn2, "US ★ ESPN+ 2:")
	if res, err := svc.CleanupSlotNames(ctx); err != nil || res.Ran {
		t.Errorf("second run = %+v, %v", res, err)
	}
	if got := streams(espn2); len(got) != 2 {
		t.Errorf("ESPN 2 streams after second run = %v", got)
	}
}
