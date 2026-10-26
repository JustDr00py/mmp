package health

import (
	"testing"
	"time"
)

var now = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func bucket(age time.Duration, outcome string, n int64) Bucket {
	return Bucket{Start: now.Add(-age), ProviderID: 1, Outcome: outcome, Count: n}
}

func TestComputeWithheldUntilEnoughAttempts(t *testing.T) {
	s := Compute([]Bucket{bucket(0, OutcomeOpenFailed, MinAttempts-1)}, now)
	if s.Enough {
		t.Fatalf("Enough = true with %d attempts, want false", MinAttempts-1)
	}
	if s = Compute([]Bucket{bucket(0, OutcomeOpenFailed, MinAttempts)}, now); !s.Enough {
		t.Fatalf("Enough = false with %d attempts, want true", MinAttempts)
	}
}

func TestComputeAllGood(t *testing.T) {
	s := Compute([]Bucket{bucket(0, OutcomeOpenOK, 50)}, now)
	if s.Value != 100 || s.Failures != 0 || s.Attempts != 50 {
		t.Fatalf("got %+v, want value 100, no failures", s)
	}
}

func TestComputePriorDampsSmallSamples(t *testing.T) {
	// Ten failures and no successes: ratio 10/(10+20) gives 67, not 0.
	s := Compute([]Bucket{bucket(0, OutcomeOpenFailed, 10)}, now)
	if s.Value != 67 {
		t.Fatalf("Value = %d, want 67", s.Value)
	}
}

func TestComputeOldOutcomesIgnored(t *testing.T) {
	s := Compute([]Bucket{bucket(Window+time.Hour, OutcomeOpenFailed, 100)}, now)
	if s.Attempts != 0 || s.Enough {
		t.Fatalf("got %+v, want the old outcomes dropped", s)
	}
}

func TestComputeRecentFailuresWeighMore(t *testing.T) {
	// The same ten failures count for less once they are a half-life old.
	fresh := Compute([]Bucket{bucket(0, OutcomeOpenFailed, 10), bucket(0, OutcomeOpenOK, 10)}, now)
	aged := Compute([]Bucket{bucket(HalfLife, OutcomeOpenFailed, 10), bucket(0, OutcomeOpenOK, 10)}, now)
	if aged.Value <= fresh.Value {
		t.Fatalf("aged value %d should beat fresh value %d", aged.Value, fresh.Value)
	}
}

func TestComputeMidPlayFailureWeighsMoreThanOpenFailure(t *testing.T) {
	ok := []Bucket{bucket(0, OutcomeOpenOK, 30)}
	open := Compute(append(ok, bucket(0, OutcomeOpenFailed, 10)), now)
	mid := Compute(append(ok, bucket(0, OutcomeMidPlayFailure, 10)), now)
	if mid.Value >= open.Value {
		t.Fatalf("mid-play value %d should be below open-failed value %d", mid.Value, open.Value)
	}
}

func TestComputeProbeFailureWeighsLessThanOpenFailure(t *testing.T) {
	ok := []Bucket{bucket(0, OutcomeOpenOK, 30)}
	open := Compute(append(ok, bucket(0, OutcomeOpenFailed, 10)), now)
	probe := Compute(append(ok, bucket(0, OutcomeProbeFail, 10)), now)
	if probe.Value <= open.Value {
		t.Fatalf("probe value %d should be above open-failed value %d", probe.Value, open.Value)
	}
}

func TestComputeIgnoresUnknownOutcomes(t *testing.T) {
	s := Compute([]Bucket{bucket(0, "something_else", 50)}, now)
	if s.Attempts != 0 {
		t.Fatalf("Attempts = %d, want unknown outcomes ignored", s.Attempts)
	}
}

func TestByKeyGroupsScores(t *testing.T) {
	buckets := []Bucket{
		{Start: now, ProviderID: 1, Backbone: "A", Outcome: OutcomeOpenOK, Count: 20},
		{Start: now, ProviderID: 2, Backbone: "A", Outcome: OutcomeOpenFailed, Count: 20},
		{Start: now, ProviderID: 3, Backbone: "B", Outcome: OutcomeOpenOK, Count: 20},
	}
	byProvider := ByKey(buckets, func(b Bucket) int64 { return b.ProviderID }, now)
	if len(byProvider) != 3 || byProvider[1].Value != 100 || byProvider[2].Value >= 100 {
		t.Fatalf("by provider = %+v", byProvider)
	}
	byBackbone := ByKey(buckets, func(b Bucket) string { return b.Backbone }, now)
	if len(byBackbone) != 2 || byBackbone["B"].Value != 100 || byBackbone["A"].Attempts != 40 {
		t.Fatalf("by backbone = %+v", byBackbone)
	}
}
