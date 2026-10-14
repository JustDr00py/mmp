// Package metrics exposes MMP's state to Prometheus at /metrics. Values are
// read at scrape time from the proxy and output packages, which only keep
// plain counters, so they carry no Prometheus dependency.
package metrics

import (
	"crypto/subtle"
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/sysadmin/mmp/internal/proxy"
)

// Sources are where the numbers come from; nil fields are skipped.
type Sources struct {
	Channels    func() []proxy.ChannelStatus // live sessions (proxy.Manager.Channels)
	Proxy       *proxy.Metrics               // traffic and failure counters
	Slots       func() map[int64]int         // provider slots in use
	Connections func() map[int64]int         // customer streams per user
	HLSClients  func() int                   // active HLS leases
	EventsDrop  func() uint64                // events not persisted (queue full)
	Version     string
}

var (
	descSessions      = prometheus.NewDesc("mmp_sessions", "Channel sessions by state.", []string{"state"}, nil)
	descViewers       = prometheus.NewDesc("mmp_session_viewers", "Viewers attached to channel sessions (TS clients and HLS segmenters).", nil, nil)
	descSessionFO     = prometheus.NewDesc("mmp_session_failovers", "Failovers of each live channel session since it started.", []string{"channel_id"}, nil)
	descConnections   = prometheus.NewDesc("mmp_customer_connections", "Customer connections held (TS streams and HLS clients).", nil, nil)
	descHLS           = prometheus.NewDesc("mmp_hls_clients", "Active HLS clients.", nil, nil)
	descSlots         = prometheus.NewDesc("mmp_provider_slots_in_use", "Provider connection slots in use.", []string{"provider_id"}, nil)
	descUpBytes       = prometheus.NewDesc("mmp_upstream_bytes_total", "MPEG-TS bytes received from providers and fed to sessions.", nil, nil)
	descViewerBytes   = prometheus.NewDesc("mmp_viewer_bytes_total", "MPEG-TS bytes sent to TS viewers.", nil, nil)
	descFailovers     = prometheus.NewDesc("mmp_failovers_total", "Switches to another upstream after a playing one failed.", nil, nil)
	descOpenFailures  = prometheus.NewDesc("mmp_upstream_open_failures_total", "Upstream connections that could not be opened.", nil, nil)
	descUpFailures    = prometheus.NewDesc("mmp_upstream_failures_total", "Playing upstreams that failed, by reason.", []string{"reason"}, nil)
	descExhausted     = prometheus.NewDesc("mmp_sessions_exhausted_total", "Sessions that gave up: no candidate worked within the retry window.", nil, nil)
	descEventsDropped = prometheus.NewDesc("mmp_events_dropped_total", "Proxy events not written to the database because the queue was full.", nil, nil)
	descBuild         = prometheus.NewDesc("mmp_build_info", "MMP build information.", []string{"version"}, nil)
)

// failureReasons are always exported (at 0 until seen) so rates and alerts
// work from the first scrape.
var failureReasons = []string{"stalled", "low_bitrate", "cc_burst", "eof", "read_error", "ring_closed"}

type collector struct{ s Sources }

func (c collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descSessions, descViewers, descSessionFO, descConnections, descHLS, descSlots,
		descUpBytes, descViewerBytes, descFailovers, descOpenFailures, descUpFailures, descExhausted, descEventsDropped, descBuild} {
		ch <- d
	}
}

func (c collector) Collect(ch chan<- prometheus.Metric) {
	gauge := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
	}
	counter := func(d *prometheus.Desc, v uint64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, float64(v), labels...)
	}

	gauge(descBuild, 1, c.s.Version)
	if c.s.Channels != nil {
		states := map[string]float64{"connecting": 0, "streaming": 0, "waiting": 0}
		viewers := 0
		for _, st := range c.s.Channels() {
			states[st.State]++
			viewers += st.Viewers
			gauge(descSessionFO, float64(st.Failovers), strconv.FormatInt(st.ChannelID, 10))
		}
		for state, n := range states {
			gauge(descSessions, n, state)
		}
		gauge(descViewers, float64(viewers))
	}
	if c.s.Connections != nil {
		total := 0
		for _, n := range c.s.Connections() {
			total += n
		}
		gauge(descConnections, float64(total))
	}
	if c.s.HLSClients != nil {
		gauge(descHLS, float64(c.s.HLSClients()))
	}
	if c.s.Slots != nil {
		for id, n := range c.s.Slots() {
			gauge(descSlots, float64(n), strconv.FormatInt(id, 10))
		}
	}
	if c.s.Proxy != nil {
		snap := c.s.Proxy.Snapshot()
		counter(descUpBytes, snap.UpstreamBytes)
		counter(descViewerBytes, snap.ViewerBytes)
		counter(descFailovers, snap.Failovers)
		counter(descOpenFailures, snap.OpenFailures)
		counter(descExhausted, snap.Exhausted)
		for _, reason := range failureReasons {
			counter(descUpFailures, snap.UpstreamFailures[reason], reason)
		}
	}
	if c.s.EventsDrop != nil {
		counter(descEventsDropped, c.s.EventsDrop())
	}
}

// Handler serves the registry in the Prometheus text format. With a
// non-empty token, scrapes must send "Authorization: Bearer <token>"
// (Prometheus: authorization.credentials).
func Handler(s Sources, token string) http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collector{s},
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	h := promhttp.HandlerFor(reg, promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError})
	if token == "" {
		return h
	}
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="mmp metrics"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}
