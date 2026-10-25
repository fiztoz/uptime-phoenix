//go:build linux || darwin

package provisioning

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type receiveEffectWriter struct {
	inner  ports.ProbeBinaryStageWriter
	before func()
	calls  int
}

func (w *receiveEffectWriter) StageBinary(ctx context.Context, intent domain.ProbeBinaryStageIntent, p ports.PreparedProbeBinary) (domain.ProbeBinaryStageSeal, error) {
	w.calls++
	if w.before != nil {
		w.before()
	}
	return w.inner.StageBinary(ctx, intent, p)
}

func receiveEffectService(t *testing.T, w ports.ProbeBinaryStageWriter) (*services.ProbeBinaryStageReceiveService, string) {
	t.Helper()
	scratch := t.TempDir()
	s, err := services.NewProbeBinaryStageReceiveService(NewProbeBinaryPreparer(scratch), w)
	if err != nil {
		t.Fatal(err)
	}
	return s, scratch
}
func assertReceiveEmpty(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		items, err := os.ReadDir(p)
		if err != nil || len(items) != 0 {
			t.Fatal("receive left pre-stage effects/private snapshots", p, err)
		}
	}
}

func TestLocalBinaryStageReceiveEffects(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			data := localStageFixture(t, arch)
			parent, fd, w, _ := localStageService(t)
			input := &stageStream{Reader: bytes.NewReader(data)}
			wrapped := &receiveEffectWriter{inner: w, before: func() {
				if input.closes != 1 {
					t.Fatal("writer invoked before owned source closed/joined")
				}
			}}
			s, scratch := receiveEffectService(t, wrapped)
			intent := stageIntent(artifactPin(data, arch), "11111111-1111-4111-8111-111111111141")
			canary := filepath.Join(parent, "must-not-execute")
			t.Setenv("PHOENIX_ARTIFACT_EXECUTION_CANARY", canary)
			seal, err := s.Receive(t.Context(), intent, input)
			if err != nil || seal.Intent != intent || !seal.Valid() {
				t.Fatal("receive did not stage exact verified bytes", err)
			}
			if input.closes != 1 || wrapped.calls != 1 {
				t.Fatal("receive source/stage count", input.closes, wrapped.calls)
			}
			assertLocalStageGraph(t, parent, intent, data)
			assertReceiveEmpty(t, scratch)
			if _, err := fd.Stat(); err != nil {
				t.Fatal("receive closed borrowed parent", err)
			}
			if _, err := os.Stat(canary); !os.IsNotExist(err) {
				t.Fatal("receive executed artifact", err)
			}
			next := &stageStream{Reader: bytes.NewReader(data)}
			// The callback is deliberately tied to the currently owned input.
			input = next
			seal, err = s.Receive(t.Context(), intent, next)
			if err != domain.ErrProbeBinaryStageWriteConflict || seal != (domain.ProbeBinaryStageSeal{}) || next.closes != 1 {
				t.Fatal("receive adopted existing stage", err)
			}
			assertLocalStageGraph(t, parent, intent, data)
			assertReceiveEmpty(t, scratch)
		})
	}
}

func TestLocalBinaryStageReceiveRefusals(t *testing.T) {
	data := localStageFixture(t, "amd64")
	for _, mode := range []string{"short", "extra", "pause_extra", "no_progress", "bad_digest", "bad_elf", "bad_intent", "source_close", "late_source_error", "mixed_eof_error"} {
		t.Run(mode, func(t *testing.T) {
			parent, _, w, _ := localStageService(t)
			wrapped := &receiveEffectWriter{inner: w}
			s, scratch := receiveEffectService(t, wrapped)
			intent := stageIntent(artifactPin(data, "amd64"), "11111111-1111-4111-8111-111111111142")
			input := &stageStream{Reader: bytes.NewReader(data)}
			want := domain.ErrProbeArtifactSize
			switch mode {
			case "short":
				input.Reader = bytes.NewReader(data[:len(data)-1])
			case "extra":
				input.Reader = io.MultiReader(bytes.NewReader(data), bytes.NewReader([]byte{1}))
			case "pause_extra":
				input.Reader = &localStageEOFPause{Reader: bytes.NewReader(data)}
			case "no_progress":
				input.Reader = &localStageNoProgress{}
				want = domain.ErrProbeArtifactStorage
			case "bad_digest":
				intent.Identity.Binary.SHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
				want = domain.ErrProbeArtifactDigest
			case "bad_elf":
				bad := append([]byte(nil), data...)
				bad[0] = 0
				intent.Identity.Binary = artifactPin(bad, "amd64")
				input.Reader = bytes.NewReader(bad)
				want = domain.ErrProbeArtifactMetadata
			case "bad_intent":
				intent.AttemptID = "not-a-uuid"
				want = domain.ErrProbeBinaryStageWriteInput
			case "source_close":
				input.closeErr = errors.New("private-source-close")
				want = domain.ErrProbeArtifactStorage
			case "late_source_error":
				input.Reader = io.MultiReader(bytes.NewReader(data), receiveEffectErrorReader{errors.New("private-source-read")})
				want = domain.ErrProbeArtifactStorage
			case "mixed_eof_error":
				input.Reader = io.MultiReader(bytes.NewReader(data), receiveEffectErrorReader{errors.Join(io.EOF, errors.New("private-source-read"))})
				want = domain.ErrProbeArtifactStorage
			}
			seal, err := s.Receive(t.Context(), intent, input)
			if err != want || seal != (domain.ProbeBinaryStageSeal{}) {
				t.Fatal("receive pre-stage refusal masked input error", err, "want", want)
			}
			if wrapped.calls != 0 || input.closes != 1 {
				t.Fatal("invalid input invoked writer or leaked source", wrapped.calls, input.closes)
			}
			assertReceiveEmpty(t, parent, scratch)
		})
	}
}

