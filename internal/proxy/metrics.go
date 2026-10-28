package proxy

import (
	"maps"
	"sync"
	"sync/atomic"
)

// Metrics counts proxy activity for monitoring. All methods are safe on a
// nil *Metrics (counting disabled) and for concurrent use. The proxy only
// counts; internal/metrics exposes the numbers to Prometheus.
type Metrics struct {
	upstreamBytes atomic.Uint64
	viewerBytes   atomic.Uint64
	failovers     atomic.Uint64
	openFailures  atomic.Uint64
	exhausted     atomic.Uint64

	mu       sync.Mutex
	failures map[string]uint64 // upstream failures by FailureCategory
}

// NewMetrics returns zeroed counters.
func NewMetrics() *Metrics { return &Metrics{failures: map[string]uint64{}} }

func (m *Metrics) addUpstreamBytes(n int) {
	if m != nil {
		m.upstreamBytes.Add(uint64(n))
	}
}

func (m *Metrics) addViewerBytes(n int) {
	if m != nil {
		m.viewerBytes.Add(uint64(n))
	}
}

func (m *Metrics) failover() {
	if m != nil {
		m.failovers.Add(1)
	}
}

func (m *Metrics) openFailure() {
	if m != nil {
		m.openFailures.Add(1)
	}
}

func (m *Metrics) exhaust() {
	if m != nil {
		m.exhausted.Add(1)
	}
}

func (m *Metrics) upstreamFailure(reason string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.failures[FailureCategory(reason)]++
	m.mu.Unlock()
}

// FailureCategory maps a session's failure reason to a bounded label:
// the watchdog verdicts, "eof", "ring_closed" or "read_error" (whose
// free-form error text must never become a label).
func FailureCategory(reason string) string {
	switch reason {
	case Stalled.String(), LowBitrate.String(), CCBurst.String(), Behind.String(), "eof":
		return reason
	case "ring closed":
		return "ring_closed"
	}
	return "read_error"
}

// MetricsSnapshot is a point-in-time copy of the counters.
type MetricsSnapshot struct {
	UpstreamBytes, ViewerBytes         uint64
	Failovers, OpenFailures, Exhausted uint64
	UpstreamFailures                   map[string]uint64
}

// Snapshot copies the counters.
func (m *Metrics) Snapshot() MetricsSnapshot {
	if m == nil {
		return MetricsSnapshot{UpstreamFailures: map[string]uint64{}}
	}
	m.mu.Lock()
	failures := maps.Clone(m.failures)
	m.mu.Unlock()
	return MetricsSnapshot{
		UpstreamBytes: m.upstreamBytes.Load(), ViewerBytes: m.viewerBytes.Load(),
		Failovers: m.failovers.Load(), OpenFailures: m.openFailures.Load(), Exhausted: m.exhausted.Load(),
		UpstreamFailures: failures,
	}
}
