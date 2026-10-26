//go:build linux || darwin

package bootstrap

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"embed"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	httppkg "github.com/fiztoz/uptime-phoenix/internal/adapters/http"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// Production-path tests for POST /api/probe-host-bootstraps:
//
//	httptest -> real router (SessionOrAPIKey + RequireAdmin + credential rate
//	limit, real AuthService/JWT/API keys on SQLite) -> ProbeHostBootstrapHandlers
//	-> services.ProbeHostBootstrapService -> provisioning.SSHProbeHostBootstrapper
//	(composed by newProbeInstallOperations from the real key/policy/artifact
//	configuration) -> a loopback Go SSH server with password authentication.
//
// The server never runs the scripts (the production layout writes /usr/local/bin
// as root); it answers the upload step as a non-Linux host. Script effects are
// covered by the provisioning package's real-shell fixture.

const hostBootstrapAPIPassword = "api-path-Secret-Passw0rd-913"

type hostBootstrapAPIServer struct {
	address       string
	hostKey       ssh.Signer
	passwordAuths atomic.Int32
	mu            sync.Mutex
	passwords     []string
	commands      []string
}

func serveHostBootstrapAPI(t *testing.T) *hostBootstrapAPIServer {
	t.Helper()
	_, hostKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(hostKey)
	if err != nil {
		t.Fatal(err)
	}
	s := &hostBootstrapAPIServer{hostKey: signer}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.address = listener.Addr().String()
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
				s.serve(conn)
			}()
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); wg.Wait() })
	return s
}

func (s *hostBootstrapAPIServer) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	config := &ssh.ServerConfig{PasswordCallback: func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		s.passwordAuths.Add(1)
		s.mu.Lock()
		s.passwords = append(s.passwords, string(password))
		s.mu.Unlock()
		if meta.User() != "ops" || string(password) != hostBootstrapAPIPassword {
			return nil, errors.New("denied")
		}
		return nil, nil
	}}
	config.AddHostKey(s.hostKey)
	server, channels, requests, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	defer func() { _ = server.Close() }()
	go ssh.DiscardRequests(requests)
	for request := range channels {
		channel, reqs, err := request.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = channel.Close() }()
			for req := range reqs {
				var command struct{ Command string }
				if req.Type != "exec" || ssh.Unmarshal(req.Payload, &command) != nil {
					_ = req.Reply(false, nil)
					continue
				}
				_ = req.Reply(true, nil)
				s.mu.Lock()
				s.commands = append(s.commands, command.Command)
				s.mu.Unlock()
				// Answer the upload step as a non-Linux host: exit 30.
				_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{30}))
				return
			}
		}()
	}
}

func hostBootstrapHelperELF(t *testing.T) ([]byte, domain.ProbeBinaryPin) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"go.mod":                          "module github.com/fiztoz/uptime-phoenix\n\ngo 1.23\n",
		"cmd/phoenix-probe-admin/main.go": "package main\nfunc main(){println(\"helper\")}\n",
	} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, goToolPath(t), "build", "-trimpath", "-ldflags=-s -w", "-o", "helper", "./cmd/phoenix-probe-admin")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0", "GOENV=off", "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "GOWORK=off", "GOFLAGS=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helper build: %v %s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(dir, "helper"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return data, domain.ProbeBinaryPin{Version: "1.2.3", Architecture: "amd64", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))}
}

