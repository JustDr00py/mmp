// Command mmp is the IPTV multiplexer / failover proxy server.
package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JustDr00py/mmp/internal/api"
	"github.com/JustDr00py/mmp/internal/auth"
	"github.com/JustDr00py/mmp/internal/backup"
	"github.com/JustDr00py/mmp/internal/channel"
	"github.com/JustDr00py/mmp/internal/config"
	"github.com/JustDr00py/mmp/internal/db"
	eventlog "github.com/JustDr00py/mmp/internal/events"
	"github.com/JustDr00py/mmp/internal/health"
	"github.com/JustDr00py/mmp/internal/metrics"
	"github.com/JustDr00py/mmp/internal/output"
	"github.com/JustDr00py/mmp/internal/proxy"
	"github.com/JustDr00py/mmp/internal/server"
	"github.com/JustDr00py/mmp/internal/source"
	"github.com/JustDr00py/mmp/internal/web"
)

var version = "dev"

func main() {
	configPath := flag.String("config", "", "path to YAML config file (optional; env vars override)")
	migrateOnly := flag.Bool("migrate", false, "run migrations and exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	backupNow := flag.Bool("backup", false, "write a backup to the backup directory and exit")
	restoreFile := flag.String("restore", "", "restore this backup file (replaces all data) and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("mmp", version)
		return
	}

	if *backupNow || *restoreFile != "" {
		if err := runBackupCLI(context.Background(), *configPath, *backupNow, *restoreFile); err != nil {
			fmt.Fprintf(os.Stderr, "mmp: %v\n", err)
			os.Exit(1)
		}
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
	proxyMetrics := proxy.NewMetrics()
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
			Window:         max(pc.HealthWindow.Duration, pc.StallTimeout.Duration),
		},
		MaxRetryWindow: pc.MaxRetryWindow.Duration,
		Log:            log,
		OnEvent:        events.Record,
		Metrics:        proxyMetrics,
		Probe:          mediaProbe(cfg.Probe.FFprobePath, log),
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
	go eventlog.RunPruner(ctx, queries, log, time.Hour) // keep events to their set age
	if err := auth.EnsureAdmin(ctx, queries, log, cfg.Admin.Username, cfg.Admin.Password); err != nil {
		return err
	}
	authn := auth.NewAuthenticator(queries, log, cfg.Auth.SessionTTL.Duration,
		strings.HasPrefix(cfg.BaseURL, "https://"))
	// Shared failure lockout for every password check (admin + customer).
	loginLimit := auth.NewLimiter(cfg.Auth.MaxAttempts, cfg.Auth.Lockout.Duration)
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

	refresher := source.NewRefresher(log, pool, cfg.Refresh.MaxDisablePct)
	matcher := channel.NewService(log, pool)
	// match.* only seeds the first start; then the Settings page decides.
	if err := matcher.SeedMatchSettings(ctx, channel.AutoMatch{Enabled: cfg.Match.Auto, MinProviders: cfg.Match.MinProviders,
		NewChannels: cfg.Match.AutoNewChannels, Group: cfg.Match.AutoGroup, Bouquets: cfg.Match.AutoBouquets}); err != nil {
		return err
	}
	refresher.AfterRefresh = matcher.AutoMatchHook()
	apiEnv := &api.Env{Log: log, Store: db.New(pool), Refresh: refresher,
		Match: matcher, Streams: streams, Auth: authn, LoginLimit: loginLimit, DefaultRefresh: cfg.Refresh.Interval.Duration,
		InTx: func(ctx context.Context, fn func(api.Store) error) error {
			return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { return fn(db.New(tx)) })
		}}
	srv := server.New(cfg, log, pool)
	srv.Mux().Route("/api/v1", func(r chi.Router) { api.Mount(r, apiEnv) })

	// Customer outputs: playlists, EPG, Xtream API and live streams.
	access := output.NewAccess(auth.NewCustomers(queries, time.Minute))
	outputs := &output.Handlers{Access: access, Store: queries, Streams: streams, BaseURL: cfg.BaseURL, Log: log,
		Limit: loginLimit}
	outputs.Slots = capacity // VOD playback shares provider slots with live sessions
	outputs.VOD = &output.VODCatalog{Queries: queries, Refresher: refresher,
		InfoTTL: cfg.VOD.InfoTTL.Duration, EpisodeTTL: cfg.VOD.EpisodeTTL.Duration}
	if hc := cfg.HLS; hc.Enabled {
		outputs.HLS = output.NewHLS(ctx, output.HLSConfig{Segment: hc.Segment.Duration, Window: hc.Window, Idle: hc.Idle.Duration},
			streams, access, log)
	}
	outputs.Mount(srv.Mux())
	backups, err := newBackups(pool, cfg, log)
	if err != nil {
		return err
	}
	go backups.Scheduler.Run(ctx)
	ui, err := web.New(web.Deps{Auth: authn, Store: queries, Refresh: refresher, Match: matcher, Live: streams, Viewers: access,
		Backups: backups, Restart: func() { restartSelf(log) },
		Slots: capacity.Snapshot, Connections: access.Connections, BaseURL: cfg.BaseURL, Log: log, Version: version, Ctx: ctx,
		DefaultRefresh: cfg.Refresh.Interval.Duration, EpisodeTTL: cfg.VOD.EpisodeTTL.Duration,
		LoginLimit: loginLimit,
		InTx: func(ctx context.Context, fn func(web.Store) error) error {
			return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { return fn(db.New(tx)) })
		}})
	if err != nil {
		return fmt.Errorf("admin ui: %w", err)
	}
	ui.Mount(srv.Mux())
	if cfg.Metrics.Enabled {
		src := metrics.Sources{Channels: streams.Channels, Proxy: proxyMetrics, Slots: capacity.Snapshot,
			Connections: access.Connections, EventsDrop: events.Dropped, Version: version}
		if outputs.HLS != nil {
			src.HLSClients = outputs.HLS.Leases
		}
		srv.Mux().Handle("/metrics", metrics.Handler(src, cfg.Metrics.Token))
	}

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

