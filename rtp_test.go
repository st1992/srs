package main

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// syncBuffer is a concurrency-safe bytes.Buffer wrapper for tests that read
// log output from a different goroutine than the one writing it (e.g. the
// no-media watchdog timer, which fires on its own goroutine).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

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
	rec, err := newRTPRecorder(srvConn, dir, "callX", "15551234567", "15559876543", time.Now().UnixMilli(), "inbound", pcmuPT, 0, testLogger())
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
	// callID with @, /, : and - all sanitized to _; DNIS/ANI are phone numbers.
	rec, err := newRTPRecorder(srvConn, dir, "call/with:weird@chars", "8777953602", "4694733291", startMs, "1", 0, 0, testLogger())
	require.NoError(t, err)

	// Start the read loop so Close() can unblock cleanly on done.
	go rec.run()
	defer rec.Close()

	// Fields are separated by '-'; within-component special chars become '_'.
	assert.Contains(t, rec.Path(), "call_with_weird_chars")
	assert.Contains(t, rec.Path(), "8777953602")
	assert.Contains(t, rec.Path(), "4694733291")
	assert.Contains(t, rec.Path(), "1750000000000")
	assert.Contains(t, rec.Path(), "-1.ulaw")
	// Verify the full stem format: {callID}-{dnis}-{ani}-{ts}-{label}.ulaw
	assert.Contains(t, rec.Path(), "call_with_weird_chars-8777953602-4694733291-1750000000000-1.ulaw")
}

func TestRTPRecorder_FileNaming_DashesInCallID(t *testing.T) {
	dir := t.TempDir()

	srvConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	require.NoError(t, err)

	const startMs = int64(1750000000000)
	// UUID-style callID with '-'; they are sanitized to '_' so they cannot
	// be confused with the '-' field separator.
	rec, err := newRTPRecorder(srvConn, dir, "a1b2c3d4-e5f6@sip.example.com", "8005551234", "2125559876", startMs, "2", 0, 0, testLogger())
	require.NoError(t, err)

	go rec.run()
	defer rec.Close()

	assert.Contains(t, rec.Path(), "a1b2c3d4_e5f6_sip.example.com-8005551234-2125559876-1750000000000-2.ulaw")
}

func TestRTPRecorder_NoMediaWatchdog_FiresWhenNoPacketsReceived(t *testing.T) {
	dir := t.TempDir()

	srvConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	require.NoError(t, err)

	buf := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(buf, nil))

	rec, err := newRTPRecorder(srvConn, dir, "callWatchdog", "1", "2", time.Now().UnixMilli(), "inbound", 0, 1, log)
	require.NoError(t, err)

	go rec.run()
	defer rec.Close()

	// No packets are ever sent; the 1s watchdog should fire.
	require.Eventually(t, func() bool {
		return strings.Contains(buf.String(), eventNoRTPReceived)
	}, 3*time.Second, 50*time.Millisecond, "expected no_rtp_received warning to be logged")
}

func TestRTPRecorder_NoMediaWatchdog_SilentWhenPacketsReceived(t *testing.T) {
	dir := t.TempDir()

	srvConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	require.NoError(t, err)
	serverAddr := srvConn.LocalAddr().(*net.UDPAddr)

	buf := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(buf, nil))

	const pcmuPT = uint8(0)
	rec, err := newRTPRecorder(srvConn, dir, "callWatchdog2", "1", "2", time.Now().UnixMilli(), "inbound", pcmuPT, 1, log)
	require.NoError(t, err)

	go rec.run()

	client, err := net.DialUDP("udp", nil, serverAddr)
	require.NoError(t, err)
	defer client.Close()
	_, err = client.Write(rtpPacket(pcmuPT, 1, []byte("abc")))
	require.NoError(t, err)

	time.Sleep(1500 * time.Millisecond)
	rec.Close()

	assert.NotContains(t, buf.String(), eventNoRTPReceived)
}

func TestRTPRecorder_ReplaceSink(t *testing.T) {
	dir := t.TempDir()

	srvConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	require.NoError(t, err)
	serverAddr := srvConn.LocalAddr().(*net.UDPAddr)

	const pcmuPT = uint8(0)
	rec, err := newRTPRecorder(srvConn, dir, "callSplit", "1", "2", time.Now().UnixMilli(), "inbound", pcmuPT, 0, testLogger())
	require.NoError(t, err)

	go rec.run()

	client, err := net.DialUDP("udp", nil, serverAddr)
	require.NoError(t, err)
	defer client.Close()

	_, err = client.Write(rtpPacket(pcmuPT, 1, []byte("first")))
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	oldPath := rec.Path()
	newSink, err := newFileSink(dir, "callSplit", "1", "2", time.Now().UnixMilli(), "inbound")
	require.NoError(t, err)

	old := rec.ReplaceSink(newSink)
	require.NotNil(t, old)
	require.NoError(t, old.Close())

	assert.NotEqual(t, oldPath, rec.Path())
	assert.Equal(t, newSink.Path(), rec.Path())

	_, err = client.Write(rtpPacket(pcmuPT, 2, []byte("second")))
	require.NoError(t, err)
	time.Sleep(200 * time.Millisecond)
	rec.Close()

	oldData, err := os.ReadFile(oldPath)
	require.NoError(t, err)
	assert.Equal(t, "first", string(oldData))

	newData, err := os.ReadFile(newSink.Path())
	require.NoError(t, err)
	assert.Equal(t, "second", string(newData))
}

func TestSanitizeFileComponent(t *testing.T) {
	assert.Equal(t, "a_b_c", sanitizeFileComponent("a/b:c"))
	assert.Equal(t, "unknown", sanitizeFileComponent(""))
	assert.Equal(t, "normal", sanitizeFileComponent("normal"))
	// '-' must be replaced so it cannot clash with the '-' field separator.
	assert.Equal(t, "a_b_c", sanitizeFileComponent("a-b-c"))
	// '@' replacement (Call-ID host separator).
	assert.Equal(t, "1234_10.0.0.1", sanitizeFileComponent("1234@10.0.0.1"))
}
