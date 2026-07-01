package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentAssistAPIStartStop(t *testing.T) {
	srv, _, _, _, _ := newTestRecorderServer(t)
	cfg := *srv.cfg
	api := NewAPIServer(&cfg, srv, testLogger())

	startReq := httptest.NewRequest(http.MethodPost, "/v1/agent-assist/start", strings.NewReader(`{"call_id":"call-1","metadata":{"ticket":"t1"}}`))
	startRec := httptest.NewRecorder()
	api.server.Handler.ServeHTTP(startRec, startReq)

	require.Equal(t, http.StatusOK, startRec.Code)
	assert.Contains(t, startRec.Body.String(), `"call_id":"call-1"`)
	assert.Contains(t, startRec.Body.String(), `"agent_assist_conversation_id":"conv-123"`)
	assert.Contains(t, startRec.Body.String(), `"state":"agent_assist"`)

	stopReq := httptest.NewRequest(http.MethodPost, "/v1/agent-assist/stop", strings.NewReader(`{"call_id":"call-1"}`))
	stopRec := httptest.NewRecorder()
	api.server.Handler.ServeHTTP(stopRec, stopReq)

	require.Equal(t, http.StatusOK, stopRec.Code)
	assert.Contains(t, stopRec.Body.String(), `"state":"recording"`)
}

func TestAgentAssistAPIAuthAndNotFound(t *testing.T) {
	srv, _, _, _, _ := newTestRecorderServer(t)
	cfg := *srv.cfg
	cfg.APIAuthToken = "secret"
	api := NewAPIServer(&cfg, srv, testLogger())

	unauthReq := httptest.NewRequest(http.MethodPost, "/v1/agent-assist/start", strings.NewReader(`{"call_id":"call-1","metadata":{}}`))
	unauthRec := httptest.NewRecorder()
	api.server.Handler.ServeHTTP(unauthRec, unauthReq)
	require.Equal(t, http.StatusUnauthorized, unauthRec.Code)

	missingReq := httptest.NewRequest(http.MethodPost, "/v1/agent-assist/start", strings.NewReader(`{"call_id":"missing","metadata":{}}`))
	missingReq.Header.Set("Authorization", "Bearer secret")
	missingRec := httptest.NewRecorder()
	api.server.Handler.ServeHTTP(missingRec, missingReq)
	require.Equal(t, http.StatusNotFound, missingRec.Code)
}
