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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JustDr00py/mmp/internal/channel"
	"github.com/JustDr00py/mmp/internal/db"
)

func TestSettingsMatch(t *testing.T) {
	var svc *channel.Service
	b, q := pgUI(t, func(d *Deps, _ *pgxpool.Pool) { svc = d.Match.(*channel.Service) })
	ctx := context.Background()
	sports, _ := q.CreateChannelGroup(ctx, db.CreateChannelGroupParams{Name: "Sports"})
	basic, _ := q.CreateBouquet(ctx, db.CreateBouquetParams{Name: "Basic"})
	kids, _ := q.CreateBouquet(ctx, db.CreateBouquetParams{Name: "Kids"})

	// Seeded from the configuration on first start; a second seed (the
	// next start, with .env changed) does not override it.
	if err := svc.SeedMatchSettings(ctx, channel.AutoMatch{Enabled: true, MinProviders: 3, NewChannels: true,
		Group: "sports", Bouquets: []string{"basic", "Nope"}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.SeedMatchSettings(ctx, channel.AutoMatch{MinProviders: 1, Group: "none"}); err != nil {
		t.Fatal(err)
	}
	m, _ := svc.MatchSettings(ctx)
	if want := (channel.MatchSettings{Auto: true, NewChannels: true, MinProviders: 3, Group: channel.GroupFixed,
		GroupID: sports.ID, Bouquets: channel.BouquetsFixed, BouquetIDs: []int64{basic.ID}}); !matchEqual(m, want) {
		t.Fatalf("seeded = %+v, want %+v", m, want)
	}

	_, body := b.get("/admin/settings")
	for _, want := range []string{`name="auto" checked`, `value="3"`, `<option value="` + strconv.FormatInt(sports.ID, 10) + `" selected>`,
		`value="fixed" checked`, `name="bq" value="` + strconv.FormatInt(basic.ID, 10) + `" checked`,
		`name="bq" value="` + strconv.FormatInt(kids.ID, 10) + `">`} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page lacks %q", want)
		}
	}

	resp, _ := b.post("/admin/settings/match", url.Values{"min_providers": {"1"}, "group": {"source"}, "bouquets": {"auto"},
		"bq": {strconv.FormatInt(kids.ID, 10)}}, b.base)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/admin/settings?msg=match" {
		t.Fatalf("save = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	m, _ = svc.MatchSettings(ctx)
	if want := (channel.MatchSettings{MinProviders: 1, Group: channel.GroupSource, Bouquets: channel.BouquetsAuto}); !matchEqual(m, want) {
		t.Fatalf("saved = %+v, want %+v", m, want)
	}
	if _, body := b.get("/admin/settings?msg=match"); !strings.Contains(body, "Matching settings saved.") ||
		strings.Contains(body, `name="auto" checked`) {
		t.Errorf("after save:\n%s", body)
	}

	for _, tt := range []struct {
		form url.Values
		want string
	}{
		{url.Values{"min_providers": {"0"}, "group": {"source"}, "bouquets": {"auto"}}, "minimum providers must be between 1 and 100"},
		{url.Values{"min_providers": {"2"}, "group": {"999999"}, "bouquets": {"auto"}}, "That group no longer exists."},
		{url.Values{"min_providers": {"2"}, "group": {"source"}, "bouquets": {"x"}}, "choose the bouquets of new channels"},
	} {
		if resp, body := b.post("/admin/settings/match", tt.form, b.base); resp.StatusCode != http.StatusUnprocessableEntity ||
			!strings.Contains(body, tt.want) {
			t.Errorf("%v = %d, want error %q", tt.form, resp.StatusCode, tt.want)
		}
	}

	// A chosen group deleted afterwards is reported on the page.
	if err := svc.SaveMatchSettings(ctx, channel.MatchSettings{MinProviders: 2, Group: channel.GroupFixed, GroupID: sports.ID,
		Bouquets: channel.BouquetsNone}); err != nil {
		t.Fatal(err)
	}
	if err := q.DeleteChannelGroup(ctx, sports.ID); err != nil {
		t.Fatal(err)
	}
	if _, body := b.get("/admin/settings"); !strings.Contains(body, "The group chosen before was deleted") {
		t.Error("deleted group not reported")
	}
}

func matchEqual(a, b channel.MatchSettings) bool {
	return a.Auto == b.Auto && a.NewChannels == b.NewChannels && a.MinProviders == b.MinProviders && a.Group == b.Group &&
		a.GroupID == b.GroupID && a.Bouquets == b.Bouquets && slices.Equal(a.BouquetIDs, b.BouquetIDs)
}
