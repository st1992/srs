package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewUploader_NoBucketReturnsNoop(t *testing.T) {
	cfg := DefaultConfig()
	cfg.GCSBucket = ""

	u, err := NewUploader(context.Background(), &cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("NewUploader returned error: %v", err)
	}
	if _, ok := u.(*noopUploader); !ok {
		t.Fatalf("expected *noopUploader when GCSBucket is empty, got %T", u)
	}
}

func TestObjectName_DatePartitionFromMtime(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "call-abc_inbound.ulaw")
	if err := os.WriteFile(file, []byte("payload"), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	mtime := time.Date(2026, 6, 13, 10, 30, 0, 0, time.UTC)
	if err := os.Chtimes(file, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	u := &gcsUploader{prefix: "recordings"}
	got := u.objectName(file)
	want := "recordings/2026/06/13/call-abc_inbound.ulaw"
	if got != want {
		t.Fatalf("objectName = %q, want %q", got, want)
	}

	// Idempotency: a second call with the same (unchanged) file must produce the
	// same object name so re-uploads overwrite rather than duplicate.
	if again := u.objectName(file); again != got {
		t.Fatalf("objectName not deterministic: %q != %q", again, got)
	}
}

func TestObjectName_NoPrefix(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "call-xyz_outbound.ulaw")
	if err := os.WriteFile(file, []byte("payload"), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	mtime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(file, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	u := &gcsUploader{prefix: ""}
	got := u.objectName(file)
	want := "2026/01/02/call-xyz_outbound.ulaw"
	if got != want {
		t.Fatalf("objectName = %q, want %q", got, want)
	}
}