// newBackups builds the backup service over the backup directory.
func newBackups(pool *pgxpool.Pool, cfg config.Config, log *slog.Logger) (*backup.Service, error) {
	latest, err := db.LatestMigration()
	if err != nil {
		return nil, err
	}
	store := &backup.Store{Dir: cfg.Backup.Dir, Pool: pool, Version: version}
	return &backup.Service{Store: store, Scheduler: &backup.Scheduler{Store: store, Log: log},
		Migrator: db.Migrator{DSN: cfg.DB.DSN}, Latest: latest}, nil
}

// runBackupCLI handles -backup and -restore (for when the admin UI is not
// usable): migrate, do the one thing, exit.
func runBackupCLI(ctx context.Context, configPath string, create bool, restore string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	log := newLogger(cfg)
	if err := db.Migrate(ctx, cfg.DB.DSN); err != nil {
		return err
	}
	pool, err := db.Open(ctx, db.PoolConfig{DSN: cfg.DB.DSN, MaxConns: cfg.DB.MaxConns})
	if err != nil {
		return err
	}
	defer pool.Close()
	svc, err := newBackups(pool, cfg, log)
	if err != nil {
		return err
	}
	if create {
		info, err := svc.Create(ctx, backup.KindManual)
		if err != nil {
			return err
		}
		fmt.Printf("backup written: %s (%d bytes, %d rows)\n", filepath.Join(cfg.Backup.Dir, info.Name), info.Size, info.Manifest.Rows())
		return nil
	}
	m, safety, err := svc.RestoreFile(ctx, restore)
	if safety != "" {
		fmt.Printf("data before the restore saved as %s\n", filepath.Join(cfg.Backup.Dir, safety))
	}
	if err != nil {
		return err
	}
	fmt.Printf("restored %d rows from %s (MMP %s); restart MMP if it is running\n", m.Rows(), restore, m.Version)
	return nil
}

// restartSelf replaces the process with a fresh copy of itself, so a
// restore leaves nothing stale in memory. Listening sockets close on exec
// (Go opens them close-on-exec) and the new process binds them again.
func restartSelf(log *slog.Logger) {
	exe, err := os.Executable()
	if err != nil {
		log.Error("restart after restore failed; restart MMP by hand", "err", err)
		return
	}
	log.Info("restarting after restore")
	if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
		log.Error("restart after restore failed; restart MMP by hand", "err", err)
	}
}

// mediaProbe runs ffprobe for the dashboard's stream details (resolution,
// frame rate, audio layout) when it is installed; without it the dashboard
// shows the codecs only.
func mediaProbe(path string, log *slog.Logger) proxy.MediaProbe {
	p, err := exec.LookPath(cmp.Or(path, "ffprobe"))
	if err != nil {
		log.Info("ffprobe not found: the dashboard shows codecs only", "path", cmp.Or(path, "ffprobe"))
		return nil
	}
	return proxy.FFprobe(p)
}
