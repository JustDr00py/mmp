// Package redact masks credentials embedded in URLs before they are logged.
//
// IPTV provider URLs carry secrets in several places: HTTP basic-auth
// userinfo, Xtream query parameters (username/password), Xtream path
// segments (/live/<user>/<pass>/<id>.ts), and Postgres DSNs. Rule 1 of
// AGENTS.md: never log credentials. Route every URL through this package.
package redact

import (
	"net/url"
	"regexp"
	"strings"
)

// Mask is the placeholder substituted for secret values in queries and paths.
const Mask = "***"

// userMask is used inside userinfo: unlike "***", alphanumerics survive
// url.User encoding without turning into percent escapes.
const userMask = "redacted"

// sensitiveParams are query-string parameter names whose values must be
// masked (compared case-insensitively).
var sensitiveParams = map[string]bool{
	"username":    true,
	"password":    true,
	"pass":        true,
	"pwd":         true,
	"token":       true,
	"api_key":     true,
	"apikey":      true,
	"key":         true,
	"auth":        true,
	"access_token": true,
	"secret":      true,
}

// xtreamPath matches the credential segments of Xtream-style paths such as
// /live/<user>/<pass>/123.ts, /movie/<user>/<pass>/45.mkv, /series/<user>/<pass>/….
var xtreamPath = regexp.MustCompile(`(?i)/(live|movie|series|radio)/[^/?]+/[^/?]+/`)

// userinfoRe is the fallback scrubber for text that failed to parse as a URL.
// The character class allows ':' (user:pass) but not '/', '?' or whitespace,
// and the match must end at '@' so hosts are never touched.
var userinfoRe = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*)://([^/@\s?]+)@`)

// URL returns a copy of s with embedded credentials replaced by a mask.
// If s cannot be parsed as an absolute URL, a conservative regex scrub is
// applied instead; the input is never returned unmodified when it might
// contain a userinfo section.
func URL(s string) string {
	if s == "" {
		return s
	}
	u, err := url.Parse(s)
	if err != nil {
		return scrub(s)
	}
	if !u.IsAbs() || u.Host == "" {
		// Relative reference (e.g. an HTTP request URI): no userinfo is
		// possible, but path and query credentials still get masked.
		if userinfoRe.MatchString(s) {
			return scrub(s) // embedded absolute URL somewhere in the text
		}
		path := redactPath(u.EscapedPath())
		if u.RawQuery != "" {
			return path + "?" + redactQuery(u.RawQuery)
		}
		return path
	}
	var b strings.Builder
	b.WriteString(u.Scheme)
	b.WriteString("://")
	if u.User != nil {
		b.WriteString(maskUserinfo(u.User))
		b.WriteByte('@')
	}
	b.WriteString(u.Host)
	b.WriteString(redactPath(u.EscapedPath()))
	if u.RawQuery != "" {
		b.WriteByte('?')
		b.WriteString(redactQuery(u.RawQuery))
	}
	if u.Fragment != "" {
		b.WriteByte('#')
		b.WriteString(u.EscapedFragment())
	}
	return b.String()
}

// maskUserinfo replaces a userinfo value with a safe placeholder, keeping
// only whether a password component was present.
func maskUserinfo(u *url.Userinfo) string {
	if _, pSet := u.Password(); pSet || strings.Contains(u.String(), ":") {
		return userMask + ":" + userMask
	}
	return userMask
}

// redactQuery masks sensitive parameter values in a raw query string,
// preserving the original order and encoding of everything else.
func redactQuery(rawQuery string) string {
	if !strings.Contains(rawQuery, "=") {
		return rawQuery
	}
	parts := strings.Split(rawQuery, "&")
	changed := false
	for i, p := range parts {
		if k, _, ok := strings.Cut(p, "="); ok {
			if key, err := url.QueryUnescape(k); err == nil && sensitiveParams[strings.ToLower(key)] {
				parts[i] = k + "=" + Mask
				changed = true
			}
		}
	}
	if !changed {
		return rawQuery
	}
	return strings.Join(parts, "&")
}

// redactPath masks the two credential segments in Xtream-style media paths.
func redactPath(path string) string {
	if !xtreamPath.MatchString(path) {
		return path
	}
	return xtreamPath.ReplaceAllString(path, "/$1/"+Mask+"/"+Mask+"/")
}

// scrub is the last-resort scrubber for unparseable strings.
func scrub(s string) string {
	return userinfoRe.ReplaceAllString(s, "$1://"+userMask+"@")
}
