package bootstrap

import (
	"context"
	"io"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning"
)

// probeAuthorizedDispatchUsage is the only text for a refused dispatch.
const probeAuthorizedDispatchUsage = "Usage: restrict,command=\"/usr/local/bin/phoenix-probe-admin authorized-dispatch\" in authorized_keys.\nSSH_ORIGINAL_COMMAND must be exactly one fixed authorized staging, host-action or install-preflight command.\n"

// RunAuthorizedProbeDispatch is the single forced command for a restricted
// staging key. It selects one of the two fixed authorized verbs, or the
// nonmutating install preflight, by an exact match of the requested command
// (SSH_ORIGINAL_COMMAND); anything else is refused without echoing it.
// Authority to change the host still comes only from each authorized verb's
// verified one-shot grant, never from the requested command.
func RunAuthorizedProbeDispatch(ctx context.Context, args []string, originalCommand string, input io.ReadCloser, out, stderr io.Writer) int {
	if len(args) == 0 {
		switch originalCommand {
		case provisioning.SSHProbeBinaryStageAuthorizedCommand:
			return RunAuthorizedProbeBinaryStageHelper(ctx, nil, input, out, stderr)
		case provisioning.SSHProbeHostActionCommand:
			return RunAuthorizedProbeHostActionHelper(ctx, nil, input, out, stderr)
		case provisioning.SSHProbeInstallPreflightCommand:
			return RunProbeInstallPreflightHelper(ctx, nil, input, out, stderr)
		}
	}
	if err := probeBinaryStageHelperCloseInput(input); err != nil {
		return probeBinaryStageHelperExit(stderr, err)
	}
	if !probeBinaryStageHelperNil(stderr) {
		_, _ = io.WriteString(stderr, probeAuthorizedDispatchUsage)
	}
	return 2
}
