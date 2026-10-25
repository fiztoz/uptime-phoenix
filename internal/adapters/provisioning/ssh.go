// Package provisioning implements optional operator-driven probe host discovery.
// Hub use is explicitly opt-in; it never installs or updates a remote host.
package provisioning

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const (
	preflightTimeout = 30 * time.Second
	maxDiscoverySize = 4096
)

// Fixed POSIX-shell commands only: no user-provided script, environment, PTY,
// forwarding, agent, password fallback, package manager, or persistent writes.
// sudo -n true tests privilege without prompting or changing host configuration.
// Docker discovery checks presence only; it never contacts a Docker context.
const discoveryCommand = `LC_ALL=C; export LC_ALL
set -eu
printf 'phoenix-preflight-v1\n'
printf 'kernel=%s\n' "$(uname -s)"
printf 'machine=%s\n' "$(uname -m)"
if command -v docker >/dev/null 2>&1; then printf 'docker=1\n'; else printf 'docker=0\n'; fi
if command -v systemctl >/dev/null 2>&1 && test -d /run/systemd/system; then printf 'systemd=1\n'; else printf 'systemd=0\n'; fi
if test "$(id -u)" = 0; then
  printf 'privilege=root\n'
elif command -v sudo >/dev/null 2>&1 && sudo -n -- /bin/true >/dev/null 2>&1; then
  printf 'privilege=sudo\n'
else
  printf 'privilege=none\n'
fi
`

// SSHHostTrust requires exactly one operator-verified trust source. Fingerprint
// is canonical OpenSSH SHA256:base64 (unpadded). KnownHostsFile uses OpenSSH's
// hostname/port matching, hashed hosts, certificates and revocation rules. No
// trust-on-first-use or automatic host-key replacement is supported.
type SSHHostTrust struct {
	Fingerprint    string
	KnownHostsFile string
}

// SSHHostInspector holds an ephemeral parsed key for one CLI run or API job.
// It never retains the input key bytes, reads an SSH agent/config, or stores
// credentials in hub state. Destroy the inspector after completing preflight.
type SSHHostInspector struct {
	signer ssh.Signer
	trust  ssh.HostKeyCallback
	policy probe.EndpointPolicy
	lookup func(context.Context, string, string) ([]netip.Addr, error)
	dial   func(context.Context, string, string) (net.Conn, error)
	// pin is the explicit host fingerprint, empty for known_hosts trust.
	pin      string
	algMu    sync.Mutex
	algCache map[string][]string
}

var _ ports.ProbeHostInspector = (*SSHHostInspector)(nil)

// NewSSHHostInspector parses a temporary unencrypted deployment key and an
// explicit trust source. Callers should clear their privateKey buffer afterward.
// Passphrases/passwords are intentionally unsupported in this first slice.
func NewSSHHostInspector(ctx context.Context, privateKey []byte, trust SSHHostTrust, policy probe.EndpointPolicy) (*SSHHostInspector, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(privateKey) == 0 || len(privateKey) > 64*1024 || (trust.Fingerprint == "") == (trust.KnownHostsFile == "") {
		return nil, domain.ErrProbeSSHInput
	}
	var verify ssh.HostKeyCallback
	if trust.Fingerprint != "" {
		if !validFingerprint(trust.Fingerprint) {
			return nil, domain.ErrProbeSSHInput
		}
		verify = func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if subtle.ConstantTimeCompare([]byte(ssh.FingerprintSHA256(key)), []byte(trust.Fingerprint)) != 1 {
				return domain.ErrProbeSSHTrust
			}
			return nil
		}
	} else {
		info, err := os.Lstat(trust.KnownHostsFile)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 1024*1024 {
			return nil, domain.ErrProbeSSHInput
		}
		verify, err = knownhosts.New(trust.KnownHostsFile)
		if err != nil {
			return nil, domain.ErrProbeSSHInput
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		return nil, domain.ErrProbeSSHInput
	}
	policy.AllowedCIDRs = slices.Clone(policy.AllowedCIDRs)
	for _, prefix := range policy.AllowedCIDRs {
		if !prefix.IsValid() || prefix.Addr().Is4In6() || prefix != prefix.Masked() {
			return nil, domain.ErrProbeSSHInput
		}
	}
	return &SSHHostInspector{
		signer: signer, trust: verify, policy: policy, pin: trust.Fingerprint,
		lookup: net.DefaultResolver.LookupNetIP,
		dial:   (&net.Dialer{}).DialContext,
	}, nil
}

func validFingerprint(pin string) bool {
	if !strings.HasPrefix(pin, "SHA256:") {
		return false
	}
	value := strings.TrimPrefix(pin, "SHA256:")
	decoded, err := base64.RawStdEncoding.DecodeString(value)
	return err == nil && len(decoded) == 32 && base64.RawStdEncoding.EncodeToString(decoded) == value
}

// InspectHost authenticates only after host-key verification, then runs one
// fixed discovery script with bounded stdout+stderr. Every failure discards raw
// output and library diagnostics. Cancellation closes the underlying connection
// during handshake, channel creation, command execution and output collection.
func (s *SSHHostInspector) InspectHost(ctx context.Context, host domain.ProbeSSHHost) (domain.ProbeHostFacts, error) {
	facts, err := s.inspectHostOnce(ctx, host)
	// A pre-authentication trust rejection executed nothing: learn the host's
	// matching key family once and retry.
	if errors.Is(err, domain.ErrProbeSSHTrust) && ctx != nil && s.learnHostKeyAlgorithms(ctx, host.Address) {
		return s.inspectHostOnce(ctx, host)
	}
	return facts, err
}

