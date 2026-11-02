package main

import (
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/dialogflow/apiv2beta1/dialogflowpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Test sinks run at 8000 bytes/s of audio with 160-byte (20ms) packets, so
// rotateAfter (1s) is 50 packets and hardLimit (2s) is 100.
const (
	testSampleRate  = 8000
	testPacketBytes = 160
	packetsPerSec   = testSampleRate / testPacketBytes
)

var testPolicy = rotationPolicy{rotateAfter: time.Second, hardLimit: 2 * time.Second, idle: 800 * time.Millisecond}

// fakeBidiStream stands in for a Dialogflow bidi stream. Recv returns
// whatever the test pushes, then io.EOF (or closeErr) once CloseSend is
// called.
type fakeBidiStream struct {
	recv     chan recvItem
	closed   chan struct{}
	closeErr error // returned by Recv after CloseSend instead of io.EOF

	mu         sync.Mutex
	sends      []*dialogflowpb.BidiStreamingAnalyzeContentRequest
	closeSends int
}

type recvItem struct {
	resp *dialogflowpb.BidiStreamingAnalyzeContentResponse
	err  error
}

func newFakeBidiStream() *fakeBidiStream {
	return &fakeBidiStream{recv: make(chan recvItem, 16), closed: make(chan struct{})}
}

func (f *fakeBidiStream) Send(req *dialogflowpb.BidiStreamingAnalyzeContentRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, req)
	return nil
}

func (f *fakeBidiStream) Recv() (*dialogflowpb.BidiStreamingAnalyzeContentResponse, error) {
	select {
	case item := <-f.recv:
		return item.resp, item.err
	case <-f.closed:
		if f.closeErr != nil {
			return nil, f.closeErr
		}
		return nil, io.EOF
	}
}

func (f *fakeBidiStream) CloseSend() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeSends++
	if f.closeSends == 1 {
		close(f.closed)
	}
	return nil
}

func (f *fakeBidiStream) closeSendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeSends
}

// audioSends returns the audio payloads sent, skipping the config request.
func (f *fakeBidiStream) audioSends() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]byte
	for _, req := range f.sends {
		if audio := req.GetInput().GetAudio(); audio != nil {
			out = append(out, audio)
		}
	}
	return out
}

func (f *fakeBidiStream) firstSendIsConfig() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sends) > 0 && f.sends[0].GetConfig() != nil
}

func (f *fakeBidiStream) push(resp *dialogflowpb.BidiStreamingAnalyzeContentResponse) {
	f.recv <- recvItem{resp: resp}
}

func (f *fakeBidiStream) pushErr(err error) { f.recv <- recvItem{err: err} }

// fakeOpener hands out fakeBidiStreams, each starting with a config
// request the way googleAgentAssistClient's open closure does.
type fakeOpener struct {
	mu        sync.Mutex
	streams   []*fakeBidiStream
	attempts  int
	failNext  int           // fail this many upcoming opens
	block     chan struct{} // if set, open waits for it to be closed
	onCreated func(*fakeBidiStream)
}

func (o *fakeOpener) open() (bidiAnalyzeStream, error) {
	o.mu.Lock()
	block := o.block
	o.mu.Unlock()
	if block != nil {
		<-block
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	o.attempts++
	if o.failNext > 0 {
		o.failNext--
		return nil, status.Error(codes.Unavailable, "open failed")
	}
	s := newFakeBidiStream()
	_ = s.Send(&dialogflowpb.BidiStreamingAnalyzeContentRequest{
		Request: &dialogflowpb.BidiStreamingAnalyzeContentRequest_Config_{Config: &dialogflowpb.BidiStreamingAnalyzeContentRequest_Config{}},
	})
	if o.onCreated != nil {
		o.onCreated(s)
	}
	o.streams = append(o.streams, s)
	return s, nil
}

func (o *fakeOpener) opened() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.streams)
}

func (o *fakeOpener) stream(i int) *fakeBidiStream {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.streams[i]
}

func (o *fakeOpener) allStreams() []*fakeBidiStream {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]*fakeBidiStream(nil), o.streams...)
}

// fakeClock is a manually advanced clock for idle detection.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type sinkHarness struct {
	sink    *agentAssistSink
	opener  *fakeOpener
	clock   *fakeClock
	logs    *syncBuffer
	onError atomic.Int32
	seq     uint16
}

func newSinkHarness(t *testing.T, opener *fakeOpener) *sinkHarness {
	t.Helper()
	return newSinkHarnessWithPolicy(t, opener, testPolicy)
}

