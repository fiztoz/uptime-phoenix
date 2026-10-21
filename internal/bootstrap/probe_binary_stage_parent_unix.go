//go:build linux || darwin

package bootstrap

import (
	"context"
	"io"
	"os"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// inheritedProbeBinaryStageParent borrows descriptor 3 and owns only its duplicate.
// It checks type, not pathname, host identity, authority or hostile fd replacement.
func inheritedProbeBinaryStageParent(ctx context.Context) (*os.File, error) {
	if probeBinaryStageHelperNil(ctx) {
		return nil, domain.ErrProbeBinaryStageWriteInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fd, err := unix.FcntlInt(3, unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, domain.ErrProbeBinaryStageWriteUnavailable
	}
	parent := os.NewFile(uintptr(fd), "inherited-probe-stage-parent")
	if parent == nil {
		_ = unix.Close(fd)
		return nil, domain.ErrProbeBinaryStageWriteUnavailable
	}
	info, err := parent.Stat()
	if err != nil || !info.IsDir() {
		_ = parent.Close()
		return nil, domain.ErrProbeBinaryStageWriteUnavailable
	}
	if err := ctx.Err(); err != nil {
		_ = parent.Close()
		return nil, err
	}
	return parent, nil
}

// inheritedProbeBinaryStageInput consumes input. Inherited blocking pipe files
// need a fresh pollable Go descriptor: closing an ordinary os.Stdin may not wake
// a blocked read. Only stdin's shared status flags change, and they are restored
// after the pollable duplicate joins. Descriptor 3 is never involved here.
func inheritedProbeBinaryStageInput(ctx context.Context, input io.ReadCloser) (io.ReadCloser, error) {
	raw, ok := input.(*os.File)
	if !ok {
		return input, nil // Non-file callers must supply Close-unblocks-Read.
	}
	fail := func(reason error) (io.ReadCloser, error) {
		if err := raw.Close(); err != nil {
			reason = domain.ErrProbeArtifactStorage
		}
		return nil, reason
	}
	info, err := raw.Stat()
	if err != nil {
		return fail(domain.ErrProbeArtifactStorage)
	}
	if info.Mode().IsRegular() {
		return raw, nil // No invented bound for blocking filesystem operations.
	}
	if info.Mode()&os.ModeNamedPipe == 0 {
		return fail(domain.ErrProbeBinaryStageWriteInput)
	}
	control, err := raw.SyscallConn()
	if err != nil {
		return fail(domain.ErrProbeArtifactStorage)
	}
	fd, flags := -1, 0
	var inner error
	if err = control.Control(func(original uintptr) {
		flags, inner = unix.FcntlInt(original, unix.F_GETFL, 0)
		if inner == nil {
			fd, inner = unix.FcntlInt(original, unix.F_DUPFD_CLOEXEC, 0)
		}
		if inner == nil {
			_, inner = unix.FcntlInt(original, unix.F_SETFL, flags|unix.O_NONBLOCK)
		}
	}); err != nil || inner != nil {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
		return fail(domain.ErrProbeArtifactStorage)
	}
	pollable := os.NewFile(uintptr(fd), "probe-stage-input")
	if pollable == nil {
		_ = unix.Close(fd)
		_ = control.Control(func(original uintptr) { _, _ = unix.FcntlInt(original, unix.F_SETFL, flags) })
		return fail(domain.ErrProbeArtifactStorage)
	}
	owned := &probeBinaryStageFileInput{raw: raw, data: pollable, flags: flags}
	if err := ctx.Err(); err != nil {
		if owned.Close() != nil {
			err = domain.ErrProbeArtifactStorage
		}
		return nil, err
	}
	return owned, nil
}

type probeBinaryStageFileInput struct {
	raw   *os.File
	data  *os.File
	flags int
	once  sync.Once
	err   error
}

func (s *probeBinaryStageFileInput) Read(p []byte) (int, error) { return s.data.Read(p) }

func (s *probeBinaryStageFileInput) Close() error {
	s.once.Do(func() {
		if err := s.data.Close(); err != nil {
			s.err = domain.ErrProbeArtifactStorage
		}
		control, err := s.raw.SyscallConn()
		var inner error
		if err == nil {
			err = control.Control(func(fd uintptr) { _, inner = unix.FcntlInt(fd, unix.F_SETFL, s.flags) })
		}
		if err != nil || inner != nil {
			s.err = domain.ErrProbeArtifactStorage
		}
		if err := s.raw.Close(); err != nil {
			s.err = domain.ErrProbeArtifactStorage
		}
	})
	return s.err
}
