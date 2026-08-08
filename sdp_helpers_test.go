package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Shared Test SDP Fixtures
// =============================================================================

const testSiprecSDP = `v=0
o=root 151827427 151827427 IN IP4 172.18.170.75
s=Twilio Media Gateway
c=IN IP4 168.86.139.0
t=0 0
m=audio 17588 RTP/AVP 0 8 101
a=rtpmap:0 PCMU/8000
a=rtpmap:8 PCMA/8000
a=rtpmap:101 telephone-event/8000
a=fmtp:101 0-16
a=ptime:20
a=maxptime:20
a=sendonly
a=label:inbound
m=audio 11248 RTP/AVP 0 8 101
a=rtpmap:0 PCMU/8000
a=rtpmap:8 PCMA/8000
a=rtpmap:101 telephone-event/8000
a=fmtp:101 0-16
a=ptime:20
a=maxptime:20
a=sendonly
a=label:outbound`

const testSingleMediaSDP = `v=0
o=- 151827427 151827429 IN IP4 161.115.181.250
s=LiveKit
c=IN IP4 161.115.181.250
t=0 0
m=audio 51134 RTP/AVP 0 101
a=rtpmap:0 PCMU/8000
a=rtpmap:101 telephone-event/8000
a=fmtp:101 0-16
a=ptime:20
a=recvonly
a=label:inbound`

const testSingleMediaSDPOutbound = `v=0
o=- 151827427 151827429 IN IP4 161.115.181.250
s=LiveKit
c=IN IP4 161.115.181.250
t=0 0
m=audio 58181 RTP/AVP 0 101
a=rtpmap:0 PCMU/8000
a=rtpmap:101 telephone-event/8000
a=fmtp:101 0-16
a=ptime:20
a=recvonly
a=label:outbound`

const testMultipartBody = `------=_Part_15296_823292916.1768489452115
Content-Type: application/sdp

v=0
o=root 151827427 151827427 IN IP4 172.18.170.75
s=Twilio Media Gateway
c=IN IP4 168.86.139.0
t=0 0
m=audio 17588 RTP/AVP 0 8 101
a=rtpmap:0 PCMU/8000
a=sendonly
a=label:inbound
m=audio 11248 RTP/AVP 0 8 101
a=rtpmap:0 PCMU/8000
a=sendonly
a=label:outbound
------=_Part_15296_823292916.1768489452115
Content-Type: application/rs-metadata+xml
Content-Disposition: recording-session

<?xml version="1.0" encoding="UTF-8"?>
<recording xmlns='urn:ietf:params:xml:ns:recording:1'>
    <datamode>complete</datamode>
</recording>
------=_Part_15296_823292916.1768489452115--`

// =============================================================================
// SDP Parsing Tests
// =============================================================================

func TestParseSiprecSDP(t *testing.T) {
	session, mediaBlocks, err := ParseSiprecSDP(testSiprecSDP)
	require.NoError(t, err)

	assert.NotEmpty(t, session)
	assert.Len(t, mediaBlocks, 2)

	assert.Equal(t, "inbound", ExtractSiprecMediaLabel(mediaBlocks[0]))
	assert.Equal(t, "outbound", ExtractSiprecMediaLabel(mediaBlocks[1]))
}

func TestParseSiprecSDP_SingleMedia(t *testing.T) {
	session, mediaBlocks, err := ParseSiprecSDP(testSingleMediaSDP)
	require.NoError(t, err)

	assert.NotEmpty(t, session)
	assert.Len(t, mediaBlocks, 1)
	assert.Equal(t, "inbound", ExtractSiprecMediaLabel(mediaBlocks[0]))
}

func TestParseSiprecSDP_WindowsLineEndings(t *testing.T) {
	sdpWithCRLF := strings.ReplaceAll(testSiprecSDP, "\n", "\r\n")

	session, mediaBlocks, err := ParseSiprecSDP(sdpWithCRLF)
	require.NoError(t, err)

	assert.NotEmpty(t, session)
	assert.Len(t, mediaBlocks, 2)
}

func TestParseSiprecSDP_EmptyLines(t *testing.T) {
	sdpWithEmptyLines := "\n\n" + testSiprecSDP + "\n\n"

	session, mediaBlocks, err := ParseSiprecSDP(sdpWithEmptyLines)
	require.NoError(t, err)

	assert.NotEmpty(t, session)
	assert.Len(t, mediaBlocks, 2)
}

func TestParseSiprecSDP_GroupDUP(t *testing.T) {
	sdpWithGroupDUP := "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\na=group:DUP 1 2\r\ns=test\r\nt=0 0\r\nm=audio 5000 RTP/AVP 0\r\n"

	session, _, err := ParseSiprecSDP(sdpWithGroupDUP)
	require.NoError(t, err)

	for _, line := range session {
		assert.NotContains(t, line.Value, "group:DUP")
	}
}

