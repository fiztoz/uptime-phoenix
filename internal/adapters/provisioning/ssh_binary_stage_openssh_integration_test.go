//go:build linux

package provisioning_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// The pinned LinuxServer fixture packages OpenSSH's PAM-capable binaries with
// this suffix. UsePAM is explicitly disabled in the generated sshd configuration.
const sshStageSSHD = "/usr/sbin/sshd.pam"

// TestSSHBinaryStageOpenSSH is opt-in ONLY in the task-owned private PID/network
// namespace. The dedicated runner rejects every skipped or missing leaf. Probe
// fixtures are data; only sshd, its shell and the built admin helper execute.
func TestSSHBinaryStageOpenSSH(t *testing.T) {
	if os.Getenv("PHOENIX_M8_SSH_SANDBOX") != "1" {
		t.Skip("requires dedicated isolated OpenSSH sandbox")
	}
	sshSandboxGuard(t)
	t.Run("Fixture", func(t *testing.T) {
		out, err := exec.CommandContext(t.Context(), sshStageSSHD, "-V").CombinedOutput()
		if err != nil || !strings.HasPrefix(string(out), "OpenSSH_") {
			t.Fatalf("actual OpenSSH version check failed: %v", err)
		}
		t.Log(strings.TrimSpace(string(out)))
		for _, path := range []string{"/", "/opt", "/usr/local/bin/phoenix-probe-admin"} {
			st, err := os.Lstat(path)
			if err != nil || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0o022 != 0 || st.Sys().(*syscall.Stat_t).Uid != 0 {
				t.Fatalf("untrusted fixture path %s", path)
			}
		}
	})
	if t.Failed() {
		return
	}
	t.Run("ActualHelper", func(t *testing.T) {
		for _, arch := range []string{"amd64", "arm64"} {
			t.Run(arch, func(t *testing.T) {
				f := newSSHStageFixture(t, "")
				binary := f.prepared(t, arch)
				intent := f.intent(t, binary.Pin())
				ack, err := f.sender(t).Send(t.Context(), intent, binary)
				if err != nil || ack != (provisioning.SSHBinaryStageAck{Intent: intent}) {
					t.Fatalf("actual fixed helper did not acknowledge: %v", err)
				}
				assertSSHStageGraph(t, intent)
				binary.assertBorrowed(t, 1)
			})
		}
	})
	t.Run("ReplayPreservesForeign", func(t *testing.T) {
		f := newSSHStageFixture(t, "")
		binary := f.prepared(t, "arm64")
		intent := f.intent(t, binary.Pin())
		sender := f.sender(t)
		if _, err := sender.Send(t.Context(), intent, binary); err != nil {
			t.Fatal(err)
		}
		assertSSHStageGraph(t, intent)
		marker := filepath.Join(intent.Directory(), domain.ProbeBinaryStageMarkerName)
		if err := os.Chmod(marker, 0o600); err != nil {
			t.Fatal(err)
		}
		foreign := []byte("foreign marker fixture; never adopted\n")
		if err := os.WriteFile(marker, foreign, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(marker, 0o444); err != nil {
			t.Fatal(err)
		}
		past := time.Unix(1234567890, 0)
		if err := os.Chtimes(marker, past, past); err != nil {
			t.Fatal(err)
		}
		before, err := os.Lstat(marker)
		if err != nil {
			t.Fatal(err)
		}
		ack, err := sender.Send(t.Context(), intent, binary)
		if err != provisioning.ErrSSHBinaryStageUnknown || ack != (provisioning.SSHBinaryStageAck{}) {
			t.Fatalf("remote conflict invented acknowledgement: %v", err)
		}
		after, err := os.Lstat(marker)
		got, readErr := os.ReadFile(marker)
		if err != nil || readErr != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) || !bytes.Equal(got, foreign) {
			t.Fatal("replay modified or adopted the foreign marker")
		}
		binary.assertBorrowed(t, 2)
	})
	for _, tc := range []struct{ name, forced string }{
		{"PostStageNonzero", "/fixtures/post-stage-nonzero"},
		{"PostStageLostReceipt", "/fixtures/post-stage-lost-receipt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// An explicit fixture fault: OpenSSH's ForceCommand verifies the
			// exact original exec, runs the unchanged helper, then changes only
			// its exit/receipt delivery. Positive tests use no ForceCommand.
			f := newSSHStageFixture(t, tc.forced)
			binary := f.prepared(t, "arm64")
			intent := f.intent(t, binary.Pin())
			ack, err := f.sender(t).Send(t.Context(), intent, binary)
			if err != provisioning.ErrSSHBinaryStageUnknown || ack != (provisioning.SSHBinaryStageAck{}) {
				t.Fatalf("post-stage fault invented acknowledgement: %v", err)
			}
			assertSSHStageGraph(t, intent)
			binary.assertBorrowed(t, 1)
		})
	}
	t.Run("LastByteDisconnectUnknown", func(t *testing.T) {
		f := newSSHStageFixture(t, "")
		binary := f.prepared(t, "arm64")
		intent := f.intent(t, binary.Pin())
		gate := &sshStageEOFHold{atEOF: make(chan struct{}), closed: make(chan struct{})}
		binary.wrap = func(r io.ReadCloser) io.ReadCloser { gate.attach(r); return gate }
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		type result struct {
			ack provisioning.SSHBinaryStageAck
			err error
		}
		done := make(chan result, 1)
		sender := f.sender(t)
		go func() { ack, err := sender.Send(ctx, intent, binary); done <- result{ack, err} }()
		joined := false
		t.Cleanup(func() {
			cancel()
			_ = gate.Close()
			if !joined {
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Error("local sender failed to join during cleanup")
				}
			}
		})
		select {
		case <-gate.atEOF:
		case <-time.After(10 * time.Second):
			t.Fatal("source never reached the real EOF boundary")
		}
		cancel()
		select {
		case r := <-done:
			joined = true
			if r.err != provisioning.ErrSSHBinaryStageUnknown || r.ack != (provisioning.SSHBinaryStageAck{}) {
				t.Fatalf("last-byte disconnect invented certainty: %v", r.err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("local sender did not join after disconnect")
		}
		// SSH disconnect may become helper stdin EOF and cause staging. Do not
		// assert absence or completeness: any partial/exact graph is retained.
		// Teardown below joins only this approved fixture's namespace processes.
		f.stop(t)
		_, statErr := os.Lstat(intent.Directory())
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			t.Fatal(statErr)
		}
		t.Logf("unknown outcome retained; directory_observed=%t", statErr == nil)
		binary.assertBorrowed(t, 1)
	})
	if _, err := os.Stat("/tmp/probe-executed"); !errors.Is(err, os.ErrNotExist) {
		t.Error("probe fixture execution canary is not absent")
	}
}

