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

const probeImageContentUsage = `Usage: phoenix-probe-admin verify-image-content --repository HOST/NAME --version VERSION --architecture amd64|arm64 --index PATH --index-sha256 LOWERCASE_HEX --index-size BYTES --manifest PATH --config PATH --layer PATH [--layer PATH ...]
Offline verification of trusted metadata plus ordered compressed layer bytes.
Supply one local layer file per manifest layer, in exact manifest order, 1 to 128 layers.
No registry, daemon, download, extraction or container execution is used.
Verified layer digests and diff IDs are not rootfs safety, publisher authenticity,
runtime version proof, installation, enrollment or config activation.
Content is not retained after return.
`

// probeImageLayerList collects exactly one local layer path per ordered manifest layer.
type probeImageLayerList []string

func (l *probeImageLayerList) String() string { return "" }

func (l *probeImageLayerList) Set(value string) error {
	if value == "" || len(*l) >= domain.MaxProbeImageLayers {
		return domain.ErrProbeImageInput
	}
	*l = append(*l, value)
	return nil
}

// RunProbeImageContentVerification verifies ordered layer digests and bounded diff IDs
// offline before hub startup. It reads metadata and local layer bytes only: no registry,
// daemon, extraction, filesystem write, hub configuration or database is used.
func RunProbeImageContentVerification(ctx context.Context, args []string, out, stderr io.Writer) int {
	flags := flag.NewFlagSet("verify-image-content", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var pin domain.ProbeImagePin
	var indexPath, manifestPath, configPath string
	var layers probeImageLayerList
	flags.StringVar(&pin.Repository, "repository", "", "fully qualified immutable image repository")
	flags.StringVar(&pin.Version, "version", "", "trusted release version without v")
	flags.StringVar(&pin.Architecture, "architecture", "", "linux architecture")
	flags.StringVar(&pin.IndexSHA256, "index-sha256", "", "trusted lowercase index SHA-256")
	flags.Int64Var(&pin.IndexSize, "index-size", 0, "exact index byte length")
	flags.StringVar(&indexPath, "index", "", "local index JSON")
	flags.StringVar(&manifestPath, "manifest", "", "local platform manifest JSON")
	flags.StringVar(&configPath, "config", "", "local image config JSON")
	flags.Var(&layers, "layer", "local layer bytes in exact manifest order")
	if err := flags.Parse(args); errors.Is(err, flag.ErrHelp) {
		_, _ = io.WriteString(out, probeImageContentUsage)
		return 0
	} else if err != nil || flags.NArg() != 0 || !pin.Valid() || indexPath == "" || manifestPath == "" || configPath == "" || len(layers) < 1 || len(layers) > domain.MaxProbeImageLayers {
		_, _ = io.WriteString(stderr, probeImageContentUsage)
		return 2
	}
	fail := func(err error) int {
		message := "Probe image content verification failed"
		for _, safe := range []error{domain.ErrProbeImageInput, domain.ErrProbeImageSize, domain.ErrProbeImageDigest, domain.ErrProbeImageMetadata, domain.ErrProbeImageUnavailable, domain.ErrProbeImageContentLimit, domain.ErrProbeImageContentEncoding, domain.ErrProbeImageContentDiffID} {
			if errors.Is(err, safe) {
				message = safe.Error()
				break
			}
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			message = "Probe image content verification canceled or timed out"
		}
		_, _ = fmt.Fprintln(stderr, message)
		return 1
	}
	if ctx == nil {
		return fail(domain.ErrProbeImageInput)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	opened := make([]io.ReadCloser, 0, 3+len(layers))
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
	for _, path := range layers {
		source, err := provisioning.OpenProbeImageLayerSource(ctx, path)
		if err != nil {
			closeOpened()
			return fail(err)
		}
		opened = append(opened, source)
	}
	service, err := services.NewProbeImageContentService(provisioning.NewProbeImageContentVerifier())
	if err != nil {
		closeOpened()
		return fail(err)
	}
	result, err := service.Verify(ctx, pin, ports.ProbeImageContentInputs{
		Metadata: ports.ProbeImageInputs{Index: opened[0], Manifest: opened[1], Config: opened[2]},
		Layers:   opened[3:],
	})
	if err != nil {
		return fail(err)
	}
	if ctx.Err() != nil {
		return fail(ctx.Err())
	}
	if result == nil || !result.Valid() || result.Selection.Pin != pin {
		return fail(domain.ErrProbeImageMetadata)
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
		LayerBytes             int64  `json:"layer_bytes"`
		UncompressedBytes      int64  `json:"uncompressed_bytes"`
		LayerDigestsVerified   bool   `json:"layer_digests_verified"`
		DiffIDsVerified        bool   `json:"diff_ids_verified"`
		RootfsVerified         bool   `json:"rootfs_verified"`
		RuntimeVersionVerified bool   `json:"runtime_version_verified"`
		ContentRetained        bool   `json:"content_retained"`
	}{
		State:                "image_content_digests_verified",
		PinnedVersion:        pin.Version,
		VersionSource:        "digest_bound_config_label",
		Repository:           pin.Repository,
		OS:                   "linux",
		Architecture:         pin.Architecture,
		IndexSHA256:          pin.IndexSHA256,
		IndexSize:            pin.IndexSize,
		ManifestSHA256:       result.Selection.ManifestSHA256,
		ConfigSHA256:         result.Selection.ConfigSHA256,
		ImageReference:       result.Selection.Reference(),
		LayerCount:           result.Selection.LayerCount,
		LayerBytes:           result.LayerBytes,
		UncompressedBytes:    result.UncompressedBytes,
		LayerDigestsVerified: true,
		DiffIDsVerified:      true,
	}
	if json.NewEncoder(out).Encode(view) != nil {
		return fail(domain.ErrProbeImageUnavailable)
	}
	return 0
}
