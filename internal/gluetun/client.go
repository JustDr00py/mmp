// Package gluetun talks to gluetun's HTTP control server
// (https://github.com/qdm12/gluetun-wiki/blob/main/setup/advanced/control-server.md)
// when MMP runs behind it (docker-compose.vpn.yml): the public IP the VPN
// gives MMP, the tunnel's status, and the server location, which MMP keeps
// as a setting so it survives gluetun restarts.
package gluetun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client calls one gluetun control server.
type Client struct {
	// BaseURL is the control server, e.g. http://127.0.0.1:8000 (MMP
	// shares gluetun's network, so it is local).
	BaseURL string
	// APIKey is sent as X-API-Key (gluetun's apikey role). Changing the
	// location needs it; gluetun refuses settings requests without a role.
	APIKey string
	HTTP   *http.Client // nil: a client with a 45s timeout
}

// ErrUnauthorized means gluetun refused the API key (or none was set).
var ErrUnauthorized = errors.New("gluetun refused the API key: set the same GLUETUN_API_KEY for gluetun and MMP (see .env.example)")

// PublicIP is where the internet sees MMP coming from.
type PublicIP struct {
	IP           string `json:"public_ip"`
	Country      string `json:"country"`
	Region       string `json:"region"`
	City         string `json:"city"`
	Organization string `json:"organization"`
	Timezone     string `json:"timezone"`
}

// Flag is the country's flag emoji ("" when the country is unknown).
func (p PublicIP) Flag() string { return Flag(p.Country) }

// Place is "City, Region, Country" without the empty or repeated parts.
func (p PublicIP) Place() string {
	var parts []string
	for _, s := range []string{p.City, p.Region, p.Country} {
		if s != "" && (len(parts) == 0 || !strings.EqualFold(parts[len(parts)-1], s)) {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, ", ")
}

// Settings is the part of gluetun's VPN settings MMP reads. Gluetun's
// answer also holds the VPN credentials; they are never decoded.
type Settings struct {
	Type     string // wireguard or openvpn
	Provider string // as gluetun names it, e.g. "nordvpn"
	Location Location
}

// Location is gluetun's server choice. An empty list means any.
type Location struct {
	Countries []string `json:"countries"`
	Regions   []string `json:"regions"`
	Cities    []string `json:"cities"`
	Hostnames []string `json:"hostnames"`
}

// IsZero reports whether no filter is set (any server).
func (l Location) IsZero() bool {
	return len(l.Countries)+len(l.Regions)+len(l.Cities)+len(l.Hostnames) == 0
}

// Equal compares the filters ignoring case and order (gluetun matches
// them case-insensitively).
func (l Location) Equal(o Location) bool {
	return sameSet(l.Countries, o.Countries) && sameSet(l.Regions, o.Regions) &&
		sameSet(l.Cities, o.Cities) && sameSet(l.Hostnames, o.Hostnames)
}

// String describes the location for people: "Amsterdam, Netherlands".
func (l Location) String() string {
	var parts []string
	for _, list := range [][]string{l.Hostnames, l.Cities, l.Regions, l.Countries} {
		if len(list) > 0 {
			parts = append(parts, strings.Join(list, " or "))
		}
	}
	if len(parts) == 0 {
		return "any server"
	}
	return strings.Join(parts, ", ")
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, s := range a {
		seen[strings.ToLower(s)]++
	}
	for _, s := range b {
		k := strings.ToLower(s)
		if seen[k] == 0 {
			return false
		}
		seen[k]--
	}
	return true
}

// PublicIP asks gluetun where MMP's traffic comes out. IP is empty while
// gluetun has not looked it up yet (just after connecting).
func (c *Client) PublicIP(ctx context.Context) (PublicIP, error) {
	var p PublicIP
	err := c.do(ctx, http.MethodGet, "/v1/publicip/ip", nil, &p)
	return p, err
}

// Status is the tunnel's state: running, stopped, starting, stopping or
// crashed.
func (c *Client) Status(ctx context.Context) (string, error) {
	var s struct {
		Status string `json:"status"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/vpn/status", nil, &s)
	return s.Status, err
}

// Settings reads the VPN type, provider and server location gluetun runs
// with now.
func (c *Client) Settings(ctx context.Context) (Settings, error) {
	var raw struct {
		Type     string `json:"type"`
		Provider struct {
			Name      string   `json:"name"`
			Selection Location `json:"server_selection"`
		} `json:"provider"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/vpn/settings", nil, &raw); err != nil {
		return Settings{}, err
	}
	return Settings{Type: raw.Type, Provider: raw.Provider.Name, Location: raw.Provider.Selection}, nil
}

// SetLocation moves the tunnel to a server matching l. Gluetun checks the
// choice against its server list (an unknown country is an error) and
// reconnects if it changed. Every filter is sent, so ones left empty
// clear what .env set (a hostname from SERVER_HOSTNAMES, say).
func (c *Client) SetLocation(ctx context.Context, l Location) (string, error) {
	sel := map[string][]string{"countries": nonNil(l.Countries), "regions": nonNil(l.Regions),
		"cities": nonNil(l.Cities), "hostnames": nonNil(l.Hostnames), "names": {}}
	body, _ := json.Marshal(map[string]any{"provider": map[string]any{"server_selection": sel}})
	var out bytes.Buffer
	err := c.do(ctx, http.MethodPut, "/v1/vpn/settings", body, &out)
	return strings.TrimSpace(out.String()), err
}

// Reconnect stops and restarts the tunnel (gluetun picks a server again
// within the same location).
func (c *Client) Reconnect(ctx context.Context) error {
	for _, s := range []string{"stopped", "running"} {
		body, _ := json.Marshal(map[string]string{"status": s})
		if err := c.do(ctx, http.MethodPut, "/v1/vpn/status", body, nil); err != nil {
			return err
		}
	}
	return nil
}

// nonNil turns nil into [] (gluetun keeps a filter it gets null for).
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// do sends one request; out is a JSON target, a *bytes.Buffer for the raw
// answer, or nil.
func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.APIKey != "" {
		req.Header.Set("X-API-Key", c.APIKey)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 45 * time.Second} // a location change waits for the reconnect
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("gluetun unreachable: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("gluetun: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return ErrUnauthorized
	case resp.StatusCode >= 300:
		msg := strings.TrimSpace(string(data))
		if msg == "" {
			msg = resp.Status
		}
		return fmt.Errorf("gluetun: %s", msg)
	}
	switch o := out.(type) {
	case nil:
	case *bytes.Buffer:
		o.Write(data)
	default:
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("gluetun answer: %w", err)
		}
	}
	return nil
}
