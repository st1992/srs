package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	dialogflow "cloud.google.com/go/dialogflow/apiv2beta1"
	"cloud.google.com/go/dialogflow/apiv2beta1/dialogflowpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

// AgentAssistClient starts a Google Agent Assist (Dialogflow) conversation
// for a call, returning per-leg sinks that stream RTP audio to it.
type AgentAssistClient interface {
	Start(ctx context.Context, req AgentAssistStartRequest) (*agentAssistRun, error)
	Close() error
}

// AgentAssistStartRequest describes a call whose legs should be streamed to
// Agent Assist. Labels are the recording legs' labels (e.g.
// "inbound"/"outbound"); a sink is created for each.
type AgentAssistStartRequest struct {
	CallID        string
	Metadata      map[string]any
	Labels        []string
	OnStreamError func(error)
}

// disabledAgentAssistClient is used when Agent Assist isn't configured
// (missing project/conversation profile ID); Start always fails so the API
// returns a clear error rather than silently doing nothing.
type disabledAgentAssistClient struct {
	reason string
}

func (c disabledAgentAssistClient) Start(context.Context, AgentAssistStartRequest) (*agentAssistRun, error) {
	return nil, fmt.Errorf("agent assist is disabled: %s", c.reason)
}

func (c disabledAgentAssistClient) Close() error { return nil }

// googleAgentAssistClient streams call audio to a Dialogflow conversation
// profile via the ES Participants BidiStreamingAnalyzeContent API.
type googleAgentAssistClient struct {
	cfg           *Config
	log           *slog.Logger
	conversations *dialogflow.ConversationsClient
	participants  *dialogflow.ParticipantsClient
}

// normalizeAgentAssistLocation trims and lower-cases a configured
// agent_assist_location_id, defaulting an empty value to "global". Dialogflow
// region IDs are always lower-case, and the same value feeds both the
// projects/P/locations/L resource name and the service endpoint, so there is
// exactly one normalization path.
func normalizeAgentAssistLocation(location string) string {
	normalized := strings.ToLower(strings.TrimSpace(location))
	if normalized == "" {
		return "global"
	}
	return normalized
}

// dialogflowEndpoint returns the Dialogflow gRPC endpoint serving a location.
// Regional conversation profiles are only reachable through their region's
// endpoint: the global host doesn't serve them, so a regional resource name
// sent to dialogflow.googleapis.com fails with NOT_FOUND/INVALID_ARGUMENT.
//
// The ":443" suffix is load-bearing. google.golang.org/api merges a
// user-supplied endpoint into the client's default by replacing the default's
// entire "host:port", so a bare hostname here would yield an endpoint with no
// port at all.
//
// There's deliberately no allow-list of known regions: Google adds them over
// time, and an unknown location just derives a host that fails DNS on the first
// call -- which the endpoint logged at startup makes easy to spot.
func dialogflowEndpoint(location string) string {
	normalized := normalizeAgentAssistLocation(location)
	if normalized == "global" {
		return "dialogflow.googleapis.com:443"
	}
	return fmt.Sprintf("%s-dialogflow.googleapis.com:443", normalized)
}

// NewAgentAssistClient builds a Dialogflow-backed AgentAssistClient, reusing
// cfg.GCPCredentialsFile (the same credentials used for GCS) or Application
// Default Credentials if unset. Returns a disabledAgentAssistClient (not an
// error) if Agent Assist isn't configured.
func NewAgentAssistClient(ctx context.Context, cfg *Config, log *slog.Logger) (AgentAssistClient, error) {
	if cfg.AgentAssistProjectID == "" || cfg.AgentAssistConversationProfileID == "" {
		return disabledAgentAssistClient{reason: "agent_assist_project_id and agent_assist_conversation_profile_id are required"}, nil
	}

	location := normalizeAgentAssistLocation(cfg.AgentAssistLocationID)
	endpoint := dialogflowEndpoint(location)

	log = log.With("component", "agent_assist")
	// Logged before the clients are built so a construction failure still says
	// which endpoint was attempted; the clients dial lazily, so a typo'd region
	// otherwise stays invisible until the first /v1/agent-assist/start.
	log.Info("agent assist dialogflow endpoint resolved", "location", location, "endpoint", endpoint)

	opts := []option.ClientOption{option.WithEndpoint(endpoint)}
	if cfg.GCPCredentialsFile != "" {
		opts = append(opts, option.WithCredentialsFile(cfg.GCPCredentialsFile))
	}

	conversations, err := dialogflow.NewConversationsClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("create conversations client: %w", err)
	}
	participants, err := dialogflow.NewParticipantsClient(ctx, opts...)
	if err != nil {
		_ = conversations.Close()
		return nil, fmt.Errorf("create participants client: %w", err)
	}

	return &googleAgentAssistClient{
		cfg:           cfg,
		log:           log,
		conversations: conversations,
		participants:  participants,
	}, nil
}

