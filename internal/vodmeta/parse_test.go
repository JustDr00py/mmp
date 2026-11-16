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
		// Tags after the year no longer hide it; a named language is the file's.
		{"EN ★ Topakk - 2025 [FILIPINO] [HD]", "", Parsed{"Topakk", 2025, "hd", "Filipino"}},
		{"EN ★ Trunk Locked In - 2023 [DEUTCH] [4K]", "", Parsed{"Trunk Locked In", 2023, "4k", "German"}},
		{"EN ★ Avengers  Infinity War - 2018 [3D] [FHD]", "", Parsed{"Avengers Infinity War", 2018, "fhd", "English"}},
		{"EN ★ Fall 2: Deadpoint - 2026 [CAM-VERSION] [FHD]", "", Parsed{"Fall 2: Deadpoint", 2026, "fhd", "English"}},
		{"Vivant [Multi-Audio]", "", Parsed{"Vivant", 0, "", ""}},
		{"EN ★ Day One [Muti Au/Sub]", "", Parsed{"Day One", 0, "", "English"}},
		// A part that tells copies apart stays.
		{"EN ★ UFC 328: Chimaev vs Strickland - 2026 [Main Card] [FHD]", "", Parsed{"UFC 328: Chimaev vs Strickland [Main Card]", 2026, "fhd", "English"}},
		// Loose words after a bracketed year are a panel's labels.
		{"EN - The Color Purple (1985) STEVEN SPIELBERG", "", Parsed{"The Color Purple", 1985, "", "English"}},
		{"EN - Monkey Businessmen (1946) - THE THREE STOOGES COLLECTION", "", Parsed{"Monkey Businessmen", 1946, "", "English"}},
		{"EN - Agatha Christie's Miss Marple (1984-1992)", "", Parsed{"Agatha Christie's Miss Marple", 1984, "", "English"}},
		{"Gladiator (2000) [Extended Cut]", "", Parsed{"Gladiator [Extended Cut]", 2000, "", ""}},
		// Labels no language is known for.
		{"SW ★ Invisible Man - 2020 [FHD]", "", Parsed{"Invisible Man", 2020, "fhd", "Swedish"}},
		{"ETR ★ You Are Everywhere", "", Parsed{"You Are Everywhere", 0, "", ""}},
		{"NF - Gladbeck: The Hostage Crisis  (2022)", "", Parsed{"Gladbeck: The Hostage Crisis", 2022, "", ""}},
		{"NF - NL - Annabelle: Creation", "", Parsed{"Annabelle: Creation", 0, "", "Dutch"}},
		{"EN ★ IF - 2024 [4K]", "", Parsed{"IF", 2024, "4k", "English"}},
		{"EN ★ PK - 2014", "", Parsed{"PK", 2014, "", "English"}},
		{"EN ★ WALL·E - 2008 [FHD]", "", Parsed{"WALL·E", 2008, "fhd", "English"}},
		{"JJ+E - 2021 [HD]", "", Parsed{"JJ+E", 2021, "hd", ""}},
		{"A+ - Tetris (2023)", "", Parsed{"Tetris", 2023, "", ""}},
		{"D+ - Wild Chile (GB)", "", Parsed{"Wild Chile", 0, "", "English"}},
		{"06 EN - Friday The 13th Part VI Jason Lives (1986)", "", Parsed{"Friday The 13th Part VI Jason Lives", 1986, "", "English"}},
		{"EN - Hats Off (2005) (DOC) LAUREL & HARDY", "", Parsed{"Hats Off", 2005, "", "English"}},
		{"The Convert 2023 BLURAY 1080p BluRay x264 AAC5 1  YTS MX", "", Parsed{"The Convert", 2023, "fhd", ""}},
		{"ABBA - The Movie (1977)", "", Parsed{"ABBA - The Movie", 1977, "", ""}},
		{"AC/DC Live at River Plate (2011)", "", Parsed{"AC/DC Live at River Plate", 2011, "", ""}},
		{"EN (2020) MTV Video Music Awards (2020)", "", Parsed{"EN MTV Video Music Awards", 2020, "", ""}},
		{"HD", "", Parsed{"HD", 0, "", ""}},
		{"  ", "", Parsed{"", 0, "", ""}},
	} {
		if got := Parse(tc.raw, tc.cat); got != tc.want {
			t.Errorf("Parse(%q, %q) = %+v, want %+v", tc.raw, tc.cat, got, tc.want)
		}
	}
}

func TestParseSeries(t *testing.T) {
	for _, tc := range []struct {
		raw, released string
		want          Parsed
	}{
		{"Taj Mahal 1989 [Multi-Audio]", "2020-01-14", Parsed{"Taj Mahal 1989", 2020, "", ""}},
		{"Taj Mahal 1989 [Multi-Audio]", "", Parsed{"Taj Mahal", 1989, "", ""}},
		{"Dark 2017", "2017-12-01", Parsed{"Dark", 2017, "", ""}},
		{"The Order (2018-2020)", "2019-03-07", Parsed{"The Order", 2018, "", ""}}, // bracketed: the name's
		{"Vivant [Multi-Audio]", "2023-08-16", Parsed{"Vivant", 2023, "", ""}},
	} {
		if got := ParseSeries(tc.raw, "", tc.released); got != tc.want {
			t.Errorf("ParseSeries(%q, %q) = %+v, want %+v", tc.raw, tc.released, got, tc.want)
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
