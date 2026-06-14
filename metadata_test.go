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

func TestParseSiprecMetadata_Empty(t *testing.T) {
	_, err := ParseSiprecMetadata("   ")
	require.Error(t, err)
}

func TestParseSiprecMetadata_Invalid(t *testing.T) {
	_, err := ParseSiprecMetadata("<recording><not-closed>")
	require.Error(t, err)
}