// =============================================================================
// Label / Port / Payload Type Tests
// =============================================================================

func TestExtractSiprecMediaLabel_NoLabel(t *testing.T) {
	media := []SDPLine{
		{Type: 'm', Value: "m=audio 5000 RTP/AVP 0"},
		{Type: 'a', Value: "a=rtpmap:0 PCMU/8000"},
	}
	assert.Empty(t, ExtractSiprecMediaLabel(media))
}

func TestExtractSiprecMediaPort(t *testing.T) {
	_, mediaBlocks, err := ParseSiprecSDP(testSiprecSDP)
	require.NoError(t, err)
	require.Len(t, mediaBlocks, 2)

	port1, err := ExtractSiprecMediaPort(mediaBlocks[0])
	require.NoError(t, err)
	assert.Equal(t, 17588, port1)

	port2, err := ExtractSiprecMediaPort(mediaBlocks[1])
	require.NoError(t, err)
	assert.Equal(t, 11248, port2)
}

func TestExtractPCMUPayloadType(t *testing.T) {
	_, mediaBlocks, err := ParseSiprecSDP(testSiprecSDP)
	require.NoError(t, err)
	require.Len(t, mediaBlocks, 2)

	assert.Equal(t, uint8(0), extractPCMUPayloadType(mediaBlocks[0]))
}

func TestExtractPCMUPayloadType_NonStandard(t *testing.T) {
	media := []SDPLine{
		{Type: 'm', Value: "m=audio 5000 RTP/AVP 96 101"},
		{Type: 'a', Value: "a=rtpmap:96 PCMU/8000"},
		{Type: 'a', Value: "a=rtpmap:101 telephone-event/8000"},
	}
	assert.Equal(t, uint8(96), extractPCMUPayloadType(media))
}

func TestExtractPCMUPayloadType_DefaultsToZero(t *testing.T) {
	media := []SDPLine{
		{Type: 'm', Value: "m=audio 5000 RTP/AVP 8"},
		{Type: 'a', Value: "a=rtpmap:8 PCMA/8000"},
	}
	assert.Equal(t, uint8(0), extractPCMUPayloadType(media))
}

// =============================================================================
// SDP Building Tests
// =============================================================================

func TestBuildLegAnswerSDP(t *testing.T) {
	sdp := BuildLegAnswerSDP("10.0.0.5", 12000, 0, "inbound")

	assert.Contains(t, sdp, "v=0")
	assert.Contains(t, sdp, "c=IN IP4 10.0.0.5")
	assert.Contains(t, sdp, "m=audio 12000 RTP/AVP 0")
	assert.Contains(t, sdp, "a=rtpmap:0 PCMU/8000")
	assert.Contains(t, sdp, "a=recvonly")
	assert.Contains(t, sdp, "a=label:inbound")

	// The built answer must parse back into exactly one media block.
	_, mediaBlocks, err := ParseSiprecSDP(sdp)
	require.NoError(t, err)
	require.Len(t, mediaBlocks, 1)
	port, err := ExtractSiprecMediaPort(mediaBlocks[0])
	require.NoError(t, err)
	assert.Equal(t, 12000, port)
}

func TestBuildCombinedSiprecSDP(t *testing.T) {
	session, mediaBlocks, err := ParseSiprecSDP(testSiprecSDP)
	require.NoError(t, err)
	require.Len(t, mediaBlocks, 2)

	combined := BuildCombinedSiprecSDP(session, mediaBlocks[0], mediaBlocks[1], "inbound", "outbound")

	assert.Contains(t, combined, "a=label:inbound")
	assert.Contains(t, combined, "a=label:outbound")
	assert.Contains(t, combined, "a=recvonly")
	assert.NotContains(t, combined, "a=sendonly")
}

// =============================================================================
// Answer Combining Tests
// =============================================================================

func TestCombineSiprecAnswerSDPs(t *testing.T) {
	combined, err := CombineSiprecAnswerSDPs(testSiprecSDP, testSingleMediaSDP, testSingleMediaSDPOutbound)
	require.NoError(t, err)

	assert.Contains(t, combined, "a=label:inbound")
	assert.Contains(t, combined, "a=label:outbound")
	assert.Contains(t, combined, "a=recvonly")
	assert.Contains(t, combined, "m=audio 51134")
	assert.Contains(t, combined, "m=audio 58181")
}

