package main

import (
	"io"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func rtpPacket(pt uint8, seq uint16, payload []byte) []byte {
	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    pt,
			SequenceNumber: seq,
			Timestamp:      uint32(seq) * 160,
			SSRC:           0x1234,
		},
		Payload: payload,
	}
	data, err := pkt.Marshal()
	if err != nil {
		panic(err)
	}
	return data
}

func TestRTPRecorder_WritesOnlyPCMUPayload(t *testing.T) {
	dir := t.TempDir()

	srvConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	require.NoError(t, err)
	serverAddr := srvConn.LocalAddr().(*net.UDPAddr)

	const pcmuPT = uint8(0)
	rec, err := newRTPRecorder(srvConn, dir, "callX", "15551234567", "15559876543", time.Now().UnixMilli(), "inbound", pcmuPT, testLogger())
	require.NoError(t, err)

	go rec.run()

	client, err := net.DialUDP("udp", nil, serverAddr)
	require.NoError(t, err)
	defer client.Close()

	_, err = client.Write(rtpPacket(pcmuPT, 1, []byte("abc")))
	require.NoError(t, err)
	// DTMF / telephone-event packet must be skipped.
	_, err = client.Write(rtpPacket(101, 2, []byte("XYZ")))
	require.NoError(t, err)
	_, err = client.Write(rtpPacket(pcmuPT, 3, []byte("def")))
	require.NoError(t, err)

	// Give the read loop time to process before closing.
	time.Sleep(200 * time.Millisecond)
	rec.Close()

	data, err := os.ReadFile(rec.Path())
	require.NoError(t, err)
	assert.Equal(t, "abcdef", string(data))
}

func TestRTPRecorder_FileNaming(t *testing.T) {
	dir := t.TempDir()

	srvConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	require.NoError(t, err)

	const startMs = int64(1750000000000)
	rec, err := newRTPRecorder(srvConn, dir, "call/with:weird@chars", "dnis/test", "ani@test", startMs, "in bound", 0, testLogger())
	require.NoError(t, err)

	// Start the read loop so Close() can unblock cleanly on done.
	go rec.run()
	defer rec.Close()

	assert.Contains(t, rec.Path(), "call_with_weird_chars")
	assert.Contains(t, rec.Path(), "dnis_test")
	assert.Contains(t, rec.Path(), "ani_test")
	assert.Contains(t, rec.Path(), "1750000000000")
	assert.Contains(t, rec.Path(), "in_bound.ulaw")
}


func TestSanitizeFileComponent(t *testing.T) {
	assert.Equal(t, "a_b_c", sanitizeFileComponent("a/b:c"))
	assert.Equal(t, "unknown", sanitizeFileComponent(""))
	assert.Equal(t, "normal", sanitizeFileComponent("normal"))
}
