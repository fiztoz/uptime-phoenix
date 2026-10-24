//go:build linux || darwin

package bootstrap

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
	"golang.org/x/crypto/ssh"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	httppkg "github.com/fiztoz/uptime-phoenix/internal/adapters/http"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// End-to-end tests for the M8 administrative installer API. Every layer below
// the Go SSH server is production code:
//
//	httptest request -> real Echo router (SessionOrAPIKey + RequireAdmin, real
//	AuthService/JWT/API keys on SQLite) -> handlers.ProbeInstallHandlers ->
//	services.ProbeInstallOperationService -> provisioning.SSHProbeInstallSessionFactory
//	(real key parse, artifact preparation and SSH sender) -> services.ProbeInstallRunner
//	-> journal on a real on-disk SQLite database -> SSHProbeInstallExecutor ->
//	one loopback Go SSH server that dispatches each fixed exec command to the
//	real authorized helpers (runAuthorizedProbeBinaryStageHelper and
//	runAuthorizedProbeHostActionHelper) exactly like RunAuthorizedProbeDispatch.
//
// The one-shot admissions are the real stores on the same database and the
// grants are signed by a real Ed25519ProbeStageGrantSigner over a generated
// key. The helpers' production-only host dependencies are injected: the trusted
// authority key, the host-key set, the entropy, the /opt stand-in parent and the
// host-action applier (the real provisioning.ApplyProbe* code beneath temporary
// directories standing in for /var/lib and /etc/systemd/system, with a fake
// systemctl). Nothing outside t.TempDir() is touched, nothing runs as root, no
// Docker, no network beyond 127.0.0.1, and the staged ELF is never executed.
//
// Evidence limits: SQLite only (no MariaDB/InnoDB claim), the helpers run
// in-process behind a Go SSH server (no real sshd, no uid 0, no systemd).

const (
	installAPIPath = "/api/probe-installs"

	installAPIOpA = "a1111111-1111-4111-8111-111111111111"
	installAPIOpB = "b2222222-2222-4222-8222-222222222222"
	installAPIOpC = "c3333333-3333-4333-8333-333333333333"

	// installAPIWait bounds every wait for an asynchronous state.
	installAPIWait = 60 * time.Second
	// installAPIHoldLimit bounds a held helper so a failing test never strands it.
	installAPIHoldLimit = 90 * time.Second
	installAPIPoll      = 20 * time.Millisecond
)

// installAPIKinds is the frozen systemd layout v1 order.
var installAPIKinds = []string{"data_directory", "artifact_directory", "unit_file", "service"}

// installAPIELFCache builds each Linux ELF fixture once per test binary run.
var installAPIELFCache struct {
	mu   sync.Mutex
	data map[string][]byte
}

func installAPIELFData(t *testing.T, arch string) []byte {
	t.Helper()
	installAPIELFCache.mu.Lock()
	defer installAPIELFCache.mu.Unlock()
	if data, ok := installAPIELFCache.data[arch]; ok {
		return data
	}
	data := helperEffectFixture(t, arch)
	if installAPIELFCache.data == nil {
		installAPIELFCache.data = map[string][]byte{}
	}
	installAPIELFCache.data[arch] = data
	return data
}

// installAPISystemctl is a fake systemctl keyed by unit: a unit reads
// "disabled" (exit 1) until it has been enabled, then "enabled". It models only
// the verbs the service action may use; any other verb (notably start) fails.
type installAPISystemctl struct {
	mu      sync.Mutex
	calls   [][]string
	enabled map[string]bool
}

func (s *installAPISystemctl) Run(_ context.Context, args ...string) (string, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, slices.Clone(args))
	if len(args) == 0 {
		return "", -1, errors.New("systemctl without a verb")
	}
	switch {
	case args[0] == "is-enabled" && len(args) == 2:
		if s.enabled[args[1]] {
			return "enabled\n", 0, nil
		}
		return "disabled\n", 1, nil
	case args[0] == "daemon-reload" && len(args) == 1:
		return "", 0, nil
	case args[0] == "enable" && len(args) == 2:
		s.enabled[args[1]] = true
		return "", 0, nil
	case args[0] == "disable" && len(args) == 3 && args[1] == "--now":
		delete(s.enabled, args[2])
		return "", 0, nil
	case args[0] == "reset-failed" && len(args) == 2:
		return "", 0, nil
	}
	return "", -1, errors.New("unexpected systemctl invocation " + args[0])
}

func (s *installAPISystemctl) recorded() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// installAPITree is the fake host: /var/lib and /etc/systemd/system stand-ins
// and the recording systemctl.
type installAPITree struct {
	varLib  string
	unitDir string
	// opt is the stage parent, the /opt stand-in cleanup removes from.
	opt    string
	sysctl *installAPISystemctl
}

// apply is the injected host-action applier: the real provisioning appliers
// beneath the stand-in directories.
func (tr *installAPITree) apply(ctx context.Context, intent domain.ProbeHostActionIntent) error {
	if intent.Kind == domain.ProbeHostActionCleanup {
		return tr.cleanup(ctx, intent)
	}
	dir := tr.unitDir
	if intent.Kind == domain.ProbeHostActionDataDirectory {
		dir = tr.varLib
	}
	parent, err := os.Open(dir)
	if err != nil {
		return domain.ErrProbeBinaryStageWriteUnavailable
	}
	defer func() { _ = parent.Close() }()
	switch intent.Kind {
	case domain.ProbeHostActionDataDirectory:
		return provisioning.ApplyProbeDataDirectory(ctx, parent, intent)
	case domain.ProbeHostActionUnitFile:
		return provisioning.ApplyProbeUnitFile(ctx, parent, intent)
	case domain.ProbeHostActionService:
		return provisioning.ApplyProbeService(ctx, parent, intent, tr.sysctl)
	}
	return domain.ErrProbeBinaryStageWriteInput
}

// cleanup runs the real cleanup applier beneath the three stand-in parents.
func (tr *installAPITree) cleanup(ctx context.Context, intent domain.ProbeHostActionIntent) error {
	var parents []*os.File
	defer func() {
		for _, parent := range parents {
			_ = parent.Close()
		}
	}()
	for _, dir := range []string{tr.varLib, tr.opt, tr.unitDir} {
		parent, err := os.Open(dir)
		if err != nil {
			return domain.ErrProbeBinaryStageWriteUnavailable
		}
		parents = append(parents, parent)
	}
	return provisioning.ApplyProbeCleanup(ctx, parents[0], parents[1], parents[2], intent, tr.sysctl)
}

func (tr *installAPITree) dataPath(op string) string {
	return filepath.Join(tr.varLib, "uptime-phoenix-probe-"+op)
}

func (tr *installAPITree) unitName(op string) string {
	return "uptime-phoenix-probe-" + op + ".service"
}

func (tr *installAPITree) unitPath(op string) string {
	return filepath.Join(tr.unitDir, tr.unitName(op))
}

type installAPIServerConfig struct {
	trusted     [32]byte
	stageParent string
	tree        *installAPITree
	// holdApply names a host-action kind whose applier blocks (after the grant
	// verified and stdin reached EOF) until the server is released.
	holdApply string
	// holdStageChallenge blocks the stage helper before it emits its challenge.
	holdStageChallenge bool
}

// installAPIServer is a loopback Go SSH server whose exec handler dispatches
// the two fixed authorized commands like RunAuthorizedProbeDispatch.
type installAPIServer struct {
	cfg        installAPIServerConfig
	address    string
	hostSigner ssh.Signer
	hostFP     string
	clientPEM  []byte
	clientPub  ssh.PublicKey
	clientFP   string

	held        chan string
	release     chan struct{}
	releaseOnce sync.Once

	mu    sync.Mutex
	conns int
	// preflights counts install-preflight runs; their connections are not
	// in conns, so the resource-helper assertions stay exact.
	preflights int
	commands   []string
	codes      []int
	applies    []string
	stderr     bytes.Buffer
	wg         sync.WaitGroup
}

func serveInstallAPI(t *testing.T, cfg installAPIServerConfig) *installAPIServer {
	t.Helper()
	clientPEM, clientSigner := authzE2EKey(t)
	_, hostSigner := authzE2EKey(t)
	s := &installAPIServer{
		cfg: cfg, hostSigner: hostSigner, hostFP: ssh.FingerprintSHA256(hostSigner.PublicKey()),
		clientPEM: clientPEM, clientPub: clientSigner.PublicKey(), clientFP: ssh.FingerprintSHA256(clientSigner.PublicKey()),
		held: make(chan string, 16), release: make(chan struct{}),
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.address = listener.Addr().String()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns++
			s.mu.Unlock()
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.serve(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		s.releaseAll()
		_ = listener.Close()
		done := make(chan struct{})
		go func() { s.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("install API SSH fixture did not stop")
		}
	})
	return s
}

func (s *installAPIServer) releaseAll() { s.releaseOnce.Do(func() { close(s.release) }) }

// hold signals that a helper reached its hold point and blocks it until the
// server is released, the helper's own context ends or the safety limit hits.
func (s *installAPIServer) hold(ctx context.Context, label string) bool {
	select {
	case s.held <- label:
	default:
	}
	select {
	case <-s.release:
		return true
	case <-ctx.Done():
		return false
	case <-time.After(installAPIHoldLimit):
		return false
	}
}

// awaitHeld blocks until a helper reached its hold point and returns its label.
func (s *installAPIServer) awaitHeld(t *testing.T) string {
	t.Helper()
	select {
	case label := <-s.held:
		return label
	case <-time.After(installAPIWait):
		t.Fatal("no helper reached its hold point")
		return ""
	}
}

