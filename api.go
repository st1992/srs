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

func (a *apiServer) writeCallError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errCallNotFound):
		writeAPIError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errCallClosed):
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
