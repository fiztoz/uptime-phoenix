package testutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"testing"
)

// ProbeImageContentFixture binds genuine small tar/gzip byte streams to synthetic
// metadata. It is not an actually built/published image or executable proof.
type ProbeImageContentFixture struct {
	*ProbeImageFixture
	Layers       [][]byte
	Uncompressed [][]byte
	Gzip         bool
	Docker       bool
}

// NewProbeImageContentFixture creates ordered layers for either supported schema.
func NewProbeImageContentFixture(t testing.TB, arch string, docker, compressed bool, count int) *ProbeImageContentFixture {
	t.Helper()
	f := &ProbeImageContentFixture{ProbeImageFixture: NewProbeImageFixture(t, arch, docker), Gzip: compressed, Docker: docker}
	for i := 0; i < count; i++ {
		var raw bytes.Buffer
		w := tar.NewWriter(&raw)
		data := []byte(fmt.Sprintf("synthetic layer %d\n", i))
		if err := w.WriteHeader(&tar.Header{Name: fmt.Sprintf("fixture-%d", i), Mode: 0o444, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		uncompressed := append([]byte(nil), raw.Bytes()...)
		layer := uncompressed
		if compressed {
			var b bytes.Buffer
			g := gzip.NewWriter(&b)
			if _, err := g.Write(uncompressed); err != nil {
				t.Fatal(err)
			}
			if err := g.Close(); err != nil {
				t.Fatal(err)
			}
			layer = append([]byte(nil), b.Bytes()...)
		}
		f.Uncompressed = append(f.Uncompressed, uncompressed)
		f.Layers = append(f.Layers, layer)
	}
	f.RebindContent(t)
	return f
}

// RebindContent computes the layer descriptors/diff IDs and every parent digest.
// Tests may deliberately supply invalid codec bytes; hash binding remains real.
func (f *ProbeImageContentFixture) RebindContent(t testing.TB) {
	t.Helper()
	media := "application/vnd.oci.image.layer.v1.tar"
	if f.Gzip {
		media += "+gzip"
	}
	if f.Docker {
		media = "application/vnd.docker.image.rootfs.diff.tar"
		if f.Gzip {
			media += ".gzip"
		}
	}
	layers := make([]any, len(f.Layers))
	diffs := make([]any, len(f.Uncompressed))
	for i, b := range f.Layers {
		layers[i] = map[string]any{"mediaType": media, "digest": "sha256:" + ImageSHA256(b), "size": len(b)}
	}
	for i, b := range f.Uncompressed {
		diffs[i] = "sha256:" + ImageSHA256(b)
	}
	f.Manifest["layers"] = layers
	f.Config["rootfs"].(map[string]any)["diff_ids"] = diffs
	f.SetConfigBytes(t, imageJSON(t, f.Config))
}
