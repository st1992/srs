package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConcurrent_SplitRaceWithFinalize_NoOrphanedSegment races a
// SplitRecording call against a BYE-equivalent finalizeSession call on the
// same session, many times, to catch TOCTOU windows the Go race detector
// itself can't see (every field access here is already mutex-protected;
// the hazard is two separate lock/unlock sections in finalizeSession that
// let a concurrent split slip in between "segment completed" and "session
// marked closed").
//
// Invariant checked after every trial: every segment that ever appears in
// CompletedSegments must have produced exactly one metadata JSON on disk.
// If a split wins a race with finalize in the gap between those two lock
// sections, the segment it opens gets its sink closed by finalize's Close()
// but nobody ever calls completeCurrentSegmentLocked on it -- it's silently
// dropped: no metadata JSON, and the audio file (if any bytes were written)
// never gets enqueued for upload.
func TestConcurrent_SplitRaceWithFinalize_NoOrphanedSegment(t *testing.T) {
	const trials = 300

	for i := 0; i < trials; i++ {
		srv, sess := newTestSplitServer(t)

		var wg sync.WaitGroup
		start := make(chan struct{})

		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _ = srv.SplitRecording(context.Background(), "call-1", map[string]any{"trial": i})
		}()
		go func() {
			defer wg.Done()
			<-start
			srv.finalizeSession(sess, time.Now().UTC().Format(time.RFC3339Nano), nil, "bye")
		}()
		close(start)
		wg.Wait()

		sess.mu.Lock()
		closed := sess.closed
		dangling := sess.CurrentSegment
		completed := len(sess.CompletedSegments)
		sess.mu.Unlock()

		assert.True(t, closed, "trial %d: session must end up closed", i)
		assert.Nil(t, dangling, "trial %d: no segment should be left open after the race settles", i)

		written := len(srv.metaUploader.(*captureUploader).Enqueued())
		require.Equal(t, completed, written,
			"trial %d: every completed segment must have exactly one metadata JSON written -- got %d completed segments but %d metadata files (a segment was silently dropped)",
			i, completed, written)
	}
}

// TestConcurrent_ReinviteRaceWithFinalize_NoOrphanedSegment is the
// re-INVITE-triggered counterpart to
// TestConcurrent_SplitRaceWithFinalize_NoOrphanedSegment: onReInvite calls
// rotateSegment directly (reason "reinvite") rather than going through the
// HTTP split API's SplitRecording wrapper, but shares the exact same
// sess.mu-guarded core, so it must be safe against a BYE landing at the same
// moment a re-INVITE is being processed.
func TestConcurrent_ReinviteRaceWithFinalize_NoOrphanedSegment(t *testing.T) {
	const trials = 300

	for i := 0; i < trials; i++ {
		srv, sess := newTestSplitServer(t)

		var wg sync.WaitGroup
		start := make(chan struct{})

		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _ = srv.rotateSegment(sess, time.Now().UTC(), "reinvite", nil, nil)
		}()
		go func() {
			defer wg.Done()
			<-start
			srv.finalizeSession(sess, time.Now().UTC().Format(time.RFC3339Nano), nil, "bye")
		}()
		close(start)
		wg.Wait()

		sess.mu.Lock()
		closed := sess.closed
		dangling := sess.CurrentSegment
		completed := len(sess.CompletedSegments)
		sess.mu.Unlock()

		assert.True(t, closed, "trial %d: session must end up closed", i)
		assert.Nil(t, dangling, "trial %d: no segment should be left open after the race settles", i)

		written := len(srv.metaUploader.(*captureUploader).Enqueued())
		require.Equal(t, completed, written,
			"trial %d: every completed segment must have exactly one metadata JSON written -- got %d completed segments but %d metadata files (a segment was silently dropped)",
			i, completed, written)
	}
}

