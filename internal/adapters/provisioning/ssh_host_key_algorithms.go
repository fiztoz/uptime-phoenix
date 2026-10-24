package provisioning

import (
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"slices"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// sshHostKeyProbeTimeout bounds the whole learning step: every family is
// probed concurrently within it. A real sshd answers a pre-authentication
// handshake well inside it; a server that does not answer leaves the original
// trust failure standing.
const sshHostKeyProbeTimeout = 5 * time.Second

// sshHostKeyFamilies are tried in order to find which host-key family a server
// presents the pinned key under. A server offers one key per negotiated
// algorithm, and Go prefers ECDSA, so pinning an ED25519 or RSA fingerprint
// would otherwise always fail.
var sshHostKeyFamilies = [][]string{
	{ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521},
	{ssh.KeyAlgoED25519},
	{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256},
}

var errSSHHostKeyProbeDone = errors.New("ssh host key probe complete")

// hostKeyAlgorithms returns the host-key algorithms previously learned for
// address, or nil to use the library default. It performs no I/O.
func (s *SSHHostInspector) hostKeyAlgorithms(address string) []string {
	if s == nil || s.pin == "" {
		return nil
	}
	s.algMu.Lock()
	defer s.algMu.Unlock()
	return s.algCache[address]
}

// learnHostKeyAlgorithms is called after a pre-authentication trust
// rejection. It probes each host-key family once, concurrently and within one
// bound, with handshakes aborted inside the host-key callback before any user
// authentication, and caches the first family (in sshHostKeyFamilies order)
// under which the server presents the pinned key. It reports whether a
// retry restricted to a newly learned family could succeed. With known_hosts
// trust, or when no family presents the pinned key, it returns false and the
// trust failure stands.
func (s *SSHHostInspector) learnHostKeyAlgorithms(ctx context.Context, address string) bool {
	if s == nil || s.pin == "" {
		return false
	}
	previous := s.hostKeyAlgorithms(address)
	pctx, cancel := context.WithTimeout(ctx, sshHostKeyProbeTimeout)
	defer cancel()
	matched := make([]bool, len(sshHostKeyFamilies))
	var wg sync.WaitGroup
	for i, family := range sshHostKeyFamilies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			matched[i] = s.probeHostKeyFamily(pctx, address, family)
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return false
	}
	for i, family := range sshHostKeyFamilies {
		if !matched[i] {
			continue
		}
		if slices.Equal(family, previous) {
			return false
		}
		s.algMu.Lock()
		if s.algCache == nil {
			s.algCache = make(map[string][]string)
		}
		s.algCache[address] = family
		s.algMu.Unlock()
		return true
	}
	return false
}

// probeHostKeyFamily reports whether the server presents the pinned key when
// restricted to family. It never authenticates: the callback always aborts.
func (s *SSHHostInspector) probeHostKeyFamily(pctx context.Context, address string, family []string) bool {
	conn, err := s.connect(pctx, address)
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(pctx, func() { _ = conn.Close() })
	defer stop()
	if deadline, ok := pctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	matched := false
	config := &ssh.ClientConfig{
		User:              "phoenix-host-key-probe",
		HostKeyAlgorithms: family,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			matched = subtle.ConstantTimeCompare([]byte(ssh.FingerprintSHA256(key)), []byte(s.pin)) == 1
			return errSSHHostKeyProbeDone
		},
	}
	if c, chans, reqs, err := ssh.NewClientConn(conn, address, config); err == nil {
		// Unreachable: the callback always aborts the handshake.
		_ = ssh.NewClient(c, chans, reqs).Close()
		return false
	}
	return matched
}

// withHostKeyRetry runs one send attempt and, only when it was refused by a
// pre-authentication host-key trust rejection (nothing was executed), learns
// the server's matching host-key family and retries exactly once.
func (s *SSHBinaryStageSender) withHostKeyRetry(ctx context.Context, address string, attempt func() (SSHBinaryStageAck, error)) (SSHBinaryStageAck, error) {
	ack, err := attempt()
	if errors.Is(err, domain.ErrProbeSSHTrust) && s != nil && s.insp != nil && s.insp.host.learnHostKeyAlgorithms(ctx, address) {
		return attempt()
	}
	return ack, err
}
