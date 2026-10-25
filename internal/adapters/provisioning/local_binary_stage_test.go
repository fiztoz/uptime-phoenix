//go:build linux || darwin

package provisioning

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func stageShapePin() domain.ProbeBinaryPin {
	return domain.ProbeBinaryPin{Version: "1.2.3", Architecture: "amd64", SHA256: strings.Repeat("ab", 32), Size: 17}
}

func stageIntent(pin domain.ProbeBinaryPin, id string) domain.ProbeBinaryStageIntent {
	fp := "SHA256:" + base64.RawStdEncoding.EncodeToString(make([]byte, 32))
	return domain.ProbeBinaryStageIntent{Identity: domain.ProbeInstallIdentity{OperationID: id, Host: domain.ProbeSSHHost{Address: "example.test:22", User: "deploy"}, HostKeyFingerprint: fp, ClientKeyFingerprint: fp, RequestedBy: 1, LayoutVersion: 1, Mode: domain.ProbeInstallSystemd, Binary: pin}, OwnerID: "22222222-2222-4222-8222-222222222222", AttemptID: "33333333-3333-4333-8333-333333333333"}
}

func stageParent(t *testing.T) (string, *os.File) {
	t.Helper()
	dir := t.TempDir()
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return dir, f
}

func assertStageErr(t *testing.T, seal domain.ProbeBinaryStageSeal, err, want error, secret string) {
	t.Helper()
	if err != want || seal != (domain.ProbeBinaryStageSeal{}) {
		t.Fatalf("seal=%+v err=%v want %v", seal, err, want)
	}
	if secret != "" && err != nil && strings.Contains(err.Error(), secret) {
		t.Fatal("error leaked private text")
	}
}

type stagePrepared struct {
	pin    domain.ProbeBinaryPin
	opens  int
	closes int
	open   func(context.Context) (io.ReadCloser, error)
}

func (p *stagePrepared) Pin() domain.ProbeBinaryPin { return p.pin }
func (p *stagePrepared) Open(ctx context.Context) (io.ReadCloser, error) {
	p.opens++
	if p.open != nil {
		return p.open(ctx)
	}
	return io.NopCloser(bytes.NewReader(nil)), nil
}
func (p *stagePrepared) Close(context.Context) error { p.closes++; return nil }

type stageStream struct {
	io.Reader
	closes   int
	closeErr error
}

func (s *stageStream) Close() error { s.closes++; return s.closeErr }

