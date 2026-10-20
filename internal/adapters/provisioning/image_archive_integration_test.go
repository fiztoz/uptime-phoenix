package provisioning

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
	"github.com/fiztoz/uptime-phoenix/internal/testutil"
)

func archiveRead(t *testing.T, a ports.PreparedProbeImageArchive) []byte {
	t.Helper()
	r, err := a.Open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(io.LimitReader(r, a.Descriptor().ArchiveSize+1))
	closeErr := r.Close()
	if err != nil || closeErr != nil || int64(len(b)) != a.Descriptor().ArchiveSize || testutil.ImageSHA256(b) != a.Descriptor().ArchiveSHA256 {
		t.Fatal("archive bytes differ from descriptor", err, closeErr)
	}
	return b
}

func archiveGraph(t *testing.T, b []byte) map[string][]byte {
	t.Helper()
	entries := map[string][]byte{}
	r := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal("not a complete tar", err)
		}
		if h.Format != tar.FormatUSTAR || h.Typeflag != tar.TypeReg || h.Mode != 0444 || h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" || h.Linkname != "" || len(h.PAXRecords) != 0 || !h.ModTime.Equal(time.Unix(0, 0)) {
			t.Fatal("noncanonical archive header")
		}
		if h.Name != "index.json" && h.Name != "oci-layout" && !(strings.HasPrefix(h.Name, "blobs/sha256/") && len(h.Name) == len("blobs/sha256/")+64) {
			t.Fatal("unsafe or extra archive path")
		}
		if _, ok := entries[h.Name]; ok {
			t.Fatal("duplicate archive entry")
		}
		if h.Size < 0 || h.Size > 1<<20 {
			t.Fatal("fixture entry exceeded test budget")
		}
		payload, err := io.ReadAll(io.LimitReader(r, h.Size+1))
		if err != nil || int64(len(payload)) != h.Size {
			t.Fatal("truncated archive payload")
		}
		if strings.HasPrefix(h.Name, "blobs/") && strings.TrimPrefix(h.Name, "blobs/sha256/") != testutil.ImageSHA256(payload) {
			t.Fatal("blob name not digest bound")
		}
		entries[h.Name] = payload
	}
	if len(b) < 1024 || !bytes.Equal(b[len(b)-1024:], make([]byte, 1024)) {
		t.Fatal("tar terminators missing")
	}
	return entries
}

func archiveContentInputs(f *testutil.ProbeImageContentFixture, entries map[string][]byte, root []byte) ports.ProbeImageContentInputs {
	v := ports.ProbeImageContentInputs{Metadata: ports.ProbeImageInputs{
		Index: io.NopCloser(bytes.NewReader(root)), Manifest: io.NopCloser(bytes.NewReader(entries["blobs/sha256/"+testutil.ImageSHA256(f.ManifestBytes)])), Config: io.NopCloser(bytes.NewReader(entries["blobs/sha256/"+testutil.ImageSHA256(f.ConfigBytes)])),
	}}
	for _, layer := range f.Layers {
		v.Layers = append(v.Layers, io.NopCloser(bytes.NewReader(entries["blobs/sha256/"+testutil.ImageSHA256(layer)])))
	}
	return v
}

