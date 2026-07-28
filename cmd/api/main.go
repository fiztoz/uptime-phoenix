// Package main runs Phoenix in API mode (HTTP + WebSocket only).
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
	cfg.Mode = "api"
	if err := bootstrap.Run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "phoenix-api exited with error: %v\n", err)
		os.Exit(1)
	}
}
