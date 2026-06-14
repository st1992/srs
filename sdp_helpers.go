package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SDPLine represents a parsed SDP line with its type and full value.
type SDPLine struct {
	Type  byte   // 'v', 'o', 's', 'c', 't', 'm', 'a', etc.
	Value string // The full line including the leading type=
}

// ParseSiprecSDP parses the SDP and returns session-level lines and media blocks.
// SIPREC SDPs typically have two media sections (one per recorded stream).
func ParseSiprecSDP(rawSDP string) (session []SDPLine, mediaBlocks [][]SDPLine, err error) {
	rawSDP = strings.ReplaceAll(rawSDP, "\r\n", "\n")
	rawSDP = strings.ReplaceAll(rawSDP, "\r", "\n")

	lines := strings.Split(rawSDP, "\n")
	var currentMedia []SDPLine

	for _, l := range lines {
		line := strings.TrimSpace(l)
		if line == "" {
			continue
		}

		var lineType byte = '?'
		if len(line) >= 2 && line[1] == '=' {
			lineType = line[0]
		}

		sdpLine := SDPLine{Type: lineType, Value: line}

		if strings.HasPrefix(line, "m=") {
			if currentMedia != nil {
				mediaBlocks = append(mediaBlocks, currentMedia)
			}
			currentMedia = []SDPLine{sdpLine}
			continue
		}

		if currentMedia == nil {
			// Skip a=group:DUP (SIPREC-specific grouping not needed in split SDPs)
			if strings.HasPrefix(line, "a=group:DUP") {
				continue
			}
			session = append(session, sdpLine)
		} else {
			currentMedia = append(currentMedia, sdpLine)
		}
	}

	if currentMedia != nil {
		mediaBlocks = append(mediaBlocks, currentMedia)
	}

	return session, mediaBlocks, nil
}

// ExtractSiprecMediaLabel extracts the label from media block attributes (a=label:X).
// SIPREC uses labels like "inbound" and "outbound" to identify streams.
func ExtractSiprecMediaLabel(media []SDPLine) string {
	for _, line := range media {
		if line.Type == 'a' && strings.HasPrefix(line.Value, "a=label:") {
			return strings.TrimPrefix(line.Value, "a=label:")
		}
	}
	return ""
}

// ExtractSiprecMediaPort extracts the port from the m= line of a media block.
func ExtractSiprecMediaPort(media []SDPLine) (int, error) {
	for _, line := range media {
		if line.Type == 'm' {
			parts := strings.Fields(line.Value)
			if len(parts) < 2 {
				return 0, fmt.Errorf("invalid m= line: %s", line.Value)
			}
			port, err := strconv.Atoi(parts[1])
			if err != nil {
				return 0, fmt.Errorf("invalid port in m= line: %s", line.Value)
			}
			return port, nil
		}
	}
	return 0, fmt.Errorf("no m= line found in media block")
}

// extractPCMUPayloadType inspects rtpmap lines in a media block and returns the
// RTP payload type number mapped to PCMU/8000. Defaults to 0 (the standard
// static payload type for PCMU) when no explicit rtpmap is present.
func extractPCMUPayloadType(media []SDPLine) uint8 {
	for _, line := range media {
		if line.Type != 'a' || !strings.HasPrefix(line.Value, "a=rtpmap:") {
			continue
		}
		// a=rtpmap:<pt> <encoding>/<clock>
		rest := strings.TrimPrefix(line.Value, "a=rtpmap:")
		fields := strings.Fields(rest)
		if len(fields) < 2 {
			continue
		}
		if strings.HasPrefix(strings.ToUpper(fields[1]), "PCMU/") {
			if pt, err := strconv.Atoi(fields[0]); err == nil && pt >= 0 && pt < 128 {
				return uint8(pt)
			}
		}
	}
	return 0
}

// BuildLegAnswerSDP builds a complete single-media SDP answer for one SIPREC leg.
// It forces PCMU (so no transcoding is required) and advertises the recorder's
// local media IP and the allocated RTP port. The stream is marked recvonly
// since the recorder only receives audio.
func BuildLegAnswerSDP(mediaIP string, localPort int, pcmuPT uint8, label string) string {
	sessID := uint64(time.Now().Unix()) + 2208988800 // seconds since 1900 (NTP epoch)
	var b strings.Builder
	b.WriteString("v=0\r\n")
	b.WriteString(fmt.Sprintf("o=streamlink %d %d IN IP4 %s\r\n", sessID, sessID, mediaIP))
	b.WriteString("s=EXL StreamLink\r\n")
	b.WriteString(fmt.Sprintf("c=IN IP4 %s\r\n", mediaIP))
	b.WriteString("t=0 0\r\n")
	b.WriteString(fmt.Sprintf("m=audio %d RTP/AVP %d\r\n", localPort, pcmuPT))
	b.WriteString(fmt.Sprintf("a=rtpmap:%d PCMU/8000\r\n", pcmuPT))
	b.WriteString("a=ptime:20\r\n")
	b.WriteString("a=recvonly\r\n")
	if label != "" {
		b.WriteString(fmt.Sprintf("a=label:%s\r\n", label))
	}
	return b.String()
}

// BuildCombinedSiprecSDP creates a combined SIPREC SDP from session-level lines
// and two single-media blocks, ensuring SIPREC-compliant direction/label attributes.
func BuildCombinedSiprecSDP(session, mediaA, mediaB []SDPLine, labelA, labelB string) string {
	var b strings.Builder

	for _, line := range session {
		b.WriteString(line.Value + "\r\n")
	}

	writeSiprecMediaBlockWithRecvOnly(&b, mediaA, labelA)
	writeSiprecMediaBlockWithRecvOnly(&b, mediaB, labelB)

	return b.String()
}

// writeSiprecMediaBlockWithRecvOnly writes media lines, converting sendonly/sendrecv
// to recvonly (the SRS only receives) and ensuring a label and direction are present.
func writeSiprecMediaBlockWithRecvOnly(b *strings.Builder, media []SDPLine, label string) {
	hasLabel := false
	hasDirection := false

	for _, line := range media {
		if strings.HasPrefix(line.Value, "a=label:") {
			hasLabel = true
		}
		if line.Value == "a=sendrecv" || line.Value == "a=recvonly" ||
			line.Value == "a=sendonly" || line.Value == "a=inactive" {
			hasDirection = true
		}
	}

	for _, line := range media {
		if line.Type == 'a' {
			switch line.Value {
			case "a=sendrecv", "a=sendonly":
				b.WriteString("a=recvonly\r\n")
				continue
			case "a=recvonly", "a=inactive":
				b.WriteString(line.Value + "\r\n")
				continue
			}
		}
		b.WriteString(line.Value + "\r\n")
	}

	if !hasLabel && label != "" {
		b.WriteString(fmt.Sprintf("a=label:%s\r\n", label))
	}
	if !hasDirection {
		b.WriteString("a=recvonly\r\n")
	}
}
