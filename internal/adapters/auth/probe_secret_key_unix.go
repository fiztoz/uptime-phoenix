//go:build linux || darwin

package auth

import (
	"os"
	"syscall"
)

const probeKeyNonblock = syscall.O_NONBLOCK

func probeKeyTrustedOwner(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && (uint64(stat.Uid) == uint64(os.Geteuid()) || stat.Uid == 0)
}

func probeKeyCurrentOwner(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(stat.Uid) == uint64(os.Geteuid())
}
