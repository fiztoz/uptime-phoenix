package domain

import (
	"strings"
	"testing"
)

func TestProbeImageArchiveDescriptor(t *testing.T) {
	pin := ProbeImagePin{Repository: "ghcr.io/example/probe", Version: "0.4.5", Architecture: "amd64", IndexSHA256: strings.Repeat("a", 64), IndexSize: 512}
	good := ProbeImageArchiveDescriptor{
		Content:           ProbeImageContentVerification{Selection: ProbeImageSelection{Pin: pin, ManifestSHA256: strings.Repeat("b", 64), ConfigSHA256: strings.Repeat("c", 64), LayerCount: 1}, LayerBytes: 100, UncompressedBytes: 2000},
		LayoutIndexSHA256: strings.Repeat("d", 64), LayoutIndexSize: 300,
		ArchiveSHA256: strings.Repeat("e", 64), ArchiveSize: 8192,
	}
	if !good.Valid() {
		t.Fatal("valid descriptor refused")
	}
	for _, tc := range []struct {
		name   string
		change func(*ProbeImageArchiveDescriptor)
	}{
		{"content", func(d *ProbeImageArchiveDescriptor) { d.Content = ProbeImageContentVerification{} }},
		{"index_zero", func(d *ProbeImageArchiveDescriptor) { d.LayoutIndexSize = 0 }},
		{"index_limit", func(d *ProbeImageArchiveDescriptor) { d.LayoutIndexSize = MaxProbeImageMetadataSize + 1 }},
		{"index_hash", func(d *ProbeImageArchiveDescriptor) { d.LayoutIndexSHA256 = strings.Repeat("D", 64) }},
		{"archive_hash", func(d *ProbeImageArchiveDescriptor) { d.ArchiveSHA256 = "private-path" }},
		{"archive_zero", func(d *ProbeImageArchiveDescriptor) { d.ArchiveSize = 0 }},
		{"archive_short", func(d *ProbeImageArchiveDescriptor) { d.ArchiveSize = 512 }},
		{"archive_unaligned", func(d *ProbeImageArchiveDescriptor) { d.ArchiveSize++ }},
		{"archive_limit", func(d *ProbeImageArchiveDescriptor) { d.ArchiveSize = MaxProbeImageArchiveBytes + 512 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := good
			tc.change(&d)
			if d.Valid() {
				t.Fatal("invalid descriptor accepted")
			}
		})
	}
	for _, arch := range []string{"amd64", "arm64"} {
		d := good
		d.Content.Selection.Pin.Architecture = arch
		d.LayoutIndexSize = MaxProbeImageMetadataSize
		d.ArchiveSize = MaxProbeImageArchiveBytes
		if !d.Valid() {
			t.Fatal("exact boundary refused", arch)
		}
		// A canonical source index may already be the generated root. Equality
		// is allowed; separate fields must not invent independent provenance.
		d.LayoutIndexSHA256 = d.Content.Selection.Pin.IndexSHA256
		if !d.Valid() {
			t.Fatal("identical genuine index identities refused")
		}
	}
}
