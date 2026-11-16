package vodmeta

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Parsed is what a panel's title says: "EN | The Matrix 4K (1999)" is
// The Matrix, 1999, 4K, English.
type Parsed struct {
	Title   string // clean title as written, e.g. "The Matrix"
	Year    int    // 0 when the name gives none
	Quality string // "4k", "fhd", "hd", "sd" or ""
	Lang    string // language name (see CategoryLanguage) or ""
}

// Key is the title's comparison form (see Key).
func (p Parsed) Key() string { return Key(p.Title) }

// Display is "Title (Year)", or the title alone without a year.
func (p Parsed) Display() string {
	if p.Year > 0 {
		return p.Title + " (" + strconv.Itoa(p.Year) + ")"
	}
	return p.Title
}

// qualityOf maps quality words to a tier.
var qualityOf = map[string]string{
	"4k": "4k", "uhd": "4k", "2160p": "4k", "8k": "4k",
	"fhd": "fhd", "1080p": "fhd", "1080i": "fhd",
	"hd": "hd", "720p": "hd",
	"sd": "sd", "480p": "sd", "576p": "sd",
}

// noise are release words that say nothing about which title it is.
var noise = map[string]bool{
	"x264": true, "x265": true, "h264": true, "h265": true, "hevc": true, "avc": true, "10bit": true,
	"hdr": true, "hdr10": true, "sdr": true,
	"bluray": true, "bdrip": true, "brrip": true, "webrip": true, "webdl": true, "web-dl": true,
	"hdtv": true, "dvdrip": true, "hdrip": true, "remux": true, "hdcam": true,
	"aac": true, "ac3": true, "dts": true, "atmos": true,
	"multi": true, "multisub": true, "multi-sub": true, "multisubs": true, "sub": true, "subs": true,
	"subbed": true, "dubbed": true, "vostfr": true, "vost": true,
}

// tagNoise are words that, inside brackets, only describe the file:
// "[Multi-Audio]", "[Muti Au/Sub]", "[3D]", "[CAM-VERSION]".
var tagNoise = map[string]bool{
	"audio": true, "au": true, "muti": true, "dual": true, "dub": true, "version": true,
	"3d": true, "sbs": true, "hsbs": true, "ou": true, "hou": true,
	"cam": true, "ts": true, "hdts": true, "telesync": true, "hc": true, "doc": true,
}

var (
	// titleLeadingLang: a language label opening a title: "EN | x",
	// "|FR| x", "[DE] x", "EN - x", "EN ★ x", "4K-EN x". Unlike category names a
	// colon does not count ("It: Chapter Two" is no Italian film).
	titleLeadingLang = regexp.MustCompile(`(?i)^[^\pL\pN]*(?:(?:VOD|MOVIES?|FILMS?|SERIES|SERIE|4K|UHD|FHD|HD|TV)\s*[^\pL\pN\s]+\s*)?(\pL{2,4})\s*(?:[|\]\)/»•·\p{S}]|\s[-\p{Pd}]\s)\s*`)
	// titleTrailingLang: "x (EN)", "x [FR]", "x - DE", "x | ES".
	titleTrailingLang = regexp.MustCompile(`(?i)\s*(?:[\[(]\s*(\pL{2,4})\s*[\])]|\s[|\-]\s*(\pL{2,4}))\s*$`)
	serviceLabel      = regexp.MustCompile(`^\s*\pL{1,2}\+\s*[-|★•]\s*`)
	orderLang         = regexp.MustCompile(`^\d{1,2}\s+(\pL{2,3})\s*(?:[|★•]|-\s)`)
	bracketed         = regexp.MustCompile(`[\[(]([^\[\]()]*)[\])]`)
	yearRe            = regexp.MustCompile(`^(?:19|20)\d{2}$`)
	// yearRange: a series' run, "1984-1992" or "2019-".
	yearRange = regexp.MustCompile(`^((?:19|20)\d{2})(?:\s*\p{Pd}\s*(?:(?:19|20)\d{2})?)?$`)
	// seriesNoise: season and episode markers trailing a show's name.
	seriesNoise = regexp.MustCompile(`(?i)\s*(?:\bS\d{1,2}(?:\s*E\d{1,3})?\b|\bseason\s*\d{1,2}\b|\bcomplete\b).*$`)
	spaces      = regexp.MustCompile(`\s+`)
)

// Parse reads a panel title. category is the title's category name, whose
// language label stands in when the title carries none.
func Parse(raw, category string) Parsed {
	p, _ := parse(raw, category)
	return p
}

// ParseSeries reads a show's panel name, with the release date the panel
// gives it. A bare year that date disagrees with is the title's own
// ("Taj Mahal 1989", a 2020 show); with no year in the name the date's is
// used.
func ParseSeries(raw, category, released string) Parsed {
	p, bare := parse(raw, category)
	r := ReleaseYear(released)
	switch {
	case p.Year == 0:
		p.Year = r
	case bare && r != 0 && (p.Year < r-1 || p.Year > r+1):
		p.Title += " " + strconv.Itoa(p.Year)
		p.Year = r
	}
	return p
}

