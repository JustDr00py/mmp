package vodmeta

import (
	"regexp"
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

var (
	// titleLeadingLang: a language label opening a title: "EN | x",
	// "|FR| x", "[DE] x", "EN - x", "EN ★ x", "4K-EN x". Unlike category names a
	// colon does not count ("It: Chapter Two" is no Italian film).
	titleLeadingLang = regexp.MustCompile(`(?i)^[^\pL\pN]*(?:(?:VOD|MOVIES?|FILMS?|SERIES|SERIE|4K|UHD|FHD|HD|TV)\s*[^\pL\pN\s]+\s*)?(\pL{2,4})\s*(?:[|\]\)/»•·\p{S}]|\s[-\p{Pd}]\s)\s*`)
	// titleTrailingLang: "x (EN)", "x [FR]", "x - DE", "x | ES".
	titleTrailingLang = regexp.MustCompile(`(?i)\s*(?:[\[(]\s*(\pL{2,4})\s*[\])]|\s[|\-]\s*(\pL{2,4}))\s*$`)
	bracketed         = regexp.MustCompile(`[\[(]([^\[\]()]*)[\])]`)
	yearRe            = regexp.MustCompile(`^(?:19|20)\d{2}$`)
	// seriesNoise: season and episode markers trailing a show's name.
	seriesNoise = regexp.MustCompile(`(?i)\s*(?:\bS\d{1,2}(?:\s*E\d{1,3})?\b|\bseason\s*\d{1,2}\b|\bcomplete\b).*$`)
	spaces      = regexp.MustCompile(`\s+`)
)

// Parse reads a panel title. category is the title's category name, whose
// language label stands in when the title carries none.
func Parse(raw, category string) Parsed {
	var p Parsed
	s := strings.TrimSpace(raw)
	// Release style "The.Matrix.1999.1080p": dots and underscores are
	// spaces when the name has none.
	if !strings.Contains(s, " ") && strings.Count(s, ".")+strings.Count(s, "_") >= 2 {
		s = strings.NewReplacer(".", " ", "_", " ").Replace(s)
	}

	if m := titleLeadingLang.FindStringSubmatchIndex(s); m != nil {
		if l, ok := langCodes[strings.ToUpper(s[m[2]:m[3]])]; ok && strings.TrimSpace(s[m[1]:]) != "" {
			for _, w := range strings.FieldsFunc(s[:m[2]], func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
				if q, ok := qualityOf[strings.ToLower(w)]; ok {
					p.Quality = best(p.Quality, q)
				}
			}
			p.Lang, s = l, s[m[1]:]
		}
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

	// Bracketed parts: a year is taken, quality and noise dropped, a
	// language label noted; anything else ("Director's Cut") stays.
	s = bracketed.ReplaceAllStringFunc(s, func(b string) string {
		inner := strings.TrimSpace(b[1 : len(b)-1])
		if yearRe.MatchString(inner) && plausibleYear(inner) {
			p.Year, _ = strconv.Atoi(inner)
			return " "
		}
		words := strings.FieldsFunc(inner, func(r rune) bool { return r == ' ' || r == '-' || r == ',' || r == '/' || r == '|' })
		if len(words) == 0 {
			return " "
		}
		lang := ""
		for _, w := range words {
			lw := strings.ToLower(w)
			if q, ok := qualityOf[lw]; ok {
				p.Quality = best(p.Quality, q)
				continue
			}
			if l, ok := langCodes[strings.ToUpper(w)]; ok {
				lang = l
				continue
			}
			if !noise[lw] {
				return b
			}
		}
		if p.Lang == "" && lang != "" && len(words) == 1 {
			p.Lang = lang
		}
		return " "
	})

	// Loose words: quality and noise go wherever they stand.
	words := strings.Fields(s)
	kept := words[:0]
	for _, w := range words {
		lw := strings.ToLower(strings.Trim(w, "-–—|:,★•·"))
		if q, ok := qualityOf[lw]; ok && len(words) > 1 {
			p.Quality = best(p.Quality, q)
			continue
		}
		if noise[lw] && len(words) > 1 {
			continue
		}
		kept = append(kept, w)
	}
	// A bare trailing year ("Dune Part Two 2024", "Avatar - 2009") when no
	// bracketed one was found; a title of only a year ("1917") keeps it.
	for len(kept) > 1 && strings.Trim(kept[len(kept)-1], "-–—|:,★•·") == "" {
		kept = kept[:len(kept)-1]
	}
	if p.Year == 0 && len(kept) > 1 {
		last := strings.Trim(kept[len(kept)-1], "-–—|:,★•·")
		if yearRe.MatchString(last) && plausibleYear(last) {
			p.Year, _ = strconv.Atoi(last)
			kept = kept[:len(kept)-1]
		}
	}
	s = strings.Join(kept, " ")
	if t := seriesNoise.ReplaceAllString(s, ""); strings.TrimSpace(strings.Trim(t, " -–—|:,.★•·")) != "" {
		s = t
	}
	s = strings.Trim(spaces.ReplaceAllString(s, " "), " -–—|:,.★•·")
	if s == "" {
		s = strings.TrimSpace(raw)
	}
	p.Title = s
	if p.Lang == "" {
		p.Lang = CategoryLanguage(category)
	}
	return p
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
const ParseVersion = 2
