// Package config loads MMP configuration from an optional YAML file and
// environment variables. Precedence: defaults < YAML < environment.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration so YAML can use friendly values like "5s".
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		// Also allow bare numbers (nanoseconds) for completeness.
		var n int64
		if err2 := node.Decode(&n); err2 != nil {
			return fmt.Errorf("duration: %w", err)
		}
		d.Duration = time.Duration(n)
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("duration %q: %w", s, err)
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// Config is the full server configuration.
type Config struct {
	Listen  string `yaml:"listen"`
	BaseURL string `yaml:"base_url"`
	Log     struct {
		Level  string `yaml:"level"`  // debug|info|warn|error
		Format string `yaml:"format"` // text|json
	} `yaml:"log"`
	DB struct {
		DSN      string `yaml:"dsn"`
		MaxConns int32  `yaml:"max_conns"`
	} `yaml:"db"`
	HTTP struct {
		ReadHeaderTimeout Duration `yaml:"read_header_timeout"`
		ShutdownTimeout   Duration `yaml:"shutdown_timeout"`
	} `yaml:"http"`
	Proxy struct {
		StallTimeout   Duration `yaml:"stall_timeout"`     // no upstream bytes before failover
		ConnectTimeout Duration `yaml:"connect_timeout"`   // upstream dial+headers budget
		IdleGrace      Duration `yaml:"idle_grace"`        // keep session after last viewer
		MaxRetryWindow Duration `yaml:"max_retry_window"`  // give up when all candidates fail
		CooldownBase   Duration `yaml:"cooldown_base"`     // exponential cooldown floor
		CooldownMax    Duration `yaml:"cooldown_max"`      // exponential cooldown ceiling
		BufferSizeMB   int      `yaml:"buffer_size_mb"`    // ring buffer per session
		LowBitrateKbps int      `yaml:"low_bitrate_kbps"`  // 0 disables the low-bitrate check
		MaxCCErrorRate float64  `yaml:"max_cc_error_rate"` // TS continuity-error fraction per window
		// HLS master playlists: the best variant within these caps (0 = no cap).
		HLSMaxBandwidthKbps int `yaml:"hls_max_bandwidth_kbps"`
		HLSMaxHeight        int `yaml:"hls_max_height"`
		// FFmpegPath runs stream profiles in ffmpeg mode ("" = ffmpeg from PATH).
		FFmpegPath string `yaml:"ffmpeg_path"`
	} `yaml:"proxy"`
	Refresh struct {
		Interval    Duration `yaml:"interval"`     // default source refresh cadence
		EPGInterval Duration `yaml:"epg_interval"` // EPG refresh cadence
	} `yaml:"refresh"`
	Match struct {
		Auto         bool `yaml:"auto"`          // run the matcher after each provider refresh
		MinProviders int  `yaml:"min_providers"` // auto-create channels only for groups at >= N providers
	} `yaml:"match"`
	// Probe is the background stream prober. Off by default: each probe
	// costs a provider connection slot, and some providers dislike them.
	Probe struct {
		Enabled     bool     `yaml:"enabled"`
		Interval    Duration `yaml:"interval"`     // probe every channel stream this often
		Recheck     Duration `yaml:"recheck"`      // re-probe streams in cooldown this often
		Sample      Duration `yaml:"sample"`       // how long one probe reads
		Concurrency int      `yaml:"concurrency"`  // probes at once
		FFprobe     bool     `yaml:"ffprobe"`      // also require ffprobe to find audio/video
		FFprobePath string   `yaml:"ffprobe_path"` // "" = ffprobe from PATH
	} `yaml:"probe"`
	Auth struct {
		SessionTTL Duration `yaml:"session_ttl"` // admin browser session lifetime
	} `yaml:"auth"`
	// Admin bootstraps the first admin user when none exists. Never logged.
	Admin struct {
		Username string `yaml:"username"`
		Password string `yaml:"password"`
	} `yaml:"admin"`
}