func sshSandboxGuard(t *testing.T) {
	t.Helper()
	nonce := os.Getenv("PHOENIX_M8_SSH_SANDBOX_NONCE")
	got, err := os.ReadFile("/fixtures/ssh-sandbox-nonce")
	if os.Getpid() != 1 || os.Geteuid() != 0 || !strings.HasPrefix(nonce, "phoenix-m8-ssh-") || err != nil || string(got) != nonce {
		t.Fatal("private sandbox identity required")
	}
	status, err := os.ReadFile("/proc/self/status")
	if err != nil || !strings.Contains(string(status), "NoNewPrivs:\t1\n") {
		t.Fatal("no-new-privileges required")
	}
	// These are the only approved capabilities: sshd privilege separation,
	// session identity and joining namespace-owned children, never host access.
	const allowed = uint64(1<<unix.CAP_SETUID | 1<<unix.CAP_SETGID | 1<<unix.CAP_SYS_CHROOT | 1<<unix.CAP_KILL)
	for _, key := range []string{"CapEff:", "CapPrm:", "CapBnd:"} {
		found := false
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, key) {
				value, parseErr := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, key)), 16, 64)
				if parseErr != nil || value != allowed {
					t.Fatalf("unexpected sandbox capability mask %s", key)
				}
				found = true
			}
		}
		if !found {
			t.Fatalf("missing capability mask %s", key)
		}
	}
}

