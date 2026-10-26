//go:build integration

package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/JustDr00py/mmp/internal/db"
)

func TestEPGPage(t *testing.T) {
	b, q := pgUI(t)
	ctx := context.Background()
	mk := func(name, typ, epg string, prio int32) db.Provider {
		p, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: name, Type: typ, Url: "http://" + strings.ToLower(name) + "/",
			Username: "u", Password: "pw", EpgUrl: epg, RefreshIntervalSecs: 3600, Priority: prio, Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	xt := mk("Panel", "xtream", "", 1)
	withEPG := mk("Lists", "m3u", "http://guide.example/xmltv.php?username=gu&password=guide-secret", 2)
	mk("NoGuide", "m3u", "", 3)

	src, err := q.UpsertEpgSource(ctx, db.UpsertEpgSourceParams{Name: "Lists", Url: withEPG.EpgUrl, ProviderID: withEPG.ID})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	if _, err := q.InsertEpgPrograms(ctx, []db.InsertEpgProgramsParams{
		{EpgSourceID: src.ID, ChannelTvgID: "BBC1.uk", Title: "The News", StartTime: now.Add(-10 * time.Minute), StopTime: now.Add(20 * time.Minute)},
		{EpgSourceID: src.ID, ChannelTvgID: "BBC1.uk", Title: "Weather", StartTime: now.Add(20 * time.Minute), StopTime: now.Add(30 * time.Minute)},
		{EpgSourceID: src.ID, ChannelTvgID: "old.uk", Title: "Yesterday", StartTime: now.Add(-30 * time.Hour), StopTime: now.Add(-29 * time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}
	// What a refresh records for the guide it loaded.
	if err := q.MarkEpgSourceRefreshed(ctx, db.MarkEpgSourceRefreshedParams{ID: src.ID, Programs: 3, Channels: 2}); err != nil {
		t.Fatal(err)
	}
	for i, c := range []struct{ name, tvg string }{{"BBC One", "bbc1.uk"}, {"Old Channel", "old.uk"}, {"No Tvg", ""}} {
		n := int32(101 + i)
		if _, err := q.CreateChannel(ctx, db.CreateChannelParams{Number: &n, Name: c.name, TvgID: c.tvg}); err != nil {
			t.Fatal(err)
		}
	}

	_, body := b.get("/admin/epg")
	if !strings.Contains(body, ">Panel</a>") || !strings.Contains(body, "not downloaded yet") || !strings.Contains(body, ">Lists</a>") || strings.Contains(body, ">NoGuide</a>") {
		t.Fatalf("sources:\n%s", body)
	}
	if strings.Contains(body, "guide-secret") {
		t.Error("EPG URL shown with its password")
	}
	if !strings.Contains(body, `<td class="num">3</td><td class="num">2</td>`) { // 3 programs, 2 tvg-ids
		t.Errorf("source stats missing")
	}
	// Case-insensitive match: BBC One (bbc1.uk) has "The News" now and 2 in 24h.
	if !strings.Contains(body, "<td>The News</td>") || !strings.Contains(body, `<td class="num">2</td>`) {
		t.Errorf("coverage for BBC One missing")
	}

	_, body = b.htmx(http.MethodGet, "/admin/epg?missing=1", nil, "guide")
	if !strings.HasPrefix(strings.TrimSpace(body), `<div id="guide">`) || strings.Contains(body, "BBC One") ||
		!strings.Contains(body, "Old Channel") || !strings.Contains(body, "No Tvg") {
		t.Errorf("missing-only fragment:\n%s", body)
	}

	resp, body := b.htmx(http.MethodPost, "/admin/epg/providers/"+strconv.FormatInt(xt.ID, 10)+"/refresh", nil, "epg-"+strconv.FormatInt(xt.ID, 10))
	if resp.StatusCode != 200 || !strings.HasPrefix(strings.TrimSpace(body), `<tr id="epg-`) || !strings.Contains(body, "4321 programs") {
		t.Errorf("refresh = %d\n%s", resp.StatusCode, body)
	}
}

func TestProfilePages(t *testing.T) {
	b, q := pgUI(t)
	ctx := context.Background()

	_, body := b.get("/admin/profiles")
	if !strings.Contains(body, ">default</a>") {
		t.Fatalf("list lacks the default profile:\n%s", body)
	}
	for _, bad := range []url.Values{
		{"name": {"x"}, "mode": {"ffmpeg"}, "ffmpeg_args": {"-i http://evil/ -c copy"}},
		{"name": {"x"}, "mode": {"ffmpeg"}, "ffmpeg_args": {""}},
		{"name": {"x"}, "mode": {"weird"}},
		{"name": {""}, "mode": {"passthrough"}},
	} {
		if resp, body := b.post("/admin/profiles/new", bad, b.base); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, `class="error"`) {
			t.Errorf("%v = %d", bad, resp.StatusCode)
		}
	}
	resp, _ := b.post("/admin/profiles/new", url.Values{"name": {"remux"}, "mode": {"ffmpeg"}, "ffmpeg_args": {"-c copy"}, "user_agent": {"VLC/3"}}, b.base)
	if resp.Header.Get("Location") != "/admin/profiles?msg=created" {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	if resp, body := b.post("/admin/profiles/new", url.Values{"name": {"remux"}, "mode": {"passthrough"}}, b.base); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "exists") {
		t.Errorf("duplicate = %d", resp.StatusCode)
	}
	ps, _ := q.ListStreamProfiles(ctx)
	var remux db.StreamProfile
	for _, p := range ps {
		if p.Name == "remux" {
			remux = p
		}
	}
	if remux.FfmpegArgs != "-c copy" || remux.UserAgent != "VLC/3" {
		t.Fatalf("stored = %+v", remux)
	}
	if _, err := q.CreateChannel(ctx, db.CreateChannelParams{Name: "C", StreamProfileID: &remux.ID}); err != nil {
		t.Fatal(err)
	}
	_, body = b.get("/admin/profiles")
	if !strings.Contains(body, ">remux</a>") || !strings.Contains(body, `<td class="num" data-label="Channels">1</td>`) {
		t.Errorf("channel count missing:\n%s", body)
	}
	path := "/admin/profiles/" + strconv.FormatInt(remux.ID, 10)
	if resp, _ := b.post(path, url.Values{"name": {"remux"}, "mode": {"passthrough"}}, b.base); resp.Header.Get("Location") != "/admin/profiles?msg=saved" {
		t.Errorf("update = %d", resp.StatusCode)
	}
	if p, _ := q.GetStreamProfile(ctx, remux.ID); p.Mode != "passthrough" {
		t.Errorf("after update = %+v", p)
	}
	if resp, _ := b.post(path+"/delete", url.Values{}, b.base); resp.Header.Get("Location") != "/admin/profiles?msg=deleted" {
		t.Errorf("delete = %d", resp.StatusCode)
	}
	if resp, _ := b.get(path); resp.StatusCode != http.StatusNotFound {
		t.Errorf("deleted profile = %d", resp.StatusCode)
	}
}

func TestExtraGuidePages(t *testing.T) {
	b, q := pgUI(t)
	ctx := context.Background()
	q.CreateProvider(ctx, db.CreateProviderParams{Name: "Panel", Type: "xtream", Url: "http://panel/", Username: "u", Password: "p",
		MaxConnections: 1, RefreshIntervalSecs: 3600, Priority: 2, Enabled: true})

	_, body := b.get("/admin/epg")
	if !strings.Contains(body, "Extra guide sources") || !strings.Contains(body, "providers: Panel 2") {
		t.Fatalf("guide page:\n%s", body)
	}
	if resp, body := b.post("/admin/epg/sources", url.Values{"name": {"Bad"}, "url": {"ftp://x/y.xml"}, "enabled": {"on"}}, b.base); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "http(s) address") {
		t.Errorf("bad url = %d", resp.StatusCode)
	}
	resp, _ := b.post("/admin/epg/sources", url.Values{"name": {"Community"}, "url": {"https://guide.example/us.xml.gz?token=s3cret"}, "priority": {"0"}, "enabled": {"on"}}, b.base)
	if resp.Header.Get("Location") != "/admin/epg?msg=extra-added" {
		t.Fatalf("add = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	stats, _ := q.ListEpgSourceStats(ctx)
	var id int64
	for _, s := range stats {
		if s.Name == "Community" {
			id = s.ID
		}
	}
	if id == 0 {
		t.Fatal("source not stored")
	}
	sid := strconv.FormatInt(id, 10)
	_, body = b.get("/admin/epg?msg=extra-added")
	if !strings.Contains(body, "Community") || strings.Contains(body, "s3cret") || !strings.Contains(body, "downloading now") {
		t.Errorf("list (token must be redacted):\n%s", body)
	}
	resp, body = b.htmx(http.MethodPost, "/admin/epg/sources/"+sid+"/refresh", nil, "extra-"+sid)
	if resp.StatusCode != 200 || !strings.Contains(body, "4321 programs") || !strings.HasPrefix(strings.TrimSpace(body), `<tr id="extra-`) {
		t.Errorf("download now = %d\n%s", resp.StatusCode, body)
	}
	_, body = b.get("/admin/epg/sources/" + sid + "/edit")
	if !strings.Contains(body, `value="Community"`) || !strings.Contains(body, `name="enabled" checked`) {
		t.Errorf("edit page:\n%s", body)
	}
	b.post("/admin/epg/sources/"+sid+"/edit", url.Values{"name": {"Community US"}, "url": {"https://guide.example/us.xml.gz"}, "priority": {"3"}}, b.base)
	s, _ := q.GetEpgSource(ctx, id)
	if s.Name != "Community US" || s.Enabled || s.Priority == nil || *s.Priority != 3 {
		t.Errorf("after edit = %+v", s)
	}
	b.post("/admin/epg/sources/"+sid+"/delete", url.Values{}, b.base)
	if _, err := q.GetEpgSource(ctx, id); err == nil {
		t.Error("not deleted")
	}
	if resp, _ := b.get("/admin/epg/sources/999999/edit"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown source = %d", resp.StatusCode)
	}
}

// TestProviderGuidePriority: a provider's guide ranks by the provider's
// priority unless it has its own, so guides can be reordered without
// touching failover; every place that shows a program follows that order.
func TestProviderGuidePriority(t *testing.T) {
	b, q := pgUI(t)
	ctx := context.Background()
	now := time.Now()
	mk := func(name string, prio int32, title string) db.Provider {
		t.Helper()
		p, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: name, Type: "m3u", Url: "http://" + name + "/",
			EpgUrl: "http://" + name + "/epg.xml", MaxConnections: 1, RefreshIntervalSecs: 3600, Priority: prio, Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		src, err := q.UpsertEpgSource(ctx, db.UpsertEpgSourceParams{Name: name, Url: p.EpgUrl, ProviderID: p.ID})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := q.InsertEpgPrograms(ctx, []db.InsertEpgProgramsParams{{EpgSourceID: src.ID, ChannelTvgID: "sportsnet.us",
			Title: title, StartTime: now.Add(-time.Hour), StopTime: now.Add(time.Hour)}}); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mk("Alpha", 1, "Alpha's show")
	beta := mk("Beta", 2, "Beta's show")
	ch, _ := q.CreateChannel(ctx, db.CreateChannelParams{Name: "SportsNet", TvgID: "SportsNet.us"})

	nowTitle := func() string {
		t.Helper()
		progs, err := q.ListGuidePrograms(ctx, db.ListGuideProgramsParams{TvgKeys: []string{"sportsnet.us"}, Since: now, Until: now.Add(time.Minute)})
		if err != nil || len(progs) != 1 {
			t.Fatalf("guide programs = %+v, %v", progs, err)
		}
		info, _ := q.LiveChannelInfo(ctx, []int64{ch.ID})
		if len(info) != 1 || info[0].NowTitle != progs[0].Title {
			t.Errorf("dashboard now playing = %+v, customers get %q", info, progs[0].Title)
		}
		cov, _ := q.ChannelGuideStatus(ctx, db.ChannelGuideStatusParams{Limit: 10})
		if len(cov) != 1 || cov[0].NowTitle != progs[0].Title || cov[0].Upcoming != 1 {
			t.Errorf("guide coverage = %+v, customers get %q from one guide", cov, progs[0].Title)
		}
		return progs[0].Title
	}
	if got := nowTitle(); got != "Alpha's show" {
		t.Fatalf("by provider priority: %q, want Alpha's", got)
	}

	// Beta's guide ranks first on its own; its streams keep priority 2.
	resp, _ := b.post("/admin/epg/providers/"+strconv.FormatInt(beta.ID, 10)+"/priority", url.Values{"priority": {"0"}}, b.base)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("set priority = %d", resp.StatusCode)
	}
	if got := nowTitle(); got != "Beta's show" {
		t.Errorf("with Beta's guide at 0: %q, want Beta's", got)
	}
	if p, _ := q.GetProvider(ctx, beta.ID); p.Priority != 2 {
		t.Errorf("provider priority changed to %d", p.Priority)
	}
	if _, body := b.get("/admin/epg"); !strings.Contains(body, `name="priority" value="0" placeholder="2"`) ||
		!strings.Contains(body, "its own (the provider's is 2") || strings.Index(body, ">Beta</a>") > strings.Index(body, ">Alpha</a>") {
		t.Errorf("guide page does not show Beta's own priority first:\n%s", body)
	}

	// Blank goes back to the provider's priority.
	b.post("/admin/epg/providers/"+strconv.FormatInt(beta.ID, 10)+"/priority", url.Values{"priority": {""}}, b.base)
	if got := nowTitle(); got != "Alpha's show" {
		t.Errorf("after clearing: %q, want Alpha's", got)
	}
	if resp, _ := b.post("/admin/epg/providers/"+strconv.FormatInt(beta.ID, 10)+"/priority", url.Values{"priority": {"x"}}, b.base); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("non-number = %d", resp.StatusCode)
	}
}

// TestStreamNameGuidePages: channels without a guide show what their
// stream name says on the Guide page; the Settings page turns it off and
// validates the zone.
func TestStreamNameGuidePages(t *testing.T) {
	b, q := pgUI(t)
	ctx := context.Background()
	p, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: "P", Type: "m3u", Url: "http://p/", MaxConnections: 1,
		RefreshIntervalSecs: 3600, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Add(-time.Hour) // on now
	name := "Live | UFC Fight Night | Buckley vs Malott | " + start.Format("2006-01-02") + " | " + start.Format("15:04") + " (GMT)"
	sid, err := q.UpsertProviderStream(ctx, db.UpsertProviderStreamParams{ProviderID: p.ID, ExternalID: "1", Name: name, StreamUrl: "http://p/1"})
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := q.CreateChannel(ctx, db.CreateChannelParams{Name: "PPV 1"})
	if err := q.AddStreamToChannel(ctx, db.AddStreamToChannelParams{ChannelID: ch.ID, ProviderStreamID: sid, Priority: 1}); err != nil {
		t.Fatal(err)
	}
	if rows, _ := q.ListEnabledChannels(ctx); len(rows) != 1 || rows[0].StreamName != name {
		t.Fatalf("channels carry their stream name: %+v", rows)
	}

	_, body := b.get("/admin/epg")
	for _, want := range []string{"UFC Fight Night: Buckley vs Malott", "its stream name", ">generated<"} {
		if !strings.Contains(body, want) {
			t.Errorf("guide coverage lacks %q", want)
		}
	}

	if resp, body := b.post("/admin/settings/nameguide", url.Values{"zone": {"Mars/Olympus"}}, b.base); resp.StatusCode != http.StatusUnprocessableEntity ||
		!strings.Contains(body, "unknown time zone") {
		t.Errorf("bad zone = %d", resp.StatusCode)
	}
	if resp, _ := b.post("/admin/settings/nameguide", url.Values{"zone": {"Europe/Stockholm"}}, b.base); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save = %d", resp.StatusCode) // both boxes unticked: off
	}
	if _, body := b.get("/admin/settings"); !strings.Contains(body, `value="Europe/Stockholm"`) || strings.Contains(body, `name="enabled" checked`) {
		t.Error("settings not saved")
	}
	if _, body := b.get("/admin/epg"); strings.Contains(body, "its stream name") {
		t.Error("turned off, the coverage still shows a stream-name guide")
	}
}

// The channels guides list can be searched by name or id, on the EPG page
// and as suggestions under a channel's tvg-id field.
func TestGuideChannelSearch(t *testing.T) {
	b, q := pgUI(t)
	ctx := context.Background()
	src, err := q.CreateExtraEpgSource(ctx, db.CreateExtraEpgSourceParams{Name: "US guide", Url: "http://guide.example/us.xml", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	off, err := q.CreateExtraEpgSource(ctx, db.CreateExtraEpgSourceParams{Name: "Off guide", Url: "http://guide.example/off.xml"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertEpgChannels(ctx, []db.InsertEpgChannelsParams{
		{EpgSourceID: src.ID, TvgID: "HBO.East.us", DisplayNames: []string{"HBO East", "HBO"}, ProgramCount: 40},
		{EpgSourceID: src.ID, TvgID: "HBO.West.us", DisplayNames: []string{"HBO West"}, ProgramCount: 40},
		{EpgSourceID: src.ID, TvgID: "HBO2.us", DisplayNames: []string{}, ProgramCount: 0},
		{EpgSourceID: off.ID, TvgID: "HBO.Pacific.us", DisplayNames: []string{"HBO Pacific"}, ProgramCount: 40},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateChannel(ctx, db.CreateChannelParams{Name: "HBO", TvgID: "hbo.east.us"}); err != nil {
		t.Fatal(err)
	}

	_, body := b.get("/admin/epg?gq=" + url.QueryEscape("hbo west"))
	if !strings.Contains(body, "HBO.West.us") || strings.Contains(body, "HBO.East.us") {
		t.Errorf("every word must match:\n%s", body)
	}

	_, body = b.htmx(http.MethodGet, "/admin/epg/channels?gq=hbo", nil, "guide-channels")
	if !strings.HasPrefix(strings.TrimSpace(body), `<div id="guide-channels">`) {
		t.Fatalf("fragment:\n%s", body)
	}
	east, west, two := strings.Index(body, "HBO.East.us"), strings.Index(body, "HBO.West.us"), strings.Index(body, "HBO2.us")
	if east < 0 || west < east || two < west || !strings.Contains(body, "HBO East · HBO") || !strings.Contains(body, "1 channel<") {
		t.Errorf("results (with programs first, used-by count):\n%s", body)
	}
	if strings.Contains(body, "HBO.Pacific.us") {
		t.Error("disabled guide's channels listed")
	}
	if _, body = b.htmx(http.MethodGet, "/admin/epg/channels?gq=zzz", nil, "guide-channels"); !strings.Contains(body, "No guide channel matches") {
		t.Errorf("no match:\n%s", body)
	}
	// A LIKE wildcard in the search is taken literally.
	if _, body = b.htmx(http.MethodGet, "/admin/epg/channels?gq=%25", nil, "guide-channels"); strings.Contains(body, "HBO.West.us") {
		t.Errorf("wildcard matched:\n%s", body)
	}

	_, body = b.htmx(http.MethodGet, "/admin/epg/channels/suggest?tvg_id=hbo+west", nil, "tvg-suggest")
	if !strings.Contains(body, `data-value="HBO.West.us"`) || strings.Contains(body, "HBO.East.us") {
		t.Errorf("suggestions:\n%s", body)
	}
	// The field already holds the one match: nothing to pick.
	if _, body = b.htmx(http.MethodGet, "/admin/epg/channels/suggest?tvg_id=HBO.West.us", nil, "tvg-suggest"); strings.Contains(body, "data-value") {
		t.Errorf("suggested the id already typed:\n%s", body)
	}
	if _, body = b.htmx(http.MethodGet, "/admin/epg/channels/suggest?tvg_id=", nil, "tvg-suggest"); strings.TrimSpace(body) != "" {
		t.Errorf("empty field suggestions:\n%s", body)
	}
}

// The guide channel search narrows to one guide (and then lists it
// without a search), to ids with programs, or to ids no channel uses.
func TestGuideChannelSearchFilters(t *testing.T) {
	b, q := pgUI(t)
	ctx := context.Background()
	us, err := q.CreateExtraEpgSource(ctx, db.CreateExtraEpgSourceParams{Name: "US guide", Url: "http://guide.example/us.xml", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	uk, err := q.CreateExtraEpgSource(ctx, db.CreateExtraEpgSourceParams{Name: "UK guide", Url: "http://guide.example/uk.xml", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertEpgChannels(ctx, []db.InsertEpgChannelsParams{
		{EpgSourceID: us.ID, TvgID: "Nick.us", DisplayNames: []string{"Nickelodeon East"}, ProgramCount: 40},
		{EpgSourceID: us.ID, TvgID: "NickJr.us", DisplayNames: []string{"Nick Jr"}, ProgramCount: 0},
		{EpgSourceID: us.ID, TvgID: "CNN.us", DisplayNames: []string{"CNN"}, ProgramCount: 40},
		{EpgSourceID: uk.ID, TvgID: "Nick.uk", DisplayNames: []string{"Nickelodeon UK"}, ProgramCount: 40},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateChannel(ctx, db.CreateChannelParams{Name: "Nick", TvgID: "nick.us"}); err != nil {
		t.Fatal(err)
	}
	search := func(query string) string {
		t.Helper()
		_, body := b.htmx(http.MethodGet, "/admin/epg/channels?"+query, nil, "guide-channels")
		return body
	}
	has := func(body string, ids ...string) string {
		var got []string
		for _, id := range []string{"Nick.us", "NickJr.us", "CNN.us", "Nick.uk"} {
			if strings.Contains(body, ">"+id+"<") {
				got = append(got, id)
			}
		}
		return strings.Join(got, " ")
	}
	for _, c := range []struct{ query, want string }{
		{"gq=nick", "Nick.us NickJr.us Nick.uk"},
		{"gq=nick&guide=" + strconv.FormatInt(uk.ID, 10), "Nick.uk"},
		{"gq=&guide=" + strconv.FormatInt(us.ID, 10), "Nick.us NickJr.us CNN.us"}, // a guide's whole list
		{"gq=nick&programs=1", "Nick.us Nick.uk"},
		{"gq=nick&unused=1", "NickJr.us Nick.uk"},
		{"gq=nick&guide=" + strconv.FormatInt(us.ID, 10) + "&programs=1&unused=1", ""},
		{"gq=", ""}, // no search, no guide: nothing
	} {
		if got := has(search(c.query)); got != c.want {
			t.Errorf("%s: found %q, want %q", c.query, got, c.want)
		}
	}
	// The guide picker offers the guides with a channel list.
	_, body := b.get("/admin/epg?gq=nick&guide=" + strconv.FormatInt(uk.ID, 10))
	if !strings.Contains(body, `<option value="`+strconv.FormatInt(uk.ID, 10)+`" selected>UK guide</option>`) || !strings.Contains(body, ">US guide</option>") {
		t.Errorf("guide picker:\n%s", body)
	}
}

// A provider's guide can be turned off (its downloaded guide deleted, no
// longer offered for download) and back on, from the EPG page or the
// provider form.
func TestProviderGuideSwitch(t *testing.T) {
	b, q := pgUI(t)
	ctx := context.Background()
	p, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: "Backup line", Type: "xtream", Url: "http://backup.example/",
		Username: "u", Password: "pw", RefreshIntervalSecs: 3600, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if !p.EpgEnabled {
		t.Fatal("new providers should download their guide")
	}
	src, err := q.UpsertEpgSource(ctx, db.UpsertEpgSourceParams{Name: "Backup line EPG", Url: "http://backup.example/xmltv.php", ProviderID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertEpgChannels(ctx, []db.InsertEpgChannelsParams{{EpgSourceID: src.ID, TvgID: "Nick.us", DisplayNames: []string{"Nickelodeon"}, ProgramCount: 1}}); err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(p.ID, 10)

	resp, _ := b.post("/admin/epg/providers/"+id+"/guide", url.Values{}, b.base)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("turn off = %d", resp.StatusCode)
	}
	if p, _ = q.GetProvider(ctx, p.ID); p.EpgEnabled {
		t.Error("guide still on")
	}
	if _, err := q.GetEpgSourceByProvider(ctx, &p.ID); err == nil {
		t.Error("downloaded guide kept")
	}
	if _, body := b.htmx(http.MethodGet, "/admin/epg/channels?gq=nick", nil, "guide-channels"); strings.Contains(body, "Nick.us") {
		t.Error("turned-off guide's channels still searchable")
	}
	_, body := b.get("/admin/epg")
	if !strings.Contains(body, "guide off") || !strings.Contains(body, ">Turn on</button>") {
		t.Errorf("EPG page:\n%s", body)
	}
	if resp, _ := b.post("/admin/epg/providers/"+id+"/refresh", url.Values{}, b.base); resp.StatusCode != http.StatusConflict {
		t.Errorf("download of a turned-off guide = %d, want 409", resp.StatusCode)
	}
	// Nothing recreates it, even a download that read the provider before.
	if _, err := q.UpsertEpgSource(ctx, db.UpsertEpgSourceParams{Name: "x", Url: "http://backup.example/x", ProviderID: p.ID}); err == nil {
		t.Error("guide source recreated while off")
	}

	if resp, _ := b.post("/admin/epg/providers/"+id+"/guide", url.Values{"enabled": {"on"}}, b.base); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("turn on = %d", resp.StatusCode)
	}
	if p, _ = q.GetProvider(ctx, p.ID); !p.EpgEnabled {
		t.Error("guide still off")
	}

	// The provider form's checkbox: saving it unticked turns it off.
	form := url.Values{"name": {p.Name}, "type": {"xtream"}, "url": {p.Url}, "username": {"u"},
		"max_connections": {"1"}, "refresh_minutes": {"60"}, "priority": {"0"}, "enabled": {"on"}}
	if resp, body := b.post("/admin/providers/"+id+"/edit", form, b.base); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("edit = %d:\n%s", resp.StatusCode, body)
	}
	if p, _ = q.GetProvider(ctx, p.ID); p.EpgEnabled {
		t.Error("unticked checkbox left the guide on")
	}
	form.Set("epg_enabled", "on")
	b.post("/admin/providers/"+id+"/edit", form, b.base)
	if p, _ = q.GetProvider(ctx, p.ID); !p.EpgEnabled {
		t.Error("ticked checkbox left the guide off")
	}
	if _, body := b.get("/admin/providers/" + id + "/edit"); !strings.Contains(body, `name="epg_enabled" checked`) {
		t.Error("form does not show the guide on")
	}
}
