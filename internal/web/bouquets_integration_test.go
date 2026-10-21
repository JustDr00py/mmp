//go:build integration

package web

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/JustDr00py/mmp/internal/db"
)

func TestBouquetPages(t *testing.T) {
	b, q := pgUI(t)
	ctx := context.Background()
	news, _ := q.CreateChannelGroup(ctx, db.CreateChannelGroupParams{Name: "News"})
	sport, _ := q.CreateChannelGroup(ctx, db.CreateChannelGroupParams{Name: "Sport"})
	ch := map[string]int64{}
	for i, c := range []struct {
		name  string
		group *int64
	}{{"BBC News", &news.ID}, {"CNN", &news.ID}, {"Sky Sports", &sport.ID}, {"ESPN", &sport.ID}, {"Loose Channel", nil}} {
		n := int32(101 + i)
		row, err := q.CreateChannel(ctx, db.CreateChannelParams{Number: &n, Name: c.name, GroupID: c.group})
		if err != nil {
			t.Fatal(err)
		}
		ch[c.name] = row.ID
	}
	members := func(id int64) []string {
		cs, _ := q.ListBouquetChannels(ctx, id)
		var out []string
		for _, c := range cs {
			out = append(out, c.Name)
		}
		slices.Sort(out)
		return out
	}

	_, body := b.get("/admin/bouquets")
	if !strings.Contains(body, "No bouquets yet") || !strings.Contains(body, `aria-current="page">Bouquets`) {
		t.Fatalf("empty list:\n%s", body)
	}
	if resp, body := b.post("/admin/bouquets", url.Values{"name": {""}}, b.base); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Name is required") {
		t.Errorf("empty name = %d", resp.StatusCode)
	}
	resp, _ := b.post("/admin/bouquets", url.Values{"name": {"UK Basic"}, "sort_order": {"2"}}, b.base)
	m := regexp.MustCompile(`^/admin/bouquets/(\d+)\?msg=created$`).FindStringSubmatch(resp.Header.Get("Location"))
	if m == nil {
		t.Fatalf("create = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	bid, _ := strconv.ParseInt(m[1], 10, 64)
	base := "/admin/bouquets/" + m[1]
	if resp, body := b.post("/admin/bouquets", url.Values{"name": {"UK Basic"}}, b.base); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "exists") {
		t.Errorf("duplicate = %d", resp.StatusCode)
	}

	_, body = b.get(base + "?msg=created")
	if !strings.Contains(body, "Bouquet created.") || !strings.Contains(body, "No channels yet.") || strings.Count(body, `name="channel"`) != 5 {
		t.Fatalf("editor (5 candidates):\n%s", body)
	}

	// Add selected (htmx): the editor comes back with members and the
	// remaining candidates; the filter travels along.
	resp, body = b.htmx(http.MethodPost, base+"/add", url.Values{"channel": {strconv.FormatInt(ch["BBC News"], 10), strconv.FormatInt(ch["ESPN"], 10)}, "q": {""}}, "bouquet-editor")
	if resp.StatusCode != 200 || !strings.HasPrefix(strings.TrimSpace(body), `<div id="bouquet-editor"`) || !strings.Contains(body, "Added 2 channels.") {
		t.Fatalf("add = %d\n%s", resp.StatusCode, body)
	}
	if got := members(bid); !slices.Equal(got, []string{"BBC News", "ESPN"}) {
		t.Fatalf("members = %v", got)
	}
	// Re-adding is idempotent.
	_, body = b.htmx(http.MethodPost, base+"/add", url.Values{"channel": {strconv.FormatInt(ch["ESPN"], 10)}}, "bouquet-editor")
	if !strings.Contains(body, "Added 0 channels.") {
		t.Errorf("re-add note:\n%s", body)
	}

	// Filtered candidates exclude members.
	_, body = b.htmx(http.MethodGet, base+"/editor?q=s&group="+strconv.FormatInt(sport.ID, 10), nil, "bouquet-editor")
	cands := body[strings.Index(body, "Add channels"):]
	if !strings.Contains(cands, "Sky Sports") || strings.Contains(cands, "ESPN") || strings.Contains(cands, "CNN") {
		t.Errorf("filtered candidates:\n%s", cands)
	}

	// Whole group, then channels without a group.
	b.htmx(http.MethodPost, base+"/add-group", url.Values{"add_group": {strconv.FormatInt(news.ID, 10)}}, "bouquet-editor")
	_, body = b.htmx(http.MethodPost, base+"/add-group", url.Values{"add_group": {"0"}}, "bouquet-editor")
	if !strings.Contains(body, "Added 1 channel from the group.") {
		t.Errorf("ungrouped note:\n%s", body)
	}
	if got := members(bid); !slices.Equal(got, []string{"BBC News", "CNN", "ESPN", "Loose Channel"}) {
		t.Fatalf("members after groups = %v", got)
	}

	// Remove selected; the plain-form path redirects.
	resp, _ = b.post(base+"/remove", url.Values{"channel": {strconv.FormatInt(ch["CNN"], 10), strconv.FormatInt(ch["Loose Channel"], 10)}}, b.base)
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("remove (no JS) = %d", resp.StatusCode)
	}
	if got := members(bid); !slices.Equal(got, []string{"BBC News", "ESPN"}) {
		t.Fatalf("members after remove = %v", got)
	}

	// Counts on the list, rename, delete.
	_, body = b.get("/admin/bouquets")
	if !strings.Contains(body, ">UK Basic</a>") || !strings.Contains(body, `<td class="num" data-label="Channels">2</td>`) {
		t.Errorf("list counts:\n%s", body)
	}
	if resp, _ := b.post(base, url.Values{"name": {"UK Plus"}, "sort_order": {"1"}}, b.base); resp.Header.Get("Location") != base+"?msg=saved" {
		t.Errorf("rename = %d", resp.StatusCode)
	}
	if bq, _ := q.GetBouquet(ctx, bid); bq.Name != "UK Plus" || bq.SortOrder != 1 {
		t.Errorf("after rename = %+v", bq)
	}
	if resp, _ := b.post(base+"/delete", url.Values{}, b.base); resp.Header.Get("Location") != "/admin/bouquets?msg=deleted" {
		t.Errorf("delete = %d", resp.StatusCode)
	}
	if resp, _ := b.get(base); resp.StatusCode != http.StatusNotFound {
		t.Errorf("deleted bouquet = %d", resp.StatusCode)
	}
}

