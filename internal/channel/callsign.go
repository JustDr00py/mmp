package channel

import (
	"cmp"
	"regexp"
	"strings"
	"unicode"
)

// Local stations are named differently at every provider ("CBS 12 JACKSON
// MS (WJTV)", "CBS 12 (WJTV) Jackson", "US ★ CBS 12 JACKSON MS HD" with tvg
// "CBSWJTV.us"), but they share the station's call sign. CallSign reads it,
// so the matcher can link those streams and keep stations apart that a
// provider gave the same tvg-id.

// callParenRe is a call sign in parentheses: "(KOMO)", "(WBBJ-DT3)",
// "(WPXIDT)". The letters must be capitals in the original text.
var callParenRe = regexp.MustCompile(`\(\s*([KW][A-Z]{2,3})(?:-?(DT|TV|LD|CD|HD|D)?(\d)?)?\s*\)`)

// callSuffixRe is a call sign with a subchannel or service suffix, which a
// word never has: "WTOV-D2", "WBBJ-DT3", "KHBB-DT".
var callSuffixRe = regexp.MustCompile(`\b([KW][A-Z]{2,3})-(DT|TV|LD|CD|HD|D)(\d)?\b`)

// callBareRe is a bare four-letter call sign ("TV: SEATTLE, WA KOMO ABC 4").
// Only trusted when the name also names a network.
var callBareRe = regexp.MustCompile(`\b([KW][A-Z]{3})\b`)

// networkWords are the broadcast networks local stations carry.
var networkWords = map[string]bool{
	"abc": true, "cbs": true, "nbc": true, "fox": true, "pbs": true, "cw": true,
	"mytv": true, "mnt": true, "ion": true, "telemundo": true, "univision": true,
	"unimas": true, "ind": true, "independent": true, "metv": true,
}

// callStopwords are capitalized words shaped like a call sign.
var callStopwords = map[string]bool{
	"west": true, "wild": true, "kids": true, "week": true, "wire": true, "wine": true,
	"will": true, "with": true, "what": true, "when": true, "wars": true, "wave": true,
	"wolf": true, "kick": true, "keep": true, "know": true, "wide": true, "wife": true,
	"walk": true, "ways": true, "work": true, "wwe": true, "wow": true, "waco": true,
	"kent": true, "wake": true, "kiss": true, "kill": true, "keys": true, "kart": true,
	"kidz": true, "kayo": true, "wnba": true, "kika": true, "wetv": true,
}

// CallSign returns the station a local channel's stream carries: its call
// sign, lower case, with "-N" for a subchannel other than the main one, and
// before it the network when the name or tvg-id gives one ("cbs/wjtv",
// "fox/wtov-2", "kmax"); "" when they name no station. One call sign can
// carry several networks ("ABC 4 (KITV)", "MeTV (KITV)"). The name wins
// over the tvg-id: providers copy tvg-ids between stations.
func CallSign(name, tvg string) string {
	cs := callFromName(name)
	if cs == "" {
		if cs = callFromTvg(tvg); cs == "" {
			return ""
		}
	}
	if net := cmp.Or(networkOf(name), networkOf(tvg), tvgNetwork(tvg)); net != "" {
		return net + "/" + cs
	}
	return cs
}

// networkAliases spell a network the way networkOf reports it.
var networkAliases = map[string]string{"mnt": "mytv", "independent": "ind"}

// networkOf is the first network named in s ("US| CBS 12 JACKSON" -> "cbs").
func networkOf(s string) string {
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) }) {
		if networkWords[w] {
			return cmp.Or(networkAliases[w], w)
		}
	}
	return ""
}

// tvgNetwork is the network a glued tvg-id starts with ("CBSWJTV.us").
func tvgNetwork(tvg string) string {
	stem, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(tvg)), ".")
	for _, w := range tvgNetworks {
		if strings.HasPrefix(stem, w) && len(stem) > len(w) {
			return cmp.Or(networkAliases[w], w)
		}
	}
	return ""
}

