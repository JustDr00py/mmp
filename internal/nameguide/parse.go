// Package nameguide builds a guide for channels that have none from what
// their stream names say. Event feeds are often named like
// "Next | Jets @ Patriots | NFL Game Pass | 2026-10-18 | 17:00": a status,
// a title, details, and when it starts. Parse reads such names; Programs
// turns one into guide entries (up next, the event, ended).
package nameguide

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Event is what a stream name says.
type Event struct {
	Status string     // normalised: "next", "live", "ended", or ""
	Title  string     // e.g. "UFC Fight Night: Joaquin Buckley vs Mike Malott (Main Card)"
	Desc   string     // the remaining parts, e.g. "Ultimate Fighting Championship"
	Start  *time.Time // when it starts (UTC), if the name says
	Piped  bool       // the name is in the "a | b | c" style
}

// statuses maps a name's first part to a status.
var statuses = map[string]string{
	"next": "next", "upcoming": "next", "coming up": "next", "soon": "next", "later": "next", "tbd": "next",
	"live": "live", "now": "live", "on now": "live", "on air": "live",
	"end": "ended", "ended": "ended", "finished": "ended", "final": "ended", "replay": "ended", "off": "ended",
	"offline": "ended",
}

var (
	isoDate   = regexp.MustCompile(`\b(\d{4})-(\d{1,2})-(\d{1,2})\b`)
	slashDate = regexp.MustCompile(`\b(\d{1,2})[/.](\d{1,2})[/.](\d{4})\b`) // day/month/year
	clock     = regexp.MustCompile(`(?i)\b(\d{1,2})[:.](\d{2})\s*(am|pm)?\b`)
	dayMonth  = regexp.MustCompile(`(?i)\b(\d{1,2})(?:st|nd|rd|th)?\s+(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?\b`)
	monthDay  = regexp.MustCompile(`(?i)\b(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?\s+(\d{1,2})(?:st|nd|rd|th)?\b`)
	year      = regexp.MustCompile(`\b(20\d{2})\b`)
	word      = regexp.MustCompile(`[A-Za-z]+`)
)

var months = map[string]time.Month{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8,
	"sep": 9, "oct": 10, "nov": 11, "dec": 12}

// whenWords may appear in a date/time part besides numbers and zones.
var whenWords = map[string]bool{"am": true, "pm": true, "st": true, "nd": true, "rd": true, "th": true,
	"mon": true, "tue": true, "tues": true, "wed": true, "thu": true, "thur": true, "thurs": true, "fri": true,
	"sat": true, "sun": true, "monday": true, "tuesday": true, "wednesday": true, "thursday": true, "friday": true,
	"saturday": true, "sunday": true, "at": true, "kickoff": true, "ko": true, "start": true, "starts": true}

// Parse reads a stream name. now picks the year of a date given without
// one, and the day of a time given without a date (the nearest one);
// times without a zone are in loc.
func Parse(name string, now time.Time, loc *time.Location) Event {
	var ev Event
	parts := splitParts(name)
	ev.Piped = len(parts) > 1
	if len(parts) > 1 {
		if s, ok := statuses[strings.ToLower(parts[0])]; ok {
			ev.Status = s
			parts = parts[1:]
		}
	}
	var text, when []string
	for _, p := range parts {
		if ev.Piped && isWhen(p) {
			when = append(when, p)
		} else {
			text = append(text, p)
		}
	}
	if len(when) > 0 {
		ev.Start = whenOf(strings.Join(when, " "), now, loc)
	}
	switch {
	case len(text) == 0:
	case len(text) > 1 && isMatchup(text[1]) && !isMatchup(text[0]):
		ev.Title = text[0] + ": " + text[1]
		ev.Desc = strings.Join(text[2:], " · ")
	default:
		ev.Title = text[0]
		ev.Desc = strings.Join(text[1:], " · ")
	}
	return ev
}

