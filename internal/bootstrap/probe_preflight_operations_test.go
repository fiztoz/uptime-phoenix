package bootstrap

import (
	"strings"
	"testing"
)

func TestM8PreflightConfigDefaultAndDependency(t *testing.T) {
	t.Setenv("JWT_SECRET", testJWTSecret)
	t.Setenv("PROBE_SSH_PREFLIGHT_ENABLED", "false")
	t.Setenv("PROBES_ENABLED", "false")
	t.Setenv("PROBES_AGGREGATE_PAGING_ENABLED", "false")
	cfg, err := LoadConfig()
	if err != nil || cfg.ProbeSSHPreflightEnabled {
		t.Fatalf("default preflight configuration: %v", err)
	}
	t.Setenv("PROBE_SSH_PREFLIGHT_ENABLED", "true")
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "requires PROBES_ENABLED") {
		t.Fatalf("missing dependency accepted: %v", err)
	}
	if err := Run(Config{ProbeSSHPreflightEnabled: true}); err == nil || !strings.Contains(err.Error(), "requires PROBES_ENABLED") {
		t.Fatalf("direct Run bypassed dependency before DB startup: %v", err)
	}
}

func TestM8PreflightDisabledAndWorkersDoNotAcquire(t *testing.T) {
	for _, cfg := range []Config{{}, {ProbesEnabled: true, ProbeSSHPreflightEnabled: true, Mode: "worker", ProbeEndpointPolicyFile: "must-not-be-opened"}} {
		svc, err := newProbePreflightOperations(t.Context(), cfg, nil)
		if err != nil || svc != nil {
			t.Fatalf("disabled/worker composition loaded policy or acquired work: %v", err)
		}
	}
}
