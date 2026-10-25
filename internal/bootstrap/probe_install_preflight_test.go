package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func preflightHelperReport() domain.ProbeInstallHostReport {
	return domain.ProbeInstallHostReport{Kernel: "Linux", Machine: "x86_64", Systemd: true, Root: true, Authority: true}
}

func runPreflightHelper(ctx context.Context, args []string, gather func(context.Context) (domain.ProbeInstallHostReport, error)) (int, string, string, *hostMockInput) {
	input := &hostMockInput{}
	var out, stderr bytes.Buffer
	code := runProbeInstallPreflightHelper(ctx, args, input, &out, &stderr, gather)
	return code, out.String(), stderr.String(), input
}

func TestProbeInstallPreflightHelperWritesOneCanonicalReport(t *testing.T) {
	code, out, stderr, input := runPreflightHelper(t.Context(), nil, func(context.Context) (domain.ProbeInstallHostReport, error) {
		return preflightHelperReport(), nil
	})
	if code != 0 || stderr != "" || out != string(provisioning.EncodeProbeInstallHostReport(preflightHelperReport())) {
		t.Fatalf("code=%d out=%q stderr=%q", code, out, stderr)
	}
	if input.closes.Load() != 1 || input.reads.Load() != 0 {
		t.Fatalf("input closes=%d reads=%d, want 1/0", input.closes.Load(), input.reads.Load())
	}
}

func TestProbeInstallPreflightHelperRefusals(t *testing.T) {
	gathered := 0
	ok := func(context.Context) (domain.ProbeInstallHostReport, error) {
		gathered++
		return preflightHelperReport(), nil
	}
	code, out, stderr, input := runPreflightHelper(t.Context(), []string{"--root", "/"}, ok)
	if code != 2 || out != "" || stderr != probeInstallPreflightUsage || gathered != 0 || input.closes.Load() != 1 {
		t.Fatalf("args: code=%d out=%q stderr=%q gathered=%d", code, out, stderr, gathered)
	}
	leak := errors.New("/etc/phoenix-probe/stage-authority.pub canary")
	fixed := func(report domain.ProbeInstallHostReport, err error) func(context.Context) (domain.ProbeInstallHostReport, error) {
		return func(context.Context) (domain.ProbeInstallHostReport, error) { return report, err }
	}
	for name, gather := range map[string]func(context.Context) (domain.ProbeInstallHostReport, error){
		"gather error":   fixed(domain.ProbeInstallHostReport{}, errors.Join(leak, domain.ErrProbeBinaryStageWriteUnavailable)),
		"invalid report": fixed(domain.ProbeInstallHostReport{Kernel: "Linux"}, nil),
	} {
		code, out, stderr, _ := runPreflightHelper(t.Context(), nil, gather)
		if code != 1 || out != "" || stderr == "" || strings.Contains(stderr, "canary") {
			t.Fatalf("%s: code=%d out=%q stderr=%q", name, code, out, stderr)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	gathered = 0
	if code, out, _, _ := runPreflightHelper(ctx, nil, ok); code != 1 || out != "" || gathered != 0 {
		t.Fatalf("canceled: code=%d out=%q gathered=%d", code, out, gathered)
	}
}

// The forced command routes the exact preflight command to the helper (never
// the dispatch usage) and refuses near misses.
func TestAuthorizedDispatchRoutesTheInstallPreflight(t *testing.T) {
	r := runDispatch(t.Context(), nil, provisioning.SSHProbeInstallPreflightCommand, nil)
	if r.code == 2 || r.stderr.String() == probeAuthorizedDispatchUsage {
		t.Fatalf("exact preflight command was refused by the dispatcher: code=%d stderr=%q", r.code, r.stderr.String())
	}
	if runtime.GOOS != "linux" {
		// The real gatherer never qualifies an unsupported platform.
		if r.code != 1 || r.out.Len() != 0 {
			t.Fatalf("off linux: code=%d stdout=%q", r.code, r.out.String())
		}
	} else if r.code == 0 {
		if _, err := provisioning.ParseProbeInstallHostReport(r.out.String()); err != nil {
			t.Fatalf("linux report did not parse: %v", err)
		}
	}
	for _, near := range []string{
		provisioning.SSHProbeInstallPreflightCommand + " ",
		provisioning.SSHProbeInstallPreflightCommand + " --verbose",
		"phoenix-probe-admin install-preflight",
		strings.ToUpper(provisioning.SSHProbeInstallPreflightCommand),
	} {
		runDispatch(t.Context(), nil, near, nil).wantUsageRefusal(t, near)
	}
}