func (c *googleAgentAssistClient) Close() error {
	var err error
	if c.participants != nil {
		err = errors.Join(err, c.participants.Close())
	}
	if c.conversations != nil {
		err = errors.Join(err, c.conversations.Close())
	}
	return err
}

func (c *googleAgentAssistClient) Start(ctx context.Context, req AgentAssistStartRequest) (*agentAssistRun, error) {
	parent := fmt.Sprintf("projects/%s/locations/%s", c.cfg.AgentAssistProjectID, c.location())
	profile := fmt.Sprintf("%s/conversationProfiles/%s", parent, c.cfg.AgentAssistConversationProfileID)

	conv, err := c.conversations.CreateConversation(ctx, &dialogflowpb.CreateConversationRequest{
		Parent: parent,
		Conversation: &dialogflowpb.Conversation{
			ConversationProfile: profile,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create agent assist conversation: %w", err)
	}

	params, err := metadataStruct(req.Metadata)
	if err != nil {
		return nil, fmt.Errorf("convert agent assist metadata: %w", err)
	}

	sinks := make(map[string]rtpSink, len(req.Labels))
	var created []*agentAssistSink
	cleanup := func() {
		for _, sink := range created {
			_ = sink.Close()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = c.conversations.CompleteConversation(ctx, &dialogflowpb.CompleteConversationRequest{Name: conv.Name})
	}

	for _, label := range req.Labels {
		participant, err := c.participants.CreateParticipant(ctx, &dialogflowpb.CreateParticipantRequest{
			Parent: conv.Name,
			Participant: &dialogflowpb.Participant{
				Role: c.roleForLabel(label),
			},
		})
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("create participant for %s: %w", label, err)
		}

		// Each bidi stream must outlive this Start call: streams stay open
		// for the life of the call (until StopAgentAssist/finalizeSession
		// closes the sink), potentially long after the caller's ctx is gone
		// -- e.g. the HTTP handler's r.Context(), which net/http cancels the
		// instant ServeHTTP returns. Opening one on ctx would tear it down
		// moments after /v1/agent-assist/start responds 200 OK, surfacing
		// as "context canceled" on Recv followed by EOF on the next Send.
		// context.Background() decouples a stream's lifetime from the
		// request that started it; it's torn down via CloseSend when the
		// sink rotates off it or is Close()'d instead. The sink calls open
		// again for each rotation, reusing the same participant.
		open := func() (bidiAnalyzeStream, error) {
			stream, err := c.participants.BidiStreamingAnalyzeContent(context.Background())
			if err != nil {
				return nil, fmt.Errorf("open bidi stream for %s: %w", label, err)
			}
			if err := stream.Send(c.configRequest(participant.Name, params)); err != nil {
				_ = stream.CloseSend()
				return nil, fmt.Errorf("send bidi config for %s: %w", label, err)
			}
			return stream, nil
		}
		stream, err := open()
		if err != nil {
			cleanup()
			return nil, err
		}

		sink := newAgentAssistSink(agentAssistSinkParams{
			label:          label,
			conversationID: conversationIDFromName(conv.Name),
			queuePackets:   c.cfg.AgentAssistSendQueuePackets,
			sampleRate:     c.cfg.AgentAssistSampleRateHertz,
			policy:         rotationPolicyFromConfig(c.cfg),
			onError:        req.OnStreamError,
			log:            c.log.With("sipCallID", req.CallID, "label", label, "conversation", conv.Name),
		}, stream, open)
		created = append(created, sink)
		sinks[label] = sink
		sink.start()
	}

	return &agentAssistRun{
		ConversationID: conversationIDFromName(conv.Name),
		Sinks:          sinks,
		Complete: func(ctx context.Context) error {
			if ctx == nil {
				ctx = context.Background()
			}
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			_, err := c.conversations.CompleteConversation(ctx, &dialogflowpb.CompleteConversationRequest{Name: conv.Name})
			return err
		},
	}, nil
}

// location is the region the conversation and its participants are created in.
// It shares normalizeAgentAssistLocation with the endpoint derived in
// NewAgentAssistClient so the resource name and the host it's sent to can't
// drift apart.
func (c *googleAgentAssistClient) location() string {
	return normalizeAgentAssistLocation(c.cfg.AgentAssistLocationID)
}

func (c *googleAgentAssistClient) roleForLabel(label string) dialogflowpb.Participant_Role {
	normalized := strings.ToLower(strings.TrimSpace(label))
	for _, candidate := range c.cfg.AgentAssistEndUserLabels {
		if strings.ToLower(strings.TrimSpace(candidate)) == normalized {
			return dialogflowpb.Participant_END_USER
		}
	}
	return dialogflowpb.Participant_HUMAN_AGENT
}

func (c *googleAgentAssistClient) configRequest(participant string, params *structpb.Struct) *dialogflowpb.BidiStreamingAnalyzeContentRequest {
	return &dialogflowpb.BidiStreamingAnalyzeContentRequest{
		Request: &dialogflowpb.BidiStreamingAnalyzeContentRequest_Config_{
			Config: &dialogflowpb.BidiStreamingAnalyzeContentRequest_Config{
				Participant: participant,
				Config: &dialogflowpb.BidiStreamingAnalyzeContentRequest_Config_VoiceSessionConfig_{
					VoiceSessionConfig: &dialogflowpb.BidiStreamingAnalyzeContentRequest_Config_VoiceSessionConfig{
						InputAudioEncoding:          dialogflowpb.AudioEncoding_AUDIO_ENCODING_MULAW,
						InputAudioSampleRateHertz:   int32(c.cfg.AgentAssistSampleRateHertz),
						OutputAudioEncoding:         dialogflowpb.OutputAudioEncoding_OUTPUT_AUDIO_ENCODING_MULAW,
						OutputAudioSampleRateHertz:  int32(c.cfg.AgentAssistSampleRateHertz),
						EnableCxProactiveProcessing: true,
						EnableStreamingSynthesize:   false,
					},
				},
				InitialVirtualAgentParameters: params,
			},
		},
	}
}

func metadataStruct(metadata map[string]any) (*structpb.Struct, error) {
	if len(metadata) == 0 {
		return nil, nil
	}
	return structpb.NewStruct(metadata)
}

// bidiAnalyzeStream is the subset of the Dialogflow bidi-streaming client
// agentAssistSink needs; narrowed for testability.
type bidiAnalyzeStream interface {
	Send(*dialogflowpb.BidiStreamingAnalyzeContentRequest) error
	Recv() (*dialogflowpb.BidiStreamingAnalyzeContentResponse, error)
	CloseSend() error
}

// dialogflowStreamAudioLimitSeconds is Dialogflow's cap on input audio per
// BidiStreamingAnalyzeContent stream; past it the stream fails with
// OutOfRange "More than 14 minutes of input audio".
const dialogflowStreamAudioLimitSeconds = 840

const (
	// maxStreamRetries is how many times in a row a failed stream is
	// reopened before the sink gives up and the call falls back to recording.
	maxStreamRetries = 2
	// streamRetryResetAudio is how much audio a stream must carry before
	// the retry budget is restored.
	streamRetryResetAudio = 60 * time.Second
	// rotateRetryInterval is how long to wait before trying again after a
	// proactive rotation failed to open the new stream.
	rotateRetryInterval = 2 * time.Second
)

// streamRetryBackoff is the wait before each reopen attempt, indexed by
// how many retries have already been used.
var streamRetryBackoff = [maxStreamRetries]time.Duration{250 * time.Millisecond, time.Second}

const (
	rotateReasonFinal      = "utterance_final"
	rotateReasonIdle       = "recognizer_idle"
	rotateReasonHardLimit  = "hard_limit"
	rotateReasonErrorRetry = "error_retry"
)

// rotationPolicy decides when a stream has carried enough audio to be
// swapped for a fresh one before Dialogflow's per-stream limit. Durations
// are audio time, except idle which is wall time since the recognizer last
// reported speech.
type rotationPolicy struct {
	rotateAfter time.Duration
	hardLimit   time.Duration
	idle        time.Duration
}

func rotationPolicyFromConfig(cfg *Config) rotationPolicy {
	return rotationPolicy{
		rotateAfter: time.Duration(cfg.AgentAssistStreamRotateAfterSeconds) * time.Second,
		hardLimit:   time.Duration(cfg.AgentAssistStreamHardLimitSeconds) * time.Second,
		idle:        time.Duration(cfg.AgentAssistStreamIdleMillis) * time.Millisecond,
	}
}

// reason returns why a stream that has carried audio should be rotated now,
// or "" if it shouldn't yet. atFinal is true when called for a final
// transcript (an utterance boundary); idleFor is how long the recognizer has
// been quiet. Past rotateAfter the switch waits for a boundary so a sentence
// isn't split across streams; at hardLimit it happens regardless.
func (p rotationPolicy) reason(audio time.Duration, atFinal bool, idleFor time.Duration) string {
	switch {
	case audio >= p.hardLimit:
		return rotateReasonHardLimit
	case audio < p.rotateAfter:
		return ""
	case atFinal:
		return rotateReasonFinal
	case idleFor >= p.idle:
		return rotateReasonIdle
	}
	return ""
}

// isRetryableStreamErr reports whether a failed stream is worth reopening.
// io.EOF is what Send returns once the server has ended the stream; the
// real status (if any) arrives on Recv.
func isRetryableStreamErr(err error) bool {
	if errors.Is(err, io.EOF) {
		return true
	}
	switch status.Code(err) {
	case codes.OutOfRange, codes.Unavailable, codes.Internal, codes.Aborted, codes.ResourceExhausted:
		return true
	}
	return false
}

type streamEventKind int

const (
	streamEventInterim streamEventKind = iota
	streamEventFinal
	streamEventErr
)

// streamEvent is what a stream's recvLoop reports to sendLoop. gen
// identifies the stream so events from a retired stream are ignored.
type streamEvent struct {
	gen  uint64
	kind streamEventKind
	err  error
}

// agentAssistSink is an rtpSink that queues each leg's RTP payloads and
// streams them to Dialogflow over a bidi AnalyzeContent stream, rotating
// onto a new stream for the same participant before the per-stream audio
// limit and reopening it on transient errors.
//
// sendLoop is the only goroutine that sends on, closes, or swaps the
// current stream; each stream's recvLoop reports back through events.
type agentAssistSink struct {
	label          string
	conversationID string
	sendQueue      chan []byte
	events         chan streamEvent
	done           chan struct{}
	onError        func(error)
	log            *slog.Logger

	open        func() (bidiAnalyzeStream, error)
	policy      rotationPolicy
	bytesPerSec int
	now         func() time.Time
	sleep       func(time.Duration)
	currentGen  atomic.Uint64

	// Owned by sendLoop.
	stream          bidiAnalyzeStream
	gen             uint64
	bytesSent       int
	softCrossedAt   time.Time
	lastSpeechAt    time.Time
	retries         int
	nextRotateAfter time.Time

	mu       sync.Mutex
	closed   bool
	failOnce sync.Once
}

type agentAssistSinkParams struct {
	label          string
	conversationID string
	queuePackets   int
	sampleRate     int
	policy         rotationPolicy
	onError        func(error)
	log            *slog.Logger
}

// newAgentAssistSink wraps an already-configured first stream; open must
// return further streams configured for the same participant.
func newAgentAssistSink(p agentAssistSinkParams, stream bidiAnalyzeStream, open func() (bidiAnalyzeStream, error)) *agentAssistSink {
	return &agentAssistSink{
		label:          p.label,
		conversationID: p.conversationID,
		sendQueue:      make(chan []byte, p.queuePackets),
		events:         make(chan streamEvent, 16),
		done:           make(chan struct{}),
		onError:        p.onError,
		log:            p.log,
		open:           open,
		policy:         p.policy,
		bytesPerSec:    p.sampleRate, // μ-law: one byte per sample
		now:            time.Now,
		sleep:          time.Sleep,
		stream:         stream,
	}
}

func (s *agentAssistSink) start() {
	s.lastSpeechAt = s.now()
	go s.sendLoop()
	go s.recvLoop(s.stream, 0)
}

func (s *agentAssistSink) WriteRTPPayload(payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errSinkClosed
	}
	audio := append([]byte(nil), payload...)
	select {
	case s.sendQueue <- audio:
		return nil
	default:
		return fmt.Errorf("agent assist send queue full for label %s", s.label)
	}
}

func (s *agentAssistSink) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.sendQueue)
	s.mu.Unlock()

	select {
	case <-s.done:
		return nil
	case <-time.After(2 * time.Second):
		return fmt.Errorf("timed out closing agent assist stream for label %s", s.label)
	}
}