func (s *SSHHostInspector) inspectHostOnce(ctx context.Context, host domain.ProbeSSHHost) (domain.ProbeHostFacts, error) {
	text, err := s.runFixedCommand(ctx, host, discoveryCommand)
	if errors.Is(err, errSSHAuthRejected) {
		// Discovery keeps its documented connection-or-authentication sentinel.
		return domain.ProbeHostFacts{}, domain.ErrProbeSSHConnect
	}
	if err != nil {
		return domain.ProbeHostFacts{}, err
	}
	return parseDiscovery(text)
}

// errSSHAuthRejected reports a handshake that verified the host key and then
// failed: the server refused the client key (or dropped the connection
// during authentication). Nothing was executed.
var errSSHAuthRejected = errors.New("SSH authentication rejected after host verification")

// runFixedCommand authenticates only after host-key verification, runs one
// fixed command with bounded stdout+stderr and returns its stdout. A nonzero
// exit, oversized output or any channel failure is ErrProbeSSHDiscovery; raw
// output and library diagnostics are discarded. Cancellation closes the
// connection during handshake, channel creation, execution and collection.
func (s *SSHHostInspector) runFixedCommand(ctx context.Context, host domain.ProbeSSHHost, command string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s == nil || s.signer == nil || s.trust == nil || !host.Valid() {
		return "", domain.ErrProbeSSHInput
	}
	conn, err := s.connect(ctx, host.Address)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return "", domain.ErrProbeSSHConnect
	}
	trustRejected, trusted := false, false
	config := &ssh.ClientConfig{
		User:              host.User,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(s.signer)},
		HostKeyAlgorithms: s.hostKeyAlgorithms(host.Address),
		HostKeyCallback: func(name string, addr net.Addr, key ssh.PublicKey) error {
			if err := s.trust(name, addr, key); err != nil {
				trustRejected = true
				return domain.ErrProbeSSHTrust
			}
			trusted = true
			return nil
		},
	}
	cc, channels, requests, err := ssh.NewClientConn(conn, host.Address, config)
	if err != nil {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if trustRejected {
			return "", domain.ErrProbeSSHTrust
		}
		if trusted {
			return "", errSSHAuthRejected
		}
		return "", domain.ErrProbeSSHConnect
	}
	client := ssh.NewClient(cc, channels, requests)
	defer func() { _ = client.Close() }()
	session, err := client.NewSession()
	if err != nil {
		return "", discoveryError(ctx)
	}
	defer func() { _ = session.Close() }()
	output := &discoveryOutput{close: conn.Close}
	session.Stdout = discoveryWriter{output: output, capture: true}
	session.Stderr = discoveryWriter{output: output}
	if err := session.Run(command); err != nil {
		return "", discoveryError(ctx)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if output.exceeded {
		return "", domain.ErrProbeSSHDiscovery
	}
	return output.stdout.String(), nil
}

func discoveryError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return domain.ErrProbeSSHDiscovery
}

// Resolve once, authorize each candidate, and dial only a numeric IP. Keep the
// original hostname:port for known_hosts so DNS cannot change the trust target.
func (s *SSHHostInspector) connect(ctx context.Context, address string) (net.Conn, error) {
	host, port, _ := net.SplitHostPort(address) // syntax was validated before I/O
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip}
	} else {
		var err error
		ips, err = s.lookup(ctx, "ip", host)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, domain.ErrProbeSSHConnect
		}
	}
	permitted := false
	for _, ip := range ips {
		if ip.Zone() != "" || probe.ValidateDestination(ip, s.policy) != nil {
			continue
		}
		permitted = true
		conn, err := s.dial(ctx, "tcp", net.JoinHostPort(ip.Unmap().String(), port))
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	if !permitted {
		return nil, domain.ErrProbeSSHDestination
	}
	return nil, domain.ErrProbeSSHConnect
}

type discoveryOutput struct {
	mu       sync.Mutex
	stdout   bytes.Buffer
	size     int
	exceeded bool
	close    func() error
}

type discoveryWriter struct {
	output  *discoveryOutput
	capture bool
}

func (w discoveryWriter) Write(p []byte) (int, error) {
	o := w.output
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(p) > maxDiscoverySize-o.size {
		o.exceeded = true
		_ = o.close()
		return 0, domain.ErrProbeSSHDiscovery
	}
	o.size += len(p)
	if w.capture {
		return o.stdout.Write(p)
	}
	return len(p), nil
}

func parseDiscovery(text string) (domain.ProbeHostFacts, error) {
	var empty domain.ProbeHostFacts
	lines := strings.Split(text, "\n")
	if len(lines) != 7 || lines[0] != "phoenix-preflight-v1" || lines[6] != "" {
		return empty, domain.ErrProbeSSHDiscovery
	}
	keys := []string{"kernel=", "machine=", "docker=", "systemd=", "privilege="}
	values := make([]string, len(keys))
	for i, key := range keys {
		if !strings.HasPrefix(lines[i+1], key) {
			return empty, domain.ErrProbeSSHDiscovery
		}
		values[i] = strings.TrimPrefix(lines[i+1], key)
	}
	for _, value := range values[:2] {
		if len(value) < 1 || len(value) > 64 {
			return empty, domain.ErrProbeSSHDiscovery
		}
		for _, c := range value {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
				return empty, domain.ErrProbeSSHDiscovery
			}
		}
	}
	if values[2] != "0" && values[2] != "1" || values[3] != "0" && values[3] != "1" ||
		!slices.Contains([]string{"root", "sudo", "none"}, values[4]) {
		return empty, domain.ErrProbeSSHDiscovery
	}
	return domain.ProbeHostFacts{
		Kernel: values[0], Machine: values[1], DockerCLI: values[2] == "1", Systemd: values[3] == "1", Privilege: values[4],
	}, nil
}

// Check interfaces without exposing a general-purpose command runner.
var _ io.Writer = discoveryWriter{}
