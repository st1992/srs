package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		in      string
		want    slog.Level
		wantErr bool
	}{
		{"trace", LevelTrace, false},
		{"debug", slog.LevelDebug, false},
		{"", 0, true},
		{"Info", slog.LevelInfo, false},
		{"NOTICE", LevelNotice, false},
		{"warn", slog.LevelWarn, false},
		{"warning", slog.LevelWarn, false},
		{"error", slog.LevelError, false},
		{"critical", LevelCritical, false},
		{"nonsense", 0, true},
	}
	for _, tt := range tests {
		got, err := parseLogLevel(tt.in)
		if tt.wantErr {
			assert.Error(t, err, tt.in)
			continue
		}
		require.NoError(t, err, tt.in)
		assert.Equal(t, tt.want, got, tt.in)
	}
}

func TestGCPSeverity(t *testing.T) {
	tests := []struct {
		level slog.Level
		want  string
	}{
		{LevelTrace, "DEBUG"},
		{slog.LevelDebug, "DEBUG"},
		{slog.LevelInfo, "INFO"},
		{LevelNotice, "NOTICE"},
		{slog.LevelWarn, "WARNING"},
		{slog.LevelError, "ERROR"},
		{LevelCritical, "CRITICAL"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, gcpSeverity(tt.level))
	}
}

func TestNewLogger_EmitsGCPSeverityField(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{
		Level:       LevelTrace,
		ReplaceAttr: gcpReplaceAttr,
	}))

	log.Log(context.Background(), LevelCritical, "something bad happened", "event", "test_event")

	var out map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &out))
	assert.Equal(t, "CRITICAL", out["severity"])
	assert.Equal(t, "something bad happened", out["message"])
	assert.Equal(t, "test_event", out["event"])
	assert.NotContains(t, out, "level")
	assert.NotContains(t, out, "msg")
}

func TestRecoverAndLog_RecoversPanicAndLogsCritical(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{
		Level:       slog.LevelDebug,
		ReplaceAttr: gcpReplaceAttr,
	}))

	func() {
		defer recoverAndLog(log, "test_source")
		panic("boom")
	}()

	var out map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &out))
	assert.Equal(t, "CRITICAL", out["severity"])
	assert.Equal(t, eventPanicRecovered, out["event"])
	assert.Equal(t, "test_source", out["source"])
	assert.Contains(t, out["panic"], "boom")
}
