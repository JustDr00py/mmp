//go:build integration

package web

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/JustDr00py/mmp/internal/db"
)

func TestProviderClone(t *testing.T) {
	b, q := pgUI(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	src, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: "Panel A", Type: "xtream", Url: "http://panel.example",
		Username: "alice", Password: "pw-a", EpgUrl: "http://panel.example/xmltv.php?username=alice&password=pw-a",
		MaxConnections: 2, UserAgent: "UA/1", RefreshIntervalSecs: 7200, Priority: 3, Enabled: true, Backbone: "Net",
		GroupFilter: true, VpnUrl: "http://vpn.panel.example"})
	must(err)
	must(q.AddProviderGroups(ctx, db.AddProviderGroupsParams{ProviderID: src.ID, GroupTitles: []string{"US | News", "US | Sports"}}))
	must(q.SetProviderVODEnabled(ctx, db.SetProviderVODEnabledParams{ID: src.ID, VodEnabled: true}))
	must(q.SetProviderVODCategoryFilter(ctx, db.SetProviderVODCategoryFilterParams{ID: src.ID, VodCategoryFilter: true}))
	must(q.UpsertVODCategories(ctx, db.UpsertVODCategoriesParams{ProviderID: src.ID, Kind: "movie",
		ExternalIds: []string{"1", "2"}, Names: []string{"EN | Action", "|FR| Films"}}))
	must(q.UpsertVODCategories(ctx, db.UpsertVODCategoriesParams{ProviderID: src.ID, Kind: "series",
		ExternalIds: []string{"5"}, Names: []string{"EN | Drama"}}))
	ids := map[string]int64{}
	for _, kind := range []string{"movie", "series"} {
		cats, _ := q.ListVODCategoriesAdmin(ctx, db.ListVODCategoriesAdminParams{Kind: kind})
		for _, c := range cats {
			ids[c.Name] = c.ID
		}
	}
	must(q.SetVODCategorySelection(ctx, db.SetVODCategorySelectionParams{ProviderID: src.ID, Ids: []int64{ids["EN | Action"], ids["EN | Drama"]}}))
	bq, _ := q.CreateBouquet(ctx, db.CreateBouquetParams{Name: "Basic"})
	_, err = q.AddVODCategoriesToBouquet(ctx, db.AddVODCategoriesToBouquetParams{BouquetID: bq.ID, CategoryIds: []int64{ids["EN | Action"]}})
	must(err)

	page := "/admin/providers/" + strconv.FormatInt(src.ID, 10) + "/clone"
	if _, body := b.get("/admin/providers"); !strings.Contains(body, `href="`+page+`"`) {
		t.Error("the providers list lacks the clone link")
	}
	_, body := b.get(page)
	for _, want := range []string{"Clone Panel A", `value="Panel A (copy)"`, "2 chosen", "2 of 3 categories", "in 1 bouquet"} {
		if !strings.Contains(body, want) {
			t.Errorf("clone page lacks %q", want)
		}
	}

	for _, tt := range []struct {
		form url.Values
		want string
	}{
		{url.Values{"name": {"B"}, "username": {"alice"}, "password": {"x"}}, "own login"},
		{url.Values{"name": {"B"}, "username": {"bob"}}, "needs a username and a password"},
		{url.Values{"name": {""}, "username": {"bob"}, "password": {"x"}}, "Name is required"},
		{url.Values{"name": {"B"}, "username": {"bob"}, "password": {"x"}, "url": {"not a url"}}, "URL"},
	} {
		if resp, body := b.post(page, tt.form, b.base); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, tt.want) {
			t.Errorf("%v = %d, want 422 with %q", tt.form, resp.StatusCode, tt.want)
		}
	}

	resp, _ := b.post(page, url.Values{"name": {"Panel B"}, "username": {"bob"}, "password": {"pw-b"}}, b.base)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/admin/providers?msg=cloned" {
		t.Fatalf("clone = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	all, _ := q.ListProviders(ctx)
	i := slices.IndexFunc(all, func(p db.Provider) bool { return p.Name == "Panel B" })
	if i < 0 {
		t.Fatal("no clone")
	}
	c := all[i]
	if c.ID == src.ID || c.Type != "xtream" || c.Url != src.Url || c.Username != "bob" || c.Password != "pw-b" ||
		c.Backbone != "Net" || c.Priority != 3 || c.MaxConnections != 2 || c.UserAgent != "UA/1" || c.RefreshIntervalSecs != 7200 ||
		c.VpnUrl != src.VpnUrl || !c.Enabled || !c.VodEnabled || !c.GroupFilter || !c.VodCategoryFilter || c.LastRefreshedAt != nil {
		t.Errorf("clone = %+v", c)
	}
	if eu, _ := url.Parse(c.EpgUrl); eu.Query().Get("username") != "bob" || eu.Query().Get("password") != "pw-b" {
		t.Errorf("clone's guide URL = %q, want the clone's login", c.EpgUrl)
	}
	s, err := q.ProviderCloneSummary(ctx, c.ID)
	must(err)
	if s.GroupsChosen != 2 || s.Categories != 3 || s.CategoriesChosen != 2 || s.Bouquets != 1 {
		t.Errorf("clone carries %+v, want 2 groups, 2 of 3 categories, 1 bouquet", s)
	}
	sel, _ := q.ListSelectedVODCategories(ctx, c.ID)
	if len(sel) != 2 {
		t.Errorf("clone's selected categories = %+v", sel)
	}
	// The source is untouched.
	if s, _ := q.ProviderCloneSummary(ctx, src.ID); s.GroupsChosen != 2 || s.CategoriesChosen != 2 {
		t.Errorf("source after clone = %+v", s)
	}

	// Another URL: the same username is then another account.
	resp, _ = b.post(page, url.Values{"name": {"Panel C"}, "username": {"alice"}, "password": {"pw-c"}, "url": {"http://other.example"}}, b.base)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("clone with URL = %d", resp.StatusCode)
	}
	all, _ = q.ListProviders(ctx)
	if i := slices.IndexFunc(all, func(p db.Provider) bool { return p.Name == "Panel C" }); i < 0 || all[i].Url != "http://other.example" {
		t.Error("clone with its own URL not saved")
	}
}

func TestSwapCredentials(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"http://p.example/xmltv.php?username=alice&password=pw", "http://p.example/xmltv.php?password=new&username=bob"},
		{"http://guide.example/all.xml.gz", "http://guide.example/all.xml.gz"},
		{"http://p.example/xmltv.php?username=carol&password=pw", "http://p.example/xmltv.php?username=carol&password=pw"},
		{"", ""},
	} {
		if got := swapCredentials(tt.in, "alice", "pw", "bob", "new"); got != tt.want {
			t.Errorf("swapCredentials(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
