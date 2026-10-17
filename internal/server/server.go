// Package server assembles the root HTTP server: shared middleware,
// /healthz, and graceful shutdown. Feature routers (admin API, web UI,
// customer outputs) mount onto the exported Mux.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/JustDr00py/mmp/internal/config"
)

// Pinger reports dependency health for /healthz. *pgxpool.Pool satisfies it.
type Pinger interface {
	Ping(context.Context) error
}

// Server is the root HTTP server.
type Server struct {
	cfg       config.Config
	log       *slog.Logger
	pinger    Pinger
	mux       *chi.Mux
	http      *http.Server
	listening bool
	serveErr  chan error
	addr      atomic.Pointer[string]
}

// New builds the server with base middleware and /healthz registered.
// pinger may be nil (health reports OK without a dependency check).
// idleTimeout is how long a keep-alive connection may wait for its next
// request. Players reload HLS playlists every few seconds, well within it.
const idleTimeout = 2 * time.Minute

func New(cfg config.Config, log *slog.Logger, pinger Pinger) *Server {
	mux := chi.NewMux()
	trusted, _ := cfg.TrustedProxyPrefixes() // checked by cfg.Validate
	mux.Use(RealIP(trusted))                 // first: everything after sees the real client
	mux.Use(RequestID)
	mux.Use(RequestLogger(log))

	s := &Server{cfg: cfg, log: log, pinger: pinger, mux: mux, serveErr: make(chan error, 1)}
	mux.Get("/healthz", s.handleHealthz)
	s.http = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout.Duration,
		// Closes keep-alive connections left idle between requests, so
		// idle sockets cannot pile up. It never cuts a response in flight.
		IdleTimeout: idleTimeout,
		// Streaming responses live for hours: no overall read/write
		// timeouts here; liveness is handled by session teardowns.
	}
	return s
}

// Mux exposes the root router for feature packages to mount on.
func (s *Server) Mux() *chi.Mux { return s.mux }

// Addr returns the bound address once listening, or "" before.
func (s *Server) Addr() string {
	if p := s.addr.Load(); p != nil {
		return *p
	}
	return ""
}

// Listen starts listening and returns the bound address (useful when the
// config requests an ephemeral port).
func (s *Server) Listen() (net.Addr, error) {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("server: listen %s: %w", s.cfg.Listen, err)
	}
	s.listening = true
	a := ln.Addr().String()
	s.addr.Store(&a)
	go func() {
		if err := s.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.serveErr <- err
		}
	}()
	return ln.Addr(), nil
}

// Run blocks until ctx is cancelled or serving fails, then drains in-flight
// requests within the configured shutdown timeout.
func (s *Server) Run(ctx context.Context) error {
	if !s.listening {
		addr, err := s.Listen()
		if err != nil {
			return err
		}
		s.log.Info("server listening", "addr", addr.String())
	}
	select {
	case <-ctx.Done():
	case err := <-s.serveErr:
		if err != nil {
			return fmt.Errorf("server: serve: %w", err)
		}
	}
	s.log.Info("server draining")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.HTTP.ShutdownTimeout.Duration)
	defer cancel()
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("server: shutdown: %w", err)
	}
	s.log.Info("server stopped")
	return nil
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if s.pinger != nil {
		pingCtx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if err := s.pinger.Ping(pingCtx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
