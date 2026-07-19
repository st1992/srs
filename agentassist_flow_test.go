package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

type fakeAgentAssistClient struct {
	mu        sync.Mutex
	starts    int
	completes int
}

func (c *fakeAgentAssistClient) Start(_ context.Context, req AgentAssistStartRequest) (*agentAssistRun, error) {
	c.mu.Lock()
	c.starts++
	conv := fmt.Sprintf("conv-%d", 122+c.starts)
	c.mu.Unlock()

	sinks := make(map[string]rtpSink, len(req.Labels))
	for _, label := range req.Labels {
		sinks[label] = &memorySink{kind: "agent_assist"}
	}
	return &agentAssistRun{
		ConversationID: conv,
		Sinks:          sinks,
		Complete: func(context.Context) error {
			c.mu.Lock()
			c.completes++
			c.mu.Unlock()
			return nil
		},
	}, nil
}

func (c *fakeAgentAssistClient) Close() error { return nil }

type memorySink struct {
	mu      sync.Mutex
	kind    string
	payload []byte
	closed  bool
	err     error
}

func (s *memorySink) WriteRTPPayload(payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.payload = append(s.payload, payload...)
	return nil
}

func (s *memorySink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *memorySink) Path() string { return "" }
func (s *memorySink) Kind() string {
	if s.kind == "" {
		return "memory"
	}
	return s.kind
}

func newTestRecorderServer(t *testing.T) (*recorderServer, *recSession, *captureUploader, *captureUploader, *fakeAgentAssistClient) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.RecordingDir = t.TempDir()
	audio := &captureUploader{}
	meta := &captureUploader{}
	assist := &fakeAgentAssistClient{}
	srv := &recorderServer{
		cfg:          &cfg,
		log:          testLogger(),
		sessions:     newSessionStore(),
		uploader:     audio,
		metaUploader: meta,
		assist:       assist,
	}
	start := time.Date(2026, 7, 1, 1, 0, 0, 0, time.UTC)
	sess := &recSession{
		CallID:    "call-1",
		SourceIP:  "10.0.0.10:5060",
		From:      "sip:1111@example.com",
		To:        "sip:2222@example.com",
		DNIS:      "2222",
		ANI:       "1111",
		Headers:   map[string]string{"Call-ID": "call-1"},
		Legs:      []*rtpRecorder{closedRecorder("inbound", "/tmp/inbound.ulaw"), closedRecorder("outbound", "/tmp/outbound.ulaw")},
		StartTime: start.Format(time.RFC3339Nano),
		CreatedAt: start,
	}
	sess.beginRecordingSegmentLocked(start)
	srv.sessions.Set(sess.CallID, sess)
	return srv, sess, audio, meta, assist
}

func TestAgentAssistStartStopSegmentsRecording(t *testing.T) {
	srv, sess, audio, meta, assist := newTestRecorderServer(t)

	started, err := srv.StartAgentAssist(context.Background(), "call-1", map[string]any{"case_id": "abc"})
	require.NoError(t, err)
	assert.Equal(t, "conv-123", started.ConversationID)
	assert.Equal(t, sessionModeAgentAssist, started.State)
	assert.Equal(t, sessionModeAgentAssist, sess.Mode)
	assert.Len(t, audio.enqueued, 2)
	require.Len(t, meta.enqueued, 1)

	// No server-side audio recording happens while in Agent Assist mode: every
	// leg's sink must be the Agent Assist stream, never a file recording sink.
	for _, leg := range sess.Legs {
		assert.Equal(t, "agent_assist", leg.SinkKind())
	}

	preAssistMeta, err := os.ReadFile(meta.enqueued[0])
	require.NoError(t, err)
	assert.Contains(t, string(preAssistMeta), "agent_assist_start")
	assert.Contains(t, string(preAssistMeta), "case_id")
	assert.NotContains(t, string(preAssistMeta), "bye_metadata")

	stopped, err := srv.StopAgentAssist(context.Background(), "call-1")
	require.NoError(t, err)
	assert.Equal(t, "conv-123", stopped.ConversationID)
	assert.Equal(t, sessionModeRecording, stopped.State)
	assert.Equal(t, sessionModeRecording, sess.Mode)
	assert.Len(t, meta.enqueued, 2)
	assert.Len(t, audio.active, 2)
	assert.Equal(t, 1, assist.starts)
	assert.Equal(t, 1, assist.completes)

	agentMeta, err := os.ReadFile(meta.enqueued[1])
	require.NoError(t, err)
	assert.Contains(t, string(agentMeta), "agent_assist_stop")
	assert.Contains(t, string(agentMeta), "conv-123")
	assert.True(t, strings.Contains(audio.active[0], "seg") || len(audio.active[0]) > 0)
}

func TestAgentAssistStartWhileActiveRestarts(t *testing.T) {
	srv, sess, _, meta, assist := newTestRecorderServer(t)
	first, err := srv.StartAgentAssist(context.Background(), "call-1", nil)
	require.NoError(t, err)
	assert.Equal(t, "conv-123", first.ConversationID)

	again, err := srv.StartAgentAssist(context.Background(), "call-1", nil)
	require.NoError(t, err)
	assert.Equal(t, "conv-124", again.ConversationID)
	assert.NotEqual(t, first.ConversationID, again.ConversationID)
	assert.Equal(t, sessionModeAgentAssist, again.State)
	assert.Equal(t, sessionModeAgentAssist, sess.Mode)

	assert.Equal(t, 2, assist.starts)
	assert.Equal(t, 1, assist.completes) // the superseded (first) conversation was completed

	// meta.enqueued[0] is the initial recording segment closed by the first
	// StartAgentAssist call; meta.enqueued[1] is the first agent_assist
	// segment, closed out by the restart.
	require.Len(t, meta.enqueued, 2)
	restartMeta, err := os.ReadFile(meta.enqueued[1])
	require.NoError(t, err)
	assert.Contains(t, string(restartMeta), "agent_assist_restart")
	assert.Contains(t, string(restartMeta), "conv-123")
}

