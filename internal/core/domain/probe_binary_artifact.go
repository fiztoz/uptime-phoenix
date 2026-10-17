package domain

import (
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
)

// MaxProbeBinarySize bounds disk and parser work for one prepared probe binary.
const MaxProbeBinarySize int64 = 128 * 1024 * 1024

// Artifact errors are fixed diagnostics; filesystem paths and bytes are private.
var (
	ErrProbeArtifactInput    = errors.New("invalid pinned probe artifact input")
	ErrProbeArtifactSize     = errors.New("probe artifact size mismatch")
	ErrProbeArtifactDigest   = errors.New("probe artifact digest mismatch")
	ErrProbeArtifactMetadata = errors.New("probe artifact build metadata mismatch")
	ErrProbeArtifactStorage  = errors.New("probe artifact storage unavailable")
	ErrProbeArtifactClosed   = errors.New("prepared probe artifact is closed")
)

var probeReleaseVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)

// ProbeBinaryPin identifies trusted release bytes, not an installed runtime.
type ProbeBinaryPin struct {
	Version      string
	Architecture string
	SHA256       string
	Size         int64
}

// Valid follows the release filename/version grammar and requires an exact pin.
func (p ProbeBinaryPin) Valid() bool {
	if len(p.Version) > 64 || !probeReleaseVersion.MatchString(p.Version) ||
		(p.Architecture != "amd64" && p.Architecture != "arm64") ||
		p.Size <= 0 || p.Size > MaxProbeBinarySize || len(p.SHA256) != 64 ||
		strings.ToLower(p.SHA256) != p.SHA256 {
		return false
	}
	_, err := hex.DecodeString(p.SHA256)
	return err == nil
}

// ReleaseName matches scripts/release/dry-run.sh; it is not an input pathname.
func (p ProbeBinaryPin) ReleaseName() string {
	if !p.Valid() {
		return ""
	}
	return "uptime-phoenix-probe_" + p.Version + "_linux_" + p.Architecture
}
