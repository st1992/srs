package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memorySink is a fake rtpSink used to simulate an Agent Assist stream in
// tests, without touching Dialogflow.
type memorySink struct {
	mu     sync.Mutex
	label  string
	closed bool
	writes [][]byte
}

func (s *memorySink) WriteRTPPayload(p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errSinkClosed
	}
	s.writes = append(s.writes, append([]byte(nil), p...))
	return nil
}

func (s *memorySink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *memorySink) Path() string { return "" }
func (s *memorySink) Kind() string { return "agent_assist" }

// fakeAgentAssistClient stands in for a real Dialogflow client: Start
// returns memorySinks and records how many times it was started/completed.
type fakeAgentAssistClient struct {
	mu             sync.Mutex
	starts         int
	completes      int
	startErr       error
	conversationID string
}

func (c *fakeAgentAssistClient) Start(_ context.Context, req AgentAssistStartRequest) (*agentAssistRun, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.startErr != nil {
		return nil, c.startErr
	}
	c.starts++
	conv := c.conversationID
	if conv == "" {
		conv = "conv-123"
	}
	sinks := make(map[string]rtpSink, len(req.Labels))
	for _, label := range req.Labels {
		sinks[label] = &memorySink{label: label}
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

func (c *fakeAgentAssistClient) completions() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.completes
}

// newTestAgentAssistServer builds on newTestSplitServerWithCallID, wiring in
// a fakeAgentAssistClient so StartAgentAssist/StopAgentAssist can be
// exercised without a real Dialogflow backend.
func newTestAgentAssistServer(t *testing.T) (*recorderServer, *recSession, *fakeAgentAssistClient) {
	t.Helper()
	srv, sess := newTestSplitServer(t)
	fake := &fakeAgentAssistClient{}
	srv.assist = fake
	return srv, sess, fake
}

func TestStartAgentAssist_SwapsSinksAndClosesRecordingSegment(t *testing.T) {
	srv, sess, fake := newTestAgentAssistServer(t)

	result, err := srv.StartAgentAssist(context.Background(), "call-1", map[string]any{"ticket": "t1"})
	require.NoError(t, err)
	assert.Equal(t, sessionModeAgentAssist, result.State)
	assert.Equal(t, "conv-123", result.ConversationID)
	assert.Equal(t, 1, fake.starts)

	sess.mu.Lock()
	mode := sess.Mode
	current := sess.CurrentSegment
	agentAssistSet := sess.AgentAssist != nil
	sess.mu.Unlock()
	assert.Equal(t, sessionModeAgentAssist, mode)
	assert.True(t, agentAssistSet)
	require.NotNil(t, current)
	assert.Equal(t, sessionModeAgentAssist, current.Mode)
	assert.Equal(t, "conv-123", current.ConversationID)

	for _, leg := range sess.Legs {
		assert.Equal(t, "agent_assist", leg.SinkKind())
	}

	// The pre-agent-assist recording segment's metadata JSON should already
	// be enqueued, and its files must no longer be reported by
	// RecordingFiles() now that the legs stream to Agent Assist.
	meta := srv.metaUploader.(*captureUploader).Enqueued()
	require.Len(t, meta, 1)
	assert.Empty(t, sess.RecordingFiles())
}

func TestStartAgentAssist_CallNotFound(t *testing.T) {
	srv, _, _ := newTestAgentAssistServer(t)
	_, err := srv.StartAgentAssist(context.Background(), "does-not-exist", nil)
	assert.ErrorIs(t, err, errCallNotFound)
}

func TestStartAgentAssist_IdempotentWhenAlreadyStarted(t *testing.T) {
	srv, _, fake := newTestAgentAssistServer(t)

	_, err := srv.StartAgentAssist(context.Background(), "call-1", nil)
	require.NoError(t, err)

	result, err := srv.StartAgentAssist(context.Background(), "call-1", nil)
	require.NoError(t, err)
	assert.Equal(t, sessionModeAgentAssist, result.State)
	assert.Equal(t, 1, fake.starts, "a second start while already in agent assist mode must not open a new conversation")
}

func TestStartAgentAssist_CallAlreadyClosed(t *testing.T) {
	srv, sess, _ := newTestAgentAssistServer(t)
	sess.Close()

	_, err := srv.StartAgentAssist(context.Background(), "call-1", nil)
	assert.ErrorIs(t, err, errCallClosed)
}

func TestStartAgentAssist_PropagatesClientError(t *testing.T) {
	srv, _, fake := newTestAgentAssistServer(t)
	fake.startErr = errors.New("dialogflow unavailable")

	_, err := srv.StartAgentAssist(context.Background(), "call-1", nil)
	assert.ErrorContains(t, err, "dialogflow unavailable")
}

// TestStartAgentAssist_MatchesByCallIDPrefix mirrors
// TestSplitRecording_MatchesByCallIDPrefix in server_split_test.go: callers
// may pass just the leading "_"-delimited segment of a Call-ID (see
// sessionStore.GetByPrefix), same as /v1/recording/split.
func TestStartAgentAssist_MatchesByCallIDPrefix(t *testing.T) {
	fullCallID := "12344555_438274632_47324923@10.10.10.153"
	srv, sess := newTestSplitServerWithCallID(t, fullCallID)
	fake := &fakeAgentAssistClient{}
	srv.assist = fake

	result, err := srv.StartAgentAssist(context.Background(), "12344555", nil)
	require.NoError(t, err)
	assert.Equal(t, sess.CallID, result.CallID, "the result must carry the resolved full Call-ID, not the prefix the caller sent")
	assert.Equal(t, sessionModeAgentAssist, result.State)
}

func TestStopAgentAssist_ResumesRecordingWithNewSegment(t *testing.T) {
	srv, sess, fake := newTestAgentAssistServer(t)

	_, err := srv.StartAgentAssist(context.Background(), "call-1", nil)
	require.NoError(t, err)

	result, err := srv.StopAgentAssist(context.Background(), "call-1")
	require.NoError(t, err)
	assert.Equal(t, sessionModeRecording, result.State)
	assert.Equal(t, "conv-123", result.ConversationID)
	assert.Equal(t, 1, fake.completions())

	sess.mu.Lock()
	mode := sess.Mode
	agentAssistCleared := sess.AgentAssist == nil
	current := sess.CurrentSegment
	completedCount := len(sess.CompletedSegments)
	sess.mu.Unlock()
	assert.Equal(t, sessionModeRecording, mode)
	assert.True(t, agentAssistCleared)
	require.NotNil(t, current)
	assert.Equal(t, sessionModeRecording, current.Mode)
	assert.Equal(t, 2, completedCount, "both the original recording segment and the agent-assist segment must be closed out")

	for _, leg := range sess.Legs {
		assert.Equal(t, "recording", leg.SinkKind())
	}
	assert.NotEmpty(t, sess.RecordingFiles())
}

func TestStopAgentAssist_IdempotentWhenAlreadyRecording(t *testing.T) {
	srv, _, fake := newTestAgentAssistServer(t)

	result, err := srv.StopAgentAssist(context.Background(), "call-1")
	require.NoError(t, err)
	assert.Equal(t, sessionModeRecording, result.State)
	assert.Equal(t, 0, fake.starts)
}

func TestStopAgentAssist_CallNotFound(t *testing.T) {
	srv, _, _ := newTestAgentAssistServer(t)
	_, err := srv.StopAgentAssist(context.Background(), "does-not-exist")
	assert.ErrorIs(t, err, errCallNotFound)
}

func TestStopAgentAssist_MatchesByCallIDPrefix(t *testing.T) {
	fullCallID := "12344555_438274632_47324923@10.10.10.153"
	srv, sess := newTestSplitServerWithCallID(t, fullCallID)
	fake := &fakeAgentAssistClient{}
	srv.assist = fake

	_, err := srv.StartAgentAssist(context.Background(), fullCallID, nil)
	require.NoError(t, err)

	result, err := srv.StopAgentAssist(context.Background(), "12344555")
	require.NoError(t, err)
	assert.Equal(t, sess.CallID, result.CallID)
	assert.Equal(t, sessionModeRecording, result.State)
}

func TestSplitRecording_RejectedWhileInAgentAssistMode(t *testing.T) {
	srv, _, _ := newTestAgentAssistServer(t)

	_, err := srv.StartAgentAssist(context.Background(), "call-1", nil)
	require.NoError(t, err)

	_, err = srv.SplitRecording(context.Background(), "call-1", nil)
	assert.ErrorIs(t, err, errInvalidTransition)
}

func TestFinalizeSession_CompletesInProgressAgentAssistConversation(t *testing.T) {
	srv, sess, fake := newTestAgentAssistServer(t)

	_, err := srv.StartAgentAssist(context.Background(), "call-1", nil)
	require.NoError(t, err)

	srv.finalizeSession(sess, "2026-01-01T00:00:00Z", nil, "bye")
	assert.Equal(t, 1, fake.completions(), "finalizing a call still in agent assist mode must complete its Dialogflow conversation")
}

// --- HTTP API ---

func TestAgentAssistAPI_StartAndStop(t *testing.T) {
	srv, sess, _ := newTestAgentAssistServer(t)
	cfg := *srv.cfg
	api := NewAPIServer(&cfg, srv, testLogger())

	startReq := httptest.NewRequest(http.MethodPost, "/v1/agent-assist/start", strings.NewReader(`{"call_id":"call-1","metadata":{"ticket":"t1"}}`))
	startRec := httptest.NewRecorder()
	api.server.Handler.ServeHTTP(startRec, startReq)
	require.Equal(t, http.StatusOK, startRec.Code)
	assert.Contains(t, startRec.Body.String(), `"state":"agent_assist"`)

	for _, leg := range sess.Legs {
		assert.Equal(t, "agent_assist", leg.SinkKind())
	}

	stopReq := httptest.NewRequest(http.MethodPost, "/v1/agent-assist/stop", strings.NewReader(`{"call_id":"call-1"}`))
	stopRec := httptest.NewRecorder()
	api.server.Handler.ServeHTTP(stopRec, stopReq)
	require.Equal(t, http.StatusOK, stopRec.Code)
	assert.Contains(t, stopRec.Body.String(), `"state":"recording"`)

	for _, leg := range sess.Legs {
		assert.Equal(t, "recording", leg.SinkKind())
	}
}

func TestAgentAssistAPI_StartMatchesByCallIDPrefix(t *testing.T) {
	fullCallID := "12344555_438274632_47324923@10.10.10.153"
	srv, _ := newTestSplitServerWithCallID(t, fullCallID)
	srv.assist = &fakeAgentAssistClient{}
	cfg := *srv.cfg
	api := NewAPIServer(&cfg, srv, testLogger())

	req := httptest.NewRequest(http.MethodPost, "/v1/agent-assist/start", strings.NewReader(`{"call_id":"12344555"}`))
	rec := httptest.NewRecorder()
	api.server.Handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"call_id":"`+fullCallID+`"`)
}

func TestAgentAssistAPI_StartNotFound(t *testing.T) {
	srv, _, _ := newTestAgentAssistServer(t)
	cfg := *srv.cfg
	api := NewAPIServer(&cfg, srv, testLogger())

	req := httptest.NewRequest(http.MethodPost, "/v1/agent-assist/start", strings.NewReader(`{"call_id":"missing"}`))
	rec := httptest.NewRecorder()
	api.server.Handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestAgentAssistAPI_SplitConflictsWhileActive(t *testing.T) {
	srv, _, _ := newTestAgentAssistServer(t)
	cfg := *srv.cfg
	api := NewAPIServer(&cfg, srv, testLogger())

	startReq := httptest.NewRequest(http.MethodPost, "/v1/agent-assist/start", strings.NewReader(`{"call_id":"call-1"}`))
	startRec := httptest.NewRecorder()
	api.server.Handler.ServeHTTP(startRec, startReq)
	require.Equal(t, http.StatusOK, startRec.Code)

	splitReq := httptest.NewRequest(http.MethodPost, "/v1/recording/split", strings.NewReader(`{"call_id":"call-1"}`))
	splitRec := httptest.NewRecorder()
	api.server.Handler.ServeHTTP(splitRec, splitReq)
	assert.Equal(t, http.StatusConflict, splitRec.Code)
}

// The rest of this file is one named test per scenario, but the endpoint
// helpers are pure functions of a single string, so a table reads better here
// -- the same shape TestConfigValidate uses in config_test.go.

func TestNormalizeAgentAssistLocation(t *testing.T) {
	tests := []struct {
		name     string
		location string
		want     string
	}{
		{name: "empty defaults to global", location: "", want: "global"},
		{name: "whitespace defaults to global", location: "   ", want: "global"},
		{name: "global passes through", location: "global", want: "global"},
		{name: "uppercase global lowered", location: "GLOBAL", want: "global"},
		{name: "us multi-region", location: "us", want: "us"},
		{name: "surrounding whitespace trimmed", location: "  us-central1  ", want: "us-central1"},
		{name: "mixed case lowered", location: "US-Central1", want: "us-central1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, normalizeAgentAssistLocation(tt.location))
		})
	}
}

func TestDialogflowEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		location string
		want     string
	}{
		{name: "global", location: "global", want: "dialogflow.googleapis.com:443"},
		{name: "empty defaults to global", location: "", want: "dialogflow.googleapis.com:443"},
		{name: "whitespace defaults to global", location: "   ", want: "dialogflow.googleapis.com:443"},
		{name: "uppercase global", location: "GLOBAL", want: "dialogflow.googleapis.com:443"},
		{name: "us multi-region", location: "us", want: "us-dialogflow.googleapis.com:443"},
		{name: "us-central1", location: "us-central1", want: "us-central1-dialogflow.googleapis.com:443"},
		{name: "europe-west2", location: "europe-west2", want: "europe-west2-dialogflow.googleapis.com:443"},
		{name: "australia-southeast1", location: "australia-southeast1", want: "australia-southeast1-dialogflow.googleapis.com:443"},
		{name: "surrounding whitespace trimmed", location: "  us-central1  ", want: "us-central1-dialogflow.googleapis.com:443"},
		{name: "mixed case lowered", location: "US-Central1", want: "us-central1-dialogflow.googleapis.com:443"},
		// Deliberate: there's no allow-list of known regions, so an unknown
		// one still derives a host (and fails DNS on first use) rather than
		// being silently rewritten to global.
		{name: "unknown region still derives", location: "mars-north1", want: "mars-north1-dialogflow.googleapis.com:443"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, dialogflowEndpoint(tt.location))
		})
	}
}

// The endpoint is derived in NewAgentAssistClient and the resource name in
// Start; this guards against the two normalizing differently. location() reads
// only cfg, so the nil Dialogflow clients here are harmless.
func TestGoogleAgentAssistClientLocationUsesSharedNormalization(t *testing.T) {
	c := &googleAgentAssistClient{cfg: &Config{AgentAssistLocationID: "  US-Central1 "}}
	assert.Equal(t, "us-central1", c.location())
	assert.Equal(t, "us-central1-dialogflow.googleapis.com:443", dialogflowEndpoint(c.cfg.AgentAssistLocationID))
}