// parse is Parse, also telling whether the year was a bare word (not
// bracketed), which can be part of a title.
func parse(raw, category string) (p Parsed, bare bool) {
	s := strings.TrimSpace(raw)
	// Release style "The.Matrix.1999.1080p": dots and underscores are
	// spaces when the name has none.
	if !strings.Contains(s, " ") && strings.Count(s, ".")+strings.Count(s, "_") >= 2 {
		s = strings.NewReplacer(".", " ", "_", " ").Replace(s)
	}

	// A streaming service's label, "A+ - Tetris", "D+ - Sketchbook"; a
	// collection's number before a language, "06 EN - Friday The 13th".
	s = serviceLabel.ReplaceAllString(s, "")
	if m := orderLang.FindStringSubmatch(s); m != nil {
		if _, ok := langCodes[strings.ToUpper(m[1])]; ok {
			s = strings.TrimLeft(s, "0123456789 ")
		}
	}
	// Up to two labels: "NF - NL - Annabelle: Creation".
	for n := 0; n < 2; n++ {
		m := titleLeadingLang.FindStringSubmatchIndex(s)
		if m == nil {
			break
		}
		code := s[m[2]:m[3]]
		l, ok := langCodes[strings.ToUpper(code)]
		label := ok || unknownLabel(code, s[m[3]:m[1]])
		if n > 0 { // a second one only as a known code: not "EN ★ IF - 2024"
			label = ok && code == strings.ToUpper(code) && hasWords(s[m[1]:])
		}
		if !label || strings.TrimSpace(s[m[1]:]) == "" {
			break
		}
		for _, w := range strings.FieldsFunc(s[:m[2]], func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
			if q, ok := qualityOf[strings.ToLower(w)]; ok {
				p.Quality = best(p.Quality, q)
			}
		}
		if l != "" {
			p.Lang = l
		}
		s = s[m[1]:]
	}
	if m := titleTrailingLang.FindStringSubmatchIndex(s); m != nil {
		code := ""
		if m[2] >= 0 {
			code = s[m[2]:m[3]]
		} else if m[4] >= 0 {
			code = s[m[4]:m[5]]
		}
		if l, ok := langCodes[strings.ToUpper(code)]; ok && strings.TrimSpace(s[:m[0]]) != "" {
			if p.Lang == "" {
				p.Lang = l
			}
			s = s[:m[0]]
		}
	}

	// Bracketed parts: the first year (or a series' run) is taken and
	// marks where the title ends; quality and noise are dropped, a
	// language noted; anything else ("Director's Cut", "Main Card") stays,
	// as one word.
	s = bracketed.ReplaceAllStringFunc(s, func(b string) string {
		inner := strings.TrimSpace(b[1 : len(b)-1])
		if m := yearRange.FindStringSubmatch(inner); m != nil && plausibleYear(m[1]) {
			p.Year, _ = strconv.Atoi(m[1]) // the last one: "EN (2020) MTV Video Music Awards (2020)"
			return " " + yearMark + " "
		}
		words := strings.FieldsFunc(inner, func(r rune) bool { return r == ' ' || r == '-' || r == ',' || r == '/' || r == '|' })
		if len(words) == 0 {
			return " "
		}
		code, named := "", ""
		for _, w := range words {
			lw := strings.ToLower(w)
			if q, ok := qualityOf[lw]; ok {
				p.Quality = best(p.Quality, q)
				continue
			}
			if l, ok := langCodes[strings.ToUpper(w)]; ok {
				code = l
				continue
			}
			if l, ok := langWords[strings.ToUpper(w)]; ok {
				named = l
				continue
			}
			if !noise[lw] && !tagNoise[lw] {
				return " " + strings.ReplaceAll(b, " ", keptSpace) + " "
			}
		}
		switch {
		case named != "": // "EN ★ Topakk [FILIPINO]": the file's own language
			p.Lang = named
		case p.Lang == "" && code != "" && len(words) == 1:
			p.Lang = code
		}
		return " "
	})

	// Loose words: quality and noise go wherever they stand.
	words := strings.Fields(s)
	kept := words[:0]
	for _, w := range words {
		lw := strings.ToLower(strings.Trim(w, "-–—|:,★•·"))
		q, isQuality := qualityOf[lw]
		if (isQuality || noise[lw]) && len(words) > 1 {
			p.Quality = best(p.Quality, q)
			// A bare year that release words follow ends the title:
			// "The Convert 2023 BLURAY 1080p x264 AAC5 1 YTS MX".
			if n := len(kept); p.Year == 0 && n > 1 && yearRe.MatchString(kept[n-1]) && plausibleYear(kept[n-1]) &&
				!slices.Contains(kept, yearMark) {
				p.Year, _ = strconv.Atoi(kept[n-1])
				kept[n-1] = yearMark
			}
			continue
		}
		kept = append(kept, w)
	}
	for len(kept) > 1 && strings.Trim(kept[len(kept)-1], seps) == "" {
		kept = kept[:len(kept)-1]
	}
	// The title ends at its year: a bracketed one, else a bare one last
	// ("Dune Part Two 2024") or after a dash ("UFC 318 - 2025 [Prelims]").
	// A title of only a year ("1917") keeps it.
	cut := -1
	for i, w := range kept { // the last mark; earlier ones go
		if w == yearMark {
			cut = i
		}
	}
	if cut < 0 && p.Year == 0 {
		for i := len(kept) - 1; i >= 1; i-- {
			w := strings.Trim(kept[i], seps)
			if !yearRe.MatchString(w) || !plausibleYear(w) {
				continue
			}
			if i == len(kept)-1 || strings.Trim(kept[i-1], "-–—") == "" {
				p.Year, _ = strconv.Atoi(w)
				cut, bare = i, true
			}
			break
		}
	}
	if cut >= 0 {
		// After the year only bracketed parts belong to the title; loose
		// words are a panel's labels: "(1985) STEVEN SPIELBERG",
		// "(1946) - THE THREE STOOGES COLLECTION".
		end := cut // the title ends before the year and any dash before it
		for end > 0 && strings.Trim(kept[end-1], seps) == "" {
			end--
		}
		// Earlier bracketed years go too: "EN (2020) MTV Awards (2020)".
		title := slices.DeleteFunc(slices.Clone(kept[:end]), func(w string) bool { return w == yearMark })
		for _, w := range kept[cut+1:] {
			if strings.Contains(w, keptSpace) || strings.ContainsAny(w[:1], "[(") {
				title = append(title, w)
			}
		}
		kept = title
	}
	s = strings.ReplaceAll(strings.Join(kept, " "), keptSpace, " ")
	if t := seriesNoise.ReplaceAllString(s, ""); strings.TrimSpace(strings.Trim(t, " -–—|:,.★•·")) != "" {
		s = t
	}
	s = strings.Trim(spaces.ReplaceAllString(s, " "), " .-–—|:,★•·")
	if s == "" {
		s = strings.TrimSpace(raw)
	}
	p.Title = s
	if p.Lang == "" {
		p.Lang = CategoryLanguage(category)
	}
	return p, bare
}

