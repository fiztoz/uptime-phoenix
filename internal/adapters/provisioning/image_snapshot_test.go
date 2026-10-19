package provisioning

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
	"github.com/fiztoz/uptime-phoenix/internal/testutil"
)

func snapshotPrepare(t *testing.T, f *testutil.ProbeImageContentFixture) (*preparedProbeImage, string) {
	t.Helper()
	base := t.TempDir()
	in, all := contentInputs(f)
	p, err := NewProbeImageSnapshotPreparer(base).PrepareImageSnapshot(t.Context(), f.Pin, in)
	if err != nil {
		t.Fatal(err)
	}
	contentClosed(t, all)
	owned := p.(*preparedProbeImage)
	t.Cleanup(func() { _ = owned.Close(context.Background()) })
	return owned, base
}

func snapshotStreams(in ports.ProbeImageContentInputs) []io.ReadCloser {
	return append([]io.ReadCloser{in.Metadata.Index, in.Metadata.Manifest, in.Metadata.Config}, in.Layers...)
}
func snapshotCloseBundle(in ports.ProbeImageContentInputs) {
	for _, r := range snapshotStreams(in) {
		if r != nil {
			_ = r.Close()
		}
	}
}
func snapshotEmpty(t *testing.T, base string) {
	t.Helper()
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) != 0 {
		t.Fatalf("snapshot cleanup incomplete: %d entries / %v", len(entries), err)
	}
}

func TestProbeImageSnapshotAdapterLifecycle(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		for _, docker := range []bool{false, true} {
			for _, gzip := range []bool{false, true} {
				name := arch + "/oci/raw"
				if docker {
					name = arch + "/docker/raw"
				}
				if gzip {
					name += "-gzip"
				}
				t.Run(name, func(t *testing.T) {
					f := testutil.NewProbeImageContentFixture(t, arch, docker, gzip, 2)
					base := t.TempDir()
					in, all := contentInputs(f)
					svc, err := services.NewProbeImageSnapshotService(NewProbeImageSnapshotPreparer(base))
					if err != nil {
						t.Fatal(err)
					}
					p, err := svc.Prepare(t.Context(), f.Pin, in)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = p.Close(context.Background()) }()
					contentClosed(t, all)
					v := p.Verification()
					if !v.Valid() || v.Selection.Pin != f.Pin || v.Selection.LayerCount != 2 || v.LayerBytes != int64(len(f.Layers[0])+len(f.Layers[1])) {
						t.Fatal("snapshot lost exact verification")
					}
					owned := p.(*preparedProbeImage)
					info, err := os.Stat(owned.dir)
					if err != nil || info.Mode().Perm() != 0700 {
						t.Fatal("directory is not private")
					}
					for _, entry := range owned.files {
						info, e := os.Stat(filepath.Join(owned.dir, entry.name))
						if e != nil || info.Mode().Perm() != 0400 || !info.Mode().IsRegular() {
							t.Fatal("snapshot bytes not sealed")
						}
					}
					want := [][]byte{append([]byte(nil), f.IndexBytes...), append([]byte(nil), f.ManifestBytes...), append([]byte(nil), f.ConfigBytes...), append([]byte(nil), f.Layers[0]...), append([]byte(nil), f.Layers[1]...)}
					// Caller-owned inputs remain mutable but are no longer authoritative.
					for _, b := range [][]byte{f.IndexBytes, f.ManifestBytes, f.ConfigBytes, f.Layers[0], f.Layers[1]} {
						b[0] ^= 1
					}
					for n := 0; n < 2; n++ {
						bundle, e := p.Open(t.Context())
						if e != nil {
							t.Fatal(e)
						}
						if extra, e := p.Open(t.Context()); e != domain.ErrProbeImageSnapshotBusy || len(extra.Layers) != 0 {
							t.Fatal("multiple active bundles accepted")
						}
						for i, r := range snapshotStreams(bundle) {
							got, e := io.ReadAll(r)
							if e != nil || !bytes.Equal(got, want[i]) {
								t.Fatalf("private copy %d changed or reordered: %v", i, e)
							}
							if e = r.Close(); e != nil {
								t.Fatal(e)
							}
						}
					}
					if err := p.Close(nil); err != nil {
						t.Fatal(err)
					}
					snapshotEmpty(t, base)
					if _, e := p.Open(t.Context()); e != domain.ErrProbeImageSnapshotClosed {
						t.Fatal("closed snapshot reopened")
					}
					if e := p.Close(t.Context()); e != nil {
						t.Fatal("close not idempotent")
					}
				})
			}
		}
	}
}

