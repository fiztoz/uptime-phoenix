package domain

import (
	"errors"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// Provisioning errors are fixed, nonsecret diagnostics. Transport implementations
// must not attach remote output, credentials, or underlying SSH error strings.
var (
	ErrProbeSSHInput       = errors.New("invalid SSH preflight input")
	ErrProbeSSHTrust       = errors.New("SSH host identity verification failed")
	ErrProbeSSHDestination = errors.New("SSH destination rejected by policy")
	ErrProbeSSHConnect     = errors.New("SSH connection or authentication failed")
	ErrProbeSSHDiscovery   = errors.New("SSH host discovery failed")
	ErrProbeHostPlatform   = errors.New("unsupported probe host OS or architecture")
	ErrProbeHostRuntime    = errors.New("probe host requires systemd or an existing Docker CLI")
	ErrProbeHostPrivilege  = errors.New("probe host requires root or noninteractive sudo")
)

// ProbeSSHHost identifies an operator-authorized SSH destination, not a runtime
// probe registration. Credentials and host trust are deliberately separate.
type ProbeSSHHost struct {
	Address string
	User    string
}

// Valid checks syntax without resolving or contacting the destination.
func (h ProbeSSHHost) Valid() bool {
	if len(h.User) < 1 || len(h.User) > 64 || len(h.Address) > 260 {
		return false
	}
	for _, c := range h.User {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	host, port, err := net.SplitHostPort(h.Address)
	if err != nil || host == "" || strings.ContainsAny(host, "%/\\@?# \t\r\n\x00") {
		return false
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return false
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.Zone() == ""
	}
	if len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) < 1 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// ProbeHostFacts contains only bounded discovery fields. DockerCLI means the
// executable exists, not that a daemon is reachable or installation succeeded.
// Systemd requires both systemctl and the running-system marker directory.
type ProbeHostFacts struct {
	Kernel    string
	Machine   string
	DockerCLI bool
	Systemd   bool
	Privilege string // root, sudo (sudo -n true succeeded), or none
}

// ProbeHostPreflight is compatibility evidence only. It is never an enrollment,
// runtime reachability, configuration activation, or installation receipt.
type ProbeHostPreflight struct {
	OS           string
	Architecture string
	DockerCLI    bool
	Systemd      bool
	Privilege    string
}
