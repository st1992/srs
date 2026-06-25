package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	content := `sip_listen_addr: "0.0.0.0:5070"
media_ip: "10.1.2.3"
rtp_port_start: 20000
rtp_port_end: 21000
recording_dir: "/tmp/recordings"
gcs_bucket: "my-recordings"
gcs_metadata_bucket: "my-metadata"
gcs_metadata_object_prefix: "calls"
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	cfg, err := LoadConfig(path)
	require.NoError(t, err)

	assert.Equal(t, "0.0.0.0:5070", cfg.SIPListenAddr)
	assert.Equal(t, "10.1.2.3", cfg.MediaIP)
	assert.Equal(t, 20000, cfg.RTPPortStart)
	assert.Equal(t, 21000, cfg.RTPPortEnd)
	assert.Equal(t, "/tmp/recordings", cfg.RecordingDir)
	assert.Equal(t, "my-recordings", cfg.GCSBucket)
	assert.Equal(t, "my-metadata", cfg.GCSMetadataBucket)
	assert.Equal(t, "calls", cfg.GCSMetadataObjectPrefix)
}

func TestLoadConfig_AppliesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	// Only override one field; everything else should use defaults.
	require.NoError(t, os.WriteFile(path, []byte("gcs_bucket: \"my-bucket\"\n"), 0o644))

	cfg, err := LoadConfig(path)
	require.NoError(t, err)

	def := DefaultConfig()
	assert.Equal(t, def.SIPListenAddr, cfg.SIPListenAddr)
	assert.Equal(t, def.RTPPortStart, cfg.RTPPortStart)
	assert.Equal(t, def.RTPPortEnd, cfg.RTPPortEnd)
	assert.Equal(t, def.RecordingDir, cfg.RecordingDir)
}

func TestLoadConfig_MissingFile(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	require.Error(t, err)
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantErr bool
	}{
		{name: "valid", mutate: func(c *Config) {}, wantErr: false},
		{name: "empty listen addr", mutate: func(c *Config) { c.SIPListenAddr = "" }, wantErr: true},
		{name: "zero start port", mutate: func(c *Config) { c.RTPPortStart = 0 }, wantErr: true},
		{name: "start after end", mutate: func(c *Config) { c.RTPPortStart, c.RTPPortEnd = 30000, 20000 }, wantErr: true},
		{name: "empty recording dir", mutate: func(c *Config) { c.RecordingDir = "" }, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tt.mutate(&cfg)
			err := cfg.Validate()
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
