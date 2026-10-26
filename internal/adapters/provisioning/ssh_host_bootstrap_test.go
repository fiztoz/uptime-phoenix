package provisioning

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

const (
	bootstrapTestPassword = "correct-horse-battery-STAPLE-7319"
	bootstrapForcedOption = `command="/usr/local/bin/phoenix-probe-admin authorized-dispatch"`
)

// bootstrapHost is a real loopback SSH server standing in for a target host.
// Password sessions run the received command with the local /bin/sh, so the
// production scripts execute for real against a private prefix; a PATH shim
// supplies Linux uname output and a sudo that checks the password it reads
// from stdin. Public-key logins emulate sshd's first-match authorized_keys
// semantics over the file the script wrote and answer the forced
// install-preflight from the files actually present.
type bootstrapHost struct {
	t             *testing.T
	address       string
	hostKey       ssh.Signer
	layout        hostBootstrapLayout
	loginUser     string
	loginPassword string
	machine       string
	sudoPassword  string
	rejectRoot    bool

	passwordAuths atomic.Int32
	keyAuths      atomic.Int32
	mu            sync.Mutex
	commands      []string
	transcripts   []string
}

func newBootstrapHost(t *testing.T, user string, configure func(*bootstrapHost)) *bootstrapHost {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell host fixture")
	}
	base := t.TempDir()
	uid, gid := strconv.Itoa(os.Getuid()), strconv.Itoa(os.Getgid())
	h := &bootstrapHost{
		t: t, loginUser: user, loginPassword: bootstrapTestPassword, machine: "x86_64",
		sudoPassword: bootstrapTestPassword,
		layout: hostBootstrapLayout{
			uploadBase: filepath.Join(base, "tmp"), prefix: filepath.Join(base, "root"),
			uid: uid, gid: gid, sshDir: filepath.Join(base, "root", "root", ".ssh"),
		},
	}
	_, h.hostKey = deploymentKey(t)
	for _, dir := range []string{h.layout.uploadBase, filepath.Join(h.layout.prefix, "usr", "local"), filepath.Join(h.layout.prefix, "etc"), filepath.Join(h.layout.prefix, "root")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if configure != nil {
		configure(h)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h.address = listener.Addr().String()
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				h.serve(ctx, conn)
			}()
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		wg.Wait()
	})
	return h
}

func (h *bootstrapHost) shimDir() string {
	dir := filepath.Join(filepath.Dir(h.layout.prefix), "shim")
	if _, err := os.Stat(dir); err == nil {
		return dir
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		h.t.Error(err)
	}
	uname := "#!/bin/sh\ncase \"$1\" in -s) echo Linux ;; -m) echo " + h.machine + " ;; *) exec /usr/bin/uname \"$@\" ;; esac\n"
	sudo := "#!/bin/sh\nIFS= read -r pw || exit 1\n[ \"$pw\" = \"" + h.sudoPassword + "\" ] || exit 1\n[ \"$1\" = -S ] && [ \"$2\" = -- ] || exit 1\nshift 2\nexec \"$@\"\n"
	for name, body := range map[string]string{"uname": uname, "sudo": sudo} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil {
			h.t.Error(err)
		}
	}
	return dir
}

func (h *bootstrapHost) serve(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	config := &ssh.ServerConfig{
		PasswordCallback: func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			h.passwordAuths.Add(1)
			if meta.User() != h.loginUser || string(password) != h.loginPassword {
				return nil, errors.New("password rejected")
			}
			return &ssh.Permissions{Extensions: map[string]string{"mode": "password"}}, nil
		},
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			h.keyAuths.Add(1)
			if meta.User() != "root" || h.rejectRoot || !h.restrictedEntry(key) {
				return nil, errors.New("key rejected")
			}
			return &ssh.Permissions{Extensions: map[string]string{"mode": "forced"}}, nil
		},
	}
	config.AddHostKey(h.hostKey)
	server, channels, requests, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	defer func() { _ = server.Close() }()
	go ssh.DiscardRequests(requests)
	var sessions sync.WaitGroup
	defer sessions.Wait()
	for request := range channels {
		if request.ChannelType() != "session" {
			_ = request.Reject(ssh.Prohibited, "session only")
			continue
		}
		channel, reqs, err := request.Accept()
		if err != nil {
			return
		}
		sessions.Add(1)
		go func() {
			defer sessions.Done()
			defer func() { _ = channel.Close() }()
			for req := range reqs {
				var command struct{ Command string }
				if req.Type != "exec" || ssh.Unmarshal(req.Payload, &command) != nil {
					_ = req.Reply(false, nil)
					continue
				}
				_ = req.Reply(true, nil)
				status := h.exec(ctx, server.Permissions.Extensions["mode"], command.Command, channel)
				_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
				return
			}
		}()
	}
}

