package services

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ProbeHostPreflightService checks initial-install compatibility without making
// changes to the host or creating a hub registration/administrative operation.
type ProbeHostPreflightService struct {
	inspector ports.ProbeHostInspector
}

// NewProbeHostPreflightService binds a trusted, bounded host inspector.
func NewProbeHostPreflightService(inspector ports.ProbeHostInspector) *ProbeHostPreflightService {
	return &ProbeHostPreflightService{inspector: inspector}
}

// Inspect returns only supported Linux initial-install prerequisites. A passed
// preflight does not prove Docker daemon health, runtime enrollment, listener
// reachability, artifact integrity, or configuration activation.
func (s *ProbeHostPreflightService) Inspect(ctx context.Context, host domain.ProbeSSHHost) (*domain.ProbeHostPreflight, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !host.Valid() {
		return nil, domain.ErrProbeSSHInput
	}
	if s == nil || s.inspector == nil {
		return nil, domain.ErrInternal
	}
	facts, err := s.inspector.InspectHost(ctx, host)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := &domain.ProbeHostPreflight{
		OS: "linux", DockerCLI: facts.DockerCLI, Systemd: facts.Systemd, Privilege: facts.Privilege,
	}
	if facts.Kernel != "Linux" {
		return nil, domain.ErrProbeHostPlatform
	}
	switch facts.Machine {
	case "x86_64":
		result.Architecture = "amd64"
	case "aarch64", "arm64":
		result.Architecture = "arm64"
	default:
		return nil, domain.ErrProbeHostPlatform
	}
	if facts.Privilege != "root" && facts.Privilege != "sudo" {
		return nil, domain.ErrProbeHostPrivilege
	}
	if !facts.Systemd && !facts.DockerCLI {
		return nil, domain.ErrProbeHostRuntime
	}
	return result, nil
}
