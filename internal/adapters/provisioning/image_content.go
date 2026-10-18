package provisioning

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
	"io"
	"sync"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type probeImageContentLimits struct{ layer, total, inflatedLayer, inflatedTotal int64 }

// ProbeImageContentVerifier checks observed layer and uncompressed digests only.
// It neither interprets tar entries nor retains, extracts, loads or executes bytes.
type ProbeImageContentVerifier struct{ limits probeImageContentLimits }

// NewProbeImageContentVerifier constructs the fixed-budget offline verifier.
func NewProbeImageContentVerifier() *ProbeImageContentVerifier {
	return &ProbeImageContentVerifier{limits: probeImageContentLimits{
		layer: domain.MaxProbeImageLayerBytes, total: domain.MaxProbeImageContentBytes,
		inflatedLayer: domain.MaxProbeImageLayerUncompressedBytes, inflatedTotal: domain.MaxProbeImageContentUncompressedBytes,
	}}
}

// VerifyImageContent owns all distinct streams, including failed/canceled input.
func (p *ProbeImageContentVerifier) VerifyImageContent(ctx context.Context, pin domain.ProbeImagePin, in ports.ProbeImageContentInputs) (result *domain.ProbeImageContentVerification, err error) {
	layers := append([]io.ReadCloser(nil), in.Layers...)
	var once sync.Once
	closeAll := func() {
		once.Do(func() {
			for _, r := range append([]io.ReadCloser{in.Metadata.Index, in.Metadata.Manifest, in.Metadata.Config}, layers...) {
				if r != nil {
					_ = r.Close()
				}
			}
		})
	}
	defer closeAll()
	if ctx == nil || !pin.Valid() || in.Metadata.Index == nil || in.Metadata.Manifest == nil || in.Metadata.Config == nil || len(layers) < 1 || len(layers) > domain.MaxProbeImageLayers {
		return nil, domain.ErrProbeImageInput
	}
	for _, r := range layers {
		if r == nil {
			return nil, domain.ErrProbeImageInput
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	joined := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(joined); closeAll() })
	defer func() {
		closeAll()
		if !stop() {
			<-joined
		}
		if ctx.Err() != nil {
			result = nil
			err = ctx.Err()
		}
	}()
	index, err := readProbeImageBlob(ctx, in.Metadata.Index, pin.IndexSize, pin.IndexSHA256)
	if err != nil {
		return nil, err
	}
	selected, err := selectProbeImage(ctx, index, pin.Architecture)
	if err != nil {
		return nil, err
	}
	manifest, err := readProbeImageBlob(ctx, in.Metadata.Manifest, selected.size, selected.sha)
	if err != nil {
		return nil, err
	}
	configDesc, descriptors, err := probeImageManifest(ctx, manifest, selected.media)
	if err != nil {
		return nil, err
	}
	config, err := readProbeImageBlob(ctx, in.Metadata.Config, configDesc.size, configDesc.sha)
	if err != nil {
		return nil, err
	}
	if err = validateProbeImageConfig(ctx, config, pin, len(descriptors)); err != nil {
		return nil, err
	}
	if len(descriptors) != len(layers) {
		return nil, domain.ErrProbeImageInput
	}
	// Validate the entire declared chain before touching any layer, including
	// unsupported encodings later in the list. Metadata-only zstd stays valid.
	var declared int64
	for _, d := range descriptors {
		if _, ok := probeImageLayerGzip(d.media); !ok {
			return nil, domain.ErrProbeImageContentEncoding
		}
		if d.size > p.limits.layer || d.size > p.limits.total-declared {
			return nil, domain.ErrProbeImageContentLimit
		}
		declared += d.size
	}
	root, err := probeImageObjectFrom(ctx, config, "rootfs")
	if err != nil {
		return nil, err
	}
	fsObject, err := probeImageObjectFrom(ctx, root["rootfs"], "diff_ids")
	if err != nil {
		return nil, err
	}
	diffs, ok := fsObject.array("diff_ids")
	if !ok || len(diffs) != len(descriptors) {
		return nil, domain.ErrProbeImageMetadata
	}
	verification := &domain.ProbeImageContentVerification{Selection: domain.ProbeImageSelection{Pin: pin, ManifestSHA256: selected.sha, ConfigSHA256: configDesc.sha, LayerCount: len(descriptors)}}
	for i, d := range descriptors {
		var diff string
		if json.Unmarshal(diffs[i], &diff) != nil {
			return nil, domain.ErrProbeImageMetadata
		}
		sha, ok := probeImageSHA256(diff)
		if !ok {
			return nil, domain.ErrProbeImageMetadata
		}
		compressed, _ := probeImageLayerGzip(d.media)
		budget := min(p.limits.inflatedLayer, p.limits.inflatedTotal-verification.UncompressedBytes)
		if budget < 0 {
			return nil, domain.ErrProbeImageContentLimit
		}
		n, err := verifyProbeImageLayer(ctx, layers[i], d, sha, compressed, budget)
		if err != nil {
			return nil, err
		}
		verification.LayerBytes += d.size
		verification.UncompressedBytes += n
	}
	return verification, nil
}

