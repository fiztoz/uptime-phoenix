package bootstrap

import (
	"testing"
)

func TestProbeKeyConfigValidation(t *testing.T) {
	// Clean environment
	t.Setenv("PROBE_HUB_ID", "")
	t.Setenv("PROBE_SECRET_KEY_FILE", "")

	// 1. Default / empty is valid
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error on default config: %v", err)
	}
	if cfg.ProbeSecretKeyFile != "" || cfg.ProbeHubID != "" {
		t.Fatalf("expected empty probe config by default")
	}

	// 2. Valid PROBE_HUB_ID
	t.Setenv("PROBE_HUB_ID", "11111111-2222-4333-8444-555555555555")
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error with valid PROBE_HUB_ID: %v", err)
	}
	if cfg.ProbeHubID != "11111111-2222-4333-8444-555555555555" {
		t.Fatalf("expected ProbeHubID to be set")
	}

	// 3. Invalid PROBE_HUB_ID
	t.Setenv("PROBE_HUB_ID", "not-a-uuid")
	_, err = LoadConfig()
	if err == nil {
		t.Fatalf("expected error on invalid PROBE_HUB_ID")
	}

	t.Setenv("PROBE_HUB_ID", "")
}
