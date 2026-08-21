package main

import (
	"strings"
	"sync"
	"time"
)

// callSegment describes one continuous slice of a call's recording, from
// when it started (call setup, or an API-triggered split) until it ended
// (another split, or the call's BYE/shutdown).
type callSegment struct {
	Sequence        int               `json:"sequence"`
	StartTime       string            `json:"start_time"`
	EndTime         string            `json:"end_time,omitempty"`
	RecordingFiles  map[string]string `json:"recording_files,omitempty"`
	RequestMetadata map[string]any    `json:"request_metadata,omitempty"`
	StopReason      string            `json:"stop_reason,omitempty"`

	// StartMs is the Unix-millisecond timestamp used to name this segment's
	// recording files (see newFileSink). It is kept internal (not
	// serialised) so the segment's metadata JSON file can be named with the
	// exact same timestamp component, letting the JSON and its matching
	// .ulaw files always be correlated by filename alone.
	StartMs int64 `json:"-"`
}

// recSession tracks a single SIPREC recording call and its two media legs.
type recSession struct {
	// CallID is the original SIP Call-ID.
	CallID string
	// SourceIP is the remote signaling source address.
	SourceIP string
	// From/To are the SIP From/To URIs of the original INVITE.
	From string
	To   string
	// DNIS is the called-party number, extracted from rs-metadata call_data
	// when available and falling back to the user part of the SIP To URI.
	DNIS string
	// ANI is the calling-party number, extracted from rs-metadata call_data
	// when available and falling back to the user part of the SIP From URI.
	ANI string
	// Headers captures selected SIP headers from the INVITE.
	Headers map[string]string
	// Metadata is the parsed rs-metadata (may be nil if absent/invalid).
	Metadata *SiprecMetadata
	// Legs holds the two media recorders, keyed by label (e.g. "inbound"/"outbound").
	Legs []*rtpRecorder

	// StartTime is the ISO 8601 timestamp captured when the INVITE arrived.
	// It is used as the Timestamp on published events so it matches the
	// Unix-millisecond component embedded in the recording file names.
	StartTime string

	CreatedAt time.Time

	// lastSegmentStartMs is the Unix-millisecond timestamp last used to name
	// a segment's recording files, seeded from the call's own start time.
	// nextSegmentStartMsLocked uses it to guarantee every segment gets a
	// strictly increasing timestamp even if two segments start within the
	// same millisecond (e.g. a split requested immediately after the call
	// begins) — otherwise a same-millisecond split would reuse the exact
	// filename of the still-open previous segment and os.Create would
	// truncate it out from under the open file handle.
	lastSegmentStartMs int64

	// SegmentSeq is the sequence number to assign to the next segment.
	SegmentSeq int
	// CurrentSegment is the in-progress recording segment, or nil if the
	// call hasn't started its first segment yet.
	CurrentSegment *callSegment
	// CompletedSegments holds every segment that has been closed out, in
	// order, via completeCurrentSegmentLocked.
	CompletedSegments []*callSegment

	mu     sync.Mutex
	closed bool
}

// RecordingFiles returns a map of leg label -> recording file path. If a
// segment is in progress, its recording file paths are returned; otherwise
// the legs' current paths are used directly.
func (s *recSession) RecordingFiles() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.CurrentSegment != nil && len(s.CurrentSegment.RecordingFiles) > 0 {
		files := make(map[string]string, len(s.CurrentSegment.RecordingFiles))
		for label, p := range s.CurrentSegment.RecordingFiles {
			files[label] = p
		}
		return files
	}
	return s.recordingFilesLocked()
}

func (s *recSession) recordingFilesLocked() map[string]string {
	files := make(map[string]string, len(s.Legs))
	for _, leg := range s.Legs {
		files[leg.label] = leg.Path()
	}
	return files
}

// beginRecordingSegmentLocked opens a new current segment starting at start,
// seeded with the legs' current recording file paths. startMs is the
// Unix-millisecond timestamp used to name those recording files (from
// nextSegmentStartMsLocked), so the segment's metadata JSON can later be
// named with the same timestamp. Callers must hold s.mu.
func (s *recSession) beginRecordingSegmentLocked(start time.Time, startMs int64) {
	s.CurrentSegment = &callSegment{
		Sequence:       s.SegmentSeq,
		StartTime:      start.UTC().Format(time.RFC3339Nano),
		StartMs:        startMs,
		RecordingFiles: s.recordingFilesLocked(),
	}
	s.SegmentSeq++
}

