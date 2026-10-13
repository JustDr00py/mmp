package server

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"github.com/sysadmin/mmp/internal/redact"
)

// statusWriter records the response status and size for logging.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Flush lets streaming handlers (proxy, SSE) pass through the wrapper.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

type ctxKey int

const requestIDKey ctxKey = 0

// RequestID assigns a random id to every request, echoes it in the
// X-Request-ID response header, and stores it in the request context.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			buf := make([]byte, 8)
			if _, err := rand.Read(buf); err != nil {
				id = time.Now().Format("150405.000000000")
			} else {
				id = hex.EncodeToString(buf)
			}
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(contextWithRequestID(r, id)))
	})
}

// RequestLogger logs one line per request at info level (debug for health
// checks). URLs are redacted because player URLs carry credentials.
func RequestLogger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)
			if sw.status == 0 {
				sw.status = http.StatusOK
			}
			attrs := []any{
				"req_id", RequestIDFrom(r),
				"method", r.Method,
				"path", redact.URL(r.URL.RequestURI()),
				"status", sw.status,
				"bytes", sw.bytes,
				"duration", time.Since(start).Round(time.Millisecond).String(),
				"remote", r.RemoteAddr,
			}
			if sw.status >= 500 {
				log.Error("http", attrs...)
			} else if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
				log.Debug("http", attrs...)
			} else {
				log.Info("http", attrs...)
			}
		})
	}
}
