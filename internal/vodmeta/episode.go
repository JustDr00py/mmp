package vodmeta

import (
	"regexp"
	"strconv"
	"strings"
)

// episodeMarker is an episode's place in its show: "S01E01", "S1 E2",
// "S01E01E02", "S01E01-E02", "1x01".
var episodeMarker = regexp.MustCompile(`(?i)\b(?:S\d{1,2}\s*E\d{1,4}(?:\s*-?\s*E\d{1,4})*|\d{1,2}x\d{2,4})\b`)

// episodeNum is the episode's number in a marker.
var episodeNum = regexp.MustCompile(`(?i)\bS\d{1,2}\s*E(\d{1,4})|\b\d{1,2}x(\d{2,4})\b`)

// EpisodeTitle is EpisodeName, or "Episode 512" for a name that says
// nothing beyond the show and the episode's place ("EN ★ One Piece -
// S05E512 - [HD]"). The number is the name's own (panels count One Piece
// from 1 each season but name it by its absolute number), else num; with
// neither, the raw name.
func EpisodeTitle(raw, show string, num int) string {
	if s := EpisodeName(raw, show); s != "" {
		return s
	}
	if m := episodeNum.FindStringSubmatch(raw); m != nil {
		num, _ = strconv.Atoi(m[1] + m[2]) // one group matches
	}
	if num > 0 {
		return "Episode " + strconv.Itoa(num)
	}
	return strings.TrimSpace(raw)
}

// EpisodeName is an episode's own name from the title a panel gives it:
// "EN ★ Silicon Valley - S01E01 - Minimum Viable Product [SD]" is
// "Minimum Viable Product". show is the series' clean title, cut from the
// front of names that carry no episode marker. It is "" when the name
// holds nothing else.
func EpisodeName(raw, show string) string {
	s := strings.TrimSpace(raw)
	if loc := episodeMarker.FindStringIndex(s); loc != nil {
		s = s[loc[1]:]
	} else {
		for n := 0; n < 2; n++ { // up to two labels, as in Parse
			m := titleLeadingLang.FindStringSubmatchIndex(s)
			if m == nil {
				break
			}
			code := s[m[2]:m[3]]
			if _, ok := langCodes[strings.ToUpper(code)]; !ok && !unknownLabel(code, s[m[3]:m[1]]) {
				break
			}
			s = s[m[1]:]
		}
		if k := Key(show); k != "" {
			for i := range s { // the shortest front whose key is the show's
				if Key(s[:i]) == k && (i == len(s) || !isWordByte(s[i])) {
					s = s[i:]
					break
				}
			}
		}
	}
	s = trimTags(s)
	if strings.Trim(s, seps+" .") == "" {
		return ""
	}
	return s
}

// trimTags drops trailing bracketed tags that only describe the file
// ("[HD]", "[MULTI-SUB]", "(EN)") and the separators around the name.
func trimTags(s string) string {
	for {
		s = strings.Trim(s, seps+" ")
		loc := bracketed.FindAllStringSubmatchIndex(s, -1)
		if len(loc) == 0 {
			return s
		}
		last := loc[len(loc)-1]
		if last[1] != len(s) || !fileTag(s[last[2]:last[3]]) {
			return s
		}
		s = s[:last[0]]
	}
}

// fileTag: every word inside a bracket is quality, language or release
// noise.
func fileTag(inner string) bool {
	words := strings.FieldsFunc(inner, func(r rune) bool { return r == ' ' || r == '-' || r == ',' || r == '/' || r == '|' })
	for _, w := range words {
		lw := strings.ToLower(w)
		_, q := qualityOf[lw]
		_, c := langCodes[strings.ToUpper(w)]
		_, l := langWords[strings.ToUpper(w)]
		if !q && !c && !l && !noise[lw] && !tagNoise[lw] {
			return false
		}
	}
	return true
}

func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b >= 0x80
}