func (s *agentAssistSink) Path() string { return "" }
func (s *agentAssistSink) Kind() string { return "agent_assist" }

func (s *agentAssistSink) sendLoop() {
	defer close(s.done)
	for {
		select {
		case audio, ok := <-s.sendQueue:
			if !ok {
				if err := s.stream.CloseSend(); err != nil {
					s.fail(err)
				}
				return
			}
			if !s.sendAudio(audio) {
				return
			}
		case ev := <-s.events:
			if !s.handleEvent(ev) {
				return
			}
		}
	}
}

// sendAudio sends one payload on the current stream, reopening the stream
// on a retryable error, then rotates if the stream is due. It returns false
// once the sink has failed.
func (s *agentAssistSink) sendAudio(audio []byte) bool {
	for {
		err := s.stream.Send(&dialogflowpb.BidiStreamingAnalyzeContentRequest{
			Request: &dialogflowpb.BidiStreamingAnalyzeContentRequest_Input_{
				Input: &dialogflowpb.BidiStreamingAnalyzeContentRequest_Input{
					Input: &dialogflowpb.BidiStreamingAnalyzeContentRequest_Input_Audio{
						Audio: audio,
					},
				},
			},
		})
		if err == nil {
			break
		}
		if !s.recover(err) {
			s.fail(err)
			return false
		}
	}

	s.bytesSent += len(audio)
	audioSent := s.audioSent()
	if audioSent >= streamRetryResetAudio {
		s.retries = 0
	}
	if s.softCrossedAt.IsZero() && audioSent >= s.policy.rotateAfter {
		s.softCrossedAt = s.now()
	}
	s.maybeRotate(false)
	return true
}

