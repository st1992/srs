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
	rec, err := newRTPRecorder(srvConn, dir, "callX", "inbound", pcmuPT, testLogger())
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

	rec, err := newRTPRecorder(srvConn, dir, "call/with:weird@chars", "in bound", 0, testLogger())
	require.NoError(t, err)

	// Start the read loop so Close() can unblock cleanly on done.
	go rec.run()
	defer rec.Close()

	assert.Contains(t, rec.Path(), "call_with_weird_chars_in_bound.ulaw")
}

func TestPortAllocator_DistinctPortsInRange(t *testing.T) {
	alloc := newPortAllocator("127.0.0.1", 41000, 41010)

	conn1, port1, err := alloc.open()
	require.NoError(t, err)
	defer conn1.Close()

	conn2, port2, err := alloc.open()
	require.NoError(t, err)
	defer conn2.Close()

	assert.NotEqual(t, port1, port2)
	assert.GreaterOrEqual(t, port1, 41000)
	assert.LessOrEqual(t, port1, 41010)
	assert.GreaterOrEqual(t, port2, 41000)
	assert.LessOrEqual(t, port2, 41010)
}

func TestPortAllocator_Exhausted(t *testing.T) {
	// Single-port range: first open succeeds, second fails.
	alloc := newPortAllocator("127.0.0.1", 41020, 41020)

	conn1, _, err := alloc.open()
	require.NoError(t, err)
	defer conn1.Close()

	_, _, err = alloc.open()
	require.Error(t, err)
}

func TestSanitizeFileComponent(t *testing.T) {
	assert.Equal(t, "a_b_c", sanitizeFileComponent("a/b:c"))
	assert.Equal(t, "unknown", sanitizeFileComponent(""))
	assert.Equal(t, "normal", sanitizeFileComponent("normal"))
}
