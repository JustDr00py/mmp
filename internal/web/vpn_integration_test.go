//go:build integration

package web

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/gluetun"
)

func TestVPNPage(t *testing.T) {
	var mu sync.Mutex
	loc := gluetun.Location{Countries: []string{"Netherlands"}}
	g := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("X-API-Key") != "key" {
			http.Error(w, "", http.StatusUnauthorized)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/publicip/ip":
			io.WriteString(w, `{"public_ip":"86.104.23.169","country":"Netherlands","city":"Amsterdam","organization":"AS25369 Hydra"}`)
		case "GET /v1/vpn/status":
			io.WriteString(w, `{"status":"running"}`)
		case "GET /v1/vpn/settings":
			json.NewEncoder(w).Encode(map[string]any{"type": "wireguard",
				"provider": map[string]any{"name": "nordvpn", "server_selection": loc}})
		case "PUT /v1/vpn/settings":
			var v struct {
				Provider struct {
					Sel gluetun.Location `json:"server_selection"`
				} `json:"provider"`
			}
			json.NewDecoder(r.Body).Decode(&v)
			loc = v.Provider.Sel
			io.WriteString(w, "settings updated")
		default:
			http.NotFound(w, r)
		}
	}))
	defer g.Close()
	servers := filepath.Join(t.TempDir(), "servers.json")
	os.WriteFile(servers, []byte(`{"nordvpn":{"servers":[
	 {"vpn":"wireguard","country":"Netherlands","city":"Amsterdam"},
	 {"vpn":"wireguard","country":"Germany","city":"Frankfurt"},
	 {"vpn":"wireguard","country":"Germany","city":"Berlin"}]}}`), 0o644)

	b, _ := pgUI(t, func(d *Deps, _ *pgxpool.Pool) {
		c := &gluetun.Client{BaseURL: g.URL, APIKey: "key"}
		d.VPN = &VPN{Client: c, Servers: &gluetun.ServerList{Path: servers},
			Keeper: &gluetun.Keeper{Client: c, Store: d.Store.(*db.Queries), Log: slog.New(slog.DiscardHandler)}}
	})

	_, body := b.get("/admin/vpn")
	for _, want := range []string{"86.104.23.169", "🇳🇱", "Amsterdam", "Connected", "nordvpn", "WireGuard",
		`<option value="Netherlands" selected>🇳🇱 Netherlands</option>`, `<optgroup label="Germany"><option value="Berlin">`,
		"Set by .env: <strong>Netherlands</strong>", `href="/admin/vpn"`} {
		if !strings.Contains(body, want) {
			t.Errorf("VPN page lacks %q", want)
		}
	}
	if _, frag := b.get("/admin/vpn/status"); !strings.Contains(frag, "86.104.23.169") || strings.Contains(frag, "<html") {
		t.Errorf("status fragment = %q", frag)
	}

	// A city from another country is refused before gluetun is asked.
	resp, body := b.post("/admin/vpn/location", url.Values{"country": {"Netherlands"}, "city": {"Berlin"}}, b.base)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Berlin is not in Netherlands") {
		t.Fatalf("mismatched city = %d", resp.StatusCode)
	}
	// A city alone takes its country.
	resp, _ = b.post("/admin/vpn/location", url.Values{"country": {""}, "city": {"Frankfurt"}}, b.base)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/admin/vpn?msg=location" {
		t.Fatalf("change = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	mu.Lock()
	got := loc
	mu.Unlock()
	if !got.Equal(gluetun.Location{Countries: []string{"Germany"}, Cities: []string{"Frankfurt"}}) {
		t.Fatalf("gluetun location = %+v", got)
	}
	_, body = b.get("/admin/vpn?msg=location")
	if !strings.Contains(body, "Chosen here: <strong>Frankfurt, Germany</strong>") || !strings.Contains(body, "Use the .env location") {
		t.Error("page does not show the chosen location")
	}

	resp, _ = b.post("/admin/vpn/default", nil, b.base)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("default = %d", resp.StatusCode)
	}
	mu.Lock()
	got = loc
	mu.Unlock()
	if !got.Equal(gluetun.Location{Countries: []string{"Netherlands"}}) {
		t.Fatalf("after .env = %+v", got)
	}
}

func TestVPNHiddenWithoutGluetun(t *testing.T) {
	b, _ := pgUI(t)
	if resp, _ := b.get("/admin/vpn"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("VPN page without gluetun = %d", resp.StatusCode)
	}
	if _, body := b.get("/admin/settings"); strings.Contains(body, `href="/admin/vpn"`) {
		t.Error("nav shows VPN without gluetun")
	}
}
