// Command mmp is the IPTV multiplexer / failover proxy server.
package main

import (
	"cmp"
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
	"github.com/sysadmin/mmp/internal/auth"
	"github.com/sysadmin/mmp/internal/channel"
	"github.com/sysadmin/mmp/internal/config"
	"github.com/sysadmin/mmp/internal/db"
	"github.com/sysadmin/mmp/internal/health"
	"github.com/sysadmin/mmp/internal/output"
	"github.com/sysadmin/mmp/internal/proxy"
	"github.com/sysadmin/mmp/internal/server"
	"github.com/sysadmin/mmp/internal/source"
	"github.com/sysadmin/mmp/internal/web"
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
	// HTTP-TS or HLS; sessions add ffmpeg for channels with an ffmpeg profile.
	direct := proxy.NewProtocolOpener(pc.ConnectTimeout.Duration, proxy.VariantLimit{
		MaxBandwidth: int64(pc.HLSMaxBandwidthKbps) * 1000,
		MaxHeight:    pc.HLSMaxHeight,
	})
	capacity := proxy.NewCapacity()
	cooldowns := proxy.NewCooldowns(pc.CooldownBase.Duration, pc.CooldownMax.Duration)
	streams := proxy.NewManager(ctx, proxy.ManagerConfig{
		Source: proxy.SortedByHealth(proxy.DBSource(db.New(pool)),
			func() bool { return pc.SortByHealth }),
		Opener:    proxy.NewProfileOpener(direct, pc.FFmpegPath),
		Capacity:  capacity,
		Cooldowns: cooldowns,
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

	if pr := cfg.Probe; pr.Enabled {
		ffprobe := ""
		if pr.FFprobe {
			ffprobe = cmp.Or(pr.FFprobePath, "ffprobe")
		}
		prober := &health.Prober{Log: log, Store: db.New(pool), Opener: direct,
			Capacity: capacity, Cooldowns: cooldowns, Busy: streams.BusyStreams,
			Cfg: health.Config{Interval: pr.Interval.Duration, Recheck: pr.Recheck.Duration,
				Sample: pr.Sample.Duration, Concurrency: pr.Concurrency, FFprobePath: ffprobe}}
		go prober.Run(ctx)
	}

	queries := db.New(pool)
	if err := auth.EnsureAdmin(ctx, queries, log, cfg.Admin.Username, cfg.Admin.Password); err != nil {
		return err
	}
	authn := auth.NewAuthenticator(queries, log, cfg.Auth.SessionTTL.Duration,
		strings.HasPrefix(cfg.BaseURL, "https://"))
	go func() { // purge expired admin sessions hourly
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n, err := queries.DeleteExpiredSessions(ctx); err != nil {
					log.Warn("purge sessions", "err", err)
				} else if n > 0 {
					log.Info("purged expired sessions", "count", n)
				}
			}
		}
	}()

	refresher := source.NewRefresher(log, pool)
	matcher := channel.NewService(log, pool)
	if cfg.Match.Auto {
		refresher.AfterRefresh = matcher.AutoMatchHook(cfg.Match.MinProviders)
	}
	apiEnv := &api.Env{Log: log, Store: db.New(pool), Refresh: refresher,
		Match: matcher, Streams: streams, Auth: authn,
		InTx: func(ctx context.Context, fn func(api.Store) error) error {
			return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { return fn(db.New(tx)) })
		}}
	srv := server.New(cfg, log, pool)
	srv.Mux().Route("/api/v1", func(r chi.Router) { api.Mount(r, apiEnv) })

	// Customer outputs: playlists, EPG, Xtream API and live streams.
	access := output.NewAccess(auth.NewCustomers(queries, time.Minute))
	outputs := &output.Handlers{Access: access, Store: queries, Streams: streams, BaseURL: cfg.BaseURL, Log: log}
	if hc := cfg.HLS; hc.Enabled {
		outputs.HLS = output.NewHLS(ctx, output.HLSConfig{Segment: hc.Segment.Duration, Window: hc.Window, Idle: hc.Idle.Duration},
			streams, access, log)
	}
	outputs.Mount(srv.Mux())
	ui, err := web.New(authn, streams, log, version)
	if err != nil {
		return fmt.Errorf("admin ui: %w", err)
	}
	ui.Mount(srv.Mux())

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
