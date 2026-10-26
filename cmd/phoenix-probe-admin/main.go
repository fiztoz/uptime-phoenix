// Command phoenix-probe-admin provides explicit local hub-operator registration,
// enrollment, assignment and complete snapshot preparation for the M2 runtime,
// plus local binary staging, optional SSH preflight and offline artifact verification
// without hub startup.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/fiztoz/uptime-phoenix/internal/bootstrap"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if len(os.Args) > 1 && os.Args[1] == "inspect-host" {
		os.Exit(bootstrap.RunProbeSSHPreflight(ctx, os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "stage-binary" {
		os.Exit(bootstrap.RunInheritedProbeBinaryStageHelper(ctx, os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "stage-binary-host" {
		os.Exit(bootstrap.RunHostProbeBinaryStageHelper(ctx, os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "stage-binary-authorized" {
		os.Exit(bootstrap.RunAuthorizedProbeBinaryStageHelper(ctx, os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "stage-host-action-authorized" {
		os.Exit(bootstrap.RunAuthorizedProbeHostActionHelper(ctx, os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "install-preflight" {
		os.Exit(bootstrap.RunProbeInstallPreflightHelper(ctx, os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "authorized-dispatch" {
		os.Exit(bootstrap.RunAuthorizedProbeDispatch(ctx, os.Args[2:], os.Getenv("SSH_ORIGINAL_COMMAND"), os.Stdin, os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "stage-authority" {
		os.Exit(bootstrap.RunProbeStageAuthorityKey(ctx, os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "verify-artifact" {
		os.Exit(bootstrap.RunProbeBinaryVerification(ctx, os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "verify-image" {
		os.Exit(bootstrap.RunProbeImageVerification(ctx, os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "verify-image-content" {
		os.Exit(bootstrap.RunProbeImageContentVerification(ctx, os.Args[2:], os.Stdout, os.Stderr))
	}
	cfg, err := bootstrap.LoadConfig()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, hubConfigErrorMessage(err))
		os.Exit(2)
	}
	os.Exit(bootstrap.RunProbeAdmin(ctx, cfg, os.Args[1:], os.Stdout, os.Stderr))
}

// hubConfigErrorMessage is the stderr line for a LoadConfig failure. Other
// config errors may quote environment values, so they stay generic; a
// JWT_SECRET error never echoes the key, so it is shown in full because the
// operator otherwise cannot tell that this hub-wide setting is what to fix.
func hubConfigErrorMessage(err error) string {
	if errors.Is(err, bootstrap.ErrInvalidJWTSecret) {
		return "Invalid hub environment configuration: " + err.Error() +
			" (phoenix-probe-admin loads the hub config, so it needs the same JWT_SECRET as the API)"
	}
	return "Invalid hub environment configuration"
}
