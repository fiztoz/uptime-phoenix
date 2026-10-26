package provisioning

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/testutil"
)

type contentStream struct {
	r               io.Reader
	reads, closed   atomic.Int32
	bytes           atomic.Int64
	onRead, onClose func()
}

func (r *contentStream) Read(p []byte) (int, error) {
	r.reads.Add(1)
	if r.onRead != nil {
		r.onRead()
	}
	n, err := r.r.Read(p)
	r.bytes.Add(int64(n))
	return n, err
}
func (r *contentStream) Close() error {
	r.closed.Add(1)
	if r.onClose != nil {
		r.onClose()
	}
	return nil
}
func contentInputs(f *testutil.ProbeImageContentFixture) (ports.ProbeImageContentInputs, []*contentStream) {
	var all []*contentStream
	add := func(b []byte) *contentStream {
		r := &contentStream{r: bytes.NewReader(b)}
		all = append(all, r)
		return r
	}
	in := ports.ProbeImageContentInputs{Metadata: ports.ProbeImageInputs{Index: add(f.IndexBytes), Manifest: add(f.ManifestBytes), Config: add(f.ConfigBytes)}}
	for _, b := range f.Layers {
		in.Layers = append(in.Layers, add(b))
	}
	return in, all
}
func contentClosed(t *testing.T, all []*contentStream) {
	t.Helper()
	for i, r := range all {
		if n := r.closed.Load(); n != 1 {
			t.Fatalf("owned stream %d closed %d times, want exactly once", i, n)
		}
	}
}
func contentNoLayersRead(t *testing.T, all []*contentStream) {
	t.Helper()
	for i, r := range all[3:] {
		if r.reads.Load() != 0 {
			t.Fatalf("layer %d read before metadata/limits accepted", i)
		}
	}
}