type hostBootstrapAPIFixture struct {
	server                    *hostBootstrapAPIServer
	ops                       *probeInstallOperations
	authSvc                   *services.AuthService
	users                     *sqlite.UserRepo
	apiKeys                   *sqlite.APIKeyRepo
	adminToken, viewerToken   string
	writeKey, readKey, viewer string
	pin                       domain.ProbeBinaryPin
	deployKey                 string
	logs                      *lockedBuffer
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newHostBootstrapAPIFixture(t *testing.T) *hostBootstrapAPIFixture {
	t.Helper()
	ctx := context.Background()
	data, pin := hostBootstrapHelperELF(t)
	keyDir := t.TempDir()
	if err := os.Chmod(keyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(keyDir, "stage-authority.key")
	if err := auth.CreateProbeSecretKeyFile(ctx, keyPath); err != nil {
		t.Fatal(err)
	}
	policyPath := filepath.Join(keyDir, "endpoint-policy.json")
	if err := os.WriteFile(policyPath, []byte(`{"allowed_cidrs":["127.0.0.1/32"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	artifactDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(artifactDir, domain.HelperReleaseName(pin)), data, 0o600); err != nil {
		t.Fatal(err)
	}
	db := lifecycleOpenDB(t, "file:"+filepath.Join(t.TempDir(), "bootstrap-api.db")+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)", true)
	ops, err := newProbeInstallOperations(ctx, Config{
		ProbesEnabled: true, ProbeSSHInstallEnabled: true, ProbeStageAuthorityKeyFile: keyPath,
		ProbeInstallArtifactDir: artifactDir, ProbeEndpointPolicyFile: policyPath,
	}, db, &services.ProbeAdminService{})
	if err != nil || ops == nil || ops.bootstrapService() == nil {
		t.Fatalf("composition: ops=%v err=%v", ops, err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := ops.Close(closeCtx); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	_, deploy, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(deploy)
	if err != nil {
		t.Fatal(err)
	}
	f := &hostBootstrapAPIFixture{
		server: serveHostBootstrapAPI(t), ops: ops, pin: pin,
		users: sqlite.NewUserRepo(db), apiKeys: sqlite.NewAPIKeyRepo(db),
		deployKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		logs:      &lockedBuffer{},
	}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	jwt := auth.NewJWTAuthenticator(strings.Repeat("m8-host-bootstrap-", 4), 1, f.users)
	f.authSvc = services.NewAuthService(f.users, f.apiKeys, jwt, nil)
	installFixture := &installAPIFixture{db: db, users: f.users}
	adminID := installFixture.newUser(t, "bootstrap-admin", true)
	viewerID := installFixture.newUser(t, "bootstrap-viewer", false)
	if f.adminToken, err = jwt.IssueSession(ctx, adminID); err != nil {
		t.Fatal(err)
	}
	if f.viewerToken, err = jwt.IssueSession(ctx, viewerID); err != nil {
		t.Fatal(err)
	}
	if f.writeKey, _, err = f.authSvc.CreateAPIKey(ctx, adminID, "bootstrap-write", []string{"write"}, nil); err != nil {
		t.Fatal(err)
	}
	if f.readKey, _, err = f.authSvc.CreateAPIKey(ctx, adminID, "bootstrap-read", []string{"read"}, nil); err != nil {
		t.Fatal(err)
	}
	if f.viewer, _, err = f.authSvc.CreateAPIKey(ctx, viewerID, "viewer-write", []string{"write"}, nil); err != nil {
		t.Fatal(err)
	}
	return f
}

// router builds a fresh router (and so a fresh rate-limit budget).
func (f *hostBootstrapAPIFixture) router() http.Handler {
	opts := httppkg.RouterOptions{ProbeHostBootstraps: handlers.NewProbeHostBootstrapHandlers(f.ops.bootstrapService(), true)}
	return httppkg.NewRouter(
		handlers.NewHealthHandlers(func() bool { return true }), handlers.NewAuthHandlers(f.authSvc), nil, nil, nil,
		nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil,
		f.authSvc, services.NewAccessService(f.users, nil, nil, nil), f.apiKeys, nil, embed.FS{}, opts, nil, "")
}

func (f *hostBootstrapAPIFixture) body(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	m := map[string]any{
		"ssh_address": f.server.address, "ssh_user": "ops", "ssh_password": hostBootstrapAPIPassword,
		"host_key_fingerprint": ssh.FingerprintSHA256(f.server.hostKey.PublicKey()), "private_key": f.deployKey,
		"helper_version": f.pin.Version, "helper_architecture": f.pin.Architecture,
		"helper_sha256": f.pin.SHA256, "helper_size": f.pin.Size,
	}
	if mutate != nil {
		mutate(m)
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (f *hostBootstrapAPIFixture) post(t *testing.T, h http.Handler, authorization, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/probe-host-bootstraps", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	for _, secret := range []string{hostBootstrapAPIPassword, "PRIVATE KEY", f.server.address} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("response leaks %q: %s", secret, rec.Body.String())
		}
	}
	return rec
}

func TestProbeHostBootstrapAPIAuthGates(t *testing.T) {
	f := newHostBootstrapAPIFixture(t)
	h := f.router()
	for name, tc := range map[string]struct {
		authorization string
		status        int
	}{
		"anonymous":        {"", http.StatusUnauthorized},
		"read-scope key":   {"ApiKey " + f.readKey, http.StatusUnauthorized},
		"non-admin jwt":    {"Bearer " + f.viewerToken, http.StatusForbidden},
		"non-admin apikey": {"ApiKey " + f.viewer, http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			if rec := f.post(t, h, tc.authorization, f.body(t, nil)); rec.Code != tc.status {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
	if f.server.passwordAuths.Load() != 0 {
		t.Fatal("a refused caller reached the host")
	}
}

func TestProbeHostBootstrapAPIProductionPath(t *testing.T) {
	f := newHostBootstrapAPIFixture(t)
	h := f.router()

	// A wrong host pin is refused before the password is offered.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	otherSigner, _ := ssh.NewSignerFromKey(other)
	rec := f.post(t, h, "Bearer "+f.adminToken, f.body(t, func(m map[string]any) {
		m["host_key_fingerprint"] = ssh.FingerprintSHA256(otherSigner.PublicKey())
	}))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"code":"host_trust_failed"`) {
		t.Fatalf("wrong pin: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if f.server.passwordAuths.Load() != 0 {
		t.Fatal("password offered to an unverified host")
	}

	// Pinned host, write-scope admin API key: the password reaches the host's
	// SSH authentication exactly once, and the host refusal maps to a fixed code.
	rec = f.post(t, h, "ApiKey "+f.writeKey, f.body(t, nil))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"code":"host_unsupported_os"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	f.server.mu.Lock()
	passwords, commands := append([]string(nil), f.server.passwords...), append([]string(nil), f.server.commands...)
	f.server.mu.Unlock()
	if len(passwords) != 1 || passwords[0] != hostBootstrapAPIPassword {
		t.Fatalf("host saw %d password attempts", len(passwords))
	}
	if len(commands) != 1 || !strings.Contains(commands[0], "phoenix-host-bootstrap-upload") {
		t.Fatalf("commands=%d", len(commands))
	}
	for _, command := range commands {
		if strings.Contains(command, hostBootstrapAPIPassword) {
			t.Fatal("password placed on a command line")
		}
	}

	// A wrong password is a fixed refusal too.
	rec = f.post(t, h, "Bearer "+f.adminToken, f.body(t, func(m map[string]any) { m["ssh_password"] = "not-the-password" }))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"code":"host_password_rejected"`) {
		t.Fatalf("wrong password: status=%d body=%s", rec.Code, rec.Body.String())
	}

	logs := f.logs.String()
	for _, secret := range []string{hostBootstrapAPIPassword, "not-the-password", "PRIVATE KEY", f.server.address} {
		if strings.Contains(logs, secret) {
			t.Fatalf("logs leak %q", secret)
		}
	}
	if !strings.Contains(logs, `"outcome":"host_unsupported_os"`) {
		t.Fatalf("audit outcome missing: %s", logs)
	}
}

func TestProbeHostBootstrapAPIRateLimited(t *testing.T) {
	f := newHostBootstrapAPIFixture(t)
	h := f.router()
	body := f.body(t, func(m map[string]any) { m["ssh_password"] = "guess" })
	for i := range 5 {
		if rec := f.post(t, h, "Bearer "+f.adminToken, body); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("attempt %d status=%d body=%s", i+1, rec.Code, rec.Body.String())
		}
	}
	rec := f.post(t, h, "Bearer "+f.adminToken, body)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("sixth attempt status=%d", rec.Code)
	}
	if got := f.server.passwordAuths.Load(); got != 5 {
		t.Fatalf("host saw %d password attempts, want 5 (the limited one must not connect)", got)
	}
}
