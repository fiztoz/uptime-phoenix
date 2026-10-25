package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// probeBinaryStageHostUsage is the only text emitted for a non-empty argument
// list by the host-local launcher. It is exit 2, stdout stays empty, and the
// rejected arguments are never echoed.
const probeBinaryStageHostUsage = "Usage: phoenix-probe-admin stage-binary-host\nNo flags, paths, or arguments. Linux uid 0 only; fixed root-owned /opt parent.\nThis is NOT installation, current DB authority, fencing, enrollment, or cleanup.\n"

// RunHostProbeBinaryStageHelper stages one stdin frame under the host-local
// fixed staging parent selected by openHostProbeBinaryStageParent (supplied by
// the platform files). Non-empty args are refused before any parent acquisition
// and are never echoed. It owns a non-typed-nil input on every return, closes
// the acquired parent exactly once before any stdout, and exits 0 only for the
// single fresh-stage receipt line. That receipt is not installation, current DB
// authority, fencing, enrollment, or cleanup; a failed or uncertain attempt
// emits no trusted receipt (a failed public write can leave partial bytes).
func RunHostProbeBinaryStageHelper(ctx context.Context, args []string, input io.ReadCloser, out, stderr io.Writer) int {
	return runHostProbeBinaryStageHelper(ctx, args, input, out, stderr, openHostProbeBinaryStageParent)
}

// runHostProbeBinaryStageHelper is the mock boundary seam: acquire is injected
// solely so tests can exercise acquisition refusals without any filesystem.
// Everything after acquisition reuses the unchanged inherited-fd helper path.
func runHostProbeBinaryStageHelper(ctx context.Context, args []string, input io.ReadCloser, out, stderr io.Writer, acquire func(context.Context) (*os.File, error)) int {
	if len(args) != 0 {
		return probeBinaryStageHostRefuseUsage(input, stderr)
	}
	if probeBinaryStageHelperNil(ctx) || probeBinaryStageHelperNil(input) || probeBinaryStageHelperNil(out) || probeBinaryStageHelperNil(stderr) {
		return probeBinaryStageHelperRefuse(input, stderr, domain.ErrProbeBinaryStageWriteInput)
	}
	if err := ctx.Err(); err != nil {
		return probeBinaryStageHelperRefuse(input, stderr, probeBinaryStageHelperContextError(err))
	}
	var parent *os.File
	var err error
	if !probeBinaryStageHelperNil(acquire) {
		parent, err = acquire(ctx)
	}
	if err != nil || parent == nil {
		// Acquisition owns its own partial descriptors on error, but a nonnil
		// error-bearing parent is still defensively closed here. A close
		// failure is the louder fact and wins every other classification;
		// a live caller's cancellation sentinel survives only after cleanup.
		if parent != nil {
			if cerr := parent.Close(); cerr != nil {
				return probeBinaryStageHelperRefuse(input, stderr, domain.ErrProbeArtifactStorage)
			}
		}
		if cerr := ctx.Err(); cerr != nil {
			return probeBinaryStageHelperRefuse(input, stderr, probeBinaryStageHelperContextError(cerr))
		}
		return probeBinaryStageHelperRefuse(input, stderr, probeBinaryStageHostAcquireError(err))
	}
	// inheritedProbeBinaryStageInput consumes the raw input even on its own
	// error, so that path must never close it a second time.
	ownedInput, err := inheritedProbeBinaryStageInput(ctx, input)
	if err != nil {
		if parent.Close() != nil {
			err = domain.ErrProbeArtifactStorage
		}
		return probeBinaryStageHelperExit(stderr, err)
	}
	// RunProbeBinaryStageHelper owns ownedInput from here; it is never closed
	// again by this wrapper. The parent is closed exactly once before the
	// buffered receipt can reach stdout.
	var buf, diagnostic bytes.Buffer
	closed := false
	defer func() {
		if !closed {
			_ = parent.Close()
		}
	}()
	code := RunProbeBinaryStageHelper(ctx, nil, parent, ownedInput, &buf, &diagnostic)
	closeErr := parent.Close()
	closed = true
	return finishInheritedProbeBinaryStageHelper(ctx, code, closeErr, buf.Bytes(), diagnostic.String(), out, stderr)
}

// probeBinaryStageHostRefuseUsage closes the owned input, then emits the fixed
// host usage text and exits 2. An input close failure is a storage refusal
// instead, and the rejected arguments are never echoed.
func probeBinaryStageHostRefuseUsage(input io.ReadCloser, stderr io.Writer) int {
	if err := probeBinaryStageHelperCloseInput(input); err != nil {
		return probeBinaryStageHelperExit(stderr, err)
	}
	if !probeBinaryStageHelperNil(stderr) {
		_, _ = io.WriteString(stderr, probeBinaryStageHostUsage)
	}
	return 2
}

// probeBinaryStageHostAcquireError sanitizes an acquisition failure for public
// stderr. A context sentinel and a bare storage failure are the only causes a
// live caller may see; every other cause, including a nil success, becomes
// Unavailable and no underlying message is echoed.
func probeBinaryStageHostAcquireError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return probeBinaryStageHelperContextError(err)
	}
	if errors.Is(err, domain.ErrProbeArtifactStorage) {
		return domain.ErrProbeArtifactStorage
	}
	return domain.ErrProbeBinaryStageWriteUnavailable
}
