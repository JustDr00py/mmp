//go:build integration

package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JustDr00py/mmp/internal/channel"
	"github.com/JustDr00py/mmp/internal/db"
)

func TestProviderGroups(t *testing.T) {
	b, q := pgUI(t)
	ctx := context.Background()
	prov, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: "Big", Type: "m3u", Url: "http://big/", MaxConnections: 1,
		RefreshIntervalSecs: 3600, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	for _, s := range []struct{ name, group string }{
		{"US Fox News", "US News"}, {"US CNN", "US News"}, {"UK Sky News", "UK News"}, {"AR Jazeera News", "Arabic"}, {"Loose News", ""},
	} {
		id, err := q.UpsertProviderStream(ctx, db.UpsertProviderStreamParams{ProviderID: prov.ID, ExternalID: s.name, Name: s.name,
			GroupTitle: s.group, StreamUrl: "http://big/" + s.name})
		if err != nil {
			t.Fatal(err)
		}
		ids[s.name] = id
	}
	// A channel already using a UK stream keeps it whatever is selected.
	ch, _ := q.CreateChannel(ctx, db.CreateChannelParams{Name: "Sky News"})
	if err := q.AddStreamToChannel(ctx, db.AddStreamToChannelParams{ChannelID: ch.ID, ProviderStreamID: ids["UK Sky News"], Priority: 1}); err != nil {
		t.Fatal(err)
	}
	pid := strconv.FormatInt(prov.ID, 10)
	page := "/admin/providers/" + pid + "/groups"

	_, body := b.get(page)
	for _, want := range []string{`value="US News"`, `US News <span class="sub">2 streams`, `value="Arabic"`, `<em>(no group)</em>`, `value="all" checked`} {
		if !strings.Contains(body, want) {
			t.Errorf("groups page lacks %q", want)
		}
	}
	if _, body = b.get("/admin/providers"); !strings.Contains(body, "all 4 groups") {
		t.Errorf("providers row lacks the group summary")
	}
	shown := func(path string) string {
		_, body := b.get(path)
		return body
	}
	if body := shown("/admin/match?min=1"); !strings.Contains(body, "AR Jazeera News") {
		t.Fatalf("before filtering, match lacks Arabic:\n%s", body)
	}

	// Only US News.
	if resp, _ := b.post(page, url.Values{"mode": {"selected"}, "group": {"US News"}}, b.base); resp.StatusCode != http.StatusSeeOther ||
		resp.Header.Get("Location") != page+"?msg=saved" {
		t.Fatalf("save = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if p, _ := q.GetProvider(ctx, prov.ID); !p.GroupFilter {
		t.Error("group_filter not on")
	}
	body = shown(page + "?msg=saved")
	if !strings.Contains(body, `value="selected" id="mode-selected" checked`) || !strings.Contains(body, `value="US News" checked`) ||
		strings.Contains(body, `value="Arabic" checked`) || !strings.Contains(body, "Groups saved.") {
		t.Errorf("page after save:\n%s", body)
	}
	if body := shown("/admin/providers"); !strings.Contains(body, "1 of 4 groups") {
		t.Error("providers row lacks 1 of 4 groups")
	}
	body = shown("/admin/match?min=1")
	if !strings.Contains(body, "US Fox News") || strings.Contains(body, "AR Jazeera") || strings.Contains(body, "Loose News") {
		t.Errorf("match with filter:\n%s", body)
	}
	_, body = b.htmx(http.MethodGet, "/admin/channels/"+strconv.FormatInt(ch.ID, 10)+"/picker?q=news", nil, "picker")
	if !strings.Contains(body, "US Fox News") || strings.Contains(body, "UK Sky") || strings.Contains(body, "AR Jazeera") {
		t.Errorf("picker with filter:\n%s", body[:min(len(body), 3000)])
	}
	if rows, _ := q.ListChannelStreams(ctx, ch.ID); len(rows) != 1 {
		t.Errorf("channel lost its stream: %v", rows)
	}

	// The ungrouped streams can be selected too.
	b.post(page, url.Values{"mode": {"selected"}, "group": {"US News", ""}}, b.base)
	if body := shown("/admin/match?min=1"); !strings.Contains(body, "Loose News") {
		t.Error("(no group) not selectable")
	}

	// Back to all groups: everything is offered again, the ticks are kept.
	b.post(page, url.Values{"mode": {"all"}}, b.base)
	if body := shown("/admin/match?min=1"); !strings.Contains(body, "AR Jazeera News") {
		t.Error("all groups mode still filters")
	}
	if body := shown(page); !strings.Contains(body, `value="US News" checked`) || !strings.Contains(body, `value="all" checked`) {
		t.Error("selection not kept in all-groups mode")
	}
	if resp, _ := b.get("/admin/providers/999999/groups"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown provider = %d", resp.StatusCode)
	}
}

// TestNewProviderWaitsForGroups: a provider added in the UI uses none of its
// groups until they are chosen, so its first refresh matches nothing; a
// plain save of the groups matches nothing either, "Save and match" does.
func TestNewProviderWaitsForGroups(t *testing.T) {
	var svc *channel.Service
	b, q := pgUI(t, func(d *Deps, _ *pgxpool.Pool) { svc = d.Match.(*channel.Service) })
	ctx := context.Background()
	if err := svc.SeedMatchSettings(ctx, channel.AutoMatch{Enabled: true, MinProviders: 1, NewChannels: true}); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"name": {"Fresh"}, "type": {"m3u"}, "url": {"http://fresh/"}, "max_connections": {"1"},
		"refresh_minutes": {"60"}, "priority": {"0"}, "enabled": {"on"}}
	if resp, body := b.post("/admin/providers/new", form, b.base); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create = %d\n%s", resp.StatusCode, body)
	}
	ps, _ := q.ListProviders(ctx)
	if len(ps) != 1 || !ps[0].GroupFilter {
		t.Fatalf("new provider = %+v, want group_filter on", ps)
	}
	prov := ps[0]
	for _, s := range []struct{ name, group string }{{"US Fox News", "US News"}, {"AR Jazeera", "Arabic"}} {
		if _, err := q.UpsertProviderStream(ctx, db.UpsertProviderStreamParams{ProviderID: prov.ID, ExternalID: s.name, Name: s.name,
			GroupTitle: s.group, StreamUrl: "http://fresh/" + s.name}); err != nil {
			t.Fatal(err)
		}
	}
	// What a refresh's auto-match does: nothing from this provider yet.
	if res, ran, err := svc.AutoMatch(ctx, prov.ID); err != nil || !ran || res.Created != 0 || res.Attached != 0 {
		t.Fatalf("auto-match before choosing groups = %+v %v %v", res, ran, err)
	}
	if _, body := b.get("/admin/providers"); !strings.Contains(body, "choose groups") {
		t.Error("providers row does not ask to choose groups")
	}

	page := "/admin/providers/" + strconv.FormatInt(prov.ID, 10) + "/groups"
	if _, body := b.get(page); !strings.Contains(body, `name="action" value="match">Save and match`) {
		t.Error("groups page lacks Save and match")
	}
	resp, _ := b.post(page, url.Values{"mode": {"selected"}, "group": {"US News"}}, b.base)
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || loc != page+"?msg=saved" {
		t.Fatalf("save = %d %s", resp.StatusCode, loc)
	}
	if chs, _ := q.ListChannels(ctx, db.ListChannelsParams{Limit: 10}); len(chs) != 0 {
		t.Fatalf("plain save matched: %+v", chs)
	}
	// Save and match runs even with automatic matching turned off.
	m, _ := svc.MatchSettings(ctx)
	m.Auto = false
	if err := svc.SaveMatchSettings(ctx, m); err != nil {
		t.Fatal(err)
	}
	resp, _ = b.post(page, url.Values{"mode": {"selected"}, "group": {"US News"}, "action": {"match"}}, b.base)
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(loc, "created=1") {
		t.Fatalf("save and match = %d %s", resp.StatusCode, loc)
	}
	if _, body := b.get(loc); !strings.Contains(body, "Matching created 1 channels") {
		t.Errorf("no match flash:\n%s", body)
	}
	chs, _ := q.ListChannels(ctx, db.ListChannelsParams{Limit: 10})
	if len(chs) != 1 || !strings.Contains(chs[0].Name, "Fox") {
		t.Errorf("channels = %+v, want only Fox News", chs)
	}
}
