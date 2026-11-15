package vodmeta

import "testing"

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		raw, cat string
		want     Parsed
	}{
		{"EN | The Matrix 4K (1999)", "", Parsed{"The Matrix", 1999, "4k", "English"}},
		{"|FR| Le Parrain (1972)", "", Parsed{"Le Parrain", 1972, "", "French"}},
		{"[DE] Der Untergang (2004) [FHD]", "", Parsed{"Der Untergang", 2004, "fhd", "German"}},
		{"The Matrix (1999) [MULTI-SUB]", "EN | Action", Parsed{"The Matrix", 1999, "", "English"}},
		{"4K-EN - Dune Part Two 2024", "", Parsed{"Dune Part Two", 2024, "4k", "English"}},
		{"Blade Runner 2049 (2017)", "", Parsed{"Blade Runner 2049", 2017, "", ""}},
		{"Blade Runner 2049", "", Parsed{"Blade Runner 2049", 0, "", ""}},
		{"Avatar: The Way of Water - 2022", "", Parsed{"Avatar: The Way of Water", 2022, "", ""}},
		{"The.Matrix.1999.1080p.BluRay.x264", "", Parsed{"The Matrix", 1999, "fhd", ""}},
		{"Spider-Man: No Way Home (2021) (EN)", "", Parsed{"Spider-Man: No Way Home", 2021, "", "English"}},
		{"Breaking Bad (2008) S01", "", Parsed{"Breaking Bad", 2008, "", ""}},
		{"The Office - Season 2", "", Parsed{"The Office", 0, "", ""}},
		{"It: Chapter Two (2019)", "", Parsed{"It: Chapter Two", 2019, "", ""}},
		{"1917", "", Parsed{"1917", 0, "", ""}},
		{"2012 (2009)", "", Parsed{"2012", 2009, "", ""}},
		{"Gladiator (Extended Cut) (2000) HEVC", "", Parsed{"Gladiator (Extended Cut)", 2000, "", ""}},
		{"Amélie (2001) - FR", "", Parsed{"Amélie", 2001, "", "French"}},
		{"Up (2009)", "|FR| Animation", Parsed{"Up", 2009, "", "French"}},
		{"EN ★ Flamin' Hot - 2023 [4K]", "", Parsed{"Flamin' Hot", 2023, "4k", "English"}},
		{"EN  ★ Five Blind Dates - 2024 [FHD]", "", Parsed{"Five Blind Dates", 2024, "fhd", "English"}},
		{"EN ★ Jurassic World  Fallen Kingdom - 2018 [4K]", "", Parsed{"Jurassic World Fallen Kingdom", 2018, "4k", "English"}},
		{"EN ★ A Merry Little Ex-Mas - 2025 [4K]", "", Parsed{"A Merry Little Ex-Mas", 2025, "4k", "English"}},
		{"FR • Le Parrain (1972)", "", Parsed{"Le Parrain", 1972, "", "French"}},
		{"DE ► Der Untergang – 2004", "", Parsed{"Der Untergang", 2004, "", "German"}},
		{"HD", "", Parsed{"HD", 0, "", ""}},
		{"  ", "", Parsed{"", 0, "", ""}},
	} {
		if got := Parse(tc.raw, tc.cat); got != tc.want {
			t.Errorf("Parse(%q, %q) = %+v, want %+v", tc.raw, tc.cat, got, tc.want)
		}
	}
}

func TestKey(t *testing.T) {
	for in, want := range map[string]string{
		"Spider-Man: No Way Home": "spider man no way home",
		"Amélie":                  "amelie",
		"Fast & Furious":          "fast and furious",
		"Don't Look Up":           "dont look up",
		"  The   Matrix ":         "the matrix",
	} {
		if got := Key(in); got != want {
			t.Errorf("Key(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDisplay(t *testing.T) {
	if got := (Parsed{Title: "The Matrix", Year: 1999}).Display(); got != "The Matrix (1999)" {
		t.Errorf("Display = %q", got)
	}
	if got := (Parsed{Title: "The Matrix"}).Display(); got != "The Matrix" {
		t.Errorf("Display = %q", got)
	}
}