func (s *installAPIServer) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	config := &ssh.ServerConfig{PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if meta.User() != "root" || !bytes.Equal(key.Marshal(), s.clientPub.Marshal()) {
			return nil, errors.New("rejected")
		}
		return nil, nil
	}}
	config.AddHostKey(s.hostSigner)
	server, channels, requests, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	defer func() { _ = server.Close() }()
	go ssh.DiscardRequests(requests)
	for newCh := range channels {
		ch, reqs, err := newCh.Accept()
		if err != nil {
			return
		}
		for req := range reqs {
			var exec struct{ Command string }
			if req.Type != "exec" || ssh.Unmarshal(req.Payload, &exec) != nil {
				_ = req.Reply(false, nil)
				continue
			}
			_ = req.Reply(true, nil)
			if exec.Command == provisioning.SSHProbeInstallPreflightCommand {
				var stderr bytes.Buffer
				code := runProbeInstallPreflightHelper(context.Background(), nil, authzE2EInput{ch: ch}, ch, &stderr, s.preflightReport)
				s.mu.Lock()
				s.conns--
				s.preflights++
				s.mu.Unlock()
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(code)}))
				_ = ch.Close()
				break
			}
			s.mu.Lock()
			s.commands = append(s.commands, exec.Command)
			s.mu.Unlock()
			var stderr bytes.Buffer
			code := s.dispatch(exec.Command, ch, &stderr)
			s.mu.Lock()
			s.codes = append(s.codes, code)
			s.stderr.Write(stderr.Bytes())
			s.mu.Unlock()
			_, _ = ch.Stderr().Write(stderr.Bytes())
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(code)}))
			_ = ch.Close()
			break
		}
		return
	}
}

// dispatch routes the exact fixed commands to the real helpers, like the
// production forced command; anything else is refused with 127.
func (s *installAPIServer) dispatch(command string, ch ssh.Channel, stderr *bytes.Buffer) int {
	ctx := context.Background()
	hostKeys := func(context.Context) ([]string, error) { return []string{s.hostFP}, nil }
	switch command {
	case provisioning.SSHProbeBinaryStageAuthorizedCommand:
		loadKey := func(ctx context.Context) ([32]byte, error) {
			if s.cfg.holdStageChallenge && !s.hold(ctx, "stage_challenge") {
				return [32]byte{}, errors.New("held helper was not released")
			}
			return s.cfg.trusted, nil
		}
		acquire := func(context.Context) (*os.File, error) { return os.Open(s.cfg.stageParent) }
		return runAuthorizedProbeBinaryStageHelper(ctx, nil, authzE2EInput{ch: ch}, ch, stderr, loadKey, acquire, rand.Reader, hostKeys)
	case provisioning.SSHProbeHostActionCommand:
		loadKey := func(context.Context) ([32]byte, error) { return s.cfg.trusted, nil }
		return runAuthorizedProbeHostActionHelper(ctx, nil, authzE2EInput{ch: ch}, ch, stderr, loadKey, hostKeys, rand.Reader, probeHostActionWithoutReceipt(s.apply))
	}
	return 127
}

// preflightReport describes a clean amd64 systemd host whose existing
// installs are the Phoenix entries already in the fixture tree.
func (s *installAPIServer) preflightReport(context.Context) (domain.ProbeInstallHostReport, error) {
	report := domain.ProbeInstallHostReport{Kernel: "Linux", Machine: "x86_64", Systemd: true, Root: true, Authority: true}
	for _, dir := range []string{s.cfg.stageParent, s.cfg.tree.varLib, s.cfg.tree.unitDir} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return domain.ProbeInstallHostReport{}, err
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), probeInstallPreflightOwnedPrefix) {
				report.ExistingInstalls++
			}
		}
	}
	return report, nil
}

// apply records the action, optionally holds it, then applies it for real.
func (s *installAPIServer) apply(ctx context.Context, intent domain.ProbeHostActionIntent) error {
	s.mu.Lock()
	s.applies = append(s.applies, intent.Kind)
	s.mu.Unlock()
	if intent.Kind == s.cfg.holdApply && !s.hold(ctx, intent.Kind) {
		return domain.ErrProbeBinaryStageWriteUnavailable
	}
	return s.cfg.tree.apply(ctx, intent)
}

type installAPIServerView struct {
	conns      int
	preflights int
	commands   []string
	codes      []int
	applies    []string
	stderr     string
}

func (s *installAPIServer) view() installAPIServerView {
	s.mu.Lock()
	defer s.mu.Unlock()
	return installAPIServerView{
		conns: s.conns, preflights: s.preflights, commands: slices.Clone(s.commands), codes: slices.Clone(s.codes),
		applies: slices.Clone(s.applies), stderr: s.stderr.String(),
	}
}

// awaitCodes waits until n helper runs have exited and returns the record.
func (s *installAPIServer) awaitCodes(t *testing.T, n int) installAPIServerView {
	t.Helper()
	deadline := time.Now().Add(installAPIWait)
	for {
		v := s.view()
		if len(v.codes) >= n {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper exits = %d, want %d", len(v.codes), n)
		}
		time.Sleep(installAPIPoll)
	}
}

type installAPIOptions struct {
	holdApply          string
	holdStageChallenge bool
	skipArtifact       bool
	// viaComposition builds the service through the production composition
	// root (newProbeInstallOperations: key and policy loaded from files)
	// instead of composing the pieces here.
	viaComposition bool
}

type installAPICounts struct{ operations, resources, stage, actions int }

// installAPIFixture is the complete production composition under test.
type installAPIFixture struct {
	server      *installAPIServer
	tree        *installAPITree
	db          *bun.DB
	dbPath      string
	data        []byte
	pin         domain.ProbeBinaryPin
	artifactDir string
	stageParent string
	scratch     string

	svc *services.ProbeInstallOperationService
	// ops is the production composition (viaComposition only).
	ops *probeInstallOperations
	// shutdown joins every run and releases the artifact directory exactly
	// once; the cleanup calls it and tests may call it explicitly first.
	shutdown func(context.Context) error

	users   ports.UserRepository
	apiKeys ports.APIKeyRepository
	authSvc *services.AuthService

	adminID, adminTwoID, viewerID int64
	adminToken, viewerToken       string
	writeKey, readKey, viewerKey  string

	handler http.Handler
}

