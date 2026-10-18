package source

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

// ValidateProvider checks provider settings, shared by the REST API and
// the admin UI so both accept exactly the same input.
func ValidateProvider(name, typ, rawURL, epgURL string, maxConns, refreshSecs int32) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("name is required")
	}
	if typ != "m3u" && typ != "xtream" {
		return errors.New(`type must be "m3u" or "xtream"`)
	}
	if !absHTTP(rawURL) {
		return errors.New("url must be an absolute http(s) URL")
	}
	if epgURL != "" && !absHTTP(epgURL) {
		return errors.New("epg_url must be an absolute http(s) URL")
	}
	if maxConns < 0 {
		return errors.New("max_connections must be >= 0")
	}
	if refreshSecs <= 0 {
		return errors.New("refresh_interval_secs must be > 0")
	}
	return nil
}

func absHTTP(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// ValidateRefreshMaxDisablePct checks a per-provider refresh disable
// threshold override (nil = keep the global default). Shared by the REST
// API and the admin UI.
func ValidateRefreshMaxDisablePct(pct *int32) error {
	if pct == nil {
		return nil
	}
	if *pct < 0 || *pct > 100 {
		return errors.New("refresh_max_disable_pct must be within 0-100")
	}
	return nil
}

// MaxBackboneLen bounds a provider's backbone label.
const MaxBackboneLen = 60

// NoBackbone is the filter value for providers without a backbone.
const NoBackbone = "-"

// ErrBackboneTooLong: a backbone label longer than MaxBackboneLen.
var ErrBackboneTooLong = fmt.Errorf("backbone must be at most %d characters", MaxBackboneLen)

// NormalizeBackbone tidies a backbone label: trimmed, inner whitespace
// collapsed, and spelled like a known label that differs only in case, so
// "strong" joins an existing "Strong" instead of starting a second one.
func NormalizeBackbone(s string, known []string) (string, error) {
	s = strings.Join(strings.Fields(s), " ")
	if s == NoBackbone { // the filter's "no backbone" value is not a label
		s = ""
	}
	if utf8.RuneCountInString(s) > MaxBackboneLen {
		return "", ErrBackboneTooLong
	}
	for _, k := range known {
		if strings.EqualFold(k, s) {
			return k, nil
		}
	}
	return s, nil
}