func TestProbeImageArchiveRealGraph(t *testing.T) {
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
					f.ChangeIndex(t, func(index map[string]any) {
						index["manifests"] = append(index["manifests"].([]any), map[string]any{"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": "sha256:" + strings.Repeat("a", 64), "size": 1, "platform": map[string]any{"os": "linux", "architecture": "s390x"}})
					})
					snapshot, _ := snapshotPrepare(t, f)
					base := t.TempDir()
					svc, err := services.NewProbeImageArchiveService(NewProbeImageArchivePacker(base))
					if err != nil {
						t.Fatal(err)
					}
					a, err := svc.Pack(t.Context(), f.Pin, snapshot)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = a.Close(nil) }()
					d := a.Descriptor()
					if !d.Valid() || d.Content != snapshot.Verification() || d.Content.Selection.Pin != f.Pin {
						t.Fatal("original receipt lost")
					}
					b := archiveRead(t, a)
					entries := archiveGraph(t, b)
					if string(entries["oci-layout"]) != "{\"imageLayoutVersion\":\"1.0.0\"}" {
						t.Fatal("missing OCI layout version")
					}
					var index struct {
						SchemaVersion int    `json:"schemaVersion"`
						MediaType     string `json:"mediaType"`
						Manifests     []struct {
							Digest   string `json:"digest"`
							Size     int64  `json:"size"`
							Platform struct {
								OS           string `json:"os"`
								Architecture string `json:"architecture"`
							} `json:"platform"`
						} `json:"manifests"`
					}
					if json.Unmarshal(entries["index.json"], &index) != nil || index.SchemaVersion != 2 || index.MediaType != "application/vnd.oci.image.index.v1+json" || len(index.Manifests) != 1 {
						t.Fatal("generated root is not single target")
					}
					m := index.Manifests[0]
					if m.Digest != "sha256:"+testutil.ImageSHA256(f.ManifestBytes) || m.Size != int64(len(f.ManifestBytes)) || m.Platform.OS != "linux" || m.Platform.Architecture != arch {
						t.Fatal("generated root changed selected manifest/platform")
					}
					if d.LayoutIndexSHA256 != testutil.ImageSHA256(entries["index.json"]) || d.LayoutIndexSize != int64(len(entries["index.json"])) || d.LayoutIndexSHA256 == f.Pin.IndexSHA256 {
						t.Fatal("generated and original index provenance conflated")
					}
					want := map[string][]byte{testutil.ImageSHA256(f.ManifestBytes): f.ManifestBytes, testutil.ImageSHA256(f.ConfigBytes): f.ConfigBytes}
					for _, l := range f.Layers {
						want[testutil.ImageSHA256(l)] = l
					}
					if len(entries) != len(want)+2 {
						t.Fatal("archive graph contains missing/extra blobs")
					}
					for sha, payload := range want {
						if !bytes.Equal(entries["blobs/sha256/"+sha], payload) {
							t.Fatal("manifest/config/layer bytes rewritten")
						}
					}
					v, err := NewProbeImageContentVerifier().VerifyImageContent(t.Context(), f.Pin, archiveContentInputs(f, entries, f.IndexBytes))
					if err != nil || v == nil || *v != d.Content {
						t.Fatal("archive graph failed original-index content verification", err)
					}
					local := f.Pin
					local.IndexSHA256 = d.LayoutIndexSHA256
					local.IndexSize = d.LayoutIndexSize
					localResult, err := NewProbeImageContentVerifier().VerifyImageContent(t.Context(), local, archiveContentInputs(f, entries, entries["index.json"]))
					if err != nil || localResult == nil || localResult.Selection.ManifestSHA256 != d.Content.Selection.ManifestSHA256 || localResult.Selection.Pin != local {
						t.Fatal("generated root graph is not closed", err)
					}
					if wrong, err := NewProbeImageContentVerifier().VerifyImageContent(t.Context(), f.Pin, archiveContentInputs(f, entries, entries["index.json"])); err == nil || wrong != nil {
						t.Fatal("new index accepted under original pin")
					}
					second, err := svc.Pack(t.Context(), f.Pin, snapshot)
					if err != nil {
						t.Fatal("borrowed snapshot unusable", err)
					}
					if !bytes.Equal(b, archiveRead(t, second)) {
						t.Fatal("archive bytes are nondeterministic")
					}
					if err = second.Close(nil); err != nil {
						t.Fatal(err)
					}
					if err = snapshot.Close(nil); err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(b, archiveRead(t, a)) {
						t.Fatal("archive depends on closed source")
					}
					if err = a.Close(nil); err != nil {
						t.Fatal(err)
					}
					snapshotEmpty(t, base)
				})
			}
		}
	}
}

type archiveBorrowed struct {
	verification domain.ProbeImageContentVerification
	open         func(context.Context) (ports.ProbeImageContentInputs, error)
	closes       atomic.Int32
}