// TestConcurrent_MultipleSplitsSameCall fires many concurrent
// SplitRecording calls at the same live call and checks that every
// successful split produced a unique, gapless segment sequence with exactly
// one metadata JSON each, and every leg's file set is unique (no filename
// reused between segments, which would silently truncate a still-open
// sibling file -- see the lastSegmentStartMs collision guard in session.go).
func TestConcurrent_MultipleSplitsSameCall(t *testing.T) {
	const workers = 20
	srv, sess := newTestSplitServer(t)

	var wg sync.WaitGroup
	start := make(chan struct{})
	successes := make([]bool, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			_, err := srv.SplitRecording(context.Background(), "call-1", map[string]any{"worker": idx})
			successes[idx] = err == nil
		}(i)
	}
	close(start)
	wg.Wait()

	successCount := 0
	for _, ok := range successes {
		if ok {
			successCount++
		}
	}

	sess.mu.Lock()
	completedSegments := append([]*callSegment(nil), sess.CompletedSegments...)
	sess.mu.Unlock()

	require.Equal(t, successCount, len(completedSegments), "every successful split must close exactly one segment")

	seenSeq := map[int]bool{}
	seenFiles := map[string]bool{}
	for _, seg := range completedSegments {
		assert.False(t, seenSeq[seg.Sequence], "segment sequence %d reused", seg.Sequence)
		seenSeq[seg.Sequence] = true
		for label, path := range seg.RecordingFiles {
			key := fmt.Sprintf("%s:%s", label, path)
			assert.False(t, seenFiles[key], "recording file %s reused across segments", path)
			seenFiles[key] = true
		}
	}

	written := len(srv.metaUploader.(*captureUploader).Enqueued())
	assert.Equal(t, len(completedSegments), written, "every completed segment must have exactly one metadata JSON written")
}

// TestConcurrent_RTPDuringSplit sends a continuous stream of RTP packets on
// a live leg while repeatedly splitting that same call, verifying the
// sink-swap in rtp.go (rtpRecorder.ReplaceSink) never crashes, races, or
// double-writes: every byte that lands in a segment's file must have been
// sent, and total bytes recorded across all segments must not exceed total
// bytes sent (a few packets landing exactly on a swap boundary and being
// dropped is expected/acceptable UDP behavior, not a bug).
func TestConcurrent_RTPDuringSplit(t *testing.T) {
	srv, sess := newTestSplitServer(t)
	inboundLeg := sess.Legs[0]
	require.Equal(t, "inbound", inboundLeg.label)

	client, err := net.DialUDP("udp", nil, inboundLeg.conn.LocalAddr().(*net.UDPAddr))
	require.NoError(t, err)
	defer client.Close()

	stop := make(chan struct{})
	var sent int
	var senderWG sync.WaitGroup
	senderWG.Add(1)
	go func() {
		defer senderWG.Done()
		seq := uint16(0)
		for {
			select {
			case <-stop:
				return
			default:
				_, werr := client.Write(rtpPacket(0, seq, []byte("x")))
				if werr == nil {
					sent++
				}
				seq++
				time.Sleep(time.Millisecond)
			}
		}
	}()

	for i := 0; i < 15; i++ {
		_, err := srv.SplitRecording(context.Background(), "call-1", nil)
		require.NoError(t, err)
		time.Sleep(5 * time.Millisecond)
	}

	close(stop)
	senderWG.Wait()
	time.Sleep(50 * time.Millisecond) // drain in-flight packets

	// Finalize to close out the last segment and its files.
	srv.finalizeSession(sess, time.Now().UTC().Format(time.RFC3339Nano), nil, "bye")

	sess.mu.Lock()
	segments := append([]*callSegment(nil), sess.CompletedSegments...)
	sess.mu.Unlock()

	totalBytes := 0
	for _, seg := range segments {
		path, ok := seg.RecordingFiles["inbound"]
		if !ok {
			continue
		}
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		totalBytes += len(data)
	}

	assert.LessOrEqual(t, totalBytes, sent, "recorded bytes must never exceed bytes actually sent")
	t.Logf("sent=%d recorded=%d (some loss at swap boundaries is expected)", sent, totalBytes)
}
