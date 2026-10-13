// Command mmp is the IPTV multiplexer / failover proxy server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/sysadmin/mmp/internal/api"
	"github.com/sysadmin/mmp/internal/channel"
	"github.com/sysadmin/mmp/internal/config"
	"github.com/sysadmin/mmp/internal/db"
	"github.com/sysadmin/mmp/internal/proxy"
	"github.com/sysadmin/mmp/internal/server"
	"github.com/sysadmin/mmp/internal/source"
)

var version = "dev"

func main() {
	configPath := flag.String("config", "", "path to YAML config file (optional; env vars override)")
	migrateOnly := flag.Bool("migrate", false, "run migrations and exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("mmp", version)
		return
	}

	if err := run(context.Background(), *configPath, *migrateOnly); err != nil {
		fmt.Fprintf(os.Stderr, "mmp: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, configPath string, migrateOnly bool) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	log := newLogger(cfg)
	log.Info("mmp starting", "version", version, "listen", cfg.Listen)

	if err := db.Migrate(ctx, cfg.DB.DSN); err != nil {
		return err
	}
	log.Info("migrations applied")
	if migrateOnly {
		return nil
	}

	pool, err := db.Open(ctx, db.PoolConfig{DSN: cfg.DB.DSN, MaxConns: cfg.DB.MaxConns})
	if err != nil {
		return err
	}
	defer pool.Close()

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Proxy: sessions live under the signal ctx, so SIGTERM ends every
	// stream (viewers get EOF) and graceful HTTP shutdown is not held open.
	events := proxy.NewEventRecorder(db.New(pool), log, 1024)
	eventsDone := make(chan struct{})
	go func() { events.Run(ctx); close(eventsDone) }()
	defer func() { <-eventsDone }() // after streams.Shutdown (defers run LIFO): last events are flushed

	pc := cfg.Proxy
	streams := proxy.NewManager(ctx, proxy.ManagerConfig{
		Source:    proxy.DBSource(db.New(pool)),
		Opener:    proxy.NewHTTPOpener(pc.ConnectTimeout.Duration),
		Capacity:  proxy.NewCapacity(),
		Cooldowns: proxy.NewCooldowns(pc.CooldownBase.Duration, pc.CooldownMax.Duration),
		RingSize:  pc.BufferSizeMB << 20,
		// ~1 MiB of buffered stream lets a joining player find a keyframe fast.
		JoinBacklog: 1 << 20,
		IdleGrace:   pc.IdleGrace.Duration,
		Thresholds: proxy.Thresholds{
			StallTimeout:   pc.StallTimeout.Duration,
			LowBitrateKbps: pc.LowBitrateKbps,
			MaxCCErrorRate: pc.MaxCCErrorRate,
			Window:         5 * time.Second,
		},
		MaxRetryWindow: pc.MaxRetryWindow.Duration,
		Log:            log,
		OnEvent:        events.Record,
	})
	defer streams.Shutdown()

	refresher := source.NewRefresher(log, pool)
	apiEnv := &api.Env{Log: log, Store: db.New(pool), Refresh: refresher,
		Match: channel.NewService(log, pool), Streams: streams,
		InTx: func(ctx context.Context, fn func(api.Store) error) error {
			return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { return fn(db.New(tx)) })
		}}
	srv := server.New(cfg, log, pool)
	srv.Mux().Route("/api/v1", func(r chi.Router) { api.Mount(r, apiEnv) })

	// Background workers.
	go func() {
		if err := refresher.Start(ctx, time.Minute, cfg.Refresh.EPGInterval.Duration); err != nil {
			log.Error("refresher stopped", "err", err)
		}
	}()

	if err := srv.Run(ctx); err != nil {
		return err
	}
	log.Info("mmp stopped")
	return nil
}

func newLogger(cfg config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.Log.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	if strings.EqualFold(cfg.Log.Format, "json") {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}
