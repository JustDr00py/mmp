package vodmeta

import "testing"

func TestEpisodeTitle(t *testing.T) {
	for _, tc := range []struct {
		raw, show string
		num       int
		want      string
	}{
		{"EN ★ Silicon Valley - S01E01 - Minimum Viable Product [SD]", "Silicon Valley", 1, "Minimum Viable Product"},
		{"LINGO║ Tulsa King - S01E01 - Go West, Old Man", "Tulsa King", 1, "Go West, Old Man"},
		{"EN ★ South Park - S01E09 - Mr. Hankey, the Christmas Poo [FHD]", "South Park", 9, "Mr. Hankey, the Christmas Poo"},
		{"EN ★ Breaking Bad - S03E03 - I.F.T. [4K]", "Breaking Bad", 3, "I.F.T."},
		{"EN ★ Breaking Bad - S02E09 - 4 Days Out [4K]", "Breaking Bad", 9, "4 Days Out"},
		{"EN ★ Bluey - S03E49 - Surprise! [FHD]", "Bluey", 49, "Surprise!"},
		{"EN ★ Silicon Valley - S02E08 - White Hat/Black Hat [SD]", "Silicon Valley", 8, "White Hat/Black Hat"},
		{"EN ★ One Piece - S01E51 - Fiery Cooking Battle? Sanji vs. The Beautiful Chef! [SD]", "One Piece", 51, "Fiery Cooking Battle? Sanji vs. The Beautiful Chef!"},
		{"Show - S01E01 - Pilot (Part 1) [HD]", "Show", 1, "Pilot (Part 1)"},
		{"Show - S01E01E02 - Two Parter", "Show", 1, "Two Parter"},
		{"Show 1x05 - Name (EN) [MULTI-SUB]", "Show", 5, "Name"},
		// Nothing but the show and the place.
		{"EN ★ One Piece - S05E512 - [HD]", "One Piece", 512, "Episode 512"},
		{"MULTI║ Coven Academy (2026) - S01E21", "Coven Academy", 21, "Episode 21"},
		{"EN ★ One Piece - S05E486 - [HD]", "One Piece", 87, "Episode 486"},
		{"Show 2x07 [HD]", "Show", 3, "Episode 7"},
		{"Show [HD]", "Show", 3, "Episode 3"},
		{"S01E01", "", 0, "Episode 1"},
		{"[HD]", "", 0, "[HD]"},
		// No marker: the label and the show's name go.
		{"EN ★ Tulsa King - Go West, Old Man [HD]", "Tulsa King", 1, "Go West, Old Man"},
		{"Go West, Old Man", "Tulsa King", 1, "Go West, Old Man"},
		{"Tulsa Kingdom", "Tulsa King", 1, "Tulsa Kingdom"},
		{"Episode 4", "Show", 4, "Episode 4"},
	} {
		if got := EpisodeTitle(tc.raw, tc.show, tc.num); got != tc.want {
			t.Errorf("EpisodeTitle(%q, %q, %d) = %q, want %q", tc.raw, tc.show, tc.num, got, tc.want)
		}
	}
}