type sshStageFixture struct {
	address   string
	clientKey []byte
	hostPin   string
	clientPin string
	cmd       *exec.Cmd
	done      chan error
	stopOnce  sync.Once
}

func sshStageKey(t *testing.T) ([]byte, ssh.Signer) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(key, "task-owned fixture")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(block), signer
}

func newSSHStageFixture(t *testing.T, forced string) *sshStageFixture {
	t.Helper()
	return newSSHStageConfiguredFixture(t, forced, nil)
}

// newSSHStageConfiguredFixture permits only test-owned, pre-start policy setup.
// The callback receives the generated fixture directory, never a host account.
func newSSHStageConfiguredFixture(t *testing.T, forced string, configure func(*sshStageFixture, string, string) string) *sshStageFixture {
	t.Helper()
	sshSandboxGuard(t)
	// StrictModes walks AuthorizedKeysFile's ancestor chain. /tmp is
	// intentionally world-writable, so use this namespace's protected /opt
	// for generated sshd credentials, not a user home or a host path.
	dir, err := os.MkdirTemp("/opt", "sshd-fixture-")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		current, err := os.Lstat(dir)
		if err != nil || !os.SameFile(identity, current) || current.Mode()&os.ModeSymlink != 0 {
			t.Error("generated credential directory identity changed; retained")
			return
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Error("removing owned generated fixture credentials")
		}
	})
	clientKey, client := sshStageKey(t)
	hostKey, host := sshStageKey(t)
	for name, body := range map[string][]byte{"host-key": hostKey, "authorized_keys": ssh.MarshalAuthorizedKey(client.PublicKey())} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatal("writing generated fixture key")
		}
	}
	f := &sshStageFixture{address: "127.0.0.1:22222", clientKey: clientKey, hostPin: ssh.FingerprintSHA256(host.PublicKey()), clientPin: ssh.FingerprintSHA256(client.PublicKey()), done: make(chan error, 1)}
	config := "Port 22222\nListenAddress 127.0.0.1\nHostKey " + filepath.Join(dir, "host-key") + "\nAuthorizedKeysFile " + filepath.Join(dir, "authorized_keys") + "\n" +
		"PidFile none\nUsePAM no\nStrictModes yes\nPasswordAuthentication no\nKbdInteractiveAuthentication no\nPermitRootLogin prohibit-password\nPubkeyAuthentication yes\nAuthenticationMethods publickey\nAllowUsers root\nAllowTcpForwarding no\nAllowAgentForwarding no\nPermitTunnel no\nX11Forwarding no\nPermitTTY no\nUseDNS no\nPrintMotd no\nPrintLastLog no\nLogLevel ERROR\nMaxSessions 1\nMaxStartups 4\nLoginGraceTime 10\nSetEnv DB_ENGINE=invalid PRODUCTION=not-a-boolean TMPDIR=/tmp\n"
	if forced != "" {
		if forced != "/fixtures/post-stage-nonzero" && forced != "/fixtures/post-stage-lost-receipt" {
			t.Fatal("invalid fixture-only command fault")
		}
		config += "ForceCommand " + forced + "\n"
	}
	if configure != nil {
		config = configure(f, dir, config)
	}
	configPath := filepath.Join(dir, "sshd_config")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	log, err := os.OpenFile(filepath.Join(dir, "sshd-log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.cmd = exec.Command(sshStageSSHD, "-D", "-e", "-f", configPath)
	f.cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "HOME=/root", "TMPDIR=/tmp"}
	f.cmd.Stdout, f.cmd.Stderr = log, log
	if err := f.cmd.Start(); err != nil {
		_ = log.Close()
		t.Fatal("starting actual OpenSSH daemon")
	}
	go func() { err := f.cmd.Wait(); _ = log.Close(); f.done <- err }()
	t.Cleanup(func() { f.stop(t) })
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, dialErr := net.DialTimeout("tcp", f.address, 100*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			return f
		}
		select {
		case _ = <-f.done:
			// Put completion back for the exactly-once cleanup join.
			f.done <- errors.New("sshd exited before readiness")
			t.Fatal("actual OpenSSH daemon exited before readiness")
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("actual OpenSSH daemon did not become ready")
	return nil
}

func (f *sshStageFixture) stop(t *testing.T) {
	t.Helper()
	f.stopOnce.Do(func() {
		sshSandboxGuard(t)
		_ = f.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case _ = <-f.done:
		case <-time.After(3 * time.Second):
			_ = f.cmd.Process.Kill()
			select {
			case _ = <-f.done:
			case <-time.After(3 * time.Second):
				t.Error("fixture daemon did not join")
				return
			}
		}
		// This test is PID 1 of the runner-inspected private namespace. Reap
		// only its remaining namespace children after the daemon Cmd.Wait;
		// never call this on a host or claim general SSH remote termination.
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			entries, err := os.ReadDir("/proc")
			if err != nil {
				t.Error("cannot inspect owned namespace children")
				return
			}
			remaining := 0
			for _, entry := range entries {
				pid, err := strconv.Atoi(entry.Name())
				if err == nil && pid > 1 {
					remaining++
					if err := unix.Kill(pid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
						t.Error("cannot stop owned namespace child")
					}
				}
			}
			for {
				pid, err := unix.Wait4(-1, nil, unix.WNOHANG, nil)
				if pid <= 0 || err != nil {
					break
				}
			}
			if remaining == 0 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("owned namespace children remain after fixture join")
	})
}