func callFromName(name string) string {
	if !strings.ContainsAny(name, "KW") { // most names: skip the regexps
		return ""
	}
	if strings.Contains(name, "(") {
		if m := callParenRe.FindStringSubmatch(name); m != nil && !callStopwords[strings.ToLower(m[1])] {
			return callKey(m[1], m[3])
		}
	}
	if strings.Contains(name, "-") {
		if m := callSuffixRe.FindStringSubmatch(name); m != nil && !callStopwords[strings.ToLower(m[1])] {
			return callKey(m[1], m[3])
		}
	}
	if networkOf(name) == "" {
		return ""
	}
	found := ""
	for _, m := range callBareRe.FindAllStringSubmatch(name, -1) {
		w := strings.ToLower(m[1])
		if callStopwords[w] || networkWords[w] {
			continue
		}
		if found != "" && found != w { // two candidates: neither is trusted
			return ""
		}
		found = w
	}
	return found
}

// callFromTvg reads the forms providers use: "loc.NBC (WTOV) Steubenville,
// OH", "NBCWTOV.us", "cbs12wjtv.us", "abc13ktrktv.us", "cbs3wwaydt2.us" and
// "KTRK.us". Glued forms count only after a network name, a bare one only
// with the US domain.
func callFromTvg(tvg string) string {
	if m := callParenRe.FindStringSubmatch(tvg); m != nil && !callStopwords[strings.ToLower(m[1])] {
		return callKey(m[1], m[3])
	}
	stem, domain, _ := strings.Cut(strings.ToLower(strings.TrimSpace(tvg)), ".")
	if stem == "" || strings.IndexFunc(stem, func(r rune) bool { return r > unicode.MaxASCII || !(unicode.IsLetter(r) || unicode.IsDigit(r)) }) >= 0 {
		return ""
	}
	if len(stem) == 4 && isCallLetters(stem) {
		if domain != "us" || callStopwords[stem] {
			return ""
		}
		return stem
	}
	rest, ok := cutNetwork(stem)
	if !ok {
		return ""
	}
	sub := ""
	if n := len(rest); n > 0 && rest[n-1] >= '0' && rest[n-1] <= '9' && strings.HasSuffix(rest[:n-1], "dt") {
		sub, rest = rest[n-1:], rest[:n-3] // "wwaydt2"
	}
	try := func(s string) string {
		if len(s) >= 4 && isCallLetters(s[len(s)-4:]) {
			if w := s[len(s)-4:]; !callStopwords[w] {
				return w
			}
		}
		// Three letters only right after a channel number: "cbs3kyw".
		if len(s) >= 4 && isCallLetters(s[len(s)-3:]) && s[len(s)-4] >= '0' && s[len(s)-4] <= '9' {
			return s[len(s)-3:]
		}
		return ""
	}
	cs := try(rest)
	if cs == "" {
		for _, suf := range []string{"tv", "dt", "hd"} { // "abc13ktrktv"
			if r, ok := strings.CutSuffix(rest, suf); ok {
				if cs = try(r); cs != "" {
					break
				}
			}
		}
	}
	if cs == "" {
		return ""
	}
	return callKey(cs, sub)
}

// tvgNetworks are the network names a glued tvg-id starts with, longest
// first.
var tvgNetworks = []string{"independent", "telemundo", "univision", "unimas", "mytv", "metv",
	"abc", "cbs", "nbc", "fox", "pbs", "mnt", "ion", "ind", "cw"}

// cutNetwork removes a leading network name from a tvg stem.
func cutNetwork(stem string) (string, bool) {
	for _, w := range tvgNetworks {
		if rest, ok := strings.CutPrefix(stem, w); ok && rest != "" {
			return rest, true
		}
	}
	return "", false
}

func isCallLetters(s string) bool {
	if s[0] != 'k' && s[0] != 'w' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < 'a' || s[i] > 'z' {
			return false
		}
	}
	return true
}

// callKey is the match key of a call sign: the main channel (no digit, or
// 1) is the plain call sign, other subchannels add their number.
func callKey(call, sub string) string {
	k := strings.ToLower(call)
	if sub != "" && sub != "0" && sub != "1" {
		k += "-" + sub
	}
	return k
}
