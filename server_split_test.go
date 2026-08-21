package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSplitRecording_MetadataAttachesToNewSegmentNotClosedOne(t *testing.T) {
	srv, sess := newTestSplitServer(t)

	result, err := srv.SplitRecording(context.Background(), "call-1", map[string]any{"ticket": "t1"})
	require.NoError(t, err)
	assert.Equal(t, 1, result.NewSegmentSeq)

	require.NotNil(t, result.ClosedSegment)
	assert.Equal(t, 0, result.ClosedSegment.Sequence)
	assert.Equal(t, "api_split", result.ClosedSegment.StopReason)
	assert.Nil(t, result.ClosedSegment.RequestMetadata, "the caller-supplied metadata must not be retroactively attached to the segment that just closed")

	sess.mu.Lock()
	current := sess.CurrentSegment
	sess.mu.Unlock()
	require.NotNil(t, current)
	assert.Equal(t, map[string]any{"ticket": "t1"}, current.RequestMetadata, "the caller-supplied metadata belongs to the new segment going forward")

	// The closed segment's metadata JSON should already be on disk.
	meta := srv.metaUploader.(*captureUploader).Enqueued()
	require.Len(t, meta, 1)
	data, err := os.ReadFile(meta[0])
	require.NoError(t, err)
	var record callMetadataRecord
	require.NoError(t, json.Unmarshal(data, &record))
	assert.Equal(t, "call-1", record.SIPCallID)
	require.NotNil(t, record.Segment)
	assert.Equal(t, 0, record.Segment.Sequence)
	assert.Nil(t, record.Segment.RequestMetadata)

	// The metadata JSON must be named with the same timestamp component as
	// the segment's own .ulaw recording files, not the seg000/seg001-style
	// suffix used previously -- that way the JSON and its matching audio
	// files can be correlated by filename alone. Each audio file's stem is
	// "{metaStem}-{label}", so the metadata stem must be its prefix.
	metaStem := strings.TrimSuffix(filepath.Base(meta[0]), ".json")
	require.NotEmpty(t, result.ClosedSegment.RecordingFiles)
	for _, audioPath := range result.ClosedSegment.RecordingFiles {
		audioBase := filepath.Base(audioPath)
		audioStem := strings.TrimSuffix(audioBase, filepath.Ext(audioBase))
		assert.True(t, strings.HasPrefix(audioStem, metaStem+"-"),
			"expected metadata stem %q to be a prefix of audio stem %q", metaStem, audioStem)
	}
}

func TestSplitRecording_SequenceIncrementsAcrossMultipleSplits(t *testing.T) {
	srv, sess := newTestSplitServer(t)

	r1, err := srv.SplitRecording(context.Background(), "call-1", map[string]any{"a": 1})
	require.NoError(t, err)
	assert.Equal(t, 1, r1.NewSegmentSeq)

	r2, err := srv.SplitRecording(context.Background(), "call-1", map[string]any{"a": 2})
	require.NoError(t, err)
	assert.Equal(t, 2, r2.NewSegmentSeq)
	require.NotNil(t, r2.ClosedSegment)
	assert.Equal(t, 1, r2.ClosedSegment.Sequence)

	sess.mu.Lock()
	completedCount := len(sess.CompletedSegments)
	sess.mu.Unlock()
	assert.Equal(t, 2, completedCount)
}

func TestSplitRecording_MatchesByCallIDPrefix(t *testing.T) {
	fullCallID := "12344555_438274632_47324923@10.10.10.153"
	srv, sess := newTestSplitServerWithCallID(t, fullCallID)

	result, err := srv.SplitRecording(context.Background(), "12344555", nil)
	require.NoError(t, err)
	assert.Equal(t, sess.CallID, result.CallID)
	assert.Equal(t, 1, result.NewSegmentSeq)
}

func TestSplitRecording_PrefixMatchPicksMostRecentSession(t *testing.T) {
	srv, older := newTestSplitServerWithCallID(t, "12344555_111_aaa@10.10.10.1")
	older.CreatedAt = time.Now().Add(-time.Hour)

	newer := &recSession{
		CallID:    "12344555_222_bbb@10.10.10.2",
		CreatedAt: time.Now(),
		Legs: []*rtpRecorder{
			closedRecorder("inbound", "/tmp/newer-inbound.ulaw"),
			closedRecorder("outbound", "/tmp/newer-outbound.ulaw"),
		},
	}
	newer.beginRecordingSegmentLocked(newer.CreatedAt, newer.CreatedAt.UnixMilli())
	srv.sessions.Set(newer.CallID, newer)
	t.Cleanup(newer.Close)

	result, err := srv.SplitRecording(context.Background(), "12344555", nil)
	require.NoError(t, err)
	assert.Equal(t, newer.CallID, result.CallID)
}