// Defaults returns the built-in configuration.
func Defaults() Config {
	var c Config
	c.Listen = ":8080"
	c.BaseURL = "http://localhost:8080"
	c.Log.Level = "info"
	c.Log.Format = "text"
	c.DB.DSN = "postgres://mmp:mmp@localhost:5432/mmp?sslmode=disable"
	c.DB.MaxConns = 8
	c.HTTP.ReadHeaderTimeout = Duration{10 * time.Second}
	c.HTTP.ShutdownTimeout = Duration{15 * time.Second}
	c.Proxy.StallTimeout = Duration{5 * time.Second}
	c.Proxy.ConnectTimeout = Duration{7 * time.Second}
	c.Proxy.IdleGrace = Duration{10 * time.Second}
	c.Proxy.MaxRetryWindow = Duration{30 * time.Second}
	c.Proxy.CooldownBase = Duration{5 * time.Second}
	c.Proxy.CooldownMax = Duration{10 * time.Minute}
	c.Proxy.BufferSizeMB = 32
	c.Proxy.LowBitrateKbps = 20
	c.Proxy.MaxCCErrorRate = 0.05
	c.Refresh.Interval = Duration{6 * time.Hour}
	c.Refresh.EPGInterval = Duration{12 * time.Hour}
	c.Match.MinProviders = 2
	c.Probe.Interval = Duration{30 * time.Minute}
	c.Probe.Recheck = Duration{time.Minute}
	c.Probe.Sample = Duration{3 * time.Second}
	c.Probe.Concurrency = 2
	c.Auth.SessionTTL = Duration{7 * 24 * time.Hour}
	return c
}

// Load builds the configuration: defaults, then the YAML file at path (if
// non-empty; it must exist when named), then environment variables (MMP_*).
func Load(path string) (Config, error) {
	c := Defaults()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return c, fmt.Errorf("read config %s: %w", path, err)
		}
		if err := yaml.Unmarshal(data, &c); err != nil {
			return c, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	if err := c.applyEnv(os.LookupEnv); err != nil {
		return c, err
	}
	return c, nil
}

// envLookup is swappable for tests.
type envLookup func(key string) (string, bool)

func (c *Config) applyEnv(getenv envLookup) error {
	str := func(key string, dst *string) {
		if v, ok := getenv(key); ok && v != "" {
			*dst = v
		}
	}
	dur := func(key string, dst *Duration) error {
		v, ok := getenv(key)
		if !ok {
			return nil
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("%s=%q: %w", key, v, err)
		}
		dst.Duration = d
		return nil
	}
	int32v := func(key string, dst *int32) error {
		v, ok := getenv(key)
		if !ok {
			return nil
		}
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return fmt.Errorf("%s=%q: %w", key, v, err)
		}
		*dst = int32(n)
		return nil
	}
	intv := func(key string, dst *int) error {
		v, ok := getenv(key)
		if !ok {
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("%s=%q: %w", key, v, err)
		}
		*dst = n
		return nil
	}
	boolv := func(key string, dst *bool) error {
		v, ok := getenv(key)
		if !ok {
			return nil
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("%s=%q: %w", key, v, err)
		}
		*dst = b
		return nil
	}
	floatv := func(key string, dst *float64) error {
		v, ok := getenv(key)
		if !ok {
			return nil
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("%s=%q: %w", key, v, err)
		}
		*dst = f
		return nil
	}

	str("MMP_LISTEN", &c.Listen)
	str("MMP_BASE_URL", &c.BaseURL)
	str("MMP_LOG_LEVEL", &c.Log.Level)
	str("MMP_PROXY_FFMPEG_PATH", &c.Proxy.FFmpegPath)
	str("MMP_PROBE_FFPROBE_PATH", &c.Probe.FFprobePath)
	str("MMP_LOG_FORMAT", &c.Log.Format)
	str("MMP_DB_DSN", &c.DB.DSN)
	str("MMP_ADMIN_USERNAME", &c.Admin.Username)
	str("MMP_ADMIN_PASSWORD", &c.Admin.Password)
	for _, e := range []error{
		int32v("MMP_DB_MAX_CONNS", &c.DB.MaxConns),
		dur("MMP_HTTP_READ_HEADER_TIMEOUT", &c.HTTP.ReadHeaderTimeout),
		dur("MMP_HTTP_SHUTDOWN_TIMEOUT", &c.HTTP.ShutdownTimeout),
		dur("MMP_PROXY_STALL_TIMEOUT", &c.Proxy.StallTimeout),
		dur("MMP_PROXY_CONNECT_TIMEOUT", &c.Proxy.ConnectTimeout),
		dur("MMP_PROXY_IDLE_GRACE", &c.Proxy.IdleGrace),
		dur("MMP_PROXY_MAX_RETRY_WINDOW", &c.Proxy.MaxRetryWindow),
		dur("MMP_PROXY_COOLDOWN_BASE", &c.Proxy.CooldownBase),
		dur("MMP_PROXY_COOLDOWN_MAX", &c.Proxy.CooldownMax),
		dur("MMP_REFRESH_INTERVAL", &c.Refresh.Interval),
		dur("MMP_REFRESH_EPG_INTERVAL", &c.Refresh.EPGInterval),
		dur("MMP_AUTH_SESSION_TTL", &c.Auth.SessionTTL),
		intv("MMP_PROXY_BUFFER_SIZE_MB", &c.Proxy.BufferSizeMB),
		intv("MMP_PROXY_LOW_BITRATE_KBPS", &c.Proxy.LowBitrateKbps),
		floatv("MMP_PROXY_MAX_CC_ERROR_RATE", &c.Proxy.MaxCCErrorRate),
		intv("MMP_PROXY_HLS_MAX_BANDWIDTH_KBPS", &c.Proxy.HLSMaxBandwidthKbps),
		intv("MMP_PROXY_HLS_MAX_HEIGHT", &c.Proxy.HLSMaxHeight),
		boolv("MMP_MATCH_AUTO", &c.Match.Auto),
		boolv("MMP_PROBE_ENABLED", &c.Probe.Enabled),
		dur("MMP_PROBE_INTERVAL", &c.Probe.Interval),
		dur("MMP_PROBE_RECHECK", &c.Probe.Recheck),
		dur("MMP_PROBE_SAMPLE", &c.Probe.Sample),
		intv("MMP_PROBE_CONCURRENCY", &c.Probe.Concurrency),
		boolv("MMP_PROBE_FFPROBE", &c.Probe.FFprobe),
		intv("MMP_MATCH_MIN_PROVIDERS", &c.Match.MinProviders),
	} {
		if e != nil {
			return e
		}
	}
	return nil
}

