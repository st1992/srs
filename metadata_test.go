package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testRichMetadata = `<?xml version="1.0" encoding="UTF-8"?>
<recording xmlns='urn:ietf:params:xml:ns:recording:1'>
    <datamode>complete</datamode>
    <session session_id="aaaa-bbbb">
        <sipSessionID>cccc-dddd</sipSessionID>
    </session>
    <participant participant_id="p1">
        <nameID aor="sip:alice@example.com">
            <name>Alice</name>
        </nameID>
    </participant>
    <participant participant_id="p2">
        <nameID aor="sip:bob@example.com">
            <name>Bob</name>
        </nameID>
    </participant>
    <stream stream_id="s1" session_id="aaaa-bbbb">
        <label>inbound</label>
    </stream>
    <stream stream_id="s2" session_id="aaaa-bbbb">
        <label>outbound</label>
    </stream>
    <participantstreamassoc participant_id="p1">
        <send>s1</send>
        <recv>s2</recv>
    </participantstreamassoc>
</recording>`

// testSonusInviteMetadata mirrors the rs-metadata from a real Sonus/Ribbon SBC INVITE.
const testSonusInviteMetadata = `<?xml version="1.0" encoding="UTF-8"?>
<recording xmlns='urn:ietf:params:xml:ns:recording'>
    <datamode>complete</datamode>
    <group group_id="NTczMjI2ODAtNTI3Zi0xMA==">
        <associate-time>2026-06-25T04:50:41Z</associate-time>
        <callData xmlns='urn:ietf:params:xml:ns:callData'>
            <fromhdr>&lt;sip:4694733291@10.87.18.120&gt;;isup-oli=62;tag=gK0c3ab0a7</fromhdr>
            <tohdr>&lt;sip:8777953602@10.87.18.120:5060;user=phone&gt;;tag=as7df1d0cd</tohdr>
            <callid>4990206_132492727@10.87.18.17</callid>
            <gcid>4990206</gcid>
        </callData>
    </group>
    <session session_id="NWE5OGFiYmUtNTI3Zi0xMA==">
        <group-ref>NTczMjI2ODAtNTI3Zi0xMA==</group-ref>
        <start-time>2026-06-25T04:50:41Z</start-time>
    </session>
    <participant participant_id="NTczMjI2ODEtNTI3Zi0xMA==">
        <nameID aor="4694733291@10.87.18.120">
            <name xml:lang="en"> </name>
        </nameID>
    </participant>
    <participant participant_id="NTczMjI2ODItNTI3Zi0xMA==">
        <nameID aor="8777953602@10.87.18.120">
            <name xml:lang="en"> </name>
        </nameID>
    </participant>
    <stream stream_id="NTczMjI2ODQtNTI3Zi0xMA==" session_id="NWE5OGFiYmUtNTI3Zi0xMA==">
        <label>1</label>
        <associate-time>2026-06-25T04:50:41Z</associate-time>
    </stream>
    <stream stream_id="NTczMjI2ODUtNTI3Zi0xMA==" session_id="NWE5OGFiYmUtNTI3Zi0xMA==">
        <label>2</label>
        <associate-time>2026-06-25T04:50:41Z</associate-time>
    </stream>
    <sessionrecordingassoc session_id="NWE5OGFiYmUtNTI3Zi0xMA==">
        <associate-time>2026-06-25T04:50:41Z</associate-time>
    </sessionrecordingassoc>
    <participantsessionassoc participant_id="NTczMjI2ODEtNTI3Zi0xMA==" session_id="NWE5OGFiYmUtNTI3Zi0xMA==">
        <associate-time>2026-06-25T04:50:41Z</associate-time>
    </participantsessionassoc>
    <participantsessionassoc participant_id="NTczMjI2ODItNTI3Zi0xMA==" session_id="NWE5OGFiYmUtNTI3Zi0xMA==">
        <associate-time>2026-06-25T04:50:41Z</associate-time>
    </participantsessionassoc>
    <participantstreamassoc participant_id="NTczMjI2ODEtNTI3Zi0xMA==">
        <send>NTczMjI2ODQtNTI3Zi0xMA==</send>
        <recv>NTczMjI2ODUtNTI3Zi0xMA==</recv>
    </participantstreamassoc>
    <participantstreamassoc participant_id="NTczMjI2ODItNTI3Zi0xMA==">
        <send>NTczMjI2ODUtNTI3Zi0xMA==</send>
        <recv>NTczMjI2ODQtNTI3Zi0xMA==</recv>
    </participantstreamassoc>
</recording>`

