// Package main is the composition root for Phoenix (all-in-one deployment).
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
	if err := bootstrap.Run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "phoenix exited with error: %v\n", err)
		os.Exit(1)
	}
}