const (
	// yearMark stands where a bracketed year was, until the title is cut.
	yearMark = "\x01"
	// keptSpace joins a kept bracketed part's words into one.
	keptSpace = "\x02"
	// seps are separators trimmed off words and titles.
	seps = "-–—|:,★•·"
)

// unknownLabel: a label no language is known for still opens a title when
// it is capitals and a symbol follows ("SW ★ x", "ETR ★ x"), or two
// capitals and a dash ("NF - x"); "ABBA - The Movie" stays.
func unknownLabel(code, sep string) bool {
	if code != strings.ToUpper(code) {
		return false
	}
	if strings.IndexFunc(sep, func(r rune) bool { return unicode.IsSymbol(r) || strings.ContainsRune("|])»•·", r) }) >= 0 {
		// A space after it: not "WALL·E", "JJ+E"; not "/": "AC/DC Live".
		return strings.TrimRightFunc(sep, unicode.IsSpace) != sep
	}
	return len([]rune(code)) == 2 && strings.IndexFunc(sep, func(r rune) bool { return unicode.Is(unicode.Pd, r) }) >= 0
}

// hasWords: s has a letter outside brackets ("2024 [4K]" has none).
func hasWords(s string) bool {
	return strings.IndexFunc(bracketed.ReplaceAllString(s, ""), unicode.IsLetter) >= 0
}

// plausibleYear: "2049" in "Blade Runner 2049" is not a release year.
func plausibleYear(s string) bool {
	y, err := strconv.Atoi(s)
	return err == nil && y >= 1900 && y <= time.Now().Year()+1
}

var qualityRank = map[string]int{"": 0, "sd": 1, "hd": 2, "fhd": 3, "4k": 4}

func best(a, b string) string {
	if qualityRank[b] > qualityRank[a] {
		return b
	}
	return a
}

// Key reduces a title to its comparison form: lowercase, accents folded,
// "&" as "and", apostrophes dropped and other punctuation as spaces.
// "Spider-Man: No Way Home" and "spider man no way home" match.
func Key(title string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(strings.ToLower(title)) {
		switch {
		case unicode.Is(unicode.Mn, r), r == '\'', r == '’', r == '`':
		case r == '&':
			b.WriteString(" and ")
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			b.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// ReleaseYear is the year a release date ("2008-01-20", "2008") starts
// with, or 0.
func ReleaseYear(date string) int {
	date = strings.TrimSpace(date)
	if len(date) < 4 || !yearRe.MatchString(date[:4]) || !plausibleYear(date[:4]) {
		return 0
	}
	y, _ := strconv.Atoi(date[:4])
	return y
}

// ParseVersion changes whenever Parse reads names differently; titles
// stored by an older parser are parsed again at startup (see Reparse).
const ParseVersion = 3
