package main

import (
	"encoding/xml"
	"fmt"
	"strings"
)

// SiprecMetadata is the parsed representation of an rs-metadata+xml document
// (RFC 7865). Only the fields commonly populated by recording clients are modeled;
// unknown elements are ignored by the XML decoder.
type SiprecMetadata struct {
	XMLName      xml.Name      `xml:"recording" json:"-"`
	DataMode     string        `xml:"datamode" json:"data_mode,omitempty"`
	Sessions     []Session     `xml:"session" json:"sessions,omitempty"`
	Participants []Participant `xml:"participant" json:"participants,omitempty"`
	Streams      []Stream      `xml:"stream" json:"streams,omitempty"`

	// ParticipantSessionAssoc associates participants with sessions.
	ParticipantSessionAssoc []ParticipantSessionAssoc `xml:"participantsessionassoc" json:"participant_session_assoc,omitempty"`
	// ParticipantStreamAssoc associates participants with the streams they send/receive.
	ParticipantStreamAssoc []ParticipantStreamAssoc `xml:"participantstreamassoc" json:"participant_stream_assoc,omitempty"`
}

// Session describes a recorded communication session.
type Session struct {
	SessionID    string `xml:"session_id,attr" json:"session_id,omitempty"`
	SIPSessionID string `xml:"sipSessionID" json:"sip_session_id,omitempty"`
}

// Participant describes a party in the recorded session.
type Participant struct {
	ParticipantID string   `xml:"participant_id,attr" json:"participant_id,omitempty"`
	NameIDs       []NameID `xml:"nameID" json:"name_ids,omitempty"`
}

// NameID carries the address-of-record and display name for a participant.
type NameID struct {
	AOR  string `xml:"aor,attr" json:"aor,omitempty"`
	Name string `xml:"name" json:"name,omitempty"`
}

// Stream describes a recorded media stream and its label.
type Stream struct {
	StreamID  string `xml:"stream_id,attr" json:"stream_id,omitempty"`
	SessionID string `xml:"session_id,attr" json:"session_id,omitempty"`
	Label     string `xml:"label" json:"label,omitempty"`
}

// ParticipantSessionAssoc links a participant to a session.
type ParticipantSessionAssoc struct {
	ParticipantID string `xml:"participant_id,attr" json:"participant_id,omitempty"`
	SessionID     string `xml:"session_id,attr" json:"session_id,omitempty"`
}

// ParticipantStreamAssoc links a participant to the streams it sends/receives.
type ParticipantStreamAssoc struct {
	ParticipantID string   `xml:"participant_id,attr" json:"participant_id,omitempty"`
	Send          []string `xml:"send" json:"send,omitempty"`
	Recv          []string `xml:"recv" json:"recv,omitempty"`
}

// ParseSiprecMetadata unmarshals an rs-metadata+xml document into a SiprecMetadata.
func ParseSiprecMetadata(raw string) (*SiprecMetadata, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("empty metadata document")
	}

	var meta SiprecMetadata
	if err := xml.Unmarshal([]byte(raw), &meta); err != nil {
		return nil, fmt.Errorf("failed to unmarshal rs-metadata XML: %w", err)
	}
	return &meta, nil
}
