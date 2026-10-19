package web

import (
	"cmp"
	"regexp"
	"slices"
	"strings"
)

// Xtream panels give no language per title, but nearly all of them label
// their movie and series categories with one: "EN | Action", "|FR| Films",
// "[DE] Serien", "ENGLISH MOVIES", "Action (ES)". categoryLanguage reads
// that label, so the admin can filter and grant categories by language.

// langCodes maps the short labels panels use, languages and the countries
// that stand for them, to a language name.
var langCodes = map[string]string{
	"EN": "English", "ENG": "English", "UK": "English", "GB": "English", "US": "English", "USA": "English",
	"AU": "English", "NZ": "English", "IE": "English",
	"FR": "French", "FRA": "French", "FRE": "French",
	"DE": "German", "GER": "German", "DEU": "German", "AT": "German",
	"ES": "Spanish", "ESP": "Spanish", "SPA": "Spanish", "LAT": "Spanish", "MX": "Spanish",
	"IT": "Italian", "ITA": "Italian",
	"PT": "Portuguese", "POR": "Portuguese", "BR": "Portuguese",
	"NL": "Dutch", "NED": "Dutch", "DUT": "Dutch",
	"PL": "Polish", "POL": "Polish",
	"TR": "Turkish", "TUR": "Turkish",
	"RU": "Russian", "RUS": "Russian",
	"AR": "Arabic", "ARA": "Arabic",
	"GR": "Greek", "GRE": "Greek", "EL": "Greek",
	"HI": "Hindi", "HIN": "Hindi", "IN": "Indian",
	"SE": "Swedish", "SV": "Swedish", "SWE": "Swedish",
	"NO": "Norwegian", "NOR": "Norwegian",
	"DK": "Danish", "DA": "Danish", "DAN": "Danish",
	"FI": "Finnish", "FIN": "Finnish",
	"RO": "Romanian", "ROM": "Romanian",
	"HU": "Hungarian", "HUN": "Hungarian",
	"CZ": "Czech", "CS": "Czech",
	"BG": "Bulgarian", "HR": "Croatian", "RS": "Serbian", "SR": "Serbian",
	"AL": "Albanian", "SQ": "Albanian", "EXYU": "Ex-Yu",
	"KU": "Kurdish", "KUR": "Kurdish",
	"FA": "Persian", "IR": "Persian", "UR": "Urdu", "PK": "Urdu",
	"BN": "Bengali", "TA": "Tamil", "TAM": "Tamil", "TE": "Telugu", "ML": "Malayalam", "PA": "Punjabi",
	"CN": "Chinese", "ZH": "Chinese", "CHI": "Chinese",
	"JP": "Japanese", "JA": "Japanese", "JAP": "Japanese",
	"KR": "Korean", "KO": "Korean", "KOR": "Korean",
	"VN": "Vietnamese", "VI": "Vietnamese", "TH": "Thai", "PH": "Filipino",
	"HE": "Hebrew", "IL": "Hebrew", "HEB": "Hebrew",
}

// langWords are language names, in English and their own language, that
// mark a category wherever they stand ("FRENCH MOVIES", "Peliculas Latino").
var langWords = map[string]string{
	"ENGLISH": "English", "FRENCH": "French", "FRANCAIS": "French", "FRANÇAIS": "French",
	"GERMAN": "German", "DEUTSCH": "German", "SPANISH": "Spanish", "ESPANOL": "Spanish", "ESPAÑOL": "Spanish",
	"LATINO": "Spanish", "CASTELLANO": "Spanish", "ITALIAN": "Italian", "ITALIANO": "Italian",
	"PORTUGUESE": "Portuguese", "PORTUGUES": "Portuguese", "PORTUGUÊS": "Portuguese", "BRASIL": "Portuguese",
	"DUTCH": "Dutch", "NEDERLANDS": "Dutch", "POLISH": "Polish", "POLSKI": "Polish",
	"TURKISH": "Turkish", "TURKCE": "Turkish", "TÜRKÇE": "Turkish", "RUSSIAN": "Russian", "ARABIC": "Arabic",
	"GREEK": "Greek", "HINDI": "Hindi", "PUNJABI": "Punjabi", "TAMIL": "Tamil", "TELUGU": "Telugu",
	"MALAYALAM": "Malayalam", "URDU": "Urdu", "BENGALI": "Bengali", "PERSIAN": "Persian", "FARSI": "Persian",
	"KURDISH": "Kurdish", "ALBANIAN": "Albanian", "SWEDISH": "Swedish", "NORWEGIAN": "Norwegian",
	"DANISH": "Danish", "FINNISH": "Finnish", "ROMANIAN": "Romanian", "HUNGARIAN": "Hungarian",
	"CZECH": "Czech", "BULGARIAN": "Bulgarian", "CROATIAN": "Croatian", "SERBIAN": "Serbian",
	"CHINESE": "Chinese", "JAPANESE": "Japanese", "KOREAN": "Korean", "VIETNAMESE": "Vietnamese",
	"THAI": "Thai", "FILIPINO": "Filipino", "HEBREW": "Hebrew",
}

