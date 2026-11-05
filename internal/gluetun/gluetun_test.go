package gluetun

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/JustDr00py/mmp/internal/db"
)

func TestFlag(t *testing.T) {
	for name, want := range map[string]string{
		"Netherlands": "🇳🇱", "United States": "🇺🇸", "USA": "🇺🇸", "UK": "🇬🇧", "Korea (South)": "🇰🇷",
		"India (via Singapore)": "🇮🇳", "Hong Kong, SAR China": "🇭🇰", "Antiguaand Barbuda": "🇦🇬",
		"Czech Republic": "🇨🇿", "NL": "🇳🇱", "Atlantis": "", "": "",
	} {
		if got := Flag(name); got != want {
			t.Errorf("Flag(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestLocation(t *testing.T) {
	a := Location{Countries: []string{"Netherlands", "Germany"}}
	b := Location{Countries: []string{"germany", "NETHERLANDS"}, Cities: []string{}}
	if !a.Equal(b) {
		t.Error("same countries in another case and order should be equal")
	}
	if a.Equal(Location{Countries: []string{"Netherlands"}}) {
		t.Error("different lists equal")
	}
	if got := (Location{Countries: []string{"Netherlands"}, Cities: []string{"Amsterdam"}}).String(); got != "Amsterdam, Netherlands" {
		t.Errorf("String = %q", got)
	}
	if got := (Location{}).String(); got != "any server" {
		t.Errorf("String = %q", got)
	}
	p := PublicIP{City: "Amsterdam", Region: "North Holland", Country: "Netherlands"}
	if p.Place() != "Amsterdam, North Holland, Netherlands" {
		t.Errorf("Place = %q", p.Place())
	}
	if (PublicIP{City: "Singapore", Region: "Singapore", Country: "Singapore"}).Place() != "Singapore" {
		t.Error("repeated parts kept")
	}
}

func TestServerList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "servers.json")
	data := `{"version":1,"nordvpn":{"version":1,"servers":[
	 {"vpn":"wireguard","country":"Netherlands","region":"Europe","city":"Amsterdam","hostname":"nl1"},
	 {"vpn":"wireguard","country":"Netherlands","region":"Europe","city":"Amsterdam","hostname":"nl2"},
	 {"vpn":"openvpn","country":"Germany","region":"Europe","city":"Berlin","hostname":"de1"},
	 {"vpn":"wireguard","country":"Germany","region":"Europe","city":"Frankfurt","hostname":"de2"}]},
	 "private internet access":{"servers":[{"region":"AU Adelaide"},{"region":"CA Toronto"}]}}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	l := &ServerList{Path: path}
	c, err := l.Choices("NordVPN", "wireguard")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Countries) != 2 || c.Countries[0].Name != "Germany" || c.Countries[1].Servers != 2 || c.Countries[1].Flag != "🇳🇱" {
		t.Errorf("countries = %+v", c.Countries)
	}
	if len(c.Regions) != 0 {
		t.Errorf("continents offered as regions: %v", c.Regions)
	}
	if len(c.Cities) != 2 || c.Cities[0].Name != "Frankfurt" || c.Cities[1].Name != "Amsterdam" {
		t.Errorf("cities = %+v (Berlin is OpenVPN only)", c.Cities)
	}
	pia, _ := l.Choices("private internet access", "openvpn")
	if len(pia.Regions) != 2 || len(pia.Countries) != 0 {
		t.Errorf("pia = %+v", pia)
	}
	none, err := (&ServerList{Path: filepath.Join(t.TempDir(), "missing.json")}).Choices("nordvpn", "wireguard")
	if err != nil || !none.Empty() {
		t.Errorf("missing file: %+v %v", none, err)
	}
}

// fakeGluetun is a control server holding a location.
type fakeGluetun struct {
	mu   sync.Mutex
	loc  Location
	puts []string
	key  string
}

func (f *fakeGluetun) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-API-Key") != f.key {
		http.Error(w, "", http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method + " " + r.URL.Path {
	case "GET /v1/vpn/settings":
		json.NewEncoder(w).Encode(map[string]any{"type": "wireguard", "provider": map[string]any{
			"name": "nordvpn", "server_selection": f.loc}, "wireguard": map[string]any{"private_key": "secret"}})
	case "PUT /v1/vpn/settings":
		b, _ := io.ReadAll(r.Body)
		f.puts = append(f.puts, string(b))
		var v struct {
			Provider struct {
				Sel Location `json:"server_selection"`
			} `json:"provider"`
		}
		json.Unmarshal(b, &v)
		if len(v.Provider.Sel.Countries) > 0 && v.Provider.Sel.Countries[0] == "Atlantis" {
			http.Error(w, "the country specified is not valid: Atlantis", http.StatusBadRequest)
			return
		}
		f.loc = v.Provider.Sel
		io.WriteString(w, "settings updated")
	case "GET /v1/publicip/ip":
		io.WriteString(w, `{"public_ip":"1.2.3.4","country":"Netherlands","city":"Amsterdam"}`)
	case "GET /v1/vpn/status":
		io.WriteString(w, `{"status":"running"}`)
	case "PUT /v1/vpn/status":
		f.puts = append(f.puts, "status")
		io.WriteString(w, `{"outcome":"ok"}`)
	default:
		http.NotFound(w, r)
	}
}

type memStore struct{ m map[string][]byte }

func (s *memStore) GetSetting(_ context.Context, k string) ([]byte, error) {
	if v, ok := s.m[k]; ok {
		return v, nil
	}
	return nil, pgx.ErrNoRows
}

func (s *memStore) SetSetting(_ context.Context, a db.SetSettingParams) error {
	s.m[a.Key] = a.Value
	return nil
}

func TestKeeper(t *testing.T) {
	ctx := context.Background()
	f := &fakeGluetun{key: "k", loc: Location{Countries: []string{"Netherlands"}, Hostnames: []string{"nl1"}}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, APIKey: "k"}
	k := &Keeper{Client: c, Store: &memStore{m: map[string][]byte{}}}

	if ip, err := c.PublicIP(ctx); err != nil || ip.IP != "1.2.3.4" || ip.Flag() != "🇳🇱" {
		t.Fatalf("PublicIP = %+v, %v", ip, err)
	}
	if _, err := (&Client{BaseURL: srv.URL}).Settings(ctx); err != ErrUnauthorized {
		t.Fatalf("no key: %v", err)
	}
	if _, err := k.Set(ctx, Location{Countries: []string{"Atlantis"}}); err == nil || !strings.Contains(err.Error(), "Atlantis") {
		t.Fatalf("invalid country: %v", err)
	}
	if s, _ := k.Load(ctx); s.Active {
		t.Fatal("a refused location was saved")
	}

	want := Location{Countries: []string{"Germany"}, Cities: []string{"Frankfurt"}}
	if _, err := k.Set(ctx, want); err != nil {
		t.Fatal(err)
	}
	if !f.loc.Equal(want) {
		t.Fatalf("gluetun has %+v (the .env hostname must be cleared)", f.loc)
	}
	if !strings.Contains(f.puts[len(f.puts)-1], `"hostnames":[]`) {
		t.Errorf("empty filters must be sent as [] to clear them: %s", f.puts[len(f.puts)-1])
	}

	// Nothing to do while gluetun has it; set again after a restart.
	if applied, err := k.Sync(ctx); err != nil || applied {
		t.Fatalf("Sync = %v, %v", applied, err)
	}
	f.loc = Location{Countries: []string{"Netherlands"}, Hostnames: []string{"nl1"}} // restarted with .env
	if applied, err := k.Sync(ctx); err != nil || !applied || !f.loc.Equal(want) {
		t.Fatalf("Sync after restart = %v, %v, %+v", applied, err, f.loc)
	}

	// Back to .env: the location gluetun had before MMP changed it.
	if _, err := k.UseDefault(ctx); err != nil {
		t.Fatal(err)
	}
	if !f.loc.Equal(Location{Countries: []string{"Netherlands"}, Hostnames: []string{"nl1"}}) {
		t.Fatalf("default not restored: %+v", f.loc)
	}
	f.loc = Location{Countries: []string{"Sweden"}}
	if applied, _ := k.Sync(ctx); applied {
		t.Fatal("Sync must leave gluetun alone once .env is in charge")
	}

	n := len(f.puts)
	if err := c.Reconnect(ctx); err != nil || len(f.puts) != n+2 {
		t.Fatalf("Reconnect: %v, %d puts", err, len(f.puts)-n)
	}
}
