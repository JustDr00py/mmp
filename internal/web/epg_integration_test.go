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

	src, err := q.UpsertEpgSource(ctx, db.UpsertEpgSourceParams{Name: "Lists", Url: withEPG.EpgUrl, ProviderID: &withEPG.ID})
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