func splitParts(name string) []string {
	var out []string
	for _, p := range strings.Split(name, "|") {
		if p = strings.Join(strings.Fields(p), " "); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// isMatchup reports whether a part names two sides ("Jets @ Patriots").
func isMatchup(s string) bool {
	l := " " + strings.ToLower(s) + " "
	for _, sep := range []string{" vs ", " vs. ", " v ", " @ ", " at "} {
		if strings.Contains(l, sep) {
			return true
		}
	}
	return false
}

// isWhen reports whether a part is only a date and/or time: numbers,
// months, weekdays, am/pm, time zones and a country tag like "(SE)".
func isWhen(p string) bool {
	if !clock.MatchString(p) && !isoDate.MatchString(p) && !slashDate.MatchString(p) &&
		!dayMonth.MatchString(p) && !monthDay.MatchString(p) {
		return false
	}
	for _, w := range word.FindAllString(p, -1) {
		lw := strings.ToLower(w)
		if _, ok := months[lw[:min(3, len(lw))]]; ok && (len(lw) == 3 || monthName(lw)) {
			continue
		}
		if whenWords[lw] || zoneOf(w) != nil {
			continue
		}
		if len(w) == 2 && strings.ToUpper(w) == w { // country tag: (SE), (UK)
			continue
		}
		return false
	}
	return true
}

func monthName(w string) bool {
	for _, m := range []string{"january", "february", "march", "april", "june", "july", "august", "september",
		"october", "november", "december", "sept"} {
		if w == m {
			return true
		}
	}
	return false
}

// whenOf works out the start time a date/time text gives, or nil.
func whenOf(s string, now time.Time, loc *time.Location) *time.Time {
	m := clock.FindStringSubmatch(slashDate.ReplaceAllString(isoDate.ReplaceAllString(s, " "), " ")) // "18.10.2026" is no time
	if m == nil {
		return nil // a date alone does not say when to start
	}
	hour, _ := strconv.Atoi(m[1])
	minute, _ := strconv.Atoi(m[2])
	switch strings.ToLower(m[3]) {
	case "pm":
		if hour < 12 {
			hour += 12
		}
	case "am":
		if hour == 12 {
			hour = 0
		}
	}
	if hour > 23 || minute > 59 {
		return nil
	}
	for _, w := range word.FindAllString(s, -1) {
		if z := zoneOf(w); z != nil {
			loc = z
			break
		}
	}
	local := now.In(loc)
	y, mo, d, dated := 0, time.Month(0), 0, false
	switch {
	case isoDate.MatchString(s):
		p := isoDate.FindStringSubmatch(s)
		y, _ = strconv.Atoi(p[1])
		mi, _ := strconv.Atoi(p[2])
		d, _ = strconv.Atoi(p[3])
		mo, dated = time.Month(mi), true
	case slashDate.MatchString(s):
		p := slashDate.FindStringSubmatch(s)
		d, _ = strconv.Atoi(p[1])
		mi, _ := strconv.Atoi(p[2])
		y, _ = strconv.Atoi(p[3])
		mo, dated = time.Month(mi), true
	case dayMonth.MatchString(s):
		p := dayMonth.FindStringSubmatch(s)
		d, _ = strconv.Atoi(p[1])
		mo, dated = months[strings.ToLower(p[2])], true
	case monthDay.MatchString(s):
		p := monthDay.FindStringSubmatch(s)
		d, _ = strconv.Atoi(p[2])
		mo, dated = months[strings.ToLower(p[1])], true
	}
	var t time.Time
	switch {
	case dated && y == 0: // the year nearest to now
		if p := year.FindStringSubmatch(s); p != nil {
			y, _ = strconv.Atoi(p[1])
			t = time.Date(y, mo, d, hour, minute, 0, 0, loc)
		} else {
			t = nearest(local, func(y int) time.Time { return time.Date(y, mo, d, hour, minute, 0, 0, loc) },
				local.Year()-1, local.Year(), local.Year()+1)
		}
	case dated:
		t = time.Date(y, mo, d, hour, minute, 0, 0, loc)
	default: // a time alone: the nearest such time to now
		t = nearest(local, func(off int) time.Time {
			return time.Date(local.Year(), local.Month(), local.Day()+off, hour, minute, 0, 0, loc)
		}, -1, 0, 1)
	}
	if t.Month() != mo && dated { // e.g. 31 Feb
		return nil
	}
	t = t.UTC()
	return &t
}

func nearest(now time.Time, at func(int) time.Time, opts ...int) time.Time {
	best := at(opts[0])
	for _, o := range opts[1:] {
		if c := at(o); absDur(c.Sub(now)) < absDur(best.Sub(now)) {
			best = c
		}
	}
	return best
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
