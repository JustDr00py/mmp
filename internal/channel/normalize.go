// Package channel groups equivalent provider streams under logical
// channels: name normalization and cross-provider matching.
package channel

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// qualityTokens are tokens stripped from stream names; they carry no
// channel identity ("HD" vs "FHD" is the same channel).
var qualityTokens = map[string]bool{
	"hd": true, "fhd": true, "uhd": true, "sd": true,
	"4k": true, "8k": true,
	"720p": true, "1080p": true, "1080i": true, "480p": true, "576p": true,
	"h264": true, "h265": true, "hevc": true, "mpeg2": true, "mpeg4": true,
	"hdtv": true, "sdtv": true,
}

// countryTokens are stripped as standalone tokens anywhere in the name,
// as long as something else remains. Longer names ("USA") are not: "USA
// Network" is a channel, not "USA" + "Network".
var countryTokens = map[string]bool{
	"us": true, "uk": true, "ca": true, "fr": true, "de": true, "es": true,
	"it": true, "nl": true, "pt": true, "br": true, "mx": true, "ar": true,
	"ru": true, "in": true, "au": true, "nz": true, "jp": true, "kr": true,
	"cn": true, "pl": true, "se": true, "no": true, "dk": true, "fi": true,
	"tr": true, "gr": true, "ie": true, "za": true, "ch": true, "at": true,
	"be": true, "cl": true, "co": true, "pe": true, "il": true, "ae": true,
	"sa": true, "cz": true, "ro": true, "hu": true, "ua": true, "th": true,
	"vn": true, "ph": true, "my": true, "sg": true, "id": true, "hk": true,
	"tw": true, "lt": true, "lv": true, "ee": true, "hr": true, "rs": true,
	"bg": true, "sk": true, "si": true,
}

// longCountryPrefixes are stripped only when glued to a separator in the
// original text ("USA: NBC", "United States - ESPN").
var longCountryPrefixes = []string{"usa", "united states", "united kingdom", "canada"}

// NormalizeName reduces a provider stream name to a match key: lowercase,
// accents folded, punctuation turned to spaces, quality and country
// decorations dropped, whitespace collapsed. "US: ABC East FHD" and
// "abc-east hd" both become "abc east".
func NormalizeName(name string) string {
	s := foldAccents(strings.ToLower(name))

	// Long country prefixes only count when glued to a separator:
	// "usa: nbc" -> "nbc", but "usa network" stays intact.
	for _, p := range longCountryPrefixes {
		if !strings.HasPrefix(s, p) {
			continue
		}
		rest := strings.TrimLeft(s[len(p):], " ")
		if rest != "" && strings.ContainsRune(":-|/", rune(rest[0])) {
			s = rest[1:]
			break
		}
	}

	s = strings.Map(func(r rune) rune {
		if unicode.IsPunct(r) || unicode.IsSymbol(r) {
			return ' '
		}
		return r
	}, s)

	words := strings.Fields(s)
	noQuality := make([]string, 0, len(words))
	for _, w := range words {
		if !qualityTokens[w] {
			noQuality = append(noQuality, w)
		}
	}
	noCountry := make([]string, 0, len(noQuality))
	for _, w := range noQuality {
		if !countryTokens[w] {
			noCountry = append(noCountry, w)
		}
	}

	// Never reduce to nothing: a name that is only decorations keeps its
	// quality-stripped form, and failing that the raw words.
	switch {
	case len(noCountry) > 0:
		return strings.Join(noCountry, " ")
	case len(noQuality) > 0:
		return strings.Join(noQuality, " ")
	default:
		return strings.Join(words, " ")
	}
}

// foldAccents strips diacritics: "série" -> "serie".
func foldAccents(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range norm.NFD.String(s) {
		switch {
		case r == 'ß':
			b.WriteString("ss")
		case unicode.Is(unicode.Mn, r):
			// combining mark: drop
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
