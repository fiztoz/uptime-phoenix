package services

import (
	"context"
	"errors"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

type preflightInspector struct {
	calls int
	facts domain.ProbeHostFacts
	err   error
}

func (s *preflightInspector) InspectHost(_ context.Context, _ domain.ProbeSSHHost) (domain.ProbeHostFacts, error) {
	s.calls++
	return s.facts, s.err
}

func TestProbeHostPreflight(t *testing.T) {
	host := domain.ProbeSSHHost{Address: "edge.example:22", User: "root"}
	for _, tc := range []struct {
		name    string
		kernel  string
		machine string
		priv    string
		docker  bool
		systemd bool
		arch    string
		err     error
	}{
		{"systemd_clean_host", "Linux", "x86_64", "root", false, true, "amd64", nil},
		{"docker_without_systemd", "Linux", "aarch64", "sudo", true, false, "arm64", nil},
		{"arm64_alias", "Linux", "arm64", "root", false, true, "arm64", nil},
		{"wrong_os", "Darwin", "arm64", "root", true, true, "", domain.ErrProbeHostPlatform},
		{"wrong_arch", "Linux", "armv7l", "root", true, true, "", domain.ErrProbeHostPlatform},
		{"no_sudo", "Linux", "x86_64", "none", true, true, "", domain.ErrProbeHostPrivilege},
		{"unknown_privilege", "Linux", "x86_64", "unexpected", true, true, "", domain.ErrProbeHostPrivilege},
		{"no_runtime", "Linux", "x86_64", "sudo", false, false, "", domain.ErrProbeHostRuntime},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inspector := &preflightInspector{facts: domain.ProbeHostFacts{
				Kernel: tc.kernel, Machine: tc.machine, Privilege: tc.priv, DockerCLI: tc.docker, Systemd: tc.systemd,
			}}
			result, err := NewProbeHostPreflightService(inspector).Inspect(t.Context(), host)
			if inspector.calls != 1 || !errors.Is(err, tc.err) {
				t.Fatalf("calls=%d error=%v, want %v", inspector.calls, err, tc.err)
			}
			if tc.err != nil {
				if result != nil {
					t.Fatal("failed preflight returned a result")
				}
				return
			}
			if result == nil || result.OS != "linux" || result.Architecture != tc.arch || result.Privilege != tc.priv || result.DockerCLI != tc.docker || result.Systemd != tc.systemd {
				t.Fatalf("wrong compatibility facts: %+v", result)
			}
		})
	}
}

func TestProbeHostPreflightRefusesBeforeIO(t *testing.T) {
	inspector := &preflightInspector{}
	service := NewProbeHostPreflightService(inspector)
	if _, err := service.Inspect(t.Context(), domain.ProbeSSHHost{}); !errors.Is(err, domain.ErrProbeSSHInput) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := service.Inspect(ctx, domain.ProbeSSHHost{Address: "edge:22", User: "root"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if inspector.calls != 0 {
		t.Fatal("invalid or canceled request contacted inspector")
	}
}

func TestProbeHostPreflightTransportFailure(t *testing.T) {
	inspector := &preflightInspector{err: domain.ErrProbeSSHTrust}
	if result, err := NewProbeHostPreflightService(inspector).Inspect(t.Context(), domain.ProbeSSHHost{Address: "edge:22", User: "root"}); result != nil || !errors.Is(err, domain.ErrProbeSSHTrust) {
		t.Fatalf("result=%v error=%v", result, err)
	}
}