// Validate checks semantic constraints that YAML/env parsing cannot.
func (c *Config) Validate() error {
	if c.Listen == "" {
		return errors.New("config: listen address is empty")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("config: base_url %q must be an absolute URL", c.BaseURL)
	}
	if strings.HasSuffix(c.BaseURL, "/") {
		return fmt.Errorf("config: base_url %q must not end with /", c.BaseURL)
	}
	if c.DB.DSN == "" {
		return errors.New("config: db.dsn is empty")
	}
	if c.Auth.SessionTTL.Duration <= 0 {
		return errors.New("config: auth.session_ttl must be > 0")
	}
	if c.DB.MaxConns < 1 {
		return errors.New("config: db.max_conns must be >= 1")
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: log.level %q invalid", c.Log.Level)
	}
	switch c.Log.Format {
	case "text", "json":
	default:
		return fmt.Errorf("config: log.format %q invalid", c.Log.Format)
	}
	p := c.Proxy
	if p.StallTimeout.Duration <= 0 || p.ConnectTimeout.Duration <= 0 ||
		p.IdleGrace.Duration < 0 || p.MaxRetryWindow.Duration <= 0 ||
		p.CooldownBase.Duration <= 0 || p.CooldownMax.Duration < p.CooldownBase.Duration {
		return errors.New("config: proxy timeouts are inconsistent (stall/connect/retry/cooldowns must be > 0; cooldown_max >= cooldown_base)")
	}
	if p.BufferSizeMB < 1 {
		return errors.New("config: proxy.buffer_size_mb must be >= 1")
	}
	if p.MaxCCErrorRate < 0 || p.MaxCCErrorRate > 1 {
		return errors.New("config: proxy.max_cc_error_rate must be within [0,1]")
	}
	if p.HLSMaxBandwidthKbps < 0 || p.HLSMaxHeight < 0 {
		return errors.New("config: proxy.hls_max_bandwidth_kbps and hls_max_height must be >= 0")
	}
	if pr := c.Probe; pr.Interval.Duration <= 0 || pr.Recheck.Duration <= 0 || pr.Sample.Duration <= 0 || pr.Concurrency < 1 {
		return errors.New("config: probe interval/recheck/sample must be > 0 and concurrency >= 1")
	}
	if c.Match.MinProviders < 1 {
		return errors.New("config: match.min_providers must be >= 1")
	}
	return nil
}
