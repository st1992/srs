// Command siprec-recorder is a SIPREC recording server.
//
// It answers SIPREC INVITEs, receives the two RTP audio streams, writes the raw
// PCMU (G.711 mu-law) payloads to .ulaw files without any transcoding, parses
// the rs-metadata XML and SIP headers, and uploads per-call metadata JSON files
// to a dedicated GCS bucket.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// buildExpires is injected at build time via -ldflags "-X main.buildExpires=YYYY-MM-DD".
// Defaults to 2026-09-14 (3 months from the initial release).
var buildExpires = "2026-09-14"

func checkExpiry() error {
	exp, err := time.Parse("2006-01-02", buildExpires)
	if err != nil {
		return fmt.Errorf("invalid build expiry date %q: %w", buildExpires, err)
	}
	if time.Now().UTC().After(exp.Add(24 * time.Hour)) {
		return fmt.Errorf("this build expired on %s", buildExpires)
	}
	return nil
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := checkExpiry(); err != nil {
		log.Error("build expiry check failed", "err", err, "expires", buildExpires)
		os.Exit(1)
	}

	configPath := flag.String("config", "config.yaml", "path to the YAML configuration file")
	flag.Parse()

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Error("failed to load configuration", "err", err)
		os.Exit(1)
	}

	if err := os.MkdirAll(cfg.RecordingDir, 0o755); err != nil {
		log.Error("failed to create recording directory", "err", err, "dir", cfg.RecordingDir)
		os.Exit(1)
	}

	ctx := context.Background()

	uploader, err := NewUploader(ctx, cfg, log)
	if err != nil {
		log.Error("failed to initialize GCS uploader", "err", err)
		os.Exit(1)
	}

	metaUploader, err := NewMetadataUploader(ctx, cfg, log)
	if err != nil {
		log.Error("failed to initialize GCS metadata uploader", "err", err)
		os.Exit(1)
	}

	locator, err := NewCallLocator(ctx, cfg, log)
	if err != nil {
		log.Error("failed to initialize call locator", "err", err)
		os.Exit(1)
	}
	defer locator.Close()

	assist, err := NewAgentAssistClient(ctx, cfg, log)
	if err != nil {
		log.Error("failed to initialize Agent Assist client", "err", err)
		os.Exit(1)
	}
	defer assist.Close()

	srv, err := NewServer(cfg, uploader, metaUploader, locator, assist, log)
	if err != nil {
		log.Error("failed to create SIPREC server", "err", err)
		os.Exit(1)
	}

	if err := srv.Start(); err != nil {
		log.Error("failed to start SIPREC server", "err", err)
		os.Exit(1)
	}

	api := NewAPIServer(cfg, srv, log)
	api.Start()

	log.Info("siprec-recorder started")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Info("shutting down")
	apiCtx, apiCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer apiCancel()
	if err := api.Stop(apiCtx); err != nil {
		log.Warn("failed to stop HTTP API cleanly", "err", err)
	}
	srv.Stop()
}
