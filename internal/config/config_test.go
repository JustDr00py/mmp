package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mmp.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaults(t *testing.T) {
	// Ensure no stray MMP_* env influences the test.
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Listen != ":8080" {
		t.Errorf("listen = %q, want :8080", c.Listen)
	}
	if c.Proxy.StallTimeout.Duration != 12*time.Second || c.Proxy.HealthWindow.Duration != 15*time.Second {
		t.Errorf("stall_timeout / health_window = %v / %v, want 12s / 15s", c.Proxy.StallTimeout, c.Proxy.HealthWindow)
	}
	if c.Proxy.IdleGrace.Duration != 10*time.Second {
		t.Errorf("idle_grace = %v, want 10s", c.Proxy.IdleGrace)
	}
	if c.Proxy.BufferSizeMB != 32 {
		t.Errorf("buffer_size_mb = %d, want 32", c.Proxy.BufferSizeMB)
	}
	if c.Match.Auto || c.Match.MinProviders != 2 {
		t.Errorf("match = %+v, want auto off with min 2", c.Match)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("defaults do not validate: %v", err)
	}
}

func TestLoadYAMLOverrides(t *testing.T) {
	p := writeTemp(t, `
listen: ":9999"
base_url: http://tv.example.com
log: {level: debug, format: json}
db: {dsn: "postgres://u:p@db:5432/x?sslmode=disable", max_conns: 3}
proxy:
  stall_timeout: 20s
  idle_grace: 1m
  buffer_size_mb: 64
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Listen != ":9999" {
		t.Errorf("listen = %q", c.Listen)
	}
	if c.BaseURL != "http://tv.example.com" {
		t.Errorf("base_url = %q", c.BaseURL)
	}
	if c.Log.Level != "debug" || c.Log.Format != "json" {
		t.Errorf("log = %+v", c.Log)
	}
	if c.DB.MaxConns != 3 {
		t.Errorf("max_conns = %d", c.DB.MaxConns)
	}
	if c.Proxy.StallTimeout.Duration != 20*time.Second {
		t.Errorf("stall_timeout = %v, want 20s", c.Proxy.StallTimeout)
	}
	if c.Proxy.IdleGrace.Duration != time.Minute {
		t.Errorf("idle_grace = %v, want 1m", c.Proxy.IdleGrace)
	}
	if c.Proxy.BufferSizeMB != 64 {
		t.Errorf("buffer_size_mb = %d", c.Proxy.BufferSizeMB)
	}
	// Unspecified values keep their defaults.
	if c.Proxy.ConnectTimeout.Duration != 7*time.Second {
		t.Errorf("connect_timeout = %v, want default 7s", c.Proxy.ConnectTimeout)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("validate: %v", err)
	}
}

func TestEnvOverridesYAML(t *testing.T) {
	p := writeTemp(t, "listen: \":9999\"\nproxy: {stall_timeout: 12s, hls_max_height: 720}\n")
	t.Setenv("MMP_LISTEN", ":7777")
	t.Setenv("MMP_PROXY_STALL_TIMEOUT", "3s")
	t.Setenv("MMP_DB_MAX_CONNS", "5")
	t.Setenv("MMP_MATCH_AUTO", "true")
	t.Setenv("MMP_MATCH_MIN_PROVIDERS", "3")
	t.Setenv("MMP_PROXY_HLS_MAX_BANDWIDTH_KBPS", "4000")
	t.Setenv("MMP_PROXY_FFMPEG_PATH", "/opt/ffmpeg/bin/ffmpeg")
	t.Setenv("MMP_PROBE_ENABLED", "true")
	t.Setenv("MMP_PROXY_SORT_BY_HEALTH", "true")
	t.Setenv("MMP_METRICS_TOKEN", "scrape-secret")
	t.Setenv("MMP_PROBE_RECHECK", "15s")
	t.Setenv("MMP_AUTH_MAX_ATTEMPTS", "25")
	t.Setenv("MMP_AUTH_LOCKOUT", "2m")

	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Listen != ":7777" {
		t.Errorf("listen = %q, want :7777 (env beats yaml)", c.Listen)
	}
	if c.Proxy.StallTimeout.Duration != 3*time.Second {
		t.Errorf("stall_timeout = %v, want 3s", c.Proxy.StallTimeout)
	}
	if c.DB.MaxConns != 5 {
		t.Errorf("max_conns = %d, want 5", c.DB.MaxConns)
	}
	if !c.Match.Auto || c.Match.MinProviders != 3 {
		t.Errorf("match = %+v, want auto with min 3", c.Match)
	}
	if c.Proxy.HLSMaxHeight != 720 || c.Proxy.HLSMaxBandwidthKbps != 4000 {
		t.Errorf("hls caps = %d / %d kbps", c.Proxy.HLSMaxHeight, c.Proxy.HLSMaxBandwidthKbps)
	}
	if c.Proxy.FFmpegPath != "/opt/ffmpeg/bin/ffmpeg" {
		t.Errorf("ffmpeg_path = %q", c.Proxy.FFmpegPath)
	}
	if !c.Metrics.Enabled || c.Metrics.Token != "scrape-secret" {
		t.Errorf("metrics = %+v", c.Metrics)
	}
	if !c.Proxy.SortByHealth {
		t.Error("sort_by_health not set from env")
	}
	if !c.Probe.Enabled || c.Probe.Recheck.Duration != 15*time.Second || c.Probe.Interval.Duration != 30*time.Minute {
		t.Errorf("probe = %+v", c.Probe)
	}
	if c.Auth.MaxAttempts != 25 || c.Auth.Lockout.Duration != 2*time.Minute {
		t.Errorf("auth limiting = %d / %v, want 25 / 2m", c.Auth.MaxAttempts, c.Auth.Lockout)
	}
}

func TestEnvError(t *testing.T) {
	t.Setenv("MMP_PROXY_STALL_TIMEOUT", "not-a-duration")
	if _, err := Load(""); err == nil {
		t.Fatal("want error for invalid duration env")
	}
}

func TestLoadErrors(t *testing.T) {
	if _, err := Load("/nonexistent/mmp.yaml"); err == nil {
		t.Error("missing named config file should error")
	}
	p := writeTemp(t, "proxy: {stall_timeout: [broken\n")
	if _, err := Load(p); err == nil {
		t.Error("malformed YAML should error")
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"bad base_url", func(c *Config) { c.BaseURL = "not a url" }, "base_url"},
		{"trailing slash", func(c *Config) { c.BaseURL = "http://x/" }, "must not end"},
		{"empty dsn", func(c *Config) { c.DB.DSN = "" }, "dsn"},
		{"zero conns", func(c *Config) { c.DB.MaxConns = 0 }, "max_conns"},
		{"bad level", func(c *Config) { c.Log.Level = "loud" }, "log.level"},
		{"bad format", func(c *Config) { c.Log.Format = "xml" }, "log.format"},
		{"cooldown inversion", func(c *Config) { c.Proxy.CooldownMax.Duration = time.Second }, "cooldown"},
		{"zero buffer", func(c *Config) { c.Proxy.BufferSizeMB = 0 }, "buffer_size_mb"},
		{"cc rate too big", func(c *Config) { c.Proxy.MaxCCErrorRate = 2 }, "cc_error_rate"},
		{"refresh interval too short", func(c *Config) { c.Refresh.Interval = Duration{10 * time.Second} }, "refresh.interval"},
		{"disable pct too big", func(c *Config) { c.Refresh.MaxDisablePct = 101 }, "max_disable_pct"},
		{"zero health window", func(c *Config) { c.Proxy.HealthWindow = Duration{} }, "health_window"},
		{"tiny hls window", func(c *Config) { c.HLS.Window = 1 }, "hls"},
		{"zero probe concurrency", func(c *Config) { c.Probe.Concurrency = 0 }, "probe"},
		{"negative hls cap", func(c *Config) { c.Proxy.HLSMaxHeight = -1 }, "hls_max_height"},
		{"zero match providers", func(c *Config) { c.Match.MinProviders = 0 }, "min_providers"},
		{"negative attempts", func(c *Config) { c.Auth.MaxAttempts = -1 }, "max_attempts"},
		{"limiting without lockout", func(c *Config) { c.Auth.Lockout.Duration = 0 }, "lockout"},
		{"bad trusted proxy", func(c *Config) { c.HTTP.TrustedProxies = []string{"10.89.0.0/24", "caddy"} }, "trusted_proxies"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Defaults()
			tt.mut(&c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("want validation error containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

func TestTrustedProxies(t *testing.T) {
	p := writeTemp(t, "http: {trusted_proxies: [\"10.89.0.0/24\"]}\n")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c.TrustedProxyPrefixes(); len(got) != 1 || got[0].String() != "10.89.0.0/24" {
		t.Errorf("yaml prefixes = %v", got)
	}

	t.Setenv("MMP_HTTP_TRUSTED_PROXIES", " 172.18.0.5 , fd00::/8,10.0.0.7/8,::ffff:192.168.1.2")
	if c, err = Load(p); err != nil {
		t.Fatal(err)
	}
	got, err := c.TrustedProxyPrefixes()
	if err != nil {
		t.Fatal(err)
	}
	var s []string
	for _, p := range got {
		s = append(s, p.String())
	}
	if want := "172.18.0.5/32 fd00::/8 10.0.0.0/8 192.168.1.2/32"; strings.Join(s, " ") != want {
		t.Errorf("env prefixes = %v, want %s (env replaces yaml; single IPs and masked CIDRs)", s, want)
	}
	if d := Defaults(); len(d.HTTP.TrustedProxies) != 0 {
		t.Error("headers trusted by default")
	}
}