func TestProbeImageContentAdapterValid(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		for _, docker := range []bool{false, true} {
			for _, compressed := range []bool{false, true} {
				name := arch + "/oci/raw"
				if docker {
					name = arch + "/docker/raw"
				}
				if compressed {
					name += "-gzip"
				}
				t.Run(name, func(t *testing.T) {
					f := testutil.NewProbeImageContentFixture(t, arch, docker, compressed, 2)
					in, all := contentInputs(f)
					got, err := NewProbeImageContentVerifier().VerifyImageContent(t.Context(), f.Pin, in)
					if err != nil {
						t.Fatal(err)
					}
					if got == nil || !got.Valid() || got.Selection.Pin != f.Pin || got.Selection.LayerCount != 2 {
						t.Fatal("lost exact content selection")
					}
					if got.Selection.ManifestSHA256 != testutil.ImageSHA256(f.ManifestBytes) || got.Selection.ConfigSHA256 != testutil.ImageSHA256(f.ConfigBytes) {
						t.Fatal("content detached from metadata")
					}
					if got.LayerBytes != int64(len(f.Layers[0])+len(f.Layers[1])) || got.UncompressedBytes != int64(len(f.Uncompressed[0])+len(f.Uncompressed[1])) {
						t.Fatal("wrong observed byte totals")
					}
					contentClosed(t, all)
				})
			}
		}
	}
}
func TestProbeImageContentAdapterRefusals(t *testing.T) {
	cases := []struct {
		name   string
		change func(*testutil.ProbeImageContentFixture)
		want   error
		before bool
	}{
		{"index_digest", func(f *testutil.ProbeImageContentFixture) { f.Pin.IndexSHA256 = strings.Repeat("0", 64) }, domain.ErrProbeImageDigest, true},
		{"manifest_digest", func(f *testutil.ProbeImageContentFixture) { f.ManifestBytes[0] = '!' }, domain.ErrProbeImageDigest, true},
		{"config_digest", func(f *testutil.ProbeImageContentFixture) { f.ConfigBytes[0] = '!' }, domain.ErrProbeImageDigest, true},
		// MTIME changes compressed identity without changing decoded bytes or
		// the gzip checksum, so another guard cannot mask a missing hash check.
		{"compressed_digest", func(f *testutil.ProbeImageContentFixture) { f.Layers[0][4] ^= 1 }, domain.ErrProbeImageDigest, false},
		{"declared_size", func(f *testutil.ProbeImageContentFixture) {
			f.ChangeManifest(t, func(o map[string]any) { o["layers"].([]any)[0].(map[string]any)["size"] = len(f.Layers[0]) + 1 })
		}, domain.ErrProbeImageSize, false},
		{"compressed_short", func(f *testutil.ProbeImageContentFixture) { f.Layers[0] = f.Layers[0][:len(f.Layers[0])-1] }, domain.ErrProbeImageSize, false},
		{"compressed_extra", func(f *testutil.ProbeImageContentFixture) { f.Layers[0] = append(f.Layers[0], 1) }, domain.ErrProbeImageSize, false},
		{"layer_order", func(f *testutil.ProbeImageContentFixture) {
			f.Layers[0], f.Layers[1] = f.Layers[1], f.Layers[0]
			// Keep the original ordered digests but rebind actual sizes, so a
			// size refusal cannot mask the load-bearing digest-order assertion.
			f.ChangeManifest(t, func(o map[string]any) {
				for i, raw := range o["layers"].([]any) {
					raw.(map[string]any)["size"] = len(f.Layers[i])
				}
			})
		}, domain.ErrProbeImageDigest, false},
		{"diff_id", func(f *testutil.ProbeImageContentFixture) {
			f.ChangeConfig(t, func(o map[string]any) {
				o["rootfs"].(map[string]any)["diff_ids"].([]any)[0] = "sha256:" + strings.Repeat("0", 64)
			})
		}, domain.ErrProbeImageContentDiffID, false},
		{"gzip_truncated_rebound", func(f *testutil.ProbeImageContentFixture) {
			f.Layers[0] = f.Layers[0][:len(f.Layers[0])-1]
			f.RebindContent(t)
		}, domain.ErrProbeImageContentEncoding, false},
		{"gzip_checksum_rebound", func(f *testutil.ProbeImageContentFixture) { f.Layers[0][len(f.Layers[0])-8] ^= 1; f.RebindContent(t) }, domain.ErrProbeImageContentEncoding, false},
		{"gzip_trailing_rebound", func(f *testutil.ProbeImageContentFixture) {
			f.Layers[0] = append(f.Layers[0], []byte("invalid-trailer")...)
			f.RebindContent(t)
		}, domain.ErrProbeImageContentEncoding, false},
		{"later_zstd_before_reads", func(f *testutil.ProbeImageContentFixture) {
			f.ChangeManifest(t, func(o map[string]any) {
				o["layers"].([]any)[1].(map[string]any)["mediaType"] = "application/vnd.oci.image.layer.v1.tar+zstd"
			})
		}, domain.ErrProbeImageContentEncoding, true},
		{"declared_layer_limit", func(f *testutil.ProbeImageContentFixture) {
			f.ChangeManifest(t, func(o map[string]any) {
				o["layers"].([]any)[1].(map[string]any)["size"] = domain.MaxProbeImageLayerBytes + 1
			})
		}, domain.ErrProbeImageContentLimit, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := testutil.NewProbeImageContentFixture(t, "amd64", false, true, 2)
			tc.change(f)
			in, all := contentInputs(f)
			got, err := NewProbeImageContentVerifier().VerifyImageContent(t.Context(), f.Pin, in)
			if got != nil || err != tc.want {
				t.Fatalf("content refusal = %v/%v, want %v", got, err, tc.want)
			}
			contentClosed(t, all)
			if tc.before {
				contentNoLayersRead(t, all)
			}
		})
	}
}
func TestProbeImageContentAdapterBudgets(t *testing.T) {
	for _, kind := range []string{"layer", "total", "inflated_layer", "inflated_total"} {
		for _, short := range []bool{false, true} {
			name := kind + "/exact"
			if short {
				name = kind + "/one_over"
			}
			t.Run(name, func(t *testing.T) {
				f := testutil.NewProbeImageContentFixture(t, "arm64", true, true, 2)
				v := NewProbeImageContentVerifier()
				n := int64(max(len(f.Layers[0]), len(f.Layers[1])))
				raw := int64(len(f.Uncompressed[0]))
				subtract := int64(0)
				if short {
					subtract = 1
				}
				switch kind {
				case "layer":
					v.limits.layer = n - subtract
				case "total":
					v.limits.total = int64(len(f.Layers[0])+len(f.Layers[1])) - subtract
				case "inflated_layer":
					v.limits.inflatedLayer = raw - subtract
				case "inflated_total":
					v.limits.inflatedTotal = 2*raw - subtract
				}
				in, all := contentInputs(f)
				got, err := v.VerifyImageContent(t.Context(), f.Pin, in)
				contentClosed(t, all)
				if short {
					if got != nil || err != domain.ErrProbeImageContentLimit {
						t.Fatalf("budget %s not enforced: %v/%v", kind, got, err)
					}
					if kind == "layer" || kind == "total" {
						contentNoLayersRead(t, all)
					}
				} else if err != nil || got == nil {
					t.Fatalf("exact %s boundary rejected: %v", kind, err)
				}
			})
		}
	}
}
func TestProbeImageContentAdapterLayerCount(t *testing.T) {
	f := testutil.NewProbeImageContentFixture(t, "amd64", false, true, 2)
	in, all := contentInputs(f)
	in.Layers = append(in.Layers, &contentStream{r: strings.NewReader("extra")})
	all = append(all, in.Layers[2].(*contentStream))
	got, err := NewProbeImageContentVerifier().VerifyImageContent(t.Context(), f.Pin, in)
	if got != nil || err != domain.ErrProbeImageInput {
		t.Fatal("extra stream accepted")
	}
	contentClosed(t, all)
	contentNoLayersRead(t, all)
}
func TestProbeImageContentAdapterOwnsInvalidInputs(t *testing.T) {
	for _, kind := range []string{"nil_ctx", "bad_pin", "nil_layer", "no_layers", "too_many", "nil_metadata"} {
		t.Run(kind, func(t *testing.T) {
			f := testutil.NewProbeImageContentFixture(t, "amd64", false, true, 1)
			in, all := contentInputs(f)
			ctx := t.Context()
			switch kind {
			case "nil_ctx":
				ctx = nil
			case "bad_pin":
				f.Pin.Version = ""
			case "nil_layer":
				in.Layers = append(in.Layers, nil)
			case "no_layers":
				in.Layers = nil
				all = all[:3]
			case "too_many":
				for range 128 {
					r := &contentStream{r: strings.NewReader("extra")}
					in.Layers = append(in.Layers, r)
					all = append(all, r)
				}
			case "nil_metadata":
				in.Metadata.Manifest = nil
				all = append(all[:1], all[2:]...)
			}
			got, err := NewProbeImageContentVerifier().VerifyImageContent(ctx, f.Pin, in)
			if got != nil || err != domain.ErrProbeImageInput {
				t.Fatalf("invalid %s accepted: %v/%v", kind, got, err)
			}
			contentClosed(t, all)
		})
	}
}
func TestProbeImageContentAdapterOpaqueAndMultimember(t *testing.T) {
	t.Run("opaque_bytes_not_rootfs_proof", func(t *testing.T) {
		f := testutil.NewProbeImageContentFixture(t, "amd64", false, false, 1)
		f.Layers[0] = []byte("not a tar archive; must never be extracted")
		f.Uncompressed[0] = f.Layers[0]
		f.RebindContent(t)
		in, all := contentInputs(f)
		got, err := NewProbeImageContentVerifier().VerifyImageContent(t.Context(), f.Pin, in)
		if err != nil || got == nil {
			t.Fatal("opaque digest binding advertised tar validation")
		}
		contentClosed(t, all)
	})
	t.Run("concatenated_gzip_members", func(t *testing.T) {
		f := testutil.NewProbeImageContentFixture(t, "amd64", false, true, 1)
		raw := f.Uncompressed[0]
		var b bytes.Buffer
		for _, part := range [][]byte{raw[:len(raw)/2], raw[len(raw)/2:]} {
			g := gzip.NewWriter(&b)
			if _, err := g.Write(part); err != nil {
				t.Fatal(err)
			}
			if err := g.Close(); err != nil {
				t.Fatal(err)
			}
		}
		f.Layers[0] = b.Bytes()
		f.RebindContent(t)
		in, all := contentInputs(f)
		got, err := NewProbeImageContentVerifier().VerifyImageContent(t.Context(), f.Pin, in)
		if err != nil || got.UncompressedBytes != int64(len(raw)) {
			t.Fatalf("multimember bytes lost: %v", err)
		}
		contentClosed(t, all)
	})
}