// testSonusByeMetadata mirrors the rs-metadata from a real Sonus/Ribbon SBC BYE.
const testSonusByeMetadata = `<?xml version="1.0" encoding="UTF-8"?>
        <recording xmlns='urn:ietf:params:xml:ns:recording'>
        <datamode>Partial</datamode>
        <session session_id="NjUwODA1M2UtNTI4MC0xMA==">
        </session>
        <participant
        participant_id="NjFhMGYwMDEtNTI4MC0xMA=="
        session_id="NjUwODA1M2UtNTI4MC0xMA==">
        <disassociate-time>2026-06-25T04:58:15Z</disassociate-time>
        </participant>
        <participant
        participant_id="NjFhMGYwMDItNTI4MC0xMA=="
        session_id="NjUwODA1M2UtNTI4MC0xMA==">
        <disassociate-time>2026-06-25T04:58:15Z</disassociate-time>
        </participant>
        </recording>`

func TestParseSiprecMetadata_Simple(t *testing.T) {
	meta, err := ParseSiprecMetadata(`<?xml version="1.0" encoding="UTF-8"?>
<recording xmlns='urn:ietf:params:xml:ns:recording:1'>
    <datamode>complete</datamode>
</recording>`)
	require.NoError(t, err)
	assert.Equal(t, "complete", meta.DataMode)
}

func TestParseSiprecMetadata_Rich(t *testing.T) {
	meta, err := ParseSiprecMetadata(testRichMetadata)
	require.NoError(t, err)

	assert.Equal(t, "complete", meta.DataMode)

	require.Len(t, meta.Sessions, 1)
	assert.Equal(t, "aaaa-bbbb", meta.Sessions[0].SessionID)
	assert.Equal(t, "cccc-dddd", meta.Sessions[0].SIPSessionID)

	require.Len(t, meta.Participants, 2)
	require.Len(t, meta.Participants[0].NameIDs, 1)
	assert.Equal(t, "sip:alice@example.com", meta.Participants[0].NameIDs[0].AOR)
	assert.Equal(t, "Alice", meta.Participants[0].NameIDs[0].Name)

	require.Len(t, meta.Streams, 2)
	assert.Equal(t, "s1", meta.Streams[0].StreamID)
	assert.Equal(t, "inbound", meta.Streams[0].Label)

	require.Len(t, meta.ParticipantStreamAssoc, 1)
	assert.Equal(t, "p1", meta.ParticipantStreamAssoc[0].ParticipantID)
	assert.Equal(t, []string{"s1"}, meta.ParticipantStreamAssoc[0].Send)
	assert.Equal(t, []string{"s2"}, meta.ParticipantStreamAssoc[0].Recv)
}

