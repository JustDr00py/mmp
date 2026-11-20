package health

import (
	"fmt"
	"math"
	"strconv"
	"time"
)

// Channel play health: how each source (backbone, else provider) in a
// channel's failover list played for viewers, from the failures it had while
// playing weighed by the hours it was watched. A source that keeps failing on
// one channel is suggested lower in that channel's list; nothing is moved
// until an admin applies the suggestion.
const (
	// ChannelHalfLife is how fast play history fades. Longer than the
	// provider score's: a feed that is bad every evening should stay
	// flagged through the day.
	ChannelHalfLife = 48 * time.Hour
	// MinPlayFailures is the failure count in the window below which a
	// source is never flagged.
	MinPlayFailures = 3
	// MaxFailuresPerHour is the play failure rate, per hour watched, above
	// which a source is flagged.
	MaxFailuresPerHour = 1.0
	// minWatchHours floors the watch time a rate divides by, so a source
	// that fails within minutes every time reads as a high rate, not ∞.
	minWatchHours = 1.0 / 6
)

// ChannelStream is one entry of a channel's failover list, in list order.
type ChannelStream struct {
	StreamID   int64
	ProviderID int64
	Backbone   string
}

// Source names a stream's source: its backbone, else its provider.
func (c ChannelStream) Source() string {
	if c.Backbone != "" {
		return c.Backbone
	}
	return "provider #" + strconv.FormatInt(c.ProviderID, 10)
}

// SourceHealth is how one source in a channel's list played over the window.
type SourceHealth struct {
	Source     string
	Streams    int     // streams of this source in the list
	FellBehind int64   // raw counts in the window
	Failures   int64   // every play failure, FellBehind included
	Hours      float64 // raw watch time in the window
	Rate       float64 // decayed failures per decayed hour watched
	Bad        bool    // flagged: suggested lower in the list
}

// Reason puts a flagged source's record in words.
func (h SourceHealth) Reason() string {
	what := "failed while playing"
	if h.FellBehind == h.Failures {
		what = "fell behind real time"
	}
	return fmt.Sprintf("%s %s %d× in %.1f h watched (last 7 days)", h.Source, what, h.Failures, h.Hours)
}

// ChannelPlay is a channel's play health and, when a source is flagged, the
// order the failover list should take.
type ChannelPlay struct {
	Sources []SourceHealth // in list order, sources with any play history
	Order   []int64        // suggested stream order; nil: keep the list as is
	// AllBad: every source in the list is flagged, so the network or the
	// channel's own feed is the likelier cause and no order is suggested.
	AllBad bool
}

// Flagged returns the flagged sources.
func (p ChannelPlay) Flagged() []SourceHealth {
	var out []SourceHealth
	for _, s := range p.Sources {
		if s.Bad {
			out = append(out, s)
		}
	}
	return out
}

// isPlayFailure: the outcomes a viewer saw as a failure while watching.
func isPlayFailure(outcome string) bool {
	return outcome == OutcomeFellBehind || outcome == OutcomeMidPlayFailure
}

// ChannelPlayHealth judges list's sources from the per-stream outcome
// buckets as of now. Buckets older than Window are ignored.
func ChannelPlayHealth(buckets []Bucket, list []ChannelStream, now time.Time) ChannelPlay {
	source := make(map[int64]string, len(list))
	var names []string
	streams := map[string]int{}
	for _, c := range list {
		src := c.Source()
		source[c.StreamID] = src
		if streams[src] == 0 {
			names = append(names, src)
		}
		streams[src]++
	}

	type acc struct {
		SourceHealth
		failed, watched float64 // decayed
		seen            bool
	}
	accs := make(map[string]*acc, len(names))
	for _, n := range names {
		accs[n] = &acc{SourceHealth: SourceHealth{Source: n, Streams: streams[n]}}
	}
	for _, b := range buckets {
		src, ok := source[b.StreamID]
		age := now.Sub(b.Start)
		if !ok || b.StreamID == 0 || b.Count <= 0 || age > Window {
			continue
		}
		decay := math.Pow(0.5, max(age, 0).Hours()/ChannelHalfLife.Hours())
		a := accs[src]
		switch {
		case b.Outcome == OutcomePlayed10m:
			a.Hours += float64(b.Count) / 6
			a.watched += float64(b.Count) / 6 * decay
			a.seen = true
		case isPlayFailure(b.Outcome):
			a.Failures += b.Count
			if b.Outcome == OutcomeFellBehind {
				a.FellBehind += b.Count
			}
			a.failed += float64(b.Count) * decay
			a.seen = true
		}
	}

	var out ChannelPlay
	bad := map[string]bool{}
	clean := false // some source in the list is not flagged
	for _, n := range names {
		a := accs[n]
		a.Rate = a.failed / max(a.watched, minWatchHours)
		a.Bad = a.Failures >= MinPlayFailures && a.Rate > MaxFailuresPerHour
		if a.Bad {
			bad[n] = true
		} else {
			clean = true
		}
		if a.seen {
			out.Sources = append(out.Sources, a.SourceHealth)
		}
	}
	out.AllBad = len(bad) > 0 && !clean
	if len(bad) == 0 || !clean {
		// Nothing to move, or every source is bad: reordering would only
		// shuffle equally bad sources.
		return out
	}
	var good, worse []int64
	for _, c := range list {
		if bad[source[c.StreamID]] {
			worse = append(worse, c.StreamID)
		} else {
			good = append(good, c.StreamID)
		}
	}
	order := append(good, worse...)
	for i, c := range list {
		if order[i] != c.StreamID {
			out.Order = order
			break
		}
	}
	return out
}