func probeImageLayerGzip(media string) (compressed, supported bool) {
	switch media {
	case "application/vnd.oci.image.layer.v1.tar", "application/vnd.docker.image.rootfs.diff.tar":
		return false, true
	case "application/vnd.oci.image.layer.v1.tar+gzip", "application/vnd.docker.image.rootfs.diff.tar.gzip":
		return true, true
	default:
		return false, false
	}
}

// probeImageContentReader observes exact bounded compressed bytes and keeps
// source errors separate from codec errors, without exposing either diagnostic.
type probeImageContentReader struct {
	r         io.Reader
	digest    hash.Hash
	count     int64
	sourceErr error
}

func (r *probeImageContentReader) Read(b []byte) (int, error) {
	n, err := r.r.Read(b)
	if n > 0 {
		_, _ = r.digest.Write(b[:n])
		r.count += int64(n)
	}
	if err != nil && err != io.EOF {
		r.sourceErr = err
	}
	return n, err
}

func verifyProbeImageLayer(ctx context.Context, source io.Reader, descriptor probeImageDescriptor, diff string, compressed bool, budget int64) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	encoded := &probeImageContentReader{r: io.LimitReader(probeImageContextReader{ctx: ctx, r: source}, descriptor.size+1), digest: sha256.New()}
	var decoded io.Reader = encoded
	var g *gzip.Reader
	var decodeErr error
	if compressed {
		g, decodeErr = gzip.NewReader(encoded)
		if decodeErr == nil {
			decoded = g
			defer func() { _ = g.Close() }()
		}
	}
	unpacked := sha256.New()
	var count int64
	if decodeErr == nil {
		count, decodeErr = io.CopyBuffer(unpacked, io.LimitReader(probeImageContextReader{ctx: ctx, r: decoded}, budget+1), make([]byte, 32*1024))
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if count > budget {
			return 0, domain.ErrProbeImageContentLimit
		}
	}
	// A codec may stop before the source ends. Finish the bounded compressed
	// hash/size check even after a codec error, never trusting unread trailers.
	_, drainErr := io.CopyBuffer(io.Discard, encoded, make([]byte, 32*1024))
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if encoded.sourceErr != nil || drainErr != nil {
		return 0, domain.ErrProbeImageUnavailable
	}
	if encoded.count != descriptor.size {
		return 0, domain.ErrProbeImageSize
	}
	if hex.EncodeToString(encoded.digest.Sum(nil)) != descriptor.sha {
		return 0, domain.ErrProbeImageDigest
	}
	if decodeErr != nil {
		return 0, domain.ErrProbeImageContentEncoding
	}
	if hex.EncodeToString(unpacked.Sum(nil)) != diff {
		return 0, domain.ErrProbeImageContentDiffID
	}
	return count, nil
}

var _ ports.ProbeImageContentVerifier = (*ProbeImageContentVerifier)(nil)
