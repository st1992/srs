package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
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
	Target        agentAssistTarget
	OnStreamError func(error)
}

// agentAssistTarget selects the conversation profile, and the region it lives
// in, for one conversation. The zero value means the configured default.
type agentAssistTarget struct {
	ProfileID string
	Location  string
}

var (
	// agentAssistRegionPattern only admits a DNS label fragment: the region is
	// interpolated into the host that GCP credentials are sent to (see
	// dialogflowEndpoint), so a caller must not be able to smuggle in a dot,
	// slash or port. 52 characters keeps "<region>-dialogflow" a legal label.
	agentAssistRegionPattern  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,50}[a-z0-9])?$`)
	agentAssistProfilePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

// parseAgentAssistTarget validates a request's optional profile and region.
// A profile only exists in its own region, so the two are used as a pair or
// not at all: if either is blank the zero target (configured default) is
// returned, with ignoredLone set when the other one was supplied.
func parseAgentAssistTarget(profileID, region string) (target agentAssistTarget, ignoredLone bool, err error) {
	profileID = strings.TrimSpace(profileID)
	region = strings.TrimSpace(region)
	if profileID == "" || region == "" {
		return agentAssistTarget{}, profileID != "" || region != "", nil
	}
	region = normalizeAgentAssistLocation(region)
	if !agentAssistProfilePattern.MatchString(profileID) {
		return agentAssistTarget{}, false, errors.New("aa_conversation_profile_id must be a bare profile ID (letters, digits, '-' or '_', at most 128 characters)")
	}
	if !agentAssistRegionPattern.MatchString(region) {
		return agentAssistTarget{}, false, errors.New("aa_region must be a region ID such as global or us-central1")
	}
	return agentAssistTarget{ProfileID: profileID, Location: region}, false, nil
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
	cfg *Config
	log *slog.Logger

	// newClients builds the clients for one location; a field so tests can
	// point them at a local server.
	newClients func(ctx context.Context, location string) (*dialogflowClients, error)

	mu      sync.Mutex
	clients map[string]*dialogflowClients // keyed by normalized location
}

// dialogflowClients are the Dialogflow clients connected to one location's
// endpoint.
type dialogflowClients struct {
	conversations *dialogflow.ConversationsClient
	participants  *dialogflow.ParticipantsClient
}

func (d *dialogflowClients) Close() error {
	return errors.Join(d.participants.Close(), d.conversations.Close())
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

	c := &googleAgentAssistClient{cfg: cfg, log: log.With("component", "agent_assist")}
	c.newClients = c.dialClients
	// The default location's clients are built up front so bad credentials
	// fail at startup; other locations' are built when first requested.
	if _, err := c.clientsFor(ctx, c.resolveTarget(agentAssistTarget{}).Location); err != nil {
		return nil, err
	}
	return c, nil
}

// clientsFor returns the clients for a normalized location, building them on
// first use. Construction doesn't connect, so holding mu across it is brief;
// a failed construction isn't cached.
func (c *googleAgentAssistClient) clientsFor(ctx context.Context, location string) (*dialogflowClients, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if clients, ok := c.clients[location]; ok {
		return clients, nil
	}
	clients, err := c.newClients(ctx, location)
	if err != nil {
		return nil, err
	}
	if c.clients == nil {
		c.clients = make(map[string]*dialogflowClients)
	}
	c.clients[location] = clients
	return clients, nil
}

func (c *googleAgentAssistClient) dialClients(ctx context.Context, location string) (*dialogflowClients, error) {
	endpoint := dialogflowEndpoint(location)
	// Logged before the clients are built so a construction failure still says
	// which endpoint was attempted; the clients dial lazily, so a typo'd region
	// otherwise stays invisible until the first /v1/agent-assist/start.
	c.log.Info("agent assist dialogflow endpoint resolved", "location", location, "endpoint", endpoint)

	opts := []option.ClientOption{option.WithEndpoint(endpoint)}
	if c.cfg.GCPCredentialsFile != "" {
		opts = append(opts, option.WithCredentialsFile(c.cfg.GCPCredentialsFile))
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
	return &dialogflowClients{conversations: conversations, participants: participants}, nil
}

func (c *googleAgentAssistClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var err error
	for _, clients := range c.clients {
		err = errors.Join(err, clients.Close())
	}
	return err
}

// agentAssistSetupTimeout bounds the unary calls that set up a conversation.
// The SDK retries Unavailable (which is what a nonexistent region's DNS
// failure looks like) for up to 60s, past the HTTP API's 30s WriteTimeout.
// Variable so tests can shrink it.
var agentAssistSetupTimeout = 20 * time.Second

func (c *googleAgentAssistClient) Start(ctx context.Context, req AgentAssistStartRequest) (*agentAssistRun, error) {
	target := c.resolveTarget(req.Target)
	parent := fmt.Sprintf("projects/%s/locations/%s", c.cfg.AgentAssistProjectID, target.Location)
	profile := fmt.Sprintf("%s/conversationProfiles/%s", parent, target.ProfileID)

	params, err := metadataStruct(req.Metadata)
	if err != nil {
		return nil, fmt.Errorf("convert agent assist metadata: %w", err)
	}

	// Everything this conversation does, up to and including rotations and
	// CompleteConversation, must go through its own region's clients. They're
	// shared and outlive this request, hence not built on ctx.
	clients, err := c.clientsFor(context.Background(), target.Location)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, agentAssistSetupTimeout)
	defer cancel()

	conv, err := clients.conversations.CreateConversation(ctx, &dialogflowpb.CreateConversationRequest{
		Parent: parent,
		Conversation: &dialogflowpb.Conversation{
			ConversationProfile: profile,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create agent assist conversation (profile %s, region %s): %w", target.ProfileID, target.Location, err)
	}
	c.log.Info("agent assist conversation created",
		"sipCallID", req.CallID,
		"conversationID", conversationIDFromName(conv.Name),
		"profileID", target.ProfileID,
		"location", target.Location,
	)

	sinks := make(map[string]rtpSink, len(req.Labels))
	var created []*agentAssistSink
	cleanup := func() {
		for _, sink := range created {
			_ = sink.Close()
		}
		if err := c.completeConversation(context.Background(), clients, conv.Name); err != nil {
			c.log.Warn("failed to complete orphaned agent assist conversation after start failure",
				"event", eventAgentAssistCompleteFailed,
				"err", err,
				"sipCallID", req.CallID,
				"conversationID", conversationIDFromName(conv.Name),
			)
		}
	}

	for _, label := range req.Labels {
		participant, err := clients.participants.CreateParticipant(ctx, &dialogflowpb.CreateParticipantRequest{
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
			stream, err := clients.participants.BidiStreamingAnalyzeContent(context.Background())
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
			return c.completeConversation(ctx, clients, conv.Name)
		},
	}, nil
}

// completeConversation sends CompleteConversation for name, retrying
// transient failures. Completing promptly is what lets Dialogflow close the
// session, merge its audio, and export it to Insights; a conversation left
// open sits until Google's 24h hard timeout.
func (c *googleAgentAssistClient) completeConversation(ctx context.Context, clients *dialogflowClients, name string) error {
	return completeConversationWithRetry(ctx, func(ctx context.Context) error {
		_, err := clients.conversations.CompleteConversation(ctx, &dialogflowpb.CompleteConversationRequest{Name: name})
		return err
	})
}

// Retry budget for CompleteConversation: 3 attempts of up to 10s each with
// 500ms then 2s between them, ~32.5s worst case. Variables so tests can
// shrink them.
var (
	completeConversationAttempts       = 3
	completeConversationAttemptTimeout = 10 * time.Second
	completeConversationBackoff        = []time.Duration{500 * time.Millisecond, 2 * time.Second}
)

// completeConversationWithRetry runs call up to completeConversationAttempts
// times, each under its own completeConversationAttemptTimeout, retrying only
// errors retryableCompleteError deems transient. It gives up as soon as ctx
// is done, so callers bound the whole budget with ctx's deadline (shutdown)
// and must not hand it a request-scoped ctx that can be cancelled early.
func completeConversationWithRetry(ctx context.Context, call func(context.Context) error) error {
	var err error
	for attempt := 0; attempt < completeConversationAttempts; attempt++ {
		if attempt > 0 {
			wait := completeConversationBackoff[len(completeConversationBackoff)-1]
			if attempt-1 < len(completeConversationBackoff) {
				wait = completeConversationBackoff[attempt-1]
			}
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return errors.Join(err, ctx.Err())
			case <-timer.C:
			}
		}
		attemptCtx, cancel := context.WithTimeout(ctx, completeConversationAttemptTimeout)
		err = call(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return errors.Join(err, ctx.Err())
		}
		if !retryableCompleteError(err) {
			return err
		}
	}
	return err
}

// retryableCompleteError reports whether a CompleteConversation failure is
// worth retrying. A per-attempt timeout surfaces as DeadlineExceeded (or a
// bare context.DeadlineExceeded) and is retried; the caller has already
// checked that the parent ctx is still alive.
func retryableCompleteError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted, codes.Aborted, codes.Internal:
		return true
	default:
		return false
	}
}

// resolveTarget fills a zero target from config. The resulting Location is
// normalized and feeds both the resource name and, via clientsFor, the host
// it's sent to, so the two can't drift apart.
func (c *googleAgentAssistClient) resolveTarget(target agentAssistTarget) agentAssistTarget {
	if target == (agentAssistTarget{}) {
		target = agentAssistTarget{
			ProfileID: c.cfg.AgentAssistConversationProfileID,
			Location:  c.cfg.AgentAssistLocationID,
		}
	}
	target.Location = normalizeAgentAssistLocation(target.Location)
	return target
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
	// streamRetryResetAge is how old a stream must get before the retry
	// budget is restored.
	streamRetryResetAge = 60 * time.Second
	// rotateRetryInterval is how long to wait before trying again after a
	// proactive rotation failed to open the new stream.
	rotateRetryInterval = 2 * time.Second
	// rotationCheckInterval is how often the rotation policy is re-checked
	// when no audio or transcript arrives to trigger it (hold, silence
	// suppression), so a stream can't age past the limit unnoticed.
	rotationCheckInterval = time.Second
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

// rotationPolicy decides when a stream is old enough to be swapped for a
// fresh one before Dialogflow's per-stream limit. rotateAfter and hardLimit
// apply to the stream's age (see agentAssistSink.streamAge); idle is wall
// time since the recognizer last reported speech.
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

// reason returns why a stream of the given age should be rotated now, or ""
// if it shouldn't yet. atFinal is true when called for a final
// transcript (an utterance boundary); idleFor is how long the recognizer has
// been quiet. Past rotateAfter the switch waits for a boundary so a sentence
// isn't split across streams; at hardLimit it happens regardless.
func (p rotationPolicy) reason(age time.Duration, atFinal bool, idleFor time.Duration) string {
	switch {
	case age >= p.hardLimit:
		return rotateReasonHardLimit
	case age < p.rotateAfter:
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

// openResult is the outcome of a stream open run in the background.
type openResult struct {
	stream bidiAnalyzeStream
	err    error
}

// agentAssistSink is an rtpSink that queues each leg's RTP payloads and
// streams them to Dialogflow over a bidi AnalyzeContent stream, rotating
// onto a new stream for the same participant before the per-stream audio
// limit and reopening it on transient errors.
//
// sendLoop is the only goroutine that sends on, closes, or swaps the
// current stream; each stream's recvLoop reports back through events, and a
// planned rotation's new stream is opened in the background and handed
// back through pending while audio keeps flowing to the current one.
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
	tick        <-chan time.Time // nil until start(), unless a test injects one
	ticker      *time.Ticker
	currentGen  atomic.Uint64

	// Owned by sendLoop.
	stream          bidiAnalyzeStream
	gen             uint64
	streamOpenedAt  time.Time
	bytesSent       int
	softCrossedAt   time.Time
	lastSpeechAt    time.Time
	retries         int
	nextRotateAfter time.Time
	pending         chan openResult // in-flight background open; nil if none
	pendingReason   string
	pendingStarted  time.Time

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
	s.streamOpenedAt = s.lastSpeechAt
	if s.tick == nil {
		s.ticker = time.NewTicker(rotationCheckInterval)
		s.tick = s.ticker.C
	}
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
		return fmt.Errorf("agent assist send queue full for label %s: %w", s.label, errSinkFull)
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
	defer s.abandonPending()
	if s.ticker != nil {
		defer s.ticker.Stop()
	}
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
		case res := <-s.pending: // nil channel (no open in flight) never fires
			s.finishPendingOpen(res)
		case <-s.tick:
			s.maybeRotate(false)
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

// streamAge is how long the current stream counts as having run. Audio is
// sent in real time, so wall time since the stream opened is normally the
// larger term: it keeps counting through hold, silence suppression and
// packet loss, when no audio is sent but Dialogflow still counts toward
// its limit. audioSent covers a backlog flushed faster than real time.
func (s *agentAssistSink) streamAge() time.Duration {
	return max(s.now().Sub(s.streamOpenedAt), s.audioSent())
}

// maybeRotate starts opening a fresh stream in the background if the
// policy says it's time; the swap happens when the open completes.
func (s *agentAssistSink) maybeRotate(atFinal bool) {
	age := s.streamAge()
	if age >= streamRetryResetAge {
		s.retries = 0
	}
	if s.softCrossedAt.IsZero() && age >= s.policy.rotateAfter {
		s.softCrossedAt = s.now()
	}
	if s.pending != nil {
		return
	}

	var idleFor time.Duration
	if !s.softCrossedAt.IsZero() {
		since := s.lastSpeechAt
		if s.softCrossedAt.After(since) {
			since = s.softCrossedAt
		}
		idleFor = s.now().Sub(since)
	}
	reason := s.policy.reason(age, atFinal, idleFor)
	if reason == "" || s.now().Before(s.nextRotateAfter) {
		return
	}

	ch := make(chan openResult, 1) // buffered so the opener never blocks
	open := s.open
	go func() {
		stream, err := open()
		ch <- openResult{stream: stream, err: err}
	}()
	s.pending, s.pendingReason, s.pendingStarted = ch, reason, s.now()
}

// finishPendingOpen swaps to a stream opened in the background, or keeps
// the current one and retries shortly if the open failed. If the current
// stream reaches the 14-minute limit first, the error path reopens it.
func (s *agentAssistSink) finishPendingOpen(res openResult) {
	s.pending = nil
	if res.err != nil {
		s.nextRotateAfter = s.now().Add(rotateRetryInterval)
		s.log.Warn("agent assist stream rotation failed, keeping current stream", "reason", s.pendingReason, "err", res.err)
		return
	}
	s.swapStream(res.stream, s.pendingReason, s.pendingStarted)
}

// abandonPending closes any stream a still-running background open
// produces after sendLoop has exited, so it isn't leaked.
func (s *agentAssistSink) abandonPending() {
	if s.pending == nil {
		return
	}
	ch := s.pending
	s.pending = nil
	go func() {
		if res := <-ch; res.stream != nil {
			_ = res.stream.CloseSend()
		}
	}()
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
		started := s.now()
		next, openErr := s.openNow()
		if openErr == nil {
			s.swapStream(next, rotateReasonErrorRetry, started)
			return true
		}
		err = openErr
	}
	return false
}

// openNow returns a new stream for the current one, which has failed. A
// background open already in flight is waited for and used if it
// succeeded; otherwise a stream is opened synchronously. Audio arriving
// meanwhile waits in sendQueue.
func (s *agentAssistSink) openNow() (bidiAnalyzeStream, error) {
	if s.pending != nil {
		res := <-s.pending
		s.pending = nil
		if res.err == nil {
			return res.stream, nil
		}
	}
	return s.open()
}

// swapStream makes next the current stream. The old stream is half-closed;
// its recvLoop keeps draining until EOF so its last results are still
// logged.
func (s *agentAssistSink) swapStream(next bidiAnalyzeStream, reason string, started time.Time) {
	old, oldAudio, oldAge := s.stream, s.audioSent(), s.now().Sub(s.streamOpenedAt)
	s.gen++
	s.currentGen.Store(s.gen)
	s.stream = next
	s.streamOpenedAt = s.now()
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
		"streamSeconds", oldAge.Seconds(),
		"openLatencyMs", s.now().Sub(started).Milliseconds(),
	)
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
