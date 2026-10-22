//go:build linux

package bootstrap

import (
	"context"
	"os"

	"golang.org/x/sys/unix"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// openHostProbeBinaryStageParent selects only this namespace's protected /opt.
// Its metadata is not a current DB grant, operation ownership or a host fence.
// The caller owns the returned descriptor; no target directory is created here.
func openHostProbeBinaryStageParent(ctx context.Context) (*os.File, error) {
	if probeBinaryStageHelperNil(ctx) {
		return nil, domain.ErrProbeBinaryStageWriteInput
	}
	if err := ctx.Err(); err != nil {
		return nil, probeBinaryStageHelperContextError(err)
	}
	if os.Geteuid() != 0 {
		return nil, domain.ErrProbeBinaryStageWriteUnavailable
	}
	const flags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	root, err := unix.Open("/", flags, 0)
	if err != nil {
		return nil, domain.ErrProbeBinaryStageWriteUnavailable
	}
	var rootStat unix.Stat_t
	if err = unix.Fstat(root, &rootStat); err != nil || !probeBinaryStageHostDirectory(&rootStat) {
		if unix.Close(root) != nil {
			return nil, domain.ErrProbeArtifactStorage
		}
		return nil, domain.ErrProbeBinaryStageWriteUnavailable
	}
	if err = ctx.Err(); err != nil {
		if unix.Close(root) != nil {
			return nil, domain.ErrProbeArtifactStorage
		}
		return nil, probeBinaryStageHelperContextError(err)
	}
	parent, openErr := unix.Openat(root, "opt", flags, 0)
	rootCloseErr := unix.Close(root)
	if openErr != nil || rootCloseErr != nil {
		reason := domain.ErrProbeBinaryStageWriteUnavailable
		if rootCloseErr != nil {
			reason = domain.ErrProbeArtifactStorage
		}
		if parent >= 0 && unix.Close(parent) != nil {
			reason = domain.ErrProbeArtifactStorage
		}
		return nil, reason
	}
	refuse := func(reason error) (*os.File, error) {
		if unix.Close(parent) != nil {
			reason = domain.ErrProbeArtifactStorage
		}
		return nil, reason
	}
	var info unix.Stat_t
	if err = unix.Fstat(parent, &info); err != nil || !probeBinaryStageHostDirectory(&info) {
		return refuse(domain.ErrProbeBinaryStageWriteUnavailable)
	}
	if err = ctx.Err(); err != nil {
		return refuse(probeBinaryStageHelperContextError(err))
	}
	file := os.NewFile(uintptr(parent), "fixed-host-probe-stage-parent")
	if file == nil {
		return refuse(domain.ErrProbeBinaryStageWriteUnavailable)
	}
	return file, nil
}

func probeBinaryStageHostDirectory(info *unix.Stat_t) bool {
	return info != nil && info.Mode&unix.S_IFMT == unix.S_IFDIR &&
		info.Uid == 0 && info.Mode&0o022 == 0 && info.Nlink > 0
}
