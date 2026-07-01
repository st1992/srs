package main

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config holds the configuration for the minimal SIPREC recorder.
type Config struct {
	// SIPListenAddr is the address the SIP server listens on (UDP), e.g. "0.0.0.0:5060".
	SIPListenAddr string `yaml:"sip_listen_addr"`

	// MediaIP is the IP address advertised in SDP answers for RTP media.
	// If empty, the recorder attempts to detect a non-loopback IPv4 address.
	MediaIP string `yaml:"media_ip"`

	// RTPPortStart and RTPPortEnd define the inclusive UDP port range used to
	// receive RTP for recording legs.
	RTPPortStart int `yaml:"rtp_port_start"`
	RTPPortEnd   int `yaml:"rtp_port_end"`

	// RecordingDir is the directory where .ulaw files are written.
	RecordingDir string `yaml:"recording_dir"`

	// GCPCredentialsFile is an optional path to a GCP service account JSON key.
	// If empty, Application Default Credentials are used (e.g. Workload Identity).
	GCPCredentialsFile string `yaml:"gcp_credentials_file"`

	// GCSBucket is the Google Cloud Storage bucket recordings are uploaded to.
	// If empty, uploading is disabled and recordings remain on local disk.
	GCSBucket string `yaml:"gcs_bucket"`

	// GCSObjectPrefix is an optional prefix (folder) prepended to recording object names.
	GCSObjectPrefix string `yaml:"gcs_object_prefix"`

	// GCSMetadataBucket is the GCS bucket where per-call metadata JSON files are
	// uploaded. If empty, metadata files remain on local disk only.
	GCSMetadataBucket string `yaml:"gcs_metadata_bucket"`

	// GCSMetadataObjectPrefix is an optional prefix prepended to metadata object names.
	// Defaults to "metadata".
	GCSMetadataObjectPrefix string `yaml:"gcs_metadata_object_prefix"`

	// DeleteAfterUpload removes the local .ulaw file only after a successful
	// upload to GCS. Defaults to true.
	DeleteAfterUpload bool `yaml:"delete_after_upload"`

	// UploadWorkers is the number of concurrent upload workers. Defaults to 2.
	UploadWorkers int `yaml:"upload_workers"`

	// UploadMaxRetries is the number of upload attempts per file before giving
	// up (the file is left on disk for a later sweep). Defaults to 5.
	UploadMaxRetries int `yaml:"upload_max_retries"`

	// UploadSweepIntervalSec controls how often the recording directory is
	// scanned for orphaned/failed files to (re)upload. Defaults to 60 seconds.
	UploadSweepIntervalSec int `yaml:"upload_sweep_interval_sec"`

	// ShutdownUploadTimeoutSec bounds how long shutdown waits for in-flight
	// uploads to drain. Defaults to 30 seconds.
	ShutdownUploadTimeoutSec int `yaml:"shutdown_upload_timeout_sec"`

	// HTTPListenAddr is the pod-local HTTP API listen address for operational
	// APIs such as Agent Assist start/stop.
	HTTPListenAddr string `yaml:"http_listen_addr"`

	// APIAdvertiseIP is the IP stored in Redis loc:<Call-ID>. If empty, the
	// recorder uses the same non-loopback address detection as SIP Contact.
	APIAdvertiseIP string `yaml:"api_advertise_ip"`

	// APIAuthToken optionally protects the pod-local HTTP API with a bearer token.
	APIAuthToken string `yaml:"api_auth_token"`

	// Redis settings used for call-location registration. Redis is expected to
	// be reachable on a private IP/plain TCP port with no TLS and no AUTH.
	RedisAddr              string `yaml:"redis_addr"`
	RedisDB                int    `yaml:"redis_db"`
	RedisLocatorTTLSeconds int    `yaml:"redis_locator_ttl_seconds"`

	// Agent Assist / Dialogflow configuration.
	AgentAssistProjectID             string   `yaml:"agent_assist_project_id"`
	AgentAssistLocationID            string   `yaml:"agent_assist_location_id"`
	AgentAssistConversationProfileID string   `yaml:"agent_assist_conversation_profile_id"`
	AgentAssistSampleRateHertz       int      `yaml:"agent_assist_sample_rate_hertz"`
	AgentAssistSendQueuePackets      int      `yaml:"agent_assist_send_queue_packets"`
	AgentAssistEndUserLabels         []string `yaml:"agent_assist_end_user_labels"`
	AgentAssistHumanAgentLabels      []string `yaml:"agent_assist_human_agent_labels"`
}

// DefaultConfig returns a Config populated with sensible defaults.
func DefaultConfig() Config {
	return Config{
		SIPListenAddr:               "0.0.0.0:5060",
		RTPPortStart:                10000,
		RTPPortEnd:                  11000,
		RecordingDir:                ".",
		GCSObjectPrefix:             "recordings",
		GCSMetadataObjectPrefix:     "metadata",
		DeleteAfterUpload:           true,
		UploadWorkers:               2,
		UploadMaxRetries:            5,
		UploadSweepIntervalSec:      60,
		ShutdownUploadTimeoutSec:    30,
		HTTPListenAddr:              "0.0.0.0:8080",
		RedisLocatorTTLSeconds:      3600,
		AgentAssistLocationID:       "global",
		AgentAssistSampleRateHertz:  8000,
		AgentAssistSendQueuePackets: 250,
		AgentAssistEndUserLabels:    []string{"inbound", "1"},
		AgentAssistHumanAgentLabels: []string{"outbound", "2"},
	}
}

// LoadConfig reads and parses a YAML config file, applying defaults for any
// fields not specified.
func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %q: %w", path, err)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file %q: %w", path, err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// Validate checks that the configuration is internally consistent.
func (c *Config) Validate() error {
	if c.SIPListenAddr == "" {
		return fmt.Errorf("sip_listen_addr must be set")
	}
	if c.RTPPortStart <= 0 || c.RTPPortEnd <= 0 {
		return fmt.Errorf("rtp_port_start and rtp_port_end must be positive")
	}
	if c.RTPPortStart > c.RTPPortEnd {
		return fmt.Errorf("rtp_port_start (%d) must not exceed rtp_port_end (%d)", c.RTPPortStart, c.RTPPortEnd)
	}
	if c.RecordingDir == "" {
		return fmt.Errorf("recording_dir must be set")
	}
	if c.HTTPListenAddr == "" {
		return fmt.Errorf("http_listen_addr must be set")
	}
	if c.RedisLocatorTTLSeconds <= 0 {
		return fmt.Errorf("redis_locator_ttl_seconds must be positive")
	}
	if c.AgentAssistSampleRateHertz <= 0 {
		return fmt.Errorf("agent_assist_sample_rate_hertz must be positive")
	}
	if c.AgentAssistSendQueuePackets <= 0 {
		return fmt.Errorf("agent_assist_send_queue_packets must be positive")
	}
	return nil
}
