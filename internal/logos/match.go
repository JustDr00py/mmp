package logos

import (
	"cmp"
	"math"
	"slices"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// words splits s into lowercase search words: accents folded, '&' read as
// "and" and '+' as "plus" (the library spells them out: a-and-e-us.png,
// espn-plus-us.png), everything else that is not a letter or digit a
// separator.
func words(s string) []string {
	s = strings.NewReplacer("&", " and ", "+", " plus ").Replace(strings.ToLower(s))
	var b strings.Builder
	for _, r := range norm.NFD.String(s) {
		switch {
		case unicode.Is(unicode.Mn, r): // accent marks
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			b.WriteByte(' ')
		}
	}
	return strings.Fields(b.String())
}

// softWords say little about which channel a name is: they count for less
// on both sides.
var softWords = map[string]float64{"tv": .4, "channel": .4, "network": .5, "the": .3, "and": .5, "plus": .7}

// dropWords are provider decorations on channel names (quality, feed
// labels) that no logo file carries.
var dropWords = map[string]bool{
	"hd": true, "fhd": true, "uhd": true, "sd": true, "shd": true, "lhd": true, "4k": true, "8k": true,
	"hevc": true, "h264": true, "h265": true, "720p": true, "1080p": true, "1080i": true,
	"pc": true, "raw": true, "vip": true, "backup": true, "bk": true, "feed": true,
}

// feedWords name a regional feed: a channel called "HGTV West" should get
// hgtv-us.png, so they are not required, but a file that has one ("-west")
// fits a channel that has it better.
var feedWords = map[string]bool{"east": true, "west": true, "pacific": true, "mountain": true, "central": true, "atlantic": true}

// variantWords mark alternative renderings of the same logo (horizontal,
// light, dated): a file with them is a weaker fit than the plain one.
var variantWords = map[string]bool{
	"hz": true, "light": true, "dark": true, "logo": true, "alt": true, "white": true, "black": true,
	"color": true, "colour": true, "mono": true, "icon": true, "square": true, "flat": true, "default": true,
}

// countryAliases map a name prefix to the country code the library's file
// names end with.
var countryAliases = map[string]string{"usa": "us", "gb": "uk", "can": "ca"}

// weight is how much a word says about which logo is meant: rare words
// (call letters, brand names) more than common ones ("los", "news"), and
// a word no file has the most (missing it costs the most).
func (l *Library) weight(w string) float64 {
	idf := math.Log(1 + float64(len(l.entries))/float64(len(l.byWord[w])+1))
	if s, ok := softWords[w]; ok {
		return s * idf
	}
	if l.stations[w] {
		return 3 * idf // a local station's logo is the one with its call letters
	}
	return idf
}

// isCallLetters reports whether w is shaped like a US call sign ("ktla").
func isCallLetters(w string) bool {
	if len(w) < 3 || len(w) > 4 || (w[0] != 'k' && w[0] != 'w') {
		return false
	}
	return strings.Trim(w, "abcdefghijklmnopqrstuvwxyz") == ""
}

// fileWeight is what a file word the query lacks costs. Extra words in a
// file name cost half: channel names carry cities and slogans that file
// names leave out more often than the reverse.
func (l *Library) fileWeight(w string) float64 {
	if variantWords[w] || isYear(w) || softWords[w] > 0 {
		return .2 * l.weight(w)
	}
	return .5 * l.weight(w)
}

func isYear(w string) bool {
	return len(w) == 4 && (strings.HasPrefix(w, "19") || strings.HasPrefix(w, "20")) && strings.Trim(w, "0123456789") == ""
}

// query is a channel name prepared for matching: the words a logo should
// have, optional feed words, and the country the name points at.
type query struct {
	words, feed []string
	country     string
	// unlisted is a word not to look files up by: the country word in the
	// reading that keeps it ("usa" would find usa-us.png for every "USA …"
	// channel).
	unlisted string
}

// queries reads a channel name. A leading or trailing country word ("USA
// TMZ", "Sky News UK") gives a country and a second reading without it;
// both are tried, since "USA Network" is a channel whose name starts with
// one.
func (l *Library) queries(name string) []query {
	var q query
	for _, w := range words(name) {
		switch {
		case dropWords[w]:
		case feedWords[w]:
			q.feed = append(q.feed, w)
		default:
			q.words = append(q.words, w)
		}
	}
	if len(q.words) == 0 {
		return nil
	}
	out := []query{q}
	if len(q.words) < 2 {
		return out
	}
	for _, i := range []int{0, len(q.words) - 1} {
		if c := l.countryCode(q.words[i]); c != "" {
			rest := append(slices.Clone(q.words[:i]), q.words[i+1:]...)
			out[0].country, out[0].unlisted = c, q.words[i]
			out = append(out, query{words: rest, feed: q.feed, country: c})
			break
		}
	}
	return out
}

// countryCode is the library country code w names ("us", "usa"), or "".
func (l *Library) countryCode(w string) string {
	if c, ok := countryAliases[w]; ok {
		w = c
	}
	if l.countries[w] {
		return w
	}
	return ""
}

// score rates how well e fits q, from 0 to about 1: the weighted share of
// words the two have in common. prefer is the country to favour when the
// name names none.
func (l *Library) score(q query, e *Entry, prefer string) float64 {
	var common, total, key float64
	in := func(w string, list []string) bool { return slices.Contains(list, w) }
	if len(e.words) != len(q.words) && strings.Join(q.words, "") == e.joined {
		// The same words, spaced differently: "TruTV" is tru-tv.png.
		return 1 + countryFit(q.country, prefer, e.country)
	}
	for _, w := range q.words {
		wt := l.weight(w)
		total += wt
		if in(w, e.words) {
			common += wt
		} else {
			key = max(key, wt)
		}
	}
	for _, w := range e.words {
		switch {
		case in(w, q.words):
		case in(w, q.feed): // the file is this feed's logo
			common += .5
			total += .5
		default:
			total += l.fileWeight(w)
		}
	}
	if total == 0 || common == 0 {
		return 0
	}
	s := common / total
	// Missing the name's most telling word (KTLA in "KTLA Los Angeles")
	// rules a logo out more than the arithmetic alone says.
	if key > 0 && key >= l.maxWeight(q.words)-.01 && len(q.words) > 1 {
		s *= .6
	}
	return s + countryFit(q.country, prefer, e.country)
}

// countryFit adjusts a score for the logo's country: a little up when it
// is the one named or preferred, clearly down when the name names another.
func countryFit(named, prefer, logo string) float64 {
	switch c := cmp.Or(named, prefer); {
	case c == "" || logo == "":
		return 0
	case c == logo:
		return .04
	case named != "":
		return -.15
	}
	return 0
}

func (l *Library) maxWeight(ws []string) float64 {
	m := 0.0
	for _, w := range ws {
		m = max(m, l.weight(w))
	}
	return m
}

// Hit is a library entry with how well it matched.
type Hit struct {
	*Entry
	URL   string
	Score float64
}

// Confident is the score from which a suggestion is taken without a look.
const Confident = .9

// Confident reports whether the hit is a near-certain fit.
func (h Hit) Confident() bool { return h.Score >= Confident }

// Percent is the score for display.
func (h Hit) Percent() int { return int(min(h.Score, 1)*100 + .5) }

// Suggest ranks the logos that best fit a channel name, best first.
// prefer is a country code (see PreferredCountry) favoured when the name
// names no country; "" favours none.
func (l *Library) Suggest(name, prefer string, limit int) []Hit {
	l.mu.RLock()
	defer l.mu.RUnlock()
	qs := l.queries(name)
	best := map[*Entry]float64{}
	for _, q := range qs {
		for _, e := range l.byJoined[strings.Join(q.words, "")] {
			if s := l.score(q, e, prefer); s > best[e] {
				best[e] = s
			}
		}
		for _, w := range q.words {
			if (softWords[w] > 0 && len(q.words) > 1) || w == q.unlisted {
				continue // "tv" alone would pull in thousands of files
			}
			for _, e := range l.byWord[w] {
				if s := l.score(q, e, prefer); s > best[e] {
					best[e] = s
				}
			}
		}
	}
	hits := make([]Hit, 0, len(best))
	for e, s := range best {
		if s >= .2 {
			hits = append(hits, Hit{Entry: e, Score: s})
		}
	}
	return l.top(hits, limit)
}

// PreferredCountry is the country most of names point at ("USA …"), for
// Suggest; "" when none stands out.
func (l *Library) PreferredCountry(names []string) string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	counts := map[string]int{}
	for _, n := range names {
		if qs := l.queries(n); len(qs) > 0 && qs[0].country != "" {
			counts[qs[0].country]++
		}
	}
	best, n := "", 0
	for c, k := range counts {
		if k > n || (k == n && c < best) {
			best, n = c, k
		}
	}
	return best
}

