package nameguide

import (
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	now := time.Date(2026, 10, 18, 13, 28, 0, 0, time.UTC)
	utc := func(s string) *time.Time {
		v, err := time.Parse("2006-01-02 15:04", s)
		if err != nil {
			t.Fatal(err)
		}
		return &v
	}
	for _, tt := range []struct {
		name string
		want Event
		loc  *time.Location
	}{
		{"End | UFC Fight Night | Joaquin Buckley vs Mike Malott (Main Card) | Ultimate Fighting Championship | 2026-10-18 | 00:00 (GMT)",
			Event{Status: "ended", Title: "UFC Fight Night: Joaquin Buckley vs Mike Malott (Main Card)",
				Desc: "Ultimate Fighting Championship", Start: utc("2026-10-18 00:00"), Piped: true}, nil},
		{"Next | Jacksonville Jaguars Vs Houston Texans",
			Event{Status: "next", Title: "Jacksonville Jaguars Vs Houston Texans", Piped: true}, nil},
		{"Next | Jets @ Patriots | NFL Game Pass | 2026-10-18 | 17:00",
			Event{Status: "next", Title: "Jets @ Patriots", Desc: "NFL Game Pass", Start: utc("2026-10-18 17:00"), Piped: true}, nil},
		{"Next | Jets @ Patriots | NFL Game Pass | 2026-10-18 | 17:00", // zoneless times in the configured zone
			Event{Status: "next", Title: "Jets @ Patriots", Desc: "NFL Game Pass", Start: utc("2026-10-18 21:00"), Piped: true},
			mustLoad(t, "America/New_York")},
		{"NEXT | NFL: SAINTS @ GIANTS | Sun 18 Oct 18:55 CEST (SE)", // CEST is UTC+2
			Event{Status: "next", Title: "NFL: SAINTS @ GIANTS", Start: utc("2026-10-18 16:55"), Piped: true}, nil},
		{"LIVE | NBA | Lakers vs Celtics | Oct 19 7:30 PM ET",
			Event{Status: "live", Title: "NBA: Lakers vs Celtics", Start: utc("2026-10-19 23:30"), Piped: true}, nil},
		{"Premier League | Arsenal v Chelsea | 18.10.2026 | 16.30 BST",
			Event{Title: "Premier League: Arsenal v Chelsea", Start: utc("2026-10-18 15:30"), Piped: true}, nil},
		{"PPV 1 | 20:00", // a time alone: the nearest such time
			Event{Title: "PPV 1", Start: utc("2026-10-18 20:00"), Piped: true}, nil},
		{"Event | Dec 31 23:00 UTC", // without a year: the nearest year
			Event{Title: "Event", Start: utc("2026-12-31 23:00"), Piped: true}, nil},
		{"US: ESPN HD", Event{Title: "US: ESPN HD"}, nil}, // not an event name
		{"Match of the Day 2 | BBC One", Event{Title: "Match of the Day 2", Desc: "BBC One", Piped: true}, nil},
		{"End | ", Event{Title: "End"}, nil}, // a status needs something after it
	} {
		loc := tt.loc
		if loc == nil {
			loc = time.UTC
		}
		got := Parse(tt.name, now, loc)
		if got.Status != tt.want.Status || got.Title != tt.want.Title || got.Desc != tt.want.Desc || got.Piped != tt.want.Piped ||
			(got.Start == nil) != (tt.want.Start == nil) || (got.Start != nil && !got.Start.Equal(*tt.want.Start)) {
			t.Errorf("Parse(%q)\n got %+v start %v\nwant %+v start %v", tt.name, got, got.Start, tt.want, tt.want.Start)
		}
	}
}

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("no zone data for %s", name)
	}
	return loc
}

func TestPrograms(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 10, 18, h, 0, 0, 0, time.UTC) }
	start := at(17)
	ev := Event{Title: "Jets @ Patriots", Desc: "NFL Game Pass", Start: &start}
	got := Programs(ev, at(13), at(24))
	want := []Program{
		{at(13), at(17), "Up next: Jets @ Patriots", "NFL Game Pass"},
		{at(17), at(20), "Jets @ Patriots", "NFL Game Pass"},
		{at(20), at(24), "Ended: Jets @ Patriots", "NFL Game Pass"},
	}
	if len(got) != len(want) {
		t.Fatalf("programs = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("program %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// No start: the title fills the window in 6-hour blocks aligned in UTC.
	got = Programs(Event{Status: "next", Title: "Jaguars Vs Texans"}, at(13), at(24))
	if len(got) != 2 || got[0].Stop != at(18) || got[1].Start != at(18) || got[1].Stop != at(24) ||
		got[0].Title != "Up next: Jaguars Vs Texans" {
		t.Errorf("all-day = %+v", got)
	}
	// An event wholly before the window: only "Ended".
	if got := Programs(ev, at(21), at(23)); len(got) != 1 || got[0].Title != "Ended: Jets @ Patriots" {
		t.Errorf("after the event = %+v", got)
	}
	// "Up next" and "Ended" stay within a day of the event.
	got = Programs(ev, at(17).Add(-72*time.Hour), at(17).Add(72*time.Hour))
	if first, last := got[0], got[len(got)-1]; !first.Start.Equal(at(17).Add(-24*time.Hour)) || !last.Stop.Equal(at(20).Add(24*time.Hour)) {
		t.Errorf("filler spans %v .. %v, want a day either side", first.Start, last.Stop)
	}
	if got := Programs(Event{}, at(1), at(2)); got != nil {
		t.Errorf("no title = %+v", got)
	}
}

func TestSettingsFor(t *testing.T) {
	now := time.Date(2026, 10, 18, 13, 0, 0, 0, time.UTC)
	s := DefaultSettings()
	if ev, ok := s.For("PPV 1", "Next | Jets @ Patriots | 2026-10-18 | 17:00", now); !ok || ev.Title != "Jets @ Patriots" {
		t.Errorf("event stream = %+v %v", ev, ok)
	}
	if ev, ok := s.For("ESPN", "US: ESPN HD", now); !ok || ev.Title != "ESPN" {
		t.Errorf("plain channel = %+v %v, want an all-day entry with the channel's name", ev, ok)
	}
	s.All = false
	if _, ok := s.For("ESPN", "US: ESPN HD", now); ok {
		t.Error("plain channel with All off")
	}
	s.Enabled = false
	if _, ok := s.For("PPV 1", "Next | Jets @ Patriots", now); ok {
		t.Error("disabled")
	}
	if err := (Settings{Zone: "Mars/Olympus"}).Validate(); err == nil {
		t.Error("bad zone accepted")
	}
}
