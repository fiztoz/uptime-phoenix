//go:build linux

package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

const (
	probeSystemctlTimeout   = 30 * time.Second
	probeSystemctlMaxOutput = 4096
)

// probeSystemctlCandidates are the only systemctl paths ever executed.
var probeSystemctlCandidates = []string{"/usr/bin/systemctl", "/bin/systemctl"}

// applyHostProbeAction opens the protected fixed parent for the intent's kind
// (literal /var/lib or /etc/systemd/system) and applies the action beneath it.
func applyHostProbeAction(ctx context.Context, intent domain.ProbeHostActionIntent) error {
	if err := probeBinaryStageAuthorityPrecheck(ctx); err != nil {
		return err
	}
	var names []string
	switch intent.Kind {
	case domain.ProbeHostActionDataDirectory:
		names = []string{"var", "lib"}
	case domain.ProbeHostActionUnitFile, domain.ProbeHostActionService:
		names = []string{"etc", "systemd", "system"}
	default:
		return domain.ErrProbeBinaryStageWriteInput
	}
	fd, err := openProtectedProbeBinaryStageDir(names...)
	if err != nil {
		return domain.ErrProbeBinaryStageWriteUnavailable
	}
	parent := os.NewFile(uintptr(fd), "probe-host-action-parent")
	if parent == nil {
		_ = probeBinaryStageAuthorityClose(fd, nil)
		return domain.ErrProbeBinaryStageWriteUnavailable
	}
	defer func() { _ = parent.Close() }()
	switch intent.Kind {
	case domain.ProbeHostActionDataDirectory:
		return provisioning.ApplyProbeDataDirectory(ctx, parent, intent)
	case domain.ProbeHostActionUnitFile:
		return provisioning.ApplyProbeUnitFile(ctx, parent, intent)
	default:
		systemctl, err := newHostProbeSystemctl()
		if err != nil {
			return domain.ErrProbeBinaryStageWriteUnavailable
		}
		return provisioning.ApplyProbeService(ctx, parent, intent, systemctl)
	}
}

// hostProbeSystemctl executes one root-owned, non-group/world-writable
// systemctl from a fixed absolute path with an empty environment.
type hostProbeSystemctl struct{ path string }

func newHostProbeSystemctl() (*hostProbeSystemctl, error) {
	for _, candidate := range probeSystemctlCandidates {
		info, err := os.Stat(candidate)
		if err != nil {
			continue
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !info.Mode().IsRegular() || !ok || st.Uid != 0 || info.Mode().Perm()&0o022 != 0 {
			return nil, domain.ErrProbeBinaryStageWriteUnavailable
		}
		return &hostProbeSystemctl{path: candidate}, nil
	}
	return nil, domain.ErrProbeBinaryStageWriteUnavailable
}

// Run returns bounded stdout and the exit status; a nonzero exit is not an error.
func (s *hostProbeSystemctl) Run(ctx context.Context, args ...string) (string, int, error) {
	runCtx, cancel := context.WithTimeout(ctx, probeSystemctlTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, s.path, args...)
	cmd.Env = []string{}
	cmd.Dir = "/"
	stdout := &probeBoundedBuffer{max: probeSystemctlMaxOutput}
	cmd.Stdout = stdout
	cmd.Stderr = &probeBoundedBuffer{max: probeSystemctlMaxOutput}
	err := cmd.Run()
	if stdout.over {
		return "", -1, domain.ErrProbeBinaryStageWriteUnknown
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && runCtx.Err() == nil {
		return stdout.String(), exit.ExitCode(), nil
	}
	if err != nil {
		return "", -1, err
	}
	return stdout.String(), 0, nil
}

type probeBoundedBuffer struct {
	bytes.Buffer
	max  int
	over bool
}

func (b *probeBoundedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.max {
		b.over = true
		return 0, errors.New("output bound exceeded")
	}
	return b.Buffer.Write(p)
}
