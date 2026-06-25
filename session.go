package main

import (
	"sync"
	"time"
)

// recSession tracks a single SIPREC recording call and its two media legs.
type recSession struct {
	// CallID is the original SIP Call-ID.
	CallID string
	// SourceIP is the remote signaling source address.
	SourceIP string
	// From/To are the SIP From/To URIs of the original INVITE.
	From string
	To   string
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

	mu     sync.Mutex
	closed bool
}

// RecordingFiles returns a map of leg label -> recording file path.
func (s *recSession) RecordingFiles() map[string]string {
	files := make(map[string]string, len(s.Legs))
	for _, leg := range s.Legs {
		files[leg.label] = leg.Path()
	}
	return files
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
