//go:build linux || darwin

package bootstrap

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
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// authzE2EAdmissions is a one-shot in-memory admission store: each attempt is
// consumed exactly once and never replenished, mirroring the stored contract.
type authzE2EAdmissions struct {
	mu       sync.Mutex
	consumed map[string]domain.ProbeBinaryStageAdmissionRequest
	calls    int
}

func (r *authzE2EAdmissions) AdmitBinaryStage(ctx context.Context, request domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageAdmission, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if err := ctx.Err(); err != nil {
		return domain.ProbeBinaryStageAdmission{}, err
	}
	if _, ok := r.consumed[request.Intent.AttemptID]; ok {
		return domain.ProbeBinaryStageAdmission{}, ports.ErrConflict
	}
	r.consumed[request.Intent.AttemptID] = request
	return domain.ProbeBinaryStageAdmission{Request: request, AdmittedAt: time.Now().UTC().Truncate(time.Microsecond)}, nil
}

func (r *authzE2EAdmissions) count() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, len(r.consumed)
}

// authzE2ETamper passes requests through to the real service, then corrupts
// the returned signature so only the host-side verification can refuse it.
type authzE2ETamper struct {
	inner provisioning.ProbeBinaryStageAuthorizer
}

func (a authzE2ETamper) Authorize(ctx context.Context, request domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageGrant, error) {
	grant, err := a.inner.Authorize(ctx, request)
	grant.Signature[0] ^= 0x01
	return grant, err
}

type authzE2EInput struct{ ch ssh.Channel }

func (i authzE2EInput) Read(p []byte) (int, error) { return i.ch.Read(p) }

// Close leaves the channel open: the helper closes input before it writes
// its receipt on the same channel.
func (authzE2EInput) Close() error { return nil }

type authzE2EServer struct {
	address    string
	hostSigner ssh.Signer
	clientPEM  []byte
	clientPub  ssh.PublicKey
	trusted    [32]byte
	parentPath string

	mu       sync.Mutex
	commands []string
	stdinLen int
	codes    []int
	stderr   bytes.Buffer
	wg       sync.WaitGroup
}

func authzE2EKey(t *testing.T) ([]byte, ssh.Signer) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), signer
}

func serveAuthzE2E(t *testing.T, trusted [32]byte, parentPath string) *authzE2EServer {
	t.Helper()
	clientPEM, clientSigner := authzE2EKey(t)
	_, hostSigner := authzE2EKey(t)
	s := &authzE2EServer{hostSigner: hostSigner, clientPEM: clientPEM, clientPub: clientSigner.PublicKey(), trusted: trusted, parentPath: parentPath}
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
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.serve(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		done := make(chan struct{})
		go func() { s.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("authorized SSH fixture did not stop")
		}
	})
	return s
}