func TestProbeImageSnapshotAdapterRefusals(t *testing.T) {
	cases := []struct {
		name   string
		change func(*testutil.ProbeImageContentFixture)
		want   error
		before bool
	}{
		{"index_digest", func(f *testutil.ProbeImageContentFixture) { f.Pin.IndexSHA256 = strings.Repeat("0", 64) }, domain.ErrProbeImageDigest, true},
		{"config_digest", func(f *testutil.ProbeImageContentFixture) { f.ConfigBytes[0] = '!' }, domain.ErrProbeImageDigest, true},
		{"layer_digest", func(f *testutil.ProbeImageContentFixture) { f.Layers[0][4] ^= 1 }, domain.ErrProbeImageDigest, false},
		{"size", func(f *testutil.ProbeImageContentFixture) { f.Layers[0] = append(f.Layers[0], 1) }, domain.ErrProbeImageSize, false},
		{"codec", func(f *testutil.ProbeImageContentFixture) {
			f.Layers[0] = f.Layers[0][:len(f.Layers[0])-1]
			f.RebindContent(t)
		}, domain.ErrProbeImageContentEncoding, false},
		{"diff_id", func(f *testutil.ProbeImageContentFixture) {
			f.ChangeConfig(t, func(o map[string]any) {
				o["rootfs"].(map[string]any)["diff_ids"].([]any)[0] = "sha256:" + strings.Repeat("0", 64)
			})
		}, domain.ErrProbeImageContentDiffID, false},
		{"later_zstd", func(f *testutil.ProbeImageContentFixture) {
			f.ChangeManifest(t, func(o map[string]any) {
				o["layers"].([]any)[1].(map[string]any)["mediaType"] = "application/vnd.oci.image.layer.v1.tar+zstd"
			})
		}, domain.ErrProbeImageContentEncoding, true},
		{"declared_budget", func(f *testutil.ProbeImageContentFixture) {
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
			base := t.TempDir()
			p, err := NewProbeImageSnapshotPreparer(base).PrepareImageSnapshot(t.Context(), f.Pin, in)
			if p != nil || err != tc.want {
				t.Fatalf("refusal = %v / %v, want %v", p, err, tc.want)
			}
			contentClosed(t, all)
			if tc.before {
				contentNoLayersRead(t, all)
			}
			snapshotEmpty(t, base)
		})
	}
}

func TestProbeImageSnapshotAdapterBudgets(t *testing.T) {
	for _, kind := range []string{"layer", "total", "inflated_layer", "inflated_total"} {
		for _, over := range []bool{false, true} {
			name := kind + "/exact"
			if over {
				name = kind + "/one_over"
			}
			t.Run(name, func(t *testing.T) {
				f := testutil.NewProbeImageContentFixture(t, "arm64", true, true, 2)
				p := NewProbeImageSnapshotPreparer(t.TempDir())
				delta := int64(0)
				if over {
					delta = 1
				}
				switch kind {
				case "layer":
					p.verifier.limits.layer = int64(max(len(f.Layers[0]), len(f.Layers[1]))) - delta
				case "total":
					p.verifier.limits.total = int64(len(f.Layers[0])+len(f.Layers[1])) - delta
				case "inflated_layer":
					p.verifier.limits.inflatedLayer = int64(len(f.Uncompressed[0])) - delta
				case "inflated_total":
					p.verifier.limits.inflatedTotal = int64(len(f.Uncompressed[0])+len(f.Uncompressed[1])) - delta
				}
				in, all := contentInputs(f)
				got, err := p.PrepareImageSnapshot(t.Context(), f.Pin, in)
				contentClosed(t, all)
				if over {
					if got != nil || err != domain.ErrProbeImageContentLimit {
						t.Fatalf("budget not enforced: %v / %v", got, err)
					}
					snapshotEmpty(t, p.baseDir)
				} else {
					if err != nil || got == nil {
						t.Fatalf("exact bound refused: %v", err)
					}
					if e := got.Close(t.Context()); e != nil {
						t.Fatal(e)
					}
				}
			})
		}
	}
}