var (
	// leadingLang: a short label at the start, after any symbols and a
	// generic word ("VOD |", "4K -"), followed by a separator: "EN | x",
	// "|FR| x", "[DE] x", "EN: x", "(ES) x". A separator is required, so
	// a title word ("IT Crowd") is not read as a language.
	leadingLang = regexp.MustCompile(`^[^\pL\pN]*(?:(?:VOD|MOVIES?|FILMS?|SERIES|SERIE|4K|UHD|FHD|HD|TV)\s*[^\pL\pN\s]*\s*)?(\pL{2,4})\s*[|:\-\]\)/»]`)
	// taggedLang: a short label joined to a generic word by a separator
	// needs none after it: "4K-EN Action", "VOD|FR Films".
	taggedLang = regexp.MustCompile(`^[^\pL\pN]*(?:VOD|MOVIES?|FILMS?|SERIES|SERIE|4K|UHD|FHD|HD|TV)\s*[^\pL\pN\s]+\s*(\pL{2,4})\b`)
	// trailingLang: a short label at the end, bracketed or after a spaced
	// separator: "x (EN)", "x [FR]", "x - DE", "x | ES". "Sci-Fi" does
	// not count.
	trailingLang = regexp.MustCompile(`(?:[\[(]\s*(\pL{2,4})\s*[\])]|\s[|\-]\s*(\pL{2,4}))\s*$`)
	langWordRe   = regexp.MustCompile(`\pL+`)
)

// categoryLanguage returns the language a category name is labelled with,
// or "" when it carries none that is known.
func categoryLanguage(name string) string {
	up := strings.ToUpper(strings.TrimSpace(name))
	for _, re := range []*regexp.Regexp{leadingLang, taggedLang} {
		if m := re.FindStringSubmatch(up); m != nil {
			if l, ok := langCodes[m[1]]; ok {
				return l
			}
		}
	}
	for _, w := range langWordRe.FindAllString(up, -1) {
		if l, ok := langWords[w]; ok {
			return l
		}
	}
	if m := trailingLang.FindStringSubmatch(up); m != nil {
		if l, ok := langCodes[cmp.Or(m[1], m[2])]; ok {
			return l
		}
	}
	return ""
}

// noLanguage stands for categories without a known label in filters.
const noLanguage = "-"

// langOption is one language of a filter, with how many categories have it.
type langOption struct {
	Value, Label string
	Categories   int
}

// languagesOf lists the languages names carry, most categories first, with
// unlabelled ones last.
func languagesOf(names []string) []langOption {
	count := map[string]int{}
	for _, n := range names {
		count[categoryLanguage(n)]++
	}
	var out []langOption
	for l, n := range count {
		if l != "" {
			out = append(out, langOption{Value: l, Label: l, Categories: n})
		}
	}
	slices.SortFunc(out, func(a, b langOption) int {
		return cmp.Or(cmp.Compare(b.Categories, a.Categories), cmp.Compare(a.Label, b.Label))
	})
	if n := count[""]; n > 0 {
		out = append(out, langOption{Value: noLanguage, Label: "No language label", Categories: n})
	}
	return out
}

// inLanguage reports whether a category name matches a filter value
// ("" matches everything).
func inLanguage(name, lang string) bool {
	switch lang {
	case "":
		return true
	case noLanguage:
		return categoryLanguage(name) == ""
	}
	return categoryLanguage(name) == lang
}