func (s *archiveBorrowed) Verification() domain.ProbeImageContentVerification { return s.verification }
func (s *archiveBorrowed) Open(ctx context.Context) (ports.ProbeImageContentInputs, error) {
	return s.open(ctx)
}
func (s *archiveBorrowed) Close(context.Context) error {
	s.closes.Add(1)
	return errors.New("borrowed snapshot must not close")
}
func archiveReceipt(f *testutil.ProbeImageContentFixture) domain.ProbeImageContentVerification {
	v := domain.ProbeImageContentVerification{Selection: domain.ProbeImageSelection{Pin: f.Pin, ManifestSHA256: strings.TrimPrefix(f.Index["manifests"].([]any)[0].(map[string]any)["digest"].(string), "sha256:"), ConfigSHA256: strings.TrimPrefix(f.Manifest["config"].(map[string]any)["digest"].(string), "sha256:"), LayerCount: len(f.Layers)}}
	for _, l := range f.Manifest["layers"].([]any) {
		v.LayerBytes += int64(l.(map[string]any)["size"].(int))
	}
	for _, l := range f.Uncompressed {
		v.UncompressedBytes += int64(len(l))
	}
	return v
}

func TestProbeImageArchiveByteStreamDifferential(t *testing.T) {
	f := testutil.NewProbeImageContentFixture(t, "amd64", false, false, 1)
	in, all := contentInputs(f)
	fake := &archiveBorrowed{verification: archiveReceipt(f), open: func(context.Context) (ports.ProbeImageContentInputs, error) { return in, nil }}
	a, err := NewProbeImageArchivePacker(t.TempDir()).PackImageArchive(t.Context(), f.Pin, fake)
	if err != nil {
		t.Fatal("valid byte-stream differential refused", err)
	}
	defer func() { _ = a.Close(nil) }()
	_ = archiveGraph(t, archiveRead(t, a))
	contentClosed(t, all)
}

func TestProbeImageArchiveLayerEOFLifecycle(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "layer-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if _, err = file.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	r := &archiveLayerReader{source: &imageSnapshotSource{input: file}, tw: tar.NewWriter(io.Discard), name: "blobs/sha256/" + strings.Repeat("a", 64), size: 7, state: &archivePackState{}}
	if _, err = io.Copy(io.Discard, r); err != nil {
		t.Fatal(err)
	}
	if n, e := r.Read(make([]byte, 1)); n != 0 || e != io.EOF {
		t.Fatal("EOF was not sticky after underlying source closure", n, e)
	}
}

func TestProbeImageArchiveDuplicateLayers(t *testing.T) {
	f := testutil.NewProbeImageContentFixture(t, "amd64", false, true, 3)
	f.Layers[1] = append([]byte(nil), f.Layers[0]...)
	f.Uncompressed[1] = append([]byte(nil), f.Uncompressed[0]...)
	f.RebindContent(t)
	s, _ := snapshotPrepare(t, f)
	a, err := NewProbeImageArchivePacker(t.TempDir()).PackImageArchive(t.Context(), f.Pin, s)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close(nil) }()
	entries := archiveGraph(t, archiveRead(t, a))
	if len(entries) != 6 {
		t.Fatal("duplicate digest emitted twice")
	}
	v, err := NewProbeImageContentVerifier().VerifyImageContent(t.Context(), f.Pin, archiveContentInputs(f, entries, f.IndexBytes))
	if err != nil || v == nil || *v != a.Descriptor().Content {
		t.Fatal("ordered repeated edge lost verification", err)
	}
	// Even a duplicate blob's second stream must be consumed and verified.
	f.Layers[1][0] ^= 1
	in, all := contentInputs(f)
	fake := &archiveBorrowed{verification: s.Verification(), open: func(context.Context) (ports.ProbeImageContentInputs, error) { return in, nil }}
	bad, e := NewProbeImageArchivePacker(t.TempDir()).PackImageArchive(t.Context(), f.Pin, fake)
	if bad != nil || e != domain.ErrProbeImageDigest {
		t.Fatal("duplicate stream escaped verification", e)
	}
	contentClosed(t, all)
	if fake.closes.Load() != 0 {
		t.Fatal("borrowed snapshot closed")
	}
}