func TestParseSiprecMetadata_SonusInvite(t *testing.T) {
	meta, err := ParseSiprecMetadata(testSonusInviteMetadata)
	require.NoError(t, err)

	assert.Equal(t, "complete", meta.DataMode)

	// Group with callData
	require.Len(t, meta.Groups, 1)
	g := meta.Groups[0]
	assert.Equal(t, "NTczMjI2ODAtNTI3Zi0xMA==", g.GroupID)
	assert.Equal(t, "2026-06-25T04:50:41Z", g.AssociateTime)
	require.NotNil(t, g.CallData)
	assert.Contains(t, g.CallData.FromHdr, "4694733291")
	assert.Contains(t, g.CallData.ToHdr, "8777953602")
	assert.Equal(t, "4990206_132492727@10.87.18.17", g.CallData.CallID)
	assert.Equal(t, "4990206", g.CallData.GCID)

	// Session with group-ref and start-time
	require.Len(t, meta.Sessions, 1)
	s := meta.Sessions[0]
	assert.Equal(t, "NWE5OGFiYmUtNTI3Zi0xMA==", s.SessionID)
	assert.Equal(t, "NTczMjI2ODAtNTI3Zi0xMA==", s.GroupRef)
	assert.Equal(t, "2026-06-25T04:50:41Z", s.StartTime)

	// Participants
	require.Len(t, meta.Participants, 2)
	assert.Equal(t, "NTczMjI2ODEtNTI3Zi0xMA==", meta.Participants[0].ParticipantID)
	assert.Equal(t, "4694733291@10.87.18.120", meta.Participants[0].NameIDs[0].AOR)
	assert.Equal(t, "NTczMjI2ODItNTI3Zi0xMA==", meta.Participants[1].ParticipantID)
	assert.Equal(t, "8777953602@10.87.18.120", meta.Participants[1].NameIDs[0].AOR)

	// Streams with associate-time
	require.Len(t, meta.Streams, 2)
	assert.Equal(t, "NTczMjI2ODQtNTI3Zi0xMA==", meta.Streams[0].StreamID)
	assert.Equal(t, "NWE5OGFiYmUtNTI3Zi0xMA==", meta.Streams[0].SessionID)
	assert.Equal(t, "1", meta.Streams[0].Label)
	assert.Equal(t, "2026-06-25T04:50:41Z", meta.Streams[0].AssociateTime)

	// SessionRecordingAssoc
	require.Len(t, meta.SessionRecordingAssoc, 1)
	assert.Equal(t, "NWE5OGFiYmUtNTI3Zi0xMA==", meta.SessionRecordingAssoc[0].SessionID)
	assert.Equal(t, "2026-06-25T04:50:41Z", meta.SessionRecordingAssoc[0].AssociateTime)

	// ParticipantSessionAssoc with associate-time
	require.Len(t, meta.ParticipantSessionAssoc, 2)
	assert.Equal(t, "NTczMjI2ODEtNTI3Zi0xMA==", meta.ParticipantSessionAssoc[0].ParticipantID)
	assert.Equal(t, "NWE5OGFiYmUtNTI3Zi0xMA==", meta.ParticipantSessionAssoc[0].SessionID)
	assert.Equal(t, "2026-06-25T04:50:41Z", meta.ParticipantSessionAssoc[0].AssociateTime)

	// ParticipantStreamAssoc
	require.Len(t, meta.ParticipantStreamAssoc, 2)
	assert.Equal(t, "NTczMjI2ODEtNTI3Zi0xMA==", meta.ParticipantStreamAssoc[0].ParticipantID)
	assert.Equal(t, []string{"NTczMjI2ODQtNTI3Zi0xMA=="}, meta.ParticipantStreamAssoc[0].Send)
	assert.Equal(t, []string{"NTczMjI2ODUtNTI3Zi0xMA=="}, meta.ParticipantStreamAssoc[0].Recv)
}

func TestParseSiprecMetadata_SonusBye(t *testing.T) {
	meta, err := ParseSiprecMetadata(testSonusByeMetadata)
	require.NoError(t, err)

	assert.Equal(t, "Partial", meta.DataMode)

	// Session present but empty
	require.Len(t, meta.Sessions, 1)
	assert.Equal(t, "NjUwODA1M2UtNTI4MC0xMA==", meta.Sessions[0].SessionID)

	// Participants with session_id attribute and disassociate-time
	require.Len(t, meta.Participants, 2)
	p0 := meta.Participants[0]
	assert.Equal(t, "NjFhMGYwMDEtNTI4MC0xMA==", p0.ParticipantID)
	assert.Equal(t, "NjUwODA1M2UtNTI4MC0xMA==", p0.SessionID)
	assert.Equal(t, "2026-06-25T04:58:15Z", p0.DisassociateTime)

	p1 := meta.Participants[1]
	assert.Equal(t, "NjFhMGYwMDItNTI4MC0xMA==", p1.ParticipantID)
	assert.Equal(t, "NjUwODA1M2UtNTI4MC0xMA==", p1.SessionID)
	assert.Equal(t, "2026-06-25T04:58:15Z", p1.DisassociateTime)
}

func TestParseSiprecMetadata_Empty(t *testing.T) {
	_, err := ParseSiprecMetadata("   ")
	require.Error(t, err)
}

func TestParseSiprecMetadata_Invalid(t *testing.T) {
	_, err := ParseSiprecMetadata("<recording><not-closed>")
	require.Error(t, err)
}
