package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// hostMockInput is a mock raw stream. It records reads and closes and never
// touches a filesystem.
type hostMockInput struct {
	reads    atomic.Int32
	closes   atomic.Int32
	closeErr error
}

func (r *hostMockInput) Read(_ []byte) (int, error) {
	r.reads.Add(1)
	return 0, io.EOF
}

func (r *hostMockInput) Close() error {
	r.closes.Add(1)
	return r.closeErr
}

type hostMockCtx struct{ context.Context }

// hostMockAcquirer counts injected acquisition calls for exact-call assertions.
type hostMockAcquirer struct {
	calls atomic.Int32
	fn    func(context.Context) (*os.File, error)
}

func (a *hostMockAcquirer) acquire(ctx context.Context) (*os.File, error) {
	a.calls.Add(1)
	return a.fn(ctx)
}

// hostAcquireFail is an injected acquirer that must never run in the refusal
// paths covered here.
func hostAcquireFail(t *testing.T) func(context.Context) (*os.File, error) {
	t.Helper()
	return func(context.Context) (*os.File, error) {
		t.Error("acquisition dispatched")
		return nil, domain.ErrProbeBinaryStageWriteUnavailable
	}
}

func TestRunHostProbeBinaryStageHelperUsageNeverEchoesArgsOrAcquires(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"--path"}, {"--path=/opt/secret"}, {"sensitive-path-canary"}, {""}} {
		input := &hostMockInput{}
		var out, stderr bytes.Buffer
		acq := &hostMockAcquirer{fn: hostAcquireFail(t)}
		code := runHostProbeBinaryStageHelper(nil, args, input, &out, &stderr, acq.acquire)
		if code != 2 || out.Len() != 0 || input.closes.Load() != 1 || input.reads.Load() != 0 || acq.calls.Load() != 0 {
			t.Fatalf("args %q code=%d closes=%d reads=%d calls=%d", args, code, input.closes.Load(), input.reads.Load(), acq.calls.Load())
		}
		if stderr.String() != probeBinaryStageHostUsage ||
			strings.Contains(stderr.String(), "canary") ||
			strings.Contains(stderr.String(), "--path") ||
			strings.Contains(stderr.String(), "/opt/secret") {
			t.Fatalf("args %q leaked into usage %q", args, stderr.String())
		}
	}
	// An input close failure on the usage path is a storage refusal, not exit 2.
	broken := &hostMockInput{closeErr: errors.New("sensitive-close-canary")}
	var out, stderr bytes.Buffer
	acq := &hostMockAcquirer{fn: hostAcquireFail(t)}
	code := runHostProbeBinaryStageHelper(nil, []string{"--path=/opt/secret"}, broken, &out, &stderr, acq.acquire)
	if code != 1 || out.Len() != 0 || stderr.String() != domain.ErrProbeArtifactStorage.Error()+"\n" ||
		strings.Contains(stderr.String(), "canary") || strings.Contains(stderr.String(), "/opt/secret") || acq.calls.Load() != 0 {
		t.Fatalf("usage close failure code=%d err=%q calls=%d", code, stderr.String(), acq.calls.Load())
	}
	// A typed-nil input still dispatches usage without acquisition.
	var typed *hostMockInput
	var input io.ReadCloser = typed
	stderr.Reset()
	if code = runHostProbeBinaryStageHelper(nil, []string{"--help"}, input, io.Discard, &stderr, acq.acquire); code != 2 || stderr.String() != probeBinaryStageHostUsage {
		t.Fatalf("typed-nil usage dispatch code=%d err=%q", code, stderr.String())
	}
}

