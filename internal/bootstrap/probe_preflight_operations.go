package bootstrap

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func validateProbePreflightConfig(cfg Config) error {
	if cfg.ProbeSSHPreflightEnabled && !cfg.ProbesEnabled {
		return fmt.Errorf("PROBE_SSH_PREFLIGHT_ENABLED requires PROBES_ENABLED")
	}
	return nil
}

// newProbePreflightOperations is deliberately API-local: workers never acquire
// another process's jobs or credentials, and the default path loads no policy.
func newProbePreflightOperations(ctx context.Context, cfg Config, db *bun.DB) (*services.ProbePreflightOperationService, error) {
	if err := validateProbePreflightConfig(cfg); err != nil {
		return nil, err
	}
	if !cfg.ProbeSSHPreflightEnabled || cfg.Mode == "worker" {
		return nil, nil
	}
	policy, err := probe.LoadEndpointPolicy(cfg.ProbeEndpointPolicyFile)
	if err != nil {
		return nil, err
	}
	owner, err := uuid.NewRandom()
	if err != nil {
		return nil, fmt.Errorf("SSH preflight owner unavailable")
	}
	return services.NewProbePreflightOperationService(ctx, repository.NewProbePreflightStore(db), provisioning.NewSSHHostInspectorFactory(policy), owner.String(), 4)
}
