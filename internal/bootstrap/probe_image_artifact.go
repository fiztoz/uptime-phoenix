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
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

const probeImageUsage = `Usage: phoenix-probe-admin verify-image --repository HOST/NAME --version VERSION --architecture amd64|arm64 --index PATH --index-sha256 LOWERCASE_HEX --index-size BYTES --manifest PATH --config PATH
Offline verification of trusted index -> platform manifest -> image configuration.
Supply an out-of-band trusted index digest and exact size; no adjacent-file trust.
No registry, daemon, download, layer verification or container execution is used.
Version provenance is a digest-bound config label, not an inspected runtime version.
This is NOT installation, enrollment, cleanup authority or config activation.
`

// RunProbeImageVerification resolves offline digest-bound metadata before hub startup.
func RunProbeImageVerification(ctx context.Context, args []string, out, stderr io.Writer) int {
	flags := flag.NewFlagSet("verify-image", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var pin domain.ProbeImagePin
	var indexPath, manifestPath, configPath string
	flags.StringVar(&pin.Repository, "repository", "", "fully qualified immutable image repository")
	flags.StringVar(&pin.Version, "version", "", "trusted release version without v")
	flags.StringVar(&pin.Architecture, "architecture", "", "linux architecture")
	flags.StringVar(&pin.IndexSHA256, "index-sha256", "", "trusted lowercase index SHA-256")
	flags.Int64Var(&pin.IndexSize, "index-size", 0, "exact index byte length")
	flags.StringVar(&indexPath, "index", "", "local index JSON")
	flags.StringVar(&manifestPath, "manifest", "", "local platform manifest JSON")
	flags.StringVar(&configPath, "config", "", "local image config JSON")
	if err := flags.Parse(args); errors.Is(err, flag.ErrHelp) {
		_, _ = io.WriteString(out, probeImageUsage)
		return 0
	} else if err != nil || flags.NArg() != 0 || !pin.Valid() || indexPath == "" || manifestPath == "" || configPath == "" {
		_, _ = io.WriteString(stderr, probeImageUsage)
		return 2
	}
	fail := func(err error) int {
		message := "Probe image metadata verification failed"
		for _, safe := range []error{domain.ErrProbeImageInput, domain.ErrProbeImageSize, domain.ErrProbeImageDigest, domain.ErrProbeImageMetadata, domain.ErrProbeImageUnavailable} {
			if errors.Is(err, safe) {
				message = safe.Error()
				break
			}
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			message = "Probe image metadata verification canceled or timed out"
		}
		_, _ = fmt.Fprintln(stderr, message)
		return 1
	}
	if ctx == nil {
		return fail(domain.ErrProbeImageInput)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	opened := make([]io.ReadCloser, 0, 3)
	closeOpened := func() {
		for _, r := range opened {
			_ = r.Close()
		}
	}
	for _, path := range []string{indexPath, manifestPath, configPath} {
		source, err := provisioning.OpenProbeImageMetadataSource(ctx, path)
		if err != nil {
			closeOpened()
			return fail(err)
		}
		opened = append(opened, source)
	}
	service, err := services.NewProbeImagePreparationService(provisioning.NewProbeImageMetadataPreparer())
	if err != nil {
		closeOpened()
		return fail(err)
	}
	result, err := service.Prepare(ctx, pin, ports.ProbeImageInputs{Index: opened[0], Manifest: opened[1], Config: opened[2]})
	if err != nil {
		return fail(err)
	}
	if ctx.Err() != nil {
		return fail(ctx.Err())
	}
	view := struct {
		State                  string `json:"state"`
		PinnedVersion          string `json:"pinned_version"`
		VersionSource          string `json:"version_source"`
		Repository             string `json:"repository"`
		OS                     string `json:"os"`
		Architecture           string `json:"architecture"`
		IndexSHA256            string `json:"index_sha256"`
		IndexSize              int64  `json:"index_size"`
		ManifestSHA256         string `json:"manifest_sha256"`
		ConfigSHA256           string `json:"config_sha256"`
		ImageReference         string `json:"image_reference"`
		LayerCount             int    `json:"layer_count"`
		LayersVerified         bool   `json:"layers_verified"`
		RuntimeVersionVerified bool   `json:"runtime_version_verified"`
	}{State: "image_metadata_verified", PinnedVersion: pin.Version, VersionSource: "digest_bound_config_label", Repository: pin.Repository, OS: "linux", Architecture: pin.Architecture, IndexSHA256: pin.IndexSHA256, IndexSize: pin.IndexSize, ManifestSHA256: result.ManifestSHA256, ConfigSHA256: result.ConfigSHA256, ImageReference: result.Reference(), LayerCount: result.LayerCount}
	if json.NewEncoder(out).Encode(view) != nil {
		return fail(domain.ErrProbeImageUnavailable)
	}
	return 0
}
