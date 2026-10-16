package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JustDr00py/mmp/internal/proxy"
)

func scrape(t *testing.T, h http.Handler, auth string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Body)
	return rec.Code, string(b)
}

func TestHandlerExposesMetrics(t *testing.T) {
	src := Sources{
		Channels: func() []proxy.ChannelStatus {
			return []proxy.ChannelStatus{
				{SessionStatus: proxy.SessionStatus{ChannelID: 7, State: "streaming", Failovers: 2, Since: time.Now()}, Viewers: 3},
				{SessionStatus: proxy.SessionStatus{ChannelID: 9, State: "waiting"}, Viewers: 1},
			}
		},
		Proxy:       proxy.NewMetrics(),
		Slots:       func() map[int64]int { return map[int64]int{4: 2} },
		Connections: func() map[int64]int { return map[int64]int{1: 2, 2: 1} },
		HLSClients:  func() int { return 1 },
		EventsDrop:  func() uint64 { return 0 },
		Version:     "v1.2.3",
	}
	code, body := scrape(t, Handler(src, ""), "")
	if code != 200 {
		t.Fatalf("scrape = %d", code)
	}
	for _, want := range []string{
		`mmp_sessions{state="streaming"} 1`, `mmp_sessions{state="waiting"} 1`, `mmp_sessions{state="connecting"} 0`,
		`mmp_session_viewers 4`, `mmp_session_failovers{channel_id="7"} 2`,
		`mmp_customer_connections 3`, `mmp_hls_clients 1`, `mmp_provider_slots_in_use{provider_id="4"} 2`,
		`mmp_upstream_bytes_total 0`, `mmp_failovers_total 0`, `mmp_upstream_failures_total{reason="stalled"} 0`,
		`mmp_upstream_failures_total{reason="read_error"} 0`, `mmp_sessions_exhausted_total 0`, `mmp_events_dropped_total 0`,
		`mmp_build_info{version="v1.2.3"} 1`, "# TYPE mmp_failovers_total counter", "go_goroutines", "process_resident_memory_bytes",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape lacks %q", want)
		}
	}
}

func TestHandlerToken(t *testing.T) {
	h := Handler(Sources{Version: "x"}, "scrape-secret")
	for auth, want := range map[string]int{
		"":                     http.StatusUnauthorized,
		"Bearer wrong":         http.StatusUnauthorized,
		"scrape-secret":        http.StatusUnauthorized,
		"Bearer scrape-secret": http.StatusOK,
	} {
		if code, _ := scrape(t, h, auth); code != want {
			t.Errorf("auth %q = %d, want %d", auth, code, want)
		}
	}
}
