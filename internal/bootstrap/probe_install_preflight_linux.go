//go:build linux

package bootstrap

import (
	"context"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// gatherHostProbeInstallReport reads fixed host facts only. The authority
// check reuses the authorized helpers' protected loader, so "authority=1"
// means those helpers would accept the key. A missing install parent counts
// as no entries; an unreadable one fails the whole report.
func gatherHostProbeInstallReport(ctx context.Context) (domain.ProbeInstallHostReport, error) {
	var zero domain.ProbeInstallHostReport
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		return zero, domain.ErrProbeBinaryStageWriteUnavailable
	}
	report := domain.ProbeInstallHostReport{
		Kernel:  unix.ByteSliceToString(uts.Sysname[:]),
		Machine: unix.ByteSliceToString(uts.Machine[:]),
		Root:    os.Geteuid() == 0,
	}
	booted, err := os.Stat("/run/systemd/system")
	report.Systemd = err == nil && booted.IsDir() && probeInstallExecutable("/usr/bin/systemctl", "/bin/systemctl")
	report.DockerCLI = probeInstallExecutable("/usr/bin/docker", "/usr/local/bin/docker") && probeInstallSocket(probeInstallDockerSocket)
	if report.Root {
		key, err := loadHostProbeBinaryStageAuthority(ctx)
		report.Authority = err == nil && key != ([32]byte{})
	}
	for _, parent := range probeInstallPreflightParents {
		if err := ctx.Err(); err != nil {
			return zero, probeBinaryStageHelperContextError(err)
		}
		entries, err := os.ReadDir(parent)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return zero, domain.ErrProbeBinaryStageWriteUnavailable
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), probeInstallPreflightOwnedPrefix) &&
				report.ExistingInstalls < domain.ProbeInstallPreflightMaxExisting {
				report.ExistingInstalls++
			}
		}
	}
	if !report.Valid() {
		return zero, domain.ErrProbeBinaryStageWriteUnavailable
	}
	return report, nil
}

// probeInstallDockerSocket is the daemon socket the Docker install path uses.
const probeInstallDockerSocket = "/var/run/docker.sock"

// probeInstallSocket reports whether path is itself a unix socket (Lstat, so a
// link or any other file type never counts).
func probeInstallSocket(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSocket != 0
}

// probeInstallExecutable reports whether any fixed path is an executable
// regular file (symlinks followed, as an operator's install would be).
func probeInstallExecutable(paths ...string) bool {
	for _, path := range paths {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return true
		}
	}
	return false
}
