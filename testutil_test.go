package main

import (
	"context"
	"sync"
)

// captureUploader is an in-memory Uploader used by tests to assert which
// files were marked active / enqueued for upload, without touching GCS.
type captureUploader struct {
	mu       sync.Mutex
	active   []string
	enqueued []string
}

func (u *captureUploader) MarkActive(p string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.active = append(u.active, p)
}

func (u *captureUploader) Enqueue(p string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.enqueued = append(u.enqueued, p)
}

func (u *captureUploader) Sweep()                   {}
func (u *captureUploader) Shutdown(context.Context) {}

func (u *captureUploader) Enqueued() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]string, len(u.enqueued))
	copy(out, u.enqueued)
	return out
}
