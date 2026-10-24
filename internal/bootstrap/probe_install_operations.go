package bootstrap

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// probeInstallCapacity bounds concurrent installs per API process.
const probeInstallCapacity = 2

func validateProbeInstallConfig(cfg Config) error {
	if !cfg.ProbeSSHInstallEnabled {
		return nil
	}
	switch {
	case !cfg.ProbesEnabled:
		return fmt.Errorf("PROBE_SSH_INSTALL_ENABLED requires PROBES_ENABLED")
	case cfg.ProbeStageAuthorityKeyFile == "":
		return fmt.Errorf("PROBE_SSH_INSTALL_ENABLED requires PROBE_STAGE_AUTHORITY_KEY_FILE")
	case cfg.ProbeInstallArtifactDir == "":
		return fmt.Errorf("PROBE_SSH_INSTALL_ENABLED requires PROBE_INSTALL_ARTIFACT_DIR")
	}
	return nil
}

// probeInstallOperations owns the API-local install service and the artifact
// directory handle its session factory holds open.
type probeInstallOperations struct {
	svc     *services.ProbeInstallOperationService
	factory *provisioning.SSHProbeInstallSessionFactory
}

// service returns the handler-facing service, or a typed nil when disabled.
func (o *probeInstallOperations) service() *services.ProbeInstallOperationService {
	if o == nil {
		return nil
	}
	return o.svc
}

// Close joins every run, then releases the artifact directory.
func (o *probeInstallOperations) Close(ctx context.Context) error {
	if o == nil {
		return nil
	}
	return errors.Join(o.svc.Close(ctx), o.factory.Close())
}

// newProbeInstallOperations is API-local like preflight: workers never run
// installs, and the default deployment loads no key, policy or artifact dir.
// The stage authority key loads at startup so a bad key fails fast.
func newProbeInstallOperations(ctx context.Context, cfg Config, db *bun.DB) (*probeInstallOperations, error) {
	if err := validateProbeInstallConfig(cfg); err != nil {
		return nil, err
	}
	if !cfg.ProbeSSHInstallEnabled || cfg.Mode == "worker" {
		return nil, nil
	}
	policy, err := probe.LoadEndpointPolicy(cfg.ProbeEndpointPolicyFile)
	if err != nil {
		return nil, err
	}
	signer, err := LoadProbeStageGrantSigner(ctx, cfg.ProbeStageAuthorityKeyFile)
	if err != nil {
		return nil, fmt.Errorf("loading stage authority key: %w", err)
	}
	stage, err := services.NewProbeBinaryStageAuthorizationService(repository.NewProbeBinaryStageAdmissionStore(db), signer)
	if err != nil {
		return nil, err
	}
	actions, err := services.NewProbeHostActionAuthorizationService(repository.NewProbeHostActionAdmissionStore(db), signer)
	if err != nil {
		return nil, err
	}
	factory, err := provisioning.NewSSHProbeInstallSessionFactory(cfg.ProbeInstallArtifactDir, policy, stage, actions)
	if err != nil {
		return nil, err
	}
	store := repository.NewProbeInstallJournalStore(db)
	journal, err := services.NewProbeInstallJournalService(store, store)
	if err != nil {
		_ = factory.Close()
		return nil, err
	}
	owner, err := uuid.NewRandom()
	if err != nil {
		_ = factory.Close()
		return nil, fmt.Errorf("probe install owner unavailable")
	}
	svc, err := services.NewProbeInstallOperationService(ctx, journal, factory, owner.String(), probeInstallCapacity)
	if err != nil {
		_ = factory.Close()
		return nil, err
	}
	return &probeInstallOperations{svc: svc, factory: factory}, nil
}
