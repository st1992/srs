package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"google.golang.org/api/option"
)

// Event types published to Pub/Sub.
const (
	EventCallStart = "call_start"
	EventCallEnd   = "call_end"
)

// SiprecEvent is the JSON payload published to the GCP Pub/Sub topic.
type SiprecEvent struct {
	Event          string            `json:"event"`
	Timestamp      string            `json:"timestamp"`
	SIPCallID      string            `json:"sip_call_id"`
	From           string            `json:"from,omitempty"`
	To             string            `json:"to,omitempty"`
	SourceIP       string            `json:"source_ip,omitempty"`
	RecordingFiles map[string]string `json:"recording_files,omitempty"`
	SiprecMetadata *SiprecMetadata   `json:"siprec_metadata,omitempty"`
	// ByeMetadata carries the rs-metadata from the BYE body (call_end events only).
	// It typically contains disassociate-time for participants.
	ByeMetadata    *SiprecMetadata   `json:"bye_metadata,omitempty"`
	SIPHeaders     map[string]string `json:"sip_headers,omitempty"`
	Reason         string            `json:"reason,omitempty"`
}

// Publisher abstracts event publishing so the server can run without Pub/Sub configured.
type Publisher interface {
	Publish(ctx context.Context, ev *SiprecEvent)
	Close()
}

// noopPublisher is used when Pub/Sub is not configured; it discards events.
type noopPublisher struct {
	log *slog.Logger
}

func (p *noopPublisher) Publish(_ context.Context, ev *SiprecEvent) {
	p.log.Debug("pubsub not configured; dropping event", "event", ev.Event, "sipCallID", ev.SIPCallID)
}

func (p *noopPublisher) Close() {}

// pubsubPublisher publishes events to a GCP Cloud Pub/Sub topic.
type pubsubPublisher struct {
	client    *pubsub.Client
	publisher *pubsub.Publisher
	log       *slog.Logger
}

// NewPublisher creates a Pub/Sub publisher. If projectID or topicID is empty, a
// no-op publisher is returned so the recorder still functions without Pub/Sub.
func NewPublisher(ctx context.Context, cfg *Config, log *slog.Logger) (Publisher, error) {
	if cfg.PubSubProjectID == "" || cfg.PubSubTopicID == "" {
		log.Warn("Pub/Sub not configured (missing project or topic); events will be dropped")
		return &noopPublisher{log: log}, nil
	}

	var opts []option.ClientOption
	if cfg.GCPCredentialsFile != "" {
		opts = append(opts, option.WithCredentialsFile(cfg.GCPCredentialsFile))
	}

	client, err := pubsub.NewClient(ctx, cfg.PubSubProjectID, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create Pub/Sub client: %w", err)
	}

	return &pubsubPublisher{
		client:    client,
		publisher: client.Publisher(cfg.PubSubTopicID),
		log:       log.With("pubsubTopic", cfg.PubSubTopicID),
	}, nil
}

// Publish marshals and asynchronously publishes the event. Failures are logged
// but never block or fail the call.
func (p *pubsubPublisher) Publish(ctx context.Context, ev *SiprecEvent) {
	if ev.Timestamp == "" {
		ev.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}

	data, err := json.Marshal(ev)
	if err != nil {
		p.log.Error("failed to marshal SIPREC event", "err", err, "event", ev.Event)
		return
	}

	result := p.publisher.Publish(ctx, &pubsub.Message{
		Data: data,
		Attributes: map[string]string{
			"event":       ev.Event,
			"sip_call_id": ev.SIPCallID,
		},
	})

	go func(ev *SiprecEvent) {
		id, err := result.Get(context.Background())
		if err != nil {
			p.log.Error("failed to publish SIPREC event", "err", err, "event", ev.Event, "sipCallID", ev.SIPCallID)
			return
		}
		p.log.Info("published SIPREC event", "event", ev.Event, "sipCallID", ev.SIPCallID, "messageID", id)
	}(ev)
}

// Close flushes buffered messages and releases client resources.
func (p *pubsubPublisher) Close() {
	if p.publisher != nil {
		p.publisher.Stop()
	}
	if p.client != nil {
		_ = p.client.Close()
	}
}
