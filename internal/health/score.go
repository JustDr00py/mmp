package health

import (
	"math"
	"time"
)

// Outcome names, as the proxy records them. Strings, not proxy constants,
// so this package stays free of the proxy.
const (
	OutcomeOpenOK         = "open_ok"
	OutcomeOpenFailed     = "open_failed"
	OutcomeMidPlayFailure = "mid_play_failure"
	OutcomeProbeOK        = "probe_ok"
	OutcomeProbeFail      = "probe_fail"
)

const (
	// Window is how far back outcomes count.
	Window = 7 * 24 * time.Hour
	// HalfLife is how fast an outcome's weight fades.
	HalfLife = 6 * time.Hour
	// MinAttempts is the outcome count below which no score is shown.
	MinAttempts = 10
	// priorWeight is attempt weight every provider starts with, so a single
	// failure among two attempts does not read as a 50% failure rate.
	priorWeight = 20.0
)

// outcomeWeight is how much an outcome counts toward the score. Failures
// count as failures; the rest count as attempts that worked.
type outcomeWeight struct {
	failure bool
	weight  float64
}

// A mid-play failure hurts more than a failed open: the viewer saw it.
// Probe results are weighted lower: nobody was watching the stream.
var outcomeWeights = map[string]outcomeWeight{
	OutcomeOpenOK:         {failure: false, weight: 1},
	OutcomeProbeOK:        {failure: false, weight: 0.5},
	OutcomeOpenFailed:     {failure: true, weight: 1},
	OutcomeMidPlayFailure: {failure: true, weight: 2},
	OutcomeProbeFail:      {failure: true, weight: 0.5},
}

// Bucket is one stored outcome count: Count outcomes of one kind for one
// provider, under the backbone it had, in the minute starting at Start.
type Bucket struct {
	Start      time.Time
	ProviderID int64
	Backbone   string
	Outcome    string
	Count      int64
}

// Score is a health score for one provider or backbone.
type Score struct {
	Value    int   // 0 (every attempt failed) to 100; meaningful only when Enough
	Enough   bool  // false: too few attempts in the window to score
	Attempts int64 // raw outcome count in the window
	Failures int64 // raw failure count in the window
}

// Compute scores buckets as of now. Outcomes older than Window are ignored;
// newer ones weigh more, halving every HalfLife.
func Compute(buckets []Bucket, now time.Time) Score {
	var s Score
	var total, failed float64
	for _, b := range buckets {
		w, ok := outcomeWeights[b.Outcome]
		age := now.Sub(b.Start)
		if !ok || age > Window || b.Count <= 0 {
			continue
		}
		age = max(age, 0)
		decayed := w.weight * float64(b.Count) * math.Pow(0.5, age.Hours()/HalfLife.Hours())
		total += decayed
		s.Attempts += b.Count
		if w.failure {
			failed += decayed
			s.Failures += b.Count
		}
	}
	s.Enough = s.Attempts >= MinAttempts
	ratio := math.Min(1, failed/(total+priorWeight))
	s.Value = int(math.Round(100 * (1 - ratio)))
	return s
}

// ByKey scores buckets grouped by key, such as provider or backbone label.
func ByKey[K comparable](buckets []Bucket, key func(Bucket) K, now time.Time) map[K]Score {
	groups := map[K][]Bucket{}
	for _, b := range buckets {
		k := key(b)
		groups[k] = append(groups[k], b)
	}
	out := make(map[K]Score, len(groups))
	for k, bs := range groups {
		out[k] = Compute(bs, now)
	}
	return out
}
