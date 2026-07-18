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

type AgentAssistClient interface {
	Start(ctx context.Context, req AgentAssistStartRequest) (*agentAssistRun, error)
	Close() error
}

type AgentAssistStartRequest struct {
	CallID        string
	Metadata      map[string]any
	Labels        []string
	OnStreamError func(error)
}

type disabledAgentAssistClient struct {
	reason string
}

func (c disabledAgentAssistClient) Start(context.Context, AgentAssistStartRequest) (*agentAssistRun, error) {
	return nil, fmt.Errorf("agent assist is disabled: %s", c.reason)
}

func (c disabledAgentAssistClient) Close() error { return nil }

type googleAgentAssistClient struct {
	cfg           *Config
	log           *slog.Logger
	conversations *dialogflow.ConversationsClient
	participants  *dialogflow.ParticipantsClient

	// streamCtx backs the long-lived bidi audio streams, which must outlive
	// the HTTP request that starts them. It is canceled only on Close.
	streamCtx    context.Context
	streamCancel context.CancelFunc
}

func NewAgentAssistClient(ctx context.Context, cfg *Config, log *slog.Logger) (AgentAssistClient, error) {
	if cfg.AgentAssistProjectID == "" || cfg.AgentAssistConversationProfileID == "" {
		return disabledAgentAssistClient{reason: "agent_assist_project_id and agent_assist_conversation_profile_id are required"}, nil
	}

	var opts []option.ClientOption
	if cfg.GCPCredentialsFile != "" {
		opts = append(opts, option.WithCredentialsFile(cfg.GCPCredentialsFile))
	}
	if cfg.AgentAssistLocationID != "" && cfg.AgentAssistLocationID != "global" {
		opts = append(opts, option.WithEndpoint(fmt.Sprintf("%s-dialogflow.googleapis.com:443", cfg.AgentAssistLocationID)))
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

	streamCtx, streamCancel := context.WithCancel(context.Background())
	return &googleAgentAssistClient{
		cfg:           cfg,
		log:           log.With("component", "agent_assist"),
		conversations: conversations,
		participants:  participants,
		streamCtx:     streamCtx,
		streamCancel:  streamCancel,
	}, nil
}

func (c *googleAgentAssistClient) Close() error {
	c.streamCancel()
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

		stream, err := c.participants.BidiStreamingAnalyzeContent(c.streamCtx)
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

func (c *googleAgentAssistClient) location() string {
	if c.cfg.AgentAssistLocationID == "" {
		return "global"
	}
	return c.cfg.AgentAssistLocationID
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

type bidiAnalyzeStream interface {
	Send(*dialogflowpb.BidiStreamingAnalyzeContentRequest) error
	Recv() (*dialogflowpb.BidiStreamingAnalyzeContentResponse, error)
	CloseSend() error
}

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
