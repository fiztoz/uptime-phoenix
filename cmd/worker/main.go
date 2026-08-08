// Package main runs Phoenix in worker mode (scheduler + checkers + notifications).
package main

import (
	"fmt"
	"os"

	"github.com/fiztoz/uptime-phoenix/internal/bootstrap"
)

func main() {
	cfg, err := bootstrap.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to parse config: %v\n", err)
		os.Exit(1)
	}
	cfg.Mode = "worker"
	if err := bootstrap.Run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "uptime-phoenix-worker exited with error: %v\n", err)
		os.Exit(1)
	}
}
