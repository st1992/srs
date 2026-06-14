package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

// Uploader moves completed recordings to durable storage.
//
// Lifecycle for a recording file:
//  1. MarkActive(path) is called when recording starts so the sweeper ignores it.
//  2. Enqueue(path) is called once the file is closed; it is uploaded and, on
//     success, deleted from local disk.
//  3. Sweep() scans the recording directory for orphaned/failed files (e.g. left
//     behind by a crash) and re-enqueues them.
type Uploader interface {
	MarkActive(path string)
	Enqueue(path string)
	Sweep()
	Shutdown(ctx context.Context)
}

// noopUploader keeps recordings on local disk (used when no GCS bucket is set).
type noopUploader struct {
	log *slog.Logger
}

func (n *noopUploader) MarkActive(string)       {}
func (n *noopUploader) Enqueue(p string)         { n.log.Debug("GCS upload disabled; keeping file", "file", p) }
func (n *noopUploader) Sweep()                   {}
func (n *noopUploader) Shutdown(context.Context) {}

// gcsUploader uploads .ulaw recordings to a GCS bucket with retries and
// deletes the local copy only after a confirmed successful upload.
type gcsUploader struct {
	client      *storage.Client
	bucket      string
	prefix      string
	dir         string
	deleteAfter bool
	maxRetries  int
	log         *slog.Logger

	queue  chan string
	wg     sync.WaitGroup
	sweep  *time.Ticker
	stop   chan struct{}
	closed chan struct{}

	mu       sync.Mutex
	active   map[string]struct{} // currently being recorded
	pending  map[string]struct{} // queued or uploading
	draining bool
}

// NewUploader builds an Uploader. If cfg.GCSBucket is empty a no-op uploader is
// returned so the recorder still works with local-only storage.
func NewUploader(ctx context.Context, cfg *Config, log *slog.Logger) (Uploader, error) {
	if cfg.GCSBucket == "" {
		log.Warn("GCS bucket not configured; recordings will stay on local disk")
		return &noopUploader{log: log}, nil
	}

	var opts []option.ClientOption
	if cfg.GCPCredentialsFile != "" {
		opts = append(opts, option.WithCredentialsFile(cfg.GCPCredentialsFile))
	}
	client, err := storage.NewClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCS client: %w", err)
	}

	workers := cfg.UploadWorkers
	if workers < 1 {
		workers = 1
	}

	u := &gcsUploader{
		client:      client,
		bucket:      cfg.GCSBucket,
		prefix:      cfg.GCSObjectPrefix,
		dir:         cfg.RecordingDir,
		deleteAfter: cfg.DeleteAfterUpload,
		maxRetries:  cfg.UploadMaxRetries,
		log:         log.With("gcsBucket", cfg.GCSBucket),
		queue:       make(chan string, 1024),
		stop:        make(chan struct{}),
		closed:      make(chan struct{}),
		active:      make(map[string]struct{}),
		pending:     make(map[string]struct{}),
	}

	for i := 0; i < workers; i++ {
		u.wg.Add(1)
		go u.worker()
	}

	// Recover any files left over from a previous run, then sweep periodically.
	u.Sweep()
	interval := time.Duration(cfg.UploadSweepIntervalSec) * time.Second
	if interval <= 0 {
		interval = 60 * time.Second
	}
	u.sweep = time.NewTicker(interval)
	go u.sweepLoop()

	return u, nil
}

// MarkActive registers a file as in-progress so the sweeper won't upload it.
func (u *gcsUploader) MarkActive(p string) {
	u.mu.Lock()
	u.active[p] = struct{}{}
	u.mu.Unlock()
}

// Enqueue schedules a closed recording file for upload.
func (u *gcsUploader) Enqueue(p string) {
	u.mu.Lock()
	delete(u.active, p)
	if u.draining {
		u.mu.Unlock()
		return
	}
	if _, exists := u.pending[p]; exists {
		u.mu.Unlock()
		return
	}
	u.pending[p] = struct{}{}
	u.mu.Unlock()

	select {
	case u.queue <- p:
	default:
		// Queue full: hand off to a goroutine so we never block the caller.
		go func() { u.queue <- p }()
	}
}

