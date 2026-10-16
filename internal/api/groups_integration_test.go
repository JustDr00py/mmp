//go:build integration

package api

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/JustDr00py/mmp/internal/db"
)

func TestProviderGroupsAPI(t *testing.T) {
	env := newAPITestEnv(t)
	q := env.Store.(*db.Queries)
	h := apiServer(env)
	ctx := context.Background()
	prov, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: "P", Type: "m3u", Url: "http://p/", MaxConnections: 1, RefreshIntervalSecs: 3600, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	for i, g := range []string{"US", "US", "UK"} {
		if _, err := q.UpsertProviderStream(ctx, db.UpsertProviderStreamParams{ProviderID: prov.ID, ExternalID: strconv.Itoa(i), Name: "S" + strconv.Itoa(i),
			GroupTitle: g, StreamUrl: "http://p/" + strconv.Itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}
	base := "/api/v1/providers/" + strconv.FormatInt(prov.ID, 10)
	groups := func(obj map[string]any) map[string]map[string]any {
		out := map[string]map[string]any{}
		for _, g := range obj["groups"].([]any) {
			m := g.(map[string]any)
			out[m["name"].(string)] = m
		}
		return out
	}
	code, obj, _ := call(t, h, http.MethodGet, base+"/groups", nil)
	if code != 200 || obj["filter"] != false || groups(obj)["US"]["streams"] != float64(2) || groups(obj)["UK"]["selected"] != false {
		t.Fatalf("list = %d %v", code, obj)
	}
	code, obj, _ = call(t, h, http.MethodPut, base+"/groups", map[string]any{"filter": true, "groups": []string{"US"}})
	if code != 200 || obj["filter"] != true || groups(obj)["US"]["selected"] != true || groups(obj)["UK"]["selected"] != false {
		t.Fatalf("set = %d %v", code, obj)
	}
	if _, p, _ := call(t, h, http.MethodGet, base, nil); p["group_filter"] != true {
		t.Errorf("provider group_filter = %v", p["group_filter"])
	}
	if rows, _ := q.ListUnassignedStreams(ctx); len(rows) != 2 {
		t.Errorf("unassigned with filter = %d, want the 2 US streams", len(rows))
	}
	// filter off without groups keeps the selection.
	code, obj, _ = call(t, h, http.MethodPut, base+"/groups", map[string]any{"filter": false})
	if code != 200 || obj["filter"] != false || groups(obj)["US"]["selected"] != true {
		t.Errorf("filter off = %d %v", code, obj)
	}
	if code, _, _ := call(t, h, http.MethodPut, "/api/v1/providers/999999/groups", map[string]any{"filter": true}); code != http.StatusNotFound {
		t.Errorf("unknown provider = %d", code)
	}
}
