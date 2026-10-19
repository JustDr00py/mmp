package channel

import (
	"strings"
	"testing"
)

func TestNormalizeName(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		// Quality decorations.
		{"ABC HD", "abc"},
		{"ABC FHD", "abc"},
		{"NBC (East) FHD", "nbc east"},
		{"Sports 4K", "sports"},
		{"Movie 1080p", "movie"},
		{"Test HEVC", "test"},
		// Country prefixes and standalone codes.
		{"US: ABC East", "abc east"},
		{"USA: NBC", "nbc"},
		{"USA Network", "usa network"}, // "USA" without separator stays
		{"United States - ESPN", "espn"},
		{"TF1 FR", "tf1"},
		{"Canal+ Sport", "canal sport"}, // '+' is punctuation
		// Separators fold to spaces.
		{"Sky-Sports_Main.Event", "sky sports main event"},
		{"Das|Erste", "das erste"},
		// Accents.
		{"Série Club HD", "serie club"},
		{"TVE 1 Ñ HD", "tve 1 n"}, // Ñ folds to n (match key)
		{"ß channel", "ss channel"},
		// Whole name is decoration: keep quality-stripped words.
		{"US HD", "us"},
		{"HD", "hd"},
		// Case and whitespace.
		{"  aBc   EaSt  ", "abc east"},
		// Superscript tags.
		{"MTV ᴿᴬᵂ", "mtv"},
		{"MTVᴿᴬᵂ", "mtv"},
		{"ESPN ᴴᴰ", "espn"},
		{"Fox ᵁᴴᴰ ⁶⁰ᶠᵖˢ", "fox"},
		{"Nick ᴸᴵⱽᴱ", "nick"},
	}
	for _, tt := range tests {
		if got := NormalizeName(tt.in); got != tt.want {
			t.Errorf("NormalizeName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestNormalizeGroupsEquiv is the property the matcher relies on: names
// that are the same channel from different providers collide on one key.
func TestNormalizeGroupsEquiv(t *testing.T) {
	groups := [][]string{
		{"US: ABC East HD", "ABC EAST FHD", "abc-east (us)"},
		{"Das Erste HD", "DAS ERSTE", "de: Das Erste FHD"},
	}
	seen := map[string]string{}
	for _, g := range groups {
		key := NormalizeName(g[0])
		for _, name := range g[1:] {
			if got := NormalizeName(name); got != key {
				t.Errorf("%q -> %q, want key %q", name, got, key)
			}
		}
		if prev, dup := seen[key]; dup {
			t.Errorf("key %q shared by two different groups (%s)", key, prev)
		}
		seen[key] = strings.Join(g, "/")
	}
}

// TestNormalizeDistinctStays: different channels must not collide.
func TestNormalizeDistinctStays(t *testing.T) {
	a, b := NormalizeName("ABC News"), NormalizeName("ABC Family")
	if a == b {
		t.Errorf("distinct channels collided on %q", a)
	}
}