// handleEvent applies a recvLoop report from the current stream. It returns
// false once the sink has failed.
func (s *agentAssistSink) handleEvent(ev streamEvent) bool {
	if ev.gen != s.gen {
		return true // from a retired stream that's draining
	}
	switch ev.kind {
	case streamEventInterim:
		s.lastSpeechAt = s.now()
	case streamEventFinal:
		s.lastSpeechAt = s.now()
		s.maybeRotate(true)
	case streamEventErr:
		if !s.recover(ev.err) {
			s.fail(ev.err)
			return false
		}
	}
	return true
}

func (s *agentAssistSink) audioSent() time.Duration {
	return time.Duration(s.bytesSent) * time.Second / time.Duration(s.bytesPerSec)
}

// maybeRotate swaps to a fresh stream if the policy says it's time.
func (s *agentAssistSink) maybeRotate(atFinal bool) {
	var idleFor time.Duration
	if !s.softCrossedAt.IsZero() {
		since := s.lastSpeechAt
		if s.softCrossedAt.After(since) {
			since = s.softCrossedAt
		}
		idleFor = s.now().Sub(since)
	}
	reason := s.policy.reason(s.audioSent(), atFinal, idleFor)
	if reason == "" || s.now().Before(s.nextRotateAfter) {
		return
	}
	if err := s.rotate(reason); err != nil {
		// Keep using the current stream and try again shortly; if it hits
		// the 14-minute limit first, the error path reopens it.
		s.nextRotateAfter = s.now().Add(rotateRetryInterval)
		s.log.Warn("agent assist stream rotation failed, keeping current stream", "reason", reason, "err", err)
	}
}

