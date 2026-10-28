package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFailureCategory(t *testing.T) {
	for in, want := range map[string]string{
		"stalled": "stalled", "low_bitrate": "low_bitrate", "cc_burst": "cc_burst", "behind": "behind", "eof": "eof",
		"ring closed": "ring_closed", "read: connection reset by peer": "read_error", "": "read_error",
	} {
		if got := FailureCategory(in); got != want {
			t.Errorf("FailureCategory(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMetricsNilSafe(t *testing.T) {
	var m *Metrics
	m.addUpstreamBytes(1)
	m.addViewerBytes(1)
	m.failover()
	m.openFailure()
	m.exhaust()
	m.upstreamFailure("eof")
	if s := m.Snapshot(); s.UpstreamBytes != 0 || s.UpstreamFailures == nil {
		t.Errorf("nil snapshot = %+v", s)
	}
}

func TestSessionMetrics(t *testing.T) {
	t.Run("failover", func(t *testing.T) {
		m := NewMetrics()
		a := newFakeProvider(t, 'A', "eof", 400)
		b := newFakeProvider(t, 'B', "ok", 0)
		h := startSession(t, []Candidate{
			{StreamID: 1, ProviderID: 1, URL: a.URL + "/1.ts"},
			{StreamID: 2, ProviderID: 2, URL: b.URL + "/2.ts"},
		}, func(cfg *SessionConfig) { cfg.Metrics = m })
		if _, err := h.watch(t, 10*time.Second, func(p [][]byte) bool { return countTag(p, 'B') >= 100 }); err != nil {
			t.Fatal(err)
		}
		h.stop(t)
		s := m.Snapshot()
		if s.Failovers != 1 || s.UpstreamFailures["eof"] != 1 || s.OpenFailures != 0 || s.UpstreamBytes < 500*TSPacketSize {
			t.Errorf("snapshot = %+v", s)
		}
	})
	t.Run("open failure", func(t *testing.T) {
		m := NewMetrics()
		a := newFakeProvider(t, 'A', "503", 0)
		b := newFakeProvider(t, 'B', "ok", 0)
		h := startSession(t, []Candidate{
			{StreamID: 1, ProviderID: 1, URL: a.URL + "/1.ts"},
			{StreamID: 2, ProviderID: 2, URL: b.URL + "/2.ts"},
		}, func(cfg *SessionConfig) { cfg.Metrics = m })
		if _, err := h.watch(t, 10*time.Second, func(p [][]byte) bool { return countTag(p, 'B') >= 50 }); err != nil {
			t.Fatal(err)
		}
		h.stop(t)
		// Nothing was playing before B: an open failure, not a failover.
		if s := m.Snapshot(); s.OpenFailures != 1 || s.Failovers != 0 {
			t.Errorf("snapshot = %+v", s)
		}
	})
	t.Run("exhausted", func(t *testing.T) {
		m := NewMetrics()
		a := newFakeProvider(t, 'A', "503", 0)
		h := startSession(t, []Candidate{{StreamID: 1, ProviderID: 1, URL: a.URL + "/1.ts"}}, func(cfg *SessionConfig) {
			cfg.Metrics = m
			cfg.MaxRetryWindow = 300 * time.Millisecond
			cfg.Cooldowns = NewCooldowns(50*time.Millisecond, 100*time.Millisecond)
		})
		select {
		case err := <-h.runErr:
			h.runErr <- err // leave it for the harness cleanup
		case <-time.After(5 * time.Second):
			t.Fatal("session did not give up")
		}
		if s := m.Snapshot(); s.Exhausted != 1 || s.OpenFailures < 1 {
			t.Errorf("snapshot = %+v", s)
		}
	})
}

func TestViewerBytesCounted(t *testing.T) {
	m := NewMetrics()
	up := newFakeProvider(t, 'A', "ok", 0)
	mgr := newTestManager(t, map[int64][]Candidate{1: {{StreamID: 1, ProviderID: 1, URL: up.URL + "/1.ts"}}},
		func(cfg *ManagerConfig) { cfg.Metrics = m })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, err := mgr.Attach(1)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer v.Close()
		ServeViewer(w, r, v)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(resp.Body, make([]byte, 200*TSPacketSize)); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if s := m.Snapshot(); s.ViewerBytes < 200*TSPacketSize || s.UpstreamBytes < s.ViewerBytes/2 {
		t.Errorf("snapshot = %+v", s)
	}
}