func (f *sshStageFixture) sender(t *testing.T) *provisioning.SSHBinaryStageSender {
	t.Helper()
	sender, err := provisioning.NewSSHBinaryStageSender(t.Context(), f.clientKey, f.hostPin, f.clientPin, probe.EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}})
	if err != nil {
		t.Fatal(err)
	}
	return sender
}

func (f *sshStageFixture) intent(t *testing.T, pin domain.ProbeBinaryPin) domain.ProbeBinaryStageIntent {
	t.Helper()
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	id[6] = id[6]&15 | 64
	id[8] = id[8]&63 | 128
	x := hex.EncodeToString(id[:])
	op := x[:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:]
	intent := domain.ProbeBinaryStageIntent{Identity: domain.ProbeInstallIdentity{OperationID: op, Host: domain.ProbeSSHHost{Address: f.address, User: "root"}, HostKeyFingerprint: f.hostPin, ClientKeyFingerprint: f.clientPin, RequestedBy: 7, LayoutVersion: 1, Mode: domain.ProbeInstallSystemd, Binary: pin}, OwnerID: "22222222-2222-4222-8222-222222222222", AttemptID: "33333333-3333-4333-8333-333333333333"}
	if !intent.Valid() {
		t.Fatal("invalid generated test intent")
	}
	return intent
}

type sshStageTrackedBinary struct {
	ports.PreparedProbeBinary
	opens   atomic.Int32
	closes  atomic.Int32
	readers atomic.Int32
	wrap    func(io.ReadCloser) io.ReadCloser
}

func (b *sshStageTrackedBinary) Open(ctx context.Context) (io.ReadCloser, error) {
	b.opens.Add(1)
	r, err := b.PreparedProbeBinary.Open(ctx)
	if err != nil {
		return r, err
	}
	if b.wrap != nil {
		r = b.wrap(r)
	}
	return &sshStageTrackedReader{ReadCloser: r, count: &b.readers}, nil
}
func (b *sshStageTrackedBinary) Close(ctx context.Context) error {
	b.closes.Add(1)
	return b.PreparedProbeBinary.Close(ctx)
}
func (b *sshStageTrackedBinary) assertBorrowed(t *testing.T, calls int32) {
	t.Helper()
	if b.opens.Load() != calls || b.readers.Load() != calls || b.closes.Load() != 0 {
		t.Fatalf("unexpected source lifetime: opens=%d reader closes=%d snapshot closes=%d", b.opens.Load(), b.readers.Load(), b.closes.Load())
	}
}