type contentErrorReader struct{}

func (contentErrorReader) Read([]byte) (int, error) {
	return 0, errors.New("private-reader-diagnostic")
}
func TestProbeImageContentAdapterReaderSecrecy(t *testing.T) {
	f := testutil.NewProbeImageContentFixture(t, "amd64", false, true, 1)
	in, all := contentInputs(f)
	all[3].r = contentErrorReader{}
	got, err := NewProbeImageContentVerifier().VerifyImageContent(t.Context(), f.Pin, in)
	if got != nil || err != domain.ErrProbeImageUnavailable {
		t.Fatalf("private reader error escaped: %v", err)
	}
	contentClosed(t, all)
}

type contentBlocked struct {
	entered, closed chan struct{}
	once            sync.Once
}

func (r *contentBlocked) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.entered) })
	<-r.closed
	return 0, errors.New("private-blocked-read")
}
func (r *contentBlocked) Close() error { close(r.closed); return nil }
func TestProbeImageContentAdapterCancellation(t *testing.T) {
	for _, where := range []string{"before", "metadata", "layer", "inflate", "close"} {
		t.Run(where, func(t *testing.T) {
			f := testutil.NewProbeImageContentFixture(t, "amd64", false, true, 2)
			in, all := contentInputs(f)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var blocked *contentBlocked
			switch where {
			case "before":
				cancel()
			case "metadata", "layer":
				i := 0
				if where == "layer" {
					i = 3
				}
				blocked = &contentBlocked{entered: make(chan struct{}), closed: make(chan struct{})}
				all[i].r = blocked
				all[i].onClose = func() { _ = blocked.Close() }
			case "inflate":
				all[3].r = &contentOneByteReader{r: bytes.NewReader(f.Layers[0])}
				all[3].onRead = func() {
					if all[3].reads.Load() == 20 {
						cancel()
					}
				}
			case "close":
				all[0].onClose = cancel
			}
			if blocked != nil {
				go func() {
					select {
					case <-blocked.entered:
						cancel()
					case <-ctx.Done():
					}
				}()
			}
			got, err := NewProbeImageContentVerifier().VerifyImageContent(ctx, f.Pin, in)
			if got != nil || err != context.Canceled {
				t.Fatalf("cancellation at %s not dominant: %v/%v", where, got, err)
			}
			contentClosed(t, all)
		})
	}
}

type contentOneByteReader struct{ r io.Reader }

func (r *contentOneByteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return r.r.Read(p)
}

func TestProbeImageContentAdapterClosesCancellationJoin(t *testing.T) {
	f := testutil.NewProbeImageContentFixture(t, "amd64", false, true, 1)
	in, all := contentInputs(f)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	all[3].onRead = func() { cancel(); <-entered }
	all[3].onClose = func() { once.Do(func() { close(entered) }); <-release }
	done := make(chan error, 1)
	go func() { _, err := NewProbeImageContentVerifier().VerifyImageContent(ctx, f.Pin, in); done <- err }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not close layer")
	}
	select {
	case _ = <-done:
		t.Fatal("returned before owned closer joined")
	default:
	}
	close(release)
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("verifier did not join")
	}
	contentClosed(t, all)
}
