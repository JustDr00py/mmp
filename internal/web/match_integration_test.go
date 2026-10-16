//go:build integration

package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/JustDr00py/mmp/internal/db"
)

// seedGrouped: providers A and B both carry n sports channels ("US| SPORTS")
// and two news channels ("US| NEWS"); returns stream ids by name@provider.
func seedGrouped(t *testing.T, q *db.Queries, sports int) map[string]int64 {
	t.Helper()
	ctx := context.Background()
	ids := map[string]int64{}
	for i, pn := range []string{"A", "B"} {
		prov, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: pn, Type: "m3u", Url: "http://" + pn + "/", MaxConnections: 1,
			RefreshIntervalSecs: 3600, Priority: int32(i + 1), Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		add := func(name, group, logo string) {
			id, err := q.UpsertProviderStream(ctx, db.UpsertProviderStreamParams{ProviderID: prov.ID, ExternalID: name, Name: name,
				GroupTitle: group, LogoUrl: logo, StreamUrl: "http://" + pn + "/" + name})
			if err != nil {
				t.Fatal(err)
			}
			ids[name+"@"+pn] = id
		}
		for n := range sports {
			add(fmt.Sprintf("US: Sport %03d", n), "US| SPORTS", "")
		}
		add("US: CNN HD", "US| NEWS", "http://logos.example/cnn.png")
		add("US: Fox News", "US| NEWS", "")
	}
	return ids
}

var keyRe = regexp.MustCompile(`name="key" value="([^"]+)" aria-label="Apply ([^"]+)"`)

func keysOn(body string) map[string]string { // name -> key
	out := map[string]string{}
	for _, m := range keyRe.FindAllStringSubmatch(body, -1) {
		out[m[2]] = m[1]
	}
	return out
}

func TestMatchSearchGroupsPaging(t *testing.T) {
	b, q := pgUI(t)
	seedGrouped(t, q, 60)

	// Sorted by name, 50 a page, with group counts.
	_, body := b.get("/admin/match")
	if !strings.Contains(body, "1–50 of 62") || !strings.Contains(body, `US| SPORTS (60)`) || !strings.Contains(body, `US| NEWS (2)`) {
		t.Fatalf("first page:\n%s", body)
	}
	if i, j := strings.Index(body, `aria-label="Apply CNN"`), strings.Index(body, `aria-label="Apply Fox News"`); i < 0 || j < 0 || i > j {
		t.Errorf("not sorted by name (CNN %d, Fox %d)", i, j)
	}
	if !strings.Contains(body, `src="http://logos.example/cnn.png"`) || !strings.Contains(body, "same name") {
		t.Error("logo or plain-language reason missing")
	}
	_, body = b.get("/admin/match?offset=50")
	if !strings.Contains(body, "51–62 of 62") || !strings.Contains(body, "← Previous") || strings.Contains(body, "Next →") {
		t.Errorf("second page:\n%s", body)
	}

	// Group filter and live search (htmx fragment).
	_, body = b.get("/admin/match?group=" + url.QueryEscape("US| NEWS"))
	if k := keysOn(body); len(k) != 2 || !strings.Contains(body, "1–2 of 2 (filtered from 62)") {
		t.Errorf("group filter: %v", k)
	}
	resp, body := b.htmx(http.MethodGet, "/admin/match?q=sport+007", nil, "match-results")
	if !strings.HasPrefix(strings.TrimSpace(body), `<div id="match-results">`) || len(keysOn(body)) != 1 || resp.StatusCode != 200 {
		t.Errorf("search fragment:\n%s", body)
	}
	_, body = b.htmx(http.MethodGet, "/admin/match?q=zzz", nil, "match-results")
	if !strings.Contains(body, "No suggestions match “zzz”") {
		t.Errorf("empty search:\n%s", body)
	}
}

