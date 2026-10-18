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
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JustDr00py/mmp/internal/db"
)

func TestProviderBackbones(t *testing.T) {
	ref := &fakeRefresher{}
	b, q := pgUI(t, func(d *Deps, _ *pgxpool.Pool) { d.Refresh = ref })
	ctx := context.Background()
	ids := map[string]int64{}
	for _, p := range []struct{ name, backbone string }{
		{"Alpha TV", "Strong"}, {"Beta TV", "Strong"}, {"Gamma TV", "Trex"}, {"Delta TV", ""},
	} {
		pr, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: p.name, Type: "m3u", Url: "http://" + strings.ToLower(p.name[:1]) + "/",
			MaxConnections: 1, RefreshIntervalSecs: 3600, Enabled: true, Backbone: p.backbone})
		if err != nil {
			t.Fatal(err)
		}
		ids[p.name] = pr.ID
	}
	get := func(id int64) db.Provider {
		p, err := q.GetProvider(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	// The form spells a label like the one in use and offers the labels.
	resp, body := b.get("/admin/providers/new")
	if resp.StatusCode != 200 || !strings.Contains(body, `<option value="Strong">`) || !strings.Contains(body, `<option value="Trex">`) {
		t.Fatalf("new form = %d, no backbone suggestions:\n%s", resp.StatusCode, body)
	}
	form := url.Values{"name": {"Epsilon TV"}, "type": {"m3u"}, "url": {"http://e/"}, "max_connections": {"1"},
		"refresh_minutes": {"60"}, "priority": {"0"}, "enabled": {"on"}, "backbone": {"  strong "}}
	if resp, body := b.post("/admin/providers/new", form, b.base); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create = %d\n%s", resp.StatusCode, body)
	}
	ps, _ := q.ListProviders(ctx)
	for _, p := range ps {
		if p.Name == "Epsilon TV" {
			ids[p.Name] = p.ID
			if p.Backbone != "Strong" {
				t.Errorf("created backbone = %q, want Strong", p.Backbone)
			}
		}
	}
	form.Set("backbone", strings.Repeat("x", 61))
	if resp, body := b.post("/admin/providers/new", form, b.base); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "at most 60") {
		t.Errorf("long backbone = %d", resp.StatusCode)
	}

	// Filtering by backbone, no backbone, search and status.
	names := func(path string) []string {
		t.Helper()
		resp, body := b.get(path)
		if resp.StatusCode != 200 {
			t.Fatalf("%s = %d", path, resp.StatusCode)
		}
		var out []string
		for _, n := range []string{"Alpha TV", "Beta TV", "Gamma TV", "Delta TV", "Epsilon TV"} {
			if strings.Contains(body, ">"+n+"</a>") {
				out = append(out, n)
			}
		}
		return out
	}
	for path, want := range map[string][]string{
		"/admin/providers":                      {"Alpha TV", "Beta TV", "Gamma TV", "Delta TV", "Epsilon TV"},
		"/admin/providers?backbone=Strong":      {"Alpha TV", "Beta TV", "Epsilon TV"},
		"/admin/providers?backbone=-":           {"Delta TV"},
		"/admin/providers?q=trex":               {"Gamma TV"},
		"/admin/providers?q=tv+alpha":           {"Alpha TV"},
		"/admin/providers?backbone=Nope":        nil,
		"/admin/providers?backbone=Strong&q=be": {"Beta TV"},
	} {
		if got := names(path); !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", path, got, want)
		}
	}
	if _, body := b.get("/admin/providers"); !strings.Contains(body, "Strong (3)") || !strings.Contains(body, "No backbone (1)") {
		t.Errorf("backbone options missing counts")
	}

	bulk := func(v url.Values) (*http.Response, string) {
		t.Helper()
		return b.post("/admin/providers/bulk", v, b.base)
	}
	// Selected providers get a backbone; a filtered list follows them.
	resp, _ = bulk(url.Values{"action": {"backbone"}, "backbone_to": {"trex"}, "backbone": {"Strong"},
		"provider": {strconv.FormatInt(ids["Alpha TV"], 10), strconv.FormatInt(ids["Beta TV"], 10), "x"}})
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || !strings.Contains(loc, "backbone=Trex") || !strings.Contains(loc, "n=2") {
		t.Fatalf("bulk backbone = %d %s", resp.StatusCode, loc)
	}
	if get(ids["Alpha TV"]).Backbone != "Trex" || get(ids["Beta TV"]).Backbone != "Trex" || get(ids["Epsilon TV"]).Backbone != "Strong" {
		t.Errorf("backbones after bulk: %q %q %q", get(ids["Alpha TV"]).Backbone, get(ids["Beta TV"]).Backbone, get(ids["Epsilon TV"]).Backbone)
	}
	if _, body := b.get(resp.Header.Get("Location")); !strings.Contains(body, "Set the backbone of 2 providers.") {
		t.Errorf("no bulk flash")
	}
	// scope=all applies to every provider passing the posted filter.
	resp, _ = bulk(url.Values{"action": {"disable"}, "scope": {"all"}, "backbone": {"Trex"}})
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "n=3") {
		t.Fatalf("disable all = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if get(ids["Gamma TV"]).Enabled || get(ids["Alpha TV"]).Enabled || !get(ids["Delta TV"]).Enabled {
		t.Errorf("disable all Trex touched the wrong providers")
	}
	if got := names("/admin/providers?status=disabled"); !slices.Equal(got, []string{"Alpha TV", "Beta TV", "Gamma TV"}) {
		t.Errorf("disabled filter: %v", got)
	}
	resp, _ = bulk(url.Values{"action": {"priority"}, "priority_to": {"7"}, "provider": {strconv.FormatInt(ids["Delta TV"], 10)}})
	if resp.StatusCode != http.StatusSeeOther || get(ids["Delta TV"]).Priority != 7 {
		t.Errorf("priority = %d, %d", resp.StatusCode, get(ids["Delta TV"]).Priority)
	}
	// A blank label clears it.
	bulk(url.Values{"action": {"backbone"}, "backbone_to": {""}, "provider": {strconv.FormatInt(ids["Epsilon TV"], 10)}})
	if bb := get(ids["Epsilon TV"]).Backbone; bb != "" {
		t.Errorf("cleared backbone = %q", bb)
	}

	for _, bad := range []url.Values{
		{"action": {"explode"}, "provider": {"1"}},
		{"action": {"priority"}, "priority_to": {"high"}, "provider": {strconv.FormatInt(ids["Delta TV"], 10)}},
		{"action": {"backbone"}, "backbone_to": {strings.Repeat("y", 61)}, "provider": {strconv.FormatInt(ids["Delta TV"], 10)}},
	} {
		if resp, _ := bulk(bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%v = %d, want 400", bad, resp.StatusCode)
		}
	}
	if resp, _ := bulk(url.Values{"action": {"enable"}}); !strings.Contains(resp.Header.Get("Location"), "msg=none") {
		t.Errorf("nothing selected = %s", resp.Header.Get("Location"))
	}

	// Refresh runs in the background, one provider at a time.
	resp, _ = bulk(url.Values{"action": {"refresh"}, "scope": {"all"}, "backbone": {"Trex"}})
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "act=refresh") {
		t.Fatalf("bulk refresh = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		ref.mu.Lock()
		n := len(ref.calls)
		got := slices.Sorted(slices.Values(ref.calls))
		ref.mu.Unlock()
		if n == 3 {
			want := slices.Sorted(slices.Values([]int64{ids["Alpha TV"], ids["Beta TV"], ids["Gamma TV"]}))
			if !slices.Equal(got, want) {
				t.Errorf("refreshed %v, want %v", got, want)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("refreshed %d providers, want 3", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestBackbonePickerAndMatch(t *testing.T) {
	b, q := pgUI(t)
	ctx := context.Background()
	for _, p := range []struct{ name, backbone string }{{"A", "Strong"}, {"B", "Strong"}, {"C", "Trex"}} {
		pr, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: p.name, Type: "m3u", Url: "http://" + p.name + "/",
			MaxConnections: 1, RefreshIntervalSecs: 3600, Enabled: true, Backbone: p.backbone})
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{"Arena Sports", "Only " + p.name} {
			_, err := q.UpsertProviderStream(ctx, db.UpsertProviderStreamParams{ProviderID: pr.ID, ExternalID: n, Name: n,
				StreamUrl: "http://" + p.name + "/" + n})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	ch, err := q.CreateChannel(ctx, db.CreateChannelParams{Name: "Arena"})
	if err != nil {
		t.Fatal(err)
	}
	picker := func(backbone string) string {
		_, body := b.get("/admin/channels/" + strconv.FormatInt(ch.ID, 10) + "/picker?q=arena&backbone=" + url.QueryEscape(backbone))
		return body
	}
	if body := picker(""); strings.Count(body, "<li>") != 3 || !strings.Contains(body, "A (Strong)") {
		t.Errorf("picker, any backbone:\n%s", body)
	}
	if body := picker("Trex"); strings.Count(body, "<li>") != 1 || !strings.Contains(body, "C (Trex)") {
		t.Errorf("picker, Trex:\n%s", body)
	}
	if body := picker("-"); strings.Contains(body, "<li>") {
		t.Errorf("picker, no backbone:\n%s", body)
	}
	if _, body := b.get("/admin/channels/" + strconv.FormatInt(ch.ID, 10)); !strings.Contains(body, `<option value="Trex">Trex</option>`) {
		t.Errorf("channel page has no backbone filter")
	}

	// Match: on Trex, Arena Sports has one provider, so it needs "any
	// provider"; on Strong it has two. Applying narrows to the backbone.
	_, body := b.get("/admin/match?min=2&backbone=Trex")
	if strings.Contains(body, "Arena Sports") {
		t.Errorf("Trex at 2+ providers shows Arena Sports")
	}
	_, body = b.get("/admin/match?min=2&backbone=Strong")
	if !strings.Contains(body, "Arena Sports") || !strings.Contains(body, "Strong (2)") {
		t.Fatalf("Strong at 2+ providers lacks Arena Sports:\n%s", body)
	}
	resp, _ := b.post("/admin/match/apply", url.Values{"scope": {"all"}, "min": {"2"}, "backbone": {"Strong"}, "cg": {"none"}}, b.base)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("apply = %d", resp.StatusCode)
	}
	keys, _ := q.ListChannelMatchKeys(ctx)
	var arena int64
	for _, k := range keys {
		if k.Name == "Arena Sports" {
			arena = k.ID
		}
	}
	cs, err := q.ListChannelStreams(ctx, arena)
	if err != nil || arena == 0 {
		t.Fatalf("no Arena Sports channel (%v)", err)
	}
	var provs []string
	for _, s := range cs {
		provs = append(provs, s.ProviderName)
	}
	slices.Sort(provs)
	if !slices.Equal(provs, []string{"A", "B"}) {
		t.Errorf("Arena Sports got streams from %v, want A and B", provs)
	}
}
