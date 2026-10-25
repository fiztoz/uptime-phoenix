package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

const probeArtifactUsage = `Usage: phoenix-probe-admin verify-artifact --file PATH --version VERSION --architecture amd64|arm64 --sha256 LOWERCASE_HEX --size BYTES
Offline Linux probe binary preparation. Supply an out-of-band trusted version,
digest and exact size. No automatic checksum trust, download or artifact execution.
Copies to a private snapshot, checks exact bytes and Go/ELF component/platform,
then disposes it. pinned_version comes from the operator, not extracted metadata.
This is NOT host compatibility, installation, enrollment or config activation.
`

// RunProbeBinaryVerification verifies an offline pin without loading hub config.
// Exit zero reports artifact_verified only; its private snapshot is then removed.
func RunProbeBinaryVerification(ctx context.Context, args []string, out, stderr io.Writer) int {
	flags := flag.NewFlagSet("verify-artifact", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var source string
	var pin domain.ProbeBinaryPin
	flags.StringVar(&source, "file", "", "local release binary")
	flags.StringVar(&pin.Version, "version", "", "trusted release version without v")
	flags.StringVar(&pin.Architecture, "architecture", "", "linux architecture")
	flags.StringVar(&pin.SHA256, "sha256", "", "trusted lowercase SHA-256")
	flags.Int64Var(&pin.Size, "size", 0, "exact byte length")
	if err := flags.Parse(args); errors.Is(err, flag.ErrHelp) {
		_, _ = io.WriteString(out, probeArtifactUsage)
		return 0
	} else if err != nil || flags.NArg() != 0 || source == "" || !pin.Valid() {
		_, _ = io.WriteString(stderr, probeArtifactUsage)
		return 2
	}
	fail := func(err error) int {
		message := "Probe artifact verification failed"
		for _, safe := range []error{domain.ErrProbeArtifactInput, domain.ErrProbeArtifactSize, domain.ErrProbeArtifactDigest, domain.ErrProbeArtifactMetadata, domain.ErrProbeArtifactStorage, domain.ErrProbeArtifactClosed} {
			if errors.Is(err, safe) {
				message = safe.Error()
				break
			}
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			message = "Probe artifact verification canceled or timed out"
		}
		_, _ = fmt.Fprintln(stderr, message)
		return 1
	}
	if ctx == nil {
		return fail(domain.ErrProbeArtifactInput)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	input, err := provisioning.OpenProbeBinarySource(ctx, source)
	if err != nil {
		return fail(err)
	}
	service, err := services.NewProbeBinaryPreparationService(provisioning.NewProbeBinaryPreparer(""))
	if err != nil {
		_ = input.Close()
		return fail(err)
	}
	prepared, err := service.Prepare(ctx, pin, input)
	if err != nil {
		return fail(err)
	}
	// Cleanup is part of the command's effect, not a best-effort success defer.
	// No reusable snapshot path or installation authority crosses this boundary.
	if err = prepared.Close(context.WithoutCancel(ctx)); err != nil {
		return fail(err)
	}
	if ctx.Err() != nil {
		return fail(ctx.Err())
	}
	view := struct {
		State         string `json:"state"`
		ReleaseName   string `json:"release_name"`
		PinnedVersion string `json:"pinned_version"`
		VersionSource string `json:"version_source"`
		OS            string `json:"os"`
		Architecture  string `json:"architecture"`
		SHA256        string `json:"sha256"`
		Size          int64  `json:"size"`
	}{"artifact_verified", pin.ReleaseName(), pin.Version, "operator_pin", "linux", pin.Architecture, pin.SHA256, pin.Size}
	if json.NewEncoder(out).Encode(view) != nil {
		return fail(domain.ErrProbeArtifactStorage)
	}
	return 0
}
