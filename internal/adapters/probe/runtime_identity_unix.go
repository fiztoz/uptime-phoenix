//go:build unix

package probe

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	probeNonblock = syscall.O_NONBLOCK
	probeNoFollow = syscall.O_NOFOLLOW
)

type dirLock struct {
	file *os.File
}

func acquireDirLock(dirFile *os.File) (*dirLock, error) {
	if dirFile == nil {
		return nil, errors.New("directory handle is required to acquire lock")
	}
	dirFd := int(dirFile.Fd())

	// Open descriptor relative to directory handle without following symlinks and non-blocking
	fd, err := unix.Openat(dirFd, lockFileName, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			// Create fresh lock file with O_EXCL to prevent racing
			fd, err = unix.Openat(dirFd, lockFileName, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
			if err != nil {
				if errors.Is(err, unix.EEXIST) {
					// Raced with another creator; retry opening existing
					fd, err = unix.Openat(dirFd, lockFileName, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
				}
			} else {
				_ = unix.Fchmod(fd, 0600)
			}
		}
		if err != nil {
			return nil, probeIOError("open lock file", err)
		}
	}

	f := os.NewFile(uintptr(fd), lockFileName)

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = f.Close()
		return nil, probeIOError("stat lock file", err)
	}

	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = f.Close()
		return nil, errors.New("lock file must be a regular file")
	}
	if st.Mode&0777 != 0600 {
		_ = f.Close()
		return nil, fmt.Errorf("lock file permissions too permissive: %04o (must be exact 0600)", st.Mode&0777)
	}
	if st.Uid != uint32(os.Geteuid()) {
		_ = f.Close()
		return nil, fmt.Errorf("lock file owned by uid %d, expected process uid %d", st.Uid, os.Geteuid())
	}

	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if err == unix.EWOULDBLOCK || err == unix.EAGAIN {
			return nil, errors.New("data directory is locked by another process")
		}
		return nil, errors.New("flock failed")
	}

	return &dirLock{file: f}, nil
}

func (l *dirLock) release() error {
	if l == nil || l.file == nil {
		return nil
	}
	defer func() {
		_ = l.file.Close()
		l.file = nil
	}()
	// Never unlink lock file; release kernel lock only
	return unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
}

func probeTrustedOwner(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func probeCurrentOwner(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}