// Sweep enqueues any *.ulaw files in the recording dir that are neither active
// nor already pending (recovers orphaned/failed uploads).
func (u *gcsUploader) Sweep() {
	matches, err := filepath.Glob(filepath.Join(u.dir, "*.ulaw"))
	if err != nil {
		u.log.Error("upload sweep glob failed", "err", err)
		return
	}
	for _, p := range matches {
		u.mu.Lock()
		_, isActive := u.active[p]
		_, isPending := u.pending[p]
		u.mu.Unlock()
		if isActive || isPending {
			continue
		}
		u.log.Info("sweeping orphaned recording for upload", "file", p)
		u.Enqueue(p)
	}
}

func (u *gcsUploader) sweepLoop() {
	for {
		select {
		case <-u.stop:
			return
		case <-u.sweep.C:
			u.Sweep()
		}
	}
}

func (u *gcsUploader) worker() {
	defer u.wg.Done()
	for p := range u.queue {
		u.process(p)
		u.mu.Lock()
		delete(u.pending, p)
		u.mu.Unlock()
	}
}

// process uploads a single file with retries and deletes it on success.
func (u *gcsUploader) process(p string) {
	if _, err := os.Stat(p); err != nil {
		// File no longer exists (already uploaded/removed); nothing to do.
		return
	}

	objectName := u.objectName(p)
	log := u.log.With("file", p, "object", objectName)

	maxAttempts := u.maxRetries
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err := u.upload(p, objectName)
		if err == nil {
			log.Info("uploaded recording to GCS", "attempt", attempt)
			if u.deleteAfter {
				if rmErr := os.Remove(p); rmErr != nil {
					log.Error("upload succeeded but local delete failed", "err", rmErr)
				} else {
					log.Info("deleted local recording after successful upload")
				}
			}
			return
		}

		log.Warn("GCS upload attempt failed", "attempt", attempt, "err", err)
		if attempt < maxAttempts {
			backoff := time.Duration(attempt) * time.Second
			select {
			case <-u.stop:
				return
			case <-time.After(backoff):
			}
		}
	}

	log.Error("giving up uploading recording; left on local disk for next sweep")
}

// upload streams the file to GCS and finalizes the object.
func (u *gcsUploader) upload(p, objectName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	f, err := os.Open(p)
	if err != nil {
		return fmt.Errorf("open recording: %w", err)
	}
	defer f.Close()

	w := u.client.Bucket(u.bucket).Object(objectName).NewWriter(ctx)
	w.ContentType = "audio/basic" // G.711 mu-law
	if _, err := io.Copy(w, f); err != nil {
		_ = w.Close()
		return fmt.Errorf("copy to GCS: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("finalize GCS object: %w", err)
	}
	return nil
}

// objectName builds a deterministic, idempotent GCS object name for a file.
//
// The date partition is derived from the file's modification time (not the
// upload time) so that a re-upload after a crash/restart maps to the exact same
// object and simply overwrites it, rather than creating a duplicate under a
// different date.
func (u *gcsUploader) objectName(p string) string {
	base := filepath.Base(p)

	var datePart string
	if fi, err := os.Stat(p); err == nil {
		datePart = fi.ModTime().UTC().Format("2006/01/02")
	} else {
		datePart = time.Now().UTC().Format("2006/01/02")
	}

	if u.prefix == "" {
		return path.Join(datePart, base)
	}
	return path.Join(u.prefix, datePart, base)
}

// Shutdown stops accepting new work, drains in-flight uploads up to ctx, and
// closes the GCS client.
func (u *gcsUploader) Shutdown(ctx context.Context) {
	u.mu.Lock()
	if u.draining {
		u.mu.Unlock()
		return
	}
	u.draining = true
	u.mu.Unlock()

	close(u.stop)
	if u.sweep != nil {
		u.sweep.Stop()
	}
	close(u.queue)

	done := make(chan struct{})
	go func() {
		u.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		u.log.Info("all uploads drained")
	case <-ctx.Done():
		u.log.Warn("shutdown timeout reached; some uploads may be incomplete")
	}

	_ = u.client.Close()
}
