package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

const probeSSHPreflightUsage = `Usage: phoenix-probe-admin inspect-host --address HOST:PORT --user USER --identity-file PATH (--host-key-sha256 SHA256:BASE64 | --known-hosts PATH) [--endpoint-policy PATH]
Read-only SSH preflight; no hub environment or database is needed.
Use a temporary deployment key in a private regular file. Trust must be verified
out of band; host-key scanning alone is not verification. No passwords, SSH agent,
SSH config, forwarding, or automatic trust-on-first-use are used. Private/loopback
hosts require an explicit endpoint-policy CIDR; metadata/link-local are forbidden.
Runs fixed uname/runtime discovery and, if not root, sudo -n /bin/true without a
password prompt. The host's normal SSH/sudo audit logging still applies.
The 30-second limit covers DNS, connect, handshake and discovery. Output contains
only compatibility facts, never remote terminal output or credentials. A passed
preflight is NOT an install, runtime reachability, enrollment or config receipt.
`

// RunProbeSSHPreflight is the optional local-operator composition root for M8's
// first slice. It never opens the hub database or mutates a remote installation.
// Exit 0 means compatible host prerequisites only; installation is not attempted.
func RunProbeSSHPreflight(ctx context.Context, args []string, out, stderr io.Writer) int {
	f := flag.NewFlagSet("inspect-host", flag.ContinueOnError)
	f.SetOutput(io.Discard) // flag errors may contain arbitrary secret arguments
	var address, user, identity, fingerprint, knownHosts, policyFile string
	f.StringVar(&address, "address", "", "SSH hostname/IP and numeric port")
	f.StringVar(&user, "user", "", "SSH user")
	f.StringVar(&identity, "identity-file", "", "private temporary deployment key file")
	f.StringVar(&fingerprint, "host-key-sha256", "", "verified OpenSSH SHA256 fingerprint")
	f.StringVar(&knownHosts, "known-hosts", "", "verified OpenSSH known_hosts file")
	f.StringVar(&policyFile, "endpoint-policy", "", "explicit destination CIDR policy file")
	if err := f.Parse(args); errors.Is(err, flag.ErrHelp) {
		_, _ = io.WriteString(out, probeSSHPreflightUsage)
		return 0
	} else if err != nil || f.NArg() != 0 {
		_, _ = io.WriteString(stderr, probeSSHPreflightUsage)
		return 2
	}
	host := domain.ProbeSSHHost{Address: address, User: user}
	if !host.Valid() || identity == "" || (fingerprint == "") == (knownHosts == "") {
		_, _ = io.WriteString(stderr, probeSSHPreflightUsage)
		return 2
	}
	fail := func(message string) int { _, _ = fmt.Fprintln(stderr, message); return 1 }
	if ctx.Err() != nil {
		return fail("SSH preflight canceled")
	}
	policy, err := probe.LoadEndpointPolicy(policyFile)
	if err != nil {
		return fail("SSH endpoint policy is invalid")
	}
	key, err := readPrivateProbeInput(identity, 64*1024)
	if err != nil {
		return fail("SSH identity must be a private regular file of at most 65536 bytes")
	}
	defer clear(key)
	inspector, err := provisioning.NewSSHHostInspector(ctx, key, provisioning.SSHHostTrust{
		Fingerprint: fingerprint, KnownHostsFile: knownHosts,
	}, policy)
	if err != nil {
		return fail("SSH key or explicit host trust is invalid or unavailable")
	}
	result, err := services.NewProbeHostPreflightService(inspector).Inspect(ctx, host)
	if err != nil {
		// Only fixed diagnostics cross the CLI boundary, including when an
		// unexpected transport error or a future implementation is supplied.
		for _, safe := range []error{
			domain.ErrProbeSSHInput, domain.ErrProbeSSHTrust, domain.ErrProbeSSHDestination,
			domain.ErrProbeSSHConnect, domain.ErrProbeSSHDiscovery, domain.ErrProbeHostPlatform,
			domain.ErrProbeHostPrivilege, domain.ErrProbeHostRuntime,
		} {
			if errors.Is(err, safe) {
				return fail(safe.Error())
			}
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return fail("SSH preflight canceled or timed out")
		}
		return fail("SSH preflight failed")
	}
	// Explicit view: never serialize a domain object (including CLI output).
	view := struct {
		State        string `json:"state"`
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		DockerCLI    bool   `json:"docker_cli"`
		Systemd      bool   `json:"systemd"`
		Privilege    string `json:"privilege"`
	}{"preflight_passed", result.OS, result.Architecture, result.DockerCLI, result.Systemd, result.Privilege}
	if json.NewEncoder(out).Encode(view) != nil {
		return fail("SSH preflight result could not be written")
	}
	return 0
}