func TestProbeImageArchiveRefusals(t *testing.T) {
	for _, kind := range []string{"digest", "size", "codec", "diff_id", "metadata", "receipt", "open_private", "open_eof", "partial_open", "typed_nil", "duplicate_handle", "source_io"} {
		t.Run(kind, func(t *testing.T) {
			f := testutil.NewProbeImageContentFixture(t, "amd64", false, true, 1)
			want := error(domain.ErrProbeImageDigest)
			switch kind {
			case "digest":
				f.Layers[0][4] ^= 1
			case "size":
				f.Layers[0] = f.Layers[0][:len(f.Layers[0])-1]
				want = domain.ErrProbeImageSize
			case "codec":
				f.Layers[0][0] ^= 1
				f.RebindContent(t)
				want = domain.ErrProbeImageContentEncoding
			case "diff_id":
				f.Uncompressed[0] = []byte("different")
				f.RebindContent(t)
				want = domain.ErrProbeImageContentDiffID
			case "metadata":
				f.ManifestBytes[0] ^= 1
			case "receipt":
				want = domain.ErrProbeImageMetadata
			case "open_private", "open_eof", "partial_open", "source_io":
				want = domain.ErrProbeImageUnavailable
			case "typed_nil", "duplicate_handle":
				want = domain.ErrProbeImageInput
			}
			in, all := contentInputs(f)
			v := archiveReceipt(f)
			if kind == "receipt" {
				v.LayerBytes++
			}
			if kind == "typed_nil" {
				in.Metadata.Config = (*snapshotNilSource)(nil)
			}
			if kind == "duplicate_handle" {
				in.Metadata.Config = in.Metadata.Index
			}
			if kind == "source_io" {
				in.Layers[0] = &archiveErrorReader{}
			}
			fake := &archiveBorrowed{verification: v, open: func(context.Context) (ports.ProbeImageContentInputs, error) {
				if kind == "open_private" || kind == "partial_open" {
					return in, errors.New("sensitive-archive-input-path")
				}
				if kind == "open_eof" {
					return in, io.EOF
				}
				return in, nil
			}}
			base := t.TempDir()
			got, err := NewProbeImageArchivePacker(base).PackImageArchive(t.Context(), f.Pin, fake)
			if got != nil || err != want {
				if got != nil {
					_ = got.Close(nil)
				}
				t.Fatal("refusal did not preserve fixed cause", err, want)
			}
			if strings.Contains(err.Error(), "sensitive") || fake.closes.Load() != 0 {
				t.Fatal("borrowed ownership or secrecy violated")
			}
			if kind != "typed_nil" && kind != "duplicate_handle" && kind != "source_io" {
				contentClosed(t, all)
			} else {
				for i, stream := range all {
					unused := ((kind == "typed_nil" || kind == "duplicate_handle") && i == 2) || (kind == "source_io" && i == 3)
					wantClosed := int32(1)
					if unused {
						wantClosed = 0
					}
					if stream.closed.Load() != wantClosed {
						t.Fatal("supplied handle not closed exactly once")
					}
					if unused {
						_ = stream.Close()
					}
				}
				if kind == "source_io" && in.Layers[0].(*archiveErrorReader).closed.Load() != 1 {
					t.Fatal("error source not closed")
				}
			}
			if kind == "metadata" || kind == "typed_nil" || kind == "duplicate_handle" || strings.HasPrefix(kind, "open_") || kind == "partial_open" {
				contentNoLayersRead(t, all)
			}
			snapshotEmpty(t, base)
		})
	}
}

type archiveErrorReader struct{ closed atomic.Int32 }

func (*archiveErrorReader) Read([]byte) (int, error) {
	return 0, errors.New("sensitive-archive-reader-path")
}
func (r *archiveErrorReader) Close() error { r.closed.Add(1); return nil }

func archiveStagingForeign(t *testing.T, base string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(base, probeImageArchivePrefix+"*"))
	if err != nil || len(matches) != 1 {
		t.Fatal("private staging directory missing")
	}
	return filepath.Join(matches[0], "foreign")
}

func TestProbeImageArchiveStorageAndReadback(t *testing.T) {
	for _, kind := range []string{"readback", "budget", "write", "unknown_cancel"} {
		t.Run(kind, func(t *testing.T) {
			f := testutil.NewProbeImageContentFixture(t, "arm64", false, false, 1)
			s, _ := snapshotPrepare(t, f)
			base := t.TempDir()
			p := NewProbeImageArchivePacker(base)
			want := error(domain.ErrProbeImageArchiveStorage)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch kind {
			case "readback":
				p.beforeSeal = func(file *os.File) error { _, err := file.WriteAt([]byte{1}, 1024); return err }
				want = domain.ErrProbeImageArchiveIntegrity
			case "budget":
				p.limit = 1024
			case "write":
				p.writeFault = func() bool { return true }
			case "unknown_cancel":
				p.beforeSeal = func(file *os.File) error {
					if err := os.WriteFile(archiveStagingForeign(t, base), []byte("preserve"), 0600); err != nil {
						return err
					}
					cancel()
					return nil
				}
			}
			a, err := p.PackImageArchive(ctx, f.Pin, s)
			if a != nil || err != want {
				if a != nil {
					_ = a.Close(nil)
				}
				t.Fatal("storage or integrity failure hidden", err, want)
			}
			if kind == "unknown_cancel" {
				matches, _ := filepath.Glob(filepath.Join(base, "*", "foreign"))
				if len(matches) != 1 {
					t.Fatal("unknown file was recursively deleted")
				}
				data, e := os.ReadFile(matches[0])
				if e != nil || string(data) != "preserve" {
					t.Fatal("foreign data altered")
				}
			} else {
				snapshotEmpty(t, base)
			}
			bundle, e := s.Open(t.Context())
			if e != nil {
				t.Fatal("borrowed source left busy", e)
			}
			snapshotCloseBundle(bundle)
		})
	}
}