func newSinkHarnessWithPolicy(t *testing.T, opener *fakeOpener, policy rotationPolicy) *sinkHarness {
	t.Helper()
	if opener == nil {
		opener = &fakeOpener{}
	}
	h := &sinkHarness{opener: opener, clock: &fakeClock{t: time.Unix(0, 0)}, logs: &syncBuffer{}}
	first, err := opener.open()
	require.NoError(t, err)
	h.sink = newAgentAssistSink(agentAssistSinkParams{
		label:        "inbound",
		queuePackets: 4000,
		sampleRate:   testSampleRate,
		policy:       policy,
		onError:      func(error) { h.onError.Add(1) },
		log:          slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}, first, opener.open)
	h.sink.now = h.clock.now
	h.sink.sleep = func(time.Duration) {}
	h.sink.start()
	t.Cleanup(func() { _ = h.sink.Close() })
	return h
}

// write queues n numbered packets.
func (h *sinkHarness) write(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		p := make([]byte, testPacketBytes)
		binary.BigEndian.PutUint16(p, h.seq)
		h.seq++
		require.NoError(t, h.sink.WriteRTPPayload(p))
	}
}

// waitAudio waits until stream i has received n audio packets.
func (h *sinkHarness) waitAudio(t *testing.T, i, n int) {
	t.Helper()
	require.Eventually(t, func() bool { return len(h.opener.stream(i).audioSends()) >= n }, time.Second, time.Millisecond)
}

func (h *sinkHarness) waitLog(t *testing.T, substr string) {
	t.Helper()
	require.Eventually(t, func() bool { return strings.Contains(h.logs.String(), substr) }, time.Second, time.Millisecond, "log %q", substr)
}

func (h *sinkHarness) waitOpened(t *testing.T, n int) {
	t.Helper()
	require.Eventually(t, func() bool { return h.opener.opened() == n }, time.Second, time.Millisecond)
}

func interim() *dialogflowpb.BidiStreamingAnalyzeContentResponse {
	return &dialogflowpb.BidiStreamingAnalyzeContentResponse{
		Response: &dialogflowpb.BidiStreamingAnalyzeContentResponse_RecognitionResult{
			RecognitionResult: &dialogflowpb.StreamingRecognitionResult{
				MessageType: dialogflowpb.StreamingRecognitionResult_TRANSCRIPT,
				Transcript:  "hello",
			},
		},
	}
}

func final() *dialogflowpb.BidiStreamingAnalyzeContentResponse {
	return &dialogflowpb.BidiStreamingAnalyzeContentResponse{
		Response: &dialogflowpb.BidiStreamingAnalyzeContentResponse_RecognitionResult{
			RecognitionResult: &dialogflowpb.StreamingRecognitionResult{
				MessageType: dialogflowpb.StreamingRecognitionResult_TRANSCRIPT,
				Transcript:  "hello there",
				IsFinal:     true,
			},
		},
	}
}

func turnComplete() *dialogflowpb.BidiStreamingAnalyzeContentResponse {
	return &dialogflowpb.BidiStreamingAnalyzeContentResponse{
		Response: &dialogflowpb.BidiStreamingAnalyzeContentResponse_TurnComplete_{
			TurnComplete: &dialogflowpb.BidiStreamingAnalyzeContentResponse_TurnComplete{},
		},
	}
}

func TestRotationPolicy(t *testing.T) {
	tests := []struct {
		name    string
		audio   time.Duration
		atFinal bool
		idleFor time.Duration
		want    string
	}{
		{name: "below rotate-after, final", audio: 999 * time.Millisecond, atFinal: true, idleFor: time.Hour, want: ""},
		{name: "past rotate-after, speaking", audio: 1500 * time.Millisecond, idleFor: 100 * time.Millisecond, want: ""},
		{name: "past rotate-after, final", audio: time.Second, atFinal: true, want: rotateReasonFinal},
		{name: "past rotate-after, idle", audio: 1500 * time.Millisecond, idleFor: 800 * time.Millisecond, want: rotateReasonIdle},
		{name: "hard limit mid-utterance", audio: 2 * time.Second, want: rotateReasonHardLimit},
		{name: "hard limit beats final", audio: 3 * time.Second, atFinal: true, want: rotateReasonHardLimit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, testPolicy.reason(tt.audio, tt.atFinal, tt.idleFor))
		})
	}
}

func TestRotationPolicyFromConfigDefaults(t *testing.T) {
	cfg := DefaultConfig()
	p := rotationPolicyFromConfig(&cfg)
	assert.Equal(t, 13*time.Minute, p.rotateAfter)
	assert.Equal(t, 13*time.Minute+45*time.Second, p.hardLimit)
	assert.Equal(t, 800*time.Millisecond, p.idle)
	assert.Less(t, p.hardLimit, dialogflowStreamAudioLimitSeconds*time.Second)
}