// TestBouquetClone: a clone gets the source's channels and movie and series
// categories but none of its users, and can then be trimmed on its own.
func TestBouquetClone(t *testing.T) {
	b, q := pgUI(t)
	ctx := context.Background()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	src, err := q.CreateBouquet(ctx, db.CreateBouquetParams{Name: "US Full", SortOrder: 3})
	must(err)
	var chans []int64
	for _, name := range []string{"CNN", "ESPN", "Fox News"} {
		c, err := q.CreateChannel(ctx, db.CreateChannelParams{Name: name})
		must(err)
		chans = append(chans, c.ID)
	}
	_, err = q.AddChannelsToBouquet(ctx, db.AddChannelsToBouquetParams{BouquetID: src.ID, ChannelIds: chans})
	must(err)
	p, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: "Panel", Type: "xtream", Url: "http://panel/", Username: "u",
		Password: "pw", MaxConnections: 1, RefreshIntervalSecs: 3600, Enabled: true})
	must(err)
	must(q.UpsertVODCategories(ctx, db.UpsertVODCategoriesParams{ProviderID: p.ID, Kind: "movie", ExternalIds: []string{"1"}, Names: []string{"Action"}}))
	cats, _ := q.ListVODCategoriesAdmin(ctx, db.ListVODCategoriesAdminParams{Kind: "movie", ProviderID: &p.ID})
	_, err = q.AddVODCategoriesToBouquet(ctx, db.AddVODCategoriesToBouquetParams{BouquetID: src.ID, CategoryIds: []int64{cats[0].ID}})
	must(err)
	u, err := q.CreateUser(ctx, db.CreateUserParams{Username: "alice", PasswordHash: "x", Role: "customer",
		MaxConnections: 1, Enabled: true, StreamToken: "tok-alice"})
	must(err)
	must(q.AddUserBouquets(ctx, db.AddUserBouquetsParams{UserID: u.ID, BouquetIds: []int64{src.ID}}))

	page := "/admin/bouquets/" + strconv.FormatInt(src.ID, 10)
	if _, body := b.get(page); !strings.Contains(body, `value="US Full (copy)"`) {
		t.Errorf("clone form lacks the suggested name")
	}
	for _, name := range []string{"", "US Full"} {
		if resp, body := b.post(page+"/clone", url.Values{"name": {name}}, b.base); resp.StatusCode != http.StatusUnprocessableEntity ||
			(name == "" && !strings.Contains(body, "Name is required")) || (name != "" && !strings.Contains(body, "exists")) {
			t.Errorf("clone named %q = %d", name, resp.StatusCode)
		}
	}
	resp, _ := b.post(page+"/clone", url.Values{"name": {"EU Lite"}}, b.base)
	m := regexp.MustCompile(`^/admin/bouquets/(\d+)\?msg=cloned$`).FindStringSubmatch(resp.Header.Get("Location"))
	if m == nil {
		t.Fatalf("clone = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	id, _ := strconv.ParseInt(m[1], 10, 64)
	cl, err := q.GetBouquet(ctx, id)
	if err != nil || cl.Name != "EU Lite" || cl.SortOrder != 3 {
		t.Errorf("clone = %+v %v", cl, err)
	}
	if cs, _ := q.ListBouquetChannels(ctx, id); len(cs) != 3 {
		t.Errorf("clone channels = %d, want 3", len(cs))
	}
	if vc, _ := q.ListBouquetVODCategories(ctx, id); len(vc) != 1 {
		t.Errorf("clone categories = %d, want 1", len(vc))
	}
	if ids, _ := q.ListUserBouquetIDs(ctx, u.ID); !slices.Equal(ids, []int64{src.ID}) {
		t.Errorf("alice's bouquets = %v, want only the source", ids)
	}
	if _, body := b.get(resp.Header.Get("Location")); !strings.Contains(body, "Bouquet cloned") {
		t.Error("no clone flash")
	}
	// Trimming the clone leaves the source alone.
	_, err = q.RemoveChannelsFromBouquet(ctx, db.RemoveChannelsFromBouquetParams{BouquetID: id, ChannelIds: chans[:1]})
	must(err)
	if cs, _ := q.ListBouquetChannels(ctx, src.ID); len(cs) != 3 {
		t.Errorf("source channels = %d after trimming the clone, want 3", len(cs))
	}
}
