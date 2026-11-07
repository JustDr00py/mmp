package eventroute

import (
	"testing"
	"time"
)

var teams = []Team{
	{1, "Atlanta Hawks"}, {2, "Boston Celtics"}, {3, "Charlotte Hornets"}, {4, "Washington Wizards"},
	{5, "Toronto Raptors"}, {6, "Milwaukee Bucks"}, {7, "Golen State Warriors"}, {8, "LA Lakers"},
	{9, "Philadelphia 76ers"}, {10, "Cleveland Cavaliers"}, {11, "Orlando Magic"},
}

func ny(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no zone data")
	}
	return loc
}

func TestTeamKeys(t *testing.T) {
	for _, tt := range []struct {
		channel, title string
		want           bool
	}{
		{"Toronto Raptors", "Milwaukee Bucks vs Toronto Raptors", true},
		{"Toronto Raptors", "NBA: Toronto vs Milwaukee", true}, // a city only one team has
		{"Golen State Warriors", "Orlando Magic vs Golden State Warriors", true},
		{"LA Lakers", "Los Angeles Lakers vs Portland Trail Blazers", true},
		{"LA Lakers", "Los Angeles Clippers vs Utah Jazz", false}, // two teams share LA
		{"Philadelphia 76ers", "NBA: Sixers vs Cavaliers", true},
		{"Cleveland Cavaliers", "NBA: Sixers vs Cavaliers", true},
		{"Miami Heat", "Heatwave Classic", false}, // whole words only
		{"Seattle Storm", "Storm vs Aces", true},  // not in the table: its last word
	} {
		if got := teamKeys(tt.channel).in(norm(tt.title)); got != tt.want {
			t.Errorf("%q in %q = %v, want %v", tt.channel, tt.title, got, tt.want)
		}
	}
}

func TestPlan(t *testing.T) {
	loc := ny(t)
	now := time.Date(2026, 11, 6, 20, 15, 0, 0, loc) // during the 8:00 PM games
	slots := []Slot{
		{ID: 101, Name: "NBA 01", Streams: []string{
			"US - NBA GAME 01 : ATLANTA HAWKS @ WASHINGTON WIZARDS NOV 6 – 7:00 PM ET / NOV 7 – 12:00 AM UK",
			"Atlanta Hawks vs Washington Wizards @ Nov 6 7:00 PM :NBA  01",
			"Atlanta Hawks vs Washington Wizards @ Nov 6 7:00 PM :NBA  01",
			"|US| NBA GAME 01:", // empty: no vote
			"USA NBA 01: Boston Celtics vs Charlotte Hornets @ 07:00 PM EST", // another slot's game: outvoted
		}},
		{ID: 102, Name: "NBA 02", Streams: []string{
			"Charlotte Hornets vs Boston Celtics @ Nov 6 7:00 PM :NBA  02",
			"US - NBA GAME 02 : CHARLOTTE HORNETS @ BOSTON CELTICS NOV 6 – 7:00 PM ET / NOV 7 – 12:00 AM UK",
		}},
		{ID: 106, Name: "NBA 06", Streams: []string{
			"Milwaukee Bucks vs Toronto Raptors @ Nov 6 8:00 PM :NBA  06",
			"US ★ NBA 06 : ",
		}},
		{ID: 104, Name: "NBA 04", Streams: []string{
			"Orlando Magic vs Phoenix Suns @ Mar 31 7:00 PM :NBA  01", // a name left from March
		}},
		{ID: 110, Name: "NBA 10", Streams: []string{":NBA  10"}},
		{ID: 108, Name: "NBA 08", Streams: []string{ // starts at 10: not yet (30 min before)
			"Los Angeles Lakers vs Portland Trail Blazers @ Nov 6 10:00 PM :NBA  08",
		}},
	}
	got := Plan(slots, teams, now, loc, 30*time.Minute, 4*time.Hour)
	want := map[int64]int64{1: 101, 4: 101, 2: 102, 3: 102, 5: 106, 6: 106}
	if len(got) != len(want) {
		t.Fatalf("got %d overrides, want %d: %+v", len(got), len(want), got)
	}
	for _, o := range got {
		if want[o.ChannelID] != o.SourceID {
			t.Errorf("channel %d routed to %d, want %d", o.ChannelID, o.SourceID, want[o.ChannelID])
		}
	}
	for _, o := range got {
		if o.ChannelID == 5 {
			start := time.Date(2026, 11, 6, 20, 0, 0, 0, loc)
			if o.Title != "Milwaukee Bucks vs Toronto Raptors" || !o.Start.Equal(start.Add(-30*time.Minute)) ||
				!o.End.Equal(start.Add(4*time.Hour)) || o.SourceName != "NBA 06" {
				t.Errorf("Raptors override = %+v", o)
			}
		}
		if o.ChannelID == 1 && o.Title != "Atlanta Hawks @ Washington Wizards" {
			t.Errorf("Hawks title = %q (capitals tidied)", o.Title)
		}
	}
}

func TestPlanWindow(t *testing.T) {
	loc := ny(t)
	start := time.Date(2026, 11, 6, 22, 0, 0, 0, loc)
	slots := []Slot{{ID: 108, Name: "NBA 08", Streams: []string{
		"Los Angeles Lakers vs Portland Trail Blazers @ Nov 6 10:00 PM :NBA  08"}}}
	for _, tt := range []struct {
		at   time.Time
		want int
	}{
		{start.Add(-31 * time.Minute), 0},
		{start.Add(-30 * time.Minute), 1},
		{start.Add(4*time.Hour - time.Minute), 1},
		{start.Add(4 * time.Hour), 0},
	} {
		if got := Plan(slots, teams, tt.at, loc, 30*time.Minute, 4*time.Hour); len(got) != tt.want {
			t.Errorf("at %v: %d overrides, want %d", tt.at, len(got), tt.want)
		}
	}
}

func TestPlanNotAGame(t *testing.T) {
	loc := ny(t)
	now := time.Date(2026, 11, 6, 15, 10, 0, 0, loc)
	slots := []Slot{{ID: 150, Name: "ESPN+ 50", Streams: []string{
		"US - ESPN PLUS 50 : TORONTO RAPTORS POSTGAME NOV 6 – 3:00 PM ET / 8:00 PM UK"}}}
	if got := Plan(slots, teams, now, loc, 30*time.Minute, 4*time.Hour); len(got) != 0 {
		t.Errorf("a show is no game: %+v", got)
	}
}

func TestValidate(t *testing.T) {
	s := DefaultSettings()
	if err := s.Validate(); err != nil {
		t.Errorf("defaults: %v", err)
	}
	s.Enabled = true
	if s.Validate() == nil {
		t.Error("on without rules should fail")
	}
	s.Rules = []Rule{{SourceGroupIDs: []int64{1}, TargetGroupIDs: []int64{1, 2}}}
	if s.Validate() == nil {
		t.Error("overlapping groups should fail")
	}
	s.Rules[0].TargetGroupIDs = []int64{2, 3}
	if err := s.Validate(); err != nil {
		t.Error(err)
	}
}