// Search finds logos for words an admin typed: every word must start a
// word of the file name or its folders ("ktla", "cw 5", "united-states").
// Uploaded logos come first; an empty search lists them alone.
func (l *Library) Search(text string, limit int) []Hit {
	l.mu.RLock()
	defer l.mu.RUnlock()
	ws := words(text)
	var hits []Hit
	for _, e := range l.entries {
		if len(ws) == 0 && !e.Uploaded {
			continue
		}
		s, ok := 0.0, true
		for _, w := range ws {
			switch {
			case slices.Contains(e.words, w):
				s += 2
			case slices.ContainsFunc(e.words, func(f string) bool { return strings.HasPrefix(f, w) }):
				s++
			case w == e.country || slices.ContainsFunc(e.folders, func(f string) bool { return strings.HasPrefix(f, w) }):
				s += .5
			default:
				ok = false
			}
			if !ok {
				break
			}
		}
		if !ok {
			continue
		}
		// Fewer extra words: the plain logo before its variants.
		s -= float64(len(e.words)) * .01
		if e.Uploaded {
			s += 10
		}
		hits = append(hits, Hit{Entry: e, Score: s})
	}
	hits = l.top(hits, limit)
	for i := range hits {
		hits[i].Score = 0 // a ranking, not a fit
	}
	return hits
}

// top sorts hits (score, then uploads, national before local, shorter
// names) and keeps limit of them, with their URLs.
func (l *Library) top(hits []Hit, limit int) []Hit {
	slices.SortFunc(hits, func(a, b Hit) int {
		return cmp.Or(
			cmp.Compare(b.Score, a.Score),
			boolCmp(b.Uploaded, a.Uploaded),
			cmp.Compare(strings.Count(a.Path, "/"), strings.Count(b.Path, "/")),
			cmp.Compare(len(a.Name), len(b.Name)),
			strings.Compare(a.Path, b.Path))
	})
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	for i := range hits {
		hits[i].URL = l.URL(hits[i].Path)
	}
	return hits
}

func boolCmp(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}
