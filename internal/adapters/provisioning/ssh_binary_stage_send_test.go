package provisioning

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// sendTFixture is a real loopback SSH protocol server. It is protocol and
// lifetime evidence only: no OpenSSH shell, installed helper, host key, agent,
// home directory or privileged service is consulted. Generated test keys only.
type sendTFixture struct {
	cfg        sendTConfig
	address    string
	hostSigner ssh.Signer
	clientKey  []byte
	clientPub  ssh.PublicKey

	connCalls atomic.Int32
	authCalls atomic.Int32
	execCalls atomic.Int32

	mu          sync.Mutex
	reqs        []string
	execs       []string
	stdin       bytes.Buffer
	stdinEOF    bool
	closing     bool
	connections map[net.Conn]struct{}

	wg sync.WaitGroup
}

type sendTConfig struct {
	stdout           func(f *sendTFixture) string
	stderr           string
	exit             *uint32
	signal           string
	noExit           bool
	stallHandshake   bool
	holdAfterSession bool
	handle           func(f *sendTFixture, ch ssh.Channel, command string, dead <-chan struct{})
}

// sendTExit returns one fixed nonzero exit status for the fixture.
func sendTExit() *uint32 {
	v := uint32(3)
	return &v
}

func serveStageSend(t *testing.T, cfg sendTConfig) *sendTFixture {
	t.Helper()
	clientKey, clientSigner := deploymentKey(t)
	_, hostSigner := deploymentKey(t)
	f := &sendTFixture{
		cfg:         cfg,
		hostSigner:  hostSigner,
		clientKey:   clientKey,
		clientPub:   clientSigner.PublicKey(),
		connections: make(map[net.Conn]struct{}),
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.address = listener.Addr().String()
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			if f.closing {
				f.mu.Unlock()
				_ = conn.Close()
				return
			}
			f.connections[conn] = struct{}{}
			f.mu.Unlock()
			f.connCalls.Add(1)
			f.wg.Add(1)
			go func() {
				defer f.wg.Done()
				defer func() { f.mu.Lock(); delete(f.connections, conn); f.mu.Unlock() }()
				f.serveConn(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		f.mu.Lock()
		f.closing = true
		for conn := range f.connections {
			_ = conn.Close()
		}
		f.mu.Unlock()
		done := make(chan struct{})
		go func() {
			defer close(done)
			f.wg.Wait()
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("send fixture failed to stop")
		}
	})
	return f
}

func (f *sendTFixture) serveConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	if f.cfg.stallHandshake {
		_, _ = io.Copy(io.Discard, conn)
		return
	}
	config := &ssh.ServerConfig{PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		f.authCalls.Add(1)
		if meta.User() != "root" || !bytes.Equal(key.Marshal(), f.clientPub.Marshal()) {
			return nil, errors.New("authentication rejected")
		}
		return nil, nil
	}}
	config.AddHostKey(f.hostSigner)
	server, channels, requests, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	defer func() { _ = server.Close() }()
	f.wg.Add(2)
	go func() { defer f.wg.Done(); ssh.DiscardRequests(requests) }()
	dead := make(chan struct{})
	go func() {
		defer f.wg.Done()
		_ = server.Wait()
		close(dead)
	}()
	defer func() { _ = server.Close(); <-dead }()
	for newCh := range channels {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.Prohibited, "session only")
			continue
		}
		ch, reqs, err := newCh.Accept()
		if err != nil {
			return
		}
		f.serveSession(ch, reqs, dead)
		if f.cfg.holdAfterSession {
			<-dead
		}
		return
	}
}

func (f *sendTFixture) serveSession(ch ssh.Channel, reqs <-chan *ssh.Request, dead <-chan struct{}) {
	defer func() { _ = ch.Close() }()
	for req := range reqs {
		f.mu.Lock()
		f.reqs = append(f.reqs, req.Type)
		f.mu.Unlock()
		var exec struct{ Command string }
		if req.Type != "exec" || ssh.Unmarshal(req.Payload, &exec) != nil {
			_ = req.Reply(false, nil)
			continue
		}
		f.mu.Lock()
		f.execs = append(f.execs, exec.Command)
		f.mu.Unlock()
		f.execCalls.Add(1)
		_ = req.Reply(true, nil)
		handle := f.cfg.handle
		if handle == nil {
			handle = sendTDefaultExec
		}
		handle(f, ch, exec.Command, dead)
		return
	}
}

// sendTDefaultExec drains the exact stream to a real stdin EOF, then writes the
// configured receipt and exit status. It never fabricates a receipt from the
// received bytes.
func sendTDefaultExec(f *sendTFixture, ch ssh.Channel, _ string, _ <-chan struct{}) {
	_, err := io.Copy(sendTSink{f: f}, ch)
	if err == nil {
		f.mu.Lock()
		f.stdinEOF = true
		f.mu.Unlock()
	}
	if f.cfg.stdout != nil {
		_, _ = io.WriteString(ch, f.cfg.stdout(f))
	}
	_, _ = io.WriteString(ch.Stderr(), f.cfg.stderr)
	f.sendExit(ch)
}