// recover reopens the stream after err, within the retry budget. It
// returns false if err isn't retryable or the budget is spent.
func (s *agentAssistSink) recover(err error) bool {
	if !isRetryableStreamErr(err) {
		return false
	}
	for s.retries < maxStreamRetries {
		s.sleep(streamRetryBackoff[s.retries])
		s.retries++
		s.log.Warn("agent assist stream error, reopening", "err", err, "attempt", s.retries)
		openErr := s.rotate(rotateReasonErrorRetry)
		if openErr == nil {
			return true
		}
		err = openErr
	}
	return false
}

// rotate opens a new stream for the same participant and makes it current.
// The old stream is half-closed; its recvLoop keeps draining until EOF so
// its last results are still logged.
func (s *agentAssistSink) rotate(reason string) error {
	started := s.now()
	next, err := s.open()
	if err != nil {
		return err
	}

	old, oldAudio := s.stream, s.audioSent()
	s.gen++
	s.currentGen.Store(s.gen)
	s.stream = next
	s.bytesSent = 0
	s.softCrossedAt = time.Time{}
	s.lastSpeechAt = s.now()
	s.nextRotateAfter = time.Time{}
	go s.recvLoop(next, s.gen)

	if err := old.CloseSend(); err != nil {
		s.log.Debug("agent assist close retired stream", "err", err)
	}
	s.log.Info("agent assist stream rotated",
		"reason", reason,
		"generation", s.gen,
		"audioSeconds", oldAudio.Seconds(),
		"openLatencyMs", s.now().Sub(started).Milliseconds(),
	)
	return nil
}

