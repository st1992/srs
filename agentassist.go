package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	dialogflow "cloud.google.com/go/dialogflow/apiv2beta1"
	"cloud.google.com/go/dialogflow/apiv2beta1/dialogflowpb"
	"google.golang.org/api/option"
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

		// The bidi stream must outlive this Start call: it stays open for
		// the life of the call (until StopAgentAssist/finalizeSession closes
		// it), potentially long after the caller's ctx is gone -- e.g. the
		// HTTP handler's r.Context(), which net/http cancels the instant
		// ServeHTTP returns. Opening it on ctx would tear the stream down
		// moments after /v1/agent-assist/start responds 200 OK, surfacing
		// as "context canceled" on Recv followed by EOF on the next Send.
		// context.Background() decouples the stream's lifetime from the
		// request that started it; it's torn down via CloseSend when the
		// sink is Close()'d instead.
		stream, err := c.participants.BidiStreamingAnalyzeContent(context.Background())
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("open bidi stream for %s: %w", label, err)
		}

		if err := stream.Send(c.configRequest(participant.Name, params)); err != nil {
			_ = stream.CloseSend()
			cleanup()
			return nil, fmt.Errorf("send bidi config for %s: %w", label, err)
		}

		sink := &agentAssistSink{
			label:          label,
			conversationID: conversationIDFromName(conv.Name),
			stream:         stream,
			sendQueue:      make(chan []byte, c.cfg.AgentAssistSendQueuePackets),
			done:           make(chan struct{}),
			onError:        req.OnStreamError,
			log:            c.log.With("sipCallID", req.CallID, "label", label, "conversation", conv.Name),
		}
		created = append(created, sink)
		sinks[label] = sink
		go sink.sendLoop()
		go sink.recvLoop()
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

// agentAssistSink is an rtpSink that queues each leg's RTP payloads and
// streams them to Dialogflow over a bidi AnalyzeContent stream.
type agentAssistSink struct {
	label          string
	conversationID string
	stream         bidiAnalyzeStream
	sendQueue      chan []byte
	done           chan struct{}
	onError        func(error)
	log            *slog.Logger

	mu       sync.Mutex
	closed   bool
	failOnce sync.Once
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
	for audio := range s.sendQueue {
		err := s.stream.Send(&dialogflowpb.BidiStreamingAnalyzeContentRequest{
			Request: &dialogflowpb.BidiStreamingAnalyzeContentRequest_Input_{
				Input: &dialogflowpb.BidiStreamingAnalyzeContentRequest_Input{
					Input: &dialogflowpb.BidiStreamingAnalyzeContentRequest_Input_Audio{
						Audio: audio,
					},
				},
			},
		})
		if err != nil {
			s.fail(err)
			return
		}
	}
	if err := s.stream.CloseSend(); err != nil {
		s.fail(err)
	}
}

func (s *agentAssistSink) fail(err error) {
	if err == nil || errors.Is(err, errSinkClosed) {
		return
	}
	s.failOnce.Do(func() {
		s.log.Error("agent assist bidi send error", "err", err)
		if s.onError != nil {
			go s.onError(err)
		}
	})
}

func (s *agentAssistSink) recvLoop() {
	for {
		resp, err := s.stream.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			s.log.Error("agent assist bidi receive error", "err", err)
			return
		}
		if result := resp.GetRecognitionResult(); result != nil {
			s.log.Debug("agent assist recognition result", "transcript", result.GetTranscript(), "is_final", result.GetIsFinal())
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
