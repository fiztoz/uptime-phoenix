//go:build !linux && !darwin

package auth

import "os"

// File provisioning fails closed where POSIX ownership checks are unavailable.
const probeKeyNonblock = 0

func probeKeyTrustedOwner(os.FileInfo) bool { return false }
func probeKeyCurrentOwner(os.FileInfo) bool { return false }