func TestLocalBinaryStageWriteRefusals(t *testing.T) {
	pin := stageShapePin()
	intent := stageIntent(pin, "11111111-1111-4111-8111-111111111111")
	dir, parent := stageParent(t)
	w, err := NewLocalBinaryStageWriter(t.Context(), parent)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := parent.Stat(); err != nil || !info.IsDir() {
		t.Fatal("constructor closed parent", err)
	}
	var nilWriter *LocalBinaryStageWriter
	seal, err := nilWriter.StageBinary(t.Context(), intent, &stagePrepared{pin: pin})
	assertStageErr(t, seal, err, domain.ErrProbeBinaryStageWriteInput, "")
	var nilCtx context.Context // deliberately nil: the refusal under test
	seal, err = w.StageBinary(nilCtx, intent, &stagePrepared{pin: pin})
	assertStageErr(t, seal, err, domain.ErrProbeBinaryStageWriteInput, "")
	if _, err := NewLocalBinaryStageWriter(nilCtx, parent); err != domain.ErrProbeBinaryStageWriteInput {
		t.Fatal(err)
	}
	if _, err := NewLocalBinaryStageWriter(t.Context(), nil); err != domain.ErrProbeBinaryStageWriteInput {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := NewLocalBinaryStageWriter(canceled, parent); err != context.Canceled {
		t.Fatal(err)
	}
	closed, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLocalBinaryStageWriter(t.Context(), closed); err != domain.ErrProbeBinaryStageWriteUnavailable {
		t.Fatal(err)
	}
	filePath := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(filePath, []byte("parent-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(filePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if _, err := NewLocalBinaryStageWriter(t.Context(), file); err != domain.ErrProbeBinaryStageWriteUnavailable {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filePath); err != nil || string(got) != "parent-file" {
		t.Fatal("non-directory parent mutated", err)
	}

	bad := intent
	bad.OwnerID = "nope"
	prep := &stagePrepared{pin: pin}
	seal, err = w.StageBinary(t.Context(), bad, prep)
	assertStageErr(t, seal, err, domain.ErrProbeBinaryStageWriteInput, "")
	if prep.opens != 0 || prep.closes != 0 {
		t.Fatal("invalid intent touched binary")
	}
	other := pin
	other.SHA256 = strings.Repeat("cd", 32)
	prep = &stagePrepared{pin: other}
	seal, err = w.StageBinary(t.Context(), intent, prep)
	assertStageErr(t, seal, err, domain.ErrProbeBinaryStageWriteInput, "")
	if prep.opens != 0 {
		t.Fatal("pin mismatch opened source")
	}
	var typed *stagePrepared
	var as ports.PreparedProbeBinary = typed
	seal, err = w.StageBinary(t.Context(), intent, as)
	assertStageErr(t, seal, err, domain.ErrProbeBinaryStageWriteInput, "")
	seal, err = w.StageBinary(canceled, intent, &stagePrepared{pin: pin})
	assertStageErr(t, seal, err, context.Canceled, "")
	deadline, stop := context.WithTimeout(t.Context(), time.Nanosecond)
	defer stop()
	time.Sleep(time.Millisecond)
	seal, err = w.StageBinary(deadline, intent, &stagePrepared{pin: pin})
	assertStageErr(t, seal, err, context.DeadlineExceeded, "")

	stream := &stageStream{}
	prep = &stagePrepared{pin: pin, open: func(context.Context) (io.ReadCloser, error) {
		return stream, errors.New("secret " + dir)
	}}
	seal, err = w.StageBinary(t.Context(), intent, prep)
	assertStageErr(t, seal, err, domain.ErrProbeBinaryStageWriteUnavailable, dir)
	if stream.closes != 1 || prep.closes != 0 {
		t.Fatal("partial open not owned", stream.closes, prep.closes)
	}
	prep = &stagePrepared{pin: pin, open: func(context.Context) (io.ReadCloser, error) { return nil, nil }}
	seal, err = w.StageBinary(t.Context(), intent, prep)
	assertStageErr(t, seal, err, domain.ErrProbeBinaryStageWriteUnavailable, "")
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatal("pre-create refusal mutated parent", err)
	}

	live, liveCancel := context.WithCancel(t.Context())
	prep = &stagePrepared{pin: pin, open: func(context.Context) (io.ReadCloser, error) {
		liveCancel()
		return &stageStream{Reader: bytes.NewReader(nil)}, nil
	}}
	seal, err = w.StageBinary(live, intent, prep)
	assertStageErr(t, seal, err, context.Canceled, "")
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatal("canceled open created a directory", err)
	}
}