type receiveEffectErrorReader struct{ err error }

func (r receiveEffectErrorReader) Read([]byte) (int, error) { return 0, r.err }

type receiveEffectBlockedEOF struct {
	*bytes.Reader
	entered      chan struct{}
	readDone     chan struct{}
	closeEntered chan struct{}
	releaseClose chan struct{}
	once         sync.Once
}

func (r *receiveEffectBlockedEOF) Read(p []byte) (int, error) {
	if r.Reader.Len() > 0 {
		return r.Reader.Read(p)
	}
	r.once.Do(func() { close(r.entered) })
	<-r.readDone
	return 0, os.ErrClosed
}
func (r *receiveEffectBlockedEOF) Close() error {
	close(r.closeEntered)
	close(r.readDone)
	<-r.releaseClose
	return nil
}
func TestLocalBinaryStageReceiveEOFAndCloseJoin(t *testing.T) {
	data := localStageFixture(t, "amd64")
	parent, _, w, _ := localStageService(t)
	wrapped := &receiveEffectWriter{inner: w}
	s, scratch := receiveEffectService(t, wrapped)
	input := &receiveEffectBlockedEOF{Reader: bytes.NewReader(data), entered: make(chan struct{}), readDone: make(chan struct{}), closeEntered: make(chan struct{}), releaseClose: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(input.releaseClose) }) }
	t.Cleanup(release)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	intent := stageIntent(artifactPin(data, "amd64"), "11111111-1111-4111-8111-111111111143")
	done := make(chan error, 1)
	go func() {
		seal, err := s.Receive(ctx, intent, input)
		if seal != (domain.ProbeBinaryStageSeal{}) {
			done <- errors.New("receive returned seal without EOF")
			return
		}
		done <- err
	}()
	select {
	case <-input.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("receive did not ask for real outer EOF")
	}
	cancel()
	select {
	case <-input.closeEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not close receive input")
	}
	select {
	case err := <-done:
		t.Fatal("receive returned before input close joined", err)
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatal("receive EOF cancel classification", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("receive did not join canceled EOF")
	}
	if wrapped.calls != 0 {
		t.Fatal("receive staged without confirmed EOF")
	}
	assertReceiveEmpty(t, parent, scratch)
}

func TestLocalBinaryStageReceiveUnknownEffects(t *testing.T) {
	data := localStageFixture(t, "amd64")
	parent, _, w, _ := localStageService(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w.beforeMarker = func() error { cancel(); return nil }
	s, scratch := receiveEffectService(t, w)
	intent := stageIntent(artifactPin(data, "amd64"), "11111111-1111-4111-8111-111111111144")
	input := &stageStream{Reader: bytes.NewReader(data)}
	seal, err := s.Receive(ctx, intent, input)
	if err != domain.ErrProbeBinaryStageWriteUnknown || seal != (domain.ProbeBinaryStageSeal{}) {
		t.Fatal("receive hid effects after cancellation", err)
	}
	dir := filepath.Join(parent, (domain.ProbeBinaryStageSeal{Intent: intent}).DirectoryName())
	if _, err := os.Stat(filepath.Join(dir, intent.Identity.Binary.ReleaseName())); err != nil {
		t.Fatal("receive erased partial stage", err)
	}
	if _, err := os.Stat(filepath.Join(dir, domain.ProbeBinaryStageMarkerName)); !os.IsNotExist(err) {
		t.Fatal("receive marker after known cancel", err)
	}
	assertReceiveEmpty(t, scratch)
	if input.closes != 1 {
		t.Fatal("receive input close count", input.closes)
	}
}
