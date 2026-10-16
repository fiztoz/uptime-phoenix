package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func preflightCommandKey(t *testing.T) (ssh.Signer, []byte) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	data, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return signer, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: data})
}

func TestProbeAdminSSHPreflightAcrossProcesses(t *testing.T) {
	for _, scenario := range []string{"compatible", "wrong_host_key", "missing_sudo", "wrong_architecture", "failed_command"} {
		t.Run(scenario, func(t *testing.T) {
			hostSigner, _ := preflightCommandKey(t)
			clientSigner, key := preflightCommandKey(t)
			dir := t.TempDir()
			identity, policy := filepath.Join(dir, "deploy.key"), filepath.Join(dir, "endpoints.json")
			if err := os.WriteFile(identity, key, 0600); err != nil {
				t.Fatal(err)
			}
			clear(key)
			if err := os.WriteFile(policy, []byte(`{"allowed_cidrs":["127.0.0.1/32"]}`), 0600); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = listener.Close() }()
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			var authenticated atomic.Bool
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
				config := &ssh.ServerConfig{PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
					authenticated.Store(true)
					if meta.User() != "deploy" || !bytes.Equal(key.Marshal(), clientSigner.PublicKey().Marshal()) {
						return nil, errors.New("invalid deployment key")
					}
					return nil, nil
				}}
				config.AddHostKey(hostSigner)
				server, channels, requests, err := ssh.NewServerConn(conn, config)
				if err != nil {
					return
				}
				defer func() { _ = server.Close() }()
				go ssh.DiscardRequests(requests)
				for c := range channels {
					if c.ChannelType() != "session" {
						_ = c.Reject(ssh.Prohibited, "session only")
						continue
					}
					channel, requests, err := c.Accept()
					if err != nil {
						return
					}
					defer func() { _ = channel.Close() }()
					for req := range requests {
						if req.Type != "exec" {
							_ = req.Reply(false, nil)
							continue
						}
						_ = req.Reply(true, nil)
						machine, privilege, exit := "aarch64", "sudo", uint32(0)
						if scenario == "wrong_architecture" {
							machine = "mips"
						}
						if scenario == "missing_sudo" {
							privilege = "none"
						}
						if scenario == "failed_command" {
							exit = 1
						}
						_, _ = io.WriteString(channel, "phoenix-preflight-v1\nkernel=Linux\nmachine="+machine+"\ndocker=0\nsystemd=1\nprivilege="+privilege+"\n")
						_, _ = io.WriteString(channel.Stderr(), "sensitive-remote-canary\n")
						_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{exit}))
						return
					}
				}
			}()
			pin := ssh.FingerprintSHA256(hostSigner.PublicKey())
			if scenario == "wrong_host_key" {
				pin = ssh.FingerprintSHA256(clientSigner.PublicKey())
			}
			out, stderr, err := runPreflightCommand(t, ctx, "--address", listener.Addr().String(), "--user", "deploy", "--identity-file", identity, "--host-key-sha256", pin, "--endpoint-policy", policy)
			if strings.Contains(string(out)+string(stderr), "sensitive-remote-canary") || bytes.Contains(out, []byte("PRIVATE KEY")) {
				t.Fatal("remote output or key leaked through command")
			}
			if scenario == "compatible" {
				var view map[string]json.RawMessage
				if err != nil || len(stderr) != 0 || json.Unmarshal(out, &view) != nil || len(view) != 6 || string(view["state"]) != `"preflight_passed"` || string(view["architecture"]) != `"arm64"` {
					t.Fatal("real CLI did not emit exactly the nonsecret preflight view")
				}
			} else if err == nil || len(out) != 0 {
				t.Fatal("failed CLI emitted success or exited zero")
			}
			if scenario == "wrong_host_key" && authenticated.Load() {
				t.Fatal("host-key failure did not precede authentication")
			}
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("command transport did not stop")
			}
		})
	}
}

func runPreflightCommand(t *testing.T, ctx context.Context, args ...string) ([]byte, []byte, error) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	argv := append([]string{"-test.run=^TestProbeAdminHelperProcess$", "--", "inspect-host"}, args...)
	cmd := exec.CommandContext(ctx, executable, argv...)
	// Deliberately invalid hub config proves dispatch occurs before LoadConfig,
	// migrations, protection-key access or any other hub startup dependency.
	cmd.Env = []string{"PHOENIX_PROBE_ADMIN_TEST_PROCESS=1", "DB_ENGINE=invalid", "PRODUCTION=not-a-boolean"}
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err = cmd.Run()
	return out.Bytes(), stderr.Bytes(), err
}

func TestProbeAdminSSHPreflightHelpAndInvalidInput(t *testing.T) {
	out, stderr, err := runPreflightCommand(t, t.Context(), "--help")
	if err != nil || len(stderr) != 0 || !bytes.Contains(out, []byte("NOT an install")) {
		t.Fatal("inspect-host help needs no hub config")
	}
	for _, args := range [][]string{
		{}, {"--unknown-sensitive-canary"}, {"--address", "sensitive-argument-canary", "--user", "deploy"},
		{"--address", "edge.example:22", "--user", "deploy", "--identity-file", "sensitive-path-canary", "--host-key-sha256", "bad", "--known-hosts", "bad"},
	} {
		out, stderr, err := runPreflightCommand(t, t.Context(), args...)
		if err == nil || len(out) != 0 || bytes.Contains(stderr, []byte("canary")) {
			t.Fatal("invalid invocation succeeded or echoed raw arguments")
		}
	}
}
