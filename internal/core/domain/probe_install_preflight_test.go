package domain

import (
	"strings"
	"testing"
)

func preflightIdentity() ProbeInstallIdentity {
	fp := "SHA256:" + strings.Repeat("A", 43)
	return ProbeInstallIdentity{OperationID: "11111111-1111-4111-8111-111111111111", Host: ProbeSSHHost{Address: "edge.example:22", User: "root"},
		HostKeyFingerprint: fp, ClientKeyFingerprint: fp, RequestedBy: 1, LayoutVersion: 1, Mode: ProbeInstallSystemd,
		Binary: ProbeBinaryPin{Version: "1.2.3", Architecture: "arm64", SHA256: strings.Repeat("a", 64), Size: 17}}
}

func TestProbeInstallHostReportRefusalOrder(t *testing.T) {
	identity := preflightIdentity()
	if !identity.Valid() {
		t.Fatal("fixture identity invalid")
	}
	clean := ProbeInstallHostReport{Kernel: "Linux", Machine: "aarch64", Systemd: true, Root: true, Authority: true}
	if code := clean.Refusal(identity); code != "" {
		t.Fatalf("clean host refused: %q", code)
	}
	// Each case breaks everything from its check onward, so the first
	// failing check in the documented order is the one reported.
	for _, tc := range []struct {
		edit func(*ProbeInstallHostReport)
		want string
	}{
		{func(r *ProbeInstallHostReport) {
			*r = ProbeInstallHostReport{Kernel: "Darwin", Machine: "x86_64", ExistingInstalls: 1}
		}, ProbeInstallPreflightOS},
		{func(r *ProbeInstallHostReport) {
			*r = ProbeInstallHostReport{Kernel: "Linux", Machine: "x86_64", ExistingInstalls: 1}
		}, ProbeInstallPreflightArchitecture},
		{func(r *ProbeInstallHostReport) {
			*r = ProbeInstallHostReport{Kernel: "Linux", Machine: "aarch64", ExistingInstalls: 1}
		}, ProbeInstallPreflightRoot},
		{func(r *ProbeInstallHostReport) {
			*r = ProbeInstallHostReport{Kernel: "Linux", Machine: "arm64", Root: true, ExistingInstalls: 1}
		}, ProbeInstallPreflightSystemd},
		{func(r *ProbeInstallHostReport) {
			*r = ProbeInstallHostReport{Kernel: "Linux", Machine: "aarch64", Root: true, Systemd: true, ExistingInstalls: 1}
		}, ProbeInstallPreflightAuthority},
		{func(r *ProbeInstallHostReport) { r.ExistingInstalls = 1 }, ProbeInstallPreflightExisting},
		{func(r *ProbeInstallHostReport) { r.ExistingInstalls = ProbeInstallPreflightMaxExisting + 1 }, ProbeInstallPreflightHelper},
		{func(r *ProbeInstallHostReport) { r.Machine = "" }, ProbeInstallPreflightHelper},
	} {
		r := clean
		tc.edit(&r)
		if got := r.Refusal(identity); got != tc.want || !ValidProbeInstallPreflightCode(got) {
			t.Fatalf("%+v: refusal %q, want %q", r, got, tc.want)
		}
	}
	if code := clean.Refusal(ProbeInstallIdentity{}); code != ProbeInstallPreflightHelper {
		t.Fatalf("invalid identity: %q", code)
	}
}

func TestProbeInstallHostArchitecture(t *testing.T) {
	for machine, want := range map[string]string{"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64", "armv7l": "", "i686": "", "": ""} {
		if got := ProbeInstallHostArchitecture(machine); got != want {
			t.Fatalf("%q -> %q, want %q", machine, got, want)
		}
	}
}
