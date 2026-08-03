package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"
)

// Custom slog levels filling the gaps between the 4 stdlib levels
// (Debug=-4, Info=0, Warn=4, Error=8) so the app can express the same
// granularity as GCP Cloud Logging's severity enum. Alert/Emergency are
// deliberately omitted: those are infra-paging severities, not something
// application code should decide to emit.
const (
	LevelTrace    = slog.Level(-8)
	LevelNotice   = slog.Level(2)
	LevelCritical = slog.Level(12)
)

// parseLogLevel converts a config string into a slog.Level.
func parseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "trace":
		return LevelTrace, nil
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "notice":
		return LevelNotice, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	case "critical":
		return LevelCritical, nil
	default:
		return 0, fmt.Errorf("unknown log_level %q", s)
	}
}

// gcpSeverity maps a slog.Level onto a GCP Cloud Logging LogSeverity string.
// See https://cloud.google.com/logging/docs/reference/v2/rest/v2/LogEntry#logseverity
func gcpSeverity(l slog.Level) string {
	switch {
	case l < slog.LevelInfo:
		return "DEBUG"
	case l < LevelNotice:
		return "INFO"
	case l < slog.LevelWarn:
		return "NOTICE"
	case l < slog.LevelError:
		return "WARNING"
	case l < LevelCritical:
		return "ERROR"
	default:
		return "CRITICAL"
	}
}

// newLogger builds the application's structured logger. It emits JSON to
// stdout with keys GCP Cloud Logging recognizes as special fields, so log
// entries get correct severity in Cloud Logging without any client-side
// GCP logging library:
//   - "severity" (not slog's default "level") carrying a LogSeverity string.
//   - "time" is left as slog's default RFC3339 timestamp, which already
//     matches Cloud Logging's special "time" field.
//   - "message" (not slog's default "msg") so Log Explorer's summary line
//     heuristic picks it up.
func newLogger(level slog.Level) *slog.Logger {
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: gcpReplaceAttr,
	})
	return slog.New(h)
}

// gcpReplaceAttr renames slog's default level/message keys to the ones
// Cloud Logging recognizes; see newLogger's doc comment for details.
func gcpReplaceAttr(_ []string, a slog.Attr) slog.Attr {
	switch a.Key {
	case slog.LevelKey:
		lv, _ := a.Value.Any().(slog.Level)
		a.Key = "severity"
		a.Value = slog.StringValue(gcpSeverity(lv))
	case slog.MessageKey:
		a.Key = "message"
	}
	return a
}

// Structured event names attached to log lines that should be filterable/
// alertable in Cloud Logging without matching on message text.
const (
	eventCallEstablished    = "call_established"
	eventCallRejected       = "call_rejected"
	eventCallEnded          = "call_ended"
	eventPortExhausted      = "port_exhausted"
	eventRecordingStalled   = "recording_stalled"
	eventNoRTPReceived      = "no_rtp_received"
	eventSessionStale       = "session_stale"
	eventPanicRecovered     = "panic_recovered"
	eventUploadSucceeded    = "upload_succeeded"
	eventUploadFailed       = "upload_failed"
	eventUploadExhausted    = "upload_exhausted"
	eventLicenseCheckPassed = "license_check_passed"
	eventLicenseCheckFailed = "license_check_failed"
)

// recoverAndLog is deferred at the top of goroutines/handlers this process
// owns directly so a panic in one call's processing logs a Critical event
// and unwinds cleanly instead of crashing the whole recorder (and every
// other in-flight call along with it).
func recoverAndLog(log *slog.Logger, event string) {
	if r := recover(); r != nil {
		log.Log(context.Background(), LevelCritical, "recovered from panic",
			"event", eventPanicRecovered,
			"source", event,
			"panic", fmt.Sprintf("%v", r),
			"stack", string(debug.Stack()),
		)
	}
}