type sshStageTrackedReader struct {
	io.ReadCloser
	count *atomic.Int32
}

func (r *sshStageTrackedReader) Close() error { r.count.Add(1); return r.ReadCloser.Close() }

func (f *sshStageFixture) prepared(t *testing.T, arch string) *sshStageTrackedBinary {
	t.Helper()
	data, err := os.ReadFile("/fixtures/" + arch)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	pin := domain.ProbeBinaryPin{Version: "1.2.3", Architecture: arch, SHA256: hex.EncodeToString(hash[:]), Size: int64(len(data))}
	prepared, err := provisioning.NewProbeBinaryPreparer(t.TempDir()).PrepareBinary(t.Context(), pin, io.NopCloser(bytes.NewReader(data)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := prepared.Close(context.WithoutCancel(t.Context())); err != nil {
			t.Error("disposing task-owned source snapshot")
		}
	})
	return &sshStageTrackedBinary{PreparedProbeBinary: prepared}
}

func assertSSHStageGraph(t *testing.T, intent domain.ProbeBinaryStageIntent) {
	t.Helper()
	entries, err := os.ReadDir(intent.Directory())
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{domain.ProbeBinaryStageMarkerName, intent.Identity.Binary.ReleaseName()}
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("actual remote graph differs: %v", names)
	}
	for _, item := range []struct {
		name string
		mode os.FileMode
	}{{"", 0o700}, {domain.ProbeBinaryStageMarkerName, 0o444}, {intent.Identity.Binary.ReleaseName(), 0o555}} {
		st, err := os.Lstat(filepath.Join(intent.Directory(), item.name))
		if err != nil || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm() != item.mode || st.Sys().(*syscall.Stat_t).Uid != 0 {
			t.Fatalf("actual staged identity/mode differs for %q", item.name)
		}
	}
	marker, err := os.ReadFile(filepath.Join(intent.Directory(), domain.ProbeBinaryStageMarkerName))
	if err != nil || string(marker) != intent.Marker() {
		t.Fatal("actual marker is not the exact canonical marker")
	}
	body, err := os.ReadFile(filepath.Join(intent.Directory(), intent.Identity.Binary.ReleaseName()))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if int64(len(body)) != intent.Identity.Binary.Size || hex.EncodeToString(sum[:]) != intent.Identity.Binary.SHA256 {
		t.Fatal("actual remote bytes do not match the supplied pin")
	}
}

type sshStageEOFHold struct {
	mu     sync.Mutex
	reader io.ReadCloser
	atEOF  chan struct{}
	closed chan struct{}
	eof    sync.Once
	once   sync.Once
	err    error
}

func (r *sshStageEOFHold) attach(reader io.ReadCloser) {
	r.mu.Lock()
	r.reader = reader
	select {
	case <-r.closed:
		_ = reader.Close()
	default:
	}
	r.mu.Unlock()
}

func (r *sshStageEOFHold) Read(p []byte) (int, error) {
	r.mu.Lock()
	reader := r.reader
	r.mu.Unlock()
	n, err := reader.Read(p)
	if errors.Is(err, io.EOF) {
		r.eof.Do(func() { close(r.atEOF) })
		<-r.closed
		return n, context.Canceled
	}
	return n, err
}
func (r *sshStageEOFHold) Close() error {
	r.once.Do(func() {
		r.mu.Lock()
		close(r.closed)
		reader := r.reader
		r.mu.Unlock()
		if reader != nil {
			r.err = reader.Close()
		}
	})
	return r.err
}
