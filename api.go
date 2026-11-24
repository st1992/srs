package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// apiServer is the pod-local HTTP API for operational endpoints on
// in-progress calls, e.g. splitting a call's recording into a new segment.
// An external system finds which pod owns a call via the Redis call
// locator (see locator.go) and calls this API directly on that pod.
type apiServer struct {
	cfg      *Config
	recorder *recorderServer
	log      *slog.Logger
	server   *http.Server
}

func NewAPIServer(cfg *Config, recorder *recorderServer, log *slog.Logger) *apiServer {
	api := &apiServer{
		cfg:      cfg,
		recorder: recorder,
		log:      log.With("component", "api"),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", api.handleHealth)
	mux.HandleFunc("/v1/recording/split", api.handleSplitRecording)
	mux.HandleFunc("/v1/recording/pause", api.handlePauseRecording)
	mux.HandleFunc("/v1/recording/resume", api.handleResumeRecording)
	mux.HandleFunc("/v1/agent-assist/start", api.handleStartAgentAssist)
	mux.HandleFunc("/v1/agent-assist/stop", api.handleStopAgentAssist)
	api.server = &http.Server{
		Addr:              cfg.HTTPListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
	return api
}

func (a *apiServer) Start() {
	go func() {
		a.log.Info("HTTP API listening", "addr", a.cfg.HTTPListenAddr)
		if err := a.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.log.Error("HTTP API server error", "err", err)
		}
	}()
}

func (a *apiServer) Stop(ctx context.Context) error {
	if a.server == nil {
		return nil
	}
	return a.server.Shutdown(ctx)
}

func (a *apiServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type splitRecordingRequest struct {
	CallID   string         `json:"call_id"`
	Metadata map[string]any `json:"metadata"`
}

type splitRecordingResponse struct {
	CallID            string            `json:"call_id"`
	ClosedSegment     *callSegment      `json:"closed_segment,omitempty"`
	NewSegmentSeq     int               `json:"new_segment_sequence"`
	NewRecordingFiles map[string]string `json:"new_recording_files,omitempty"`
}

func (a *apiServer) handleSplitRecording(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req splitRecordingRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.CallID) == "" {
		writeAPIError(w, http.StatusBadRequest, "call_id is required")
		return
	}
	if req.Metadata == nil {
		req.Metadata = map[string]any{}
	}

	result, err := a.recorder.SplitRecording(r.Context(), req.CallID, req.Metadata)
	if err != nil {
		a.writeCallError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, splitRecordingResponse{
		CallID:            result.CallID,
		ClosedSegment:     result.ClosedSegment,
		NewSegmentSeq:     result.NewSegmentSeq,
		NewRecordingFiles: result.NewRecordingFiles,
	})
}

type startAgentAssistRequest struct {
	CallID   string         `json:"call_id"`
	Metadata map[string]any `json:"metadata"`
	// Optional; used only as a pair (see parseAgentAssistTarget).
	ConversationProfileID string `json:"aa_conversation_profile_id"`
	Region                string `json:"aa_region"`
}

type stopAgentAssistRequest struct {
	CallID string `json:"call_id"`
}

type agentAssistResponse struct {
	CallID                    string `json:"call_id"`
	AgentAssistConversationID string `json:"agent_assist_conversation_id,omitempty"`
	State                     string `json:"state,omitempty"`
}

// handleStartAgentAssist reroutes a call's legs from recording to a new
// Google Agent Assist conversation. See recorderServer.StartAgentAssist.
func (a *apiServer) handleStartAgentAssist(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req startAgentAssistRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.CallID) == "" {
		writeAPIError(w, http.StatusBadRequest, "call_id is required")
		return
	}
	if req.Metadata == nil {
		req.Metadata = map[string]any{}
	}
	target, ignoredLone, err := parseAgentAssistTarget(req.ConversationProfileID, req.Region)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if ignoredLone {
		a.log.Warn("aa_conversation_profile_id and aa_region must be sent together; using the configured agent assist profile",
			"callID", req.CallID,
			"profileID", req.ConversationProfileID,
			"region", req.Region,
		)
	}
	result, err := a.recorder.StartAgentAssist(r.Context(), req.CallID, req.Metadata, target)
	if err != nil {
		a.writeCallError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agentAssistResponse{
		CallID:                    result.CallID,
		AgentAssistConversationID: result.ConversationID,
		State:                     string(result.State),
	})
}

// handleStopAgentAssist ends a call's Agent Assist conversation and reroutes
// its legs back to recording. See recorderServer.StopAgentAssist.
func (a *apiServer) handleStopAgentAssist(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req stopAgentAssistRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.CallID) == "" {
		writeAPIError(w, http.StatusBadRequest, "call_id is required")
		return
	}
	result, err := a.recorder.StopAgentAssist(r.Context(), req.CallID)
	if err != nil {
		a.writeCallError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agentAssistResponse{
		CallID:                    result.CallID,
		AgentAssistConversationID: result.ConversationID,
		State:                     string(result.State),
	})
}

type pauseRecordingRequest struct {
	CallID string `json:"call_id"`
}

type pauseRecordingResponse struct {
	CallID string `json:"call_id"`
	State  string `json:"state"`
	Paused bool   `json:"paused"`
}

// handlePauseRecording makes a call's legs write silence in place of the
// call audio. See recorderServer.PauseRecording.
func (a *apiServer) handlePauseRecording(w http.ResponseWriter, r *http.Request) {
	a.handleSetPaused(w, r, a.recorder.PauseRecording)
}

// handleResumeRecording switches a paused call's legs back to the call
// audio. See recorderServer.ResumeRecording.
func (a *apiServer) handleResumeRecording(w http.ResponseWriter, r *http.Request) {
	a.handleSetPaused(w, r, a.recorder.ResumeRecording)
}

func (a *apiServer) handleSetPaused(w http.ResponseWriter, r *http.Request, set func(context.Context, string) (*pauseResult, error)) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req pauseRecordingRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.CallID) == "" {
		writeAPIError(w, http.StatusBadRequest, "call_id is required")
		return
	}
	result, err := set(r.Context(), req.CallID)
	if err != nil {
		a.writeCallError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pauseRecordingResponse{
		CallID: result.CallID,
		State:  string(result.State),
		Paused: result.Paused,
	})
}

func (a *apiServer) writeCallError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errCallNotFound):
		writeAPIError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errCallClosed):
		writeAPIError(w, http.StatusConflict, err.Error())
	case errors.Is(err, errInvalidTransition):
		writeAPIError(w, http.StatusConflict, err.Error())
	default:
		writeAPIError(w, http.StatusInternalServerError, err.Error())
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	defer r.Body.Close()
	body := http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("invalid JSON: multiple JSON values")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAPIError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
