package server

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestRealIP(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.89.0.0/24"), netip.MustParsePrefix("fd00::/8")}
	tests := []struct {
		name, remote string
		xff          []string
		xRealIP      string
		want         string
	}{
		{"caddy forwards the client", "10.89.0.2:5000", []string{"203.0.113.7"}, "", "203.0.113.7:5000"},
		{"client-supplied entries left of the proxy's are ignored", "10.89.0.2:5000", []string{"1.2.3.4, 203.0.113.7"}, "", "203.0.113.7:5000"},
		{"a chain of trusted proxies is skipped", "10.89.0.2:5000", []string{"203.0.113.7, 10.89.0.9"}, "", "203.0.113.7:5000"},
		{"several header lines", "10.89.0.2:5000", []string{"198.51.100.1", "203.0.113.7"}, "", "203.0.113.7:5000"},
		{"all hops trusted: the first", "10.89.0.2:5000", []string{"10.89.0.5, 10.89.0.9"}, "", "10.89.0.5:5000"},
		{"X-Real-IP without X-Forwarded-For", "10.89.0.2:5000", nil, "203.0.113.8", "203.0.113.8:5000"},
		{"IPv6 client", "[fd00::1]:443", []string{"2001:db8::5"}, "", "[2001:db8::5]:443"},
		{"IPv4-mapped peer is trusted", "[::ffff:10.89.0.2]:5000", []string{"203.0.113.7"}, "", "203.0.113.7:5000"},
		{"untrusted peer: headers ignored", "198.51.100.9:6000", []string{"203.0.113.7"}, "203.0.113.8", "198.51.100.9:6000"},
		{"garbled chain: left alone", "10.89.0.2:5000", []string{"203.0.113.7, not-an-ip"}, "", "10.89.0.2:5000"},
		{"trusted peer without headers: left alone", "10.89.0.2:5000", nil, "", "10.89.0.2:5000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			h := RealIP(trusted)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.RemoteAddr }))
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tt.remote
			for _, v := range tt.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if tt.xRealIP != "" {
				r.Header.Set("X-Real-IP", tt.xRealIP)
			}
			h.ServeHTTP(httptest.NewRecorder(), r)
			if got != tt.want {
				t.Errorf("RemoteAddr = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRealIPOffByDefault(t *testing.T) {
	var got string
	h := RealIP(nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.RemoteAddr }))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.89.0.2:5000"
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	h.ServeHTTP(httptest.NewRecorder(), r)
	if got != "10.89.0.2:5000" {
		t.Errorf("with no trusted proxies RemoteAddr = %q, want the peer", got)
	}
}

// TestServerUsesRealIP: with a trusted proxy configured, handlers and the
// request log both see the client, not the proxy.
func TestServerUsesRealIP(t *testing.T) {
	var buf bytes.Buffer
	cfg := testCfg()
	cfg.HTTP.TrustedProxies = []string{"10.89.0.0/24"}
	s := New(cfg, slog.New(slog.NewTextHandler(&buf, nil)), nil)
	var seen string
	s.Mux().Get("/x", func(_ http.ResponseWriter, r *http.Request) { seen = r.RemoteAddr })
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "10.89.0.2:41000"
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	s.Mux().ServeHTTP(httptest.NewRecorder(), req)
	if seen != "203.0.113.7:41000" {
		t.Errorf("handler saw %q", seen)
	}
	if !strings.Contains(buf.String(), "remote=203.0.113.7:41000") {
		t.Errorf("request log: %s", buf.String())
	}
}
