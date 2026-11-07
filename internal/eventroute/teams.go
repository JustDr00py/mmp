package eventroute

import (
	"strings"
	"unicode"
)

// team is one club: the words that name it in a game title.
type team struct {
	City     string   // "Toronto"; matches only when no other team has it
	Nickname string   // "Raptors"
	Aliases  []string // other names: "Sixers", "Cavs", "OKC"
}

// nba is the built-in NBA table. Other leagues can be added as tables of
// their own; a channel no table knows falls back to its last word.
var nba = []team{
	{"Atlanta", "Hawks", nil},
	{"Boston", "Celtics", nil},
	{"Brooklyn", "Nets", nil},
	{"Charlotte", "Hornets", nil},
	{"Chicago", "Bulls", nil},
	{"Cleveland", "Cavaliers", []string{"Cavs"}},
	{"Dallas", "Mavericks", []string{"Mavs"}},
	{"Denver", "Nuggets", nil},
	{"Detroit", "Pistons", nil},
	{"Golden State", "Warriors", []string{"GSW"}},
	{"Houston", "Rockets", nil},
	{"Indiana", "Pacers", nil},
	{"Los Angeles", "Clippers", []string{"LAC"}},
	{"Los Angeles", "Lakers", []string{"LAL"}},
	{"Memphis", "Grizzlies", []string{"Grizz"}},
	{"Miami", "Heat", nil},
	{"Milwaukee", "Bucks", nil},
	{"Minnesota", "Timberwolves", []string{"Wolves", "T-Wolves"}},
	{"New Orleans", "Pelicans", []string{"Pels"}},
	{"New York", "Knicks", nil},
	{"Oklahoma City", "Thunder", []string{"OKC"}},
	{"Orlando", "Magic", nil},
	{"Philadelphia", "76ers", []string{"Sixers", "Philly"}},
	{"Phoenix", "Suns", nil},
	{"Portland", "Trail Blazers", []string{"Blazers"}},
	{"Sacramento", "Kings", nil},
	{"San Antonio", "Spurs", nil},
	{"Toronto", "Raptors", nil},
	{"Utah", "Jazz", nil},
	{"Washington", "Wizards", nil},
}

// keys are the normalised words that name a team in a title.
type keys []string

// in reports whether any key appears in title (normalised) as whole words.
func (k keys) in(title string) bool {
	for _, w := range k {
		if strings.Contains(title, w) {
			return true
		}
	}
	return false
}

// teamKeys works out which team a channel is from its name: the table
// team whose nickname or alias the name contains ("Golen State Warriors",
// "LA Lakers"), else the name's last word and the whole name. nil when
// the name gives nothing to match on.
func teamKeys(channelName string) keys {
	name := norm(channelName)
	cities := map[string]int{}
	for _, t := range nba {
		cities[norm(t.City)]++
	}
	for _, t := range nba {
		names := append([]string{t.Nickname}, t.Aliases...)
		var k keys
		hit := false
		for _, n := range names {
			n = norm(n)
			k = append(k, n)
			hit = hit || strings.Contains(name, n)
		}
		if !hit {
			continue
		}
		if c := norm(t.City); cities[c] == 1 {
			k = append(k, c)
		}
		return k
	}
	f := strings.Fields(name)
	if len(f) == 0 {
		return nil
	}
	k := keys{" " + f[len(f)-1] + " "}
	if len(f) > 1 {
		k = append(k, name)
	}
	return k
}

// norm lowercases s, turns anything but letters and digits into single
// spaces and pads it with a space each side, so " heat " only matches the
// whole word.
func norm(s string) string {
	var b strings.Builder
	b.WriteByte(' ')
	space := true
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			space = false
		} else if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	if !space {
		b.WriteByte(' ')
	}
	return b.String()
}

// isMatchup reports whether a title names two sides ("A vs B", "A @ B"),
// so shows like "Lakers Postgame Live" are not taken for a game.
func isMatchup(title string) bool {
	l := " " + strings.ToLower(title) + " "
	for _, sep := range []string{" vs ", " vs. ", " v ", " v. ", " @ ", " at ", " x "} {
		if strings.Contains(l, sep) {
			return true
		}
	}
	return false
}
