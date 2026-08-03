package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
)

const rtpReadBufferSize = 1500

// rtpRecorder receives RTP for a single SIPREC leg and writes the raw payload
// (no transcoding) of PCMU packets to a .ulaw file.
type rtpRecorder struct {
	conn   *net.UDPConn
	file   *os.File
	path   string
	label  string
	pcmuPT uint8
	log    *slog.Logger

	closed  atomic.Bool
	packets atomic.Uint64
	bytes   atomic.Uint64
	done    chan struct{}

	noMediaTimer *time.Timer
}

// newRTPRecorder creates and opens the .ulaw output file for a leg.
// The filename format is: {callID}-{dnis}-{ani}-{startTimeMs}-{label}.ulaw
// where '-' is the field separator and each component is sanitized so it
// never contains '-' (component-internal '-' becomes '_').
//
// noMediaTimeoutSec, if > 0, starts a one-shot watchdog that logs a warning
// if zero RTP packets have been received by the time it fires — the SBC
// negotiated media for this leg but never sent any, which otherwise
// produces a valid-looking empty recording with no log signal at all.
func newRTPRecorder(conn *net.UDPConn, recordingDir, callID, dnis, ani string, startTimeMs int64, label string, pcmuPT uint8, noMediaTimeoutSec int, log *slog.Logger) (*rtpRecorder, error) {
	name := fmt.Sprintf("%s-%s-%s-%d-%s.ulaw",
		sanitizeFileComponent(callID),
		sanitizeFileComponent(dnis),
		sanitizeFileComponent(ani),
		startTimeMs,
		sanitizeFileComponent(label),
	)
	path := filepath.Join(recordingDir, name)

	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("failed to create recording file %q: %w", path, err)
	}

	r := &rtpRecorder{
		conn:   conn,
		file:   f,
		path:   path,
		label:  label,
		pcmuPT: pcmuPT,
		log:    log.With("label", label, "file", path),
		done:   make(chan struct{}),
	}

	if noMediaTimeoutSec > 0 {
		r.noMediaTimer = time.AfterFunc(time.Duration(noMediaTimeoutSec)*time.Second, func() {
			if r.packets.Load() == 0 && !r.closed.Load() {
				r.log.Warn("no RTP packets received within timeout after recording started",
					"event", eventNoRTPReceived,
					"timeout_sec", noMediaTimeoutSec,
				)
			}
		})
	}

	return r, nil
}

// Path returns the absolute or relative path of the .ulaw output file.
func (r *rtpRecorder) Path() string { return r.path }

// run reads RTP packets until the recorder is closed, writing PCMU payloads to disk.
func (r *rtpRecorder) run() {
	defer close(r.done)

	buf := make([]byte, rtpReadBufferSize)
	for {
		n, _, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			if r.closed.Load() {
				return
			}
			// The call is presumably still active in the session store, but
			// this leg's audio capture just died — silent data loss unless
			// flagged loudly.
			r.log.Log(context.Background(), LevelCritical, "RTP read error; recording stopped while call may still be active",
				"event", eventRecordingStalled, "err", err)
			return
		}

		var pkt rtp.Packet
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}

		// Only write the negotiated PCMU stream; skip DTMF/telephone-event and
		// any other interleaved payload types so the .ulaw file stays clean.
		if pkt.PayloadType != r.pcmuPT {
			continue
		}

		if _, err := r.file.Write(pkt.Payload); err != nil {
			r.log.Log(context.Background(), LevelCritical, "failed to write RTP payload; recording stopped while call may still be active",
				"event", eventRecordingStalled, "err", err)
			return
		}
		r.packets.Add(1)
		r.bytes.Add(uint64(len(pkt.Payload)))
	}
}

// Close stops the recorder, closes the UDP socket, and flushes/closes the file.
func (r *rtpRecorder) Close() {
	if !r.closed.CompareAndSwap(false, true) {
		return
	}
	if r.noMediaTimer != nil {
		r.noMediaTimer.Stop()
	}
	if r.conn != nil {
		_ = r.conn.Close()
	}
	<-r.done
	if r.file != nil {
		_ = r.file.Sync()
		_ = r.file.Close()
	}
	r.log.Info("recording finished", "packets", r.packets.Load(), "bytes", r.bytes.Load())
}

// sanitizeFileComponent replaces characters that are unsafe in file names or
// that conflict with the '-' field separator used in recording file names.
// '-' itself is replaced with '_' so it cannot be confused with the separator.
func sanitizeFileComponent(s string) string {
	repl := func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', ' ', '@', '-':
			return '_'
		default:
			return r
		}
	}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		out = append(out, repl(r))
	}
	if len(out) == 0 {
		return "unknown"
	}
	return string(out)
}
