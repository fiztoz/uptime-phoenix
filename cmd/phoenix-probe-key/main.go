// Command phoenix-probe-key explicitly provisions or checks a snapshot key.
// It does not connect to a database, activate probes, or print key material.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/caarlos0/env/v11"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	const usage = "Usage: phoenix-probe-key <init|check> [--file <path>]\nPath defaults to PROBE_SECRET_KEY_FILE. Keys contain exactly 32 raw bytes.\ninit requires an existing private directory (0700) and never replaces a key.\ncheck validates the file only; it does not verify any stored snapshot or activate probes.\n"
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		_, _ = io.WriteString(stdout, usage)
		return 0
	}
	if len(args) == 0 || (args[0] != "init" && args[0] != "check") {
		_, _ = io.WriteString(stderr, usage)
		return 2
	}
	var cfg struct {
		File string `env:"PROBE_SECRET_KEY_FILE"`
	}
	if err := env.Parse(&cfg); err != nil {
		_, _ = io.WriteString(stderr, "read probe key configuration failed\n")
		return 2
	}
	flags := flag.NewFlagSet("phoenix-probe-key", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&cfg.File, "file", cfg.File, "protected key file")
	if err := flags.Parse(args[1:]); errors.Is(err, flag.ErrHelp) {
		_, _ = io.WriteString(stdout, usage)
		return 0
	} else if err != nil || flags.NArg() != 0 || cfg.File == "" {
		_, _ = io.WriteString(stderr, usage)
		return 2
	}
	var err error
	if args[0] == "init" {
		err = auth.CreateProbeSecretKeyFile(ctx, cfg.File)
	} else {
		_, err = auth.NewProbeConfigProtectorFromFile(ctx, cfg.File)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "phoenix-probe-key: %v\n", err)
		return 1
	}
	if args[0] == "init" {
		_, _ = io.WriteString(stdout, "Probe secret key created. Back it up separately from encrypted snapshots.\n")
	} else {
		_, _ = io.WriteString(stdout, "Probe secret key file is valid. Stored snapshots were not checked.\n")
	}
	return 0
}
