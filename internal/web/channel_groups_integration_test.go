//go:build integration

package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/JustDr00py/mmp/internal/db"
)

func TestChannelGroupsPage(t *testing.T) {
	b, q := pgUI(t)
	ctx := context.Background()

	resp, _ := b.post("/admin/groups", url.Values{"name": {"Sports"}, "sort_order": {"5"}}, b.base)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/admin/groups?msg=created" {
		t.Fatalf("create = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, body := b.post("/admin/groups", url.Values{"name": {"Sports"}}, b.base); resp.StatusCode != http.StatusUnprocessableEntity ||
		!strings.Contains(body, "A group with that name exists.") {
		t.Fatalf("duplicate = %d\n%s", resp.StatusCode, body)
	}
	groups, _ := q.ListChannelGroups(ctx)
	if len(groups) != 1 || groups[0].Name != "Sports" || groups[0].SortOrder != 5 {
		t.Fatalf("groups = %+v", groups)
	}
	g := groups[0]
	gid := strconv.FormatInt(g.ID, 10)
	ch, _ := q.CreateChannel(ctx, db.CreateChannelParams{Name: "ESPN", GroupID: &g.ID})

	_, body := b.get("/admin/groups")
	for _, want := range []string{`value="Sports"`, `href="/admin/channels?group=` + gid + `">1</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("groups page lacks %q", want)
		}
	}

	if resp, _ := b.post("/admin/groups/"+gid, url.Values{"name": {"Sport"}, "sort_order": {"2"}}, b.base); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("update = %d", resp.StatusCode)
	}
	if g, _ = q.GetChannelGroup(ctx, g.ID); g.Name != "Sport" || g.SortOrder != 2 {
		t.Fatalf("after update = %+v", g)
	}
	if resp, _ := b.post("/admin/groups/999999", url.Values{"name": {"X"}}, b.base); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("update missing = %d", resp.StatusCode)
	}

	if resp, _ := b.post("/admin/groups/"+gid+"/delete", nil, b.base); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	if c, _ := q.GetChannel(ctx, ch.ID); c.GroupID != nil {
		t.Fatalf("channel keeps deleted group %d", *c.GroupID)
	}
	if _, body := b.get("/admin/groups"); !strings.Contains(body, "1 channel</a> have no group") {
		t.Errorf("groups page lacks the ungrouped count:\n%s", body)
	}
}

// seedGroupedChannels creates one channel per stream group title ("" = a
// stream without a group), each playing its stream; returns channel ids by
// name.
func seedGroupedChannels(t *testing.T, q *db.Queries, groups map[string]string) map[string]int64 {
	t.Helper()
	ctx := context.Background()
	prov, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: "P", Type: "m3u", Url: "http://p/", MaxConnections: 1,
		RefreshIntervalSecs: 3600, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	for name, title := range groups {
		sid, err := q.UpsertProviderStream(ctx, db.UpsertProviderStreamParams{ProviderID: prov.ID, ExternalID: name, Name: name,
			GroupTitle: title, StreamUrl: "http://p/" + name})
		if err != nil {
			t.Fatal(err)
		}
		c, err := q.CreateChannel(ctx, db.CreateChannelParams{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		if err := q.AddStreamToChannel(ctx, db.AddStreamToChannelParams{ChannelID: c.ID, ProviderStreamID: sid, Priority: 1}); err != nil {
			t.Fatal(err)
		}
		ids[name] = c.ID
	}
	return ids
}

func groupName(t *testing.T, q *db.Queries, channelID int64) string {
	t.Helper()
	c, err := q.GetChannel(context.Background(), channelID)
	if err != nil {
		t.Fatal(err)
	}
	if c.GroupID == nil {
		return ""
	}
	g, err := q.GetChannelGroup(context.Background(), *c.GroupID)
	if err != nil {
		t.Fatal(err)
	}
	return g.Name
}

func TestChannelGroupsFill(t *testing.T) {
	b, q := pgUI(t)
	ctx := context.Background()
	ids := seedGroupedChannels(t, q, map[string]string{"CNN": "US News", "Fox": "US News", "Sky": " UK Sports ", "Loose": "", "Kept": "Movies"})
	kids, _ := q.CreateChannelGroup(ctx, db.CreateChannelGroupParams{Name: "Kids"})
	kept := []int64{ids["Kept"]}
	if _, err := q.SetChannelsGroup(ctx, db.SetChannelsGroupParams{GroupID: &kids.ID, ChannelIds: kept}); err != nil {
		t.Fatal(err)
	}

	resp, _ := b.post("/admin/groups/fill", nil, b.base)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/admin/groups?msg=filled&n=3" {
		t.Fatalf("fill = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	for name, want := range map[string]string{"CNN": "US News", "Fox": "US News", "Sky": "UK Sports", "Loose": "", "Kept": "Kids"} {
		if got := groupName(t, q, ids[name]); got != want {
			t.Errorf("%s in group %q, want %q", name, got, want)
		}
	}
	if _, body := b.get("/admin/groups?msg=filled&n=3"); !strings.Contains(body, "Put 3 channels in the group") {
		t.Errorf("fill flash missing")
	}
}

func TestChannelsSetGroup(t *testing.T) {
	b, q := pgUI(t)
	ctx := context.Background()
	ids := seedGroupedChannels(t, q, map[string]string{"CNN": "US News", "Loose": ""})
	g, _ := q.CreateChannelGroup(ctx, db.CreateChannelGroupParams{Name: "Sports"})
	sel := func(to string, names ...string) *http.Response {
		f := url.Values{"to": {to}, "q": {"o"}, "group": {"0"}}
		for _, n := range names {
			f.Add("channel", strconv.FormatInt(ids[n], 10))
		}
		resp, _ := b.post("/admin/channels/group", f, b.base)
		return resp
	}

	_, body := b.get("/admin/channels?group=-1")
	if !strings.Contains(body, ">CNN</a>") || !strings.Contains(body, `<option value="-1" selected>`) {
		t.Fatalf("ungrouped filter lacks CNN:\n%s", body)
	}

	resp := sel(strconv.FormatInt(g.ID, 10), "CNN", "Loose")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/admin/channels?msg=grouped&n=2&of=2&q=o" {
		t.Fatalf("move = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if groupName(t, q, ids["CNN"]) != "Sports" || groupName(t, q, ids["Loose"]) != "Sports" {
		t.Fatal("channels not moved")
	}
	if _, body := b.get("/admin/channels?group=-1"); strings.Contains(body, ">CNN</a>") {
		t.Error("ungrouped filter still lists CNN")
	}
	if _, body := b.get("/admin/channels?msg=grouped&n=2&of=2"); !strings.Contains(body, "Updated 2 channels of 2 selected.") {
		t.Error("flash missing")
	}

	// Provider group: only channels whose stream has one change.
	if resp := sel("source", "CNN", "Loose"); !strings.Contains(resp.Header.Get("Location"), "n=1&of=2") {
		t.Fatalf("source = %s", resp.Header.Get("Location"))
	}
	if groupName(t, q, ids["CNN"]) != "US News" || groupName(t, q, ids["Loose"]) != "Sports" {
		t.Fatal("source grouping wrong")
	}

	if resp := sel("none", "CNN"); !strings.Contains(resp.Header.Get("Location"), "n=1&of=1") {
		t.Fatalf("none = %s", resp.Header.Get("Location"))
	}
	if groupName(t, q, ids["CNN"]) != "" {
		t.Fatal("CNN still grouped")
	}

	if resp := sel("999999", "CNN"); resp.StatusCode != http.StatusConflict {
		t.Fatalf("missing group = %d", resp.StatusCode)
	}
	if resp := sel("nope", "CNN"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad target = %d", resp.StatusCode)
	}
}

func TestMatchDefaultsToProviderGroups(t *testing.T) {
	b, _ := pgUI(t)
	if _, body := b.get("/admin/match"); !strings.Contains(body, `<option value="source" selected>`) {
		t.Errorf("match does not default to provider groups")
	}
	if _, body := b.get("/admin/match?cg=none"); !strings.Contains(body, `<option value="none" selected>`) {
		t.Errorf("match forgets the no-group choice")
	}
}

func TestChannelsBouquetFilter(t *testing.T) {
	b, q := pgUI(t)
	ctx := context.Background()
	ids := map[string]int64{}
	for _, n := range []string{"CNN", "ESPN", "Loose"} {
		c, err := q.CreateChannel(ctx, db.CreateChannelParams{Name: n})
		if err != nil {
			t.Fatal(err)
		}
		ids[n] = c.ID
	}
	basic, _ := q.CreateBouquet(ctx, db.CreateBouquetParams{Name: "Basic"})
	sports, _ := q.CreateBouquet(ctx, db.CreateBouquetParams{Name: "Sports"})
	q.AddChannelsToBouquet(ctx, db.AddChannelsToBouquetParams{BouquetID: basic.ID, ChannelIds: []int64{ids["CNN"], ids["ESPN"]}})
	q.AddChannelsToBouquet(ctx, db.AddChannelsToBouquetParams{BouquetID: sports.ID, ChannelIds: []int64{ids["ESPN"]}})

	listed := func(path string) []string {
		t.Helper()
		_, body := b.get(path)
		var out []string
		for _, n := range []string{"CNN", "ESPN", "Loose"} {
			if strings.Contains(body, ">"+n+"</a>") {
				out = append(out, n)
			}
		}
		return out
	}
	for path, want := range map[string]string{
		"/admin/channels": "CNN ESPN Loose",
		"/admin/channels?bouquet=" + strconv.FormatInt(basic.ID, 10):  "CNN ESPN",
		"/admin/channels?bouquet=" + strconv.FormatInt(sports.ID, 10): "ESPN",
		"/admin/channels?bouquet=-1":                                  "Loose",
		"/admin/channels?bouquet=-1&q=cnn":                            "",
	} {
		if got := strings.Join(listed(path), " "); got != want {
			t.Errorf("%s lists %q, want %q", path, got, want)
		}
	}
	_, body := b.get("/admin/channels?bouquet=-1")
	if !strings.Contains(body, `<option value="-1" selected>In no bouquet`) || !strings.Contains(body, `name="bouquet" value="-1"`) {
		t.Error("the bouquet filter is not kept on the page and the bulk form")
	}
	// Moving channels returns to the same filtered list.
	resp, _ := b.post("/admin/channels/group", url.Values{"to": {"none"}, "bouquet": {"-1"}, "channel": {strconv.FormatInt(ids["Loose"], 10)}}, b.base)
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "bouquet=-1") {
		t.Errorf("after a bulk move: %s, want the bouquet filter kept", loc)
	}
}