func TestIsRetryableStreamErr(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{io.EOF, true},
		{status.Error(codes.OutOfRange, "More than 14 minutes of input audio."), true},
		{status.Error(codes.Unavailable, ""), true},
		{status.Error(codes.Internal, ""), true},
		{status.Error(codes.Aborted, ""), true},
		{status.Error(codes.ResourceExhausted, ""), true},
		{status.Error(codes.InvalidArgument, ""), false},
		{status.Error(codes.PermissionDenied, ""), false},
		{status.Error(codes.NotFound, ""), false},
		{status.Error(codes.Canceled, ""), false},
		{errors.New("plain"), false},
	}
	for _, tt := range tests {
		t.Run(tt.err.Error(), func(t *testing.T) {
			assert.Equal(t, tt.want, isRetryableStreamErr(tt.err))
		})
	}
}

func TestSink_NoRotationBeforeRotateAfter(t *testing.T) {
	h := newSinkHarness(t, nil)
	h.write(t, packetsPerSec-1)
	h.waitAudio(t, 0, packetsPerSec-1)
	h.opener.stream(0).push(final())

	assert.Never(t, func() bool { return h.opener.opened() > 1 }, 100*time.Millisecond, 5*time.Millisecond)
}

func TestSink_WaitsForUtteranceBoundary(t *testing.T) {
	h := newSinkHarness(t, nil)
	h.write(t, packetsPerSec+10) // past rotate-after, clock frozen so never idle
	h.waitAudio(t, 0, packetsPerSec+10)
	h.opener.stream(0).push(interim())
	h.opener.stream(0).push(interim())
	assert.Never(t, func() bool { return h.opener.opened() > 1 }, 100*time.Millisecond, 5*time.Millisecond)

	h.opener.stream(0).push(final())
	h.waitOpened(t, 2)
	h.waitLog(t, "reason="+rotateReasonFinal)
}

func TestSink_RotatesOnTurnComplete(t *testing.T) {
	h := newSinkHarness(t, nil)
	h.write(t, packetsPerSec)
	h.waitAudio(t, 0, packetsPerSec)
	h.opener.stream(0).push(turnComplete())
	h.waitOpened(t, 2)
}

func TestSink_RotatesWhenRecognizerIdle(t *testing.T) {
	h := newSinkHarness(t, nil)
	h.write(t, packetsPerSec)
	h.waitAudio(t, 0, packetsPerSec)
	h.clock.advance(799 * time.Millisecond)
	h.write(t, 1)
	h.waitAudio(t, 0, packetsPerSec+1)
	assert.Equal(t, 1, h.opener.opened())

	h.clock.advance(time.Millisecond)
	h.write(t, 1)
	h.waitOpened(t, 2)
	h.waitLog(t, "reason="+rotateReasonIdle)
}

func TestSink_InterimResultsDelayIdleRotation(t *testing.T) {
	h := newSinkHarness(t, nil)
	h.write(t, packetsPerSec)
	h.waitAudio(t, 0, packetsPerSec)
	h.clock.advance(500 * time.Millisecond)
	h.opener.stream(0).push(interim())
	// Give sendLoop the interim before more audio arrives.
	time.Sleep(20 * time.Millisecond)
	h.clock.advance(500 * time.Millisecond) // 1s past rotate-after, but only 500ms since speech
	h.write(t, 1)
	assert.Never(t, func() bool { return h.opener.opened() > 1 }, 100*time.Millisecond, 5*time.Millisecond)
}

func TestSink_HardLimitForcesRotation(t *testing.T) {
	h := newSinkHarness(t, nil)
	h.write(t, 2*packetsPerSec-1)
	h.waitAudio(t, 0, 2*packetsPerSec-1)
	assert.Equal(t, 1, h.opener.opened())

	h.write(t, 1)
	h.waitOpened(t, 2)
	assert.Len(t, h.opener.stream(0).audioSends(), 2*packetsPerSec)
	h.waitLog(t, "reason="+rotateReasonHardLimit)
}

func TestSink_NoAudioLostAcrossRotations(t *testing.T) {
	h := newSinkHarness(t, nil)
	const total = 3*2*packetsPerSec + 50 // three hard-limit rotations
	h.write(t, total)
	require.NoError(t, h.sink.Close())

	streams := h.opener.allStreams()
	require.Len(t, streams, 4)
	var seqs []uint16
	for i, s := range streams {
		assert.True(t, s.firstSendIsConfig(), "stream %d must start with config", i)
		assert.Equal(t, 1, s.closeSendCount(), "stream %d CloseSend", i)
		for _, p := range s.audioSends() {
			seqs = append(seqs, binary.BigEndian.Uint16(p))
		}
	}
	require.Len(t, seqs, total)
	for i, seq := range seqs {
		require.Equal(t, uint16(i), seq, "packet %d out of order or missing", i)
	}
	assert.Zero(t, h.onError.Load())
}

