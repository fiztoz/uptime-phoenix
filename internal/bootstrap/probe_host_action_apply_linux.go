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

// applyHostProbeAction opens the protected fixed parent(s) for the intent's
// kind (literal /var/lib, /opt or /etc/systemd/system) and applies the action
// beneath them. Only probe_init returns its own receipt.
func applyHostProbeAction(ctx context.Context, intent domain.ProbeHostActionIntent) ([]byte, error) {
	if err := probeBinaryStageAuthorityPrecheck(ctx); err != nil {
		return nil, err
	}
	switch intent.Kind {
	case domain.ProbeHostActionDataDirectory:
		return nil, withProbeHostParent(func(parent *os.File) error {
			return provisioning.ApplyProbeDataDirectory(ctx, parent, intent)
		}, "var", "lib")
	case domain.ProbeHostActionUnitFile:
		return nil, withProbeHostParent(func(parent *os.File) error {
			return provisioning.ApplyProbeUnitFile(ctx, parent, intent)
		}, "etc", "systemd", "system")
	case domain.ProbeHostActionService, domain.ProbeActivationServiceStart:
		systemctl, err := newHostProbeSystemctl()
		if err != nil {
			return nil, domain.ErrProbeBinaryStageWriteUnavailable
		}
		return nil, withProbeHostParent(func(parent *os.File) error {
			if intent.Kind == domain.ProbeActivationServiceStart {
				return provisioning.ApplyProbeServiceStart(ctx, parent, intent, systemctl)
			}
			return provisioning.ApplyProbeService(ctx, parent, intent, systemctl)
		}, "etc", "systemd", "system")
	case domain.ProbeActivationProbeInit:
		var receipt []byte
		err := withProbeHostParent(func(data *os.File) error {
			return withProbeHostParent(func(opt *os.File) error {
				result, err := provisioning.ApplyProbeInit(ctx, data, opt, intent, hostProbeCommand{})
				if err != nil {
					return err
				}
				if receipt = provisioning.EncodeProbeInitReceipt(intent, result); receipt == nil {
					return domain.ErrProbeBinaryStageWriteUnknown
				}
				return nil
			}, "opt")
		}, "var", "lib")
		return receipt, err
	case domain.ProbeHostActionCleanup:
		// systemctl is only needed when the unit file is present, so a host
		// without it can still clean a partial install that never wrote one.
		// ApplyProbeCleanup refuses a present unit when it is nil.
		var systemctl provisioning.ProbeSystemctl
		if ctl, err := newHostProbeSystemctl(); err == nil {
			systemctl = ctl
		}
		return nil, withProbeHostParent(func(data *os.File) error {
			return withProbeHostParent(func(opt *os.File) error {
				return withProbeHostParent(func(unit *os.File) error {
					return provisioning.ApplyProbeCleanup(ctx, data, opt, unit, intent, systemctl)
				}, "etc", "systemd", "system")
			}, "opt")
		}, "var", "lib")
	default:
		return nil, domain.ErrProbeBinaryStageWriteInput
	}
}

// withProbeHostParent opens one protected fixed directory chain, runs fn
// beneath it and closes it.
func withProbeHostParent(fn func(*os.File) error, names ...string) error {
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
	return fn(parent)
}

// hostProbeCommand runs the staged probe binary for probe_init with fixed
// arguments, an empty environment, a bounded timeout and bounded output.
type hostProbeCommand struct{}

// Run returns bounded stdout and the exit status; a nonzero exit is not an
// error. stderr is discarded: it is never part of any receipt.
func (hostProbeCommand) Run(ctx context.Context, path string, args ...string) ([]byte, int, error) {
	runCtx, cancel := context.WithTimeout(ctx, probeSystemctlTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, path, args...)
	cmd.Env = []string{}
	cmd.Dir = "/"
	stdout := &probeBoundedBuffer{max: probeSystemctlMaxOutput}
	cmd.Stdout = stdout
	cmd.Stderr = &probeBoundedBuffer{max: probeSystemctlMaxOutput}
	err := cmd.Run()
	if stdout.over {
		return nil, -1, domain.ErrProbeBinaryStageWriteUnknown
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && runCtx.Err() == nil {
		return stdout.Bytes(), exit.ExitCode(), nil
	}
	if err != nil {
		return nil, -1, err
	}
	return stdout.Bytes(), 0, nil
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
