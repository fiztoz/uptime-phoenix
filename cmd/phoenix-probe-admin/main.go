// Command phoenix-probe-admin provides explicit local hub-operator registration,
// enrollment, assignment and complete snapshot preparation for the M2 runtime.
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
	cfg, err := bootstrap.LoadConfig()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "Invalid hub environment configuration")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(bootstrap.RunProbeAdmin(ctx, cfg, os.Args[1:], os.Stdout, os.Stderr))
}
