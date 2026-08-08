package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
)

const rtpReadBufferSize = 1500

var errSinkClosed = errors.New("rtp sink closed")

// rtpSink is the destination for a recording leg's raw PCMU payloads. It can
// be swapped out at runtime (see rtpRecorder.ReplaceSink) so a call can be
// split into multiple recording segments without dropping any packets.
type rtpSink interface {
	WriteRTPPayload([]byte) error
	Close() error
	Path() string
}

// fileSink writes PCMU payloads straight to a .ulaw file on disk.
type fileSink struct {
	file *os.File
	path string

	mu     sync.Mutex
	closed bool
}

// newFileSink creates and opens a .ulaw output file for a leg.
// The filename format is: {callID}-{dnis}-{ani}-{startTimeMs}-{label}.ulaw
// where '-' is the field separator and each component is sanitized so it
// never contains '-' (component-internal '-' becomes '_').
func newFileSink(recordingDir, callID, dnis, ani string, startTimeMs int64, label string) (*fileSink, error) {
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
	return &fileSink{file: f, path: path}, nil
}

func (s *fileSink) WriteRTPPayload(payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.file == nil {
		return errSinkClosed
	}
	_, err := s.file.Write(payload)
	return err
}

func (s *fileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.file == nil {
		return nil
	}
	if err := s.file.Sync(); err != nil {
		_ = s.file.Close()
		s.file = nil
		return err
	}
	err := s.file.Close()
	s.file = nil
	return err
}

func (s *fileSink) Path() string { return s.path }

// rtpRecorder receives RTP for a single SIPREC leg and writes each valid
// PCMU packet to the currently selected sink. The sink can be swapped at
// runtime (e.g. when the recording is split into a new segment); packets
// are not buffered across the swap.
type rtpRecorder struct {
	conn   *net.UDPConn
	label  string
	pcmuPT uint8
	log    *slog.Logger

	mu   sync.RWMutex
	sink rtpSink
	path string

	closed  atomic.Bool
	packets atomic.Uint64
	bytes   atomic.Uint64
	done    chan struct{}

	noMediaTimer *time.Timer
}

// newRTPRecorder creates and opens the .ulaw output file for a leg.
//
// noMediaTimeoutSec, if > 0, starts a one-shot watchdog that logs a warning
// if zero RTP packets have been received by the time it fires — the SBC
// negotiated media for this leg but never sent any, which otherwise
// produces a valid-looking empty recording with no log signal at all.
func newRTPRecorder(conn *net.UDPConn, recordingDir, callID, dnis, ani string, startTimeMs int64, label string, pcmuPT uint8, noMediaTimeoutSec int, log *slog.Logger) (*rtpRecorder, error) {
	sink, err := newFileSink(recordingDir, callID, dnis, ani, startTimeMs, label)
	if err != nil {
		return nil, err
	}

	r := &rtpRecorder{
		conn:   conn,
		label:  label,
		pcmuPT: pcmuPT,
		log:    log.With("label", label, "file", sink.Path()),
		sink:   sink,
		path:   sink.Path(),
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

// Path returns the absolute or relative path of the current .ulaw output file.
func (r *rtpRecorder) Path() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.path
}

// ReplaceSink switches future RTP packets over to next and returns the
// previous sink. The caller owns closing the returned sink after the swap.
func (r *rtpRecorder) ReplaceSink(next rtpSink) rtpSink {
	r.mu.Lock()
	old := r.sink
	r.sink = next
	if next != nil {
		r.path = next.Path()
	}
	r.mu.Unlock()
	return old
}

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

		r.mu.RLock()
		sink := r.sink
		r.mu.RUnlock()

		if sink == nil {
			continue
		}
		if err := sink.WriteRTPPayload(pkt.Payload); err != nil {
			if errors.Is(err, errSinkClosed) {
				// Narrow race window during a sink swap; the packet is lost
				// but the recorder itself is healthy.
				continue
			}
			r.log.Log(context.Background(), LevelCritical, "failed to write RTP payload; recording stopped while call may still be active",
				"event", eventRecordingStalled, "err", err)
			return
		}
		r.packets.Add(1)
		r.bytes.Add(uint64(len(pkt.Payload)))
	}
}

// Close stops the recorder, closes the UDP socket, and flushes/closes the
// current sink.
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

	r.mu.Lock()
	sink := r.sink
	r.sink = nil
	r.mu.Unlock()

	if sink != nil {
		if err := sink.Close(); err != nil {
			r.log.Error("failed to close RTP sink", "err", err)
		}
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