func TestCombineSiprecAnswerSDPs_FromBuiltLegs(t *testing.T) {
	answerA := BuildLegAnswerSDP("10.0.0.5", 12000, 0, "inbound")
	answerB := BuildLegAnswerSDP("10.0.0.5", 12002, 0, "outbound")

	combined, err := CombineSiprecAnswerSDPs(testSiprecSDP, answerA, answerB)
	require.NoError(t, err)

	assert.Contains(t, combined, "m=audio 12000")
	assert.Contains(t, combined, "m=audio 12002")
	assert.Contains(t, combined, "a=label:inbound")
	assert.Contains(t, combined, "a=label:outbound")
}

func TestCombineSiprecAnswerSDPs_BadInput(t *testing.T) {
	// Answer A has two media sections, which is invalid for a single leg.
	_, err := CombineSiprecAnswerSDPs(testSiprecSDP, testSiprecSDP, testSingleMediaSDPOutbound)
	require.Error(t, err)
}

// =============================================================================
// Leg Matching Tests (re-INVITE handling)
// =============================================================================

func TestMatchLegsToMediaBlocks_MatchesByLabel(t *testing.T) {
	legs := []*rtpRecorder{
		closedRecorder("inbound", "/tmp/inbound.ulaw"),
		closedRecorder("outbound", "/tmp/outbound.ulaw"),
	}
	_, mediaBlocks, err := ParseSiprecSDP(testSiprecSDP)
	require.NoError(t, err)
	require.Len(t, mediaBlocks, 2)

	matched, err := matchLegsToMediaBlocks(legs, mediaBlocks, []string{"inbound", "outbound"})
	require.NoError(t, err)
	require.Len(t, matched, 2)
	assert.Equal(t, "inbound", matched[0].label)
	assert.Equal(t, "outbound", matched[1].label)
}

func TestMatchLegsToMediaBlocks_MatchesReorderedLabels(t *testing.T) {
	// The re-INVITE lists outbound before inbound; matching must still find
	// the correct leg for each position by label, not by index.
	reordered := `v=0
o=root 1 1 IN IP4 172.18.170.75
s=Twilio Media Gateway
c=IN IP4 168.86.139.0
t=0 0
m=audio 11248 RTP/AVP 0
a=rtpmap:0 PCMU/8000
a=label:outbound
m=audio 17588 RTP/AVP 0
a=rtpmap:0 PCMU/8000
a=label:inbound`

	legs := []*rtpRecorder{
		closedRecorder("inbound", "/tmp/inbound.ulaw"),
		closedRecorder("outbound", "/tmp/outbound.ulaw"),
	}
	_, mediaBlocks, err := ParseSiprecSDP(reordered)
	require.NoError(t, err)

	matched, err := matchLegsToMediaBlocks(legs, mediaBlocks, []string{"inbound", "outbound"})
	require.NoError(t, err)
	require.Len(t, matched, 2)
	assert.Equal(t, "outbound", matched[0].label)
	assert.Equal(t, "inbound", matched[1].label)
}

func TestMatchLegsToMediaBlocks_FallsBackToDefaultLabels(t *testing.T) {
	// Media blocks with no a=label: fall back to defaultLabels by position,
	// matching the original INVITE's leg-creation behavior.
	noLabels := `v=0
o=- 1 1 IN IP4 10.0.0.1
s=test
c=IN IP4 10.0.0.1
t=0 0
m=audio 5000 RTP/AVP 0
a=rtpmap:0 PCMU/8000
m=audio 5002 RTP/AVP 0
a=rtpmap:0 PCMU/8000`

	legs := []*rtpRecorder{
		closedRecorder("inbound", "/tmp/inbound.ulaw"),
		closedRecorder("outbound", "/tmp/outbound.ulaw"),
	}
	_, mediaBlocks, err := ParseSiprecSDP(noLabels)
	require.NoError(t, err)

	matched, err := matchLegsToMediaBlocks(legs, mediaBlocks, []string{"inbound", "outbound"})
	require.NoError(t, err)
	assert.Equal(t, "inbound", matched[0].label)
	assert.Equal(t, "outbound", matched[1].label)
}

func TestMatchLegsToMediaBlocks_ErrorsOnUnknownLabel(t *testing.T) {
	legs := []*rtpRecorder{
		closedRecorder("inbound", "/tmp/inbound.ulaw"),
		closedRecorder("outbound", "/tmp/outbound.ulaw"),
	}
	_, mediaBlocks, err := ParseSiprecSDP(testSiprecSDP) // labels: inbound, outbound
	require.NoError(t, err)

	// A leg set that doesn't include "outbound" must fail closed rather
	// than silently reusing some other leg's socket.
	_, err = matchLegsToMediaBlocks(legs[:1], mediaBlocks, []string{"inbound", "outbound"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outbound")
}
