package domain

import "errors"

// Content verification is deliberately bounded below the metadata-only 16 GiB
// descriptor allowance. These are fixed limits, not operator-tunable globals.
const (
	MaxProbeImageLayerBytes               int64 = 128 << 20
	MaxProbeImageContentBytes             int64 = 512 << 20
	MaxProbeImageLayerUncompressedBytes   int64 = 512 << 20
	MaxProbeImageContentUncompressedBytes int64 = 2 << 30
)

// Content errors are fixed; codec/reader diagnostics may contain private data.
var (
	ErrProbeImageContentLimit    = errors.New("probe image content exceeds supported limits")
	ErrProbeImageContentEncoding = errors.New("unsupported or invalid probe image layer encoding")
	ErrProbeImageContentDiffID   = errors.New("probe image uncompressed digest mismatch")
)

// ProbeImageContentVerification binds observed layer bytes and diff IDs to the
// selected metadata. It retains no bytes and proves no tar/rootfs/runtime safety,
// publisher authenticity, source immutability after return or remote authority.
type ProbeImageContentVerification struct {
	Selection         ProbeImageSelection
	LayerBytes        int64
	UncompressedBytes int64
}

// Valid checks result shape, not independent proof of a verifier's execution.
func (v ProbeImageContentVerification) Valid() bool {
	return v.Selection.Valid() && v.LayerBytes >= int64(v.Selection.LayerCount) &&
		v.LayerBytes <= min(MaxProbeImageContentBytes, MaxProbeImageLayerBytes*int64(v.Selection.LayerCount)) && v.UncompressedBytes >= 0 &&
		v.UncompressedBytes <= min(MaxProbeImageContentUncompressedBytes, MaxProbeImageLayerUncompressedBytes*int64(v.Selection.LayerCount))
}
