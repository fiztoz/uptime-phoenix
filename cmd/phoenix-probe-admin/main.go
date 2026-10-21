// Command phoenix-probe-admin provides explicit local hub-operator registration,
// enrollment, assignment and complete snapshot preparation for the M2 runtime,
// plus local binary staging, optional SSH preflight and offline artifact verification
// without hub startup.
package main

import (
	"context"
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
		_, _ = fmt.Fprintln(os.Stderr, "Invalid hub environment configuration")
		os.Exit(2)
	}
	os.Exit(bootstrap.RunProbeAdmin(ctx, cfg, os.Args[1:], os.Stdout, os.Stderr))
}