// restrictedEntry emulates sshd: the FIRST authorized_keys line naming the
// key decides, and it must carry exactly restrict + the forced dispatcher.
func (h *bootstrapHost) restrictedEntry(key ssh.PublicKey) bool {
	data, err := os.ReadFile(filepath.Join(h.layout.sshDir, "authorized_keys"))
	if err != nil {
		return false
	}
	for len(data) > 0 {
		parsed, _, options, rest, err := ssh.ParseAuthorizedKey(data)
		if err != nil {
			return false
		}
		data = rest
		if bytes.Equal(parsed.Marshal(), key.Marshal()) {
			return slices.Equal(options, []string{"restrict", bootstrapForcedOption})
		}
	}
	return false
}

func (h *bootstrapHost) exec(ctx context.Context, mode, command string, channel ssh.Channel) uint32 {
	if mode == "forced" {
		h.mu.Lock()
		h.commands = append(h.commands, mode+":"+command)
		h.transcripts = append(h.transcripts, "")
		h.mu.Unlock()
		_, _ = io.Copy(io.Discard, channel)
		if command != SSHProbeInstallPreflightCommand {
			return 2
		}
		_, _ = channel.Write(EncodeProbeInstallHostReport(h.preflightReport()))
		return 0
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Env = []string{"PATH=" + h.shimDir() + ":/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + filepath.Dir(h.layout.prefix)}
	var transcript bytes.Buffer
	cmd.Stdin = io.TeeReader(channel, &limitedBuffer{buf: &transcript, max: 4096})
	cmd.Stdout = channel
	cmd.Stderr = channel.Stderr()
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	h.mu.Lock()
	h.commands = append(h.commands, mode+":"+command)
	h.transcripts = append(h.transcripts, transcript.String())
	h.mu.Unlock()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return uint32(exitErr.ExitCode())
	}
	if err != nil {
		return 255
	}
	return 0
}

// preflightReport is what the real helper would report: authority=1 only when
// the trust file holds the hub key with root-like protection.
func (h *bootstrapHost) preflightReport() domain.ProbeInstallHostReport {
	helper, err := os.Stat(filepath.Join(h.layout.prefix, "usr", "local", "bin", "phoenix-probe-admin"))
	pub, pubErr := os.ReadFile(filepath.Join(h.layout.prefix, "etc", "phoenix-probe", "stage-authority.pub"))
	_, keyErr := ParseProbeBinaryStageAuthorityKey(pub)
	return domain.ProbeInstallHostReport{
		Kernel: "Linux", Machine: h.machine, Systemd: true,
		Root:      err == nil && helper.Mode().Perm() == 0o755,
		Authority: pubErr == nil && keyErr == nil,
	}
}

type limitedBuffer struct {
	buf *bytes.Buffer
	max int
}

func (w *limitedBuffer) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		w.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

type bootstrapFixture struct {
	host      *bootstrapHost
	helper    []byte
	pin       domain.ProbeBinaryPin
	authority [32]byte
	key       []byte
	signer    ssh.Signer
	b         *SSHProbeHostBootstrapper
}

var (
	helperFixtureOnce sync.Once
	helperFixtureData []byte
)

func bootstrapHelperELF(t *testing.T) []byte {
	t.Helper()
	helperFixtureOnce.Do(func() {
		helperFixtureData = buildArtifactFixture(t, "linux", "amd64", "cmd/phoenix-probe-admin", "0")
	})
	if helperFixtureData == nil {
		t.Fatal("helper fixture unavailable")
	}
	return helperFixtureData
}

func newBootstrapFixture(t *testing.T, user string, configure func(*bootstrapHost)) *bootstrapFixture {
	t.Helper()
	host := newBootstrapHost(t, user, configure)
	helper := bootstrapHelperELF(t)
	pin := artifactPin(helper, "amd64")
	artifacts := t.TempDir()
	if err := os.WriteFile(filepath.Join(artifacts, domain.HelperReleaseName(pin)), helper, 0o600); err != nil {
		t.Fatal(err)
	}
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var authority [32]byte
	copy(authority[:], public)
	policy := probe.EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
	b, err := NewSSHProbeHostBootstrapper(artifacts, policy, authority)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	b.layout = host.layout
	key, signer := deploymentKey(t)
	return &bootstrapFixture{host: host, helper: helper, pin: pin, authority: authority, key: key, signer: signer, b: b}
}

func (f *bootstrapFixture) request() domain.ProbeHostBootstrapRequest {
	return domain.ProbeHostBootstrapRequest{
		Host:               domain.ProbeSSHHost{Address: f.host.address, User: f.host.loginUser},
		Password:           []byte(bootstrapTestPassword),
		HostKeyFingerprint: ssh.FingerprintSHA256(f.host.hostKey.PublicKey()),
		PrivateKey:         bytes.Clone(f.key),
		Helper:             f.pin,
		RequestedBy:        1,
	}
}

func (f *bootstrapFixture) path(parts ...string) string {
	return filepath.Join(append([]string{f.host.layout.prefix}, parts...)...)
}

func (f *bootstrapFixture) wantLine() string {
	fields := strings.Fields(string(ssh.MarshalAuthorizedKey(f.signer.PublicKey())))
	return "restrict," + bootstrapForcedOption + " " + fields[0] + " " + fields[1]
}

// assertNoSecret checks that the password never reached a command line, an
// error, the uploaded data or a host-side file, and that it only ever crossed
// stdin as the sudo password line.
func (f *bootstrapFixture) assertNoSecret(t *testing.T, err error, sudo bool) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), bootstrapTestPassword) {
		t.Fatalf("error leaks the password: %v", err)
	}
	f.host.mu.Lock()
	defer f.host.mu.Unlock()
	for _, command := range f.host.commands {
		if strings.Contains(command, bootstrapTestPassword) || strings.Contains(command, "PRIVATE KEY") {
			t.Fatalf("command line carries a secret: %q", command)
		}
	}
	for i, transcript := range f.host.transcripts {
		if strings.Contains(transcript, bootstrapTestPassword) {
			isRootStep := strings.Contains(f.host.commands[i], "phoenix-host-bootstrap '")
			if !sudo || !isRootStep || transcript != bootstrapTestPassword+"\n" {
				t.Fatalf("password crossed stdin outside the sudo line: step %d", i)
			}
		}
	}
	// The sudo shim is the test's stand-in for the host's own password
	// database, so it is the one file allowed to hold the password.
	walkErr := filepath.WalkDir(filepath.Dir(f.host.layout.prefix), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || strings.HasSuffix(path, filepath.Join("shim", "sudo")) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(bootstrapTestPassword)) {
			t.Fatalf("password persisted on the host at %s", path)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
}

