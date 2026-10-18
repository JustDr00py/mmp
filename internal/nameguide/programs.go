package nameguide

import (
	"time"
)

// EventLength is how long an event is assumed to run when its name only
// says when it starts.
const EventLength = 3 * time.Hour

// block is the length of the entries that fill time with no event: long
// enough to keep guides small, short enough that players show the day.
const block = 6 * time.Hour

// around is how long "Up next" runs before an event and "Ended" after it:
// further out the name is unlikely to still describe the stream.
const around = 24 * time.Hour

// Program is one generated guide entry.
type Program struct {
	Start, Stop time.Time
	Title, Desc string
}

// Programs fills [since, until) for ev. An event with a start time gets
// "Up next: …" for a day before it, itself for EventLength, then
// "Ended: …" for a day; one without gets its title (with its status)
// throughout. Filler is cut into blocks aligned in UTC.
func Programs(ev Event, since, until time.Time) []Program {
	if ev.Title == "" || !until.After(since) {
		return nil
	}
	if ev.Start == nil {
		title := ev.Title
		switch ev.Status {
		case "next":
			title = "Up next: " + ev.Title
		case "ended":
			title = "Ended: " + ev.Title
		}
		return fill(since, until, title, ev.Desc)
	}
	start, end := *ev.Start, ev.Start.Add(EventLength)
	var out []Program
	if from, to := maxTime(since, start.Add(-around)), minTime(start, until); from.Before(to) {
		out = append(out, fill(from, to, "Up next: "+ev.Title, ev.Desc)...)
	}
	if start.Before(until) && end.After(since) {
		out = append(out, Program{Start: start, Stop: end, Title: ev.Title, Desc: ev.Desc})
	}
	if from, to := maxTime(end, since), minTime(end.Add(around), until); from.Before(to) {
		out = append(out, fill(from, to, "Ended: "+ev.Title, ev.Desc)...)
	}
	return out
}

// fill covers [from, to) with blocks titled title, cut at multiples of
// block (UTC) so the same hours give the same entries on every request.
func fill(from, to time.Time, title, desc string) []Program {
	var out []Program
	for at := from; at.Before(to); {
		next := minTime(at.UTC().Truncate(block).Add(block), to)
		out = append(out, Program{Start: at, Stop: next, Title: title, Desc: desc})
		at = next
	}
	return out
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// NowTitle is what the stream-name guide says is on at now for a channel,
// or "" when it gives the channel none.
func (s Settings) NowTitle(channelName, streamName string, now time.Time) string {
	ev, ok := s.For(channelName, streamName, now)
	if !ok {
		return ""
	}
	if ps := Programs(ev, now, now.Add(time.Second)); len(ps) > 0 {
		return ps[0].Title
	}
	return ""
}
