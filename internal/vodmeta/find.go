package vodmeta

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// How a title was found on TMDB (see Find).
const (
	HowExact = "exact" // the same title, the year near
	HowLoose = "loose" // the only one of that title, the year further off
	HowAlt   = "alt"   // the same as one of its other titles or translations
	HowFuzzy = "fuzzy" // nearly the same title, the same year
)

// epochYear is the year panels give a title they have no date for
// (a release date of 0): "A Danfo Christmas (1970)", a 2024 film.
const epochYear = 1970

// Find searches TMDB for a title and picks the hit that is it, if any,
// most certain first: the title itself, then the parts of a name that
// carries two ("Die Vorkosterinnen, The Tasters", "The Ring (La señal)"),
// each by its exact title; then a title in another language or spelling,
// by the top hits' other titles and translations; then a nearly equal
// title of the same year ("Zootopie", "Scott Pilgrim"). It returns how
// the hit was found ("" for none).
func Find(ctx context.Context, c *TMDB, kind, title string, year int) (TMDBResult, string, error) {
	f := finder{c: c, kind: kind, seen: map[searchKey][]TMDBResult{}}
	// A part of one word is weak ("Bobby, el erizo" is no film "Bobby"):
	// it is tried last, after the strong ones' other titles.
	var strong, weak []string
	for i, q := range SearchTitles(title) {
		if i > 0 && len(strings.Fields(Key(q))) < 2 {
			weak = append(weak, q)
		} else {
			strong = append(strong, q)
		}
	}
	exact := func(queries []string) (TMDBResult, string, error) {
		for _, q := range queries {
			hit, how, err := f.exact(ctx, q, year, q == title)
			if err != nil || how != "" {
				return hit, how, err
			}
		}
		return TMDBResult{}, "", nil
	}
	if hit, how, err := exact(strong); err != nil || how != "" {
		return hit, how, err
	}
	if year == 0 || year == epochYear {
		return exact(weak) // too little to go on for the rest
	}
	// Hits of the year (TMDB's order), for the name and its parts.
	var near []TMDBResult
	for _, q := range strong {
		res, err := f.search(ctx, q, year)
		if err != nil {
			return TMDBResult{}, "", err
		}
		for _, r := range res {
			if yearNear(r.Year(), year, 1) && !slices.ContainsFunc(near, func(n TMDBResult) bool { return n.ID == r.ID }) {
				near = append(near, r)
			}
		}
	}
	// The top hits' other titles: TMDB finds "Anges et Démons" by them.
	for _, r := range near[:min(2, len(near))] {
		names, err := c.Names(ctx, kind, r.ID)
		if err != nil {
			return TMDBResult{}, "", err
		}
		for _, n := range names {
			for _, q := range strong {
				if looseKey(n) == looseKey(q) {
					return r, HowAlt, nil
				}
			}
		}
	}
	if hit, how, err := exact(weak); err != nil || how != "" {
		return hit, how, err
	}
	if r, ok := pickFuzzy(near, strong, year); ok {
		return r, HowFuzzy, nil
	}
	return TMDBResult{}, "", nil
}

type searchKey struct {
	q    string
	year int
}

type finder struct {
	c    *TMDB
	kind string
	seen map[searchKey][]TMDBResult // searches made for this title
}

func (f *finder) search(ctx context.Context, q string, year int) ([]TMDBResult, error) {
	k := searchKey{q, year}
	if res, ok := f.seen[k]; ok {
		return res, nil
	}
	res, err := f.c.Search(ctx, f.kind, q, year)
	if err == nil {
		f.seen[k] = res
	}
	return res, err
}

// exact finds q by its exact title: with the year, then in every year
// (TMDB's year is the first release; a panel's may be a later one). Only
// the whole name (whole) may be the one title of its name further off.
func (f *finder) exact(ctx context.Context, q string, year int, whole bool) (TMDBResult, string, error) {
	if year == epochYear {
		year = 0
	}
	if year > 0 {
		res, err := f.search(ctx, q, year)
		if err != nil {
			return TMDBResult{}, "", err
		}
		if hit, ok := Pick(res, q, year); ok {
			return hit, HowExact, nil
		}
	}
	res, err := f.search(ctx, q, 0)
	if err != nil {
		return TMDBResult{}, "", err
	}
	if hit, ok := Pick(res, q, year); ok {
		return hit, HowExact, nil
	}
	// The only title of that name, a little further off: a panel's year
	// is often a later release's (or, for a show, its latest season's).
	// Two words or more, spelt the same: not "Plan" for "The Plan", nor
	// "OSCARS" for "Oscar's".
	if !whole || year == 0 || len(strings.Fields(Key(q))) < 2 {
		return TMDBResult{}, "", nil
	}
	if hit, ok := Pick(res, q, 0); ok && (f.kind == "series" || yearNear(hit.Year(), year, 2)) {
		if t, o := hit.Titles(); Key(t) == Key(q) || Key(o) == Key(q) {
			return hit, HowLoose, nil
		}
	}
	return TMDBResult{}, "", nil
}

