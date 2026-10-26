package bootstrap

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

type helperRC struct {
	reads    atomic.Int32
	closes   atomic.Int32
	readFn   func([]byte) (int, error)
	closeFn  func() error
	closeErr error
}

func (r *helperRC) Read(p []byte) (int, error) {
	r.reads.Add(1)
	if r.readFn != nil {
		return r.readFn(p)
	}
	return 0, io.EOF
}

func (r *helperRC) Close() error {
	r.closes.Add(1)
	if r.closeFn != nil {
		return r.closeFn()
	}
	return r.closeErr
}

type helperCtx struct{ context.Context }

type helperCountWriter struct {
	calls atomic.Int32
	n     int
	err   error
	buf   bytes.Buffer
}

func (w *helperCountWriter) Write(p []byte) (int, error) {
	w.calls.Add(1)
	_, _ = w.buf.Write(p)
	if w.err != nil {
		if w.n > 0 && w.n < len(p) {
			return w.n, w.err
		}
		return 0, w.err
	}
	if w.n > 0 && w.n < len(p) {
		return w.n, nil
	}
	return len(p), nil
}

func helperIntent() domain.ProbeBinaryStageIntent {
	fp := "SHA256:" + base64.RawStdEncoding.EncodeToString(make([]byte, 32))
	return domain.ProbeBinaryStageIntent{
		Identity: domain.ProbeInstallIdentity{
			OperationID:          "11111111-1111-4111-8111-111111111111",
			Host:                 domain.ProbeSSHHost{Address: "example.test:22", User: "deploy"},
			HostKeyFingerprint:   fp,
			ClientKeyFingerprint: fp,
			RequestedBy:          1,
			LayoutVersion:        1,
			Mode:                 domain.ProbeInstallSystemd,
			Binary: domain.ProbeBinaryPin{
				Version:      "1.2.3",
				Architecture: "amd64",
				SHA256:       strings.Repeat("a", 64),
				Size:         17,
			},
		},
		OwnerID:   "22222222-2222-4222-8222-222222222222",
		AttemptID: "33333333-3333-4333-8333-333333333333",
	}
}

func TestRunProbeBinaryStageHelperUsageDoesNotEchoOrTouchParent(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"--path"}, {"--path=/opt/secret"}, {"sensitive-path-canary"}, {""}} {
		input := &helperRC{}
		var out, stderr bytes.Buffer
		code := RunProbeBinaryStageHelper(nil, args, &os.File{}, input, &out, &stderr) //nolint:staticcheck // SA1012: usage is decided before the context is inspected, so nil must not matter
		if code != 2 || out.Len() != 0 || input.closes.Load() != 1 || stderr.String() != probeBinaryStageUsage || strings.Contains(stderr.String(), "canary") || strings.Contains(stderr.String(), "/opt") {
			t.Fatalf("args %q code=%d out=%q err=%q closes=%d", args, code, out.String(), stderr.String(), input.closes.Load())
		}
	}
	var typed *helperRC
	var input io.ReadCloser = typed
	var stderr bytes.Buffer
	if RunProbeBinaryStageHelper(nil, []string{"--help"}, nil, input, io.Discard, &stderr) != 2 || stderr.String() != probeBinaryStageUsage { //nolint:staticcheck // SA1012: typed-nil input must still reach usage with a deliberately nil context
		t.Fatal("typed-nil usage dispatch")
	}
}

func TestRunInheritedProbeBinaryStageHelperRejectsArgsBeforeParent(t *testing.T) {
	input := &helperRC{}
	var out, stderr bytes.Buffer
	code := RunInheritedProbeBinaryStageHelper(nil, []string{"--help", "sensitive-path-canary"}, input, &out, &stderr) //nolint:staticcheck // SA1012: args are refused before the context is inspected, so nil must not matter
	if code != 2 || out.Len() != 0 || input.closes.Load() != 1 || stderr.String() != probeBinaryStageUsage || strings.Contains(stderr.String(), "canary") {
		t.Fatalf("inherited args code=%d out=%q err=%q closes=%d", code, out.String(), stderr.String(), input.closes.Load())
	}
}

