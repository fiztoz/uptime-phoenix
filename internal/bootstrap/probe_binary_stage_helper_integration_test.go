//go:build linux || darwin

package bootstrap

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
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// goToolPath resolves the go command from PATH. runtime.GOROOT is deprecated
// because the build-time root is not meaningful once a binary moves hosts.
func goToolPath(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("locating the go command: %v", err)
	}
	return path
}

func helperEffectFixture(t *testing.T, arch string) []byte {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"go.mod":                      "module github.com/fiztoz/uptime-phoenix\n\ngo 1.23\n",
		"internal/version/version.go": "package version\nvar Version=\"dev\"\n",
		"cmd/probe/main.go": `package main
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
	cmd := exec.CommandContext(ctx, goToolPath(t), "build", "-trimpath", "-ldflags=-s -w -X github.com/fiztoz/uptime-phoenix/internal/version.Version=1.2.3", "-o", "fixture", "./cmd/probe")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0", "GOENV=off", "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "GOWORK=off", "GOFLAGS=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture build: %v %s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(dir, "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func helperEffectIntent(t *testing.T, data []byte, arch string) domain.ProbeBinaryStageIntent {
	t.Helper()
	intent := helperIntent()
	sum := sha256.Sum256(data)
	intent.Identity.Binary = domain.ProbeBinaryPin{Version: "1.2.3", Architecture: arch, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))}
	if !intent.Valid() {
		t.Fatal("effect fixture intent")
	}
	return intent
}

func helperEffectFrame(t *testing.T, intent domain.ProbeBinaryStageIntent, data []byte) []byte {
	t.Helper()
	header, err := provisioning.EncodeProbeBinaryStageHeader(intent)
	if err != nil {
		t.Fatal(err)
	}
	return append(header, data...)
}

func helperEffectParent(t *testing.T) (string, *os.File, string) {
	t.Helper()
	parent := t.TempDir()
	fd, err := os.Open(parent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fd.Close() })
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	return parent, fd, scratch
}

func helperEffectEmpty(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		entries, err := os.ReadDir(p)
		if err != nil || len(entries) != 0 {
			t.Fatal("helper retained pre-stage effects/private snapshot", err)
		}
	}
}

func helperEffectGraph(t *testing.T, parent string, intent domain.ProbeBinaryStageIntent, data []byte) {
	t.Helper()
	name := (domain.ProbeBinaryStageSeal{Intent: intent}).DirectoryName()
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 1 || entries[0].Name() != name {
		t.Fatal("helper did not create exact relative directory", err)
	}
	dir := filepath.Join(parent, name)
	entries, err = os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatal("helper stage graph incomplete", err)
	}
	for filename, want := range map[string][]byte{intent.Identity.Binary.ReleaseName(): data, domain.ProbeBinaryStageMarkerName: []byte(intent.Marker())} {
		p := filepath.Join(dir, filename)
		info, err := os.Lstat(p)
		perm := os.FileMode(0555)
		if filename == domain.ProbeBinaryStageMarkerName {
			perm = 0444
		}
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != perm {
			t.Fatal("helper stage file/mode", err)
		}
		got, err := os.ReadFile(p)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("helper stage bytes differ", err)
		}
	}
}

func TestProbeBinaryStageHelperActualPipeline(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			data := helperEffectFixture(t, arch)
			intent := helperEffectIntent(t, data, arch)
			parent, fd, scratch := helperEffectParent(t)
			canary := filepath.Join(t.TempDir(), "must-not-execute")
			t.Setenv("PHOENIX_ARTIFACT_EXECUTION_CANARY", canary)
			r := bytes.NewReader(helperEffectFrame(t, intent, data))
			input := &helperRC{readFn: r.Read}
			var out, stderr bytes.Buffer
			if code := RunProbeBinaryStageHelper(t.Context(), nil, fd, input, &out, &stderr); code != 0 || stderr.Len() != 0 || input.closes.Load() != 1 || !bytes.Contains(out.Bytes(), []byte(intent.MarkerSHA256())) {
				t.Fatal("actual helper stage/receipt failed", code, stderr.String())
			}
			helperEffectGraph(t, parent, intent, data)
			helperEffectEmpty(t, scratch)
			if _, err := fd.Stat(); err != nil {
				t.Fatal("helper closed borrowed parent", err)
			}
			// The canary can catch execution only when these Linux bytes are
			// executable on this host. Darwin absence is not execution evidence.
			if runtime.GOOS == "linux" && runtime.GOARCH == arch {
				if _, err := os.Stat(canary); !os.IsNotExist(err) {
					t.Fatal("helper executed artifact", err)
				}
			}
			out.Reset()
			stderr.Reset()
			r = bytes.NewReader(helperEffectFrame(t, intent, data))
			input = &helperRC{readFn: r.Read}
			if code := RunProbeBinaryStageHelper(t.Context(), nil, fd, input, &out, &stderr); code != 1 || out.Len() != 0 || stderr.String() != domain.ErrProbeBinaryStageWriteConflict.Error()+"\n" || input.closes.Load() != 1 {
				t.Fatal("helper adopted existing graph", code, stderr.String())
			}
			helperEffectGraph(t, parent, intent, data)
			helperEffectEmpty(t, scratch)
		})
	}
}

type helperEffectWriteFunc func([]byte) (int, error)

func (f helperEffectWriteFunc) Write(p []byte) (int, error) { return f(p) }

func TestProbeBinaryStageHelperRealEffectFailures(t *testing.T) {
	data := helperEffectFixture(t, "amd64")
	for _, mode := range []string{"close", "short_output", "private_output", "cancel_output", "extra", "short", "header_private", "header_no_progress", "invalid_count", "mixed_header_eof"} {
		t.Run(mode, func(t *testing.T) {
			intent := helperEffectIntent(t, data, "amd64")
			parent, fd, scratch := helperEffectParent(t)
			frame := helperEffectFrame(t, intent, data)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			r := bytes.NewReader(frame)
			input := &helperRC{readFn: r.Read}
			var out, stderr bytes.Buffer
			var sink io.Writer = &out
			staged := false
			want := domain.ErrProbeArtifactStorage
			switch mode {
			case "close":
				input.closeErr = errors.New("private-source-canary")
			case "short_output":
				staged = true
				want = domain.ErrProbeBinaryStageWriteUnknown
				sink = helperEffectWriteFunc(func(p []byte) (int, error) { return len(p) - 1, nil })
			case "private_output":
				staged = true
				want = domain.ErrProbeBinaryStageWriteUnknown
				sink = helperEffectWriteFunc(func([]byte) (int, error) { return 0, errors.New("private-output-canary") })
			case "cancel_output":
				staged = true
				want = domain.ErrProbeBinaryStageWriteUnknown
				sink = helperEffectWriteFunc(func(p []byte) (int, error) { cancel(); return out.Write(p) })
			case "extra":
				input.readFn = bytes.NewReader(append(frame, 1)).Read
				want = domain.ErrProbeArtifactSize
			case "short":
				input.readFn = bytes.NewReader(frame[:len(frame)-1]).Read
				want = domain.ErrProbeArtifactSize
			case "header_private":
				input.readFn = func([]byte) (int, error) { return 0, errors.New("private-header-canary") }
			case "header_no_progress":
				input.readFn = func([]byte) (int, error) { return 0, nil }
			case "invalid_count":
				input.readFn = func([]byte) (int, error) { return -1, nil }
			case "mixed_header_eof":
				// Fault only the first header read. Repeating it on body reads
				// would let the preparer mask a missing header EOF guard.
				first := true
				input.readFn = func(p []byte) (int, error) {
					n, err := r.Read(p)
					if first {
						first = false
						return n, errors.Join(io.EOF, errors.New("private-header-canary"))
					}
					return n, err
				}
			}
			if code := RunProbeBinaryStageHelper(ctx, nil, fd, input, sink, &stderr); code != 1 || stderr.String() != want.Error()+"\n" || input.closes.Load() != 1 || strings.Contains(stderr.String(), "canary") {
				t.Fatal("helper failure lost uncertainty/refusal/secrecy", code, stderr.String(), input.closes.Load())
			}
			if staged {
				helperEffectGraph(t, parent, intent, data)
			} else {
				helperEffectEmpty(t, parent)
				if out.Len() != 0 {
					t.Fatal("pre-stage failure emitted receipt")
				}
			}
			helperEffectEmpty(t, scratch)
			if _, err := fd.Stat(); err != nil {
				t.Fatal("failure closed borrowed parent", err)
			}
		})
	}
}

func TestProbeBinaryStageHelperWaitsForRealEOF(t *testing.T) {
	data := helperEffectFixture(t, "amd64")
	intent := helperEffectIntent(t, data, "amd64")
	parent, fd, scratch := helperEffectParent(t)
	frame := helperEffectFrame(t, intent, data)
	pr, pw := io.Pipe()
	defer func() { _ = pr.Close() }()
	defer func() { _ = pw.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{})
	var enteredOnce sync.Once
	remaining := len(frame)
	input := &helperRC{readFn: func(p []byte) (int, error) {
		if remaining == 0 {
			enteredOnce.Do(func() { close(entered) })
		}
		n, err := pr.Read(p)
		remaining -= n
		return n, err
	}, closeFn: pr.Close}
	writerDone := make(chan error, 1)
	go func() { _, err := pw.Write(frame); writerDone <- err }()
	var out, stderr bytes.Buffer
	done := make(chan int, 1)
	helperJoined, writerJoined := false, false
	t.Cleanup(func() {
		cancel()
		_ = pw.Close()
		if !helperJoined {
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("helper cleanup not joined")
			}
		}
		if !writerJoined {
			select {
			case <-writerDone:
			case <-time.After(time.Second):
				t.Error("writer cleanup not joined")
			}
		}
	})
	go func() { done <- RunProbeBinaryStageHelper(ctx, nil, fd, input, &out, &stderr) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not reach outer EOF")
	}
	select {
	case code := <-done:
		helperJoined = true
		t.Fatal("helper staged without real EOF", code)
	case <-time.After(50 * time.Millisecond):
	}
	helperEffectEmpty(t, parent)
	cancel()
	select {
	case code := <-done:
		helperJoined = true
		if code != 1 || stderr.String() != domain.ErrProbeBinaryStageWriteUnknown.Error()+"\n" || out.Len() != 0 {
			t.Fatal("helper withheld EOF cancellation acknowledged", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not join canceled stream")
	}
	select {
	case err := <-writerDone:
		writerJoined = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("fixture writer not joined")
	}
	if input.closes.Load() != 1 {
		t.Fatal("source close count", input.closes.Load())
	}
	helperEffectEmpty(t, parent, scratch)
}

func helperEffectFlags(t *testing.T, file *os.File) int {
	t.Helper()
	control, err := file.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var flags int
	var inner error
	if err := control.Control(func(fd uintptr) { flags, inner = unix.FcntlInt(fd, unix.F_GETFL, 0) }); err != nil || inner != nil {
		t.Fatal("get flags", err, inner)
	}
	return flags
}

func TestProbeBinaryStageHelperPollableInput(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pw.Close() }()
	// Fd deliberately models exec's inherited blocking stdin. Retain a second
	// descriptor only to observe restoration after the owned original closes.
	original := pr.Fd()
	fd, err := unix.FcntlInt(original, unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	observer := os.NewFile(uintptr(fd), "flag-observer")
	defer func() { _ = observer.Close() }()
	before := helperEffectFlags(t, observer)
	if before&unix.O_NONBLOCK != 0 {
		t.Fatal("fixture did not model blocking inherited stdin")
	}
	source, err := inheritedProbeBinaryStageInput(t.Context(), pr)
	if err != nil {
		t.Fatal(err)
	}
	if helperEffectFlags(t, observer)&unix.O_NONBLOCK == 0 {
		t.Fatal("stdin duplicate was not pollable")
	}
	readDone := make(chan error, 1)
	go func() { _, err := source.Read(make([]byte, 1)); readDone <- err }()
	closeDone := make(chan error, 1)
	go func() { closeDone <- source.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		_ = pw.Close()
		t.Fatal("pollable stdin Close did not unblock read")
	}
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("closed input read succeeded")
		}
	case <-time.After(time.Second):
		_ = pw.Close()
		t.Fatal("pollable stdin read did not join")
	}
	if helperEffectFlags(t, observer) != before {
		t.Fatal("stdin status flags not restored")
	}
	if _, err := pr.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("owned stdin original retained", err)
	}
	if source.Close() != nil {
		t.Fatal("pollable stdin close not idempotent")
	}
	if _, err := observer.Stat(); err != nil {
		t.Fatal("pollable wrapper closed borrowed observer")
	}

	regular, err := os.CreateTemp(t.TempDir(), "regular")
	if err != nil {
		t.Fatal(err)
	}
	owned, err := inheritedProbeBinaryStageInput(t.Context(), regular)
	if err != nil || owned != regular {
		t.Fatal("regular stdin unnecessarily rewritten", err)
	}
	if owned.Close() != nil {
		t.Fatal("regular stdin close")
	}
	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if owned, err = inheritedProbeBinaryStageInput(t.Context(), dir); owned != nil || err != domain.ErrProbeBinaryStageWriteInput {
		t.Fatal("directory stdin accepted", err)
	}
	if _, err := dir.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("refused stdin not closed")
	}
}

func TestProbeBinaryStageHelperInheritedCompletion(t *testing.T) {
	payload, err := probeBinaryStageHelperReceipt(domain.ProbeBinaryStageSeal{Intent: helperIntent()})
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"failed_status", "parent_close", "canceled", "private_diagnostic", "short_output"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var out, stderr bytes.Buffer
			var sink io.Writer = &out
			code := 0
			var closeErr error
			diagnostic := ""
			want := domain.ErrProbeBinaryStageWriteUnknown
			switch mode {
			case "failed_status":
				code = 1
				diagnostic = domain.ErrProbeArtifactStorage.Error() + "\n"
				want = domain.ErrProbeArtifactStorage
			case "parent_close":
				closeErr = errors.New("private-close-canary")
			case "canceled":
				cancel()
			case "private_diagnostic":
				code = 1
				diagnostic = "private-diagnostic-canary"
			case "short_output":
				sink = helperEffectWriteFunc(func(p []byte) (int, error) { return len(p) - 1, nil })
			}
			if code := finishInheritedProbeBinaryStageHelper(ctx, code, closeErr, payload, diagnostic, sink, &stderr); code != 1 || stderr.String() != want.Error()+"\n" || out.Len() != 0 {
				t.Fatal("inherited completion flushed failed receipt/private diagnostic", code, stderr.String())
			}
		})
	}
}

func TestProbeBinaryStageHelperGuardActiveReadAndCallbackTail(t *testing.T) {
	t.Run("active_read", func(t *testing.T) {
		entered := make(chan struct{})
		release := make(chan struct{})
		closed := make(chan struct{})
		var once sync.Once
		t.Cleanup(func() { once.Do(func() { close(release) }) })
		raw := &helperRC{readFn: func([]byte) (int, error) { close(entered); <-release; return 0, io.EOF }, closeFn: func() error { close(closed); return nil }}
		guard, err := newProbeBinaryStageHelperInput(t.Context(), raw)
		if err != nil {
			t.Fatal(err)
		}
		readDone := make(chan struct{})
		go func() { _, _ = guard.Read(make([]byte, 1)); close(readDone) }()
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("read not entered")
		}
		done := make(chan error, 1)
		go func() { done <- guard.Close() }()
		select {
		case <-closed:
		case <-time.After(time.Second):
			t.Fatal("raw close not entered")
		}
		select {
		case <-done:
			t.Fatal("guard close returned before active read joined")
		case <-time.After(50 * time.Millisecond):
		}
		once.Do(func() { close(release) })
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("guard close blocked")
		}
		select {
		case <-readDone:
		case <-time.After(time.Second):
			t.Fatal("read goroutine not joined")
		}
	})
	t.Run("callback_tail", func(t *testing.T) {
		// Model preemption after closeRaw returns but before the callback's
		// deferred close(joined). Raw Close alone cannot prove this tail joined.
		joined := make(chan struct{})
		var once sync.Once
		t.Cleanup(func() { once.Do(func() { close(joined) }) })
		raw := &helperRC{}
		stopEntered := make(chan struct{})
		guard := &probeBinaryStageHelperInput{ctx: t.Context(), raw: raw, joined: joined, stop: func() bool { close(stopEntered); return false }}
		guard.closeRaw()
		done := make(chan error, 1)
		go func() { done <- guard.Close() }()
		select {
		case <-stopEntered:
		case <-done:
			t.Fatal("guard close returned before callback tail joined")
		case <-time.After(time.Second):
			t.Fatal("callback stop not reached")
		}
		select {
		case <-done:
			t.Fatal("guard close returned before callback tail joined")
		case <-time.After(50 * time.Millisecond):
		}
		once.Do(func() { close(joined) })
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("callback tail not joined")
		}
		if raw.closes.Load() != 1 || guard.Close() != nil {
			t.Fatal("guard repeat close")
		}
	})
}
