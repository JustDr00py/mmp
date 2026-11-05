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
	"github.com/JustDr00py/mmp/internal/source"
)

func TestProviderVPNURL(t *testing.T) {
	ref := &fakeRefresher{}
	b, q := pgUI(t, func(d *Deps, _ *pgxpool.Pool) { d.Refresh = ref })
	ctx := context.Background()
	defer source.SetBehindVPN(false)
	source.SetBehindVPN(true)

	p, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: "Alpha", Type: "xtream", Url: "http://panel.example",
		MaxConnections: 1, RefreshIntervalSecs: 3600, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	edit := "/admin/providers/" + strconv.FormatInt(p.ID, 10) + "/edit"
	if _, body := b.get(edit); !strings.Contains(body, `name="vpn_url"`) || !strings.Contains(body, "(it does now)") {
		t.Fatal("edit form lacks the VPN URL field")
	}
	form := url.Values{"name": {"Alpha"}, "type": {"xtream"}, "url": {"http://panel.example"}, "max_connections": {"1"},
		"refresh_minutes": {"60"}, "priority": {"0"}, "enabled": {"on"}, "vpn_url": {"not a url"}}
	if resp, body := b.post(edit, form, b.base); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "VPN URL must be") {
		t.Fatalf("bad vpn url = %d", resp.StatusCode)
	}
	form.Set("vpn_url", " http://vpn.panel.example ")
	if resp, body := b.post(edit, form, b.base); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save = %d\n%s", resp.StatusCode, body)
	}
	got, _ := q.GetProvider(ctx, p.ID)
	if got.VpnUrl != "http://vpn.panel.example" || source.ProviderURL(got) != got.VpnUrl {
		t.Fatalf("saved vpn url = %q", got.VpnUrl)
	}
	// The address MMP uses changed: refreshed in the background.
	refreshed := func() bool {
		ref.mu.Lock()
		defer ref.mu.Unlock()
		return slices.Contains(ref.calls, p.ID)
	}
	for i := 0; i < 50 && !refreshed(); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if !refreshed() {
		t.Error("no refresh after the VPN URL was added")
	}
}
