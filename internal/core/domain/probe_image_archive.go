package domain

import "errors"

// MaxProbeImageArchiveBytes bounds the uncompressed selected-layout tar,
// including bounded metadata, regular headers, padding and end blocks.
const MaxProbeImageArchiveBytes int64 = MaxProbeImageContentBytes + 3*MaxProbeImageMetadataSize + (MaxProbeImageLayers+4)*1024 + 1024

// Archive errors never include private paths, tar data or storage diagnostics.
var (
	ErrProbeImageArchiveStorage   = errors.New("probe image archive storage unavailable")
	ErrProbeImageArchiveClosed    = errors.New("probe image archive closed")
	ErrProbeImageArchiveBusy      = errors.New("probe image archive reader already open")
	ErrProbeImageArchiveIntegrity = errors.New("probe image archive integrity mismatch")
)

// ProbeImageArchiveDescriptor distinguishes original content trust from the
// generated single-target layout root and observed tar bytes. This is not a
// transferable inclusion proof, importer/runtime attestation or remote authority.
type ProbeImageArchiveDescriptor struct {
	Content           ProbeImageContentVerification
	LayoutIndexSHA256 string
	LayoutIndexSize   int64
	ArchiveSHA256     string
	ArchiveSize       int64
}

// Valid checks bounded value shape, not independent evidence of pack execution.
func (d ProbeImageArchiveDescriptor) Valid() bool {
	if !d.Content.Valid() || d.LayoutIndexSize <= 0 || d.LayoutIndexSize > MaxProbeImageMetadataSize ||
		d.ArchiveSize < 1024 || d.ArchiveSize > MaxProbeImageArchiveBytes || d.ArchiveSize%512 != 0 {
		return false
	}
	pin := d.Content.Selection.Pin
	p := ProbeBinaryPin{Version: pin.Version, Architecture: pin.Architecture, Size: d.LayoutIndexSize, SHA256: d.LayoutIndexSHA256}
	if !p.Valid() {
		return false
	}
	p.SHA256 = d.ArchiveSHA256
	return p.Valid()
}
