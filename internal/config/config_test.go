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
	if c.Proxy.StallTimeout.Duration != 5*time.Second {
		t.Errorf("stall_timeout = %v, want 5s", c.Proxy.StallTimeout)
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
  stall_timeout: 12s
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
	if c.Proxy.StallTimeout.Duration != 12*time.Second {
		t.Errorf("stall_timeout = %v, want 12s", c.Proxy.StallTimeout)
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
		{"negative hls cap", func(c *Config) { c.Proxy.HLSMaxHeight = -1 }, "hls_max_height"},
		{"zero match providers", func(c *Config) { c.Match.MinProviders = 0 }, "min_providers"},
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