func TestSink_RetiredStreamErrorDoesNotFail(t *testing.T) {
	opener := &fakeOpener{}
	opener.onCreated = func(s *fakeBidiStream) {
		s.closeErr = status.Error(codes.OutOfRange, "More than 14 minutes of input audio.")
	}
	h := newSinkHarness(t, opener)
	h.write(t, 2*packetsPerSec)
	h.waitOpened(t, 2)

	require.Eventually(t, func() bool { return h.opener.stream(0).closeSendCount() == 1 }, time.Second, time.Millisecond)
	assert.Never(t, func() bool { return h.onError.Load() > 0 }, 100*time.Millisecond, 5*time.Millisecond)
	assert.Equal(t, 2, h.opener.opened())
	h.waitLog(t, "receive error on retired stream")
}

func TestSink_OpenFailureKeepsCurrentStream(t *testing.T) {
	h := newSinkHarness(t, nil)
	h.opener.mu.Lock()
	h.opener.failNext = 1
	h.opener.mu.Unlock()

	h.write(t, 2*packetsPerSec+5) // hard limit hit; open fails
	h.waitAudio(t, 0, 2*packetsPerSec+5)
	assert.Equal(t, 1, h.opener.opened())

	h.clock.advance(rotateRetryInterval)
	h.write(t, 1)
	h.waitOpened(t, 2)
	assert.Len(t, h.opener.stream(0).audioSends(), 2*packetsPerSec+6)
	assert.Zero(t, h.onError.Load())
}

func TestSink_RetriesRetryableErrorThenRecovers(t *testing.T) {
	h := newSinkHarness(t, nil)
	h.write(t, 10)
	h.waitAudio(t, 0, 10)
	h.opener.stream(0).pushErr(status.Error(codes.OutOfRange, "More than 14 minutes of input audio."))
	h.waitOpened(t, 2)

	h.write(t, 10)
	h.waitAudio(t, 1, 10)
	assert.Zero(t, h.onError.Load())
	h.waitLog(t, "reason="+rotateReasonErrorRetry)
}

func TestSink_FallsBackAfterRetriesExhausted(t *testing.T) {
	h := newSinkHarness(t, nil)
	for i := 0; i <= maxStreamRetries; i++ {
		h.waitOpened(t, i+1)
		h.opener.stream(i).pushErr(status.Error(codes.Unavailable, "unavailable"))
	}
	require.Eventually(t, func() bool { return h.onError.Load() == 1 }, time.Second, time.Millisecond)
	assert.Never(t, func() bool { return h.onError.Load() > 1 || h.opener.opened() > maxStreamRetries+1 }, 100*time.Millisecond, 5*time.Millisecond)
}

func TestSink_RetryBudgetResetsAfterHealthyAudio(t *testing.T) {
	h := newSinkHarnessWithPolicy(t, nil, rotationPolicy{rotateAfter: 2 * time.Minute, hardLimit: 3 * time.Minute, idle: time.Hour})
	for i := 0; i < maxStreamRetries; i++ {
		h.waitOpened(t, i+1)
		h.opener.stream(i).pushErr(status.Error(codes.Unavailable, "unavailable"))
	}
	h.waitOpened(t, maxStreamRetries+1)

	n := int(streamRetryResetAudio/time.Second) * packetsPerSec
	h.write(t, n)
	h.waitAudio(t, maxStreamRetries, n)
	h.opener.stream(maxStreamRetries).pushErr(status.Error(codes.Unavailable, "unavailable"))
	h.waitOpened(t, maxStreamRetries+2)
	assert.Zero(t, h.onError.Load())
}

func TestSink_NonRetryableErrorFailsImmediately(t *testing.T) {
	h := newSinkHarness(t, nil)
	h.opener.stream(0).pushErr(status.Error(codes.PermissionDenied, "denied"))
	require.Eventually(t, func() bool { return h.onError.Load() == 1 }, time.Second, time.Millisecond)
	assert.Equal(t, 1, h.opener.opened())
	h.waitLog(t, "agent assist stream failed")
}

func TestSink_CloseDuringRotation(t *testing.T) {
	h := newSinkHarness(t, nil)
	block := make(chan struct{})
	h.opener.mu.Lock()
	h.opener.block = block
	h.opener.mu.Unlock()

	h.write(t, 2*packetsPerSec) // hard limit: open blocks
	closed := make(chan error, 1)
	go func() { closed <- h.sink.Close() }()
	time.Sleep(20 * time.Millisecond)
	close(block)

	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return")
	}
	for i, s := range h.opener.allStreams() {
		assert.Equal(t, 1, s.closeSendCount(), "stream %d CloseSend", i)
	}
	assert.ErrorIs(t, h.sink.WriteRTPPayload([]byte{0}), errSinkClosed)
}
