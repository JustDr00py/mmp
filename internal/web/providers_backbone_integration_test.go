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

	// The picker: an edit selects the provider's backbone, and "Add new
	// backbone…" takes the label typed beside it.
	delta := "/admin/providers/" + strconv.FormatInt(ids["Delta TV"], 10) + "/edit"
	if _, body := b.get("/admin/providers/" + strconv.FormatInt(ids["Alpha TV"], 10) + "/edit"); !strings.Contains(body, `<option value="Trex" selected>`) {
		t.Errorf("edit form does not select the backbone")
	}
	form = url.Values{"name": {"Delta TV"}, "type": {"m3u"}, "url": {"http://d/"}, "max_connections": {"1"},
		"refresh_minutes": {"60"}, "priority": {"7"}, "enabled": {"on"}, "backbone": {"+new"}, "backbone_new": {""}}
	if resp, body := b.post(delta, form, b.base); resp.StatusCode != http.StatusUnprocessableEntity ||
		!strings.Contains(body, "Enter a name for the new backbone") || !strings.Contains(body, `<option value="+new" selected>`) {
		t.Errorf("blank new backbone = %d", resp.StatusCode)
	}
	form.Set("backbone_new", strings.Repeat("z", 61))
	if resp, body := b.post(delta, form, b.base); resp.StatusCode != http.StatusUnprocessableEntity ||
		!strings.Contains(body, `<option value="+new" selected>`) || !strings.Contains(body, `value="`+strings.Repeat("z", 61)+`"`) {
		t.Errorf("long new backbone = %d, typed label not kept", resp.StatusCode)
	}
	form.Set("backbone_new", "  Zeta  Net ")
	if resp, body := b.post(delta, form, b.base); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("new backbone = %d\n%s", resp.StatusCode, body)
	}
	if bb := get(ids["Delta TV"]).Backbone; bb != "Zeta Net" {
		t.Errorf("new backbone = %q, want Zeta Net", bb)
	}
	// A new label that only differs in case joins the existing one.
	resp, _ = bulk(url.Values{"action": {"backbone"}, "backbone_to": {"+new"}, "backbone_to_new": {"zeta net"},
		"provider": {strconv.FormatInt(ids["Epsilon TV"], 10)}})
	if resp.StatusCode != http.StatusSeeOther || get(ids["Epsilon TV"]).Backbone != "Zeta Net" {
		t.Errorf("bulk new backbone = %d, %q", resp.StatusCode, get(ids["Epsilon TV"]).Backbone)
	}
	if resp, _ := bulk(url.Values{"action": {"backbone"}, "backbone_to": {"+new"}, "provider": {"1"}}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bulk blank new backbone = %d, want 400", resp.StatusCode)
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

	// Match: Trex has one provider, so 2+ would show nothing there; its
	// suggestions show at its provider count, with a note. On Strong
	// Arena Sports has two. Applying narrows to the backbone.
	_, body := b.get("/admin/match?min=2&backbone=Trex")
	if !strings.Contains(body, "Arena Sports") || !strings.Contains(body, "Trex has 1 provider") {
		t.Errorf("Trex at 2+ providers lacks Arena Sports or the note:\n%s", body)
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

// TestAlignBackbonePriority: the providers page flags a backbone whose
// providers have different priorities; Align gives them its best one and
// re-sorts their channels only when asked.
func TestAlignBackbonePriority(t *testing.T) {
	b, q := pgUI(t)
	ctx := context.Background()
	ids := map[string]int64{}
	streams := map[string]int64{}
	for _, p := range []struct {
		name, backbone string
		prio           int32
	}{{"Near", "Same", 0}, {"Far", "Same", 2}, {"Solo", "", 1}, {"O1", "Other", 3}, {"O2", "Other", 3}} {
		pr, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: p.name, Type: "m3u", Url: "http://" + strings.ToLower(p.name) + "/",
			MaxConnections: 1, RefreshIntervalSecs: 3600, Priority: p.prio, Enabled: true, Backbone: p.backbone})
		if err != nil {
			t.Fatal(err)
		}
		ids[p.name] = pr.ID
		sid, err := q.UpsertProviderStream(ctx, db.UpsertProviderStreamParams{ProviderID: pr.ID, ExternalID: "news", Name: "News",
			StreamUrl: "http://" + strings.ToLower(p.name) + "/news"})
		if err != nil {
			t.Fatal(err)
		}
		streams[p.name] = sid
	}
	c, err := q.CreateChannel(ctx, db.CreateChannelParams{Name: "News"})
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range []string{"Solo", "Far"} {
		if err := q.AddStreamToChannel(ctx, db.AddStreamToChannelParams{ChannelID: c.ID, ProviderStreamID: streams[p], Priority: int32(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	prio := func(name string) int32 {
		p, err := q.GetProvider(ctx, ids[name])
		if err != nil {
			t.Fatal(err)
		}
		return p.Priority
	}
	align := func(resort bool) string {
		t.Helper()
		v := url.Values{"align": {"Same"}, "q": {"x"}}
		if resort {
			v.Set("resort_channels", "on")
		}
		resp, _ := b.post("/admin/providers/align-backbone", v, b.base)
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("align = %d", resp.StatusCode)
		}
		return resp.Header.Get("Location")
	}

	_, body := b.get("/admin/providers")
	if !strings.Contains(body, "<strong>Same</strong>: priorities 0 to 2") || strings.Contains(body, "<strong>Other</strong>: priorities") {
		t.Fatalf("mixed-backbone notice:\n%s", body)
	}

	// Without the checkbox: priorities align, the channel keeps its order.
	if loc := align(false); loc != "/admin/providers?msg=aligned&n=1&p=0&q=x" {
		t.Errorf("align redirect = %q", loc)
	}
	if prio("Far") != 0 || prio("Near") != 0 || prio("O1") != 3 || prio("Solo") != 1 {
		t.Errorf("priorities after align: Far=%d Near=%d O1=%d Solo=%d", prio("Far"), prio("Near"), prio("O1"), prio("Solo"))
	}
	if got := order(t, q, c.ID); !eq(got, []int64{streams["Solo"], streams["Far"]}) {
		t.Errorf("align without re-sort moved streams: %v", got)
	}
	if _, body := b.get("/admin/providers?msg=aligned&n=1&p=0"); !strings.Contains(body, "Gave 1 provider their backbone&#39;s best priority, 0.") ||
		strings.Contains(body, "Backbones with mixed priorities") {
		t.Errorf("after align: flash missing or notice still shown")
	}

	// With it: Far (now 0) leads Solo (1).
	if _, err := q.SetProvidersPriority(ctx, db.SetProvidersPriorityParams{Priority: 2, Ids: []int64{ids["Far"]}}); err != nil {
		t.Fatal(err)
	}
	align(true)
	if got := order(t, q, c.ID); !eq(got, []int64{streams["Far"], streams["Solo"]}) {
		t.Errorf("align with re-sort = %v, want Far first", got)
	}
	// Nothing left to change.
	if loc := align(true); !strings.Contains(loc, "n=0") {
		t.Errorf("second align = %q", loc)
	} else if _, body := b.get(loc); !strings.Contains(body, "already share one priority") {
		t.Errorf("second align flash missing")
	}
	if resp, _ := b.post("/admin/providers/align-backbone", url.Values{"align": {" "}}, b.base); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("blank backbone = %d", resp.StatusCode)
	}
}
