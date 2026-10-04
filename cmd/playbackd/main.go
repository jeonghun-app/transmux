// Command playbackd serves the operator console, authenticated video,
// recording index, camera management and bounded MP4 export jobs.
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
	"sync"
	"syscall"
	"time"

	"github.com/jeonghun-app/transmux/internal/config"
	"github.com/jeonghun-app/transmux/internal/ffmpeg"
	"github.com/jeonghun-app/transmux/internal/playback"
	"github.com/jeonghun-app/transmux/internal/recording"
	"github.com/jeonghun-app/transmux/internal/storage"
)

var version = "dev"

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "playbackd: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	filename := flag.String("config", "/etc/transmux/playback.json", "playback configuration")
	validate := flag.Bool("validate", false, "validate configuration without accessing services")
	showVersion := flag.Bool("version", false, "print version")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	cfg, err := playback.LoadConfig(*filename)
	if err != nil {
		return err
	}
	ingest, err := config.Load(cfg.IngestConfig)
	if err != nil {
		return err
	}
	if *validate {
		fmt.Println("configuration valid")
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if _, err := ffmpeg.Probe(ctx, ingest.FFmpeg.Binary); err != nil {
		return err
	}
	store, err := storage.New(ctx, ingest.Storage)
	if err != nil {
		return err
	}
	index, err := recording.Open(cfg.IndexPath)
	if err != nil {
		return err
	}
	defer index.Close()
	app, err := playback.NewServer(cfg, ingest, store, index, log)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", cfg.HTTPListen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: app.Routes(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 20 * time.Second, WriteTimeout: time.Hour,
		IdleTimeout: time.Minute, MaxHeaderBytes: 32 << 10}
	var wg sync.WaitGroup
	wg.Go(func() { app.RunBackground(ctx) })
	serveErr := make(chan error, 1)
	wg.Go(func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			stop()
		}
	})
	log.Info("playbackd ready", "version", version, "listen", listener.Addr().String(),
		"public_url", cfg.PublicURL, "retention_enabled", cfg.Retention.Enabled)
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		_ = srv.Close()
	}
	wg.Wait()
	select {
	case err := <-serveErr:
		return err
	default:
		return nil
	}
}
