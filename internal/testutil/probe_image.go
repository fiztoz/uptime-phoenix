// Package testutil contains deterministic test fixtures, never runtime authority.
package testutil

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeImageFixture binds actual JSON bytes through an index/manifest/config chain.
// Layer bytes are deliberately absent: this fixture proves metadata, not an image.
type ProbeImageFixture struct {
	Pin                                    domain.ProbeImagePin
	Index, Manifest, Config                map[string]any
	IndexBytes, ManifestBytes, ConfigBytes []byte
}

// ImageSHA256 returns canonical SHA-256 for exact fixture bytes.
func ImageSHA256(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

func imageJSON(t testing.TB, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// NewProbeImageFixture creates release-shaped OCI or Docker schema-2 metadata.
func NewProbeImageFixture(t testing.TB, arch string, docker bool) *ProbeImageFixture {
	t.Helper()
	indexMedia := "application/vnd.oci.image.index.v1+json"
	manifestMedia := "application/vnd.oci.image.manifest.v1+json"
	configMedia := "application/vnd.oci.image.config.v1+json"
	layerMedia := "application/vnd.oci.image.layer.v1.tar+gzip"
	if docker {
		indexMedia = "application/vnd.docker.distribution.manifest.list.v2+json"
		manifestMedia = "application/vnd.docker.distribution.manifest.v2+json"
		configMedia = "application/vnd.docker.container.image.v1+json"
		layerMedia = "application/vnd.docker.image.rootfs.diff.tar.gzip"
	}
	digest := "sha256:" + ImageSHA256([]byte("layer-content-not-verified"))
	f := &ProbeImageFixture{Pin: domain.ProbeImagePin{Repository: "ghcr.io/example/uptime-phoenix-probe", Version: "0.4.5", Architecture: arch}}
	f.Config = map[string]any{"architecture": arch, "os": "linux", "config": map[string]any{"User": "65532", "Entrypoint": []any{"/uptime-phoenix-probe"}, "Labels": map[string]any{"org.opencontainers.image.title": "Uptime Phoenix Probe", "org.opencontainers.image.source": "https://github.com/fiztoz/uptime-phoenix", "org.opencontainers.image.version": f.Pin.Version}, "Env": []any{"FIXTURE_VALUE=sensitive-config-marker"}}, "rootfs": map[string]any{"type": "layers", "diff_ids": []any{digest}}, "history": []any{map[string]any{"created_by": "sensitive-history-marker"}}}
	f.Manifest = map[string]any{"schemaVersion": 2, "mediaType": manifestMedia, "config": map[string]any{"mediaType": configMedia}, "layers": []any{map[string]any{"mediaType": layerMedia, "digest": digest, "size": 99}}}
	f.Index = map[string]any{"schemaVersion": 2, "mediaType": indexMedia, "manifests": []any{map[string]any{"mediaType": manifestMedia, "platform": map[string]any{"os": "linux", "architecture": arch}}}}
	f.SetConfigBytes(t, imageJSON(t, f.Config))
	return f
}

// SetConfigBytes binds arbitrary config bytes (including deliberately invalid JSON).
func (f *ProbeImageFixture) SetConfigBytes(t testing.TB, b []byte) {
	t.Helper()
	f.ConfigBytes = append([]byte(nil), b...)
	d := f.Manifest["config"].(map[string]any)
	d["digest"] = "sha256:" + ImageSHA256(b)
	d["size"] = len(b)
	f.SetManifestBytes(t, imageJSON(t, f.Manifest))
}

// SetManifestBytes binds arbitrary manifest bytes into the selected index descriptor.
func (f *ProbeImageFixture) SetManifestBytes(t testing.TB, b []byte) {
	t.Helper()
	f.ManifestBytes = append([]byte(nil), b...)
	d := f.Index["manifests"].([]any)[0].(map[string]any)
	d["digest"] = "sha256:" + ImageSHA256(b)
	d["size"] = len(b)
	f.SetIndexBytes(imageJSON(t, f.Index))
}

// SetIndexBytes replaces the explicit trusted index pin with exact supplied bytes.
func (f *ProbeImageFixture) SetIndexBytes(b []byte) {
	f.IndexBytes = append([]byte(nil), b...)
	f.Pin.IndexSHA256 = ImageSHA256(b)
	f.Pin.IndexSize = int64(len(b))
}

// ChangeConfig updates config bytes and all parent digest/size descriptors.
func (f *ProbeImageFixture) ChangeConfig(t testing.TB, fn func(map[string]any)) {
	t.Helper()
	fn(f.Config)
	f.SetConfigBytes(t, imageJSON(t, f.Config))
}

// ChangeManifest updates manifest bytes and its parent index descriptor.
func (f *ProbeImageFixture) ChangeManifest(t testing.TB, fn func(map[string]any)) {
	t.Helper()
	fn(f.Manifest)
	f.SetManifestBytes(t, imageJSON(t, f.Manifest))
}

// ChangeIndex updates index bytes and the out-of-band pin.
func (f *ProbeImageFixture) ChangeIndex(t testing.TB, fn func(map[string]any)) {
	t.Helper()
	fn(f.Index)
	f.SetIndexBytes(imageJSON(t, f.Index))
}