func (f *sendTFixture) sendExit(ch ssh.Channel) {
	if f.cfg.noExit {
		_ = ch.Close()
		return
	}
	if f.cfg.signal != "" {
		_, _ = ch.SendRequest("exit-signal", false, ssh.Marshal(struct {
			Signal     string
			CoreDumped bool
			Error      string
			Lang       string
		}{f.cfg.signal, false, "", ""}))
		_ = ch.Close()
		return
	}
	status := uint32(0)
	if f.cfg.exit != nil {
		status = *f.cfg.exit
	}
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
	_ = ch.Close()
}

const sendTStdinMax = 1 << 20

type sendTSink struct{ f *sendTFixture }

func (s sendTSink) Write(p []byte) (int, error) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if s.f.stdin.Len()+len(p) > sendTStdinMax {
		return 0, errors.New("send fixture stdin bound")
	}
	return s.f.stdin.Write(p)
}

func (f *sendTFixture) stdinBytes() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.stdin.Bytes()...)
}

func (f *sendTFixture) stdinEOFSeen() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stdinEOF
}

func (f *sendTFixture) requestTypes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reqs)
}

func (f *sendTFixture) execCommands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.execs)
}

func (f *sendTFixture) hostFP() string { return ssh.FingerprintSHA256(f.hostSigner.PublicKey()) }

func (f *sendTFixture) clientFP() string { return ssh.FingerprintSHA256(f.clientPub) }

func (f *sendTFixture) policy() probe.EndpointPolicy {
	return probe.EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
}

func (f *sendTFixture) sender(t *testing.T) *SSHBinaryStageSender {
	t.Helper()
	sender, err := NewSSHBinaryStageSender(t.Context(), f.clientKey, f.hostFP(), f.clientFP(), f.policy())
	if err != nil {
		t.Fatal(err)
	}
	return sender
}

func (f *sendTFixture) intent(pin domain.ProbeBinaryPin) domain.ProbeBinaryStageIntent {
	return domain.ProbeBinaryStageIntent{
		Identity: domain.ProbeInstallIdentity{
			OperationID:          "11111111-1111-4111-8111-111111111111",
			Host:                 domain.ProbeSSHHost{Address: f.address, User: "root"},
			HostKeyFingerprint:   f.hostFP(),
			ClientKeyFingerprint: f.clientFP(),
			RequestedBy:          7,
			LayoutVersion:        domain.ProbeInstallLayoutVersion,
			Mode:                 domain.ProbeInstallSystemd,
			Binary:               pin,
		},
		OwnerID:   "22222222-2222-4222-8222-222222222222",
		AttemptID: "33333333-3333-4333-8333-333333333333",
	}
}

// sendTReceipt is one helper receipt: a flat JSON object plus one LF with the
// nine canonical keys bound to the intent. It is fixture data only.
func sendTReceipt(intent domain.ProbeBinaryStageIntent) string {
	pin := intent.Identity.Binary
	return fmt.Sprintf(
		`{"state":"local_binary_staged","directory_name":%q,"marker_sha256":%q,"pinned_version":%q,"version_source":"operator_pin","os":"linux","architecture":%q,"sha256":%q,"size":%d}`+"\n",
		domain.ProbeBinaryStageSeal{Intent: intent}.DirectoryName(),
		intent.MarkerSHA256(),
		pin.Version, pin.Architecture, pin.SHA256, pin.Size,
	)
}

// sendTReceiptFor builds the receipt for the fixture's own intent with pin.
func sendTReceiptFor(pin domain.ProbeBinaryPin) func(*sendTFixture) string {
	return func(f *sendTFixture) string { return sendTReceipt(f.intent(pin)) }
}

// sendTOtherOpReceipt builds a receipt for the fixture's intent with pin but
// a different operation id, so the directory and marker bindings mismatch.
func sendTOtherOpReceipt(f *sendTFixture, pin domain.ProbeBinaryPin) string {
	intent := f.intent(pin)
	intent.Identity.OperationID = "44444444-4444-4444-8444-444444444444"
	return sendTReceipt(intent)
}

func sendTPin(data []byte) domain.ProbeBinaryPin {
	sum := sha256.Sum256(data)
	return domain.ProbeBinaryPin{
		Version:      "1.2.3",
		Architecture: "amd64",
		SHA256:       hex.EncodeToString(sum[:]),
		Size:         int64(len(data)),
	}
}

// sendTFakeBinary is a borrowed snapshot: the sender must never call Close.
type sendTFakeBinary struct {
	pin            domain.ProbeBinaryPin
	open           func(ctx context.Context) (io.ReadCloser, error)
	opens          atomic.Int32
	snapshotCloses atomic.Int32
}

