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

var (
	errCallNotFound      = errors.New("call not found")
	errInvalidTransition = errors.New("invalid call state transition")
)

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

type startAgentAssistRequest struct {
	CallID   string         `json:"call_id"`
	Metadata map[string]any `json:"metadata"`
}

type stopAgentAssistRequest struct {
	CallID string `json:"call_id"`
}

type agentAssistResponse struct {
	CallID                    string `json:"call_id"`
	AgentAssistConversationID string `json:"agent_assist_conversation_id,omitempty"`
	State                     string `json:"state,omitempty"`
}

func (a *apiServer) handleStartAgentAssist(w http.ResponseWriter, r *http.Request) {
	if !a.authorized(r) {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
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
	result, err := a.recorder.StartAgentAssist(r.Context(), req.CallID, req.Metadata)
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

func (a *apiServer) handleStopAgentAssist(w http.ResponseWriter, r *http.Request) {
	if !a.authorized(r) {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
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

func (a *apiServer) authorized(r *http.Request) bool {
	if a.cfg.APIAuthToken == "" {
		return true
	}
	want := "Bearer " + a.cfg.APIAuthToken
	return r.Header.Get("Authorization") == want || r.Header.Get("X-API-Token") == a.cfg.APIAuthToken
}

func (a *apiServer) writeCallError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errCallNotFound):
		writeAPIError(w, http.StatusNotFound, err.Error())
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