func TestPauseDuringRecordingBlocksWriteAndResumeRestores(t *testing.T) {
	srv, sess, _, _, _ := newTestRecorderServer(t)
	for _, leg := range sess.Legs {
		require.Equal(t, "recording", leg.SinkKind())
	}

	result, err := srv.PauseCall(context.Background(), "call-1")
	require.NoError(t, err)
	assert.True(t, result.Paused)
	assert.Equal(t, sessionModeRecording, result.State)
	for _, leg := range sess.Legs {
		assert.Equal(t, "discard", leg.SinkKind())
	}

	// Pausing an already-paused call is a no-op, not an error.
	again, err := srv.PauseCall(context.Background(), "call-1")
	require.NoError(t, err)
	assert.True(t, again.Paused)

	resumed, err := srv.ResumeCall(context.Background(), "call-1")
	require.NoError(t, err)
	assert.False(t, resumed.Paused)
	for _, leg := range sess.Legs {
		assert.Equal(t, "recording", leg.SinkKind())
	}

	// Resuming an already-resumed call is a no-op, not an error.
	again2, err := srv.ResumeCall(context.Background(), "call-1")
	require.NoError(t, err)
	assert.False(t, again2.Paused)
}

func TestPauseDuringAgentAssistSwapsToDiscardAndBack(t *testing.T) {
	srv, sess, _, _, _ := newTestRecorderServer(t)
	_, err := srv.StartAgentAssist(context.Background(), "call-1", nil)
	require.NoError(t, err)
	for _, leg := range sess.Legs {
		require.Equal(t, "agent_assist", leg.SinkKind())
	}

	_, err = srv.PauseCall(context.Background(), "call-1")
	require.NoError(t, err)
	for _, leg := range sess.Legs {
		assert.Equal(t, "discard", leg.SinkKind())
	}

	_, err = srv.ResumeCall(context.Background(), "call-1")
	require.NoError(t, err)
	for _, leg := range sess.Legs {
		assert.Equal(t, "agent_assist", leg.SinkKind())
	}
}

func TestModeSwitchBlockedWhilePaused(t *testing.T) {
	srv, _, _, _, assist := newTestRecorderServer(t)

	_, err := srv.PauseCall(context.Background(), "call-1")
	require.NoError(t, err)

	_, err = srv.StartAgentAssist(context.Background(), "call-1", nil)
	assert.ErrorIs(t, err, errCallPaused)
	assert.Equal(t, 0, assist.starts)

	_, err = srv.ResumeCall(context.Background(), "call-1")
	require.NoError(t, err)

	_, err = srv.StartAgentAssist(context.Background(), "call-1", nil)
	require.NoError(t, err)

	_, err = srv.PauseCall(context.Background(), "call-1")
	require.NoError(t, err)

	_, err = srv.StopAgentAssist(context.Background(), "call-1")
	assert.ErrorIs(t, err, errCallPaused)
}

func TestPauseResumeErrors(t *testing.T) {
	srv, sess, _, _, _ := newTestRecorderServer(t)

	_, err := srv.PauseCall(context.Background(), "missing")
	assert.ErrorIs(t, err, errCallNotFound)
	_, err = srv.ResumeCall(context.Background(), "missing")
	assert.ErrorIs(t, err, errCallNotFound)

	sess.mu.Lock()
	sess.Mode = sessionModeClosed
	sess.mu.Unlock()

	_, err = srv.PauseCall(context.Background(), "call-1")
	assert.ErrorIs(t, err, errInvalidTransition)
	_, err = srv.ResumeCall(context.Background(), "call-1")
	assert.ErrorIs(t, err, errInvalidTransition)
}

// TestFinalizeSessionWhilePausedClosesRealSink guards against a regression
// where ending a call while paused would close only the no-op discard sink
// (installed by Pause) and leave the real sink - and, for a file sink, its
// underlying os.File - never closed/flushed.
func TestFinalizeSessionWhilePausedClosesRealSink(t *testing.T) {
	srv, sess, _, meta, _ := newTestRecorderServer(t)
	real := &memorySink{kind: "recording"}
	for _, leg := range sess.Legs {
		leg.ReplaceSink(real)
	}

	_, err := srv.PauseCall(context.Background(), "call-1")
	require.NoError(t, err)
	require.True(t, sess.Paused)

	srv.finalizeSession(sess, time.Now().UTC().Format(time.RFC3339Nano), nil, "bye")

	assert.False(t, sess.Paused)
	assert.True(t, real.closed)
	assert.Len(t, meta.enqueued, 1)
}

func TestAgentAssistFallbackOnSinkError(t *testing.T) {
	srv, sess, _, meta, _ := newTestRecorderServer(t)
	_, err := srv.StartAgentAssist(context.Background(), "call-1", nil)
	require.NoError(t, err)

	srv.fallbackFromAgentAssist("call-1", assert.AnError)

	assert.Equal(t, sessionModeRecording, sess.Mode)
	require.Len(t, meta.enqueued, 2)
	data, err := os.ReadFile(meta.enqueued[1])
	require.NoError(t, err)
	assert.Contains(t, string(data), "agent_assist_error")
	assert.Contains(t, string(data), assert.AnError.Error())
}