func newInstallAPIFixture(t *testing.T, o installAPIOptions) *installAPIFixture {
	t.Helper()
	ctx := context.Background()
	// The ELF is built before TMPDIR is redirected to the private scratch area.
	data := installAPIELFData(t, "amd64")
	pin := helperEffectIntent(t, data, "amd64").Identity.Binary
	stageParent, _, scratch := helperEffectParent(t)
	var (
		authority  ed25519.PrivateKey
		trusted    [32]byte
		keyPath    string
		policyPath string
	)
	if o.viaComposition {
		// The hub's stage authority is a protected 32-byte seed file and the
		// endpoint policy a JSON file, exactly as the composition root loads them.
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		keyPath = filepath.Join(dir, "stage-authority.key")
		if err := auth.CreateProbeSecretKeyFile(ctx, keyPath); err != nil {
			t.Fatal(err)
		}
		seed, err := auth.ReadProbeSecretKeyFile(ctx, keyPath)
		if err != nil {
			t.Fatal(err)
		}
		pub, ok := ed25519.NewKeyFromSeed(seed[:]).Public().(ed25519.PublicKey)
		if !ok {
			t.Fatal("stage authority public key")
		}
		copy(trusted[:], pub)
		policyPath = filepath.Join(dir, "endpoint-policy.json")
		if err := os.WriteFile(policyPath, []byte(`{"allowed_cidrs":["127.0.0.1/32"]}`), 0o600); err != nil {
			t.Fatal(err)
		}
	} else {
		authority, trusted = authzE2EAuthority(t)
	}
	tree := &installAPITree{varLib: t.TempDir(), unitDir: t.TempDir(), opt: stageParent, sysctl: &installAPISystemctl{enabled: map[string]bool{}}}
	server := serveInstallAPI(t, installAPIServerConfig{
		trusted: trusted, stageParent: stageParent, tree: tree, holdApply: o.holdApply, holdStageChallenge: o.holdStageChallenge,
	})
	artifactDir := t.TempDir()
	if !o.skipArtifact {
		if err := os.WriteFile(filepath.Join(artifactDir, pin.ReleaseName()), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	dbPath := filepath.Join(t.TempDir(), "install-api.db")
	db := lifecycleOpenDB(t, "file:"+dbPath+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)", true)
	f := &installAPIFixture{
		server: server, tree: tree, db: db, dbPath: dbPath, data: data, pin: pin,
		artifactDir: artifactDir, stageParent: stageParent, scratch: scratch,
		users: sqlite.NewUserRepo(db), apiKeys: sqlite.NewAPIKeyRepo(db),
	}
	var closeOnce sync.Once
	var closeErr error
	if o.viaComposition {
		ops, err := newProbeInstallOperations(ctx, Config{
			ProbesEnabled: true, ProbeSSHInstallEnabled: true, ProbeStageAuthorityKeyFile: keyPath,
			ProbeInstallArtifactDir: artifactDir, ProbeEndpointPolicyFile: policyPath,
		}, db, &services.ProbeAdminService{}) // activation is composed but unused here
		if err != nil || ops == nil || ops.service() == nil {
			t.Fatalf("production composition: ops=%v err=%v", ops, err)
		}
		f.svc = ops.service()
		f.ops = ops
		f.shutdown = func(ctx context.Context) error {
			closeOnce.Do(func() { closeErr = ops.Close(ctx) })
			return closeErr
		}
	} else {
		signer, err := provisioning.NewEd25519ProbeStageGrantSigner(authority)
		if err != nil {
			t.Fatal(err)
		}
		stage, err := services.NewProbeBinaryStageAuthorizationService(repository.NewProbeBinaryStageAdmissionStore(db), signer)
		if err != nil {
			t.Fatal(err)
		}
		actions, err := services.NewProbeHostActionAuthorizationService(repository.NewProbeHostActionAdmissionStore(db), signer)
		if err != nil {
			t.Fatal(err)
		}
		policy := probe.EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
		factory, err := provisioning.NewSSHProbeInstallSessionFactory(artifactDir, policy, stage, actions)
		if err != nil {
			t.Fatal(err)
		}
		svc, err := services.NewProbeInstallOperationService(ctx, lifecycleJournal(t, db), factory, uuid.NewString(), 2)
		if err != nil {
			_ = factory.Close()
			t.Fatal(err)
		}
		f.svc = svc
		f.shutdown = func(ctx context.Context) error {
			closeOnce.Do(func() { closeErr = errors.Join(svc.Close(ctx), factory.Close()) })
			return closeErr
		}
	}
	// Registered after the server and the database, so it runs first: free any
	// held helper, join every run, then release the artifact directory.
	t.Cleanup(func() {
		server.releaseAll()
		closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := f.shutdown(closeCtx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})

	jwt := auth.NewJWTAuthenticator(strings.Repeat("m8-install-api-", 5), 1, f.users)
	var err error
	f.authSvc = services.NewAuthService(f.users, f.apiKeys, jwt, nil)
	f.adminID = f.newUser(t, "install-admin", true)
	f.adminTwoID = f.newUser(t, "install-admin-two", true)
	f.viewerID = f.newUser(t, "install-viewer", false)
	if f.adminToken, err = jwt.IssueSession(ctx, f.adminID); err != nil {
		t.Fatal(err)
	}
	if f.viewerToken, err = jwt.IssueSession(ctx, f.viewerID); err != nil {
		t.Fatal(err)
	}
	if f.writeKey, _, err = f.authSvc.CreateAPIKey(ctx, f.adminID, "install-write", []string{"write"}, nil); err != nil {
		t.Fatal(err)
	}
	if f.readKey, _, err = f.authSvc.CreateAPIKey(ctx, f.adminID, "install-read", []string{"read"}, nil); err != nil {
		t.Fatal(err)
	}
	if f.viewerKey, _, err = f.authSvc.CreateAPIKey(ctx, f.viewerID, "viewer-write", []string{"write"}, nil); err != nil {
		t.Fatal(err)
	}
	f.handler = f.router(handlers.NewProbeInstallHandlers(f.svc, true))
	return f
}

func (f *installAPIFixture) newUser(t *testing.T, name string, admin bool) int64 {
	t.Helper()
	ctx := context.Background()
	result, err := f.db.ExecContext(ctx, "INSERT INTO users (username, password_hash) VALUES (?, ?)", name, "x")
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if admin {
		user, err := f.users.GetByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		user.IsAdmin = true
		if err := f.users.Update(ctx, user); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// router builds the real router around the given install handlers (nil keeps
// the router's own default for an unwired surface).
func (f *installAPIFixture) router(h *handlers.ProbeInstallHandlers) http.Handler {
	opts := httppkg.RouterOptions{}
	if h != nil {
		opts.ProbeInstalls = h
	}
	if f.ops != nil {
		opts.ProbeActivations = handlers.NewProbeActivationHandlers(f.ops.activationService(), true)
		opts.ProbeCleanups = handlers.NewProbeCleanupHandlers(f.ops.cleanupService(), true)
	}
	return httppkg.NewRouter(
		handlers.NewHealthHandlers(func() bool { return true }), handlers.NewAuthHandlers(f.authSvc), nil, nil, nil,
		nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil,
		f.authSvc, services.NewAccessService(f.users, nil, nil, nil), f.apiKeys, nil, embed.FS{}, opts, nil, "")
}

func (f *installAPIFixture) call(t *testing.T, h http.Handler, method, path, authorization, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func (f *installAPIFixture) asAdmin(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.call(t, f.handler, method, path, "Bearer "+f.adminToken, body)
}

// body renders the strict nine-member POST body for op.
func (f *installAPIFixture) body(t *testing.T, op string, mutate ...func(map[string]any)) string {
	t.Helper()
	m := map[string]any{
		"operation_id": op, "ssh_address": f.server.address, "ssh_user": "root",
		"host_key_fingerprint": f.server.hostFP, "private_key": string(f.server.clientPEM),
		"binary_version": f.pin.Version, "binary_architecture": f.pin.Architecture,
		"binary_sha256": f.pin.SHA256, "binary_size": f.pin.Size,
	}
	for _, fn := range mutate {
		fn(m)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func (f *installAPIFixture) counts(t *testing.T) installAPICounts {
	t.Helper()
	count := func(table string) int {
		var n int
		if err := f.db.NewSelect().TableExpr(table).ColumnExpr("count(*)").Scan(context.Background(), &n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}
	return installAPICounts{
		operations: count("probe_install_operations"), resources: count("probe_install_resources"),
		stage: count("probe_binary_stage_admissions"), actions: count("probe_host_action_admissions"),
	}
}

func (f *installAPIFixture) wantCounts(t *testing.T, label string, want installAPICounts) {
	t.Helper()
	if got := f.counts(t); got != want {
		t.Fatalf("%s: durable rows %+v, want %+v", label, got, want)
	}
}

type installAPIError struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func installAPIWantError(t *testing.T, label string, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("%s: status %d, want %d (body %s)", label, rec.Code, status, rec.Body.String())
	}
	var got installAPIError
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Code != code || got.Error == "" {
		t.Fatalf("%s: error body %q (%v), want code %q", label, rec.Body.String(), err, code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("%s: Cache-Control %q, want no-store", label, cc)
	}
}

// installAPIView strictly decodes one receipt: an unexpected field fails.
func installAPIView(t *testing.T, rec *httptest.ResponseRecorder) handlers.ProbeInstallView {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	var view handlers.ProbeInstallView
	if err := dec.Decode(&view); err != nil {
		t.Fatalf("receipt body is not exactly the frozen view: %v\n%s", err, rec.Body.String())
	}
	if dec.More() {
		t.Fatalf("trailing data after the receipt: %s", rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("receipt Cache-Control %q, want no-store", rec.Header().Get("Cache-Control"))
	}
	return view
}

func installAPIShape(view handlers.ProbeInstallView) []string {
	out := make([]string, 0, len(view.Resources))
	for _, r := range view.Resources {
		code := "-"
		if r.ErrorCode != nil {
			code = *r.ErrorCode
		}
		out = append(out, r.Kind+"/"+r.State+"/"+code)
	}
	return out
}

func installAPIWantShape(t *testing.T, label string, view handlers.ProbeInstallView, want ...string) {
	t.Helper()
	if got := installAPIShape(view); !slices.Equal(got, want) {
		t.Fatalf("%s: resources\n got %q\nwant %q", label, got, want)
	}
}

func installAPISettled(status string) bool {
	switch status {
	case domain.ProbeInstallResourcesReady, domain.ProbeInstallFailed, domain.ProbeInstallCanceled, domain.ProbeInstallInterrupted:
		return true
	}
	return false
}

// awaitStatus polls GET until the receipt reports one of want. Reaching any
// other settled status fails immediately with the whole receipt.
func (f *installAPIFixture) awaitStatus(t *testing.T, op string, within time.Duration, want ...string) (handlers.ProbeInstallView, string) {
	t.Helper()
	deadline := time.Now().Add(within)
	var trail []string
	for {
		rec := f.asAdmin(t, http.MethodGet, installAPIPath+"/"+op, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", op, rec.Code, rec.Body.String())
		}
		view := installAPIView(t, rec)
		if len(trail) == 0 || trail[len(trail)-1] != view.Status {
			trail = append(trail, view.Status)
		}
		if slices.Contains(want, view.Status) {
			t.Logf("status trail for %s: %v", op[:8], trail)
			return view, rec.Body.String()
		}
		if installAPISettled(view.Status) {
			t.Fatalf("operation settled as %q, want %v\ntrail %v resources %q error %+v\nhost stderr %q",
				view.Status, want, trail, installAPIShape(view), view.Error, f.server.view().stderr)
		}
		if time.Now().After(deadline) {
			t.Fatalf("status %q after %s, want %v (trail %v, resources %q)", view.Status, within, want, trail, installAPIShape(view))
		}
		time.Sleep(installAPIPoll)
	}
}

func (f *installAPIFixture) post(t *testing.T, op string) *httptest.ResponseRecorder {
	t.Helper()
	return f.asAdmin(t, http.MethodPost, installAPIPath, f.body(t, op))
}

func (f *installAPIFixture) wantAccepted(t *testing.T, label string, rec *httptest.ResponseRecorder, op string) handlers.ProbeInstallView {
	t.Helper()
	if rec.Code != http.StatusAccepted {
		t.Fatalf("%s: status %d, want 202 (body %s)", label, rec.Code, rec.Body.String())
	}
	view := installAPIView(t, rec)
	if view.OperationID != op || view.Kind != "ssh_probe_install" || view.Mode != "systemd" {
		t.Fatalf("%s: receipt identity %+v", label, view)
	}
	f.wantNoDisclosure(t, label, rec.Body.String())
	return view
}

// installToReady submits op as an admin and waits for resources_ready.
func (f *installAPIFixture) installToReady(t *testing.T, op string) (handlers.ProbeInstallView, string) {
	t.Helper()
	view := f.wantAccepted(t, "submit", f.post(t, op), op)
	if view.Status != domain.ProbeInstallQueued && view.Status != domain.ProbeInstallRunning {
		t.Fatalf("submit status %q, want queued or running", view.Status)
	}
	return f.awaitStatus(t, op, installAPIWait, domain.ProbeInstallResourcesReady)
}

// wantNoDisclosure fails when a response carries the key, the endpoint, the
// trust pins, the credentials or any internal field name.
func (f *installAPIFixture) wantNoDisclosure(t *testing.T, label, body string) {
	t.Helper()
	forbidden := []string{
		f.server.address, f.server.hostFP, f.server.clientFP, string(f.server.clientPEM), f.adminToken, f.viewerToken, f.writeKey,
		"PRIVATE KEY", "ssh_address", "ssh_user", "host_key_fingerprint", "client_key_fingerprint", "private_key", "requested_by",
		"owner_id", "attempt", "binary_sha256", "reference", "SHA256:",
	}
	for _, line := range strings.Split(string(f.server.clientPEM), "\n") {
		if len(line) > 16 {
			forbidden = append(forbidden, line)
		}
	}
	for _, needle := range forbidden {
		if strings.Contains(body, needle) {
			t.Fatalf("%s: response discloses %q: %s", label, needle, body)
		}
	}
}

// wantKeyNotAtRest searches the database files and every directory a complete
// install touches for the transient private key in each encoding it could take.
// It refuses a vacuous pass: the database and the host trees must hold content.
func (f *installAPIFixture) wantKeyNotAtRest(t *testing.T) {
	t.Helper()
	block, _ := pem.Decode(f.server.clientPEM)
	if block == nil {
		t.Fatal("fixture key is not PEM")
	}
	needles := [][]byte{
		f.server.clientPEM, block.Bytes,
		[]byte(base64.StdEncoding.EncodeToString(block.Bytes)), []byte(base64.RawStdEncoding.EncodeToString(block.Bytes)),
	}
	scanned := 0
	scan := func(path string, content []byte) {
		scanned++
		for _, needle := range needles {
			if bytes.Contains(content, needle) {
				t.Fatalf("transient private key found at rest in %s", path)
			}
		}
	}
	databaseBytes := 0
	for _, name := range []string{f.dbPath, f.dbPath + "-wal", f.dbPath + "-shm"} {
		if content, err := os.ReadFile(name); err == nil {
			databaseBytes += len(content)
			scan(name, content)
		}
	}
	if databaseBytes == 0 {
		t.Fatal("database files are empty: the at-rest scan would be vacuous")
	}
	hostFiles := 0
	for _, dir := range []string{f.stageParent, f.tree.varLib, f.tree.unitDir, f.scratch, f.artifactDir} {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type().IsRegular() {
				content, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				hostFiles++
				scan(path, content)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scan %s: %v", dir, err)
		}
	}
	// Staged binary, marker, unit file and the operator's release file.
	if hostFiles < 4 {
		t.Fatalf("only %d host files scanned: the at-rest scan would be vacuous", hostFiles)
	}
}

// wantScratchEmpty waits (bounded) for the session's private snapshot to be
// disposed: the receipt can read resources_ready a moment before the service
// goroutine's deferred session Close has run.
func (f *installAPIFixture) wantScratchEmpty(t *testing.T, label string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		entries, err := os.ReadDir(f.scratch)
		if err == nil && len(entries) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: private snapshot area not empty: %v %v", label, entries, err)
		}
		time.Sleep(installAPIPoll)
	}
}

func (f *installAPIFixture) wantHostUntouched(t *testing.T, label string) {
	t.Helper()
	hactWantEmptyDirs(t, f.stageParent, f.tree.varLib, f.tree.unitDir)
	if calls := f.tree.sysctl.recorded(); len(calls) != 0 {
		t.Fatalf("%s: systemctl was invoked: %v", label, calls)
	}
}

func (f *installAPIFixture) stored(t *testing.T, op string) *domain.ProbeInstallOperation {
	t.Helper()
	stored, err := f.svc.Observe(context.Background(), op)
	if err != nil {
		t.Fatalf("observe stored %s: %v", op, err)
	}
	return stored
}

// wantInstalledEffects asserts every real effect of one complete install of op
// on the fake host, using the stored receipt's identity and attempt IDs.
func (f *installAPIFixture) wantInstalledEffects(t *testing.T, op string) *domain.ProbeInstallOperation {
	t.Helper()
	stored := f.stored(t, op)
	if stored.State != domain.ProbeInstallResourcesReady || len(stored.Resources) != len(installAPIKinds) {
		t.Fatalf("stored receipt %s with %d resources", stored.State, len(stored.Resources))
	}
	for i, kind := range installAPIKinds {
		if r := stored.Resources[i]; r.Spec.Kind != kind || r.State != domain.ProbeInstallResourceApplied || r.AttemptID == "" {
			t.Fatalf("stored resource %d = %+v, want applied %s", i, r, kind)
		}
	}
	if stored.Identity.RequestedBy != f.adminID || stored.Identity.ClientKeyFingerprint != f.server.clientFP ||
		stored.Identity.HostKeyFingerprint != f.server.hostFP || stored.Identity.Mode != domain.ProbeInstallSystemd ||
		stored.Identity.Host != (domain.ProbeSSHHost{Address: f.server.address, User: "root"}) || stored.Identity.Binary != f.pin {
		t.Fatalf("stored identity does not match the authenticated actor and the request: %+v", stored.Identity)
	}

	// artifact_directory: the exact staged graph for the reserved attempt.
	stage := domain.ProbeBinaryStageIntent{Identity: stored.Identity, OwnerID: stored.OwnerID, AttemptID: stored.Resources[1].AttemptID}
	if !stage.Valid() {
		t.Fatal("stage intent rebuilt from the receipt is invalid")
	}
	helperEffectGraph(t, f.stageParent, stage, f.data)
	f.wantScratchEmpty(t, "after install")

	// data_directory: a plain 0700 directory owned by the helper's uid, empty.
	info, err := os.Lstat(f.tree.dataPath(op))
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky|os.ModeSymlink) != 0 {
		t.Fatalf("data directory %v %v, want a plain directory with 0700", info, err)
	}
	hactWantOwnedByUs(t, info)
	hactWantEmptyDirs(t, f.tree.dataPath(op))

	// unit_file: the exact domain.ProbeSystemdUnit bytes, 0644.
	info, err = os.Lstat(f.tree.unitPath(op))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o644 {
		t.Fatalf("unit file %v %v, want a regular 0644 file", info, err)
	}
	got, err := os.ReadFile(f.tree.unitPath(op))
	want := domain.ProbeSystemdUnit(stored.Identity)
	if err != nil || want == "" || string(got) != want {
		t.Fatalf("unit file bytes differ from domain.ProbeSystemdUnit: %v\n got %q\nwant %q", err, got, want)
	}
	if names := entryNames(t, f.tree.varLib); len(names) != 1 {
		t.Fatalf("/var/lib stand-in holds %v, want only the data directory", names)
	}
	if names := entryNames(t, f.tree.unitDir); len(names) != 1 {
		t.Fatalf("unit directory holds %v, want only the unit file", names)
	}

	// service: enabled, never started; only the fixed sequence reached systemctl.
	unit := f.tree.unitName(op)
	wantCalls := [][]string{{"is-enabled", unit}, {"daemon-reload"}, {"enable", unit}, {"is-enabled", unit}}
	if calls := f.tree.sysctl.recorded(); !slices.EqualFunc(calls, wantCalls, slices.Equal[[]string]) {
		t.Fatalf("systemctl calls %v, want %v", calls, wantCalls)
	}
	return stored
}

// wantServerRanFullInstall asserts the exact helper dispatch of n complete
// installs: per install one data_directory action, one stage, one unit_file
// action and one service action, every helper exiting 0 with empty stderr. For
// a single install the order is the frozen layout order.
func (f *installAPIFixture) wantServerRanFullInstall(t *testing.T, installs int) {
	t.Helper()
	v := f.server.awaitCodes(t, 4*installs)
	if len(v.commands) != 4*installs || len(v.codes) != 4*installs || v.conns != 4*installs || v.stderr != "" {
		t.Fatalf("server saw conns=%d commands=%d codes=%v stderr=%q, want %d complete installs", v.conns, len(v.commands), v.codes, v.stderr, installs)
	}
	for _, code := range v.codes {
		if code != 0 {
			t.Fatalf("a helper exited non-zero: %v", v.codes)
		}
	}
	if v.preflights < installs {
		t.Fatalf("host preflights = %d, want at least one per install (%d)", v.preflights, installs)
	}
	stages, actions := 0, 0
	for _, command := range v.commands {
		switch command {
		case provisioning.SSHProbeBinaryStageAuthorizedCommand:
			stages++
		case provisioning.SSHProbeHostActionCommand:
			actions++
		default:
			t.Fatalf("unexpected exec command %q", command)
		}
	}
	if stages != installs || actions != 3*installs {
		t.Fatalf("server ran %d stage and %d host-action helpers, want %d and %d", stages, actions, installs, 3*installs)
	}
	if installs == 1 {
		want := []string{provisioning.SSHProbeHostActionCommand, provisioning.SSHProbeBinaryStageAuthorizedCommand,
			provisioning.SSHProbeHostActionCommand, provisioning.SSHProbeHostActionCommand}
		if !slices.Equal(v.commands, want) || !slices.Equal(v.applies, []string{"data_directory", "unit_file", "service"}) {
			t.Fatalf("server order commands=%q applies=%q", v.commands, v.applies)
		}
	}
}

// ---------------------------------------------------------------------------
// 1. Happy path
// ---------------------------------------------------------------------------

func TestProbeInstallAPIHappyPathAppliesEveryResourceOnTheHost(t *testing.T) {
	f := newInstallAPIFixture(t, installAPIOptions{})

	// The write-scope API key of an admin is accepted like a session.
	rec := f.call(t, f.handler, http.MethodPost, installAPIPath, "ApiKey "+f.writeKey, f.body(t, installAPIOpA))
	view := f.wantAccepted(t, "submit", rec, installAPIOpA)
	if view.Status != domain.ProbeInstallQueued && view.Status != domain.ProbeInstallRunning {
		t.Fatalf("submit status %q, want queued or running", view.Status)
	}
	if view.Binary.Version != f.pin.Version || view.Binary.Architecture != f.pin.Architecture {
		t.Fatalf("submit binary view %+v", view.Binary)
	}
	if view.DeadlineAt.Sub(view.CreatedAt) != domain.ProbeInstallLifetime || view.UpdatedAt.Before(view.CreatedAt) {
		t.Fatalf("submit timestamps created=%v updated=%v deadline=%v", view.CreatedAt, view.UpdatedAt, view.DeadlineAt)
	}
	if len(view.Resources) != len(installAPIKinds) {
		t.Fatalf("submit receipt lists %d resources", len(view.Resources))
	}

	final, finalBody := f.awaitStatus(t, installAPIOpA, installAPIWait, domain.ProbeInstallResourcesReady)
	installAPIWantShape(t, "final", final, "data_directory/applied/-", "artifact_directory/applied/-", "unit_file/applied/-", "service/applied/-")
	if final.Error != nil || final.OperationID != installAPIOpA || final.Kind != "ssh_probe_install" || final.Mode != "systemd" ||
		final.Binary.Version != f.pin.Version || final.Binary.Architecture != f.pin.Architecture {
		t.Fatalf("final receipt %+v", final)
	}
	if !final.CreatedAt.Equal(view.CreatedAt) || final.UpdatedAt.Before(view.UpdatedAt) {
		t.Fatalf("final timestamps %v/%v vs submit %v/%v", final.CreatedAt, final.UpdatedAt, view.CreatedAt, view.UpdatedAt)
	}
	f.wantNoDisclosure(t, "final GET", finalBody)

	stored := f.wantInstalledEffects(t, installAPIOpA)
	if stored.State != domain.ProbeInstallResourcesReady {
		t.Fatalf("stored state %s", stored.State)
	}
	f.wantServerRanFullInstall(t, 1)

	// One consumed one-shot admission per resource, no more.
	f.wantCounts(t, "after install", installAPICounts{operations: 1, resources: 4, stage: 1, actions: 3})
	f.wantKeyNotAtRest(t)
}

// ---------------------------------------------------------------------------
// 2. Exact retry after completion
// ---------------------------------------------------------------------------

func TestProbeInstallAPIExactRetryAfterCompletionReturnsSameReceiptWithoutNewSession(t *testing.T) {
	f := newInstallAPIFixture(t, installAPIOptions{})
	_, finalBody := f.installToReady(t, installAPIOpA)
	f.wantInstalledEffects(t, installAPIOpA)
	f.wantServerRanFullInstall(t, 1)
	before := f.server.view()
	beforeCounts := f.counts(t)
	beforeCalls := f.tree.sysctl.recorded()

	for _, authorization := range []string{"Bearer " + f.adminToken, "ApiKey " + f.writeKey} {
		rec := f.call(t, f.handler, http.MethodPost, installAPIPath, authorization, f.body(t, installAPIOpA))
		if rec.Code != http.StatusOK {
			t.Fatalf("exact retry: %d %s", rec.Code, rec.Body.String())
		}
		if rec.Body.String() != finalBody {
			t.Fatalf("exact retry changed the receipt:\n got %s\nwant %s", rec.Body.String(), finalBody)
		}
		installAPIView(t, rec)
		f.wantNoDisclosure(t, "exact retry", rec.Body.String())
	}

	after := f.server.view()
	if after.conns != before.conns || !slices.Equal(after.commands, before.commands) || !slices.Equal(after.applies, before.applies) {
		t.Fatalf("exact retry opened an SSH session or exec: before %+v after %+v", before, after)
	}
	if got := f.counts(t); got != beforeCounts {
		t.Fatalf("exact retry changed durable rows: %+v -> %+v", beforeCounts, got)
	}
	if calls := f.tree.sysctl.recorded(); !slices.EqualFunc(calls, beforeCalls, slices.Equal[[]string]) {
		t.Fatalf("exact retry reached systemctl: %v", calls)
	}
	// The retry's own session (key parse + artifact snapshot) was disposed.
	f.wantScratchEmpty(t, "after retries")
	if rec := f.asAdmin(t, http.MethodGet, installAPIPath+"/"+installAPIOpA, ""); rec.Body.String() != finalBody {
		t.Fatal("receipt differs after retries")
	}
	f.wantInstalledEffects(t, installAPIOpA)
}

// ---------------------------------------------------------------------------
// 3. Changed identity conflicts
// ---------------------------------------------------------------------------

func TestProbeInstallAPIChangedIdentityConflictsWithoutAnyExec(t *testing.T) {
	f := newInstallAPIFixture(t, installAPIOptions{})
	_, finalBody := f.installToReady(t, installAPIOpA)
	f.wantServerRanFullInstall(t, 1)
	before := f.server.view()
	beforeCounts := f.counts(t)

	// A second, different pin the operator also placed in the artifact directory.
	armData := installAPIELFData(t, "arm64")
	armPin := helperEffectIntent(t, armData, "arm64").Identity.Binary
	if err := os.WriteFile(filepath.Join(f.artifactDir, armPin.ReleaseName()), armData, 0o600); err != nil {
		t.Fatal(err)
	}
	_, otherSigner := authzE2EKey(t)

	cases := []struct {
		name          string
		authorization string
		body          string
	}{
		{"different pin", "Bearer " + f.adminToken, f.body(t, installAPIOpA, func(m map[string]any) {
			m["binary_architecture"], m["binary_sha256"], m["binary_size"] = armPin.Architecture, armPin.SHA256, armPin.Size
		})},
		{"different host", "Bearer " + f.adminToken, f.body(t, installAPIOpA, func(m map[string]any) { m["ssh_address"] = "127.0.0.1:1" })},
		{"different host key pin", "Bearer " + f.adminToken, f.body(t, installAPIOpA, func(m map[string]any) {
			m["host_key_fingerprint"] = ssh.FingerprintSHA256(otherSigner.PublicKey())
		})},
		{"different deployment key", "Bearer " + f.adminToken, f.body(t, installAPIOpA, func(m map[string]any) {
			pemBytes, _ := authzE2EKey(t)
			m["private_key"] = string(pemBytes)
		})},
		{"different actor", "Bearer " + func() string {
			token, err := auth.NewJWTAuthenticator(strings.Repeat("m8-install-api-", 5), 1, f.users).IssueSession(context.Background(), f.adminTwoID)
			if err != nil {
				t.Fatal(err)
			}
			return token
		}(), f.body(t, installAPIOpA)},
	}
	for _, tc := range cases {
		rec := f.call(t, f.handler, http.MethodPost, installAPIPath, tc.authorization, tc.body)
		installAPIWantError(t, tc.name, rec, http.StatusConflict, "install_conflict")
		f.wantNoDisclosure(t, tc.name, rec.Body.String())
	}

	after := f.server.view()
	if after.conns != before.conns || !slices.Equal(after.commands, before.commands) {
		t.Fatalf("a conflicting request reached SSH: before %+v after %+v", before, after)
	}
	if got := f.counts(t); got != beforeCounts {
		t.Fatalf("a conflicting request changed durable rows: %+v -> %+v", beforeCounts, got)
	}
	if rec := f.asAdmin(t, http.MethodGet, installAPIPath+"/"+installAPIOpA, ""); rec.Body.String() != finalBody {
		t.Fatalf("the stored receipt changed after conflicts: %s", rec.Body.String())
	}
	f.wantScratchEmpty(t, "after conflicts")
	f.wantInstalledEffects(t, installAPIOpA)
}

// ---------------------------------------------------------------------------
// 4. Artifact rejection creates nothing durable
// ---------------------------------------------------------------------------

func TestProbeInstallAPIRejectedArtifactCreatesNothingDurable(t *testing.T) {
	f := newInstallAPIFixture(t, installAPIOptions{skipArtifact: true})
	release := filepath.Join(f.artifactDir, f.pin.ReleaseName())

	assertNothing := func(label string) {
		t.Helper()
		f.wantCounts(t, label, installAPICounts{})
		installAPIWantError(t, label+" GET", f.asAdmin(t, http.MethodGet, installAPIPath+"/"+installAPIOpA, ""), http.StatusNotFound, "install_not_found")
		if v := f.server.view(); v.conns != 0 || len(v.commands) != 0 {
			t.Fatalf("%s: SSH was contacted: %+v", label, v)
		}
		f.wantScratchEmpty(t, label)
		f.wantHostUntouched(t, label)
	}
	submitRejected := func(label string) {
		t.Helper()
		rec := f.post(t, installAPIOpA)
		installAPIWantError(t, label, rec, http.StatusUnprocessableEntity, "artifact_rejected")
		f.wantNoDisclosure(t, label, rec.Body.String())
		assertNothing(label)
	}

	// The pinned release file is missing.
	submitRejected("missing artifact")

	// The file exists but its bytes do not match the pinned digest.
	corrupt := slices.Clone(f.data)
	corrupt[len(corrupt)/2] ^= 0xff
	if err := os.WriteFile(release, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	submitRejected("digest mismatch")

	// The pinned size is right but the file is truncated.
	if err := os.WriteFile(release, f.data[:len(f.data)-1], 0o600); err != nil {
		t.Fatal(err)
	}
	submitRejected("truncated artifact")
	if err := os.Remove(release); err != nil {
		t.Fatal(err)
	}

	// A symlink leaving the artifact directory is never followed, even to a
	// perfectly valid binary.
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, f.data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, release); err != nil {
		t.Fatal(err)
	}
	submitRejected("symlink escaping the artifact directory")
	if err := os.Remove(release); err != nil {
		t.Fatal(err)
	}

	// A directory is not a release file.
	if err := os.Mkdir(release, 0o700); err != nil {
		t.Fatal(err)
	}
	submitRejected("directory instead of a file")
	if err := os.Remove(release); err != nil {
		t.Fatal(err)
	}

	// Nothing durable survived any rejection, so the very same operation id is
	// still free: once the operator places the real file it installs normally.
	if err := os.WriteFile(release, f.data, 0o600); err != nil {
		t.Fatal(err)
	}
	f.installToReady(t, installAPIOpA)
	f.wantInstalledEffects(t, installAPIOpA)
	f.wantServerRanFullInstall(t, 1)
}

// ---------------------------------------------------------------------------
// 4b. Malformed submissions are refused before any durable work
// ---------------------------------------------------------------------------

func TestProbeInstallAPIRejectsMalformedSubmissionsBeforeAnyDurableWork(t *testing.T) {
	f := newInstallAPIFixture(t, installAPIOptions{})
	base := f.body(t, installAPIOpA)
	set := func(key string, value any) string {
		return f.body(t, installAPIOpA, func(m map[string]any) { m[key] = value })
	}
	cases := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{"not json", "{", 400, "invalid_install"},
		{"empty body", "", 400, "invalid_install"},
		{"unknown member", base[:len(base)-1] + `,"requested_by":1}`, 400, "invalid_install"},
		{"duplicate member", base[:len(base)-1] + `,"ssh_user":"root"}`, 400, "invalid_install"},
		{"case-variant member", strings.Replace(base, `"ssh_user"`, `"SSH_user"`, 1), 400, "invalid_install"},
		{"null member", set("private_key", nil), 400, "invalid_install"},
		{"mistyped size", set("binary_size", "1"), 400, "invalid_install"},
		{"trailing json", base + `{}`, 400, "invalid_install"},
		{"missing member", strings.Replace(base, `"binary_version"`, `"binary_versio"`, 1), 400, "invalid_install"},
		{"non-root user", set("ssh_user", "ubuntu"), 400, "invalid_install"},
		{"bad operation id", set("operation_id", "not-a-uuid"), 400, "invalid_install"},
		{"bad host fingerprint", set("host_key_fingerprint", "SHA256:abc"), 400, "invalid_install"},
		{"empty key", set("private_key", ""), 400, "invalid_install"},
		{"unparseable key", set("private_key", "not an ssh private key"), 400, "invalid_install"},
		{"zero size", set("binary_size", 0), 400, "invalid_install"},
		{"uppercase digest", set("binary_sha256", strings.ToUpper(f.pin.SHA256)), 400, "invalid_install"},
		{"unsupported architecture", set("binary_architecture", "riscv64"), 400, "invalid_install"},
		{"body over the limit", strings.Repeat("x", (128<<10)+1), 413, "install_too_large"},
	}
	for _, tc := range cases {
		rec := f.asAdmin(t, http.MethodPost, installAPIPath, tc.body)
		installAPIWantError(t, tc.name, rec, tc.status, tc.code)
		if strings.Contains(rec.Body.String(), "not an ssh private key") {
			t.Fatalf("%s: the rejected key text was echoed", tc.name)
		}
	}
	// The same malformed identifier on the read and cancel routes.
	installAPIWantError(t, "GET bad id", f.asAdmin(t, http.MethodGet, installAPIPath+"/not-a-uuid", ""), 400, "invalid_operation_id")
	installAPIWantError(t, "cancel bad id", f.asAdmin(t, http.MethodPost, installAPIPath+"/not-a-uuid/cancel", ""), 400, "invalid_operation_id")
	installAPIWantError(t, "cancel with payload", f.asAdmin(t, http.MethodPost, installAPIPath+"/"+installAPIOpA+"/cancel", "{}"), 400, "invalid_request")
	installAPIWantError(t, "GET unknown", f.asAdmin(t, http.MethodGet, installAPIPath+"/"+installAPIOpB, ""), 404, "install_not_found")
	installAPIWantError(t, "cancel unknown", f.asAdmin(t, http.MethodPost, installAPIPath+"/"+installAPIOpB+"/cancel", ""), 404, "install_not_found")

	f.wantCounts(t, "after malformed input", installAPICounts{})
	if v := f.server.view(); v.conns != 0 || len(v.commands) != 0 {
		t.Fatalf("malformed input reached SSH: %+v", v)
	}
	f.wantScratchEmpty(t, "after malformed input")
	f.wantHostUntouched(t, "after malformed input")
}

// ---------------------------------------------------------------------------
// 5. Authentication, authorization and availability gates
// ---------------------------------------------------------------------------

func TestProbeInstallAPIAuthGatesAndDisabledSurface(t *testing.T) {
	f := newInstallAPIFixture(t, installAPIOptions{})
	routes := []struct{ method, path, body string }{
		{http.MethodPost, installAPIPath, f.body(t, installAPIOpA)},
		{http.MethodGet, installAPIPath + "/" + installAPIOpA, ""},
		{http.MethodPost, installAPIPath + "/" + installAPIOpA + "/cancel", ""},
	}
	principals := []struct {
		name          string
		authorization string
		status        int
	}{
		{"unauthenticated", "", http.StatusUnauthorized},
		{"garbage bearer", "Bearer not-a-token", http.StatusUnauthorized},
		{"non-admin session", "Bearer " + f.viewerToken, http.StatusForbidden},
		{"read-scope admin key", "ApiKey " + f.readKey, http.StatusUnauthorized},
		{"write-scope non-admin key", "ApiKey " + f.viewerKey, http.StatusForbidden},
	}
	for _, route := range routes {
		for _, p := range principals {
			rec := f.call(t, f.handler, route.method, route.path, p.authorization, route.body)
			if rec.Code != p.status {
				t.Fatalf("%s %s as %s: %d, want %d (%s)", route.method, route.path, p.name, rec.Code, p.status, rec.Body.String())
			}
			f.wantNoDisclosure(t, p.name, rec.Body.String())
		}
	}
	f.wantCounts(t, "after refused principals", installAPICounts{})
	if v := f.server.view(); v.conns != 0 || len(v.commands) != 0 {
		t.Fatalf("an unauthorized request reached SSH: %+v", v)
	}
	f.wantScratchEmpty(t, "after refused principals")

	// Disabled, unwired and typed-nil surfaces answer a typed 503 for an admin
	// and never create work or contact SSH.
	var typedNil *services.ProbeInstallOperationService
	unavailable := []struct {
		name string
		h    http.Handler
		code string
	}{
		{"disabled flag", f.router(handlers.NewProbeInstallHandlers(f.svc, false)), "installs_disabled"},
		{"no handlers wired", f.router(nil), "installs_disabled"},
		{"nil service", f.router(handlers.NewProbeInstallHandlers(nil, true)), "install_unavailable"},
		{"typed nil service", f.router(handlers.NewProbeInstallHandlers(typedNil, true)), "install_unavailable"},
	}
	for _, u := range unavailable {
		for _, route := range routes {
			rec := f.call(t, u.h, route.method, route.path, "Bearer "+f.adminToken, route.body)
			installAPIWantError(t, u.name+" "+route.method+" "+route.path, rec, http.StatusServiceUnavailable, u.code)
			if !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
				t.Fatalf("%s: unavailable response is not JSON: %q", u.name, rec.Header().Get("Content-Type"))
			}
		}
		// The admin gate still precedes the availability answer.
		if rec := f.call(t, u.h, http.MethodGet, routes[1].path, "Bearer "+f.viewerToken, ""); rec.Code != http.StatusForbidden {
			t.Fatalf("%s: non-admin saw %d", u.name, rec.Code)
		}
		if rec := f.call(t, u.h, http.MethodGet, routes[1].path, "", ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: anonymous saw %d", u.name, rec.Code)
		}
	}
	f.wantCounts(t, "after unavailable surfaces", installAPICounts{})
	if v := f.server.view(); v.conns != 0 || len(v.commands) != 0 {
		t.Fatalf("an unavailable surface reached SSH: %+v", v)
	}

	// The enabled router still works for the same admin afterwards.
	if rec := f.post(t, installAPIOpA); rec.Code != http.StatusAccepted {
		t.Fatalf("enabled router after gates: %d %s", rec.Code, rec.Body.String())
	}
	f.awaitStatus(t, installAPIOpA, installAPIWait, domain.ProbeInstallResourcesReady)
}

// ---------------------------------------------------------------------------
// 6. Cancellation
// ---------------------------------------------------------------------------

// closeAndJoin joins every run through the service and returns the stored
// receipt: after Close returns no run goroutine remains.
func (f *installAPIFixture) closeAndJoin(t *testing.T, op string) *domain.ProbeInstallOperation {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := f.svc.Close(ctx); err != nil {
		t.Fatalf("service close: %v", err)
	}
	return f.stored(t, op)
}

func TestProbeInstallAPICancelWhileFirstResourceActionIsHeld(t *testing.T) {
	f := newInstallAPIFixture(t, installAPIOptions{holdApply: "data_directory"})
	f.wantAccepted(t, "submit", f.post(t, installAPIOpA), installAPIOpA)
	if label := f.server.awaitHeld(t); label != "data_directory" {
		t.Fatalf("held %q, want data_directory", label)
	}

	// The run holds its private artifact snapshot while it is in flight.
	if entries, err := os.ReadDir(f.scratch); err != nil || len(entries) == 0 {
		t.Fatalf("no private snapshot while the run is held: %v %v", entries, err)
	}
	// The first resource is in flight on the host, the rest untouched.
	running := installAPIView(t, f.asAdmin(t, http.MethodGet, installAPIPath+"/"+installAPIOpA, ""))
	if running.Status != domain.ProbeInstallRunning {
		t.Fatalf("held status %q, want running", running.Status)
	}
	installAPIWantShape(t, "held", running, "data_directory/in_flight/-", "artifact_directory/planned/-", "unit_file/planned/-", "service/planned/-")

	// An exact retry of in-progress work answers 202 with the running receipt and
	// dispatches nothing: still exactly one SSH session and one held action.
	retry := f.post(t, installAPIOpA)
	if view := f.wantAccepted(t, "in-progress exact retry", retry, installAPIOpA); view.Status != domain.ProbeInstallRunning {
		t.Fatalf("in-progress exact retry status %q, want running", view.Status)
	}
	if v := f.server.view(); v.conns != 1 || len(v.commands) != 1 || len(v.applies) != 1 {
		t.Fatalf("in-progress exact retry reached SSH: %+v", v)
	}

	// Cancel persists intent and answers 202; it never claims remote work stopped.
	rec := f.asAdmin(t, http.MethodPost, installAPIPath+"/"+installAPIOpA+"/cancel", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body.String())
	}
	requested := installAPIView(t, rec)
	if requested.Status != domain.ProbeInstallCancelRequested || requested.Error != nil {
		t.Fatalf("cancel receipt %+v", requested)
	}
	f.wantNoDisclosure(t, "cancel", rec.Body.String())
	// A repeated cancel is idempotent while the owner has not joined.
	if rec = f.asAdmin(t, http.MethodPost, installAPIPath+"/"+installAPIOpA+"/cancel", ""); rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
		t.Fatalf("repeated cancel: %d %s", rec.Code, rec.Body.String())
	}

	// The runner's watcher observes the intent, cancels the in-flight action and
	// records what it truthfully knows. Records the actual end state.
	final, finalBody := f.awaitStatus(t, installAPIOpA, 30*time.Second, domain.ProbeInstallCanceled, domain.ProbeInstallInterrupted)
	t.Logf("end state after cancel with the first action held: status=%s error=%+v resources=%q", final.Status, final.Error, installAPIShape(final))
	f.wantNoDisclosure(t, "final", finalBody)
	f.server.releaseAll()
	f.server.awaitCodes(t, 1)
	stored := f.closeAndJoin(t, installAPIOpA)
	if stored.State != final.Status {
		t.Fatalf("stored state %s differs from the observed receipt %s", stored.State, final.Status)
	}
	// Cancel is intent, not a claim that remote work stopped: the held remote
	// action completed on the host after the control plane had already recorded
	// its outcome as unknown.
	if info, err := os.Lstat(f.tree.dataPath(installAPIOpA)); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("the released data_directory action left no 0700 directory: %v %v", info, err)
	}

	// No later resource ran: one exec in total, nothing staged, no unit, no systemctl.
	v := f.server.view()
	if v.conns != 1 || !slices.Equal(v.commands, []string{provisioning.SSHProbeHostActionCommand}) || !slices.Equal(v.applies, []string{"data_directory"}) {
		t.Fatalf("server saw conns=%d commands=%q applies=%q, want exactly the held data_directory action", v.conns, v.commands, v.applies)
	}
	hactWantEmptyDirs(t, f.stageParent, f.tree.unitDir)
	if calls := f.tree.sysctl.recorded(); len(calls) != 0 {
		t.Fatalf("a later resource reached systemctl: %v", calls)
	}
	f.wantScratchEmpty(t, "after cancel")
	if got := f.counts(t); got.stage != 0 || got.actions != 1 || got.operations != 1 {
		t.Fatalf("durable rows after cancel: %+v", got)
	}
	// A settled receipt is returned unchanged by cancel and by an exact retry.
	if rec = f.asAdmin(t, http.MethodPost, installAPIPath+"/"+installAPIOpA+"/cancel", ""); rec.Code != http.StatusOK || rec.Body.String() != finalBody {
		t.Fatalf("cancel of a settled receipt: %d %s", rec.Code, rec.Body.String())
	}
}

func TestProbeInstallAPICancelBeforeSecondResourceChallengeEndsCanceled(t *testing.T) {
	f := newInstallAPIFixture(t, installAPIOptions{holdStageChallenge: true})
	f.wantAccepted(t, "submit", f.post(t, installAPIOpA), installAPIOpA)
	if label := f.server.awaitHeld(t); label != "stage_challenge" {
		t.Fatalf("held %q, want stage_challenge", label)
	}
	// data_directory is already applied; artifact_directory is in flight but no
	// byte has reached the host.
	running := installAPIView(t, f.asAdmin(t, http.MethodGet, installAPIPath+"/"+installAPIOpA, ""))
	installAPIWantShape(t, "held", running, "data_directory/applied/-", "artifact_directory/in_flight/-", "unit_file/planned/-", "service/planned/-")

	rec := f.asAdmin(t, http.MethodPost, installAPIPath+"/"+installAPIOpA+"/cancel", "")
	if rec.Code != http.StatusAccepted || installAPIView(t, rec).Status != domain.ProbeInstallCancelRequested {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body.String())
	}
	final, _ := f.awaitStatus(t, installAPIOpA, 30*time.Second, domain.ProbeInstallCanceled, domain.ProbeInstallInterrupted)
	t.Logf("end state after cancel before the stage challenge: status=%s error=%+v resources=%q", final.Status, final.Error, installAPIShape(final))
	f.server.releaseAll()
	f.server.awaitCodes(t, 2)
	stored := f.closeAndJoin(t, installAPIOpA)
	if stored.State != final.Status {
		t.Fatalf("stored state %s differs from the observed receipt %s", stored.State, final.Status)
	}

	// Nothing reached the stage helper's stdin, so nothing was admitted or staged
	// and neither the unit nor the service ran.
	v := f.server.view()
	if !slices.Equal(v.commands, []string{provisioning.SSHProbeHostActionCommand, provisioning.SSHProbeBinaryStageAuthorizedCommand}) ||
		!slices.Equal(v.applies, []string{"data_directory"}) {
		t.Fatalf("server saw commands=%q applies=%q", v.commands, v.applies)
	}
	hactWantEmptyDirs(t, f.stageParent, f.tree.unitDir)
	if calls := f.tree.sysctl.recorded(); len(calls) != 0 {
		t.Fatalf("a later resource reached systemctl: %v", calls)
	}
	if got := f.counts(t); got.stage != 0 || got.actions != 1 {
		t.Fatalf("durable admissions after cancel: %+v", got)
	}
	f.wantScratchEmpty(t, "after cancel")
}

// ---------------------------------------------------------------------------
// 7. Close joins
// ---------------------------------------------------------------------------

func TestProbeInstallAPICloseJoinsHeldRunAndRecordsOwnerOutcome(t *testing.T) {
	f := newInstallAPIFixture(t, installAPIOptions{holdApply: "data_directory"})
	f.wantAccepted(t, "submit", f.post(t, installAPIOpA), installAPIOpA)
	f.server.awaitHeld(t)
	if entries, err := os.ReadDir(f.scratch); err != nil || len(entries) == 0 {
		t.Fatalf("no private snapshot while the run is held: %v %v", entries, err)
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- f.svc.Close(context.Background()) }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(30 * time.Second):
		f.server.releaseAll()
		t.Fatal("Close did not return within 30s while the remote action was held")
	}

	// Close returned, so the run joined: the receipt is already settled by the
	// owner, never left running, and the session's snapshot is gone.
	rec := f.asAdmin(t, http.MethodGet, installAPIPath+"/"+installAPIOpA, "")
	view := installAPIView(t, rec)
	t.Logf("end state after Close with the first action held: status=%s error=%+v resources=%q", view.Status, view.Error, installAPIShape(view))
	if rec.Code != http.StatusOK || !installAPISettled(view.Status) || view.Status == domain.ProbeInstallResourcesReady {
		t.Fatalf("receipt after Close: %d status=%s", rec.Code, view.Status)
	}
	if got := f.stored(t, installAPIOpA); got.State != view.Status {
		t.Fatalf("stored state %s, view %s", got.State, view.Status)
	}
	entries, err := os.ReadDir(f.scratch)
	if err != nil || len(entries) != 0 {
		t.Fatalf("Close returned before the session snapshot was disposed: %v %v", entries, err)
	}

	// The service is closed for good: new work and exact retries are refused,
	// reads still answer.
	installAPIWantError(t, "submit after Close", f.post(t, installAPIOpB), http.StatusServiceUnavailable, "installs_closed")
	installAPIWantError(t, "retry after Close", f.post(t, installAPIOpA), http.StatusServiceUnavailable, "installs_closed")
	if rec := f.asAdmin(t, http.MethodGet, installAPIPath+"/"+installAPIOpA, ""); rec.Code != http.StatusOK {
		t.Fatalf("GET after Close: %d", rec.Code)
	}
	if rec := f.asAdmin(t, http.MethodGet, installAPIPath+"/"+installAPIOpB, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("a refused submission left a receipt: %d", rec.Code)
	}

	// Releasing the held remote helper afterwards neither resumes the run nor
	// changes the receipt: only the in-flight action itself completes.
	settled := f.stored(t, installAPIOpA)
	f.server.releaseAll()
	f.server.awaitCodes(t, 1)
	if info, err := os.Lstat(f.tree.dataPath(installAPIOpA)); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("the released data_directory action left no 0700 directory: %v %v", info, err)
	}
	v := f.server.view()
	if v.conns != 1 || !slices.Equal(v.commands, []string{provisioning.SSHProbeHostActionCommand}) {
		t.Fatalf("a later resource ran after Close: %+v", v)
	}
	hactWantEmptyDirs(t, f.stageParent, f.tree.unitDir)
	if calls := f.tree.sysctl.recorded(); len(calls) != 0 {
		t.Fatalf("a later resource reached systemctl: %v", calls)
	}
	if after := f.stored(t, installAPIOpA); after.State != settled.State || !after.UpdatedAt.Equal(settled.UpdatedAt) {
		t.Fatalf("the receipt changed after Close: %s -> %s", settled.State, after.State)
	}
}