func TestProbeImageArchiveReaderLifecycle(t *testing.T) {
	f := testutil.NewProbeImageContentFixture(t, "amd64", true, true, 1)
	s, _ := snapshotPrepare(t, f)
	for _, kind := range []string{"reader_cancel", "owner_close", "unknown_retry", "file_replace", "symlink", "directory_replace", "concurrent"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			packed, err := NewProbeImageArchivePacker(base).PackImageArchive(t.Context(), f.Pin, s)
			if err != nil {
				t.Fatal(err)
			}
			a := packed.(*preparedProbeImageArchive)
			defer func() { _ = a.Close(nil) }()
			name := filepath.Join(a.inner.dir, probeImageArchiveFile)
			info, _ := os.Stat(name)
			if info.Mode().Perm() != 0400 {
				t.Fatal("archive not sealed")
			}
			switch kind {
			case "reader_cancel", "owner_close":
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				r, e := a.Open(ctx)
				if e != nil {
					t.Fatal(e)
				}
				if other, e := a.Open(t.Context()); other != nil || e != domain.ErrProbeImageArchiveBusy {
					t.Fatal("multiple archive readers accepted")
				}
				if kind == "reader_cancel" {
					cancel()
					if _, e = r.Read(make([]byte, 1)); e != context.Canceled {
						t.Fatal("canceled reader still usable", e)
					}
					select {
					case <-r.(*archiveBoundReader).joined:
					case <-time.After(3 * time.Second):
						t.Fatal("automatic archive reader cancellation did not join")
					}
					_ = archiveRead(t, a)
					if e = r.Close(); e != nil {
						t.Fatal(e)
					}
				} else {
					if e = a.Close(nil); e != nil {
						t.Fatal(e)
					}
					if _, e = r.Read(make([]byte, 1)); e != domain.ErrProbeImageArchiveClosed {
						t.Fatal("reader outlived owner", e)
					}
				}
			case "unknown_retry":
				foreign := filepath.Join(a.inner.dir, "foreign")
				if e := os.WriteFile(foreign, []byte("preserve"), 0600); e != nil {
					t.Fatal(e)
				}
				if e := a.Close(nil); e != domain.ErrProbeImageArchiveStorage {
					t.Fatal("unknown graph accepted", e)
				}
				if _, e := os.Stat(name); e != nil {
					t.Fatal("partial deletion before graph refusal")
				}
				if e := os.Remove(foreign); e != nil {
					t.Fatal(e)
				}
				if e := a.Close(nil); e != nil {
					t.Fatal("cleanup retry failed", e)
				}
			case "file_replace":
				saved := name + ".saved"
				if e := os.Rename(name, saved); e != nil {
					t.Fatal(e)
				}
				if e := os.WriteFile(name, []byte("foreign"), 0400); e != nil {
					t.Fatal(e)
				}
				if r, e := a.Open(t.Context()); r != nil || e != domain.ErrProbeImageArchiveStorage {
					t.Fatal("replacement file adopted", e)
				}
				if e := a.Close(nil); e != domain.ErrProbeImageArchiveStorage {
					t.Fatal("replacement removed", e)
				}
				data, _ := os.ReadFile(name)
				if string(data) != "foreign" {
					t.Fatal("foreign file changed")
				}
				_ = os.Remove(name)
				_ = os.Rename(saved, name)
				if e := a.Close(nil); e != nil {
					t.Fatal(e)
				}
			case "symlink":
				saved := name + ".saved"
				if e := os.Rename(name, saved); e != nil {
					t.Fatal(e)
				}
				if e := os.Symlink(filepath.Base(saved), name); e != nil {
					t.Fatal(e)
				}
				if r, e := a.Open(t.Context()); r != nil || e != domain.ErrProbeImageArchiveStorage {
					t.Fatal("symlink archive adopted", e)
				}
				if e := a.Close(nil); e != domain.ErrProbeImageArchiveStorage {
					t.Fatal("symlink graph accepted", e)
				}
				_ = os.Remove(name)
				_ = os.Rename(saved, name)
				if e := a.Close(nil); e != nil {
					t.Fatal(e)
				}
			case "directory_replace":
				saved := a.inner.dir + ".saved"
				if e := os.Rename(a.inner.dir, saved); e != nil {
					t.Fatal(e)
				}
				if e := os.Mkdir(a.inner.dir, 0700); e != nil {
					t.Fatal(e)
				}
				foreign := filepath.Join(a.inner.dir, "foreign")
				_ = os.WriteFile(foreign, []byte("preserve"), 0600)
				if r, e := a.Open(t.Context()); r != nil || e != domain.ErrProbeImageArchiveStorage {
					t.Fatal("replacement directory adopted", e)
				}
				if e := a.Close(nil); e != domain.ErrProbeImageArchiveStorage {
					t.Fatal("replacement directory removed", e)
				}
				_ = os.Remove(foreign)
				_ = os.Remove(a.inner.dir)
				_ = os.Rename(saved, a.inner.dir)
				if e := a.Close(nil); e != nil {
					t.Fatal(e)
				}
			case "concurrent":
				var wg sync.WaitGroup
				for range 12 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						r, e := a.Open(t.Context())
						if e == nil {
							_ = r.Close()
						} else if e != domain.ErrProbeImageArchiveBusy && e != domain.ErrProbeImageArchiveClosed {
							t.Error(e)
						}
						if e = a.Close(nil); e != nil {
							t.Error(e)
						}
					}()
				}
				wg.Wait()
			}
			if e := a.Close(nil); e != nil {
				t.Fatal(e)
			}
			if r, e := a.Open(t.Context()); r != nil || e != domain.ErrProbeImageArchiveClosed {
				t.Fatal("closed archive reopened", e)
			}
			snapshotEmpty(t, base)
		})
	}
}

