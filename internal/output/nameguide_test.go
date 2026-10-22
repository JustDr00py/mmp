package output

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/nameguide"
)

// TestStreamNameGuide: channels without a guide get one from their stream
// name, in the XMLTV and in Xtream listings; channels with a guide keep it.
func TestStreamNameGuide(t *testing.T) {
	hs, h := newTestHandlers(t)
	hs.Now = func() time.Time { return guideNow } // 2026-10-13 12:00 UTC
	store := hs.Store.(*fakeStore)
	store.guide = map[string][]db.ListGuideProgramsRow{
		"news.us": {{TvgKey: "news.us", ID: 1, Title: "Real news", StartTime: guideNow, StopTime: guideNow.Add(time.Hour)}},
	}
	store.all = []db.ListEnabledChannelsRow{
		{ID: 1, Name: "News", TvgID: "news.us", StreamName: "Next | Something | 2026-10-13 | 13:00"},
		{ID: 2, Name: "PPV 1", StreamName: "Next | Jets @ Patriots | NFL Game Pass | 2026-10-13 | 17:00"},
		{ID: 3, Name: "Movies", TvgID: "movies.us", StreamName: "US: Movies HD"}, // a tvg-id no guide has
	}
	settings := nameguide.DefaultSettings()
	hs.NameGuide = func(context.Context) nameguide.Settings { return settings }

	_, _, body := get(t, h, "/xmltv.php?username=root&password=root-pw1")
	for _, want := range []string{
		`<channel id="mmp.2">`,
		`start="20261013120000 +0000" stop="20261013170000 +0000" channel="mmp.2">` + "\n  <title>Up next: Jets @ Patriots</title>",
		`start="20261013170000 +0000" stop="20261013200000 +0000" channel="mmp.2">` + "\n  <title>Jets @ Patriots</title>\n  <desc>NFL Game Pass</desc>",
		`channel="movies.us">` + "\n  <title>Movies</title>", // no event in the name: an all-day entry
		"<title>Real news</title>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("guide lacks %q", want)
		}
	}
	if strings.Contains(body, "Something") {
		t.Error("a channel with a real guide got a stream-name guide")
	}

	// Xtream short EPG for the event channel.
	_, _, body = get(t, h, "/player_api.php?username=root&password=root-pw1&action=get_short_epg&stream_id=2&limit=2")
	var resp struct {
		Listings []struct {
			Title     string `json:"title"`
			ChannelID string `json:"channel_id"`
		} `json:"epg_listings"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	if len(resp.Listings) != 2 || resp.Listings[0].ChannelID != "mmp.2" {
		t.Fatalf("short epg = %s", body)
	}
	if title, _ := base64.StdEncoding.DecodeString(resp.Listings[1].Title); string(title) != "Jets @ Patriots" {
		t.Errorf("second listing = %q", title)
	}

	// Off: nothing generated.
	settings.Enabled = false
	if _, _, body := get(t, h, "/xmltv.php?username=root&password=root-pw1"); strings.Contains(body, "Jets") || strings.Contains(body, "<title>Movies") {
		t.Errorf("disabled still generates:\n%s", body)
	}
}

// TestGuideModes: "name" channels take their stream-name guide over XMLTV
// placeholders (even with the stream-name guide off for the rest), and
// their XMLTV guide when the name gives none; "xmltv" channels never get
// a stream-name guide.
func TestGuideModes(t *testing.T) {
	hs, h := newTestHandlers(t)
	hs.Now = func() time.Time { return guideNow } // 2026-10-13 12:00 UTC
	store := hs.Store.(*fakeStore)
	placeholder := func(key, title string) []db.ListGuideProgramsRow {
		return []db.ListGuideProgramsRow{{TvgKey: key, ID: 1, Title: title, StartTime: guideNow, StopTime: guideNow.Add(time.Hour)}}
	}
	store.guide = map[string][]db.ListGuideProgramsRow{
		"espnplus1": placeholder("espnplus1", "No Event Scheduled"),
		"sports4":   placeholder("sports4", "Real sports"),
	}
	store.all = []db.ListEnabledChannelsRow{
		{ID: 1, Name: "ESPN+ 1", TvgID: "espnplus1", GuideMode: GuideName, StreamName: "Next | Bills @ Jets | ESPN+ | 2026-10-13 | 17:00"},
		{ID: 2, Name: "Docs", TvgID: "docs.us", GuideMode: GuideXMLTV, StreamName: "Next | Planet | Docs | 2026-10-13 | 17:00"},
		{ID: 4, Name: "Sports 4", TvgID: "sports4", GuideMode: GuideName, StreamName: "US: Sports 4"},
		{ID: 5, Name: "PPV 5", GuideMode: GuideAuto, StreamName: "Next | Fight Night | PPV | 2026-10-13 | 17:00"},
	}
	settings := nameguide.Settings{Enabled: false, All: false, Zone: "UTC"}
	hs.NameGuide = func(context.Context) nameguide.Settings { return settings }

	_, _, body := get(t, h, "/xmltv.php?username=root&password=root-pw1")
	for _, want := range []string{"<title>Bills @ Jets</title>", "<title>Real sports</title>"} {
		if !strings.Contains(body, want) {
			t.Errorf("guide lacks %q:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{"No Event Scheduled", "Planet", "Fight Night"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("guide has %q:\n%s", unwanted, body)
		}
	}

	// Xtream listings pick the same way.
	_, _, body = get(t, h, "/player_api.php?username=root&password=root-pw1&action=get_short_epg&stream_id=1&limit=2")
	if want := base64.StdEncoding.EncodeToString([]byte("Up next: Bills @ Jets")); !strings.Contains(body, want) {
		t.Errorf("short epg = %s", body)
	}

	// With the stream-name guide on, "auto" channels without a guide get
	// one; "xmltv" channels still do not.
	settings = nameguide.DefaultSettings()
	_, _, body = get(t, h, "/xmltv.php?username=root&password=root-pw1")
	if !strings.Contains(body, "Fight Night") {
		t.Errorf("auto channel without a guide got none:\n%s", body)
	}
	if strings.Contains(body, "Planet") || strings.Contains(body, "<title>Docs") {
		t.Errorf("xmltv-only channel got a stream-name guide:\n%s", body)
	}
}