// ---------------------------------------------------------------------------
// 8. Capacity
// ---------------------------------------------------------------------------

func TestProbeInstallAPISaturationRefusesNewWorkButAnswersExactRetry(t *testing.T) {
	f := newInstallAPIFixture(t, installAPIOptions{holdApply: "data_directory"})
	f.wantAccepted(t, "first", f.post(t, installAPIOpA), installAPIOpA)
	f.wantAccepted(t, "second", f.post(t, installAPIOpB), installAPIOpB)
	f.server.awaitHeld(t)
	f.server.awaitHeld(t)
	before := f.server.view()

	// Capacity is 2: a third distinct request is Busy with a retry hint, and the
	// session built for it is disposed with no row.
	rec := f.post(t, installAPIOpC)
	installAPIWantError(t, "third install", rec, http.StatusServiceUnavailable, "installs_busy")
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("busy response without Retry-After")
	}
	installAPIWantError(t, "third GET", f.asAdmin(t, http.MethodGet, installAPIPath+"/"+installAPIOpC, ""), http.StatusNotFound, "install_not_found")
	f.wantCounts(t, "while saturated", installAPICounts{operations: 2, resources: 8, actions: 2})

	// An exact retry of committed work still answers; a changed identity does not.
	retry := f.post(t, installAPIOpA)
	if view := f.wantAccepted(t, "saturated exact retry", retry, installAPIOpA); view.Status != domain.ProbeInstallRunning {
		t.Fatalf("saturated exact retry status %q, want running", view.Status)
	}
	changed := f.asAdmin(t, http.MethodPost, installAPIPath, f.body(t, installAPIOpA, func(m map[string]any) { m["ssh_address"] = "127.0.0.1:1" }))
	installAPIWantError(t, "saturated changed identity", changed, http.StatusConflict, "install_conflict")
	if after := f.server.view(); after.conns != before.conns || len(after.commands) != len(before.commands) {
		t.Fatalf("saturated retries reached SSH: %+v -> %+v", before, after)
	}

	// Releasing the held actions lets both installs finish and frees the
	// slots. The refused request is then admitted past capacity, but the host
	// preflight now finds the two installs on this host and refuses it with a
	// definite 422 that creates nothing: no row, no SSH resource helper.
	f.server.releaseAll()
	f.awaitStatus(t, installAPIOpA, installAPIWait, domain.ProbeInstallResourcesReady)
	f.awaitStatus(t, installAPIOpB, installAPIWait, domain.ProbeInstallResourcesReady)
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec = f.post(t, installAPIOpC)
		if rec.Code != http.StatusServiceUnavailable {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("third install still busy after capacity freed: %s", rec.Body.String())
		}
		time.Sleep(installAPIPoll)
	}
	installAPIWantError(t, "third install on an occupied host", rec, http.StatusUnprocessableEntity, domain.ProbeInstallPreflightExisting)
	installAPIWantError(t, "refused third GET", f.asAdmin(t, http.MethodGet, installAPIPath+"/"+installAPIOpC, ""), http.StatusNotFound, "install_not_found")
	f.wantServerRanFullInstall(t, 2)
	f.wantCounts(t, "after the refused third install", installAPICounts{operations: 2, resources: 8, stage: 2, actions: 6})
	if names := entryNames(t, f.stageParent); len(names) != 2 {
		t.Fatalf("stage parent holds %v, want one directory per admitted install", names)
	}
	if v := f.server.view(); v.preflights != 3 {
		t.Fatalf("host preflights = %d, want one per admitted submission (A, B, C)", v.preflights)
	}
	f.wantKeyNotAtRest(t)
}

