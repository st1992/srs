package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// closedRecorder builds an rtpRecorder usable in store tests: its done channel
// is pre-closed so Close() returns immediately without a running read loop.
func closedRecorder(label, path string) *rtpRecorder {
	done := make(chan struct{})
	close(done)
	return &rtpRecorder{label: label, sink: &fileSink{path: path, label: label}, path: path, log: testLogger(), done: done}
}

func TestSessionStore_SetGetExistsDelete(t *testing.T) {
	store := newSessionStore()

	sess := &recSession{CallID: "call-1", CreatedAt: time.Now()}
	store.Set("call-1", sess)

	got, ok := store.Get("call-1")
	require.True(t, ok)
	assert.Equal(t, "call-1", got.CallID)

	assert.True(t, store.Exists("call-1"))
	assert.False(t, store.Exists("missing"))

	deleted, ok := store.Delete("call-1")
	require.True(t, ok)
	assert.Equal(t, "call-1", deleted.CallID)
	assert.False(t, store.Exists("call-1"))

	_, ok = store.Delete("call-1")
	assert.False(t, ok)
}

func TestRecSession_RecordingFiles(t *testing.T) {
	sess := &recSession{
		CallID: "call-1",
		Legs: []*rtpRecorder{
			closedRecorder("inbound", "/tmp/call-1_inbound.ulaw"),
			closedRecorder("outbound", "/tmp/call-1_outbound.ulaw"),
		},
	}

	files := sess.RecordingFiles()
	assert.Equal(t, "/tmp/call-1_inbound.ulaw", files["inbound"])
	assert.Equal(t, "/tmp/call-1_outbound.ulaw", files["outbound"])
}

func TestRecSession_CloseIdempotent(t *testing.T) {
	sess := &recSession{
		CallID: "call-1",
		Legs:   []*rtpRecorder{closedRecorder("inbound", "/tmp/x.ulaw")},
	}
	sess.Close()
	sess.Close() // must not panic or block
}

func TestSessionStore_DrainAll(t *testing.T) {
	store := newSessionStore()
	store.Set("call-1", &recSession{
		CallID: "call-1",
		Legs:   []*rtpRecorder{closedRecorder("inbound", "/tmp/a.ulaw")},
	})
	store.Set("call-2", &recSession{
		CallID: "call-2",
		Legs:   []*rtpRecorder{closedRecorder("outbound", "/tmp/b.ulaw")},
	})

	drained := store.DrainAll()
	assert.Len(t, drained, 2)
	for _, sess := range drained {
		sess.Close()
	}

	assert.False(t, store.Exists("call-1"))
	assert.False(t, store.Exists("call-2"))
}