func (s *authzE2EServer) serve(conn net.Conn) {
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
			s.mu.Lock()
			s.commands = append(s.commands, exec.Command)
			s.mu.Unlock()
			code := 127
			var stderr bytes.Buffer
			counter := &authzE2ECounter{r: authzE2EInput{ch: ch}}
			if exec.Command == provisioning.SSHProbeBinaryStageAuthorizedCommand {
				code = runAuthorizedProbeBinaryStageHelper(context.Background(), nil, counter, ch, &stderr,
					func(context.Context) ([32]byte, error) { return s.trusted, nil },
					func(context.Context) (*os.File, error) { return os.Open(s.parentPath) },
					rand.Reader,
					func(context.Context) ([]string, error) {
						return []string{ssh.FingerprintSHA256(s.hostSigner.PublicKey())}, nil
					})
			}
			s.mu.Lock()
			s.codes = append(s.codes, code)
			s.stdinLen += counter.n
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

type authzE2ECounter struct {
	r io.ReadCloser
	n int
}

func (c *authzE2ECounter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func (c *authzE2ECounter) Close() error { return c.r.Close() }

func (s *authzE2EServer) snapshot() ([]string, []int, int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...), append([]int(nil), s.codes...), s.stdinLen, s.stderr.String()
}

type authzE2E struct {
	server   *authzE2EServer
	store    *authzE2EAdmissions
	service  *services.ProbeBinaryStageAuthorizationService
	sender   *provisioning.SSHBinaryStageSender
	intent   domain.ProbeBinaryStageIntent
	data     []byte
	parent   string
	scratch  string
	prepared ports.PreparedProbeBinary
}

// newAuthzE2E composes the real authorization service and Ed25519 signer, the
// real SSH sender, a Go SSH server running the real authorized helper, and a
// real temporary parent directory. hostKey is the key the helper trusts.
func newAuthzE2E(t *testing.T, authority ed25519.PrivateKey, hostKey [32]byte) *authzE2E {
	t.Helper()
	data := helperEffectFixture(t, "amd64")
	parent, _, scratch := helperEffectParent(t)
	server := serveAuthzE2E(t, hostKey, parent)
	signer, err := provisioning.NewEd25519ProbeStageGrantSigner(authority)
	if err != nil {
		t.Fatal(err)
	}
	store := &authzE2EAdmissions{consumed: map[string]domain.ProbeBinaryStageAdmissionRequest{}}
	service, err := services.NewProbeBinaryStageAuthorizationService(store, signer)
	if err != nil {
		t.Fatal(err)
	}
	intent := helperEffectIntent(t, data, "amd64")
	intent.Identity.Host = domain.ProbeSSHHost{Address: server.address, User: "root"}
	intent.Identity.HostKeyFingerprint = ssh.FingerprintSHA256(server.hostSigner.PublicKey())
	intent.Identity.ClientKeyFingerprint = ssh.FingerprintSHA256(server.clientPub)
	if !intent.Valid() {
		t.Fatal("root intent invalid")
	}
	policy := probe.EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
	sender, err := provisioning.NewSSHBinaryStageSender(t.Context(), server.clientPEM, intent.Identity.HostKeyFingerprint, intent.Identity.ClientKeyFingerprint, policy)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := provisioning.NewProbeBinaryPreparer(t.TempDir()).PrepareBinary(t.Context(), intent.Identity.Binary, io.NopCloser(bytes.NewReader(data)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = prepared.Close(context.Background()) })
	return &authzE2E{server: server, store: store, service: service, sender: sender, intent: intent, data: data, parent: parent, scratch: scratch, prepared: prepared}
}

func authzE2EAuthority(t *testing.T) (ed25519.PrivateKey, [32]byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var key [32]byte
	copy(key[:], pub)
	return priv, key
}

func TestAuthorizedProbeBinaryStageSSHEndToEnd(t *testing.T) {
	authority, trusted := authzE2EAuthority(t)
	e := newAuthzE2E(t, authority, trusted)

	ack, err := e.sender.SendAuthorized(t.Context(), e.intent, e.prepared, e.service)
	if err != nil || ack.Intent != e.intent {
		t.Fatalf("authorized send ack=%v err=%v", ack.Intent == e.intent, err)
	}
	helperEffectGraph(t, e.parent, e.intent, e.data)
	helperEffectEmpty(t, e.scratch)
	commands, codes, stdinLen, stderr := e.server.snapshot()
	header, _ := provisioning.EncodeProbeBinaryStageHeader(e.intent)
	if len(commands) != 1 || commands[0] != provisioning.SSHProbeBinaryStageAuthorizedCommand || len(codes) != 1 || codes[0] != 0 ||
		stdinLen != len(header)+provisioning.ProbeBinaryStageGrantFrameSize+len(e.data) || stderr != "" {
		t.Fatalf("helper commands=%q codes=%v stdin=%d stderr=%q", commands, codes, stdinLen, stderr)
	}
	if calls, consumed := e.store.count(); calls != 1 || consumed != 1 {
		t.Fatalf("admissions calls=%d consumed=%d", calls, consumed)
	}

	// Replaying the same attempt is refused by the consumed admission before
	// any stdin byte reaches the new helper process; the stage is unchanged.
	ack, err = e.sender.SendAuthorized(t.Context(), e.intent, e.prepared, e.service)
	if !errors.Is(err, provisioning.ErrSSHBinaryStageNotStaged) || ack != (provisioning.SSHBinaryStageAck{}) {
		t.Fatalf("replayed attempt ack=%v err=%v", ack, err)
	}
	helperEffectGraph(t, e.parent, e.intent, e.data)
	_, codes, stdinLen, _ = e.server.snapshot()
	if len(codes) != 2 || codes[1] == 0 || stdinLen != len(header)+provisioning.ProbeBinaryStageGrantFrameSize+len(e.data) {
		t.Fatalf("replay reached helper input codes=%v stdin=%d", codes, stdinLen)
	}
	if calls, consumed := e.store.count(); calls != 2 || consumed != 1 {
		t.Fatalf("replay admissions calls=%d consumed=%d", calls, consumed)
	}
}

func TestAuthorizedProbeBinaryStageSSHRefusesTamperedGrant(t *testing.T) {
	authority, trusted := authzE2EAuthority(t)
	e := newAuthzE2E(t, authority, trusted)

	ack, err := e.sender.SendAuthorized(t.Context(), e.intent, e.prepared, authzE2ETamper{inner: e.service})
	if !errors.Is(err, provisioning.ErrSSHBinaryStageUnknown) || ack != (provisioning.SSHBinaryStageAck{}) {
		t.Fatalf("tampered grant ack=%v err=%v", ack, err)
	}
	helperEffectEmpty(t, e.parent, e.scratch)
	_, codes, _, stderr := e.server.snapshot()
	if len(codes) != 1 || codes[0] != 1 || stderr != domain.ErrProbeBinaryStageWriteUnauthorized.Error()+"\n" {
		t.Fatalf("tampered grant codes=%v stderr=%q", codes, stderr)
	}
	if _, consumed := e.store.count(); consumed != 1 {
		t.Fatalf("tampered grant consumed=%d", consumed)
	}
}

func TestAuthorizedProbeBinaryStageSSHRefusesUntrustedAuthority(t *testing.T) {
	authority, _ := authzE2EAuthority(t)
	_, other := authzE2EAuthority(t)
	e := newAuthzE2E(t, authority, other)

	// The host trusts a different authority: the service refuses the
	// challenge before admission, so no attempt is consumed and no stdin sent.
	ack, err := e.sender.SendAuthorized(t.Context(), e.intent, e.prepared, e.service)
	if !errors.Is(err, provisioning.ErrSSHBinaryStageNotStaged) || ack != (provisioning.SSHBinaryStageAck{}) {
		t.Fatalf("untrusted authority ack=%v err=%v", ack, err)
	}
	helperEffectEmpty(t, e.parent, e.scratch)
	_, codes, stdinLen, _ := e.server.snapshot()
	if len(codes) != 1 || codes[0] == 0 || stdinLen != 0 {
		t.Fatalf("untrusted authority codes=%v stdin=%d", codes, stdinLen)
	}
	if calls, consumed := e.store.count(); calls != 0 || consumed != 0 {
		t.Fatalf("untrusted authority admissions calls=%d consumed=%d", calls, consumed)
	}
}
