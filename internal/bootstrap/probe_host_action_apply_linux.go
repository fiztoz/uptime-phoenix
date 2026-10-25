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

	// probeDockerTimeout bounds every docker invocation but load, which
	// probeDockerLoadTimeout bounds. probeDockerMaxOutput bounds stdout; the
	// largest answer is docker load's per-layer progress for a 128-layer image.
	probeDockerTimeout     = 30 * time.Second
	probeDockerLoadTimeout = 10 * time.Minute
	probeDockerMaxOutput   = 1 << 20
	// probeDockerHost pins every invocation to the daemon socket preflight
	// checks, so a docker context in root's config can never redirect the
	// install to another daemon.
	probeDockerHost = "unix:///var/run/docker.sock"
)

// probeSystemctlCandidates are the only systemctl paths ever executed.
var probeSystemctlCandidates = []string{"/usr/bin/systemctl", "/bin/systemctl"}

// probeDockerCandidates are the only docker CLI paths ever executed.
var probeDockerCandidates = []string{"/usr/bin/docker", "/usr/local/bin/docker"}

// applyHostProbeAction opens the protected fixed parent(s) for the intent's
// kind (literal /var/lib, /opt or /etc/systemd/system) and applies the action
// beneath them; Docker identities route to the Docker appliers. stagedArchive
// is the verified image_cache payload path and is empty for every other kind.
// Only probe_init returns its own receipt.
func applyHostProbeAction(ctx context.Context, intent domain.ProbeHostActionIntent, stagedArchive string) ([]byte, error) {
	if err := probeBinaryStageAuthorityPrecheck(ctx); err != nil {
		return nil, err
	}
	if intent.Identity.Mode == domain.ProbeInstallDocker {
		return applyProbeDockerHostAction(ctx, intent, stagedArchive,
			func() (provisioning.ProbeDocker, error) { return newHostProbeDocker() },
			func(fn func(*os.File) error) error { return withProbeHostParent(fn, "var", "lib") })
	}
	if stagedArchive != "" {
		return nil, domain.ErrProbeBinaryStageWriteInput
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
	return runHostProbeFixed(ctx, s.path, []string{}, probeSystemctlTimeout, probeSystemctlMaxOutput, args)
}

// hostProbeDocker executes one root-owned, non-group/world-writable docker CLI
// from a fixed absolute path with a minimal fixed environment.
type hostProbeDocker struct{ path string }

func newHostProbeDocker() (*hostProbeDocker, error) {
	for _, candidate := range probeDockerCandidates {
		info, err := os.Stat(candidate)
		if err != nil {
			continue
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !info.Mode().IsRegular() || !ok || st.Uid != 0 || info.Mode().Perm()&0o022 != 0 {
			return nil, domain.ErrProbeBinaryStageWriteUnavailable
		}
		return &hostProbeDocker{path: candidate}, nil
	}
	return nil, domain.ErrProbeBinaryStageWriteUnavailable
}

// Run returns bounded stdout and the exit status; a nonzero exit is not an
// error. The environment is only HOME (the CLI reads root's config there) and
// the pinned local daemon socket; stderr is discarded. docker load streams a
// whole image archive and gets the longer timeout.
func (d *hostProbeDocker) Run(ctx context.Context, args ...string) (string, int, error) {
	timeout := probeDockerTimeout
	if len(args) > 0 && args[0] == "load" {
		timeout = probeDockerLoadTimeout
	}
	return runHostProbeFixed(ctx, d.path, []string{"HOME=/root", "DOCKER_HOST=" + probeDockerHost}, timeout, probeDockerMaxOutput, args)
}

// runHostProbeFixed runs one fixed absolute executable with the given
// environment, a bounded timeout and bounded stdout, from "/". A nonzero exit
// is returned as the exit code, not an error; stderr is never kept.
func runHostProbeFixed(ctx context.Context, path string, env []string, timeout time.Duration, maxOutput int, args []string) (string, int, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, path, args...)
	cmd.Env = env
	cmd.Dir = "/"
	stdout := &probeBoundedBuffer{max: maxOutput}
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

// stageHostProbeImage opens the exclusive staging file for one image_cache
// attempt inside the protected /var/lib/uptime-phoenix-image-stage directory.
func stageHostProbeImage(ctx context.Context, intent domain.ProbeHostActionIntent) (probeStagedArchive, error) {
	if err := probeBinaryStageAuthorityPrecheck(ctx); err != nil {
		return nil, err
	}
	var staged *provisioning.ProbeImageStageFile
	err := withProbeHostParent(func(parent *os.File) error {
		file, err := provisioning.OpenProbeImageStageFile(ctx, parent, "/var/lib", intent)
		staged = file
		return err
	}, "var", "lib")
	if err != nil {
		return nil, err
	}
	return staged, nil
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
