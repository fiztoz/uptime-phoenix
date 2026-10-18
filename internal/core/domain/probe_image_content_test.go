package domain_test

import (
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/testutil"
)

func TestProbeImageContentVerificationShape(t *testing.T) {
	f := testutil.NewProbeImageContentFixture(t, "amd64", false, true, 1)
	base := domain.ProbeImageContentVerification{Selection: domain.ProbeImageSelection{Pin: f.Pin, ManifestSHA256: testutil.ImageSHA256(f.ManifestBytes), ConfigSHA256: testutil.ImageSHA256(f.ConfigBytes), LayerCount: 1}, LayerBytes: int64(len(f.Layers[0])), UncompressedBytes: int64(len(f.Uncompressed[0]))}
	if !base.Valid() {
		t.Fatal("valid observed content shape refused")
	}
	for _, tc := range []struct {
		name   string
		change func(*domain.ProbeImageContentVerification)
	}{
		{"pin", func(v *domain.ProbeImageContentVerification) { v.Selection.Pin.Version = "" }},
		{"compressed_zero", func(v *domain.ProbeImageContentVerification) { v.LayerBytes = 0 }},
		{"compressed_per_layer", func(v *domain.ProbeImageContentVerification) { v.LayerBytes = domain.MaxProbeImageLayerBytes + 1 }},
		{"compressed_total", func(v *domain.ProbeImageContentVerification) {
			v.Selection.LayerCount = 128
			v.LayerBytes = domain.MaxProbeImageContentBytes + 1
		}},
		{"uncompressed_negative", func(v *domain.ProbeImageContentVerification) { v.UncompressedBytes = -1 }},
		{"uncompressed_per_layer", func(v *domain.ProbeImageContentVerification) {
			v.UncompressedBytes = domain.MaxProbeImageLayerUncompressedBytes + 1
		}},
		{"uncompressed_total", func(v *domain.ProbeImageContentVerification) {
			v.Selection.LayerCount = 128
			v.UncompressedBytes = domain.MaxProbeImageContentUncompressedBytes + 1
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := base
			tc.change(&v)
			if v.Valid() {
				t.Fatal("invalid content evidence shape accepted")
			}
		})
	}
}
