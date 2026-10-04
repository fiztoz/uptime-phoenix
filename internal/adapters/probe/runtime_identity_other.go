//go:build !unix

package probe

import (
	"errors"
	"os"
)

const (
	probeNonblock = 0
	probeNoFollow = 0
)

type dirLock struct{}

func acquireDirLock(dirFile *os.File) (*dirLock, error) {
	return nil, errors.New("runtime identity locking is only supported on Unix platforms")
}

func (l *dirLock) release() error {
	return nil
}

func probeTrustedOwner(info os.FileInfo) bool {
	return false
}

func probeCurrentOwner(info os.FileInfo) bool {
	return false
}
