package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"strings"

	"github.com/livekit/sipgo/sip"
)

// =============================================================================
// SIPREC Detection
// =============================================================================

// IsSiprecInvite checks if the given SIP request is a SIPREC INVITE.
// SIPREC INVITEs are identified by the "Require: siprec" header, or by a
// "+sip.src" parameter in the Contact header (used by some implementations).
func IsSiprecInvite(req *sip.Request) bool {
	if req == nil || req.Method != sip.INVITE {
		return false
	}

	if requireHeader := req.GetHeader("Require"); requireHeader != nil {
		if strings.Contains(strings.ToLower(requireHeader.Value()), "siprec") {
			return true
		}
	}

	if contactHeader := req.Contact(); contactHeader != nil {
		if strings.Contains(contactHeader.String(), "+sip.src") {
			return true
		}
	}

	return false
}

// =============================================================================
// SDP Extraction Helpers
// =============================================================================

// ExtractSDPFromSiprecBody extracts SDP from a SIP request body.
// SIPREC bodies are often multipart/mixed containing both SDP and XML metadata.
func ExtractSDPFromSiprecBody(req *sip.Request) (string, error) {
	body := req.Body()
	if len(body) == 0 {
		return "", fmt.Errorf("request has no body")
	}

	contentType := req.ContentType()
	if contentType == nil {
		return string(body), nil
	}

	ct := contentType.Value()
	if strings.HasPrefix(ct, "application/sdp") {
		return string(body), nil
	}
	if strings.HasPrefix(ct, "multipart/") {
		return extractSDPFromMultipart(ct, body)
	}

	return string(body), nil
}

// extractSDPFromMultipart parses a multipart MIME body and extracts the SDP part.
func extractSDPFromMultipart(contentType string, body []byte) (string, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", fmt.Errorf("failed to parse Content-Type: %w", err)
	}

	if !strings.HasPrefix(mediaType, "multipart/") {
		return "", fmt.Errorf("expected multipart Content-Type, got: %s", mediaType)
	}

	boundary, ok := params["boundary"]
	if !ok {
		return "", fmt.Errorf("multipart Content-Type missing boundary parameter")
	}

	reader := multipart.NewReader(strings.NewReader(string(body)), boundary)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("failed to read multipart part: %w", err)
		}

		if strings.HasPrefix(part.Header.Get("Content-Type"), "application/sdp") {
			var buf strings.Builder
			if _, err := io.Copy(&buf, part); err != nil {
				return "", fmt.Errorf("failed to read SDP part: %w", err)
			}
			return buf.String(), nil
		}
	}

	return "", fmt.Errorf("no application/sdp part found in multipart body")
}

// ExtractSiprecMetadata extracts the rs-metadata XML from a SIPREC request body.
// The body may be either multipart/mixed (INVITE) or a bare application/rs-metadata+xml
// (BYE). Both forms are handled.
func ExtractSiprecMetadata(req *sip.Request) (string, error) {
	body := req.Body()
	if len(body) == 0 {
		return "", fmt.Errorf("request has no body")
	}

	contentType := req.ContentType()
	if contentType == nil {
		return "", fmt.Errorf("no content type header")
	}

	ct := contentType.Value()

	// BYE (and some UPDATE) messages carry the metadata directly.
	if strings.Contains(ct, "rs-metadata") || strings.Contains(ct, "recording-session") {
		return string(body), nil
	}

	if !strings.HasPrefix(ct, "multipart/") {
		return "", fmt.Errorf("not a multipart or rs-metadata body")
	}

	return extractMetadataFromMultipart(ct, body)
}

// extractMetadataFromMultipart parses a multipart MIME body and extracts the
// rs-metadata part.
func extractMetadataFromMultipart(contentType string, body []byte) (string, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", fmt.Errorf("failed to parse Content-Type: %w", err)
	}

	if !strings.HasPrefix(mediaType, "multipart/") {
		return "", fmt.Errorf("expected multipart Content-Type, got: %s", mediaType)
	}

	boundary, ok := params["boundary"]
	if !ok {
		return "", fmt.Errorf("multipart Content-Type missing boundary parameter")
	}

	reader := multipart.NewReader(strings.NewReader(string(body)), boundary)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("failed to read multipart part: %w", err)
		}

		partContentType := part.Header.Get("Content-Type")
		if strings.Contains(partContentType, "rs-metadata") ||
			strings.Contains(partContentType, "recording-session") {
			var buf strings.Builder
			if _, err := io.Copy(&buf, part); err != nil {
				return "", fmt.Errorf("failed to read metadata part: %w", err)
			}
			return buf.String(), nil
		}
	}

	return "", fmt.Errorf("no SIPREC metadata found in multipart body")
}

