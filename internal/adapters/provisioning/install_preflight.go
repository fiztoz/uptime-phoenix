package provisioning

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// SSHProbeInstallPreflightCommand is the fixed nonmutating helper invocation
// the installer runs before creating anything. It needs no grant: it reads
// host facts only and changes nothing.
const SSHProbeInstallPreflightCommand = "/usr/local/bin/phoenix-probe-admin install-preflight"

const probeInstallReportMagic = "phoenix-install-preflight-v1"

// probeInstallReportKeys is the exact, ordered line set of a report.
var probeInstallReportKeys = []string{"kernel=", "machine=", "systemd=", "docker=", "root=", "authority=", "existing="}

// EncodeProbeInstallHostReport renders the canonical report, or nil for an
// invalid one.
func EncodeProbeInstallHostReport(r domain.ProbeInstallHostReport) []byte {
	if !r.Valid() {
		return nil
	}
	values := []string{r.Kernel, r.Machine, installReportBool(r.Systemd), installReportBool(r.DockerCLI),
		installReportBool(r.Root), installReportBool(r.Authority), strconv.Itoa(r.ExistingInstalls)}
	var b strings.Builder
	b.WriteString(probeInstallReportMagic + "\n")
	for i, key := range probeInstallReportKeys {
		b.WriteString(key + values[i] + "\n")
	}
	return []byte(b.String())
}

// ParseProbeInstallHostReport accepts exactly the canonical encoding.
func ParseProbeInstallHostReport(text string) (domain.ProbeInstallHostReport, error) {
	var zero domain.ProbeInstallHostReport
	lines := strings.Split(text, "\n")
	if len(lines) != len(probeInstallReportKeys)+2 || lines[0] != probeInstallReportMagic || lines[len(lines)-1] != "" {
		return zero, domain.ErrProbeSSHDiscovery
	}
	values := make([]string, len(probeInstallReportKeys))
	for i, key := range probeInstallReportKeys {
		value, ok := strings.CutPrefix(lines[i+1], key)
		if !ok {
			return zero, domain.ErrProbeSSHDiscovery
		}
		values[i] = value
	}
	flags := make([]bool, 4)
	for i, value := range values[2:6] {
		switch value {
		case "0":
		case "1":
			flags[i] = true
		default:
			return zero, domain.ErrProbeSSHDiscovery
		}
	}
	existing, err := strconv.Atoi(values[6])
	if err != nil || strconv.Itoa(existing) != values[6] {
		return zero, domain.ErrProbeSSHDiscovery
	}
	report := domain.ProbeInstallHostReport{
		Kernel: values[0], Machine: values[1], Systemd: flags[0], DockerCLI: flags[1],
		Root: flags[2], Authority: flags[3], ExistingInstalls: existing,
	}
	if !report.Valid() {
		return zero, domain.ErrProbeSSHDiscovery
	}
	return report, nil
}

func installReportBool(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

// InspectInstallHost runs the fixed install-preflight helper as host's user
// after host-key verification and returns its parsed report. Failures are
// either the context's error or a *domain.ProbeInstallPreflightError whose
// code classifies where it stopped; nothing on the host is changed.
func (s *SSHHostInspector) InspectInstallHost(ctx context.Context, host domain.ProbeSSHHost) (domain.ProbeInstallHostReport, error) {
	var zero domain.ProbeInstallHostReport
	if ctx == nil {
		return zero, &domain.ProbeInstallPreflightError{Code: domain.ProbeInstallPreflightHelper}
	}
	text, err := s.runFixedCommand(ctx, host, SSHProbeInstallPreflightCommand)
	if errors.Is(err, domain.ErrProbeSSHTrust) && s.learnHostKeyAlgorithms(ctx, host.Address) {
		text, err = s.runFixedCommand(ctx, host, SSHProbeInstallPreflightCommand)
	}
	if err != nil {
		return zero, installPreflightError(err)
	}
	report, err := ParseProbeInstallHostReport(text)
	if err != nil {
		return zero, &domain.ProbeInstallPreflightError{Code: domain.ProbeInstallPreflightHelper}
	}
	return report, nil
}

// installPreflightError keeps context errors and maps every SSH failure to
// one fixed refusal code.
func installPreflightError(err error) error {
	for _, ctxErr := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, ctxErr) {
			return ctxErr
		}
	}
	code := domain.ProbeInstallPreflightHelper
	switch {
	case errors.Is(err, domain.ErrProbeSSHTrust):
		code = domain.ProbeInstallPreflightTrust
	case errors.Is(err, domain.ErrProbeSSHDestination):
		code = domain.ProbeInstallPreflightDestination
	case errors.Is(err, errSSHAuthRejected):
		code = domain.ProbeInstallPreflightAuth
	case errors.Is(err, domain.ErrProbeSSHConnect):
		code = domain.ProbeInstallPreflightUnreachable
	}
	return &domain.ProbeInstallPreflightError{Code: code}
}
