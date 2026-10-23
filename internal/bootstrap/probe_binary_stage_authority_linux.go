//go:build linux

package bootstrap

import (
	"context"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// probeBinaryStageHostKeyMaxSize bounds one sshd public-key file. RSA-4096
// public keys are well under 1 KiB; anything larger is refused, not truncated.
const probeBinaryStageHostKeyMaxSize = 16 * 1024

// loadHostProbeBinaryStageAuthority reads the fixed trusted key through a
// no-follow descriptor walk of literal / -> etc -> phoenix-probe. Every
// directory must be root-owned and not group/world writable; the key must be
// one root-owned, non-group/world-writable, single-link regular file of the
// exact canonical size. These checks are not key provenance.
func loadHostProbeBinaryStageAuthority(ctx context.Context) ([32]byte, error) {
	var zero [32]byte
	if err := probeBinaryStageAuthorityPrecheck(ctx); err != nil {
		return zero, err
	}
	dir, err := openProtectedProbeBinaryStageDir("etc", "phoenix-probe")
	if err != nil {
		return zero, err
	}
	file, _, err := openProtectedProbeBinaryStageFile(ctx, dir, "stage-authority.pub", provisioning.ProbeBinaryStageAuthorityKeyFileSize, true)
	if err != nil {
		return zero, err
	}
	key, readErr := probeBinaryStageAuthorityReadKey(file)
	if file.Close() != nil {
		return zero, domain.ErrProbeArtifactStorage
	}
	if readErr != nil {
		return zero, readErr
	}
	return key, nil
}

// loadHostProbeBinaryStageHostKeys returns the SHA256 fingerprints of the
// sshd host public keys present under the protected literal /etc/ssh. A
// missing file is skipped; any present file that fails protection or parsing
// refuses the whole set, as does finding none. These are the keys an SSH
// client pins, so they bind an intent to this host; they are not a proof that
// sshd actually serves them.
func loadHostProbeBinaryStageHostKeys(ctx context.Context) ([]string, error) {
	if err := probeBinaryStageAuthorityPrecheck(ctx); err != nil {
		return nil, err
	}
	dir, err := openProtectedProbeBinaryStageDir("etc", "ssh")
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(dir) }()
	var bodies [][]byte
	for _, name := range probeBinaryStageHostKeyNames {
		dup, err := unix.Dup(dir)
		if err != nil {
			return nil, domain.ErrProbeBinaryStageWriteUnavailable
		}
		file, absent, err := openProtectedProbeBinaryStageFile(ctx, dup, name, probeBinaryStageHostKeyMaxSize, false)
		if err != nil {
			return nil, err
		}
		if absent {
			continue
		}
		body := make([]byte, probeBinaryStageHostKeyMaxSize+1)
		n, readErr := io.ReadFull(file, body)
		if errors.Is(readErr, io.ErrUnexpectedEOF) {
			readErr = nil
		}
		if file.Close() != nil {
			return nil, domain.ErrProbeArtifactStorage
		}
		if readErr != nil || n == 0 || n > probeBinaryStageHostKeyMaxSize {
			return nil, domain.ErrProbeBinaryStageWriteUnavailable
		}
		bodies = append(bodies, body[:n])
	}
	return probeBinaryStageHostKeyFingerprints(bodies)
}

func probeBinaryStageAuthorityPrecheck(ctx context.Context) error {
	if probeBinaryStageHelperNil(ctx) {
		return domain.ErrProbeBinaryStageWriteInput
	}
	if err := ctx.Err(); err != nil {
		return probeBinaryStageHelperContextError(err)
	}
	if os.Geteuid() != 0 {
		return domain.ErrProbeBinaryStageWriteUnavailable
	}
	return nil
}

// openProtectedProbeBinaryStageDir walks literal / then each name with
// no-follow directory opens, checking every directory (including the last)
// is root-owned and not group/world writable. The caller owns the result.
func openProtectedProbeBinaryStageDir(names ...string) (int, error) {
	const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	dir, err := unix.Open("/", dirFlags, 0)
	if err != nil {
		return -1, domain.ErrProbeBinaryStageWriteUnavailable
	}
	for _, name := range names {
		var info unix.Stat_t
		if err = unix.Fstat(dir, &info); err != nil || !probeBinaryStageHostDirectory(&info) {
			return -1, probeBinaryStageAuthorityClose(dir, domain.ErrProbeBinaryStageWriteUnavailable)
		}
		next, openErr := unix.Openat(dir, name, dirFlags, 0)
		if closeErr := unix.Close(dir); closeErr != nil {
			if openErr == nil {
				_ = unix.Close(next)
			}
			return -1, domain.ErrProbeArtifactStorage
		}
		if openErr != nil {
			return -1, domain.ErrProbeBinaryStageWriteUnavailable
		}
		dir = next
	}
	var info unix.Stat_t
	if err = unix.Fstat(dir, &info); err != nil || !probeBinaryStageHostDirectory(&info) {
		return -1, probeBinaryStageAuthorityClose(dir, domain.ErrProbeBinaryStageWriteUnavailable)
	}
	return dir, nil
}

// openProtectedProbeBinaryStageFile consumes dir and opens one root-owned,
// non-group/world-writable, single-link regular file beneath it. exact
// requires the size to equal size; otherwise it is an upper bound. O_NONBLOCK
// keeps a planted FIFO from blocking before the regular-file check. Only an
// optional (non-exact) file's ENOENT reports absent with a nil error; every
// present-but-unprotected file is a refusal and is never skipped.
func openProtectedProbeBinaryStageFile(ctx context.Context, dir int, name string, size int64, exact bool) (*os.File, bool, error) {
	fd, openErr := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOCTTY, 0)
	if closeErr := unix.Close(dir); closeErr != nil {
		if openErr == nil {
			_ = unix.Close(fd)
		}
		return nil, false, domain.ErrProbeArtifactStorage
	}
	if openErr == unix.ENOENT && !exact {
		return nil, true, nil
	}
	if openErr != nil {
		return nil, false, domain.ErrProbeBinaryStageWriteUnavailable
	}
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil || info.Mode&unix.S_IFMT != unix.S_IFREG || info.Uid != 0 ||
		info.Mode&0o022 != 0 || info.Nlink != 1 || info.Size <= 0 || info.Size > size || (exact && info.Size != size) {
		return nil, false, probeBinaryStageAuthorityClose(fd, domain.ErrProbeBinaryStageWriteUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return nil, false, probeBinaryStageAuthorityClose(fd, probeBinaryStageHelperContextError(err))
	}
	file := os.NewFile(uintptr(fd), "probe-stage-trust-file")
	if file == nil {
		return nil, false, probeBinaryStageAuthorityClose(fd, domain.ErrProbeBinaryStageWriteUnavailable)
	}
	return file, false, nil
}

func probeBinaryStageAuthorityClose(fd int, reason error) error {
	if unix.Close(fd) != nil {
		return domain.ErrProbeArtifactStorage
	}
	return reason
}
