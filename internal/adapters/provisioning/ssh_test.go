package provisioning

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

const goodDiscovery = "phoenix-preflight-v1\nkernel=Linux\nmachine=x86_64\ndocker=0\nsystemd=1\nprivilege=root\n"

func deploymentKey(t *testing.T) ([]byte, ssh.Signer) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	data, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: data}), signer
}

type sshFixture struct {
	address       string
	hostKey       ssh.Signer
	clientKey     []byte
	authCalls     atomic.Int32
	commandCalls  atomic.Int32
	entered       chan struct{}
	closed        chan struct{}
	stdout        string
	stderr        string
	exit          uint32
	rejectAuth    bool
	stallGreeting bool
	stallChannel  bool
	stallExec     bool
	stallCommand  bool
}

// Runs a real loopback SSH protocol server; no installed SSH daemon, host key,
// agent, home directory, external endpoint or privileged service is consulted.
func serveSSH(t *testing.T, configure func(*sshFixture)) *sshFixture {
	t.Helper()
	key, clientSigner := deploymentKey(t)
	_, hostSigner := deploymentKey(t)
	f := &sshFixture{hostKey: hostSigner, clientKey: key, stdout: goodDiscovery, entered: make(chan struct{}), closed: make(chan struct{})}
	if configure != nil {
		configure(f)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.address = listener.Addr().String()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
		defer stop()
		if f.stallGreeting {
			close(f.entered)
			_, _ = io.Copy(io.Discard, conn)
			close(f.closed)
			return
		}
		config := &ssh.ServerConfig{PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			f.authCalls.Add(1)
			if f.rejectAuth || meta.User() != "deploy" || !bytes.Equal(key.Marshal(), clientSigner.PublicKey().Marshal()) {
				return nil, errors.New("authentication rejected; private server diagnostic")
			}
			return nil, nil
		}}
		config.AddHostKey(hostSigner)
		server, channels, requests, err := ssh.NewServerConn(conn, config)
		if err != nil {
			close(f.closed)
			return
		}
		defer func() { _ = server.Close() }()
		go ssh.DiscardRequests(requests)
		peerClosed := make(chan struct{})
		go func() { _ = server.Wait(); close(peerClosed) }()
		defer func() { _ = server.Close(); <-peerClosed; close(f.closed) }()
		for request := range channels {
			if request.ChannelType() != "session" {
				_ = request.Reject(ssh.Prohibited, "session only")
				continue
			}
			if f.stallChannel {
				close(f.entered)
				<-peerClosed
				return
			}
			channel, reqs, err := request.Accept()
			if err != nil {
				return
			}
			defer func() { _ = channel.Close() }()
			for req := range reqs {
				var command struct{ Command string }
				if req.Type != "exec" || ssh.Unmarshal(req.Payload, &command) != nil || command.Command != discoveryCommand {
					_ = req.Reply(false, nil)
					continue
				}
				f.commandCalls.Add(1)
				if f.stallExec {
					close(f.entered)
					<-peerClosed
					return
				}
				_ = req.Reply(true, nil)
				close(f.entered)
				if f.stallCommand {
					select {
					case <-peerClosed:
					case <-ctx.Done():
					}
					return
				}
				_, _ = io.WriteString(channel, f.stdout)
				_, _ = io.WriteString(channel.Stderr(), f.stderr)
				_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{f.exit}))
				return
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("SSH fixture failed to stop")
		}
	})
	return f
}

func (f *sshFixture) inspector(t *testing.T, trust SSHHostTrust) *SSHHostInspector {
	t.Helper()
	inspector, err := NewSSHHostInspector(t.Context(), f.clientKey, trust, probe.EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}})
	if err != nil {
		t.Fatal(err)
	}
	return inspector
}

func TestSSHPreflightPinnedHost(t *testing.T) {
	f := serveSSH(t, nil)
	inspector := f.inspector(t, SSHHostTrust{Fingerprint: ssh.FingerprintSHA256(f.hostKey.PublicKey())})
	facts, err := inspector.InspectHost(t.Context(), domain.ProbeSSHHost{Address: f.address, User: "deploy"})
	if err != nil || facts != (domain.ProbeHostFacts{Kernel: "Linux", Machine: "x86_64", Systemd: true, Privilege: "root"}) {
		t.Fatalf("facts=%+v error=%v", facts, err)
	}
	if f.authCalls.Load() == 0 || f.commandCalls.Load() != 1 {
		t.Fatal("did not authenticate and run exactly the fixed discovery command")
	}
}

