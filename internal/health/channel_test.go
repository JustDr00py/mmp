package health

import (
	"slices"
	"testing"
	"time"
)

var ktla = []ChannelStream{
	{StreamID: 1, ProviderID: 67, Backbone: "TREX"},
	{StreamID: 2, ProviderID: 38, Backbone: "TREX"},
	{StreamID: 3, ProviderID: 65, Backbone: "MAXOTT"},
	{StreamID: 4, ProviderID: 70, Backbone: ""},
}

func at(now time.Time, ago time.Duration, stream int64, outcome string, n int64) Bucket {
	return Bucket{Start: now.Add(-ago), StreamID: stream, Outcome: outcome, Count: n}
}

func TestChannelPlayHealthFlagsSlowBackbone(t *testing.T) {
	now := time.Now()
	buckets := []Bucket{
		at(now, 2*time.Hour, 1, OutcomeFellBehind, 2),
		at(now, 2*time.Hour, 1, OutcomePlayed10m, 3),
		at(now, 30*time.Hour, 2, OutcomeFellBehind, 1),
		at(now, 30*time.Hour, 2, OutcomePlayed10m, 3),
		at(now, 5*time.Hour, 3, OutcomePlayed10m, 12),
	}
	p := ChannelPlayHealth(buckets, ktla, now)
	if want := []int64{3, 4, 1, 2}; !slices.Equal(p.Order, want) {
		t.Fatalf("order = %v, want %v", p.Order, want)
	}
	f := p.Flagged()
	if len(f) != 1 || f[0].Source != "TREX" || f[0].Failures != 3 || f[0].Hours != 1 || f[0].Streams != 2 {
		t.Fatalf("flagged = %+v", f)
	}
	if got := f[0].Reason(); got != "TREX fell behind real time 3× in 1.0 h watched (last 7 days)" {
		t.Errorf("reason = %q", got)
	}
	if len(p.Sources) != 2 || p.Sources[1].Source != "MAXOTT" || p.Sources[1].Bad {
		t.Errorf("sources = %+v", p.Sources)
	}
}

func TestChannelPlayHealthNeedsEnoughFailures(t *testing.T) {
	now := time.Now()
	p := ChannelPlayHealth([]Bucket{at(now, time.Hour, 1, OutcomeFellBehind, 2)}, ktla, now)
	if p.Order != nil || len(p.Flagged()) != 0 {
		t.Fatalf("two failures flagged: %+v", p)
	}
}

func TestChannelPlayHealthWeighsWatchTime(t *testing.T) {
	now := time.Now()
	// 4 failures over 10 hours watched: under one per hour.
	p := ChannelPlayHealth([]Bucket{
		at(now, time.Hour, 1, OutcomeMidPlayFailure, 4),
		at(now, time.Hour, 1, OutcomePlayed10m, 60),
	}, ktla, now)
	if p.Order != nil {
		t.Fatalf("well-watched source flagged: %+v", p)
	}
}

func TestChannelPlayHealthFadesAndWindows(t *testing.T) {
	now := time.Now()
	old := ChannelPlayHealth([]Bucket{at(now, 8*24*time.Hour, 1, OutcomeFellBehind, 9)}, ktla, now)
	if old.Order != nil || len(old.Sources) != 0 {
		t.Fatalf("outcomes outside the window counted: %+v", old)
	}
	// Five days ago: still 3 raw failures, but the decayed rate against
	// fresh watch time is low.
	faded := ChannelPlayHealth([]Bucket{
		at(now, 5*24*time.Hour, 1, OutcomeFellBehind, 3),
		at(now, time.Hour, 1, OutcomePlayed10m, 6),
	}, ktla, now)
	if faded.Order != nil {
		t.Fatalf("faded failures flagged: %+v", faded.Sources)
	}
}

func TestChannelPlayHealthAllBadKeepsOrder(t *testing.T) {
	now := time.Now()
	list := ktla[:3] // TREX and MAXOTT only
	p := ChannelPlayHealth([]Bucket{
		at(now, time.Hour, 1, OutcomeFellBehind, 3),
		at(now, time.Hour, 3, OutcomeFellBehind, 3),
	}, list, now)
	if p.Order != nil {
		t.Fatalf("every source bad, still reordered: %v", p.Order)
	}
	if len(p.Flagged()) != 2 || !p.AllBad {
		t.Errorf("flagged = %+v, AllBad = %v", p.Flagged(), p.AllBad)
	}
}

func TestChannelPlayHealthAlreadyLast(t *testing.T) {
	now := time.Now()
	p := ChannelPlayHealth([]Bucket{at(now, time.Hour, 4, OutcomeFellBehind, 5)}, ktla, now)
	if p.Order != nil {
		t.Fatalf("flagged source already last, still reordered: %v", p.Order)
	}
	if f := p.Flagged(); len(f) != 1 || f[0].Source != "provider #70" || p.AllBad {
		t.Errorf("flagged = %+v", f)
	}
}

func TestChannelPlayHealthIgnoresOtherOutcomes(t *testing.T) {
	now := time.Now()
	p := ChannelPlayHealth([]Bucket{
		at(now, time.Hour, 1, OutcomeOpenFailed, 9),
		at(now, time.Hour, 99, OutcomeFellBehind, 9), // not in the list
	}, ktla, now)
	if p.Order != nil || len(p.Sources) != 0 {
		t.Fatalf("unrelated outcomes counted: %+v", p)
	}
}