// nextSegmentStartMsLocked returns a Unix-millisecond timestamp for naming a
// new segment's recording files, guaranteed to be strictly greater than the
// one used by the previous segment (see lastSegmentStartMs). Callers must
// hold s.mu.
func (s *recSession) nextSegmentStartMsLocked(now time.Time) int64 {
	ms := now.UnixMilli()
	if ms <= s.lastSegmentStartMs {
		ms = s.lastSegmentStartMs + 1
	}
	s.lastSegmentStartMs = ms
	return ms
}

// completeCurrentSegmentLocked closes out the current segment (if any),
// appends it to CompletedSegments, and clears CurrentSegment. Callers must
// hold s.mu.
func (s *recSession) completeCurrentSegmentLocked(end time.Time, reason string) *callSegment {
	seg := s.CurrentSegment
	if seg == nil {
		return nil
	}
	completed := *seg
	completed.EndTime = end.UTC().Format(time.RFC3339Nano)
	completed.StopReason = reason
	s.CompletedSegments = append(s.CompletedSegments, &completed)
	s.CurrentSegment = nil
	return &completed
}

// finalize atomically completes the current segment and marks the session
// closed under a single hold of s.mu, then returns the completed segment
// (nil if there wasn't one, e.g. the session was already closed) and the
// legs to close.
//
// This must NOT be split into a completeCurrentSegmentLocked call followed
// by a separate call to Close(): a concurrent SplitRecording could acquire
// s.mu in the gap between those two critical sections, see closed == false,
// and open a brand new segment that this call would never observe -- Close()
// would then close that segment's sink out from under it without ever
// completing it, silently dropping a segment (no metadata JSON, files never
// enqueued for upload). Keeping "complete the segment" and "mark closed" in
// one critical section makes SplitRecording and finalize mutually exclusive:
// whichever acquires the lock first runs to completion before the other is
// let in, so no segment can be opened after the session is closed.
func (s *recSession) finalize(end time.Time, reason string) (*callSegment, []*rtpRecorder) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil
	}
	completed := s.completeCurrentSegmentLocked(end, reason)
	s.closed = true
	return completed, s.Legs
}

// Close shuts down all leg recorders exactly once.
func (s *recSession) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	legs := s.Legs
	s.mu.Unlock()

	for _, leg := range legs {
		leg.Close()
	}
}

// sessionStore is a thread-safe map of SIP Call-ID -> recSession.
type sessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*recSession
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: make(map[string]*recSession)}
}

func (s *sessionStore) Get(callID string) (*recSession, bool) {
	s.mu.RLock()
	sess, ok := s.sessions[callID]
	s.mu.RUnlock()
	return sess, ok
}

// callIDPrefix returns the portion of a SIP Call-ID before its first "_",
// e.g. "12344555" for "12344555_438274632_47324923@10.10.10.153". If the
// Call-ID contains no "_", the whole string is returned.
func callIDPrefix(callID string) string {
	if idx := strings.IndexByte(callID, '_'); idx >= 0 {
		return callID[:idx]
	}
	return callID
}

// GetByPrefix looks up a session whose Call-ID's leading "_"-delimited
// segment equals prefix (see callIDPrefix). If more than one session
// matches, the most recently created one is returned.
func (s *sessionStore) GetByPrefix(prefix string) (*recSession, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var best *recSession
	for _, sess := range s.sessions {
		if callIDPrefix(sess.CallID) != prefix {
			continue
		}
		if best == nil || sess.CreatedAt.After(best.CreatedAt) {
			best = sess
		}
	}
	return best, best != nil
}

func (s *sessionStore) Set(callID string, sess *recSession) {
	s.mu.Lock()
	s.sessions[callID] = sess
	s.mu.Unlock()
}

func (s *sessionStore) Exists(callID string) bool {
	s.mu.RLock()
	_, ok := s.sessions[callID]
	s.mu.RUnlock()
	return ok
}

// Snapshot returns a point-in-time copy of all active sessions. Unlike
// DrainAll, the store is left unmodified.
func (s *sessionStore) Snapshot() []*recSession {
	s.mu.RLock()
	sessions := make([]*recSession, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.mu.RUnlock()
	return sessions
}

// Delete removes a session from the store and returns it if present.
func (s *sessionStore) Delete(callID string) (*recSession, bool) {
	s.mu.Lock()
	sess, ok := s.sessions[callID]
	if ok {
		delete(s.sessions, callID)
	}
	s.mu.Unlock()
	return sess, ok
}

// DrainAll removes every session from the store and returns them. The caller is
// responsible for closing them (used during shutdown).
func (s *sessionStore) DrainAll() []*recSession {
	s.mu.Lock()
	sessions := make([]*recSession, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.sessions = make(map[string]*recSession)
	s.mu.Unlock()
	return sessions
}
