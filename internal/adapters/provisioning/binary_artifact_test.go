package provisioning

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func buildArtifactFixture(t *testing.T, goos, arch, pkg, cgo string) []byte {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"go.mod":                      "module github.com/fiztoz/uptime-phoenix\n\ngo 1.23\n",
		"internal/version/version.go": "package version\nvar Version=\"dev\"\n",
		pkg + "/main.go": `package main
import("os";"github.com/fiztoz/uptime-phoenix/internal/version")
func main(){if p:=os.Getenv("PHOENIX_ARTIFACT_EXECUTION_CANARY");p!=""{_=os.WriteFile(p,[]byte("executed"),0600)};println(version.Version)}
`,
	} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-trimpath", "-ldflags=-s -w -X github.com/fiztoz/uptime-phoenix/internal/version.Version=0.4.5", "-o", "fixture", "./"+pkg)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+arch, "CGO_ENABLED="+cgo, "GOWORK=off", "GOENV=off", "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "GOFLAGS=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture build: %v: %s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(dir, "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func artifactPin(data []byte, arch string) domain.ProbeBinaryPin {
	h := sha256.Sum256(data)
	return domain.ProbeBinaryPin{Version: "0.4.5", Architecture: arch, SHA256: hex.EncodeToString(h[:]), Size: int64(len(data))}
}

type countingArtifactInput struct {
	io.Reader
	closes atomic.Int32
}

func (r *countingArtifactInput) Close() error { r.closes.Add(1); return nil }

