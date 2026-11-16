package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func legsPaused(sess *recSession) []bool {
	out := make([]bool, 0, len(sess.Legs))
	for _, leg := range sess.Legs {
		out = append(out, leg.paused.Load())
	}
	return out
}

func currentPauses(sess *recSession) []pauseInterval {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return append([]pauseInterval(nil), sess.CurrentSegment.Pauses...)
}

func TestPauseRecording_SetsLegsAndIsIdempotent(t *testing.T) {
	srv, sess := newTestSplitServer(t)

	result, err := srv.PauseRecording(context.Background(), "call-1")
	require.NoError(t, err)
	assert.Equal(t, &pauseResult{CallID: "call-1", State: sessionModeRecording, Paused: true}, result)
	assert.Equal(t, []bool{true, true}, legsPaused(sess))
	require.Len(t, currentPauses(sess), 1)

	again, err := srv.PauseRecording(context.Background(), "call-1")
	require.NoError(t, err)
	assert.Equal(t, result, again)
	assert.Len(t, currentPauses(sess), 1, "a repeated pause must not open a second interval")
}

func TestPauseRecording_MatchesByCallIDPrefix(t *testing.T) {
	fullCallID := "12344555_438274632_47324923@10.10.10.153"
	srv, sess := newTestSplitServerWithCallID(t, fullCallID)

	result, err := srv.PauseRecording(context.Background(), "12344555")
	require.NoError(t, err)
	assert.Equal(t, fullCallID, result.CallID)
	assert.Equal(t, []bool{true, true}, legsPaused(sess))

	result, err = srv.ResumeRecording(context.Background(), "12344555")
	require.NoError(t, err)
	assert.Equal(t, fullCallID, result.CallID)
	assert.False(t, result.Paused)
}

func TestPauseRecording_CallNotFound(t *testing.T) {
	srv, _ := newTestSplitServer(t)
	_, err := srv.PauseRecording(context.Background(), "nope")
	assert.ErrorIs(t, err, errCallNotFound)
	_, err = srv.ResumeRecording(context.Background(), "nope")
	assert.ErrorIs(t, err, errCallNotFound)
}

func TestPauseRecording_CallClosed(t *testing.T) {
	srv, sess := newTestSplitServer(t)
	sess.Close()
	_, err := srv.PauseRecording(context.Background(), "call-1")
	assert.ErrorIs(t, err, errCallClosed)
}

func TestResumeRecording_ClearsPauseAndClosesInterval(t *testing.T) {
	srv, sess := newTestSplitServer(t)

	resumed, err := srv.ResumeRecording(context.Background(), "call-1")
	require.NoError(t, err)
	assert.False(t, resumed.Paused, "resume on an unpaused call is a no-op")
	assert.Empty(t, currentPauses(sess))

	_, err = srv.PauseRecording(context.Background(), "call-1")
	require.NoError(t, err)
	_, err = srv.ResumeRecording(context.Background(), "call-1")
	require.NoError(t, err)

	assert.Equal(t, []bool{false, false}, legsPaused(sess))
	pauses := currentPauses(sess)
	require.Len(t, pauses, 1)
	assert.NotEmpty(t, pauses[0].StartTime)
	assert.NotEmpty(t, pauses[0].EndTime)
	assert.LessOrEqual(t, pauses[0].StartTime, pauses[0].EndTime)
}