// =============================================================================
// SIPREC Answer Combining
// =============================================================================

// CombineSiprecAnswerSDPs combines two single-media SDP answer strings into a
// single SIPREC answer SDP. The labels from the original offer are preserved.
// Each answer's session-level c= line is injected into its media block so the
// combined SDP correctly addresses each leg's media endpoint.
func CombineSiprecAnswerSDPs(originalOfferSDP, answerSDPA, answerSDPB string) (string, error) {
	_, originalMediaBlocks, err := ParseSiprecSDP(originalOfferSDP)
	if err != nil {
		return "", fmt.Errorf("failed to parse original offer SDP: %w", err)
	}

	var labelA, labelB string
	if len(originalMediaBlocks) >= 1 {
		labelA = ExtractSiprecMediaLabel(originalMediaBlocks[0])
	}
	if len(originalMediaBlocks) >= 2 {
		labelB = ExtractSiprecMediaLabel(originalMediaBlocks[1])
	}

	sessionA, mediaBlocksA, err := ParseSiprecSDP(answerSDPA)
	if err != nil {
		return "", fmt.Errorf("failed to parse answer SDP A: %w", err)
	}

	sessionB, mediaBlocksB, err := ParseSiprecSDP(answerSDPB)
	if err != nil {
		return "", fmt.Errorf("failed to parse answer SDP B: %w", err)
	}

	if len(mediaBlocksA) != 1 {
		return "", fmt.Errorf("expected 1 media section in answer A, got %d", len(mediaBlocksA))
	}
	if len(mediaBlocksB) != 1 {
		return "", fmt.Errorf("expected 1 media section in answer B, got %d", len(mediaBlocksB))
	}

	mediaA := ensureMediaConnection(mediaBlocksA[0], sessionA)
	mediaB := ensureMediaConnection(mediaBlocksB[0], sessionB)

	return BuildCombinedSiprecSDP(sessionA, mediaA, mediaB, labelA, labelB), nil
}

// ensureMediaConnection ensures the media block has its own c= line by copying
// the session-level c= line into it (inserted right after the m= line) when missing.
func ensureMediaConnection(media, session []SDPLine) []SDPLine {
	for _, line := range media {
		if line.Type == 'c' {
			return media
		}
	}

	var connLine SDPLine
	found := false
	for _, line := range session {
		if line.Type == 'c' {
			connLine = line
			found = true
			break
		}
	}
	if !found {
		return media
	}

	result := make([]SDPLine, 0, len(media)+1)
	for _, line := range media {
		result = append(result, line)
		if line.Type == 'm' {
			result = append(result, connLine)
		}
	}
	return result
}

// CreateSiprecResponse creates a SIP 200 OK response for a SIPREC INVITE with
// the combined SDP from the two recording legs.
//
// contactHost / contactPort must be the recorder's own SIP signalling address
// (typically the pod / node IP and the port we listen on). They become the
// Contact header used by the remote party to address all in-dialog requests
// (ACK, BYE, re-INVITE). MUST NOT default to the inbound RURI: that RURI is
// the address the SBC sent the INVITE to (e.g. an upstream proxy / LB VIP),
// not us — using it as Contact causes in-dialog traffic to loop through the
// proxy or get 404'd by a recorder whose sipgo mux doesn't recognise the URI.
func CreateSiprecResponse(originalInvite *sip.Request, combinedSDP string, contactHost string, contactPort int) *sip.Response {
	resp := sip.NewResponseFromRequest(originalInvite, 200, "OK", []byte(combinedSDP))

	if toHeader := resp.To(); toHeader != nil {
		if _, exists := toHeader.Params.Get("tag"); !exists {
			toHeader.Params.Add("tag", generateSiprecTag())
		}
	}

	resp.RemoveHeader("Contact")
	contactParams := sip.NewParams()
	contactParams.Add("transport", "udp")
	resp.AppendHeader(&sip.ContactHeader{
		Address: sip.Uri{
			User: originalInvite.Recipient.User,
			Host: contactHost,
			Port: contactPort,
		},
		Params: contactParams,
	})

	resp.RemoveHeader("Content-Type")
	ct := sip.ContentTypeHeader("application/sdp")
	resp.AppendHeader(&ct)

	resp.RemoveHeader("Allow")
	resp.AppendHeader(sip.NewHeader("Allow", "INVITE, ACK, BYE, CANCEL, OPTIONS"))

	resp.RemoveHeader("Supported")
	resp.AppendHeader(sip.NewHeader("Supported", "siprec"))

	return resp
}

// =============================================================================
// SIP Identifier Generation
// =============================================================================

// generateSiprecTag generates a random tag for SIP headers.
func generateSiprecTag() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "tag-fallback"
	}
	return hex.EncodeToString(b)
}