func TestProbeBinaryArtifact(t *testing.T) {
	amd := buildArtifactFixture(t, "linux", "amd64", "cmd/probe", "0")
	arm := buildArtifactFixture(t, "linux", "arm64", "cmd/probe", "0")
	hub := buildArtifactFixture(t, "linux", "amd64", "cmd/app", "0")
	cgo := buildArtifactFixture(t, "linux", "amd64", "cmd/probe", "1")
	otherOS := buildArtifactFixture(t, "darwin", "amd64", "cmd/probe", "0")
	canary := filepath.Join(t.TempDir(), "must-not-execute")
	t.Setenv("PHOENIX_ARTIFACT_EXECUTION_CANARY", canary)
	for name, data := range map[string][]byte{"amd64": amd, "arm64": arm} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			pin := artifactPin(data, name)
			sourcePath := filepath.Join(t.TempDir(), "source")
			if err := os.WriteFile(sourcePath, data, 0600); err != nil {
				t.Fatal(err)
			}
			source, err := OpenProbeBinarySource(t.Context(), sourcePath)
			if err != nil {
				t.Fatal(err)
			}
			value, err := NewProbeBinaryPreparer(root).PrepareBinary(t.Context(), pin, source)
			if err != nil {
				t.Fatal(err)
			}
			snapshot := value.(*preparedProbeBinary)
			info, err := os.Stat(snapshot.dir)
			if err != nil || info.Mode().Perm() != 0700 {
				t.Fatal("nonprivate directory")
			}
			info, err = os.Stat(filepath.Join(snapshot.dir, "probe"))
			if err != nil || info.Mode().Perm() != 0400 {
				t.Fatal("nonprivate snapshot")
			}
			if err := os.WriteFile(sourcePath, []byte("changed original"), 0600); err != nil {
				t.Fatal(err)
			}
			if snapshot.Pin() != pin {
				t.Fatal("pin changed")
			}
			reader, err := snapshot.Open(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(reader)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatal("snapshot borrowed original", err)
			}
			_ = reader.Close()
			ctx, cancel := context.WithCancel(t.Context())
			reader, err = snapshot.Open(ctx)
			if err != nil {
				t.Fatal(err)
			}
			cancel()
			if _, err := reader.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
				t.Fatal("reader ignored context", err)
			}
			_ = reader.Close()
			reader, err = snapshot.Open(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if err := snapshot.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := reader.Read(make([]byte, 1)); !errors.Is(err, domain.ErrProbeArtifactClosed) {
				t.Fatal("reader survived cleanup", err)
			}
			if _, err := snapshot.Open(t.Context()); !errors.Is(err, domain.ErrProbeArtifactClosed) {
				t.Fatal("reopened disposed snapshot")
			}
			if err := snapshot.Close(t.Context()); err != nil {
				t.Fatal("non-idempotent cleanup")
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatal("snapshot leaked")
			}
		})
	}
	for _, scenario := range []string{"digest", "short", "extra", "architecture", "component", "cgo", "other_os", "malformed", "truncated", "non_executable", "invalid_pin", "source_error"} {
		t.Run(scenario, func(t *testing.T) {
			data := append([]byte(nil), amd...)
			pin := artifactPin(data, "amd64")
			switch scenario {
			case "digest":
				pin.SHA256 = strings.Repeat("0", 64)
			case "short":
				pin.Size++
			case "extra":
				pin.Size--
			case "architecture":
				pin.Architecture = "arm64"
			case "component":
				data = hub
				pin = artifactPin(data, "amd64")
			case "cgo":
				data = cgo
				pin = artifactPin(data, "amd64")
			case "other_os":
				data = otherOS
				pin = artifactPin(data, "amd64")
			case "malformed":
				data = []byte("not ELF")
				pin = artifactPin(data, "amd64")
			case "truncated":
				data = data[:64]
				pin = artifactPin(data, "amd64")
			case "non_executable":
				data[16] = 3
				pin = artifactPin(data, "amd64")
			case "invalid_pin":
				pin.Version = "latest"
			}
			input := &countingArtifactInput{Reader: bytes.NewReader(data)}
			if scenario == "source_error" {
				input.Reader = artifactFailureReader{}
			}
			root := t.TempDir()
			prepared, err := NewProbeBinaryPreparer(root).PrepareBinary(t.Context(), pin, input)
			if err == nil || prepared != nil {
				t.Fatal("invalid bytes prepared")
			}
			if strings.Contains(err.Error(), "private-canary") {
				t.Fatal("source diagnostics escaped")
			}
			if input.closes.Load() != 1 {
				t.Fatal("input not closed exactly once")
			}
			entries, _ := os.ReadDir(root)
			if len(entries) != 0 {
				t.Fatal("partial snapshot leaked")
			}
		})
	}
	t.Run("blocked_read_cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		r, w := io.Pipe()
		defer func() { _ = w.Close() }()
		root := t.TempDir()
		done := make(chan error, 1)
		go func() {
			_, err := NewProbeBinaryPreparer(root).PrepareBinary(ctx, artifactPin(amd, "amd64"), r)
			done <- err
		}()
		if _, err := w.Write(amd[:1]); err != nil {
			t.Fatal(err)
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("blocked read did not join")
		}
		entries, _ := os.ReadDir(root)
		if len(entries) != 0 {
			t.Fatal("canceled snapshot leaked")
		}
	})
	t.Run("cleanup_refuses_replaced_directory", func(t *testing.T) {
		root := t.TempDir()
		value, err := NewProbeBinaryPreparer(root).PrepareBinary(t.Context(), artifactPin(amd, "amd64"), io.NopCloser(bytes.NewReader(amd)))
		if err != nil {
			t.Fatal(err)
		}
		snapshot := value.(*preparedProbeBinary)
		original := snapshot.dir
		if err := os.Rename(original, original+"-moved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(original, 0700); err != nil {
			t.Fatal(err)
		}
		keep := filepath.Join(original, "keep")
		if err := os.WriteFile(keep, []byte("unrelated"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := snapshot.Open(t.Context()); !errors.Is(err, domain.ErrProbeArtifactStorage) {
			t.Fatal("replacement read")
		}
		if err := snapshot.Close(t.Context()); !errors.Is(err, domain.ErrProbeArtifactStorage) {
			t.Fatal("replacement deleted")
		}
		if _, err := os.Stat(keep); err != nil {
			t.Fatal("unowned resource removed")
		}
	})
	t.Run("concurrent_readers_and_cleanup", func(t *testing.T) {
		value, err := NewProbeBinaryPreparer(t.TempDir()).PrepareBinary(t.Context(), artifactPin(amd, "amd64"), io.NopCloser(bytes.NewReader(amd)))
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				r, err := value.Open(ctx)
				if err == nil {
					_, _ = r.Read(make([]byte, 64))
					_ = r.Close()
				}
			}()
		}
		if err := value.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
	})
	t.Run("metadata_context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		input := &artifactCountingReaderAt{ReaderAt: bytes.NewReader(amd)}
		cancel()
		if validProbeBinary(ctx, input, artifactPin(amd, "amd64")) || input.reads != 0 {
			t.Fatal("metadata read after cancellation")
		}
		ctx, cancel = context.WithCancel(t.Context())
		defer cancel()
		input = &artifactCountingReaderAt{ReaderAt: bytes.NewReader(amd), after: cancel}
		if validProbeBinary(ctx, input, artifactPin(amd, "amd64")) || input.reads != 1 {
			t.Fatal("metadata reader ignored mid-parse cancellation")
		}
	})
	if _, err := os.Stat(canary); !os.IsNotExist(err) {
		t.Fatal("artifact executed")
	}
}

type artifactCountingReaderAt struct {
	io.ReaderAt
	reads int
	after func()
}

func (r *artifactCountingReaderAt) ReadAt(p []byte, offset int64) (int, error) {
	r.reads++
	n, err := r.ReaderAt.ReadAt(p, offset)
	if r.after != nil {
		r.after()
	}
	return n, err
}

type artifactFailureReader struct{}

func (artifactFailureReader) Read([]byte) (int, error) {
	return 0, errors.New("private-canary source diagnostic")
}
