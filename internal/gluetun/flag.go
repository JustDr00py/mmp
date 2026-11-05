package gluetun

import "strings"

// CountryCode is the ISO 3166-1 alpha-2 code for a country name as gluetun
// or its IP lookup spells it ("Netherlands", "UK", "Korea (South)",
// "India (via Singapore)"), or "" when unknown.
func CountryCode(name string) string {
	name = strings.TrimSpace(name)
	if c := countryCodes[normCountry(name)]; c != "" {
		return c
	}
	if len(name) == 2 && isUpperASCII(name) { // already a code ("NL")
		return name
	}
	// "India (via Singapore)", "Hong Kong, SAR China"
	if i := strings.IndexAny(name, "(,"); i > 0 {
		return countryCodes[normCountry(name[:i])]
	}
	return ""
}

// Flag is the country's flag emoji, or "" when the country is unknown.
func Flag(name string) string {
	code := CountryCode(name)
	if code == "" {
		return ""
	}
	return string([]rune{0x1F1E6 + rune(code[0]-'A'), 0x1F1E6 + rune(code[1]-'A')})
}

// normCountry keeps a name's ASCII letters, lower-cased.
func normCountry(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isUpperASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}