func (b *sendTFakeBinary) Pin() domain.ProbeBinaryPin { return b.pin }

func (b *sendTFakeBinary) Open(ctx context.Context) (io.ReadCloser, error) {
	b.opens.Add(1)
	return b.open(ctx)
}

func (b *sendTFakeBinary) Close(ctx context.Context) error {
	b.snapshotCloses.Add(1)
	return nil
}

func sendTBinary(pin domain.ProbeBinaryPin, open func(ctx context.Context) (io.ReadCloser, error)) *sendTFakeBinary {
	return &sendTFakeBinary{pin: pin, open: open}
}

func sendTBytesBinary(data []byte) *sendTFakeBinary {
	pin := sendTPin(data)
	return sendTBinary(pin, func(context.Context) (io.ReadCloser, error) {
		return sendTScripted(data, nil, io.EOF), nil
	})
}

type sendTReader struct {
	readFn  func(p []byte) (int, error)
	closeFn func() error
	closes  atomic.Int32
}

func (r *sendTReader) Read(p []byte) (int, error) { return r.readFn(p) }

func (r *sendTReader) Close() error {
	r.closes.Add(1)
	if r.closeFn != nil {
		return r.closeFn()
	}
	return nil
}

// sendTScripted serves data then tail, then repeats (0, final).
func sendTScripted(data, tail []byte, final error) *sendTReader {
	var mu sync.Mutex
	buf := append(append([]byte(nil), data...), tail...)
	off := 0
	return &sendTReader{readFn: func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		if off < len(buf) {
			n := copy(p, buf[off:])
			off += n
			return n, nil
		}
		return 0, final
	}}
}

func sendTCloseFailReader(data []byte) *sendTReader {
	r := sendTScripted(data, nil, io.EOF)
	r.closeFn = func() error { return errors.New("reader close failure") }
	return r
}

// sendTHeldClose blocks every Read until Close runs, and blocks Close until the
// test releases it. Closing unblocks Read only after the release.
type sendTHeldClose struct {
	started      chan struct{}
	closeStarted chan struct{}
	closed       chan struct{}
	release      chan struct{}
	reads        atomic.Int32
	closes       atomic.Int32
	once         sync.Once
	closeBegin   sync.Once
	closeEnd     sync.Once
}

func (r *sendTHeldClose) Read(p []byte) (int, error) {
	r.reads.Add(1)
	r.once.Do(func() { close(r.started) })
	<-r.closed
	return 0, io.ErrClosedPipe
}

func (r *sendTHeldClose) Close() error {
	r.closes.Add(1)
	r.closeBegin.Do(func() { close(r.closeStarted) })
	<-r.release
	r.closeEnd.Do(func() { close(r.closed) })
	return nil
}

type sendTNilContext struct{}

func (*sendTNilContext) Deadline() (time.Time, bool) { return time.Time{}, false }

func (*sendTNilContext) Done() <-chan struct{} { return nil }

func (*sendTNilContext) Err() error { return nil }

func (*sendTNilContext) Value(any) any { return nil }

type sendTResult struct {
	ack SSHBinaryStageAck
	err error
}

func sendTSendAsync(ctx context.Context, sender *SSHBinaryStageSender, intent domain.ProbeBinaryStageIntent, binary ports.PreparedProbeBinary) <-chan sendTResult {
	done := make(chan sendTResult, 1)
	go func() {
		ack, err := sender.Send(ctx, intent, binary)
		done <- sendTResult{ack: ack, err: err}
	}()
	return done
}

func sendTAwait(t *testing.T, done <-chan sendTResult, what string) sendTResult {
	t.Helper()
	select {
	case res := <-done:
		return res
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not return", what)
		return sendTResult{}
	}
}

func sendTExpectBlocked(t *testing.T, done <-chan sendTResult, what string) {
	t.Helper()
	select {
	case res := <-done:
		t.Fatalf("%s returned early: ack %+v err %v", what, res.ack, res.err)
	case <-time.After(250 * time.Millisecond):
	}
}

func sendTWaitSignal(t *testing.T, ready <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s not reached", what)
	}
}