func TestSplitRecording_CallNotFound(t *testing.T) {
	srv, _ := newTestSplitServer(t)
	_, err := srv.SplitRecording(context.Background(), "does-not-exist", nil)
	assert.ErrorIs(t, err, errCallNotFound)
}

func TestSplitRecording_CallAlreadyClosed(t *testing.T) {
	srv, sess := newTestSplitServer(t)
	sess.Close()

	_, err := srv.SplitRecording(context.Background(), "call-1", nil)
	assert.ErrorIs(t, err, errCallClosed)
}

func TestSplitRecording_AudioSeparatedAcrossSegments(t *testing.T) {
	srv, sess := newTestSplitServer(t)

	inboundLeg := sess.Legs[0]
	require.Equal(t, "inbound", inboundLeg.label)

	client, err := net.DialUDP("udp", nil, inboundLeg.conn.LocalAddr().(*net.UDPAddr))
	require.NoError(t, err)
	defer client.Close()

	_, err = client.Write(rtpPacket(0, 1, []byte("before")))
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	oldPath := inboundLeg.Path()

	result, err := srv.SplitRecording(context.Background(), "call-1", nil)
	require.NoError(t, err)

	_, err = client.Write(rtpPacket(0, 2, []byte("after")))
	require.NoError(t, err)
	time.Sleep(150 * time.Millisecond)

	oldData, err := os.ReadFile(oldPath)
	require.NoError(t, err)
	assert.Equal(t, "before", string(oldData))

	newData, err := os.ReadFile(result.NewRecordingFiles["inbound"])
	require.NoError(t, err)
	assert.Equal(t, "after", string(newData))
}

// TestRotateSegment_ReinviteAttachesMetaToClosedSegmentNotNew mirrors the
// re-INVITE path (onReInvite calls rotateSegment directly, not through the
// HTTP split API): the rs-metadata carried by the request that triggered
// the rotation belongs on the segment being CLOSED, not the new one --
// symmetric with how finalizeSession attaches BYE metadata to the segment
// it closes.
func TestRotateSegment_ReinviteAttachesMetaToClosedSegmentNotNew(t *testing.T) {
	srv, sess := newTestSplitServer(t)
	reinviteMeta := &SiprecMetadata{DataMode: "complete"}

	result, err := srv.rotateSegment(sess, time.Now().UTC(), "reinvite", nil, reinviteMeta)
	require.NoError(t, err)
	require.NotNil(t, result.ClosedSegment)
	assert.Equal(t, "reinvite", result.ClosedSegment.StopReason)

	meta := srv.metaUploader.(*captureUploader).Enqueued()
	require.Len(t, meta, 1)
	data, err := os.ReadFile(meta[0])
	require.NoError(t, err)
	var record callMetadataRecord
	require.NoError(t, json.Unmarshal(data, &record))
	require.NotNil(t, record.ReinviteMetadata)
	assert.Equal(t, "complete", record.ReinviteMetadata.DataMode)
	assert.Nil(t, record.ByeMetadata)

	sess.mu.Lock()
	current := sess.CurrentSegment
	sess.mu.Unlock()
	require.NotNil(t, current)
	assert.Nil(t, current.RequestMetadata, "reinvite-triggered rotations don't carry an external metadata dict for the new segment")
}

// TestRotateSegment_KeepsExistingLegsUntouched verifies the RTP sockets
// themselves are never closed or reallocated by a rotation -- only the file
// sink underneath each leg changes. This is the core assumption behind
// onReInvite reusing the same ports/answer SDP on every subsequent INVITE.
func TestRotateSegment_KeepsExistingLegsUntouched(t *testing.T) {
	srv, sess := newTestSplitServer(t)
	originalLegs := append([]*rtpRecorder(nil), sess.Legs...)
	originalConns := make([]*net.UDPConn, len(originalLegs))
	for i, leg := range originalLegs {
		originalConns[i] = leg.conn
	}

	_, err := srv.rotateSegment(sess, time.Now().UTC(), "reinvite", nil, nil)
	require.NoError(t, err)

	require.Equal(t, originalLegs, sess.Legs, "rotateSegment must not replace the leg slice")
	for i, leg := range sess.Legs {
		assert.Same(t, originalConns[i], leg.conn, "rotateSegment must not close/reallocate the RTP socket")
	}
}