func TestRunHostProbeBinaryStageHelperRejectsNilBeforeAcquire(t *testing.T) {
	var typedCtx context.Context = (*hostMockCtx)(nil)
	var typedInput *hostMockInput
	var typedOut io.Writer = (*bytes.Buffer)(nil)
	var typedStderr io.Writer = (*bytes.Buffer)(nil)
	acq := &hostMockAcquirer{fn: hostAcquireFail(t)}
	var out, stderr bytes.Buffer

	input := &hostMockInput{}
	if code := runHostProbeBinaryStageHelper(typedCtx, nil, input, &out, &stderr, acq.acquire); code != 1 ||
		out.Len() != 0 || stderr.String() != domain.ErrProbeBinaryStageWriteInput.Error()+"\n" ||
		input.closes.Load() != 1 || input.reads.Load() != 0 {
		t.Fatalf("typed-nil ctx code=%d err=%q", code, stderr.String())
	}

	stderr.Reset()
	var nilInput io.ReadCloser = typedInput
	if code := runHostProbeBinaryStageHelper(context.Background(), nil, nilInput, &out, &stderr, acq.acquire); code != 1 ||
		stderr.String() != domain.ErrProbeBinaryStageWriteInput.Error()+"\n" {
		t.Fatalf("typed-nil input code=%d err=%q", code, stderr.String())
	}

	stderr.Reset()
	live := &hostMockInput{}
	if code := runHostProbeBinaryStageHelper(context.Background(), nil, live, typedOut, &stderr, acq.acquire); code != 1 ||
		stderr.String() != domain.ErrProbeBinaryStageWriteInput.Error()+"\n" || live.closes.Load() != 1 || live.reads.Load() != 0 {
		t.Fatalf("typed-nil out code=%d err=%q", code, stderr.String())
	}

	live = &hostMockInput{}
	if code := runHostProbeBinaryStageHelper(context.Background(), nil, live, &out, typedStderr, acq.acquire); code != 1 ||
		live.closes.Load() != 1 || live.reads.Load() != 0 {
		t.Fatalf("typed-nil stderr code=%d closes=%d", code, live.closes.Load())
	}
	if acq.calls.Load() != 0 {
		t.Fatalf("acquirer dispatched %d times", acq.calls.Load())
	}

	// An input close failure overrides the input refusal with ArtifactStorage.
	broken := &hostMockInput{closeErr: errors.New("sensitive-close-canary")}
	stderr.Reset()
	code := runHostProbeBinaryStageHelper(typedCtx, nil, broken, &out, &stderr, acq.acquire)
	if code != 1 || out.Len() != 0 || stderr.String() != domain.ErrProbeArtifactStorage.Error()+"\n" ||
		strings.Contains(stderr.String(), "canary") || acq.calls.Load() != 0 {
		t.Fatalf("input close refusal code=%d err=%q", code, stderr.String())
	}
}

func TestRunHostProbeBinaryStageHelperCancelBeforeAcquire(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	input := &hostMockInput{}
	var out, stderr bytes.Buffer
	acq := &hostMockAcquirer{fn: hostAcquireFail(t)}
	code := runHostProbeBinaryStageHelper(ctx, nil, input, &out, &stderr, acq.acquire)
	if code != 1 || out.Len() != 0 || input.closes.Load() != 1 || input.reads.Load() != 0 || acq.calls.Load() != 0 ||
		stderr.String() != context.Canceled.Error()+"\n" {
		t.Fatalf("cancel code=%d err=%q closes=%d calls=%d", code, stderr.String(), input.closes.Load(), acq.calls.Load())
	}
	deadline, stop := context.WithTimeout(context.Background(), time.Nanosecond)
	defer stop()
	time.Sleep(time.Millisecond)
	input = &hostMockInput{}
	out.Reset()
	stderr.Reset()
	code = runHostProbeBinaryStageHelper(deadline, nil, input, &out, &stderr, acq.acquire)
	if code != 1 || input.closes.Load() != 1 || input.reads.Load() != 0 || acq.calls.Load() != 0 ||
		stderr.String() != context.DeadlineExceeded.Error()+"\n" {
		t.Fatalf("deadline code=%d err=%q closes=%d calls=%d", code, stderr.String(), input.closes.Load(), acq.calls.Load())
	}
	// Cancellation arriving during acquisition is preserved after cleanup.
	live, abort := context.WithCancel(context.Background())
	input = &hostMockInput{}
	out.Reset()
	stderr.Reset()
	acq2 := &hostMockAcquirer{fn: func(ctx context.Context) (*os.File, error) {
		abort()
		return nil, ctx.Err()
	}}
	code = runHostProbeBinaryStageHelper(live, nil, input, &out, &stderr, acq2.acquire)
	if code != 1 || acq2.calls.Load() != 1 || input.closes.Load() != 1 || input.reads.Load() != 0 ||
		stderr.String() != context.Canceled.Error()+"\n" {
		t.Fatalf("join cancel code=%d err=%q calls=%d", code, stderr.String(), acq2.calls.Load())
	}
}