func TestMatchApplyOptions(t *testing.T) {
	b, q := pgUI(t)
	ctx := context.Background()
	ids := seedGrouped(t, q, 60)
	bq, _ := q.CreateBouquet(ctx, db.CreateBouquetParams{Name: "Basic"})
	bqID := strconv.FormatInt(bq.ID, 10)
	bq2, _ := q.CreateBouquet(ctx, db.CreateBouquetParams{Name: "Sports"})
	bq2ID := strconv.FormatInt(bq2.ID, 10)
	q.CreateBouquet(ctx, db.CreateBouquetParams{Name: "Kids"}) // not ticked

	// One row: renamed, only provider A's stream, into a group named after
	// the provider group, and into two of the three bouquets.
	_, body := b.get("/admin/match?group=" + url.QueryEscape("US| NEWS"))
	cnn := keysOn(body)["CNN"]
	if cnn == "" {
		t.Fatalf("no CNN key:\n%s", body)
	}
	f := url.Values{"scope": {"selected"}, "key": {cnn}, "row": {cnn}, "name|" + cnn: {"CNN USA"},
		"stream|" + cnn: {strconv.FormatInt(ids["US: CNN HD@A"], 10)}, "cg": {"source"}, "bq": {bqID, bq2ID},
		"group": {"US| NEWS"}, "min": {"2"}}
	resp, _ := b.post("/admin/match/apply", f, b.base)
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(loc, "applied=1-1") || !strings.Contains(loc, "cg=source") || !strings.Contains(loc, "bq="+bqID+"&bq="+bq2ID) {
		t.Fatalf("apply = %d %s", resp.StatusCode, loc)
	}
	_, body = b.get(loc)
	if !strings.Contains(body, "Done: 1 channel created, 1 stream added, all in bouquets “Basic”, “Sports”") ||
		!strings.Contains(body, `<option value="source" selected>`) || !strings.Contains(body, `value="`+bqID+`" checked> Basic`) ||
		!strings.Contains(body, `value="`+bq2ID+`" checked> Sports`) || strings.Contains(body, `checked> Kids`) {
		t.Errorf("after apply (flash, sticky options):\n%s", body)
	}
	chans, _ := q.ListChannels(ctx, db.ListChannelsParams{Limit: 10})
	if len(chans) != 1 || chans[0].Name != "CNN USA" || chans[0].LogoUrl != "http://logos.example/cnn.png" || chans[0].GroupID == nil {
		t.Fatalf("channel = %+v", chans)
	}
	if got := order(t, q, chans[0].ID); !eq(got, []int64{ids["US: CNN HD@A"]}) {
		t.Errorf("streams = %v, want only A's", got)
	}
	groups, _ := q.ListChannelGroups(ctx)
	if len(groups) != 1 || groups[0].Name != "US| NEWS" || *chans[0].GroupID != groups[0].ID {
		t.Errorf("channel groups = %+v", groups)
	}
	for _, c := range func() []db.BouquetCountsRow { n, _ := q.BouquetCounts(ctx); return n }() {
		if want := int64(map[bool]int{true: 1}[c.ID == bq.ID || c.ID == bq2.ID]); c.Channels != want {
			t.Errorf("bouquet %d has %d channels, want %d", c.ID, c.Channels, want)
		}
	}
	// B's unticked stream stays unassigned, so it is suggested again.
	_, body = b.get("/admin/match?min=1&q=cnn")
	if !strings.Contains(body, "US: CNN HD") {
		t.Errorf("unticked stream not offered again:\n%s", body)
	}

	// Apply all matching a filter (across pages), into an existing group.
	cg, _ := q.CreateChannelGroup(ctx, db.CreateChannelGroupParams{Name: "Sports"})
	resp, _ = b.post("/admin/match/apply", url.Values{"scope": {"all"}, "group": {"US| SPORTS"}, "min": {"2"},
		"cg": {strconv.FormatInt(cg.ID, 10)}}, b.base)
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "applied=60-120") {
		t.Fatalf("apply all = %s", loc)
	}
	chans, _ = q.ListChannels(ctx, db.ListChannelsParams{Limit: 100})
	inSports := 0
	for _, c := range chans {
		if c.GroupID != nil && *c.GroupID == cg.ID {
			inSports++
		}
	}
	if len(chans) != 61 || inSports != 60 {
		t.Errorf("channels = %d, in Sports = %d", len(chans), inSports)
	}
	// Show: additions to existing channels only. A new stream named like
	// an existing channel, and a placeholder named the same, arrive.
	provs, _ := q.ListProviders(ctx)
	for _, n := range []string{"US: CNN USA FHD", "##### CNN USA #####"} {
		if _, err := q.UpsertProviderStream(ctx, db.UpsertProviderStreamParams{ProviderID: provs[0].ID, ExternalID: n, Name: n,
			GroupTitle: "US| NEWS", StreamUrl: "http://x/" + n}); err != nil {
			t.Fatal(err)
		}
	}
	_, body = b.get("/admin/match?min=1&show=existing")
	if k := keysOn(body); len(k) != 1 || k["CNN USA"] == "" {
		t.Errorf("show=existing: %v\n%s", k, body)
	}
	if strings.Contains(body, "##### CNN USA #####") {
		t.Error("placeholder offered")
	}
	if _, body = b.get("/admin/match?min=1&show=new"); keysOn(body)["CNN USA"] != "" || keysOn(body)["Fox News"] == "" {
		t.Errorf("show=new: %v", keysOn(body))
	}

	// Fox News was outside the filter and is untouched.
	if _, body = b.get("/admin/match?min=2"); len(keysOn(body)) != 1 || keysOn(body)["Fox News"] == "" {
		t.Errorf("left after apply all: %v", keysOn(body))
	}
}