func TestProbeImageSnapshotCopiedBytesReverified(t *testing.T) {
	f := testutil.NewProbeImageContentFixture(t, "amd64", false, true, 1)
	base := t.TempDir()
	in, all := contentInputs(f)
	var corruptErr error
	all[3].onClose = func() {
		entries, e := os.ReadDir(base)
		if e != nil || len(entries) != 1 {
			corruptErr = errors.New("missing private intake copy")
			return
		}
		file, e := os.OpenFile(filepath.Join(base, entries[0].Name(), "index"), os.O_WRONLY, 0)
		if e != nil {
			corruptErr = e
			return
		}
		_, corruptErr = file.WriteAt([]byte("!"), 0)
		_ = file.Close()
	}
	got, err := NewProbeImageSnapshotPreparer(base).PrepareImageSnapshot(t.Context(), f.Pin, in)
	if corruptErr != nil {
		t.Fatal(corruptErr)
	}
	if got != nil || err != domain.ErrProbeImageDigest {
		if got != nil {
			_ = got.Close(t.Context())
		}
		t.Fatalf("private copied bytes were not reverified: %v / %v", got, err)
	}
	contentClosed(t, all)
	snapshotEmpty(t, base)
}

func TestProbeImageSnapshotCancellationAndRevocation(t *testing.T) {
	t.Run("cancel_during_input_close", func(t *testing.T) {
		f := testutil.NewProbeImageContentFixture(t, "amd64", false, true, 1)
		in, all := contentInputs(f)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		all[3].onClose = cancel
		base := t.TempDir()
		p, err := NewProbeImageSnapshotPreparer(base).PrepareImageSnapshot(ctx, f.Pin, in)
		if p != nil || err != context.Canceled {
			t.Fatalf("canceled input close acknowledged: %v / %v", p, err)
		}
		contentClosed(t, all)
		snapshotEmpty(t, base)
	})
	t.Run("reader_context_and_owner_close", func(t *testing.T) {
		p, base := snapshotPrepare(t, testutil.NewProbeImageContentFixture(t, "amd64", false, true, 1))
		ctx, cancel := context.WithCancel(t.Context())
		in, err := p.Open(ctx)
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		snapshotCloseBundle(in)
		for _, r := range snapshotStreams(in) {
			if _, e := r.Read(make([]byte, 1)); e != context.Canceled {
				t.Fatalf("reader outlived context: %v", e)
			}
		}
		in, err = p.Open(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		canceled, c := context.WithCancel(t.Context())
		c()
		if e := p.Close(canceled); e != nil {
			t.Fatal(e)
		}
		for _, r := range snapshotStreams(in) {
			if _, e := r.Read(make([]byte, 1)); e != domain.ErrProbeImageSnapshotClosed {
				t.Fatalf("reader outlived snapshot: %v", e)
			}
		}
		snapshotEmpty(t, base)
	})
	t.Run("blocked_read_joins_close", func(t *testing.T) {
		f := testutil.NewProbeImageContentFixture(t, "amd64", false, true, 1)
		in, all := contentInputs(f)
		blocked := &snapshotBlockedSource{started: make(chan struct{}), released: make(chan struct{}), closing: make(chan struct{}), finish: make(chan struct{})}
		in.Layers[0] = blocked
		ctx, cancel := context.WithCancel(t.Context())
		base := t.TempDir()
		done := make(chan error, 1)
		go func() { _, e := NewProbeImageSnapshotPreparer(base).PrepareImageSnapshot(ctx, f.Pin, in); done <- e }()
		<-blocked.started
		cancel()
		<-blocked.closing
		select {
		case <-done:
			t.Fatal("preparation returned before source Close joined")
		default:
		}
		close(blocked.finish)
		select {
		case e := <-done:
			if e != context.Canceled {
				t.Fatal(e)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("cancellation did not join")
		}
		contentClosed(t, all[:3])
		snapshotEmpty(t, base)
	})
}

type snapshotBlockedSource struct {
	started, released, closing, finish chan struct{}
	start, close                       sync.Once
}

func (r *snapshotBlockedSource) Read([]byte) (int, error) {
	r.start.Do(func() { close(r.started) })
	<-r.released
	return 0, io.EOF
}
func (r *snapshotBlockedSource) Close() error {
	r.close.Do(func() { close(r.released); close(r.closing); <-r.finish })
	return nil
}

func TestProbeImageSnapshotReplacementAndCleanup(t *testing.T) {
	t.Run("directory_replacement", func(t *testing.T) {
		p, base := snapshotPrepare(t, testutil.NewProbeImageContentFixture(t, "amd64", false, false, 1))
		original := p.dir + "-original"
		foreign := p.dir + "-foreign"
		if e := os.Rename(p.dir, original); e != nil {
			t.Fatal(e)
		}
		if e := os.Mkdir(p.dir, 0700); e != nil {
			t.Fatal(e)
		}
		sentinel := filepath.Join(p.dir, "foreign")
		if e := os.WriteFile(sentinel, []byte("preserve"), 0600); e != nil {
			t.Fatal(e)
		}
		if _, e := p.Open(t.Context()); e != domain.ErrProbeImageSnapshotStorage {
			t.Fatal("replacement adopted")
		}
		if e := p.Close(t.Context()); e != domain.ErrProbeImageSnapshotStorage {
			t.Fatal("foreign directory cleanup accepted")
		}
		if b, e := os.ReadFile(sentinel); e != nil || string(b) != "preserve" {
			t.Fatal("foreign directory modified")
		}
		if e := os.Rename(p.dir, foreign); e != nil {
			t.Fatal(e)
		}
		if e := os.Rename(original, p.dir); e != nil {
			t.Fatal(e)
		}
		if e := p.Close(t.Context()); e != nil {
			t.Fatal("cleanup retry failed", e)
		}
		if e := os.Remove(filepath.Join(foreign, "foreign")); e != nil {
			t.Fatal(e)
		}
		if e := os.Remove(foreign); e != nil {
			t.Fatal(e)
		}
		snapshotEmpty(t, base)
	})
	t.Run("partial_open_file_replacement", func(t *testing.T) {
		p, _ := snapshotPrepare(t, testutil.NewProbeImageContentFixture(t, "amd64", false, true, 2))
		name := filepath.Join(p.dir, p.files[len(p.files)-1].name)
		save := name + "-original"
		if e := os.Rename(name, save); e != nil {
			t.Fatal(e)
		}
		if e := os.Symlink(save, name); e != nil {
			t.Fatal(e)
		}
		if in, e := p.Open(t.Context()); e != domain.ErrProbeImageSnapshotStorage || len(in.Layers) != 0 {
			t.Fatal("replacement or partial bundle returned")
		}
		p.mu.Lock()
		n := len(p.readers)
		p.mu.Unlock()
		if n != 0 {
			t.Fatal("partial open leaked registered readers")
		}
		if e := p.Close(t.Context()); e != domain.ErrProbeImageSnapshotStorage {
			t.Fatal("replacement cleanup accepted")
		}
		if _, e := os.Stat(save); e != nil {
			t.Fatal("original/foreign files deleted")
		}
		if e := os.Remove(name); e != nil {
			t.Fatal(e)
		}
		if e := os.Rename(save, name); e != nil {
			t.Fatal(e)
		}
		if e := p.Close(t.Context()); e != nil {
			t.Fatal(e)
		}
	})
	t.Run("unknown_file_no_partial_deletion", func(t *testing.T) {
		p, _ := snapshotPrepare(t, testutil.NewProbeImageContentFixture(t, "amd64", false, false, 1))
		foreign := filepath.Join(p.dir, "foreign")
		if e := os.WriteFile(foreign, []byte("preserve"), 0600); e != nil {
			t.Fatal(e)
		}
		if e := p.Close(t.Context()); e != domain.ErrProbeImageSnapshotStorage {
			t.Fatal("unknown file ignored")
		}
		for _, entry := range p.files {
			if _, e := os.Stat(filepath.Join(p.dir, entry.name)); e != nil {
				t.Fatal("cleanup partially deleted before complete graph refusal")
			}
		}
		if e := os.Remove(foreign); e != nil {
			t.Fatal(e)
		}
		if e := p.Close(t.Context()); e != nil {
			t.Fatal(e)
		}
	})
}

type snapshotDynamicHandle struct{ state any }

func (r snapshotDynamicHandle) source() *contentStream {
	if source, ok := r.state.(*contentStream); ok {
		return source
	}
	return r.state.([]*contentStream)[0]
}
func (r snapshotDynamicHandle) Read(b []byte) (int, error) { return r.source().Read(b) }
func (r snapshotDynamicHandle) Close() error               { return r.source().Close() }

func TestProbeImageSnapshotDynamicComparability(t *testing.T) {
	for _, comparable := range []bool{true, false} {
		name := "noncomparable_value"
		if comparable {
			name = "comparable_value"
		}
		t.Run(name, func(t *testing.T) {
			f := testutil.NewProbeImageContentFixture(t, "amd64", false, true, 1)
			in, all := contentInputs(f)
			var state any = []*contentStream{all[0]}
			if comparable {
				state = all[0]
			}
			in.Metadata.Index = snapshotDynamicHandle{state: state}
			base := t.TempDir()
			svc, e := services.NewProbeImageSnapshotService(NewProbeImageSnapshotPreparer(base))
			if e != nil {
				t.Fatal(e)
			}
			p, e := svc.Prepare(t.Context(), f.Pin, in)
			if e != nil {
				t.Fatal(e)
			}
			contentClosed(t, all)
			if e = p.Close(t.Context()); e != nil {
				t.Fatal(e)
			}
			snapshotEmpty(t, base)
		})
	}
}

type snapshotNilSource struct{}

func (*snapshotNilSource) Read([]byte) (int, error) { panic("typed nil reader touched") }
func (*snapshotNilSource) Close() error             { panic("typed nil closer touched") }

type snapshotNilContext struct{ context.Context }
type snapshotErrorReader struct{}

func (snapshotErrorReader) Read([]byte) (int, error) {
	return 0, errors.New("private local reader diagnostic")
}

func TestProbeImageSnapshotAdapterInputAndStorage(t *testing.T) {
	for _, kind := range []string{"nil_context", "typed_nil_context", "typed_nil_layer", "duplicate", "layer_count", "private_reader", "storage_parent"} {
		t.Run(kind, func(t *testing.T) {
			f := testutil.NewProbeImageContentFixture(t, "amd64", false, true, 1)
			in, all := contentInputs(f)
			owned := all
			base := t.TempDir()
			var ctx context.Context = t.Context()
			want := domain.ErrProbeImageInput
			switch kind {
			case "nil_context":
				ctx = nil
			case "typed_nil_context":
				var absent *snapshotNilContext
				ctx = absent
			case "typed_nil_layer":
				var absent *snapshotNilSource
				in.Layers[0] = absent
				owned = all[:3]
			case "duplicate":
				in.Metadata.Config = in.Metadata.Index
				owned = []*contentStream{all[0], all[1], all[3]}
			case "layer_count":
				in.Layers = append(in.Layers, &contentStream{r: strings.NewReader("extra")})
				owned = append(owned, in.Layers[1].(*contentStream))
			case "private_reader":
				all[3].r = snapshotErrorReader{}
				want = domain.ErrProbeImageUnavailable
			case "storage_parent":
				base = filepath.Join(base, "not-directory")
				if e := os.WriteFile(base, []byte("preserve"), 0600); e != nil {
					t.Fatal(e)
				}
				want = domain.ErrProbeImageSnapshotStorage
			}
			p, e := NewProbeImageSnapshotPreparer(base).PrepareImageSnapshot(ctx, f.Pin, in)
			if p != nil || e != want {
				t.Fatalf("input/storage refusal: %v / %v, want %v", p, e, want)
			}
			contentClosed(t, owned)
			if kind != "private_reader" && kind != "layer_count" {
				for _, r := range owned {
					if r.reads.Load() != 0 {
						t.Fatal("refused input performed I/O")
					}
				}
			}
			if kind == "storage_parent" {
				if b, e := os.ReadFile(base); e != nil || string(b) != "preserve" {
					t.Fatal("existing storage parent modified")
				}
			} else {
				snapshotEmpty(t, base)
			}
		})
	}
}

func TestProbeImageSnapshotMaximumBundleAndAutomaticClose(t *testing.T) {
	p, base := snapshotPrepare(t, testutil.NewProbeImageContentFixture(t, "arm64", true, true, domain.MaxProbeImageLayers))
	if _, e := p.Open(nil); e != domain.ErrProbeImageInput {
		t.Fatal("nil open context accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	in, e := p.Open(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if len(snapshotStreams(in)) != 3+domain.MaxProbeImageLayers {
		t.Fatal("lost maximum-size bundle")
	}
	cancel()
	for _, r := range snapshotStreams(in) {
		select {
		case <-r.(*imageSnapshotReader).joined:
		case <-time.After(3 * time.Second):
			t.Fatal("automatic cancellation closure did not join")
		}
	}
	reopened, e := p.Open(t.Context())
	if e != nil {
		t.Fatal("canceled bundle retained busy grant", e)
	}
	snapshotCloseBundle(reopened)
	snapshotCloseBundle(in)
	if e := p.Close(t.Context()); e != nil {
		t.Fatal(e)
	}
	snapshotEmpty(t, base)
}

func TestProbeImageSnapshotCanceledCleanupFailureIsVisible(t *testing.T) {
	for _, service := range []bool{false, true} {
		name := "adapter"
		if service {
			name = "service"
		}
		t.Run(name, func(t *testing.T) {
			f := testutil.NewProbeImageContentFixture(t, "amd64", false, true, 1)
			in, all := contentInputs(f)
			base := t.TempDir()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var hookErr error
			all[3].onClose = func() {
				entries, e := os.ReadDir(base)
				if e != nil || len(entries) != 1 {
					hookErr = errors.New("private directory missing")
					return
				}
				hookErr = os.WriteFile(filepath.Join(base, entries[0].Name(), "unknown"), []byte("preserve"), 0600)
				cancel()
			}
			adapter := NewProbeImageSnapshotPreparer(base)
			var got ports.PreparedProbeImage
			var err error
			if service {
				svc, e := services.NewProbeImageSnapshotService(adapter)
				if e != nil {
					t.Fatal(e)
				}
				got, err = svc.Prepare(ctx, f.Pin, in)
			} else {
				got, err = adapter.PrepareImageSnapshot(ctx, f.Pin, in)
			}
			if hookErr != nil {
				t.Fatal(hookErr)
			}
			if got != nil || err != domain.ErrProbeImageSnapshotStorage {
				t.Fatalf("failed cleanup hidden by cancellation: %v / %v", got, err)
			}
			contentClosed(t, all)
			entries, e := os.ReadDir(base)
			if e != nil || len(entries) != 1 {
				t.Fatal("uncertain cleanup was not preserved")
			}
			b, e := os.ReadFile(filepath.Join(base, entries[0].Name(), "unknown"))
			if e != nil || string(b) != "preserve" {
				t.Fatal("foreign contents removed")
			}
		})
	}
}

func TestProbeImageSnapshotConcurrentOpenClose(t *testing.T) {
	for range 12 {
		p, base := snapshotPrepare(t, testutil.NewProbeImageContentFixture(t, "amd64", false, true, 2))
		var wg sync.WaitGroup
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				in, e := p.Open(ctx)
				if e == nil {
					cancel()
					snapshotCloseBundle(in)
				} else if e != domain.ErrProbeImageSnapshotBusy && e != domain.ErrProbeImageSnapshotClosed {
					t.Errorf("unexpected concurrency error: %v", e)
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e := p.Close(t.Context()); e != nil {
				t.Errorf("close: %v", e)
			}
		}()
		wg.Wait()
		snapshotEmpty(t, base)
	}
}
