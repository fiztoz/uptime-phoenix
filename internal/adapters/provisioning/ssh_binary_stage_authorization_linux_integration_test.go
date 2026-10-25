//go:build linux

package provisioning_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// This literal is independent of the sender: a changed caller command must not
// silently change the key policy that this acceptance proves.
const sshAuthorizationCommand = "/usr/local/bin/phoenix-probe-admin stage-binary-host"
const sshAuthorizationKeyOptions = "restrict,command=\"" + sshAuthorizationCommand + "\" "

// TestSSHBinaryStageAuthorization runs only as PID1 in the approved private
// namespace. It proves native OpenSSH key policy, not DB authority or fencing.
func TestSSHBinaryStageAuthorization(t *testing.T) {
	if os.Getenv("PHOENIX_M8_SSH_SANDBOX") != "1" || os.Getenv("PHOENIX_M8_SSH_AUTHORIZATION_SANDBOX") != "1" {
		t.Skip("requires dedicated isolated restricted-key OpenSSH sandbox")
	}
	sshSandboxGuard(t)
	t.Run("Fixture", func(t *testing.T) {
		if provisioning.SSHProbeBinaryStageCommand != sshAuthorizationCommand {
			t.Fatal("sender command changed from the independently fixed key policy")
		}
		out, err := exec.CommandContext(t.Context(), sshStageSSHD, "-V").CombinedOutput()
		if err != nil || !strings.HasPrefix(string(out), "OpenSSH_") {
			t.Fatal("actual OpenSSH version unavailable")
		}
		t.Log(strings.TrimSpace(string(out)))
		for _, p := range []string{"/", "/opt", "/usr/local/bin/phoenix-probe-admin"} {
			st, err := os.Lstat(p)
			if err != nil || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0o022 != 0 || st.Sys().(*syscall.Stat_t).Uid != 0 {
				t.Fatal("untrusted fixed fixture path")
			}
		}
	})
	if t.Failed() {
		return
	}
	t.Run("Sender", func(t *testing.T) {
		for _, arch := range []string{"amd64", "arm64"} {
			t.Run(arch, func(t *testing.T) {
				f := newSSHAuthorizationFixture(t, false)
				b := f.prepared(t, arch)
				intent := f.intent(t, b.Pin())
				ack, err := f.sender(t).Send(t.Context(), intent, b)
				if err != nil || ack != (provisioning.SSHBinaryStageAck{Intent: intent}) {
					t.Fatal("restricted key did not reach unchanged sender/helper")
				}
				assertSSHStageGraph(t, intent)
				b.assertBorrowed(t, 1)
			})
		}
	})
	t.Run("ForcedExec", func(t *testing.T) {
		f := newSSHAuthorizationFixture(t, false)
		canary := filepath.Join(t.TempDir(), "exec-canary")
		command := sshAuthorizationCanaryCommand(canary)
		control := sshAuthorizationClient(t, f, f.controlKey)
		if err := controlRun(t, control, command); err != nil {
			t.Fatal("unrestricted same-server command control failed")
		}
		assertSSHAuthorizationCanary(t, canary)
		if err := os.Remove(canary); err != nil {
			t.Fatal("removing owned namespace control canary")
		}
		b := f.prepared(t, "arm64")
		intent := f.intent(t, b.Pin())
		body := sshAuthorizationFrame(t, intent, b)
		client := sshAuthorizationClient(t, f, f.clientKey)
		out, diagnostic, err := sshAuthorizationRequest(t, client, "exec", command, body)
		if err != nil || len(diagnostic) != 0 {
			t.Fatal("forced exec did not finish as the fixed helper")
		}
		if _, err := os.Lstat(canary); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("restricted key executed the caller-selected canary command")
		}
		assertSSHAuthorizationReceipt(t, out, intent)
		assertSSHStageGraph(t, intent)
		b.assertBorrowed(t, 1)
	})
	t.Run("ForcedShell", func(t *testing.T) {
		f := newSSHAuthorizationFixture(t, false)
		b := f.prepared(t, "arm64")
		intent := f.intent(t, b.Pin())
		body := sshAuthorizationFrame(t, intent, b)
		client := sshAuthorizationClient(t, f, f.clientKey)
		out, diagnostic, err := sshAuthorizationRequest(t, client, "shell", "", body)
		if err != nil || len(diagnostic) != 0 {
			t.Fatal("shell request did not finish as the fixed helper")
		}
		assertSSHAuthorizationReceipt(t, out, intent)
		assertSSHStageGraph(t, intent)
		b.assertBorrowed(t, 1)
	})
	t.Run("RestrictedSubsystem", func(t *testing.T) {
		f := newSSHAuthorizationFixture(t, false)
		client := sshAuthorizationClient(t, f, f.clientKey)
		status, out, diagnostic, err := sshAuthorizationSubsystem(t, client, false, sshAuthorizationSFTPInit)
		// OpenSSH intentionally ignores subsystem stderr, including when a key
		// forces an external command. No SFTP VERSION may be returned.
		if err != nil || status != 1 || len(out) != 0 || len(diagnostic) != 0 {
			t.Fatal("restricted subsystem did not refuse the SFTP frame")
		}
		_ = client.Close()
		_ = client.Wait()
		// Prove the replacement is this helper, not an arbitrary exit-1 command.
		b := f.prepared(t, "amd64")
		intent := f.intent(t, b.Pin())
		client = sshAuthorizationClient(t, f, f.clientKey)
		status, out, diagnostic, err = sshAuthorizationSubsystem(t, client, false, sshAuthorizationFrame(t, intent, b))
		if err != nil || status != 0 || len(diagnostic) != 0 {
			t.Fatal("restricted subsystem did not execute the fixed stage helper")
		}
		assertSSHAuthorizationReceipt(t, out, intent)
		assertSSHStageGraph(t, intent)
		b.assertBorrowed(t, 1)
	})
	t.Run("ControlCommand", func(t *testing.T) {
		f := newSSHAuthorizationFixture(t, false)
		client := sshAuthorizationClient(t, f, f.controlKey)
		canary := filepath.Join(t.TempDir(), "exec-canary")
		if err := controlRun(t, client, sshAuthorizationCanaryCommand(canary)); err != nil {
			t.Fatal("unrestricted command control failed")
		}
		assertSSHAuthorizationCanary(t, canary)
	})
	t.Run("ControlSubsystem", func(t *testing.T) {
		f := newSSHAuthorizationFixture(t, false)
		client := sshAuthorizationClient(t, f, f.controlKey)
		status, trailer, diagnostic, err := sshAuthorizationSubsystem(t, client, true, sshAuthorizationSFTPInit)
		if err != nil || status != 0 || len(trailer) != 0 || len(diagnostic) != 0 {
			t.Fatal("actual SFTP v3 exchange/normal completion was not established")
		}
	})
	t.Run("Forwarding", func(t *testing.T) {
		f := newSSHAuthorizationFixture(t, false)
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal("namespace echo listener")
		}
		done := make(chan error, 1)
		go func() {
			conn, err := listener.Accept()
			if err != nil {
				done <- err
				return
			}
			defer func() { _ = conn.Close() }()
			if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				done <- errors.New("namespace echo deadline unavailable")
				return
			}
			var body [4]byte
			if _, err := io.ReadFull(conn, body[:]); err != nil {
				done <- err
				return
			}
			_, err = conn.Write(body[:])
			done <- err
		}()
		joined := false
		t.Cleanup(func() {
			_ = listener.Close()
			if !joined {
				select {
				case <-done:
				case <-time.After(6 * time.Second):
					t.Error("namespace echo handler did not join")
				}
			}
		})
		control := sshAuthorizationClient(t, f, f.controlKey)
		forward, err := control.DialContext(t.Context(), "tcp", listener.Addr().String())
		if err != nil {
			t.Fatal("same-server unrestricted forwarding control failed")
		}
		// SSH forwarded channels do not implement deadlines. The enclosing
		// client transport has a deadline and the real echo socket has its own.
		if n, err := forward.Write([]byte("echo")); err != nil || n != 4 {
			_ = forward.Close()
			t.Fatal("control forward write")
		}
		var got [4]byte
		_, readErr := io.ReadFull(forward, got[:])
		_ = forward.Close()
		if readErr != nil || string(got[:]) != "echo" {
			t.Fatal("real loopback echo not proven")
		}
		select {
		case err := <-done:
			joined = true
			if err != nil {
				t.Fatal("namespace echo control failed")
			}
		case <-time.After(6 * time.Second):
			t.Fatal("namespace echo control did not join")
		}
		restricted := sshAuthorizationClient(t, f, f.clientKey)
		unexpected, err := restricted.DialContext(t.Context(), "tcp", listener.Addr().String())
		if unexpected != nil {
			_ = unexpected.Close()
		}
		var refused *ssh.OpenChannelError
		if !errors.As(err, &refused) || refused.Reason != ssh.Prohibited {
			t.Fatal("restricted key did not explicitly prohibit forwarding under permitting global policy")
		}
	})
	t.Run("ForcedOnlyRootLogin", func(t *testing.T) {
		f := newSSHAuthorizationFixture(t, true)
		b := f.prepared(t, "arm64")
		intent := f.intent(t, b.Pin())
		ack, err := f.sender(t).Send(t.Context(), intent, b)
		if err != nil || ack != (provisioning.SSHBinaryStageAck{Intent: intent}) {
			t.Fatal("forced-only root policy refused the actual restricted sender")
		}
		assertSSHStageGraph(t, intent)
		b.assertBorrowed(t, 1)
		before := sshAuthorizationRootRefusals(t, f.logPath)
		unexpected, connectErr := sshAuthorizationConnect(t.Context(), f, f.controlKey)
		acceptedSession := false
		if unexpected != nil {
			// The unprivileged preauth child can report userauth success before
			// the privileged monitor rejects root. Do not confuse a Client value
			// with a monitor-authorized session (or require one error spelling).
			session, openErr := unexpected.NewSession()
			acceptedSession = openErr == nil
			if session != nil {
				_ = session.Close()
			}
			_ = unexpected.Close()
			_ = unexpected.Wait()
		} else if connectErr == nil {
			t.Fatal("unforced root control returned neither client nor error")
		}
		if acceptedSession {
			t.Fatal("root policy admitted an unforced session")
		}
		deadline := time.Now().Add(2 * time.Second)
		for sshAuthorizationRootRefusals(t, f.logPath) <= before {
			if time.Now().After(deadline) || t.Context().Err() != nil {
				t.Fatal("unforced root refusal lacks new privileged-monitor evidence")
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	t.Run("ReplayPreservesForeign", func(t *testing.T) {
		f := newSSHAuthorizationFixture(t, false)
		b := f.prepared(t, "arm64")
		intent := f.intent(t, b.Pin())
		sender := f.sender(t)
		if _, err := sender.Send(t.Context(), intent, b); err != nil {
			t.Fatal("restricted initial stage failed")
		}
		assertSSHStageGraph(t, intent)
		marker := filepath.Join(intent.Directory(), domain.ProbeBinaryStageMarkerName)
		foreign := []byte("foreign restricted-key fixture; never adopted\n")
		if err := os.Chmod(marker, 0o600); err != nil {
			t.Fatal(err)
		}
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
		ack, err := sender.Send(t.Context(), intent, b)
		if err != provisioning.ErrSSHBinaryStageUnknown || ack != (provisioning.SSHBinaryStageAck{}) {
			t.Fatal("restricted replay invented acknowledgment")
		}
		after, statErr := os.Lstat(marker)
		body, readErr := os.ReadFile(marker)
		if statErr != nil || readErr != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) || !bytes.Equal(body, foreign) {
			t.Fatal("restricted replay replaced or changed foreign marker")
		}
		b.assertBorrowed(t, 2)
	})
	if _, err := os.Lstat("/tmp/probe-executed"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("probe execution canary present or absence unproved")
	}
}

type sshAuthorizationFixture struct {
	*sshStageFixture
	controlKey []byte
	logPath    string
}

func newSSHAuthorizationFixture(t *testing.T, forcedOnly bool) *sshAuthorizationFixture {
	t.Helper()
	controlKey, control := sshStageKey(t)
	var logPath string
	f := newSSHStageConfiguredFixture(t, "", func(f *sshStageFixture, dir, config string) string {
		logPath = filepath.Join(dir, "sshd-log")
		config = strings.Replace(config, "LogLevel ERROR\n", "LogLevel VERBOSE\n", 1)
		keyPath := filepath.Join(dir, "authorized_keys")
		restricted, err := os.ReadFile(keyPath)
		if err != nil {
			t.Fatal("reading only the generated fixture public key")
		}
		if ssh.FingerprintSHA256(control.PublicKey()) == f.clientPin {
			t.Fatal("control and restricted fixture keys are identical")
		}
		body := append([]byte(sshAuthorizationKeyOptions+strings.TrimSpace(string(restricted))+"\n"), ssh.MarshalAuthorizedKey(control.PublicKey())...)
		first, _, options, rest, parseErr := ssh.ParseAuthorizedKey(body)
		if parseErr != nil || ssh.FingerprintSHA256(first) != f.clientPin || len(options) != 2 {
			t.Fatal("restricted generated key/policy parse mismatch")
		}
		second, _, options, rest, parseErr := ssh.ParseAuthorizedKey(rest)
		if parseErr != nil || ssh.FingerprintSHA256(second) != ssh.FingerprintSHA256(control.PublicKey()) || len(options) != 0 || len(bytes.TrimSpace(rest)) != 0 {
			t.Fatal("unrestricted generated control key parse mismatch")
		}
		if err := os.WriteFile(keyPath, body, 0o600); err != nil {
			t.Fatal("writing only the generated fixture policy")
		}
		if strings.Count(config, "AllowTcpForwarding no\n") != 1 || strings.Contains(config, "ForceCommand ") || strings.Contains(config, "AcceptEnv ") {
			t.Fatal("unexpected baseline fixture policy")
		}
		config = strings.Replace(config, "AllowTcpForwarding no\n", "AllowTcpForwarding yes\n", 1)
		if forcedOnly {
			if strings.Count(config, "PermitRootLogin prohibit-password\n") != 1 {
				t.Fatal("unexpected baseline root login policy")
			}
			config = strings.Replace(config, "PermitRootLogin prohibit-password\n", "PermitRootLogin forced-commands-only\n", 1)
		}
		return config + "AllowStreamLocalForwarding no\nPermitUserEnvironment no\nPermitUserRC no\nSubsystem sftp internal-sftp\n"
	})
	if forcedOnly {
		out, err := exec.CommandContext(t.Context(), sshStageSSHD, "-T", "-f", filepath.Join(filepath.Dir(logPath), "sshd_config")).CombinedOutput()
		if err != nil {
			t.Fatal("effective test sshd configuration unavailable")
		}
		for _, required := range []string{"permitrootlogin forced-commands-only", "forcecommand none", "authenticationmethods publickey"} {
			if !bytes.Contains(append([]byte{'\n'}, out...), []byte("\n"+required+"\n")) {
				t.Fatal("effective hardened root policy differs")
			}
		}
	}
	return &sshAuthorizationFixture{sshStageFixture: f, controlKey: controlKey, logPath: logPath}
}

func sshAuthorizationRootRefusals(t *testing.T, path string) int {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal("opening generated fixture log")
	}
	body, readErr := io.ReadAll(io.LimitReader(file, 128*1024+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(body) > 128*1024 {
		t.Fatal("bounded fixture log read")
	}
	count := 0
	for _, line := range strings.Split(string(body), "\n") {
		if strings.Contains(line, "ROOT LOGIN REFUSED FROM 127.0.0.1 port ") && !strings.Contains(line, "[preauth]") && !strings.Contains(line, "[postauth]") {
			count++
		}
	}
	return count
}

func sshAuthorizationConnect(ctx context.Context, f *sshAuthorizationFixture, key []byte) (*ssh.Client, error) {
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, errors.New("invalid generated test key")
	}
	conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", f.address)
	if err != nil {
		return nil, fmt.Errorf("dialing fixture: %w", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, errors.New("fixture connection deadline unavailable")
	}
	config := &ssh.ClientConfig{User: "root", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
		if ssh.FingerprintSHA256(key) != f.hostPin {
			return errors.New("fixture host pin mismatch")
		}
		return nil
	}}
	c, channels, requests, err := ssh.NewClientConn(conn, f.address, config)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return ssh.NewClient(c, channels, requests), nil
}

func sshAuthorizationClient(t *testing.T, f *sshAuthorizationFixture, key []byte) *ssh.Client {
	t.Helper()
	client, err := sshAuthorizationConnect(t.Context(), f, key)
	if err != nil {
		t.Fatal("connecting generated key to protected fixture")
	}
	t.Cleanup(func() { _ = client.Close(); _ = client.Wait() })
	return client
}

var sshAuthorizationSFTPInit = []byte{0, 0, 0, 5, 1, 0, 0, 0, 3}

type sshAuthorizationOutput struct {
	body  bytes.Buffer
	limit int
}

func (b *sshAuthorizationOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.body.Len() {
		return 0, errors.New("fixture output limit")
	}
	return b.body.Write(p)
}

func sshAuthorizationRequest(t *testing.T, client *ssh.Client, request, command string, body []byte) ([]byte, []byte, error) {
	t.Helper()
	session, err := client.NewSession()
	if err != nil {
		t.Fatal("opening fixture session")
	}
	defer func() { _ = session.Close() }()
	out, diagnostic := &sshAuthorizationOutput{limit: 2048}, &sshAuthorizationOutput{limit: 4096}
	session.Stdout, session.Stderr, session.Stdin = out, diagnostic, bytes.NewReader(body)
	switch request {
	case "exec":
		err = session.Start(command)
	case "shell":
		err = session.Shell()
	default:
		t.Fatal("unknown fixture request")
	}
	if err == nil {
		err = session.Wait()
	}
	return out.body.Bytes(), diagnostic.body.Bytes(), err
}

func controlRun(t *testing.T, client *ssh.Client, command string) error {
	t.Helper()
	out, diagnostic, err := sshAuthorizationRequest(t, client, "exec", command, nil)
	if err == nil && (len(out) != 0 || len(diagnostic) != 0) {
		return errors.New("unexpected fixture command output")
	}
	return err
}

func sshAuthorizationCanaryCommand(p string) string {
	return "printf 'namespace-control' > '" + strings.ReplaceAll(p, "'", "'\\''") + "'"
}

func assertSSHAuthorizationCanary(t *testing.T, p string) {
	t.Helper()
	body, err := os.ReadFile(p)
	if err != nil || string(body) != "namespace-control" {
		t.Fatal("unrestricted namespace-local canary effect missing")
	}
}

func sshAuthorizationFrame(t *testing.T, intent domain.ProbeBinaryStageIntent, b *sshStageTrackedBinary) []byte {
	t.Helper()
	header, err := provisioning.EncodeProbeBinaryStageHeader(intent)
	if err != nil {
		t.Fatal("encoding fixture frame")
	}
	reader, err := b.Open(t.Context())
	if err != nil {
		t.Fatal("opening borrowed fixture snapshot")
	}
	body, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || int64(len(body)) != intent.Identity.Binary.Size {
		t.Fatal("reading/closing fixture snapshot")
	}
	return append(header, body...)
}

func assertSSHAuthorizationReceipt(t *testing.T, body []byte, intent domain.ProbeBinaryStageIntent) {
	t.Helper()
	type receipt struct {
		State         string `json:"state"`
		DirectoryName string `json:"directory_name"`
		MarkerSHA256  string `json:"marker_sha256"`
		PinnedVersion string `json:"pinned_version"`
		VersionSource string `json:"version_source"`
		OS            string `json:"os"`
		Architecture  string `json:"architecture"`
		SHA256        string `json:"sha256"`
		Size          int64  `json:"size"`
	}
	var got receipt
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if len(body) == 0 || len(body) > 2048 || body[len(body)-1] != '\n' || decoder.Decode(&got) != nil {
		t.Fatal("bounded helper receipt missing")
	}
	var extra json.RawMessage
	if !errors.Is(decoder.Decode(&extra), io.EOF) {
		t.Fatal("helper receipt has a trailer")
	}
	pin := intent.Identity.Binary
	want := receipt{State: "local_binary_staged", DirectoryName: filepath.Base(intent.Directory()), MarkerSHA256: intent.MarkerSHA256(), PinnedVersion: pin.Version, VersionSource: "operator_pin", OS: "linux", Architecture: pin.Architecture, SHA256: pin.SHA256, Size: pin.Size}
	if got != want {
		t.Fatal("helper receipt does not match the exact fixture intent")
	}
}

// RequestSubsystem does not start ssh.Session's copies or make Session.Wait
// valid. Use an owned raw channel and join its request/stderr readers instead.
func sshAuthorizationSubsystem(t *testing.T, client *ssh.Client, control bool, payload []byte) (int, []byte, []byte, error) {
	t.Helper()
	channel, requests, err := client.OpenChannel("session", nil)
	if err != nil {
		return -1, nil, nil, errors.New("subsystem channel unavailable")
	}
	type exitResult struct {
		status  int
		count   int
		invalid bool
	}
	type readResult struct {
		body []byte
		err  error
	}
	exitDone, stderrDone := make(chan exitResult, 1), make(chan readResult, 1)
	go func() {
		r := exitResult{status: -1}
		for request := range requests {
			if request.Type == "exit-status" {
				r.count++
				if len(request.Payload) != 4 {
					r.invalid = true
				} else {
					r.status = int(binary.BigEndian.Uint32(request.Payload))
				}
			} else if request.Type == "exit-signal" {
				r.invalid = true
			}
			if request.WantReply {
				_ = request.Reply(false, nil)
			}
		}
		exitDone <- r
	}()
	go func() {
		body, err := io.ReadAll(io.LimitReader(channel.Stderr(), 4097))
		stderrDone <- readResult{body, err}
	}()
	exitJoined, stderrJoined := false, false
	defer func() {
		_ = channel.Close()
		_ = client.Close()
		_ = client.Wait()
		if !exitJoined {
			select {
			case <-exitDone:
			case <-time.After(2 * time.Second):
				t.Error("subsystem exit reader did not join")
			}
		}
		if !stderrJoined {
			select {
			case <-stderrDone:
			case <-time.After(2 * time.Second):
				t.Error("subsystem stderr reader did not join")
			}
		}
	}()
	ok, err := channel.SendRequest("subsystem", true, ssh.Marshal(struct{ Name string }{"sftp"}))
	if err != nil || !ok {
		return -1, nil, nil, errors.New("subsystem request not accepted")
	}
	if n, err := channel.Write(payload); err != nil || n != len(payload) {
		return -1, nil, nil, errors.New("subsystem payload write")
	}
	if control {
		var header [4]byte
		if _, err := io.ReadFull(channel, header[:]); err != nil {
			return -1, nil, nil, errors.New("SFTP VERSION header missing")
		}
		size := binary.BigEndian.Uint32(header[:])
		if size < 5 || size > 64*1024 {
			return -1, nil, nil, errors.New("SFTP VERSION bound")
		}
		body := make([]byte, int(size))
		if _, err := io.ReadFull(channel, body); err != nil || body[0] != 2 || binary.BigEndian.Uint32(body[1:5]) != 3 {
			return -1, nil, nil, errors.New("SFTP v3 VERSION invalid")
		}
	}
	if err := channel.CloseWrite(); err != nil {
		return -1, nil, nil, errors.New("subsystem input completion")
	}
	out, err := io.ReadAll(io.LimitReader(channel, 2049))
	if err != nil || len(out) > 2048 {
		return -1, nil, nil, errors.New("subsystem stdout bound/completion")
	}
	var diagnostic readResult
	select {
	case diagnostic = <-stderrDone:
		stderrJoined = true
	case <-time.After(12 * time.Second):
		return -1, nil, nil, errors.New("subsystem stderr timeout")
	}
	if diagnostic.err != nil || len(diagnostic.body) > 4096 {
		return -1, nil, nil, errors.New("subsystem stderr bound/completion")
	}
	var exit exitResult
	select {
	case exit = <-exitDone:
		exitJoined = true
	case <-time.After(12 * time.Second):
		return -1, nil, nil, errors.New("subsystem exit timeout")
	}
	if exit.invalid || exit.count != 1 {
		return -1, nil, nil, errors.New("normal subsystem exit status missing")
	}
	return exit.status, out, diagnostic.body, nil
}