// ---------------------------------------------------------------------------
// 9. Production composition root
// ---------------------------------------------------------------------------

// TestProbeInstallAPIThroughProductionCompositionRoot drives the same install
// through newProbeInstallOperations: the stage authority seed and the endpoint
// policy are loaded from protected files exactly as at startup, the owner UUID,
// stores and capacity come from the composition root, and its Close joins the
// runs before it releases the artifact directory.
func TestProbeInstallAPIThroughProductionCompositionRoot(t *testing.T) {
	f := newInstallAPIFixture(t, installAPIOptions{viaComposition: true})
	f.installToReady(t, installAPIOpA)
	f.wantInstalledEffects(t, installAPIOpA)
	f.wantServerRanFullInstall(t, 1)
	f.wantCounts(t, "composition install", installAPICounts{operations: 1, resources: 4, stage: 1, actions: 3})
	f.wantKeyNotAtRest(t)

	closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := f.shutdown(closeCtx); err != nil {
		t.Fatalf("composition Close: %v", err)
	}
	// Closed for good: new work and exact retries are refused with a typed 503,
	// while the stored receipt stays readable.
	installAPIWantError(t, "submit after composition Close", f.post(t, installAPIOpB), http.StatusServiceUnavailable, "installs_closed")
	installAPIWantError(t, "retry after composition Close", f.post(t, installAPIOpA), http.StatusServiceUnavailable, "installs_closed")
	if view := installAPIView(t, f.asAdmin(t, http.MethodGet, installAPIPath+"/"+installAPIOpA, "")); view.Status != domain.ProbeInstallResourcesReady {
		t.Fatalf("receipt after composition Close: %s", view.Status)
	}
	f.wantScratchEmpty(t, "after composition Close")
}