func TestRunProbeBinaryStageHelperRefusesNilWithoutDispatch(t *testing.T) {
	var typedCtx context.Context = (*helperCtx)(nil)
	var typedInput *helperRC
	var typedOut io.Writer = (*bytes.Buffer)(nil)
	input := &helperRC{closeErr: errors.New("sensitive-close-canary")}
	var out, stderr bytes.Buffer
	code := RunProbeBinaryStageHelper(nil, nil, nil, input, &out, &stderr) //nolint:staticcheck // SA1012: a nil context is the refusal under test
	if code != 1 || out.Len() != 0 || input.closes.Load() != 1 || stderr.String() != domain.ErrProbeArtifactStorage.Error()+"\n" || strings.Contains(stderr.String(), "canary") {
		t.Fatalf("close failure hidden: %d %q", code, stderr.String())
	}
	if RunProbeBinaryStageHelper(typedCtx, nil, &os.File{}, typedInput, &out, &stderr) != 1 {
		t.Fatal("typed nil dispatched")
	}
	stderr.Reset()
	live := &helperRC{}
	if RunProbeBinaryStageHelper(context.Background(), nil, nil, live, typedOut, &stderr) != 1 || live.closes.Load() != 1 || stderr.String() != domain.ErrProbeBinaryStageWriteInput.Error()+"\n" {
		t.Fatal("nil parent/output")
	}
}

func TestRunProbeBinaryStageHelperCanceledBeforeParentIO(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	input := &helperRC{}
	var out, stderr bytes.Buffer
	code := RunProbeBinaryStageHelper(ctx, nil, &os.File{}, input, &out, &stderr)
	if code != 1 || out.Len() != 0 || input.closes.Load() != 1 || stderr.String() != context.Canceled.Error()+"\n" {
		t.Fatalf("cancel code=%d err=%q closes=%d", code, stderr.String(), input.closes.Load())
	}
	deadline, stop := context.WithTimeout(context.Background(), time.Nanosecond)
	defer stop()
	time.Sleep(time.Millisecond)
	input = &helperRC{}
	out.Reset()
	stderr.Reset()
	code = RunProbeBinaryStageHelper(deadline, nil, &os.File{}, input, &out, &stderr)
	if code != 1 || input.closes.Load() != 1 || stderr.String() != context.DeadlineExceeded.Error()+"\n" || strings.Contains(stderr.String(), "sensitive") {
		t.Fatal(stderr.String())
	}
}

func TestProbeBinaryStageHelperHeaderBoundaries(t *testing.T) {
	ctx := context.Background()
	empty := &helperRC{readFn: func([]byte) (int, error) { return 0, nil }}
	if _, err := readProbeBinaryStageHeader(ctx, empty); !errors.Is(err, domain.ErrProbeArtifactStorage) || empty.reads.Load() != probeBinaryStageHelperMaxEmptyReads {
		t.Fatalf("no-progress reads=%d err=%v", empty.reads.Load(), err)
	}
	progress := 0
	paced := &helperRC{readFn: func(p []byte) (int, error) {
		progress++
		if progress <= probeBinaryStageHelperMaxEmptyReads-1 {
			return 0, nil
		}
		p[0] = 'x'
		return 1, io.EOF
	}}
	if err := (&probeBinaryStageHeaderRead{ctx: ctx, r: paced}).exact(make([]byte, 1)); err != nil || progress != probeBinaryStageHelperMaxEmptyReads {
		t.Fatalf("progress err=%v reads=%d", err, progress)
	}
	for _, n := range []int{-1, 8} {
		// A simultaneous source error would mask a missing count guard.
		bad := &helperRC{readFn: func([]byte) (int, error) { return n, nil }}
		var gotErr error
		func() {
			defer func() {
				if recover() != nil {
					gotErr = errors.New("panic")
				}
			}()
			gotErr = (&probeBinaryStageHeaderRead{ctx: ctx, r: bad}).exact(make([]byte, 4))
		}()
		if !errors.Is(gotErr, domain.ErrProbeArtifactStorage) || strings.Contains(gotErr.Error(), "canary") {
			t.Fatalf("count %d err=%v", n, gotErr)
		}
	}
	leaky := &helperRC{readFn: func([]byte) (int, error) { return 0, errors.New("sensitive-read-canary") }}
	if _, err := readProbeBinaryStageHeader(ctx, leaky); !errors.Is(err, domain.ErrProbeArtifactStorage) || strings.Contains(err.Error(), "canary") {
		t.Fatal(err)
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	calls := 0
	blocked := &helperRC{readFn: func([]byte) (int, error) { calls++; return 1, nil }}
	if _, err := readProbeBinaryStageHeader(canceled, blocked); !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("header context err=%v calls=%d", err, calls)
	}
}