// Pick chooses the search hit that is the title: its title or original
// title the same (by Key, a leading article aside), and its year within
// one of the panel's (a cinema and a home release often differ). Without
// a year only a single such hit counts. Hits come in TMDB's order (most
// relevant first).
func Pick(results []TMDBResult, title string, year int) (TMDBResult, bool) {
	key := looseKey(title)
	if key == "" {
		return TMDBResult{}, false
	}
	var same []TMDBResult
	for _, r := range results {
		t, o := r.Titles()
		if looseKey(t) == key || looseKey(o) == key {
			same = append(same, r)
		}
	}
	if year == 0 {
		if len(same) == 1 {
			return same[0], true
		}
		return TMDBResult{}, false
	}
	for _, d := range []int{0, 1} { // an exact year beats a near one
		for _, r := range same {
			if y := r.Year(); y != 0 && (y-year == d || year-y == d) {
				return r, true
			}
		}
	}
	return TMDBResult{}, false
}

// pickFuzzy takes the one hit of the year whose title is nearly a query
// (a spelling apart: "Zootopie", "Dave Chapelle Sticks and Stones"), or
// the start of it ("Scott Pilgrim" for "Scott Pilgrim vs. the World").
// Two such hits are too close to call.
func pickFuzzy(near []TMDBResult, queries []string, year int) (TMDBResult, bool) {
	var found []TMDBResult
	for _, r := range near {
		t, o := r.Titles()
		for _, q := range queries {
			if similar(q, t, r.Year() == year) || similar(q, o, r.Year() == year) {
				found = append(found, r)
				break
			}
		}
	}
	if len(found) == 1 {
		return found[0], true
	}
	return TMDBResult{}, false
}

// similar: b is a's title spelt a little differently, or (in its exact
// year) a's two or more words begin b.
func similar(a, b string, sameYear bool) bool {
	ka, kb := looseKey(a), looseKey(b)
	if ka == "" || kb == "" {
		return false
	}
	if sameYear && len(strings.Fields(ka)) >= 2 && strings.HasPrefix(kb+" ", ka+" ") {
		return true
	}
	ra, rb := []rune(ka), []rune(kb)
	n := max(len(ra), len(rb))
	return n >= 6 && float64(levenshtein(ra, rb)) <= 0.15*float64(n)
}

func levenshtein(a, b []rune) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

func yearNear(y, year, d int) bool {
	return y != 0 && y-year <= d && year-y <= d
}

var leadingArticle = regexp.MustCompile(`^(?:the|a|an) `)

// looseKey is Key with a leading article dropped: "Matrix Resurrections"
// is "The Matrix Resurrections".
func looseKey(s string) string {
	k := Key(s)
	if t := leadingArticle.ReplaceAllString(k, ""); t != "" {
		return t
	}
	return k
}

var (
	altBracket = regexp.MustCompile(`\s*[\[(]([^\[\]()]+)[\])]\s*`)
	commaPair  = regexp.MustCompile(`^([^,]{2,}),\s+([^,]{2,})$`)
	// edition: a bracketed part that says which cut, not another title.
	edition = regexp.MustCompile(`(?i)\b(?:cut|edition|version|remaster(?:ed)?|uncut|unrated|extended|theatrical|imax|colou?r|b&w|special|anniversary|director'?s|main card|prelims)\b`)
	// partOf: a bracketed part naming one video of several.
	partOf = regexp.MustCompile(`(?i)\b(?:(?:early )?prelims|main card|weigh[- ]?ins?|press conference|part\s*\d+|pt\.?\s*\d+|vol(?:ume)?\.?\s*\d+|dis[ck]\s*\d+|cd\s*\d+)\b`)
	// orderNo: a collection's numbering, "09-Son Of The Pink Panther".
	orderNo = regexp.MustCompile(`^\d{1,2}\s*-\s*`)
	// trailingEdition: "Goblet Of Fire Extended", "Aliens Director's Cut".
	trailingEdition = regexp.MustCompile(`(?i)\s+(?:extended|uncut|unrated|remaster(?:ed)?|theatrical|director'?s cut|(?:extended|special|final) (?:cut|edition))$`)
	// firstOfSeries: a "1" panels add to a first film, "Extraction 1".
	firstOfSeries = regexp.MustCompile(`\s+1(\s|$)`)
)

