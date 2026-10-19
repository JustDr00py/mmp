package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/JustDr00py/mmp/internal/config"
)

func testCfg() config.Config {
	c := config.Defaults()
	c.Listen = "127.0.0.1:0" // ephemeral
	return c
}

func TestHealthz(t *testing.T) {
	tests := []struct {
		name   string
		pinger Pinger
		want   int
	}{
		{"no deps", nil, http.StatusOK},
		{"db ok", pingFunc(func(context.Context) error { return nil }), http.StatusOK},
		{"db down", pingFunc(func(context.Context) error { return errors.New("nope") }), http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(testCfg(), slog.New(slog.NewTextHandler(io.Discard, nil)), tt.pinger)
			rec := httptest.NewRecorder()
			s.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			if rec.Code != tt.want {
				t.Errorf("healthz = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

type pingFunc func(context.Context) error

func (f pingFunc) Ping(ctx context.Context) error { return f(ctx) }

func TestRequestLoggerRedactsAndTags(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := New(testCfg(), log, nil)
	// Routes that would log player credentials in the query string or path.
	s.Mux().Get("/get.php", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("x"))
	})
	s.Mux().Get("/hls/{key}/{seg}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/get.php?username=alice&password=s3cret&type=m3u_plus", nil)
	req.Header.Set("X-Request-ID", "req-123")
	rec := httptest.NewRecorder()
	s.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("X-Request-ID"); got != "req-123" {
		t.Errorf("X-Request-ID echoed = %q", got)
	}
	out := buf.String()
	if strings.Contains(out, "alice") || strings.Contains(out, "s3cret") {
		t.Errorf("credentials leaked into request log: %s", out)
	}
	if !strings.Contains(out, "req-123") || !strings.Contains(out, "418") {
		t.Errorf("log missing req id or status: %s", out)
	}

	// The HLS lease key is a bearer credential in the path.
	buf.Reset()
	s.Mux().ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/hls/Lease-S3cretkey/17.ts", nil))
	if strings.Contains(buf.String(), "Lease-S3cretkey") {
		t.Errorf("hls lease key leaked into request log: %s", buf.String())
	}
}

func TestRequestIDGenerated(t *testing.T) {
	s := New(testCfg(), slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	s.Mux().Get("/x", func(w http.ResponseWriter, r *http.Request) {
		if RequestIDFrom(r) == "" {
			t.Error("no request id in context")
		}
	})
	rec := httptest.NewRecorder()
	s.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Header().Get("X-Request-ID") == "" {
		t.Error("no X-Request-ID header generated")
	}
}

func TestRunServesWithoutExplicitListen(t *testing.T) {
	// Regression: Run must start serving even when Listen was never called.
	s := New(testCfg(), slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	var lastErr error
	for i := 0; i < 50; i++ {
		if a := s.Addr(); a != "" {
			resp, err := http.Get("http://" + a + "/healthz")
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					lastErr = fmt.Errorf("healthz = %d", resp.StatusCode)
				} else {
					lastErr = nil
				}
				break
			}
			lastErr = err
		} else {
			lastErr = errors.New("not listening yet")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("server never became reachable: %v", lastErr)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run never returned")
	}
}

func TestGracefulShutdownDrainsInFlight(t *testing.T) {
	release := make(chan struct{})
	s := New(testCfg(), slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	s.Mux().Get("/slow", func(w http.ResponseWriter, r *http.Request) {
		<-release // simulate a long streaming response
		w.WriteHeader(http.StatusOK)
	})

	addr, err := s.Listen()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	type result struct {
		code int
		err  error
	}
	respCh := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + addr.String() + "/slow")
		if err != nil {
			respCh <- result{err: err}
			return
		}
		defer resp.Body.Close()
		respCh <- result{code: resp.StatusCode}
	}()

	// Wait until the request is inside the handler, then order a shutdown.
	time.Sleep(100 * time.Millisecond)
	cancel()

	// The in-flight request must complete even after cancellation began.
	close(release)
	select {
	case r := <-respCh:
		if r.err != nil || r.code != http.StatusOK {
			t.Errorf("in-flight request = %+v, want 200", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request never completed")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run never returned")
	}
}

// TestCollapseSlashes: "//get.php" reaches the /get.php route, and
// ordinary paths are untouched.
func TestCollapseSlashes(t *testing.T) {
	s := New(testCfg(), slog.New(slog.DiscardHandler), nil)
	s.Mux().Get("/get.php", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "playlist") })
	s.Mux().Get("/live/{u}/{p}/{id}", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "live "+chi.URLParam(r, "p"))
	})
	for path, want := range map[string]string{
		"/get.php?username=a":  "playlist",
		"//get.php?username=a": "playlist",
		"///get.php":           "playlist",
		"//live/a/b/1.ts":      "live b",
		"/live//a/b/1.ts":      "live b",
		"/live/a/b%2F/1.ts":    "live b%2F", // a password ending in "/"
		"/live/a/b%2F%2F/1.ts": "live b%2F%2F",
		"/nothing//here":       "404 page not found\n",
	} {
		rec := httptest.NewRecorder()
		s.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://mmp.test"+path, nil))
		if rec.Body.String() != want {
			t.Errorf("%s = %q, want %q", path, rec.Body.String(), want)
		}
	}
}
