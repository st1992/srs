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
	return &rtpRecorder{label: label, path: path, log: testLogger(), done: done}
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

func TestRecSession_SegmentLifecycle(t *testing.T) {
	sess := &recSession{
		CallID: "call-1",
		Legs: []*rtpRecorder{
			closedRecorder("inbound", "/tmp/seg0-inbound.ulaw"),
			closedRecorder("outbound", "/tmp/seg0-outbound.ulaw"),
		},
	}

	start := time.Now()
	sess.mu.Lock()
	sess.beginRecordingSegmentLocked(start, start.UnixMilli())
	sess.mu.Unlock()

	require.NotNil(t, sess.CurrentSegment)
	assert.Equal(t, 0, sess.CurrentSegment.Sequence)
	assert.Equal(t, "/tmp/seg0-inbound.ulaw", sess.CurrentSegment.RecordingFiles["inbound"])
	assert.Equal(t, sess.CurrentSegment.RecordingFiles, sess.RecordingFiles())

	// Simulate a split: swap the legs' paths, then close out segment 0 and
	// open segment 1.
	sess.Legs = []*rtpRecorder{
		closedRecorder("inbound", "/tmp/seg1-inbound.ulaw"),
		closedRecorder("outbound", "/tmp/seg1-outbound.ulaw"),
	}

	sess.mu.Lock()
	completed := sess.completeCurrentSegmentLocked(time.Now(), "api_split")
	now := time.Now()
	sess.beginRecordingSegmentLocked(now, now.UnixMilli())
	sess.CurrentSegment.RequestMetadata = map[string]any{"ticket": "t1"}
	sess.mu.Unlock()

	require.NotNil(t, completed)
	assert.Equal(t, 0, completed.Sequence)
	assert.Equal(t, "api_split", completed.StopReason)
	assert.NotEmpty(t, completed.EndTime)
	assert.Equal(t, "/tmp/seg0-inbound.ulaw", completed.RecordingFiles["inbound"])
	require.Len(t, sess.CompletedSegments, 1)
	assert.Same(t, completed, sess.CompletedSegments[0])

	require.NotNil(t, sess.CurrentSegment)
	assert.Equal(t, 1, sess.CurrentSegment.Sequence)
	assert.Equal(t, "/tmp/seg1-inbound.ulaw", sess.CurrentSegment.RecordingFiles["inbound"])
	assert.Equal(t, map[string]any{"ticket": "t1"}, sess.CurrentSegment.RequestMetadata)
	assert.Equal(t, sess.CurrentSegment.RecordingFiles, sess.RecordingFiles())
}

func TestRecSession_CompleteCurrentSegmentLocked_NilWhenNoSegment(t *testing.T) {
	sess := &recSession{CallID: "call-1"}
	sess.mu.Lock()
	completed := sess.completeCurrentSegmentLocked(time.Now(), "bye")
	sess.mu.Unlock()
	assert.Nil(t, completed)
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
