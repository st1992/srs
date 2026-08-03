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
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	// Bootstrap logger for errors before the configured log level is known.
	log := newLogger(slog.LevelInfo)

	configPath := flag.String("config", "config.yaml", "path to the YAML configuration file")
	flag.Parse()

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Error("failed to load configuration", "err", err)
		os.Exit(1)
	}

	// Rebuild the logger at the configured level now that config is loaded
	// (LoadConfig already validated cfg.LogLevel parses).
	level, _ := parseLogLevel(cfg.LogLevel)
	log = newLogger(level)

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

	srv, err := NewServer(cfg, uploader, metaUploader, log)
	if err != nil {
		log.Error("failed to create SIPREC server", "err", err)
		os.Exit(1)
	}

	if err := srv.Start(); err != nil {
		log.Error("failed to start SIPREC server", "err", err)
		os.Exit(1)
	}

	log.Info("siprec-recorder started")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Info("shutting down")
	srv.Stop()
}
