package bootstrap

import (
	"context"
	"io"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// probeInstallPreflightUsage is the only text for a non-empty argument list.
const probeInstallPreflightUsage = "Usage: phoenix-probe-admin install-preflight\nNo flags, paths, or arguments. Reads host facts for the SSH installer and changes nothing.\n"

// probeInstallPreflightOwnedPrefix names every entry an install creates
// (data directory, artifact directory, unit file).
const probeInstallPreflightOwnedPrefix = "uptime-phoenix-probe-"

// probeInstallPreflightParents are the literal directories an install
// creates entries beneath.
var probeInstallPreflightParents = []string{"/var/lib", "/opt", "/etc/systemd/system"}

// RunProbeInstallPreflightHelper writes one canonical install-preflight
// report and exits 0. It reads no input, takes no grant and changes nothing:
// it is safe to run under the restricted staging key's forced command. Any
// failure to gather a complete report exits nonzero with a fixed error and
// prints no report.
func RunProbeInstallPreflightHelper(ctx context.Context, args []string, input io.ReadCloser, out, stderr io.Writer) int {
	return runProbeInstallPreflightHelper(ctx, args, input, out, stderr, gatherHostProbeInstallReport)
}

func runProbeInstallPreflightHelper(ctx context.Context, args []string, input io.ReadCloser, out, stderr io.Writer,
	gather func(context.Context) (domain.ProbeInstallHostReport, error),
) int {
	if err := probeBinaryStageHelperCloseInput(input); err != nil {
		return probeBinaryStageHelperExit(stderr, err)
	}
	if len(args) != 0 {
		if !probeBinaryStageHelperNil(stderr) {
			_, _ = io.WriteString(stderr, probeInstallPreflightUsage)
		}
		return 2
	}
	if probeBinaryStageHelperNil(ctx) || probeBinaryStageHelperNil(out) || probeBinaryStageHelperNil(gather) {
		return probeBinaryStageHelperExit(stderr, domain.ErrProbeBinaryStageWriteInput)
	}
	if err := ctx.Err(); err != nil {
		return probeBinaryStageHelperExit(stderr, probeBinaryStageHelperContextError(err))
	}
	report, err := gather(ctx)
	if err != nil {
		return probeBinaryStageHelperExit(stderr, probeBinaryStageHelperPreError(err))
	}
	encoded := provisioning.EncodeProbeInstallHostReport(report)
	if encoded == nil {
		return probeBinaryStageHelperExit(stderr, domain.ErrProbeBinaryStageWriteUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return probeBinaryStageHelperExit(stderr, probeBinaryStageHelperContextError(err))
	}
	if n, err := out.Write(encoded); err != nil || n != len(encoded) {
		return probeBinaryStageHelperExit(stderr, domain.ErrProbeArtifactStorage)
	}
	return 0
}