func bootstrapCode(err error) string {
	var refusal *domain.ProbeHostBootstrapError
	if errors.As(err, &refusal) {
		return refusal.Code
	}
	return ""
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode()
}

func uploadsLeft(t *testing.T, f *bootstrapFixture) []string {
	t.Helper()
	entries, err := os.ReadDir(f.host.layout.uploadBase)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestSSHHostBootstrapSudoUserInstallsVerifiesAndIsIdempotent(t *testing.T) {
	f := newBootstrapFixture(t, "ops", nil)
	foreign := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHVuci1yZWxhdGVkLWtleS1mb3ItdGVzdGluZy0wMDAw operator@laptop"
	if err := os.MkdirAll(f.host.layout.sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// No trailing newline: the appended entry must not merge into it.
	if err := os.WriteFile(filepath.Join(f.host.layout.sshDir, "authorized_keys"), []byte("# keep me\n"+foreign), 0o600); err != nil {
		t.Fatal(err)
	}
	req := f.request()
	result, err := f.b.Bootstrap(t.Context(), req)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	want := domain.ProbeHostBootstrapResult{Helper: "installed", StageAuthority: "installed", AuthorizedKey: "added", Verified: true}
	if result != want {
		t.Fatalf("result=%+v", result)
	}
	if got := f.host.passwordAuths.Load(); got != 1 {
		t.Fatalf("password logins=%d, want exactly one for every step", got)
	}
	helper, err := os.ReadFile(f.path("usr", "local", "bin", "phoenix-probe-admin"))
	if err != nil || !bytes.Equal(helper, f.helper) {
		t.Fatalf("helper bytes differ: %v", err)
	}
	if mode := fileMode(t, f.path("usr", "local", "bin", "phoenix-probe-admin")); mode != 0o755 {
		t.Fatalf("helper mode=%v", mode)
	}
	pub, err := os.ReadFile(f.path("etc", "phoenix-probe", "stage-authority.pub"))
	if err != nil || !bytes.Equal(pub, EncodeProbeBinaryStageAuthorityKey(f.authority)) {
		t.Fatalf("stage authority=%q err=%v", pub, err)
	}
	if mode := fileMode(t, f.path("etc", "phoenix-probe", "stage-authority.pub")); mode != 0o644 {
		t.Fatalf("authority mode=%v", mode)
	}
	if mode := fileMode(t, f.path("etc", "phoenix-probe")); mode != os.ModeDir|0o755 {
		t.Fatalf("authority dir mode=%v", mode)
	}
	ak := filepath.Join(f.host.layout.sshDir, "authorized_keys")
	keys, err := os.ReadFile(ak)
	if err != nil {
		t.Fatal(err)
	}
	if string(keys) != "# keep me\n"+foreign+"\n"+f.wantLine()+"\n" {
		t.Fatalf("authorized_keys=%q", keys)
	}
	if mode := fileMode(t, ak); mode != 0o600 {
		t.Fatalf("authorized_keys mode=%v", mode)
	}
	if left := uploadsLeft(t, f); len(left) != 0 {
		t.Fatalf("upload directory left behind: %v", left)
	}
	if f.host.keyAuths.Load() == 0 {
		t.Fatal("restricted key was never verified")
	}
	if !bytes.Equal(req.Password, []byte(bootstrapTestPassword)) {
		t.Fatal("adapter must only borrow the password; the service clears it")
	}
	f.assertNoSecret(t, nil, true)

	// A rerun changes nothing and still verifies.
	again, err := f.b.Bootstrap(t.Context(), f.request())
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if again != (domain.ProbeHostBootstrapResult{Helper: "unchanged", StageAuthority: "unchanged", AuthorizedKey: "unchanged", Verified: true}) {
		t.Fatalf("rerun result=%+v", again)
	}
	if after, _ := os.ReadFile(ak); !bytes.Equal(after, keys) {
		t.Fatal("rerun rewrote authorized_keys")
	}
	if left := uploadsLeft(t, f); len(left) != 0 {
		t.Fatalf("rerun left an upload: %v", left)
	}
	f.assertNoSecret(t, nil, true)
}

func TestSSHHostBootstrapRootLoginNeedsNoSudo(t *testing.T) {
	f := newBootstrapFixture(t, "root", func(h *bootstrapHost) { h.sudoPassword = "sudo-must-not-run" })
	result, err := f.b.Bootstrap(t.Context(), f.request())
	if err != nil || !result.Verified || result.AuthorizedKey != "added" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	f.host.mu.Lock()
	for _, command := range f.host.commands {
		if strings.HasPrefix(command, "password:sudo") {
			t.Fatalf("root login used sudo: %q", command)
		}
	}
	f.host.mu.Unlock()
	f.assertNoSecret(t, nil, false)
}

func TestSSHHostBootstrapRepairsModeOfMatchingFiles(t *testing.T) {
	f := newBootstrapFixture(t, "ops", nil)
	if _, err := f.b.Bootstrap(t.Context(), f.request()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f.path("etc", "phoenix-probe", "stage-authority.pub"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f.path("usr", "local", "bin", "phoenix-probe-admin"), 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := f.b.Bootstrap(t.Context(), f.request())
	if err != nil || result.Helper != "installed" || result.StageAuthority != "installed" || result.AuthorizedKey != "unchanged" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if fileMode(t, f.path("etc", "phoenix-probe", "stage-authority.pub")) != 0o644 || fileMode(t, f.path("usr", "local", "bin", "phoenix-probe-admin")) != 0o755 {
		t.Fatal("modes were not restored")
	}
}

func TestSSHHostBootstrapRefusals(t *testing.T) {
	cases := []struct {
		name      string
		user      string
		configure func(*bootstrapHost)
		prepare   func(*testing.T, *bootstrapFixture)
		request   func(*bootstrapFixture, *domain.ProbeHostBootstrapRequest)
		code      string
		// passwordSent is whether the password may have reached the host.
		passwordSent bool
		// helperWritten is whether the root step may have written anything.
		uploadLeftOK bool
	}{
		{
			name: "wrong_host_pin", user: "ops", code: domain.ProbeHostBootstrapTrust,
			request: func(f *bootstrapFixture, r *domain.ProbeHostBootstrapRequest) {
				_, other := deploymentKey(f.host.t)
				r.HostKeyFingerprint = ssh.FingerprintSHA256(other.PublicKey())
			},
		},
		{
			name: "wrong_password", user: "ops", code: domain.ProbeHostBootstrapAuth, passwordSent: true,
			configure: func(h *bootstrapHost) { h.loginPassword = "something-else" },
		},
		{
			name: "sudo_refuses", user: "ops", code: domain.ProbeHostBootstrapPrivilege, passwordSent: true,
			configure: func(h *bootstrapHost) { h.sudoPassword = "different-sudo-password" },
		},
		{
			name: "architecture_mismatch", user: "ops", code: domain.ProbeHostBootstrapArchitecture, passwordSent: true,
			configure: func(h *bootstrapHost) { h.machine = "aarch64" },
		},
		{
			name: "unrestricted_duplicate_key", user: "ops", code: domain.ProbeHostBootstrapKeyClash, passwordSent: true,
			prepare: func(t *testing.T, f *bootstrapFixture) {
				writeAuthorizedKeys(t, f, string(ssh.MarshalAuthorizedKey(f.signer.PublicKey())))
			},
		},
		{
			name: "different_stage_authority", user: "ops", code: domain.ProbeHostBootstrapAuthorityClash, passwordSent: true,
			prepare: func(t *testing.T, f *bootstrapFixture) {
				var other [32]byte
				other[0] = 7
				if err := os.MkdirAll(f.path("etc", "phoenix-probe"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(f.path("etc", "phoenix-probe", "stage-authority.pub"), EncodeProbeBinaryStageAuthorityKey(other), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "group_writable_ssh_dir", user: "ops", code: domain.ProbeHostBootstrapUnsafeLayout, passwordSent: true,
			prepare: func(t *testing.T, f *bootstrapFixture) {
				if err := os.MkdirAll(f.host.layout.sshDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(f.host.layout.sshDir, 0o770); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "restricted_key_not_accepted", user: "ops", code: domain.ProbeHostBootstrapVerifyAuth, passwordSent: true,
			configure: func(h *bootstrapHost) { h.rejectRoot = true },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newBootstrapFixture(t, tc.user, tc.configure)
			if tc.prepare != nil {
				tc.prepare(t, f)
			}
			before := snapshotTree(t, f.host.layout.prefix)
			req := f.request()
			if tc.request != nil {
				tc.request(f, &req)
			}
			result, err := f.b.Bootstrap(t.Context(), req)
			if bootstrapCode(err) != tc.code || result != (domain.ProbeHostBootstrapResult{}) {
				t.Fatalf("code=%q want %q (err=%v result=%+v)", bootstrapCode(err), tc.code, err, result)
			}
			if !tc.passwordSent && f.host.passwordAuths.Load() != 0 {
				t.Fatal("password was offered to an unverified host")
			}
			if tc.code != domain.ProbeHostBootstrapVerifyAuth {
				if after := snapshotTree(t, f.host.layout.prefix); after != before {
					t.Fatalf("a refusal changed the host:\nbefore=%s\nafter=%s", before, after)
				}
			}
			if left := uploadsLeft(t, f); len(left) != 0 {
				t.Fatalf("upload left behind: %v", left)
			}
			f.assertNoSecret(t, err, true)
		})
	}
}

func TestSSHHostBootstrapArtifactRefusedBeforeAnyConnection(t *testing.T) {
	f := newBootstrapFixture(t, "ops", nil)
	for name, mutate := range map[string]func(*domain.ProbeBinaryPin){
		"digest":  func(p *domain.ProbeBinaryPin) { p.SHA256 = strings.Repeat("0", 64) },
		"size":    func(p *domain.ProbeBinaryPin) { p.Size++ },
		"missing": func(p *domain.ProbeBinaryPin) { p.Version = "9.9.9" },
		"arch":    func(p *domain.ProbeBinaryPin) { p.Architecture = "arm64" },
	} {
		t.Run(name, func(t *testing.T) {
			req := f.request()
			mutate(&req.Helper)
			if _, err := f.b.Bootstrap(t.Context(), req); !errors.Is(err, domain.ErrProbeInstallArtifact) {
				t.Fatalf("err=%v", err)
			}
		})
	}
	if f.host.passwordAuths.Load() != 0 || f.host.keyAuths.Load() != 0 {
		t.Fatal("a rejected helper still connected")
	}
	// The probe release itself is not a helper: build info must name the helper.
	probeELF := buildArtifactFixture(t, "linux", "amd64", "cmd/probe", "0")
	pin := artifactPin(probeELF, "amd64")
	pin.Version = "0.4.6"
	if err := os.WriteFile(filepath.Join(f.b.artifacts.Name(), domain.HelperReleaseName(pin)), probeELF, 0o600); err != nil {
		t.Fatal(err)
	}
	req := f.request()
	req.Helper = pin
	if _, err := f.b.Bootstrap(t.Context(), req); !errors.Is(err, domain.ErrProbeInstallArtifact) {
		t.Fatalf("probe binary accepted as helper: %v", err)
	}
}

func TestSSHHostBootstrapInputRefusals(t *testing.T) {
	f := newBootstrapFixture(t, "ops", nil)
	for name, mutate := range map[string]func(*domain.ProbeHostBootstrapRequest){
		"newline_password": func(r *domain.ProbeHostBootstrapRequest) { r.Password = []byte("a\nb") },
		"empty_password":   func(r *domain.ProbeHostBootstrapRequest) { r.Password = nil },
		"bad_key":          func(r *domain.ProbeHostBootstrapRequest) { r.PrivateKey = []byte("not a key") },
		"bad_pin":          func(r *domain.ProbeHostBootstrapRequest) { r.HostKeyFingerprint = "SHA256:nope" },
	} {
		t.Run(name, func(t *testing.T) {
			req := f.request()
			mutate(&req)
			if _, err := f.b.Bootstrap(t.Context(), req); !errors.Is(err, domain.ErrProbeSSHInput) {
				t.Fatalf("err=%v", err)
			}
		})
	}
	if f.host.passwordAuths.Load() != 0 {
		t.Fatal("invalid input still connected")
	}
}

func TestSSHHostBootstrapCommandQuoting(t *testing.T) {
	for _, script := range []string{hostBootstrapUploadScript, hostBootstrapRootScript} {
		if strings.Contains(script, "'") {
			t.Fatal("scripts must not contain a single quote")
		}
	}
	if _, ok := hostBootstrapCommand("true", "name", false, "it's"); ok {
		t.Fatal("argument with a quote accepted")
	}
	if _, ok := hostBootstrapCommand("true", "name", false, "a b"); ok {
		t.Fatal("argument with a space accepted")
	}
	got, ok := hostBootstrapCommand("true", "name", true, "", "x")
	if !ok || got != "sudo -S -- /bin/sh -c 'true' name '' 'x'" {
		t.Fatalf("command=%q", got)
	}
	if _, ok := parseHostBootstrapResult("phoenix-host-bootstrap-v1 helper=installed authority=installed authorized_key=added\nextra\n"); ok {
		t.Fatal("trailing output accepted")
	}
	if _, ok := parseHostBootstrapResult("phoenix-host-bootstrap-v1 helper=maybe authority=installed authorized_key=added\n"); ok {
		t.Fatal("unknown state accepted")
	}
}

func TestSSHHostBootstrapCancellation(t *testing.T) {
	f := newBootstrapFixture(t, "ops", nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.b.Bootstrap(ctx, f.request()); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	ctx, cancel = context.WithTimeout(t.Context(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	if _, err := f.b.Bootstrap(ctx, f.request()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
}

func writeAuthorizedKeys(t *testing.T, f *bootstrapFixture, body string) {
	t.Helper()
	if err := os.MkdirAll(f.host.layout.sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.host.layout.sshDir, "authorized_keys"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// snapshotTree renders every path, mode and content digest under root.
func snapshotTree(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		fmt.Fprintf(&b, "%s %v", rel, info.Mode())
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(&b, " %x", sha256.Sum256(data))
		}
		b.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}
