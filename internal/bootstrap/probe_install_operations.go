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

// probeInstallOperations owns the API-local install and activation services
// and the artifact directory handle the install session factory holds open.
type probeInstallOperations struct {
	svc        *services.ProbeInstallOperationService
	activation *services.ProbeActivationOperationService
	cleanup    *services.ProbeCleanupOperationService
	factory    *provisioning.SSHProbeInstallSessionFactory
	// bootstrap is the one-time SSH-password host preparation; bootstrapper
	// holds its own artifact directory handle.
	bootstrap    *services.ProbeHostBootstrapService
	bootstrapper *provisioning.SSHProbeHostBootstrapper
}

// bootstrapService returns the handler-facing host bootstrap service, or nil.
func (o *probeInstallOperations) bootstrapService() *services.ProbeHostBootstrapService {
	if o == nil {
		return nil
	}
	return o.bootstrap
}

// service returns the handler-facing service, or a typed nil when disabled.
func (o *probeInstallOperations) service() *services.ProbeInstallOperationService {
	if o == nil {
		return nil
	}
	return o.svc
}

// activationService returns the handler-facing activation service, or nil.
func (o *probeInstallOperations) activationService() *services.ProbeActivationOperationService {
	if o == nil {
		return nil
	}
	return o.activation
}

// cleanupService returns the handler-facing cleanup service, or nil.
func (o *probeInstallOperations) cleanupService() *services.ProbeCleanupOperationService {
	if o == nil {
		return nil
	}
	return o.cleanup
}

// Close joins every cleanup, activation and install run, then releases the
// artifact directory.
func (o *probeInstallOperations) Close(ctx context.Context) error {
	if o == nil {
		return nil
	}
	var bootstrapErr, cleanupErr, activationErr error
	if o.bootstrap != nil {
		bootstrapErr = errors.Join(o.bootstrap.Close(ctx), o.bootstrapper.Close())
	}
	if o.cleanup != nil {
		cleanupErr = o.cleanup.Close(ctx)
	}
	if o.activation != nil {
		activationErr = o.activation.Close(ctx)
	}
	return errors.Join(bootstrapErr, cleanupErr, activationErr, o.svc.Close(ctx), o.factory.Close())
}

// newProbeInstallOperations is API-local like preflight: workers never run
// installs, and the default deployment loads no key, policy or artifact dir.
// The stage authority key loads at startup so a bad key fails fast.
// admin is the hub's probe admin service; activation registers and enrolls
// through it.
func newProbeInstallOperations(ctx context.Context, cfg Config, db *bun.DB, admin *services.ProbeAdminService) (*probeInstallOperations, error) {
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
	activation, err := newProbeActivationOperations(ctx, db, policy, signer, journal, admin, owner.String())
	if err != nil {
		_ = svc.Close(ctx)
		_ = factory.Close()
		return nil, err
	}
	cleanup, err := newProbeCleanupOperations(ctx, db, policy, signer, journal, owner.String())
	if err != nil {
		_ = activation.Close(ctx)
		_ = svc.Close(ctx)
		_ = factory.Close()
		return nil, err
	}
	ops := &probeInstallOperations{svc: svc, activation: activation, cleanup: cleanup, factory: factory}
	// The host bootstrap installs the same stage-authority PUBLIC key the
	// signer above signs with, so a bootstrapped host trusts this hub.
	bootstrapper, err := provisioning.NewSSHProbeHostBootstrapper(cfg.ProbeInstallArtifactDir, policy, signer.PublicKey())
	if err != nil {
		_ = ops.Close(ctx)
		return nil, err
	}
	bootstrap, err := services.NewProbeHostBootstrapService(bootstrapper, probeInstallCapacity)
	if err != nil {
		_ = bootstrapper.Close()
		_ = ops.Close(ctx)
		return nil, err
	}
	ops.bootstrap, ops.bootstrapper = bootstrap, bootstrapper
	return ops, nil
}

// newProbeCleanupOperations composes cleanup: its own one-shot admission
// table behind the same Ed25519 authority and an SSH cleanup session factory.
func newProbeCleanupOperations(ctx context.Context, db *bun.DB, policy probe.EndpointPolicy, signer *provisioning.Ed25519ProbeStageGrantSigner, journal *services.ProbeInstallJournalService, owner string) (*services.ProbeCleanupOperationService, error) {
	store := repository.NewProbeInstallCleanupStore(db)
	actions, err := services.NewProbeCleanupActionAuthorizationService(store, signer)
	if err != nil {
		return nil, err
	}
	sessions, err := provisioning.NewSSHProbeCleanupSessionFactory(policy, actions)
	if err != nil {
		return nil, err
	}
	return services.NewProbeCleanupOperationService(ctx, store, journal, sessions, owner, probeInstallCapacity)
}

// newProbeActivationOperations composes activation: its own admission table
// behind the same Ed25519 authority, the SSH host session factory and the
// probe admin service for registration and enrollment.
func newProbeActivationOperations(ctx context.Context, db *bun.DB, policy probe.EndpointPolicy, signer *provisioning.Ed25519ProbeStageGrantSigner, journal *services.ProbeInstallJournalService, admin *services.ProbeAdminService, owner string) (*services.ProbeActivationOperationService, error) {
	if admin == nil {
		return nil, fmt.Errorf("probe activation requires the probe admin service")
	}
	store := repository.NewProbeInstallActivationStore(db)
	actions, err := services.NewProbeActivationActionAuthorizationService(store, signer)
	if err != nil {
		return nil, err
	}
	sessions, err := provisioning.NewSSHProbeActivationSessionFactory(policy, actions)
	if err != nil {
		return nil, err
	}
	return services.NewProbeActivationOperationService(ctx, store, journal, sessions, admin, probe.NewEndpointReadiness(policy), owner, probeInstallCapacity)
}
