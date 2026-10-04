// Command transmuxd receives RTSP, transmuxes to HLS without touching the
// codec, and stores segments and manifests in an S3-compatible object store.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/jeonghun-app/transmux/internal/camera"
	"github.com/jeonghun-app/transmux/internal/channel"
	"github.com/jeonghun-app/transmux/internal/config"
	"github.com/jeonghun-app/transmux/internal/ffmpeg"
	"github.com/jeonghun-app/transmux/internal/health"
	"github.com/jeonghun-app/transmux/internal/metrics"
	"github.com/jeonghun-app/transmux/internal/storage"
	"github.com/jeonghun-app/transmux/internal/upload"
)

// version is set at build time with -ldflags.
var version = "dev"

func main() {
	if err := run(); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		fmt.Fprintf(os.Stderr, "transmuxd: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "/etc/transmux/config.json", "path to the JSON configuration file")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn, error")
	showVersion := flag.Bool("version", false, "print version and exit")
	validateOnly := flag.Bool("validate", false, "validate the configuration and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return nil
	}

	log := newLogger(*logLevel)

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *validateOnly {
		fmt.Println("configuration valid")
		return nil
	}

	log.Info("transmuxd starting",
		"version", version,
		"shard_id", cfg.ShardID,
		"max_channels", cfg.MaxChannels,
		"go", runtime.Version(),
		"gomaxprocs", runtime.GOMAXPROCS(0))

	// Preflight: fail fast on a broken image instead of failing once per
	// channel forever.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ver, err := ffmpeg.Probe(ctx, cfg.FFmpeg.Binary)
	if err != nil {
		return err
	}
	log.Info("ffmpeg available", "version", ver)

	if err := os.MkdirAll(cfg.SpoolDir, 0o750); err != nil {
		return fmt.Errorf("create spool dir: %w", err)
	}
	if err := os.MkdirAll(cfg.StateDir, 0o750); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	store, err := newStore(ctx, cfg)
	if err != nil {
		return err
	}
	log.Info("object store configured", "store", store.Describe())

	reg := metrics.NewRegistry()
	reg.Gauge("transmux_build_info", "Always 1; labels carry build metadata.",
		metrics.Label{Name: "version", Value: version},
		metrics.Label{Name: "shard_id", Value: cfg.ShardID}).Set(1)

	coordinator := upload.NewCoordinator(store, cfg.Upload, reg)

	provider, err := camera.NewProvider(cfg.Cameras)
	if err != nil {
		return err
	}
	log.Info("camera provider configured", "provider", provider.Name())

	mgr := channel.NewManager(cfg, provider, coordinator, reg, log)

	handler := health.NewHandler(mgr, reg, store.Describe(), log)
	srv := &http.Server{
		Handler:           handler.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Bind synchronously. A monitoring API that failed to bind used to be a
	// logged warning, leaving the daemon running with no liveness probe, no
	// readiness probe and no metrics: the orchestrator would consider the
	// shard healthy precisely because it could not ask.
	listener, err := net.Listen("tcp", cfg.HTTPListen)
	if err != nil {
		return fmt.Errorf("bind monitoring API on %s: %w", cfg.HTTPListen, err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Info("monitoring API listening", "addr", listener.Addr().String(),
			"note", "endpoints are unauthenticated; keep this listener internal")
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("monitoring API failed", "error", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		mgr.Run(ctx)
	}()

	<-ctx.Done()
	log.Info("shutdown signal received, draining channels")

	// Stop serving before the channels go away so a probe does not observe a
	// half-torn-down shard and trigger a restart loop.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("monitoring API shutdown", "error", err)
	}

	wg.Wait()
	log.Info("transmuxd stopped cleanly")
	return nil
}

func newStore(ctx context.Context, cfg config.Config) (storage.ObjectStore, error) {
	switch cfg.Storage.Backend {
	case "s3":
		return storage.NewS3Store(ctx, cfg.Storage)
	case "filesystem":
		return storage.NewFilesystemStore(cfg.Storage.Root)
	default:
		return nil, fmt.Errorf("unsupported storage backend %q", cfg.Storage.Backend)
	}
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	// JSON to stdout: container log drivers and CloudWatch parse it directly.
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
