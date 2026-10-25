//go:build linux || darwin

package provisioning

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func localStageFixture(t *testing.T, arch string) []byte {
	t.Helper()
	if dir := os.Getenv("PHOENIX_LOCAL_STAGE_FIXTURES"); dir != "" {
		b, err := os.ReadFile(filepath.Join(dir, arch))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	return buildArtifactFixture(t, "linux", arch, "cmd/probe", "0")
}

func localStageService(t *testing.T) (string, *os.File, *LocalBinaryStageWriter, *services.ProbeBinaryStageWriteService) {
	t.Helper()
	base := t.TempDir()
	p := filepath.Join(base, "parent")
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	w, err := NewLocalBinaryStageWriter(t.Context(), f)
	if err != nil {
		t.Fatal(err)
	}
	s, err := services.NewProbeBinaryStageWriteService(w)
	if err != nil {
		t.Fatal(err)
	}
	return p, f, w, s
}

func localStagePrepared(t *testing.T, data []byte, arch string) ports.PreparedProbeBinary {
	t.Helper()
	s, err := services.NewProbeBinaryPreparationService(NewProbeBinaryPreparer(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Prepare(t.Context(), artifactPin(data, arch), io.NopCloser(bytes.NewReader(data)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return p
}

func assertLocalStageGraph(t *testing.T, parent string, intent domain.ProbeBinaryStageIntent, data []byte) {
	t.Helper()
	p := filepath.Join(parent, (domain.ProbeBinaryStageSeal{Intent: intent}).DirectoryName())
	entries, err := os.ReadDir(p)
	if err != nil || len(entries) != 2 {
		t.Fatal("fresh graph not exactly two entries", err)
	}
	for _, tc := range []struct {
		name string
		mode os.FileMode
		data []byte
	}{
		{intent.Identity.Binary.ReleaseName(), 0o555, data},
		{domain.ProbeBinaryStageMarkerName, 0o444, []byte(intent.Marker())},
	} {
		name := filepath.Join(p, tc.name)
		st, err := os.Lstat(name)
		if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != tc.mode {
			t.Fatal("local file type/mode mismatch", err)
		}
		got, err := os.ReadFile(name)
		if err != nil || !bytes.Equal(got, tc.data) {
			t.Fatal("staged bytes differ from verified input", err)
		}
	}
}

func TestLocalBinaryStageEffects(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			data := localStageFixture(t, arch)
			p := localStagePrepared(t, data, arch)
			parent, fd, _, s := localStageService(t)
			intent := stageIntent(p.Pin(), "11111111-1111-4111-8111-111111111101")
			canary := filepath.Join(parent, "must-not-execute")
			t.Setenv("PHOENIX_ARTIFACT_EXECUTION_CANARY", canary)
			seal, err := s.Stage(t.Context(), intent, p)
			if err != nil || !seal.Valid() || seal.Intent != intent {
				t.Fatal("real staging failed", err)
			}
			assertLocalStageGraph(t, parent, intent, data)
			if _, err := os.Stat(canary); !os.IsNotExist(err) {
				t.Fatal("artifact executed", err)
			}
			if _, err := fd.Stat(); err != nil {
				t.Fatal("borrowed parent closed", err)
			}
			r, err := p.Open(t.Context())
			if err != nil {
				t.Fatal("borrowed prepared binary closed", err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			seal, err = s.Stage(t.Context(), intent, p)
			if err != domain.ErrProbeBinaryStageWriteConflict || seal != (domain.ProbeBinaryStageSeal{}) {
				t.Fatal("fresh stage replay/adoption allowed", err)
			}
			assertLocalStageGraph(t, parent, intent, data)
		})
	}
}

type localStageEOFPause struct {
	*bytes.Reader
	paused bool
}

func (r *localStageEOFPause) Read(p []byte) (int, error) {
	if r.Reader.Len() == 0 {
		if !r.paused {
			r.paused = true
			return 0, nil
		}
		p[0] = 'x'
		return 1, nil
	}
	return r.Reader.Read(p)
}

type localStageNoProgress struct{ reads int }

func (r *localStageNoProgress) Read([]byte) (int, error) {
	r.reads++
	if r.reads > 100 {
		return 0, io.ErrNoProgress
	}
	return 0, nil
}

func TestLocalBinaryStageProgressEOF(t *testing.T) {
	data := localStageFixture(t, "amd64")
	for _, mode := range []string{"unconfirmed_eof", "no_progress"} {
		t.Run(mode, func(t *testing.T) {
			parent, _, _, s := localStageService(t)
			intent := stageIntent(artifactPin(data, "amd64"), "11111111-1111-4111-8111-111111111102")
			var reader io.Reader = &localStageEOFPause{Reader: bytes.NewReader(data)}
			stalled := &localStageNoProgress{}
			if mode == "no_progress" {
				reader = stalled
			}
			stream := &stageStream{Reader: reader}
			p := &stagePrepared{pin: intent.Identity.Binary, open: func(context.Context) (io.ReadCloser, error) { return stream, nil }}
			seal, err := s.Stage(t.Context(), intent, p)
			if err != domain.ErrProbeBinaryStageWriteUnknown || seal != (domain.ProbeBinaryStageSeal{}) {
				t.Fatal("unconfirmed EOF accepted", err)
			}
			if stalled.reads > 100 {
				t.Fatal("zero progress was not bounded", stalled.reads)
			}
			if stream.closes != 1 || p.closes != 0 {
				t.Fatal("source lifetime violated")
			}
			dir := filepath.Join(parent, (domain.ProbeBinaryStageSeal{Intent: intent}).DirectoryName())
			if _, err := os.Lstat(dir); err != nil {
				t.Fatal("post-create uncertainty erased", err)
			}
			if _, err := os.Lstat(filepath.Join(dir, domain.ProbeBinaryStageMarkerName)); !os.IsNotExist(err) {
				t.Fatal("marker published without source completion", err)
			}
		})
	}
}

func TestLocalBinaryStageUnknownEffects(t *testing.T) {
	data := localStageFixture(t, "amd64")
	for _, mode := range []string{"readback_corrupt", "marker_corrupt", "foreign_entry", "child_replaced", "before_marker_cancel", "source_close_cancel", "source_close_error"} {
		t.Run(mode, func(t *testing.T) {
			parent, _, w, s := localStageService(t)
			p := localStagePrepared(t, data, "amd64")
			intent := stageIntent(p.Pin(), "11111111-1111-4111-8111-111111111103")
			name := (domain.ProbeBinaryStageSeal{Intent: intent}).DirectoryName()
			dir := filepath.Join(parent, name)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			outside := t.TempDir()
			canary := filepath.Join(outside, "canary")
			if err := os.WriteFile(canary, []byte("untouched"), 0o600); err != nil {
				t.Fatal(err)
			}
			noMarker := false
			switch mode {
			case "readback_corrupt":
				w.beforeReadback = func() error {
					path := filepath.Join(dir, p.Pin().ReleaseName())
					if err := os.Chmod(path, 0o600); err != nil {
						return err
					}
					f, err := os.OpenFile(path, os.O_RDWR, 0)
					if err != nil {
						return err
					}
					defer func() { _ = f.Close() }()
					if _, err := f.WriteAt([]byte{data[15] ^ 1}, 15); err != nil {
						return err
					}
					if err := f.Chmod(0o555); err != nil {
						return err
					}
					if !validProbeBinary(ctx, f, p.Pin()) {
						t.Fatal("padding corruption unexpectedly changed ELF metadata contract")
					}
					return f.Sync()
				}
			case "marker_corrupt":
				w.writeChunk = func(f *os.File, b []byte) (int, error) {
					if bytes.HasPrefix(b, []byte("phoenix-binary-stage-v1")) {
						b = append([]byte(nil), b...)
						b[0] ^= 1
					}
					return f.Write(b)
				}
			case "foreign_entry":
				w.beforeDirCheck = func() error { return os.WriteFile(filepath.Join(dir, "foreign"), []byte("preserve"), 0o600) }
			case "child_replaced":
				w.afterChildFD = func() error {
					if err := os.Rename(dir, dir+"-moved"); err != nil {
						return err
					}
					return os.Symlink(outside, dir)
				}
			case "before_marker_cancel":
				w.beforeMarker = func() error { cancel(); return nil }
				noMarker = true
			}
			var source ports.PreparedProbeBinary = p
			if mode == "source_close_cancel" || mode == "source_close_error" {
				noMarker = true
				r := &localStageCloseAction{Reader: bytes.NewReader(data), onClose: func() error {
					if mode == "source_close_cancel" {
						cancel()
						return nil
					}
					return errors.New("private storage detail")
				}}
				source = &stagePrepared{pin: p.Pin(), open: func(context.Context) (io.ReadCloser, error) { return r, nil }}
			}
			seal, err := s.Stage(ctx, intent, source)
			if err != domain.ErrProbeBinaryStageWriteUnknown || seal != (domain.ProbeBinaryStageSeal{}) {
				t.Fatal("post-create effect reported as safe refusal or success", err)
			}
			got, err := os.ReadFile(canary)
			if err != nil || string(got) != "untouched" {
				t.Fatal("foreign object mutated", err)
			}
			if mode == "child_replaced" {
				e, err := os.ReadDir(outside)
				if err != nil || len(e) != 1 {
					t.Fatal("writes followed replacement directory", err)
				}
			}
			if noMarker {
				if _, err := os.Lstat(filepath.Join(dir, domain.ProbeBinaryStageMarkerName)); !os.IsNotExist(err) {
					t.Fatal("marker published after known cancellation or source failure", err)
				}
			}
			if mode == "foreign_entry" {
				b, err := os.ReadFile(filepath.Join(dir, "foreign"))
				if err != nil || string(b) != "preserve" {
					t.Fatal("unknown entry cleaned", err)
				}
			}
		})
	}
}

type localStageCloseAction struct {
	io.Reader
	onClose func() error
}

func (r *localStageCloseAction) Close() error { return r.onClose() }

func TestLocalBinaryStageParentFDAndConcurrency(t *testing.T) {
	data := localStageFixture(t, "amd64")
	p := localStagePrepared(t, data, "amd64")
	parent, _, _, s := localStageService(t)
	moved := parent + "-moved"
	if err := os.Rename(parent, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	intent := stageIntent(p.Pin(), "11111111-1111-4111-8111-111111111104")
	const n = 8
	results := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			seal, err := s.Stage(t.Context(), intent, p)
			if err == nil && seal.Intent != intent {
				err = errors.New("foreign seal")
			}
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	fresh, conflict := 0, 0
	for err := range results {
		if err == nil {
			fresh++
		} else if err == domain.ErrProbeBinaryStageWriteConflict {
			conflict++
		} else {
			t.Fatal("concurrent stage unexpected error", err)
		}
	}
	if fresh != 1 || conflict != n-1 {
		t.Fatal("exclusive admission violated", fresh, conflict)
	}
	assertLocalStageGraph(t, moved, intent, data)
	if e, err := os.ReadDir(parent); err != nil || len(e) != 0 {
		t.Fatal("parent pathname re-resolved", err)
	}
	got, err := os.ReadFile(filepath.Join(moved, (domain.ProbeBinaryStageSeal{Intent: intent}).DirectoryName(), p.Pin().ReleaseName()))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(got)
	if hex.EncodeToString(sum[:]) != p.Pin().SHA256 {
		t.Fatal("concurrent write corruption")
	}
}