type archiveBlocked struct {
	started, closing, release, closed chan struct{}
	startOnce, closeOnce              sync.Once
}

func (b *archiveBlocked) Read([]byte) (int, error) {
	b.startOnce.Do(func() { close(b.started) })
	<-b.closed
	return 0, io.ErrClosedPipe
}
func (b *archiveBlocked) Close() error {
	b.closeOnce.Do(func() { close(b.closing); close(b.closed); <-b.release })
	return nil
}

func TestProbeImageArchiveCancellationJoinsSources(t *testing.T) {
	f := testutil.NewProbeImageContentFixture(t, "amd64", false, false, 1)
	in, all := contentInputs(f)
	blocked := &archiveBlocked{started: make(chan struct{}), closing: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
	in.Layers[0] = blocked
	fake := &archiveBorrowed{verification: archiveReceipt(f), open: func(context.Context) (ports.ProbeImageContentInputs, error) { return in, nil }}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	base := t.TempDir()
	done := make(chan error, 1)
	go func() {
		a, err := NewProbeImageArchivePacker(base).PackImageArchive(ctx, f.Pin, fake)
		if a != nil {
			_ = a.Close(nil)
			done <- errors.New("canceled pack returned archive")
			return
		}
		done <- err
	}()
	select {
	case <-blocked.started:
	case <-time.After(3 * time.Second):
		t.Fatal("layer did not start")
	}
	cancel()
	select {
	case <-blocked.closing:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not close source")
	}
	select {
	case <-done:
		t.Fatal("pack returned before source closure joined")
	case <-time.After(30 * time.Millisecond):
	}
	close(blocked.release)
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pack failed to join")
	}
	for i, c := range all {
		if i != 3 && c.closed.Load() != 1 {
			t.Fatal("partial source not closed")
		}
	}
	if fake.closes.Load() != 0 {
		t.Fatal("borrowed snapshot closed")
	}
	snapshotEmpty(t, base)
}