// ---------------------------------------------------------------------------
// 10. Cancel of a resources_ready receipt leaves it unchanged
// ---------------------------------------------------------------------------

// resources_ready has no remaining installer work: cancel answers 200 with the
// settled receipt byte-identical and never demotes it to a cancel_requested
// that no runner would finish. The service reads before cancelling; the
// journal store's own cancellation semantics are unchanged.
func TestProbeInstallAPICancelOfResourcesReadyLeavesReceiptUnchanged(t *testing.T) {
	f := newInstallAPIFixture(t, installAPIOptions{})
	_, readyBody := f.installToReady(t, installAPIOpA)
	f.wantServerRanFullInstall(t, 1)

	rec := f.asAdmin(t, http.MethodPost, installAPIPath+"/"+installAPIOpA+"/cancel", "")
	if rec.Code != http.StatusOK || rec.Body.String() != readyBody {
		t.Fatalf("cancel of resources_ready: %d %s", rec.Code, rec.Body.String())
	}
	if got := f.stored(t, installAPIOpA); got.State != domain.ProbeInstallResourcesReady {
		t.Fatalf("stored state %s", got.State)
	}
	f.wantServerRanFullInstall(t, 1)
	if v := f.server.view(); len(v.commands) != 4 {
		t.Fatalf("cancel of a settled install reached SSH: %+v", v)
	}
}

