package config

import (
	"testing"
	"time"
)

// baseConfig returns a minimal valid config YAML with the given indexer knobs
// spliced in, so polling-knob validation can be exercised end-to-end via Load.
func pollingConfigYAML(indexerExtra string) string {
	return `
network: mainnet
rpc: wss://example.com
database:
  backend: memory
indexer:
  start_block: 1
  batch_size: 100
` + indexerExtra + `
contracts:
  - name: C
    address: "0x1"
    events:
      - name: E
        table:
          type: log
`
}

func TestValidate_PollingKnobsValid(t *testing.T) {
	path := writeTestConfig(t, pollingConfigYAML(`  tip_poll_interval: 5s
  catchup_poll_interval: 200ms
  max_concurrent_catchup: 8`))

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Indexer.TipPollInterval != "5s" {
		t.Errorf("TipPollInterval = %q, want 5s", cfg.Indexer.TipPollInterval)
	}
	if cfg.Indexer.CatchupPollInterval != "200ms" {
		t.Errorf("CatchupPollInterval = %q, want 200ms", cfg.Indexer.CatchupPollInterval)
	}
	if cfg.Indexer.MaxConcurrentCatchup != 8 {
		t.Errorf("MaxConcurrentCatchup = %d, want 8", cfg.Indexer.MaxConcurrentCatchup)
	}
	// Sanity: the strings parse to the expected durations.
	if d, _ := time.ParseDuration(cfg.Indexer.TipPollInterval); d != 5*time.Second {
		t.Errorf("tip interval parsed to %v", d)
	}
}

func TestValidate_PollingKnobsOmittedOK(t *testing.T) {
	path := writeTestConfig(t, pollingConfigYAML(""))
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() with no polling knobs errored: %v", err)
	}
	if cfg.Indexer.TipPollInterval != "" || cfg.Indexer.CatchupPollInterval != "" || cfg.Indexer.MaxConcurrentCatchup != 0 {
		t.Errorf("omitted knobs should be zero-valued, got %+v", cfg.Indexer)
	}
}

func TestValidate_PollingKnobsInvalid(t *testing.T) {
	tests := []struct {
		name    string
		indexer string
	}{
		{"bad_tip_duration", "  tip_poll_interval: notaduration"},
		{"tip_too_small", "  tip_poll_interval: 50ms"},
		{"bad_catchup_duration", "  catchup_poll_interval: soon"},
		{"catchup_too_small", "  catchup_poll_interval: 5ms"},
		{"negative_concurrency", "  max_concurrent_catchup: -1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTestConfig(t, pollingConfigYAML(tt.indexer))
			if _, err := Load(path); err == nil {
				t.Fatalf("expected validation error for %s", tt.name)
			}
		})
	}
}