func TestSSHPreflightTrustBeforeAuthentication(t *testing.T) {
	for _, mode := range []string{"wrong_pin", "unknown_host", "wrong_known_key", "revoked_key"} {
		t.Run(mode, func(t *testing.T) {
			f := serveSSH(t, nil)
			_, other := deploymentKey(t)
			trust := SSHHostTrust{Fingerprint: ssh.FingerprintSHA256(other.PublicKey())}
			if mode != "wrong_pin" {
				key, host := other.PublicKey(), f.address
				if mode == "unknown_host" {
					host = "unrelated.example:22"
				}
				line := knownhosts.Line([]string{knownhosts.Normalize(host)}, key)
				if mode == "revoked_key" {
					line = "@revoked " + knownhosts.Line([]string{knownhosts.Normalize(host)}, f.hostKey.PublicKey())
				}
				path := filepath.Join(t.TempDir(), "known_hosts")
				if err := os.WriteFile(path, []byte(line+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				trust = SSHHostTrust{KnownHostsFile: path}
			}
			facts, err := f.inspector(t, trust).InspectHost(t.Context(), domain.ProbeSSHHost{Address: f.address, User: "deploy"})
			if !errors.Is(err, domain.ErrProbeSSHTrust) || facts != (domain.ProbeHostFacts{}) || f.authCalls.Load() != 0 || f.commandCalls.Load() != 0 {
				t.Fatalf("host trust did not fail before auth: error=%v auth=%d command=%d", err, f.authCalls.Load(), f.commandCalls.Load())
			}
		})
	}
}

func TestSSHPreflightKnownHostUsesOriginalDNSNameAndPort(t *testing.T) {
	f := serveSSH(t, nil)
	_, port, _ := net.SplitHostPort(f.address)
	address := net.JoinHostPort("verified.example", port)
	path := filepath.Join(t.TempDir(), "known_hosts")
	line := knownhosts.Line([]string{knownhosts.HashHostname(knownhosts.Normalize(address))}, f.hostKey.PublicKey())
	if err := os.WriteFile(path, []byte(line+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	i := f.inspector(t, SSHHostTrust{KnownHostsFile: path})
	lookups := 0
	i.lookup = func(_ context.Context, network, host string) ([]netip.Addr, error) {
		lookups++
		if network != "ip" || host != "verified.example" {
			t.Error("wrong lookup")
		}
		return []netip.Addr{netip.MustParseAddr("169.254.169.254"), netip.MustParseAddr("127.0.0.1")}, nil
	}
	originalDial := i.dial
	i.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != f.address {
			t.Fatalf("dial must use only the authorized numeric IP: %s", address)
		}
		return originalDial(ctx, network, address)
	}
	if _, err := i.InspectHost(t.Context(), domain.ProbeSSHHost{Address: address, User: "deploy"}); err != nil || lookups != 1 {
		t.Fatalf("original host trust/resolution: %v, lookups=%d", err, lookups)
	}
}

func TestSSHPreflightDestinationPolicy(t *testing.T) {
	key, signer := deploymentKey(t)
	for _, address := range []string{
		"169.254.169.254:22", "100.100.100.200:22", "[fd00:ec2::254]:22", "[fe80::1]:22", "[::ffff:169.254.169.254]:22", "0.0.0.0:22", "224.0.0.1:22",
	} {
		t.Run(address, func(t *testing.T) {
			i, err := NewSSHHostInspector(t.Context(), key, SSHHostTrust{Fingerprint: ssh.FingerprintSHA256(signer.PublicKey())}, probe.EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}})
			if err != nil {
				t.Fatal(err)
			}
			i.dial = func(context.Context, string, string) (net.Conn, error) {
				t.Fatal("forbidden destination was dialed")
				return nil, domain.ErrProbeSSHConnect
			}
			if _, err := i.InspectHost(t.Context(), domain.ProbeSSHHost{Address: address, User: "deploy"}); !errors.Is(err, domain.ErrProbeSSHDestination) {
				t.Fatal(err)
			}
		})
	}
	for _, address := range []string{"127.0.0.1:22", "10.0.0.1:22", "[::1]:22", "[fd00::1]:22"} {
		i, err := NewSSHHostInspector(t.Context(), key, SSHHostTrust{Fingerprint: ssh.FingerprintSHA256(signer.PublicKey())}, probe.EndpointPolicy{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := i.InspectHost(t.Context(), domain.ProbeSSHHost{Address: address, User: "deploy"}); !errors.Is(err, domain.ErrProbeSSHDestination) {
			t.Fatalf("default policy allowed %s: %v", address, err)
		}
	}
}

func TestSSHPreflightFailuresAreBoundedAndRedacted(t *testing.T) {
	for _, mode := range []string{"authentication", "exit_status", "malformed", "stdout_limit", "stderr_limit"} {
		t.Run(mode, func(t *testing.T) {
			f := serveSSH(t, func(f *sshFixture) {
				switch mode {
				case "authentication":
					f.rejectAuth = true
				case "exit_status":
					f.exit, f.stderr = 42, "do not expose remote diagnostics"
				case "malformed":
					f.stdout = "sensitive remote startup banner\n" + goodDiscovery
				case "stdout_limit":
					f.stdout = strings.Repeat("x", maxDiscoverySize+1)
				case "stderr_limit":
					f.stderr = strings.Repeat("x", maxDiscoverySize+1)
				}
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			facts, err := f.inspector(t, SSHHostTrust{Fingerprint: ssh.FingerprintSHA256(f.hostKey.PublicKey())}).InspectHost(ctx, domain.ProbeSSHHost{Address: f.address, User: "deploy"})
			want := domain.ErrProbeSSHDiscovery
			if mode == "authentication" {
				want = domain.ErrProbeSSHConnect
			}
			if err != want || facts != (domain.ProbeHostFacts{}) {
				t.Fatalf("error was not exact sanitized diagnostic: %v, facts=%+v", err, facts)
			}
		})
	}
}

func TestSSHPreflightCancellationClosesTransport(t *testing.T) {
	for _, phase := range []string{"handshake", "channel", "exec_ack", "command"} {
		t.Run(phase, func(t *testing.T) {
			f := serveSSH(t, func(f *sshFixture) {
				f.stallGreeting, f.stallChannel = phase == "handshake", phase == "channel"
				f.stallExec, f.stallCommand = phase == "exec_ack", phase == "command"
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan error, 1)
			inspector := f.inspector(t, SSHHostTrust{Fingerprint: ssh.FingerprintSHA256(f.hostKey.PublicKey())})
			go func() {
				_, err := inspector.InspectHost(ctx, domain.ProbeSSHHost{Address: f.address, User: "deploy"})
				result <- err
			}()
			select {
			case <-f.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("preflight did not reach stalled phase")
			}
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cancellation did not stop preflight")
			}
			select {
			case <-f.closed:
			case <-time.After(5 * time.Second):
				t.Fatal("cancellation returned without closing remote transport")
			}
		})
	}
}

func TestSSHPreflightTrustIsExplicit(t *testing.T) {
	key, signer := deploymentKey(t)
	pin := ssh.FingerprintSHA256(signer.PublicKey())
	for _, trust := range []SSHHostTrust{
		{}, {Fingerprint: pin, KnownHostsFile: "unused"}, {Fingerprint: "SHA256:not-a-pin"}, {Fingerprint: strings.TrimPrefix(pin, "SHA256:")},
		{Fingerprint: pin + "="}, {KnownHostsFile: filepath.Join(t.TempDir(), "missing")},
	} {
		if _, err := NewSSHHostInspector(t.Context(), key, trust, probe.EndpointPolicy{}); err != domain.ErrProbeSSHInput {
			t.Fatalf("ambiguous/malformed trust accepted: %v", err)
		}
	}
	if _, err := NewSSHHostInspector(t.Context(), []byte("invalid private input"), SSHHostTrust{Fingerprint: pin}, probe.EndpointPolicy{}); err != domain.ErrProbeSSHInput {
		t.Fatal("invalid private key accepted")
	}
}

func TestSSHPreflightDiscoveryParser(t *testing.T) {
	for _, text := range []string{
		goodDiscovery + "extra\n", strings.Replace(goodDiscovery, "docker=0", "docker=true", 1),
		strings.Replace(goodDiscovery, "systemd=1", "docker=1", 1), strings.Replace(goodDiscovery, "privilege=root", "privilege=all", 1),
		strings.Replace(goodDiscovery, "machine=x86_64", "machine="+strings.Repeat("a", 65), 1), strings.TrimSuffix(goodDiscovery, "\n"),
		strings.Replace(goodDiscovery, "kernel=Linux", "kernel=Linux\x00", 1),
	} {
		if facts, err := parseDiscovery(text); err != domain.ErrProbeSSHDiscovery || facts != (domain.ProbeHostFacts{}) {
			t.Fatal("malformed discovery accepted")
		}
	}
}

func TestSSHPreflightFixedScript(t *testing.T) {
	// Execute the actual script through sh with controlled tool fixtures. This
	// catches quoting/output mismatches a mock SSH server alone would hide.
	for _, privilege := range []string{"root", "sudo", "none"} {
		t.Run(privilege, func(t *testing.T) {
			dir := t.TempDir()
			id := "0"
			if privilege != "root" {
				id = "1000"
			}
			sudoExit := "0"
			if privilege == "none" {
				sudoExit = "1"
			}
			for name, script := range map[string]string{
				"uname": "#!/bin/sh\ncase \"$1\" in -s) echo Linux;; -m) echo x86_64;; *) exit 1;; esac\n",
				"id":    "#!/bin/sh\necho " + id + "\n", "sudo": "#!/bin/sh\ntest \"$1\" = -n && test \"$2\" = -- && test \"$3\" = /bin/true || exit 99\nexit " + sudoExit + "\n",
				"docker": "#!/bin/sh\nexit 99\n", // must be discovered, never executed
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", discoveryCommand)
			cmd.Env = []string{"PATH=" + dir}
			out, err := cmd.Output()
			if err != nil {
				t.Fatal("fixed script failed")
			}
			facts, err := parseDiscovery(string(out))
			if err != nil || facts.Privilege != privilege || !facts.DockerCLI || facts.Systemd || facts.Kernel != "Linux" || facts.Machine != "x86_64" {
				t.Fatalf("fixed script facts=%+v error=%v", facts, err)
			}
		})
	}
}