func TestRunHostProbeBinaryStageHelperAcquisitionRefusalSanitizes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"nil success", nil, domain.ErrProbeBinaryStageWriteUnavailable},
		{"unknown", errors.New("sensitive-acquire-canary"), domain.ErrProbeBinaryStageWriteUnavailable},
		{"wrapped unavailable", fmt.Errorf("open /opt/secret: %w", domain.ErrProbeBinaryStageWriteUnavailable), domain.ErrProbeBinaryStageWriteUnavailable},
		{"input sentinel refused as unavailable", domain.ErrProbeBinaryStageWriteInput, domain.ErrProbeBinaryStageWriteUnavailable},
		{"storage", domain.ErrProbeArtifactStorage, domain.ErrProbeArtifactStorage},
		{"wrapped storage", fmt.Errorf("dup parent: %w", domain.ErrProbeArtifactStorage), domain.ErrProbeArtifactStorage},
		{"deadline sentinel", context.DeadlineExceeded, context.DeadlineExceeded},
		{"canceled sentinel", context.Canceled, context.Canceled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := &hostMockInput{}
			var out, stderr bytes.Buffer
			acq := &hostMockAcquirer{fn: func(context.Context) (*os.File, error) { return nil, tc.err }}
			code := runHostProbeBinaryStageHelper(context.Background(), nil, input, &out, &stderr, acq.acquire)
			if code != 1 || out.Len() != 0 || acq.calls.Load() != 1 || input.reads.Load() != 0 || input.closes.Load() != 1 {
				t.Fatalf("code=%d calls=%d reads=%d closes=%d", code, acq.calls.Load(), input.reads.Load(), input.closes.Load())
			}
			if stderr.String() != tc.want.Error()+"\n" ||
				strings.Contains(stderr.String(), "canary") ||
				strings.Contains(stderr.String(), "/opt/secret") {
				t.Fatalf("stderr=%q want=%q", stderr.String(), tc.want.Error())
			}
		})
	}
	// A nil acquisition function is the same Unavailable refusal, not a panic.
	input := &hostMockInput{}
	var out, stderr bytes.Buffer
	if code := runHostProbeBinaryStageHelper(context.Background(), nil, input, &out, &stderr, nil); code != 1 ||
		stderr.String() != domain.ErrProbeBinaryStageWriteUnavailable.Error()+"\n" ||
		input.closes.Load() != 1 || input.reads.Load() != 0 {
		t.Fatalf("nil acquirer code=%d err=%q", code, stderr.String())
	}
	// An input close failure overrides the sanitized acquisition reason.
	broken := &hostMockInput{closeErr: errors.New("sensitive-close-canary")}
	out.Reset()
	stderr.Reset()
	acq := &hostMockAcquirer{fn: func(context.Context) (*os.File, error) { return nil, domain.ErrProbeBinaryStageWriteUnavailable }}
	if code := runHostProbeBinaryStageHelper(context.Background(), nil, broken, &out, &stderr, acq.acquire); code != 1 ||
		stderr.String() != domain.ErrProbeArtifactStorage.Error()+"\n" || strings.Contains(stderr.String(), "canary") {
		t.Fatalf("input close refusal code=%d err=%q", code, stderr.String())
	}
}

func TestRunHostProbeBinaryStageHelperDefensivelyClosesErrorBearingParent(t *testing.T) {
	// A zero os.File is error-bearing: its Close fails without any syscall,
	// which pins the defensive-close failure classification. The wrapper must
	// attempt that close instead of dropping the descriptor or succeeding.
	input := &hostMockInput{}
	var out, stderr bytes.Buffer
	acq := &hostMockAcquirer{fn: func(context.Context) (*os.File, error) {
		return &os.File{}, errors.New("sensitive-acquire-canary")
	}}
	code := runHostProbeBinaryStageHelper(context.Background(), nil, input, &out, &stderr, acq.acquire)
	if code != 1 || out.Len() != 0 || acq.calls.Load() != 1 || input.reads.Load() != 0 || input.closes.Load() != 1 {
		t.Fatalf("code=%d calls=%d reads=%d closes=%d", code, acq.calls.Load(), input.reads.Load(), input.closes.Load())
	}
	if stderr.String() != domain.ErrProbeArtifactStorage.Error()+"\n" || strings.Contains(stderr.String(), "canary") {
		t.Fatalf("defensive close failure err=%q", stderr.String())
	}
	// A defensive close failure wins over a live caller cancellation: the
	// cancellation sentinel survives only after cleanup.
	live, abort := context.WithCancel(context.Background())
	input = &hostMockInput{}
	out.Reset()
	stderr.Reset()
	acq2 := &hostMockAcquirer{fn: func(ctx context.Context) (*os.File, error) {
		abort()
		return &os.File{}, ctx.Err()
	}}
	code = runHostProbeBinaryStageHelper(live, nil, input, &out, &stderr, acq2.acquire)
	if code != 1 || acq2.calls.Load() != 1 || input.reads.Load() != 0 || input.closes.Load() != 1 ||
		stderr.String() != domain.ErrProbeArtifactStorage.Error()+"\n" {
		t.Fatalf("close over cancel code=%d err=%q", code, stderr.String())
	}
}