func TestProbeBinaryStageHelperReceiptAndShortWrite(t *testing.T) {
	intent := helperIntent()
	payload, err := probeBinaryStageHelperReceipt(domain.ProbeBinaryStageSeal{Intent: intent})
	if err != nil || len(payload) == 0 || len(payload) > probeBinaryStageHelperMaxResponse || payload[len(payload)-1] != '\n' || bytes.Count(payload, []byte{'\n'}) != 1 {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload, &fields) != nil || len(fields) != 9 {
		t.Fatalf("fields=%d", len(fields))
	}
	for _, key := range []string{"state", "directory_name", "marker_sha256", "pinned_version", "version_source", "os", "architecture", "sha256", "size"} {
		if _, ok := fields[key]; !ok {
			t.Fatal(key)
		}
	}
	text := string(payload)
	if strings.Contains(text, intent.Directory()) || strings.Contains(text, "example.test") || strings.Contains(text, "SHA256:") || strings.Contains(text, "applied") || strings.Contains(text, "installed") || strings.Contains(text, "/opt/") {
		t.Fatal("receipt leaked authority or host data")
	}
	if !strings.Contains(text, `"state":"local_binary_staged"`) || !strings.Contains(text, `"version_source":"operator_pin"`) || !strings.Contains(text, `"os":"linux"`) || !strings.Contains(text, `"directory_name":"uptime-phoenix-probe-11111111-1111-4111-8111-111111111111"`) || !strings.Contains(text, `"size":17`) {
		t.Fatal(text)
	}
	ok := &helperCountWriter{}
	if writeProbeBinaryStageHelperReceipt(ok, payload) != nil || ok.calls.Load() != 1 || ok.buf.String() != string(payload) {
		t.Fatal("full write")
	}
	if _, err = probeBinaryStageHelperReceipt(domain.ProbeBinaryStageSeal{}); err == nil {
		t.Fatal("invalid seal acknowledged")
	}
	short := &helperCountWriter{n: 1}
	if writeProbeBinaryStageHelperReceipt(short, payload) == nil || short.calls.Load() != 1 {
		t.Fatal("short write accepted or retried")
	}
	failed := &helperCountWriter{err: errors.New("sensitive-write-canary")}
	if err = writeProbeBinaryStageHelperReceipt(failed, payload); !errors.Is(err, domain.ErrProbeBinaryStageWriteUnknown) || strings.Contains(err.Error(), "canary") || failed.calls.Load() != 1 {
		t.Fatal(err)
	}
	huge := bytes.Repeat([]byte{'a'}, probeBinaryStageHelperMaxResponse+1)
	skipped := &helperCountWriter{}
	if writeProbeBinaryStageHelperReceipt(skipped, huge) == nil || skipped.calls.Load() != 0 {
		t.Fatal("oversized payload written")
	}
}

func TestProbeBinaryStageHelperGuardOwnership(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	raw := &helperRC{closeErr: errors.New("sensitive-close-canary")}
	guard, err := newProbeBinaryStageHelperInput(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	deadline := time.Now().Add(time.Second)
	for raw.closes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err = guard.Close(); !errors.Is(err, domain.ErrProbeArtifactStorage) || strings.Contains(err.Error(), "canary") || raw.closes.Load() != 1 {
		t.Fatalf("join close err=%v closes=%d", err, raw.closes.Load())
	}
	if guard.Close() != err || raw.closes.Load() != 1 {
		t.Fatal("close not idempotent")
	}
	live, stop := context.WithCancel(context.Background())
	defer stop()
	body := &helperRC{readFn: func(p []byte) (int, error) {
		if len(p) != 64 {
			return 0, errors.New("limited")
		}
		return 0, io.EOF
	}}
	guard, err = newProbeBinaryStageHelperInput(live, body)
	if err != nil {
		t.Fatal(err)
	}
	n, readErr := guard.Read(make([]byte, 64))
	if n != 0 || !errors.Is(readErr, io.EOF) {
		t.Fatalf("eof not retained n=%d err=%v", n, readErr)
	}
	stop()
	if _, readErr = guard.Read(make([]byte, 1)); !errors.Is(readErr, context.Canceled) || body.reads.Load() != 1 {
		t.Fatal(readErr)
	}
	if guard.Close() != nil || body.closes.Load() != 1 || guard.Close() != nil {
		t.Fatal("clean close")
	}
}

func TestProbeBinaryStageHelperGuardJoinsBlockedCallback(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	raw := &helperRC{closeFn: func() error {
		close(entered)
		<-release
		return nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	guard, err := newProbeBinaryStageHelperInput(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("callback did not enter Close")
	}
	done := make(chan error, 1)
	go func() { done <- guard.Close() }()
	select {
	case _ = <-done:
		t.Fatal("Close returned before callback finished")
	case <-time.After(30 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not join callback")
	}
	if raw.closes.Load() != 1 || guard.Close() != nil || raw.closes.Load() != 1 {
		t.Fatal("duplicate raw close")
	}
}
