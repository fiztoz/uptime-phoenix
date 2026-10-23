package bootstrap

import (
	"context"
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/caarlos0/env/v11"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// probeStageAuthorityUsage is the fixed help text of the stage-authority verb.
const probeStageAuthorityUsage = "Usage: phoenix-probe-admin stage-authority <init|check|public-key> [--file <path>]\n" +
	"Path defaults to PROBE_STAGE_AUTHORITY_KEY_FILE. The file holds exactly 32 raw bytes: an Ed25519 seed.\n" +
	"init requires an existing private directory (0700) and never replaces a key.\n" +
	"public-key prints the canonical host trust file for " + ProbeBinaryStageAuthorityKeyPath + ".\n" +
	"Use a key separate from PROBE_SECRET_KEY_FILE. No database, grant or host is contacted.\n"

// LoadProbeStageGrantSigner loads the hub's stage authority seed from a
// protected file (the same ownership, mode and size rules as the snapshot key)
// and constructs the Ed25519 grant signer. It never creates or replaces a key.
// The signer holds the expanded key in process memory; no zeroization of that
// copy is claimed. Temporary seed and key buffers are cleared.
func LoadProbeStageGrantSigner(ctx context.Context, path string) (*provisioning.Ed25519ProbeStageGrantSigner, error) {
	if probeBinaryStageHelperNil(ctx) || path == "" {
		return nil, errors.New("stage authority key file is required")
	}
	seed, err := auth.ReadProbeSecretKeyFile(ctx, path)
	defer clear(seed[:])
	if err != nil {
		return nil, err
	}
	private := ed25519.NewKeyFromSeed(seed[:])
	defer clear(private)
	signer, err := provisioning.NewEd25519ProbeStageGrantSigner(private)
	if err != nil {
		return nil, errors.New("stage authority key is not a valid Ed25519 seed")
	}
	return signer, nil
}

// RunProbeStageAuthorityKey provisions, checks or exports the public half of
// the stage authority key. It loads no hub configuration and opens no
// database. Only public-key writes to stdout, and only public bytes; private
// key material is never printed. Exit 0 success/help, 1 failure, 2 usage.
func RunProbeStageAuthorityKey(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		_, _ = io.WriteString(stdout, probeStageAuthorityUsage)
		return 0
	}
	if len(args) == 0 || (args[0] != "init" && args[0] != "check" && args[0] != "public-key") {
		_, _ = io.WriteString(stderr, probeStageAuthorityUsage)
		return 2
	}
	var cfg struct {
		File string `env:"PROBE_STAGE_AUTHORITY_KEY_FILE"`
	}
	if err := env.Parse(&cfg); err != nil {
		_, _ = io.WriteString(stderr, "read stage authority key configuration failed\n")
		return 2
	}
	flags := flag.NewFlagSet("stage-authority", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&cfg.File, "file", cfg.File, "protected stage authority key file")
	if err := flags.Parse(args[1:]); errors.Is(err, flag.ErrHelp) {
		_, _ = io.WriteString(stdout, probeStageAuthorityUsage)
		return 0
	} else if err != nil || flags.NArg() != 0 || cfg.File == "" {
		_, _ = io.WriteString(stderr, probeStageAuthorityUsage)
		return 2
	}
	if args[0] == "init" {
		if err := auth.CreateProbeSecretKeyFile(ctx, cfg.File); err != nil {
			_, _ = fmt.Fprintf(stderr, "stage-authority: %v\n", err)
			return 1
		}
		_, _ = io.WriteString(stdout, "Stage authority key created. Back it up separately; export its public key to each host.\n")
		return 0
	}
	signer, err := LoadProbeStageGrantSigner(ctx, cfg.File)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "stage-authority: %v\n", err)
		return 1
	}
	if args[0] == "check" {
		_, _ = fmt.Fprintf(stdout, "Stage authority key file is valid. Authority key id %s. No host or grant was checked.\n", domain.ProbeBinaryStageAuthorityKeyID(signer.PublicKey()))
		return 0
	}
	body := provisioning.EncodeProbeBinaryStageAuthorityKey(signer.PublicKey())
	if body == nil {
		_, _ = io.WriteString(stderr, "stage-authority: public key unavailable\n")
		return 1
	}
	if n, err := stdout.Write(body); err != nil || n != len(body) {
		_, _ = io.WriteString(stderr, "stage-authority: write public key failed\n")
		return 1
	}
	return 0
}