func sendTWaitCount(t *testing.T, counter *atomic.Int32, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for counter.Load() < 1 {
		if time.Now().After(deadline) {
			t.Fatalf("%s not reached", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSSHBinaryStageSenderRefusesInvalidInput(t *testing.T) {
	content := []byte("phoenix pinned payload")
	binary := sendTBytesBinary(content)
	f := serveStageSend(t, sendTConfig{})
	sender := f.sender(t)
	intent := f.intent(binary.Pin())

	var nilSender *SSHBinaryStageSender
	if ack, err := nilSender.Send(t.Context(), intent, binary); err != ErrSSHBinaryStageInput || ack != (SSHBinaryStageAck{}) {
		t.Fatalf("nil sender %+v %v", ack, err)
	}
	var nilCtx *sendTNilContext
	if ack, err := sender.Send(nilCtx, intent, binary); err != ErrSSHBinaryStageInput || ack != (SSHBinaryStageAck{}) {
		t.Fatalf("typed nil ctx %+v %v", ack, err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if ack, err := sender.Send(canceled, intent, binary); err != context.Canceled || ack != (SSHBinaryStageAck{}) {
		t.Fatalf("canceled ctx %+v %v", ack, err)
	}
	var nilBinary *sendTFakeBinary
	if ack, err := sender.Send(t.Context(), intent, nilBinary); err != ErrSSHBinaryStageInput || ack != (SSHBinaryStageAck{}) {
		t.Fatalf("typed nil binary %+v %v", ack, err)
	}

	badMode := intent
	badMode.Identity.Mode = domain.ProbeInstallDocker
	if ack, err := sender.Send(t.Context(), badMode, binary); err != ErrSSHBinaryStageInput || ack != (SSHBinaryStageAck{}) {
		t.Fatalf("invalid intent %+v %v", ack, err)
	}
	badUser := intent
	badUser.Identity.Host.User = "deploy"
	if ack, err := sender.Send(t.Context(), badUser, binary); err != ErrSSHBinaryStageInput || ack != (SSHBinaryStageAck{}) {
		t.Fatalf("non-root user %+v %v", ack, err)
	}
	_, otherSigner := deploymentKey(t)
	otherFP := ssh.FingerprintSHA256(otherSigner.PublicKey())
	badHostPin := intent
	badHostPin.Identity.HostKeyFingerprint = otherFP
	if ack, err := sender.Send(t.Context(), badHostPin, binary); err != ErrSSHBinaryStageInput || ack != (SSHBinaryStageAck{}) {
		t.Fatalf("host pin binding %+v %v", ack, err)
	}
	badClientPin := intent
	badClientPin.Identity.ClientKeyFingerprint = otherFP
	if ack, err := sender.Send(t.Context(), badClientPin, binary); err != ErrSSHBinaryStageInput || ack != (SSHBinaryStageAck{}) {
		t.Fatalf("client pin binding %+v %v", ack, err)
	}

	other := sendTBytesBinary([]byte("other payload"))
	if ack, err := sender.Send(t.Context(), intent, other); err != ErrSSHBinaryStageInput || ack != (SSHBinaryStageAck{}) {
		t.Fatalf("pin mismatch %+v %v", ack, err)
	}
	brokenPin := sendTBytesBinary(content)
	brokenPin.pin.Size = 0
	if ack, err := sender.Send(t.Context(), intent, brokenPin); err != ErrSSHBinaryStageInput || ack != (SSHBinaryStageAck{}) {
		t.Fatalf("invalid pin %+v %v", ack, err)
	}
	if f.connCalls.Load() != 0 || f.authCalls.Load() != 0 || f.execCalls.Load() != 0 {
		t.Fatalf("refusal performed I/O: conn %d auth %d exec %d", f.connCalls.Load(), f.authCalls.Load(), f.execCalls.Load())
	}
}

func TestSSHBinaryStageSenderConstructorTrustBeforeIO(t *testing.T) {
	key, signer := deploymentKey(t)
	_, hostSigner := deploymentKey(t)
	_, otherSigner := deploymentKey(t)
	hostFP := ssh.FingerprintSHA256(hostSigner.PublicKey())
	clientFP := ssh.FingerprintSHA256(signer.PublicKey())
	otherFP := ssh.FingerprintSHA256(otherSigner.PublicKey())
	ctx := t.Context()

	if s, err := NewSSHBinaryStageSender(nil, key, hostFP, clientFP, probe.EndpointPolicy{}); err != domain.ErrProbeSSHInput || s != nil {
		t.Fatalf("nil ctx %v", err)
	}
	var nilCtx *sendTNilContext
	if s, err := NewSSHBinaryStageSender(nilCtx, key, hostFP, clientFP, probe.EndpointPolicy{}); err != domain.ErrProbeSSHInput || s != nil {
		t.Fatalf("typed nil ctx %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if s, err := NewSSHBinaryStageSender(canceled, key, hostFP, clientFP, probe.EndpointPolicy{}); err != context.Canceled || s != nil {
		t.Fatalf("canceled ctx %v", err)
	}
	if s, err := NewSSHBinaryStageSender(ctx, nil, hostFP, clientFP, probe.EndpointPolicy{}); err != domain.ErrProbeSSHInput || s != nil {
		t.Fatalf("empty key %v", err)
	}
	if s, err := NewSSHBinaryStageSender(ctx, []byte("not a key"), hostFP, clientFP, probe.EndpointPolicy{}); err != domain.ErrProbeSSHInput || s != nil {
		t.Fatalf("bad key %v", err)
	}
	if s, err := NewSSHBinaryStageSender(ctx, key, "SHA256:!!!", clientFP, probe.EndpointPolicy{}); err != domain.ErrProbeSSHInput || s != nil {
		t.Fatalf("bad host pin %v", err)
	}
	if s, err := NewSSHBinaryStageSender(ctx, key, hostFP, "SHA256:!!!", probe.EndpointPolicy{}); err != domain.ErrProbeSSHInput || s != nil {
		t.Fatalf("bad client pin %v", err)
	}
	if s, err := NewSSHBinaryStageSender(ctx, key, hostFP, otherFP, probe.EndpointPolicy{}); err != domain.ErrProbeSSHInput || s != nil {
		t.Fatalf("client pin mismatch %v", err)
	}
	if s, err := NewSSHBinaryStageSender(ctx, key, hostFP, clientFP,
		probe.EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.1/8")}}); err != domain.ErrProbeSSHInput || s != nil {
		t.Fatalf("unmasked prefix %v", err)
	}
	sender, err := NewSSHBinaryStageSender(ctx, key, hostFP, clientFP, probe.EndpointPolicy{})
	if err != nil || sender == nil || sender.insp == nil {
		t.Fatal(err)
	}
}

func TestSSHBinaryStageSenderTrustDestinationConnect(t *testing.T) {
	content := []byte("phoenix pinned payload")
	binary := sendTBytesBinary(content)

	t.Run("host pin rejected before authentication", func(t *testing.T) {
		f := serveStageSend(t, sendTConfig{stdout: sendTReceiptFor(binary.Pin())})
		_, otherSigner := deploymentKey(t)
		wrongFP := ssh.FingerprintSHA256(otherSigner.PublicKey())
		sender, err := NewSSHBinaryStageSender(t.Context(), f.clientKey, wrongFP, f.clientFP(), f.policy())
		if err != nil {
			t.Fatal(err)
		}
		intent := f.intent(binary.Pin())
		intent.Identity.HostKeyFingerprint = wrongFP
		res := sendTAwait(t, sendTSendAsync(t.Context(), sender, intent, binary), "trust rejection")
		if res.err != domain.ErrProbeSSHTrust || res.ack != (SSHBinaryStageAck{}) {
			t.Fatalf("trust %+v %v", res.ack, res.err)
		}
		if f.authCalls.Load() != 0 {
			t.Fatalf("client authenticated before host trust: %d", f.authCalls.Load())
		}
	})

	t.Run("destination rejected by policy", func(t *testing.T) {
		f := serveStageSend(t, sendTConfig{})
		intent := f.intent(binary.Pin())
		sender, err := NewSSHBinaryStageSender(t.Context(), f.clientKey, f.hostFP(), f.clientFP(), probe.EndpointPolicy{})
		if err != nil {
			t.Fatal(err)
		}
		res := sendTAwait(t, sendTSendAsync(t.Context(), sender, intent, binary), "destination rejection")
		if res.err != domain.ErrProbeSSHDestination || res.ack != (SSHBinaryStageAck{}) {
			t.Fatalf("destination %+v %v", res.ack, res.err)
		}
		if f.connCalls.Load() != 0 {
			t.Fatalf("dialed a rejected destination: %d", f.connCalls.Load())
		}
	})

	t.Run("connect failure preserved", func(t *testing.T) {
		f := serveStageSend(t, sendTConfig{})
		intent := f.intent(binary.Pin())
		dead, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		deadAddr := dead.Addr().String()
		_ = dead.Close()
		intent.Identity.Host.Address = deadAddr
		res := sendTAwait(t, sendTSendAsync(t.Context(), f.sender(t), intent, binary), "connect failure")
		if res.err != domain.ErrProbeSSHConnect || res.ack != (SSHBinaryStageAck{}) {
			t.Fatalf("connect %+v %v", res.ack, res.err)
		}
		if f.connCalls.Load() != 0 {
			t.Fatalf("fixture contacted: %d", f.connCalls.Load())
		}
	})
}

func TestSSHBinaryStageSenderFixedExecHeaderBodyEOF(t *testing.T) {
	content := []byte("phoenix pinned payload")
	binary := sendTBytesBinary(content)
	f := serveStageSend(t, sendTConfig{stdout: sendTReceiptFor(binary.Pin())})
	intent := f.intent(binary.Pin())

	res := sendTAwait(t, sendTSendAsync(t.Context(), f.sender(t), intent, binary), "successful send")
	if res.err != nil {
		t.Fatal(res.err)
	}
	if res.ack != (SSHBinaryStageAck{Intent: intent}) {
		t.Fatalf("ack %+v", res.ack)
	}
	if got := f.requestTypes(); !slices.Equal(got, []string{"exec"}) {
		t.Fatalf("session requests %v", got)
	}
	if got := f.execCommands(); !slices.Equal(got, []string{SSHProbeBinaryStageCommand}) {
		t.Fatalf("exec commands %v", got)
	}
	header, err := EncodeProbeBinaryStageHeader(intent)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte(nil), header...), content...)
	if !bytes.Equal(f.stdinBytes(), want) {
		t.Fatalf("stdin = %d bytes, want %d", len(f.stdinBytes()), len(want))
	}
	if !f.stdinEOFSeen() {
		t.Fatal("missing stdin EOF")
	}
	if binary.opens.Load() != 1 {
		t.Fatalf("opens = %d, want 1", binary.opens.Load())
	}
	if binary.snapshotCloses.Load() != 0 {
		t.Fatalf("borrowed snapshot closed %d times", binary.snapshotCloses.Load())
	}
}

func TestSSHBinaryStageSenderSourceFailures(t *testing.T) {
	content := []byte("phoenix pinned payload")
	pin := sendTPin(content)
	cases := []struct {
		name string
		open func() (io.ReadCloser, error)
		ok   bool
	}{
		{name: "exact stream with single-wrapper EOF", open: func() (io.ReadCloser, error) {
			return sendTScripted(content, nil, fmt.Errorf("wrapped source end: %w", io.EOF)), nil
		}, ok: true},
		{name: "short body", open: func() (io.ReadCloser, error) { return sendTScripted(content[:3], nil, io.EOF), nil }},
		{name: "extra byte after pin size", open: func() (io.ReadCloser, error) { return sendTScripted(content, []byte("x"), io.EOF), nil }},
		{name: "digest mismatch", open: func() (io.ReadCloser, error) {
			return sendTScripted(bytes.Repeat([]byte("z"), len(content)), nil, io.EOF), nil
		}},
		{name: "no progress", open: func() (io.ReadCloser, error) {
			return &sendTReader{readFn: func([]byte) (int, error) { return 0, nil }}, nil
		}},
		{name: "multi-cause EOF refused", open: func() (io.ReadCloser, error) {
			return sendTScripted(content, nil, errors.Join(io.EOF, errors.New("independent failure"))), nil
		}},
		{name: "unexpected EOF refused", open: func() (io.ReadCloser, error) { return sendTScripted(content, nil, io.ErrUnexpectedEOF), nil }},
		{name: "reader close failure", open: func() (io.ReadCloser, error) { return sendTCloseFailReader(content), nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binary := sendTBinary(pin, func(context.Context) (io.ReadCloser, error) { return tc.open() })
			f := serveStageSend(t, sendTConfig{stdout: sendTReceiptFor(pin)})
			intent := f.intent(pin)
			res := sendTAwait(t, sendTSendAsync(t.Context(), f.sender(t), intent, binary), "source failure send")
			if tc.ok {
				if res.err != nil {
					t.Fatal(res.err)
				}
				if res.ack != (SSHBinaryStageAck{Intent: intent}) {
					t.Fatalf("ack %+v", res.ack)
				}
				if !f.stdinEOFSeen() {
					t.Fatal("missing stdin EOF")
				}
				return
			}
			if res.err != ErrSSHBinaryStageUnknown || res.ack != (SSHBinaryStageAck{}) {
				t.Fatalf("failure send %+v %v", res.ack, res.err)
			}
			// Channel Read can report EOF when the transport disconnects too.
			// It cannot establish that the sender sent normal channel EOF;
			// the zero acknowledgement/Unknown assertion above is load-bearing.
			if got := f.execCommands(); !slices.Equal(got, []string{SSHProbeBinaryStageCommand}) {
				t.Fatalf("exec commands %v", got)
			}
			if binary.snapshotCloses.Load() != 0 {
				t.Fatalf("borrowed snapshot closed %d times", binary.snapshotCloses.Load())
			}
		})
	}
}

func TestSSHBinaryStageSenderSourceOpenFailuresBeforeExec(t *testing.T) {
	content := []byte("phoenix pinned payload")
	pin := sendTPin(content)
	f := serveStageSend(t, sendTConfig{stdout: sendTReceiptFor(pin)})
	intent := f.intent(pin)

	t.Run("open error starts no helper", func(t *testing.T) {
		binary := sendTBinary(pin, func(context.Context) (io.ReadCloser, error) {
			return nil, errors.New("snapshot unavailable")
		})
		res := sendTAwait(t, sendTSendAsync(t.Context(), f.sender(t), intent, binary), "open failure")
		if res.err != domain.ErrProbeArtifactStorage || res.ack != (SSHBinaryStageAck{}) {
			t.Fatalf("open failure %+v %v", res.ack, res.err)
		}
		if f.connCalls.Load() != 0 || f.execCalls.Load() != 0 {
			t.Fatalf("helper started on unavailable source: conn %d exec %d", f.connCalls.Load(), f.execCalls.Load())
		}
		if binary.snapshotCloses.Load() != 0 {
			t.Fatalf("borrowed snapshot closed %d times", binary.snapshotCloses.Load())
		}
	})

	t.Run("partial open reader owned exactly once", func(t *testing.T) {
		reader := &sendTReader{readFn: func([]byte) (int, error) { return 0, io.EOF }}
		binary := sendTBinary(pin, func(context.Context) (io.ReadCloser, error) {
			return reader, errors.New("partial open")
		})
		res := sendTAwait(t, sendTSendAsync(t.Context(), f.sender(t), intent, binary), "partial open failure")
		if res.err != domain.ErrProbeArtifactStorage || res.ack != (SSHBinaryStageAck{}) {
			t.Fatalf("partial open %+v %v", res.ack, res.err)
		}
		if reader.closes.Load() != 1 {
			t.Fatalf("partial reader closes = %d, want 1", reader.closes.Load())
		}
		if f.connCalls.Load() != 0 {
			t.Fatalf("helper started on partial source: %d", f.connCalls.Load())
		}
	})

	t.Run("nil reader without error is storage failure", func(t *testing.T) {
		binary := sendTBinary(pin, func(context.Context) (io.ReadCloser, error) { return nil, nil })
		res := sendTAwait(t, sendTSendAsync(t.Context(), f.sender(t), intent, binary), "nil reader")
		if res.err != domain.ErrProbeArtifactStorage || res.ack != (SSHBinaryStageAck{}) {
			t.Fatalf("nil reader %+v %v", res.ack, res.err)
		}
		if f.connCalls.Load() != 0 {
			t.Fatalf("helper started on nil source: %d", f.connCalls.Load())
		}
	})
}

func TestSSHBinaryStageSenderBoundedChannelOutput(t *testing.T) {
	content := []byte("phoenix pinned payload")
	binary := sendTBytesBinary(content)

	t.Run("stdout bound", func(t *testing.T) {
		f := serveStageSend(t, sendTConfig{stdout: func(*sendTFixture) string { return strings.Repeat("x", stageSendStdoutMax+1) }})
		res := sendTAwait(t, sendTSendAsync(t.Context(), f.sender(t), f.intent(binary.Pin()), binary), "stdout bound")
		if res.err != ErrSSHBinaryStageUnknown || res.ack != (SSHBinaryStageAck{}) {
			t.Fatalf("stdout bound %+v %v", res.ack, res.err)
		}
	})

	t.Run("stderr bound", func(t *testing.T) {
		f := serveStageSend(t, sendTConfig{stdout: sendTReceiptFor(binary.Pin()), stderr: strings.Repeat("y", stageSendStderrMax+1)})
		res := sendTAwait(t, sendTSendAsync(t.Context(), f.sender(t), f.intent(binary.Pin()), binary), "stderr bound")
		if res.err != ErrSSHBinaryStageUnknown || res.ack != (SSHBinaryStageAck{}) {
			t.Fatalf("stderr bound %+v %v", res.ack, res.err)
		}
	})
}

func TestSSHBinaryStageSenderExitStatusUnknown(t *testing.T) {
	content := []byte("phoenix pinned payload")
	binary := sendTBytesBinary(content)
	cases := []struct {
		name string
		cfg  sendTConfig
	}{
		{name: "missing exit status", cfg: sendTConfig{stdout: sendTReceiptFor(binary.Pin()), noExit: true}},
		{name: "nonzero exit with valid receipt", cfg: sendTConfig{stdout: sendTReceiptFor(binary.Pin()), exit: sendTExit()}},
		{name: "signal exit with valid receipt", cfg: sendTConfig{stdout: sendTReceiptFor(binary.Pin()), signal: "TERM"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := serveStageSend(t, tc.cfg)
			intent := f.intent(binary.Pin())
			res := sendTAwait(t, sendTSendAsync(t.Context(), f.sender(t), intent, binary), tc.name)
			if res.err != ErrSSHBinaryStageUnknown || res.ack != (SSHBinaryStageAck{}) {
				t.Fatalf("%s %+v %v", tc.name, res.ack, res.err)
			}
			if got := f.execCommands(); !slices.Equal(got, []string{SSHProbeBinaryStageCommand}) {
				t.Fatalf("exec commands %v", got)
			}
		})
	}
}

func TestSSHBinaryStageSenderReceiptBinding(t *testing.T) {
	content := []byte("phoenix pinned payload")
	binary := sendTBytesBinary(content)
	cases := []struct {
		name    string
		receipt func(f *sendTFixture) string
	}{
		{name: "empty output", receipt: nil},
		{name: "wrong directory", receipt: func(f *sendTFixture) string { return sendTOtherOpReceipt(f, binary.Pin()) }},
		{name: "missing final LF", receipt: func(f *sendTFixture) string { return strings.TrimSuffix(sendTReceipt(f.intent(binary.Pin())), "\n") }},
		{name: "extra line", receipt: func(f *sendTFixture) string { return sendTReceipt(f.intent(binary.Pin())) + "\n" }},
		{name: "unknown key", receipt: func(f *sendTFixture) string {
			return strings.Replace(sendTReceipt(f.intent(binary.Pin())), `"state"`, `"extra"`, 1)
		}},
		{name: "truncated object", receipt: func(f *sendTFixture) string { return sendTReceipt(f.intent(binary.Pin()))[:20] }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := serveStageSend(t, sendTConfig{stdout: tc.receipt})
			intent := f.intent(binary.Pin())
			res := sendTAwait(t, sendTSendAsync(t.Context(), f.sender(t), intent, binary), tc.name)
			if res.err != ErrSSHBinaryStageUnknown || res.ack != (SSHBinaryStageAck{}) {
				t.Fatalf("%s %+v %v", tc.name, res.ack, res.err)
			}
		})
	}
}

func TestSSHBinaryStageSenderPostExecCancelUnknown(t *testing.T) {
	content := []byte("phoenix pinned payload")
	binary := sendTBytesBinary(content)
	f := serveStageSend(t, sendTConfig{handle: func(_ *sendTFixture, _ ssh.Channel, _ string, dead <-chan struct{}) {
		// Accept the exec, then never read stdin and never exit.
		<-dead
	}})
	intent := f.intent(binary.Pin())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := sendTSendAsync(ctx, f.sender(t), intent, binary)
	sendTWaitCount(t, &f.execCalls, "exec request")
	cancel()
	res := sendTAwait(t, done, "post-exec cancel")
	if res.err != ErrSSHBinaryStageUnknown || res.ack != (SSHBinaryStageAck{}) {
		t.Fatalf("post-exec cancel %+v %v", res.ack, res.err)
	}
	if got := f.execCommands(); !slices.Equal(got, []string{SSHProbeBinaryStageCommand}) {
		t.Fatalf("exec commands %v", got)
	}
	if f.connCalls.Load() != 1 {
		t.Fatalf("connections = %d, want 1 (no retry)", f.connCalls.Load())
	}
}

func TestSSHBinaryStageSenderCancelJoinsSourceAndCallback(t *testing.T) {
	reader := &sendTHeldClose{started: make(chan struct{}), closeStarted: make(chan struct{}), closed: make(chan struct{}), release: make(chan struct{})}
	binary := sendTBinary(sendTPin([]byte("x")), func(context.Context) (io.ReadCloser, error) { return reader, nil })
	f := serveStageSend(t, sendTConfig{})
	intent := f.intent(binary.Pin())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := sendTSendAsync(ctx, f.sender(t), intent, binary)
	joined := false
	var release sync.Once
	releaseReader := func() { release.Do(func() { close(reader.release) }) }
	t.Cleanup(func() {
		cancel()
		releaseReader()
		reader.closeEnd.Do(func() { close(reader.closed) })
		if !joined {
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("held-source sender did not join during cleanup")
			}
		}
	})
	sendTWaitSignal(t, reader.started, "active source Read")
	sendTExpectBlocked(t, done, "send during active read")
	cancel()
	// Observe that the actual cancellation callback entered owned Close;
	// elapsed time without that observation would not prove this boundary.
	sendTWaitSignal(t, reader.closeStarted, "source Close callback")
	sendTExpectBlocked(t, done, "send while source close is held")
	releaseReader()
	res := sendTAwait(t, done, "cancel during active read")
	joined = true
	if res.err != ErrSSHBinaryStageUnknown || res.ack != (SSHBinaryStageAck{}) {
		t.Fatalf("cancel during read %+v %v", res.ack, res.err)
	}
	if reader.closes.Load() != 1 {
		t.Fatalf("reader closes = %d, want 1", reader.closes.Load())
	}
	// A disconnected SSH channel can also read as EOF. That observation
	// cannot establish remote cancellation or absence of staging effects.
	if got := f.execCommands(); !slices.Equal(got, []string{SSHProbeBinaryStageCommand}) {
		t.Fatalf("exec commands %v", got)
	}
	if f.connCalls.Load() != 1 {
		t.Fatalf("connections = %d, want 1 (no retry)", f.connCalls.Load())
	}
}

func TestSSHBinaryStageSenderPreExecCancelContextSentinel(t *testing.T) {
	content := []byte("phoenix pinned payload")
	binary := sendTBytesBinary(content)
	f := serveStageSend(t, sendTConfig{stallHandshake: true})
	intent := f.intent(binary.Pin())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := sendTSendAsync(ctx, f.sender(t), intent, binary)
	sendTWaitCount(t, &f.connCalls, "connection")
	cancel()
	res := sendTAwait(t, done, "pre-exec cancel")
	if res.err != context.Canceled || res.ack != (SSHBinaryStageAck{}) {
		t.Fatalf("pre-exec cancel %+v %v", res.ack, res.err)
	}
	if f.execCalls.Load() != 0 {
		t.Fatalf("exec started before cancel: %d", f.execCalls.Load())
	}
	if binary.snapshotCloses.Load() != 0 {
		t.Fatalf("borrowed snapshot closed %d times", binary.snapshotCloses.Load())
	}
}
