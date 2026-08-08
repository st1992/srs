package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestSplitServer builds a recorderServer with a single active session
// ("call-1") backed by real loopback rtpRecorders, so SplitRecording exercises
// the actual sink-swap path (not a mock).
func newTestSplitServer(t *testing.T) (*recorderServer, *recSession) {
	t.Helper()
	dir := t.TempDir()
	startTime := time.Now()
	startMs := startTime.UnixMilli()

	makeLeg := func(label string) *rtpRecorder {
		conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
		require.NoError(t, err)
		rec, err := newRTPRecorder(conn, dir, "call-1", "dnis1", "ani1", startMs, label, 0, 0, testLogger())
		require.NoError(t, err)
		go rec.run()
		return rec
	}

	cfg := DefaultConfig()
	cfg.RecordingDir = dir

	srv := &recorderServer{
		cfg:          &cfg,
		log:          testLogger(),
		sessions:     newSessionStore(),
		uploader:     &captureUploader{},
		metaUploader: &captureUploader{},
	}

	sess := &recSession{
		CallID:             "call-1",
		DNIS:               "dnis1",
		ANI:                "ani1",
		Legs:               []*rtpRecorder{makeLeg("inbound"), makeLeg("outbound")},
		StartTime:          startTime.UTC().Format(time.RFC3339Nano),
		CreatedAt:          startTime,
		lastSegmentStartMs: startMs,
	}
	sess.beginRecordingSegmentLocked(startTime, startMs)
	srv.sessions.Set(sess.CallID, sess)

	t.Cleanup(sess.Close)

	return srv, sess
}

func TestSplitRecordingAPI_Success(t *testing.T) {
	srv, sess := newTestSplitServer(t)
	cfg := *srv.cfg
	api := NewAPIServer(&cfg, srv, testLogger())

	oldFiles := sess.RecordingFiles()

	req := httptest.NewRequest(http.MethodPost, "/v1/recording/split", strings.NewReader(`{"call_id":"call-1","metadata":{"ticket":"t1"}}`))
	rec := httptest.NewRecorder()
	api.server.Handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"call_id":"call-1"`)
	assert.Contains(t, rec.Body.String(), `"new_segment_sequence":1`)

	newFiles := sess.RecordingFiles()
	assert.NotEqual(t, oldFiles["inbound"], newFiles["inbound"])
	assert.NotEqual(t, oldFiles["outbound"], newFiles["outbound"])
}

func TestSplitRecordingAPI_NotFound(t *testing.T) {
	srv, _ := newTestSplitServer(t)
	cfg := *srv.cfg
	api := NewAPIServer(&cfg, srv, testLogger())

	req := httptest.NewRequest(http.MethodPost, "/v1/recording/split", strings.NewReader(`{"call_id":"missing","metadata":{}}`))
	rec := httptest.NewRecorder()
	api.server.Handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestSplitRecordingAPI_MissingCallID(t *testing.T) {
	srv, _ := newTestSplitServer(t)
	cfg := *srv.cfg
	api := NewAPIServer(&cfg, srv, testLogger())

	req := httptest.NewRequest(http.MethodPost, "/v1/recording/split", strings.NewReader(`{"metadata":{}}`))
	rec := httptest.NewRecorder()
	api.server.Handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHealthzEndpoint(t *testing.T) {
	srv, _ := newTestSplitServer(t)
	cfg := *srv.cfg
	api := NewAPIServer(&cfg, srv, testLogger())

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	api.server.Handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}
