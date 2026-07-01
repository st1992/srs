package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/pion/rtp"
)

const rtpReadBufferSize = 1500

var errSinkClosed = errors.New("rtp sink closed")

type rtpSink interface {
	WriteRTPPayload([]byte) error
	Close() error
	Path() string
	Kind() string
}

type fileSink struct {
	file  *os.File
	path  string
	label string

	mu     sync.Mutex
	closed bool
}

func newFileSink(recordingDir, callID, dnis, ani string, startTimeMs int64, label string) (*fileSink, error) {
	name := fmt.Sprintf("%s-%s-%s-%d-%s.ulaw",
		sanitizeFileComponent(callID),
		sanitizeFileComponent(dnis),
		sanitizeFileComponent(ani),
		startTimeMs,
		sanitizeFileComponent(label),
	)
	p := filepath.Join(recordingDir, name)
	f, err := os.Create(p)
	if err != nil {
		return nil, fmt.Errorf("failed to create recording file %q: %w", p, err)
	}
	return &fileSink{file: f, path: p, label: label}, nil
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
func (s *fileSink) Kind() string { return "recording" }

type discardSink struct{}

func (discardSink) WriteRTPPayload([]byte) error { return nil }
func (discardSink) Close() error                 { return nil }
func (discardSink) Path() string                 { return "" }
func (discardSink) Kind() string                 { return "discard" }

// rtpRecorder receives RTP for a single SIPREC leg and writes each valid PCMU
// packet to the currently selected sink. The sink can be swapped at runtime
// when a call enters or leaves Agent Assist; packets are not buffered.
type rtpRecorder struct {
	conn   *net.UDPConn
	label  string
	pcmuPT uint8
	log    *slog.Logger

	mu          sync.RWMutex
	sink        rtpSink
	path        string
	onSinkError func(error)
	sinkErring  atomic.Bool

	closed  atomic.Bool
	packets atomic.Uint64
	bytes   atomic.Uint64
	done    chan struct{}
}

// newRTPRecorder creates and opens the .ulaw output file for a leg.
// The filename format is: {callID}-{dnis}-{ani}-{startTimeMs}-{label}.ulaw
// where '-' is the field separator and each component is sanitized so it
// never contains '-' (component-internal '-' becomes '_').
func newRTPRecorder(conn *net.UDPConn, recordingDir, callID, dnis, ani string, startTimeMs int64, label string, pcmuPT uint8, log *slog.Logger) (*rtpRecorder, error) {
	sink, err := newFileSink(recordingDir, callID, dnis, ani, startTimeMs, label)
	if err != nil {
		return nil, err
	}

	return &rtpRecorder{
		conn:   conn,
		label:  label,
		pcmuPT: pcmuPT,
		log:    log.With("label", label, "file", sink.Path()),
		sink:   sink,
		path:   sink.Path(),
		done:   make(chan struct{}),
	}, nil
}

// Path returns the absolute or relative path of the .ulaw output file.
func (r *rtpRecorder) Path() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.sink == nil {
		return r.path
	}
	return r.sink.Path()
}

func (r *rtpRecorder) SinkKind() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.sink == nil {
		return "none"
	}
	return r.sink.Kind()
}

// ReplaceSink switches future RTP packets to next and returns the previous
// sink. The caller owns closing the returned sink after the swap.
func (r *rtpRecorder) ReplaceSink(next rtpSink) rtpSink {
	if next == nil {
		next = discardSink{}
	}
	r.mu.Lock()
	old := r.sink
	r.sink = next
	if next.Path() != "" {
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
			r.log.Error("RTP read error", "err", err)
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
		if sink != nil {
			err = sink.WriteRTPPayload(pkt.Payload)
		}
		if err != nil {
			if errors.Is(err, errSinkClosed) {
				continue
			}
			r.log.Error("failed to write RTP payload", "err", err, "sink", r.SinkKind())
			if r.onSinkError != nil && r.sinkErring.CompareAndSwap(false, true) {
				go func() {
					defer r.sinkErring.Store(false)
					r.onSinkError(err)
				}()
			}
			continue
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
	if r.conn != nil {
		_ = r.conn.Close()
	}
	if r.done != nil {
		<-r.done
	}
	r.mu.Lock()
	sink := r.sink
	r.sink = nil
	if sink != nil && sink.Path() != "" {
		r.path = sink.Path()
	}
	r.mu.Unlock()
	if sink != nil {
		if err := sink.Close(); err != nil {
			r.log.Error("failed to close RTP sink", "err", err, "sink", sink.Kind())
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