func TestPause_SpansSplit(t *testing.T) {
	srv, sess := newTestSplitServer(t)

	_, err := srv.PauseRecording(context.Background(), "call-1")
	require.NoError(t, err)
	split, err := srv.SplitRecording(context.Background(), "call-1", nil)
	require.NoError(t, err)
	assert.Equal(t, []bool{true, true}, legsPaused(sess), "pause survives the sink swap")

	require.NotNil(t, split.ClosedSegment)
	require.Len(t, split.ClosedSegment.Pauses, 1)
	assert.Equal(t, split.ClosedSegment.EndTime, split.ClosedSegment.Pauses[0].EndTime, "interval ends where the segment ends")

	newPauses := currentPauses(sess)
	require.Len(t, newPauses, 1)
	assert.Equal(t, split.ClosedSegment.EndTime, newPauses[0].StartTime, "interval reopens where the new segment starts")
	assert.Empty(t, newPauses[0].EndTime)

	_, err = srv.ResumeRecording(context.Background(), "call-1")
	require.NoError(t, err)
	assert.NotEmpty(t, currentPauses(sess)[0].EndTime)
}

func TestPause_PersistsAcrossAgentAssist(t *testing.T) {
	srv, sess, _ := newTestAgentAssistServer(t)

	_, err := srv.PauseRecording(context.Background(), "call-1")
	require.NoError(t, err)
	started, err := srv.StartAgentAssist(context.Background(), "call-1", nil)
	require.NoError(t, err)
	assert.Equal(t, sessionModeAgentAssist, started.State)
	assert.Equal(t, []bool{true, true}, legsPaused(sess))
	pauses := currentPauses(sess)
	require.Len(t, pauses, 1, "the agent assist segment opens with the pause still running")
	assert.Empty(t, pauses[0].EndTime)

	paused, err := srv.PauseRecording(context.Background(), "call-1")
	require.NoError(t, err)
	assert.Equal(t, sessionModeAgentAssist, paused.State)

	_, err = srv.StopAgentAssist(context.Background(), "call-1")
	require.NoError(t, err)
	assert.Equal(t, []bool{true, true}, legsPaused(sess))
	require.Len(t, currentPauses(sess), 1)
}

func TestPause_FinalizeClosesOpenInterval(t *testing.T) {
	srv, sess := newTestSplitServer(t)

	_, err := srv.PauseRecording(context.Background(), "call-1")
	require.NoError(t, err)
	srv.finalizeSession(sess, sess.StartTime, nil, "bye")

	require.NotEmpty(t, sess.CompletedSegments)
	last := sess.CompletedSegments[len(sess.CompletedSegments)-1]
	require.Len(t, last.Pauses, 1)
	assert.Equal(t, last.EndTime, last.Pauses[0].EndTime)
}

func TestPauseResumeAPI(t *testing.T) {
	fullCallID := "12344555_438274632_47324923@10.10.10.153"
	srv, sess := newTestSplitServerWithCallID(t, fullCallID)
	cfg := *srv.cfg
	api := NewAPIServer(&cfg, srv, testLogger())

	do := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		api.server.Handler.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		return rec
	}

	rec := do(http.MethodPost, "/v1/recording/pause", `{"call_id":"12344555"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"call_id":"`+fullCallID+`","state":"recording","paused":true}`, rec.Body.String())
	assert.Equal(t, []bool{true, true}, legsPaused(sess))

	rec = do(http.MethodPost, "/v1/recording/resume", `{"call_id":"`+fullCallID+`"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"call_id":"`+fullCallID+`","state":"recording","paused":false}`, rec.Body.String())
	assert.Equal(t, []bool{false, false}, legsPaused(sess))

	for _, path := range []string{"/v1/recording/pause", "/v1/recording/resume"} {
		assert.Equal(t, http.StatusBadRequest, do(http.MethodPost, path, `{}`).Code, path)
		assert.Equal(t, http.StatusBadRequest, do(http.MethodPost, path, `{"call_id":"12344555","extra":1}`).Code, path)
		assert.Equal(t, http.StatusMethodNotAllowed, do(http.MethodGet, path, "").Code, path)
		assert.Equal(t, http.StatusNotFound, do(http.MethodPost, path, `{"call_id":"nope"}`).Code, path)
	}

	sess.Close()
	assert.Equal(t, http.StatusConflict, do(http.MethodPost, "/v1/recording/pause", `{"call_id":"12344555"}`).Code)
}