func TestLocalBinaryStageWriteClosedParent(t *testing.T) {
	pin := stageShapePin()
	id := "11111111-1111-4111-8111-111111111112"
	dir, parent := stageParent(t)
	w, err := NewLocalBinaryStageWriter(t.Context(), parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	reusePath := filepath.Join(t.TempDir(), "reuse")
	reuse, err := os.OpenFile(reusePath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reuse.Write([]byte("reuse")); err != nil {
		t.Fatal(err)
	}
	seal, err := w.StageBinary(t.Context(), stageIntent(pin, id), &stagePrepared{pin: pin})
	assertStageErr(t, seal, err, domain.ErrProbeBinaryStageWriteUnavailable, dir)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("closed parent created a child", err)
	}
	if err := reuse.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(reusePath); err != nil || string(got) != "reuse" {
		t.Fatal("reused descriptor mutated", err)
	}
}

func TestLocalBinaryStageWriteConflicts(t *testing.T) {
	pin := stageShapePin()
	dir, parent := stageParent(t)
	w, err := NewLocalBinaryStageWriter(t.Context(), parent)
	if err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(t.TempDir(), "canary")
	if err := os.WriteFile(canary, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		id    string
		plant func(path string) error
	}{
		{id: "11111111-1111-4111-8111-111111111113", plant: func(path string) error { return os.WriteFile(path, []byte("file"), 0o600) }},
		{id: "11111111-1111-4111-8111-111111111114", plant: func(path string) error {
			if err := os.Mkdir(path, 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(path, "keep"), []byte("keep"), 0o600)
		}},
		{id: "11111111-1111-4111-8111-111111111115", plant: func(path string) error { return os.Symlink(canary, path) }},
	}
	for _, tc := range cases {
		path := filepath.Join(dir, "uptime-phoenix-probe-"+tc.id)
		if err := tc.plant(path); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(canary)
		if err != nil {
			t.Fatal(err)
		}
		stream := &stageStream{Reader: bytes.NewReader(nil)}
		prep := &stagePrepared{pin: pin, open: func(context.Context) (io.ReadCloser, error) { return stream, nil }}
		seal, err := w.StageBinary(t.Context(), stageIntent(pin, tc.id), prep)
		assertStageErr(t, seal, err, domain.ErrProbeBinaryStageWriteConflict, dir)
		if stream.closes != 1 || prep.closes != 0 || prep.opens != 1 {
			t.Fatal("conflict did not own only the opened stream")
		}
		if got, err := os.ReadFile(canary); err != nil || !bytes.Equal(got, before) {
			t.Fatal("canary changed", err)
		}
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if target, err := os.Readlink(path); err != nil || target != canary {
				t.Fatal("symlink replaced", err, target)
			}
			continue
		}
		if info.IsDir() {
			entries, err := os.ReadDir(path)
			if err != nil || len(entries) != 1 || entries[0].Name() != "keep" {
				t.Fatal("existing directory adopted", err)
			}
			continue
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != "file" {
			t.Fatal("existing file overwritten", err)
		}
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "outside" {
		t.Fatal("outside canary changed", err)
	}
}

func TestLocalBinaryStageWriteUnknownWithoutELF(t *testing.T) {
	dir, parent := stageParent(t)
	w, err := NewLocalBinaryStageWriter(t.Context(), parent)
	if err != nil {
		t.Fatal(err)
	}
	id := "11111111-1111-4111-8111-111111111116"
	pin := stageShapePin()
	intent := stageIntent(pin, id)
	name := "uptime-phoenix-probe-" + id
	w.afterChildFD = func() error { return errors.New("injected " + dir) }
	seal, err := w.StageBinary(t.Context(), intent, bytesPrepared(pin, nil))
	assertStageErr(t, seal, err, domain.ErrProbeBinaryStageWriteUnknown, dir)
	entries, err := os.ReadDir(filepath.Join(dir, name))
	if err != nil || len(entries) != 0 {
		t.Fatal("hook created or removed children", err)
	}
	w.afterChildFD = nil

	id = "11111111-1111-4111-8111-111111111117"
	pin = pinFor([]byte("ab"))
	pin.Size = 1
	intent = stageIntent(pin, id)
	seal, err = w.StageBinary(t.Context(), intent, bytesPrepared(pin, []byte("ab")))
	assertStageErr(t, seal, err, domain.ErrProbeBinaryStageWriteUnknown, "")
	got, err := os.ReadFile(filepath.Join(dir, "uptime-phoenix-probe-"+id, pin.ReleaseName()))
	if err != nil || !bytes.Equal(got, []byte("a")) {
		t.Fatal("excess sentinel stored", err, got)
	}
	if _, err := os.Lstat(filepath.Join(dir, "uptime-phoenix-probe-"+id, domain.ProbeBinaryStageMarkerName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("marker published after size failure", err)
	}

	id = "11111111-1111-4111-8111-111111111118"
	payload := []byte("not-an-elf!!!!!")
	pin = pinFor(payload)
	seal, err = w.StageBinary(t.Context(), stageIntent(pin, id), bytesPrepared(pin, payload))
	assertStageErr(t, seal, err, domain.ErrProbeBinaryStageWriteUnknown, "")
	if _, err := os.Lstat(filepath.Join(dir, "uptime-phoenix-probe-"+id, domain.ProbeBinaryStageMarkerName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("marker published after metadata failure", err)
	}

	id = "11111111-1111-4111-8111-111111111119"
	canary := filepath.Join(t.TempDir(), "link-canary")
	if err := os.WriteFile(canary, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	pin = stageShapePin()
	intent = stageIntent(pin, id)
	w.afterChildFD = func() error {
		return os.Symlink(canary, filepath.Join(dir, "uptime-phoenix-probe-"+id, pin.ReleaseName()))
	}
	seal, err = w.StageBinary(t.Context(), intent, bytesPrepared(pin, []byte("x")))
	assertStageErr(t, seal, err, domain.ErrProbeBinaryStageWriteUnknown, canary)
	if got, err := os.ReadFile(canary); err != nil || string(got) != "safe" {
		t.Fatal("create followed symlink", err)
	}
	w.afterChildFD = nil

	id = "11111111-1111-4111-8111-11111111111a"
	pin = stageShapePin()
	w.writeChunk = func(*os.File, []byte) (int, error) { return 0, nil }
	seal, err = w.StageBinary(t.Context(), stageIntent(pin, id), bytesPrepared(pin, []byte("short-write")))
	assertStageErr(t, seal, err, domain.ErrProbeBinaryStageWriteUnknown, "")
	w.writeChunk = nil
}

func TestLocalBinaryStageWriteBlockedJoin(t *testing.T) {
	dir, parent := stageParent(t)
	w, err := NewLocalBinaryStageWriter(t.Context(), parent)
	if err != nil {
		t.Fatal(err)
	}
	id := "11111111-1111-4111-8111-11111111111b"
	pin := stageShapePin()
	started := make(chan struct{})
	closed := make(chan struct{})
	release := make(chan struct{})
	unblock := make(chan struct{})
	var once sync.Once
	stream := &blockingStream{started: started, closed: closed, release: release, unblock: unblock, once: &once}
	prep := &stagePrepared{pin: pin, open: func(context.Context) (io.ReadCloser, error) { return stream, nil }}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		seal, err := w.StageBinary(ctx, stageIntent(pin, id), prep)
		if seal != (domain.ProbeBinaryStageSeal{}) && err == nil {
			done <- errors.New("sealed blocked write")
			return
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("read did not start")
	}
	cancel()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not close source")
	}
	select {
	case err := <-done:
		t.Fatal("returned before close joined", err)
	default:
	}
	close(release)
	select {
	case err := <-done:
		if err != domain.ErrProbeBinaryStageWriteUnknown {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not join source close")
	}
	if prep.closes != 0 {
		t.Fatal("closed borrowed snapshot")
	}
	if _, err := os.Lstat(filepath.Join(dir, "uptime-phoenix-probe-"+id, domain.ProbeBinaryStageMarkerName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("marker published without joined source", err)
	}
}

type blockingStream struct {
	started chan struct{}
	closed  chan struct{}
	release chan struct{}
	unblock chan struct{}
	once    *sync.Once
	read    sync.Once
}

func (b *blockingStream) Read([]byte) (int, error) {
	b.read.Do(func() { close(b.started) })
	<-b.unblock
	return 0, io.ErrUnexpectedEOF
}
func (b *blockingStream) Close() error {
	b.once.Do(func() {
		close(b.closed)
		<-b.release
		close(b.unblock)
	})
	return nil
}

func TestLocalBinaryStageWriteVerified(t *testing.T) {
	amd := buildArtifactFixture(t, "linux", "amd64", "cmd/probe", "0")
	arm := buildArtifactFixture(t, "linux", "arm64", "cmd/probe", "0")
	canary := filepath.Join(t.TempDir(), "must-not-execute")
	t.Setenv("PHOENIX_ARTIFACT_EXECUTION_CANARY", canary)
	for _, tc := range []struct {
		arch string
		data []byte
		id   string
	}{
		{arch: "amd64", data: amd, id: "11111111-1111-4111-8111-111111111121"},
		{arch: "arm64", data: arm, id: "11111111-1111-4111-8111-111111111122"},
	} {
		t.Run(tc.arch, func(t *testing.T) {
			dir, parent := stageParent(t)
			w, err := NewLocalBinaryStageWriter(t.Context(), parent)
			if err != nil {
				t.Fatal(err)
			}
			pin := artifactPin(tc.data, tc.arch)
			intent := stageIntent(pin, tc.id)
			prep := bytesPrepared(pin, tc.data)
			seal, err := w.StageBinary(t.Context(), intent, prep)
			if err != nil || !seal.Valid() || seal.Intent != intent {
				t.Fatal(err, seal)
			}
			if seal.DirectoryName() != "uptime-phoenix-probe-"+tc.id || strings.Contains(seal.DirectoryName(), "/") || seal.DirectoryName() == intent.Directory() {
				t.Fatal("directory name is not a single relative component")
			}
			stageDir := filepath.Join(dir, seal.DirectoryName())
			assertStageMode(t, stageDir, 0o700, false)
			binPath := filepath.Join(stageDir, pin.ReleaseName())
			markPath := filepath.Join(stageDir, domain.ProbeBinaryStageMarkerName)
			assertStageMode(t, binPath, 0o555, true)
			assertStageMode(t, markPath, 0o444, true)
			got, err := os.ReadFile(binPath)
			if err != nil || !bytes.Equal(got, tc.data) {
				t.Fatal("binary bytes", err)
			}
			marker, err := os.ReadFile(markPath)
			if err != nil || string(marker) != intent.Marker() {
				t.Fatal("marker bytes", err)
			}
			entries, err := os.ReadDir(stageDir)
			if err != nil || len(entries) != 2 {
				t.Fatal("directory graph", err, entries)
			}
			if prep.opens != 1 || prep.closes != 0 || prep.stream.closes != 1 {
				t.Fatal("borrowed source lifetime", prep.opens, prep.closes, prep.stream.closes)
			}
			if _, err := parent.Stat(); err != nil {
				t.Fatal("parent closed", err)
			}
			again, err := w.StageBinary(t.Context(), intent, bytesPrepared(pin, tc.data))
			assertStageErr(t, again, err, domain.ErrProbeBinaryStageWriteConflict, dir)
			got, err = os.ReadFile(binPath)
			if err != nil || !bytes.Equal(got, tc.data) {
				t.Fatal("second call overwrote", err)
			}
			if _, err := os.Lstat(canary); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("staged binary executed")
			}
			if _, err := os.Lstat(intent.Directory()); err == nil {
				t.Fatal("absolute artifact directory created")
			}
		})
	}

	t.Run("rename-parent", func(t *testing.T) {
		dir, parent := stageParent(t)
		w, err := NewLocalBinaryStageWriter(t.Context(), parent)
		if err != nil {
			t.Fatal(err)
		}
		renamed := filepath.Join(t.TempDir(), "renamed-parent")
		if err := os.Rename(dir, renamed); err != nil {
			t.Fatal(err)
		}
		pin := artifactPin(amd, "amd64")
		id := "11111111-1111-4111-8111-111111111123"
		seal, err := w.StageBinary(t.Context(), stageIntent(pin, id), bytesPrepared(pin, amd))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(renamed, seal.DirectoryName(), pin.ReleaseName())); err != nil {
			t.Fatal("rename defeated fd stage", err)
		}
	})

	t.Run("faults", func(t *testing.T) {
		pin := artifactPin(amd, "amd64")
		dir, parent := stageParent(t)
		w, err := NewLocalBinaryStageWriter(t.Context(), parent)
		if err != nil {
			t.Fatal(err)
		}
		id := "11111111-1111-4111-8111-111111111124"
		intent := stageIntent(pin, id)
		stageDir := filepath.Join(dir, "uptime-phoenix-probe-"+id)
		w.beforeReadback = func() error {
			path := filepath.Join(stageDir, pin.ReleaseName())
			if err := os.Chmod(path, 0o644); err != nil {
				return err
			}
			f, err := os.OpenFile(path, os.O_WRONLY, 0)
			if err != nil {
				return err
			}
			_, err = f.WriteAt([]byte{0}, 0)
			_ = f.Close()
			if err != nil {
				return err
			}
			return os.Chmod(path, 0o555)
		}
		seal, err := w.StageBinary(t.Context(), intent, bytesPrepared(pin, amd))
		assertStageErr(t, seal, err, domain.ErrProbeBinaryStageWriteUnknown, dir)
		if _, err := os.Lstat(filepath.Join(stageDir, domain.ProbeBinaryStageMarkerName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("marker published after corrupt readback", err)
		}

		w.beforeReadback = nil
		id = "11111111-1111-4111-8111-111111111125"
		intent = stageIntent(pin, id)
		stageDir = filepath.Join(dir, "uptime-phoenix-probe-"+id)
		w.beforeMarker = func() error { return errors.New("injected " + dir) }
		seal, err = w.StageBinary(t.Context(), intent, bytesPrepared(pin, amd))
		assertStageErr(t, seal, err, domain.ErrProbeBinaryStageWriteUnknown, dir)
		if _, err := os.Lstat(filepath.Join(stageDir, domain.ProbeBinaryStageMarkerName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("marker published after hook", err)
		}
		if _, err := os.Lstat(filepath.Join(stageDir, pin.ReleaseName())); err != nil {
			t.Fatal("binary removed after unknown", err)
		}

		w.beforeMarker = nil
		id = "11111111-1111-4111-8111-111111111126"
		intent = stageIntent(pin, id)
		w.syncFault = func(target string) error {
			if target == localStageTargetParent {
				return errors.New("injected sync " + dir)
			}
			return nil
		}
		seal, err = w.StageBinary(t.Context(), intent, bytesPrepared(pin, amd))
		assertStageErr(t, seal, err, domain.ErrProbeBinaryStageWriteUnknown, dir)
		w.syncFault = nil

		id = "11111111-1111-4111-8111-111111111127"
		intent = stageIntent(pin, id)
		w.closeFault = func(target string) error {
			if target == localStageTargetBinary {
				return errors.New("injected close " + dir)
			}
			return nil
		}
		seal, err = w.StageBinary(t.Context(), intent, bytesPrepared(pin, amd))
		assertStageErr(t, seal, err, domain.ErrProbeBinaryStageWriteUnknown, dir)
		w.closeFault = nil

		id = "11111111-1111-4111-8111-111111111128"
		intent = stageIntent(pin, id)
		stageDir = filepath.Join(dir, "uptime-phoenix-probe-"+id)
		w.beforeDirCheck = func() error {
			return os.WriteFile(filepath.Join(stageDir, "extra"), []byte("x"), 0o600)
		}
		seal, err = w.StageBinary(t.Context(), intent, bytesPrepared(pin, amd))
		assertStageErr(t, seal, err, domain.ErrProbeBinaryStageWriteUnknown, "")
		if _, err := os.Lstat(filepath.Join(stageDir, "extra")); err != nil {
			t.Fatal("unknown entry cleaned", err)
		}
		w.beforeDirCheck = nil

		id = "11111111-1111-4111-8111-111111111129"
		intent = stageIntent(pin, id)
		stageDir = filepath.Join(dir, "uptime-phoenix-probe-"+id)
		w.afterChildFD = func() error {
			if err := os.Rename(stageDir, stageDir+".old"); err != nil {
				return err
			}
			return os.Mkdir(stageDir, 0o700)
		}
		seal, err = w.StageBinary(t.Context(), intent, bytesPrepared(pin, amd))
		assertStageErr(t, seal, err, domain.ErrProbeBinaryStageWriteUnknown, "")
		entries, err := os.ReadDir(stageDir)
		if err != nil || len(entries) != 0 {
			t.Fatal("replacement directory received writes", err, entries)
		}
		w.afterChildFD = nil

		id = "11111111-1111-4111-8111-11111111112a"
		w.writeChunk = func(f *os.File, p []byte) (int, error) { return f.Write(p[:1]) }
		seal, err = w.StageBinary(t.Context(), stageIntent(pin, id), bytesPrepared(pin, amd))
		if err != nil || !seal.Valid() {
			t.Fatal("short writes were not completed", err)
		}
		got, err := os.ReadFile(filepath.Join(dir, seal.DirectoryName(), pin.ReleaseName()))
		if err != nil || !bytes.Equal(got, amd) {
			t.Fatal("short write lost bytes", err)
		}
		w.writeChunk = nil

		id = "11111111-1111-4111-8111-11111111112b"
		stream := &stageStream{Reader: bytes.NewReader(amd), closeErr: errors.New("close " + dir)}
		prep := &stagePrepared{pin: pin, open: func(context.Context) (io.ReadCloser, error) { return stream, nil }}
		seal, err = w.StageBinary(t.Context(), stageIntent(pin, id), prep)
		assertStageErr(t, seal, err, domain.ErrProbeBinaryStageWriteUnknown, dir)
		if _, err := os.Lstat(filepath.Join(dir, "uptime-phoenix-probe-"+id, domain.ProbeBinaryStageMarkerName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("marker published after source close error", err)
		}
	})

	t.Run("concurrent", func(t *testing.T) {
		dir, parent := stageParent(t)
		w, err := NewLocalBinaryStageWriter(t.Context(), parent)
		if err != nil {
			t.Fatal(err)
		}
		pin := artifactPin(arm, "arm64")
		id := "11111111-1111-4111-8111-11111111112c"
		intent := stageIntent(pin, id)
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := w.StageBinary(t.Context(), intent, bytesPrepared(pin, arm))
				errs <- err
			}()
		}
		wg.Wait()
		close(errs)
		var success, conflict int
		for err := range errs {
			switch err {
			case nil:
				success++
			case domain.ErrProbeBinaryStageWriteConflict:
				conflict++
			default:
				t.Fatal(err)
			}
		}
		if success != 1 || conflict != 1 {
			t.Fatal("concurrent result", success, conflict)
		}
		got, err := os.ReadFile(filepath.Join(dir, "uptime-phoenix-probe-"+id, pin.ReleaseName()))
		if err != nil || !bytes.Equal(got, arm) {
			t.Fatal("concurrent overwrite", err)
		}
	})

	t.Run("architecture", func(t *testing.T) {
		dir, parent := stageParent(t)
		w, err := NewLocalBinaryStageWriter(t.Context(), parent)
		if err != nil {
			t.Fatal(err)
		}
		pin := artifactPin(arm, "amd64")
		id := "11111111-1111-4111-8111-11111111112d"
		seal, err := w.StageBinary(t.Context(), stageIntent(pin, id), bytesPrepared(pin, arm))
		assertStageErr(t, seal, err, domain.ErrProbeBinaryStageWriteUnknown, dir)
	})
}

func pinFor(payload []byte) domain.ProbeBinaryPin {
	sum := sha256.Sum256(payload)
	return domain.ProbeBinaryPin{Version: "1.2.3", Architecture: "amd64", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(payload))}
}

type countingPrepared struct {
	stagePrepared
	stream *stageStream
}

func bytesPrepared(pin domain.ProbeBinaryPin, payload []byte) *countingPrepared {
	stream := &stageStream{Reader: bytes.NewReader(payload)}
	return &countingPrepared{stagePrepared: stagePrepared{pin: pin, open: func(context.Context) (io.ReadCloser, error) {
		return stream, nil
	}}, stream: stream}
}

func assertStageMode(t *testing.T, path string, perm os.FileMode, regular bool) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != perm || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("%s mode %o", path, info.Mode())
	}
	if regular != info.Mode().IsRegular() || (!regular && !info.IsDir()) {
		t.Fatalf("%s type %v", path, info.Mode())
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (regular && st.Nlink != 1) {
		t.Fatal("unexpected identity")
	}
}