func (s *agentAssistSink) fail(err error) {
	if err == nil || errors.Is(err, errSinkClosed) {
		return
	}
	s.failOnce.Do(func() {
		s.log.Error("agent assist stream failed", "err", err)
		if s.onError != nil {
			go s.onError(err)
		}
	})
}

// report hands an event to sendLoop, or drops it once sendLoop has exited.
func (s *agentAssistSink) report(ev streamEvent) {
	select {
	case s.events <- ev:
	case <-s.done:
	}
}

func (s *agentAssistSink) recvLoop(stream bidiAnalyzeStream, gen uint64) {
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			if s.currentGen.Load() == gen {
				// Logged at Warn: sendLoop reopens the stream if it can and
				// logs at Error only if it falls back to recording.
				s.log.Warn("agent assist bidi receive error", "err", err, "generation", gen)
				s.report(streamEvent{gen: gen, kind: streamEventErr, err: err})
			} else {
				s.log.Warn("agent assist bidi receive error on retired stream", "err", err, "generation", gen)
			}
			return
		}
		if result := resp.GetRecognitionResult(); result != nil {
			s.log.Debug("agent assist recognition result", "transcript", result.GetTranscript(), "is_final", result.GetIsFinal())
			if result.GetIsFinal() || result.GetMessageType() == dialogflowpb.StreamingRecognitionResult_END_OF_SINGLE_UTTERANCE {
				s.report(streamEvent{gen: gen, kind: streamEventFinal})
			} else if result.GetMessageType() == dialogflowpb.StreamingRecognitionResult_TRANSCRIPT {
				s.report(streamEvent{gen: gen, kind: streamEventInterim})
			}
		} else if resp.GetTurnComplete() != nil {
			s.report(streamEvent{gen: gen, kind: streamEventFinal})
		}
	}
}

// conversationIDFromName extracts the conversation ID from a Dialogflow
// resource name like "projects/P/locations/L/conversations/C".
func conversationIDFromName(name string) string {
	const marker = "/conversations/"
	i := strings.LastIndex(name, marker)
	if i < 0 {
		return name
	}
	rest := name[i+len(marker):]
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		return rest[:j]
	}
	return rest
}