// SearchTitles are the titles to search for a name: the name; each of two
// titles it carries ("The Ring (La señal)", "Our Fault [Culpa Nuestra]",
// "Die Vorkosterinnen, The Tasters"); and those without a collection's
// numbering ("09-Son Of The Pink Panther", "Harry Potter 4 Harry Potter
// And The Goblet Of Fire", "Extraction 1") or a cut's name ("Extended").
func SearchTitles(title string) []string {
	out := []string{title}
	add := func(s string) {
		s = strings.Trim(strings.TrimSpace(s), " -–—:,")
		if Key(s) != "" && !slices.ContainsFunc(out, func(o string) bool { return Key(o) == Key(s) }) {
			out = append(out, s)
		}
	}
	words := func(s string) int { return len(strings.Fields(Key(s))) }
	// A bracketed part of two words or more ("Gladiator (Extended Cut)"
	// is no film called "Extended Cut"); a comma pair with one part of two
	// words or more ("Love, Simon" is one title).
	// Not for a part of an event or a set: "UFC 300 [Prelims]" and
	// "UFC 300 [Main Card]" are different videos of one TMDB title.
	if m := altBracket.FindAllStringSubmatchIndex(title, -1); len(m) == 1 && !partOf.MatchString(title[m[0][2]:m[0][3]]) {
		add(title[:m[0][0]] + " " + title[m[0][1]:])
		if inner := title[m[0][2]:m[0][3]]; words(inner) >= 2 && !edition.MatchString(inner) {
			add(inner)
		}
	}
	if m := commaPair.FindStringSubmatch(title); m != nil && max(words(m[1]), words(m[2])) >= 2 {
		add(m[1])
		add(m[2])
	}
	for _, t := range slices.Clone(out) {
		t = trailingEdition.ReplaceAllString(orderNo.ReplaceAllString(t, ""), "")
		t = franchiseTitle(t)
		add(t)
		add(firstOfSeries.ReplaceAllString(t, "$1"))
	}
	return out
}

// franchiseTitle drops a franchise and number before a title that starts
// with the franchise again: "Harry Potter 4 Harry Potter And The Goblet
// Of Fire" is "Harry Potter And The Goblet Of Fire".
func franchiseTitle(s string) string {
	w := strings.Fields(s)
	for i := 1; i+2 < len(w); i++ {
		if n, err := strconv.Atoi(strings.TrimSuffix(w[i], ",")); err == nil && n > 0 && n < 100 && strings.EqualFold(w[i+1], w[0]) {
			return strings.Join(w[i+1:], " ")
		}
	}
	return s
}

// DisplayTitle is what customers see a matched title as: TMDB's title
// (cached in English) for an English or unlabelled copy whose name it
// overlaps, so copies merged under one id show one name ("The Matrix 2
// Reloaded" and "Matrix Reloaded" are "The Matrix Reloaded"); else the
// parsed title. Overlap guards a wrong id ("Monster Trucks" never shows as
// "Monsters and Men"), and a part the parsed title brackets stays: "Gladiator
// [Extended Cut]", "UFC 300 [Main Card]".
func DisplayTitle(title, tmdbTitle, lang string) string {
	if tmdbTitle == "" || title == "" || lang != "" && lang != "English" {
		return title
	}
	p, t := titleWords(title), titleWords(tmdbTitle)
	if !containsAll(p, t) && !containsAll(t, p) {
		return title
	}
	out := tmdbTitle
	for _, b := range bracketed.FindAllString(title, -1) {
		if !strings.Contains(Key(tmdbTitle), Key(b)) {
			out += " " + b
		}
	}
	return out
}

var romanNumerals = map[string]string{"ii": "2", "iii": "3", "iv": "4", "v": "5", "vi": "6", "vii": "7", "viii": "8", "ix": "9", "x": "10"}

// titleWords are a title's words to compare, numerals as numbers ("Part
// II" is "Part 2").
func titleWords(s string) []string {
	w := strings.Fields(looseKey(s))
	for i, x := range w {
		if n, ok := romanNumerals[x]; ok {
			w[i] = n
		}
	}
	return w
}

// containsAll: every word of sub is in words.
func containsAll(words, sub []string) bool {
	for _, w := range sub {
		if !slices.Contains(words, w) {
			return false
		}
	}
	return len(sub) > 0
}