// ---------------------------------------------------------------------------
// 10. Intentional cleanup through the production composition
// ---------------------------------------------------------------------------

const installAPICleanupID = "c9999999-9999-4999-8999-999999999999"

func (f *installAPIFixture) cleanupBody(t *testing.T, cleanupID string) string {
	t.Helper()
	out, err := json.Marshal(map[string]string{"cleanup_id": cleanupID, "private_key": string(f.server.clientPEM)})
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// awaitCleanup polls GET .../cleanup until a terminal status.
func (f *installAPIFixture) awaitCleanup(t *testing.T, op string) handlers.ProbeCleanupView {
	t.Helper()
	deadline := time.Now().Add(installAPIWait)
	for {
		rec := f.asAdmin(t, http.MethodGet, installAPIPath+"/"+op+"/cleanup", "")
		var view handlers.ProbeCleanupView
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
			switch view.Status {
			case domain.ProbeCleanupCleaned, domain.ProbeCleanupFailed, domain.ProbeCleanupInterrupted:
				return view
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("cleanup of %s never settled: %d %s", op, rec.Code, rec.Body.String())
		}
		time.Sleep(installAPIPoll)
	}
}

// TestProbeInstallAPICleanupRemovesTheInstallAndFreesTheHost drives a full
// install, then the cleanup route through the production composition: the
// real cleanup applier removes the unit (disabled first), the artifact
// directory and the data directory, after which the host preflight admits a
// new install on the same host. A second cleanup and an activation of the
// cleaned install are refused.
func TestProbeInstallAPICleanupRemovesTheInstallAndFreesTheHost(t *testing.T) {
	f := newInstallAPIFixture(t, installAPIOptions{viaComposition: true})
	f.installToReady(t, installAPIOpA)
	f.wantInstalledEffects(t, installAPIOpA)
	cleanupPath := installAPIPath + "/" + installAPIOpA + "/cleanup"

	if rec := f.asAdmin(t, http.MethodGet, cleanupPath, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("GET cleanup before any: %d %s", rec.Code, rec.Body.String())
	}
	// Non-admins never reach the cleanup surface.
	if rec := f.call(t, f.handler, http.MethodPost, cleanupPath, "Bearer "+f.viewerToken, f.cleanupBody(t, installAPICleanupID)); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer cleanup: %d", rec.Code)
	}
	rec := f.asAdmin(t, http.MethodPost, cleanupPath, f.cleanupBody(t, installAPICleanupID))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST cleanup: %d %s", rec.Code, rec.Body.String())
	}
	f.wantNoDisclosure(t, "cleanup submit", rec.Body.String())
	view := f.awaitCleanup(t, installAPIOpA)
	if view.Status != domain.ProbeCleanupCleaned || view.Error != nil || view.CleanupID != installAPICleanupID || view.OperationID != installAPIOpA {
		t.Fatalf("cleanup view %+v", view)
	}
	// Every resource of the operation is gone, and the unit was disabled
	// before its file was removed.
	hactWantEmptyDirs(t, f.stageParent, f.tree.varLib, f.tree.unitDir)
	calls := f.tree.sysctl.recorded()
	unit := f.tree.unitName(installAPIOpA)
	var verbs []string
	for _, call := range calls {
		verbs = append(verbs, strings.Join(call, " "))
	}
	wantTail := []string{"disable --now " + unit, "daemon-reload", "reset-failed " + unit}
	if len(verbs) < len(wantTail) || !slices.Equal(verbs[len(verbs)-len(wantTail):], wantTail) {
		t.Fatalf("systemctl calls %q, want the cleanup tail %q", verbs, wantTail)
	}

	// An exact retry returns the settled receipt; a new cleanup of a cleaned
	// install is refused, and so is activating it.
	if rec := f.asAdmin(t, http.MethodPost, cleanupPath, f.cleanupBody(t, installAPICleanupID)); rec.Code != http.StatusOK {
		t.Fatalf("exact cleanup retry: %d %s", rec.Code, rec.Body.String())
	}
	installAPIWantError(t, "second cleanup", f.asAdmin(t, http.MethodPost, cleanupPath, f.cleanupBody(t, "c8888888-8888-4888-8888-888888888888")),
		http.StatusConflict, "cleanup_ineligible")
	activate, err := json.Marshal(map[string]any{"private_key": string(f.server.clientPEM), "probe_key": "edge-a", "name": "Edge A", "location": "", "listen_port": 8443})
	if err != nil {
		t.Fatal(err)
	}
	installAPIWantError(t, "activation after cleanup", f.asAdmin(t, http.MethodPost, installAPIPath+"/"+installAPIOpA+"/activate", string(activate)),
		http.StatusConflict, "activation_conflict")

	// The host is free again: a new operation passes the preflight and installs.
	f.installToReady(t, installAPIOpB)
	if info, err := os.Lstat(f.tree.dataPath(installAPIOpB)); err != nil || !info.IsDir() {
		t.Fatalf("second install data directory: %v %v", info, err)
	}
	if _, err := os.Lstat(f.tree.unitPath(installAPIOpB)); err != nil {
		t.Fatalf("second install unit file: %v", err)
	}
	if names := entryNames(t, f.stageParent); len(names) != 1 {
		t.Fatalf("stage parent holds %v, want only the second install", names)
	}
	f.wantKeyNotAtRest(t)
}
