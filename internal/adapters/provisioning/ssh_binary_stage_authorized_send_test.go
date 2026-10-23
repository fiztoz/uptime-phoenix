package provisioning

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// These tests drive SendAuthorized through the real loopback SSH protocol
// fixture of ssh_binary_stage_send_test.go. The remote side is an emulation of
// the authorized helper (challenge line, then stdin to EOF, then grant check,
// receipt and exit status); it is protocol evidence only. Every key is
// generated per test, nothing leaves loopback, and no OpenSSH, Docker or
// installed helper is consulted.

// Independent literals: a changed constant must not silently change what these
// tests accept.
const (
	authSendCommandLiteral       = "/usr/local/bin/phoenix-probe-admin stage-binary-authorized"
	authSendLegacyCommandLiteral = "/usr/local/bin/phoenix-probe-admin stage-binary-host"
	authSendGrantMagicLiteral    = "PHXGRT1\n"
	authSendChunkGap             = 30 * time.Millisecond
)

// authSendAuthority is a generated Ed25519 authority standing in for the
// operator-installed trusted key and the signing service behind the authorizer.
type authSendAuthority struct {
	priv  ed25519.PrivateKey
	pub   ed25519.PublicKey
	raw   [32]byte
	keyID string
}

func newAuthSendAuthority(t *testing.T) *authSendAuthority {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	a := &authSendAuthority{priv: priv, pub: pub}
	copy(a.raw[:], pub)
	a.keyID = domain.ProbeBinaryStageAuthorityKeyID(a.raw)
	return a
}

func (a *authSendAuthority) sign(request domain.ProbeBinaryStageAdmissionRequest) domain.ProbeBinaryStageGrant {
	grant := domain.ProbeBinaryStageGrant{Request: request}
	copy(grant.Signature[:], ed25519.Sign(a.priv, request.SigningMessage()))
	return grant
}

func authSendNonce() [32]byte {
	var nonce [32]byte
	for i := range nonce {
		nonce[i] = byte(i + 1)
	}
	return nonce
}

// authSendAuthorizer is the fake ProbeBinaryStageAuthorizer: it signs with the
// generated authority, counts calls and can be steered by hook.
type authSendAuthorizer struct {
	authority *authSendAuthority
	hook      func(ctx context.Context, a *authSendAuthorizer, request domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageGrant, error)
	stdinLen  func() int

	calls   atomic.Int32
	entered chan struct{}
	once    sync.Once

	mu          sync.Mutex
	requests    []domain.ProbeBinaryStageAdmissionRequest
	stdinAtCall []int
	deadlines   []bool
	ctxErrAtEnd []error
}

func newAuthSendAuthorizer(authority *authSendAuthority) *authSendAuthorizer {
	return &authSendAuthorizer{authority: authority, entered: make(chan struct{})}
}

func (a *authSendAuthorizer) Authorize(ctx context.Context, request domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageGrant, error) {
	a.calls.Add(1)
	_, hasDeadline := ctx.Deadline()
	stdin := -1
	if a.stdinLen != nil {
		stdin = a.stdinLen()
	}
	a.mu.Lock()
	a.requests = append(a.requests, request)
	a.stdinAtCall = append(a.stdinAtCall, stdin)
	a.deadlines = append(a.deadlines, hasDeadline)
	a.mu.Unlock()
	a.once.Do(func() { close(a.entered) })
	if a.hook == nil {
		return a.authority.sign(request), nil
	}
	grant, err := a.hook(ctx, a, request)
	a.mu.Lock()
	a.ctxErrAtEnd = append(a.ctxErrAtEnd, ctx.Err())
	a.mu.Unlock()
	return grant, err
}

func (a *authSendAuthorizer) seenRequests() []domain.ProbeBinaryStageAdmissionRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.requests)
}

// authSendFuncAuthorizer lets a nil func value satisfy the authorizer port.
type authSendFuncAuthorizer func(ctx context.Context, request domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageGrant, error)

func (f authSendFuncAuthorizer) Authorize(ctx context.Context, request domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageGrant, error) {
	return f(ctx, request)
}

// authSendHelper emulates the authorized helper behind the exec request.
type authSendHelper struct {
	authority *authSendAuthority
	nonce     [32]byte
	pin       domain.ProbeBinaryPin
	content   []byte

	// Challenge phase.
	silent             bool                                    // never writes anything, never exits
	noChallenge        bool                                    // exits at once without a challenge
	noChallengeStatus  uint32                                  // exit status of noChallenge
	challengeFn        func(f *sendTFixture) []byte            // replaces the canonical line, one Write
	split              bool                                    // writes the canonical line in two Writes
	exitAfterChallenge bool                                    // exit status 1 right after the challenge
	dieAfter           chan struct{}                           // after the challenge, wait, then die without draining
	tail               func(f *sendTFixture, ch []byte) string // stdout after stdin EOF; default is the receipt
	stderr             string

	// Grant phase.
	verify bool // check the grant frame under the authority key before replying

	// Exit behaviour (consumed by the fixture's sendExit).
	exit           *uint32
	signal         string
	noExit         bool
	stallHandshake bool

	started  atomic.Int32
	finished atomic.Int32
	verified atomic.Int32 // 1 accepted, 2 rejected
}

func (h *authSendHelper) config() sendTConfig {
	return sendTConfig{
		handle:         h.handle,
		exit:           h.exit,
		signal:         h.signal,
		noExit:         h.noExit,
		stallHandshake: h.stallHandshake,
	}
}

func (h *authSendHelper) canonical() []byte {
	return EncodeProbeBinaryStageChallengeLine(h.authority.keyID, h.nonce)
}

// authSendExit reports one exit status and closes the channel the way sshd
// does when the process exits: the client session's Wait only returns once the
// channel is closed, EOF alone is not enough. Stdin the client writes after the
// close is dropped, so the fixture can only observe bytes sent before it.
func authSendExit(ch ssh.Channel, status uint32) {
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
	_ = ch.Close()
}

func (h *authSendHelper) drain(f *sendTFixture, ch ssh.Channel) {
	_, _ = io.Copy(sendTSink{f: f}, ch)
}

func (h *authSendHelper) handle(f *sendTFixture, ch ssh.Channel, _ string, dead <-chan struct{}) {
	h.started.Add(1)
	defer h.finished.Add(1)
	if h.silent {
		h.drain(f, ch)
		return
	}
	if h.noChallenge {
		authSendExit(ch, h.noChallengeStatus)
		return
	}
	challenge := h.canonical()
	chunks := [][]byte{challenge}
	switch {
	case h.challengeFn != nil:
		chunks = [][]byte{h.challengeFn(f)}
	case h.split:
		chunks = [][]byte{challenge[:17], challenge[17:]}
	}
	for i, chunk := range chunks {
		if i > 0 {
			time.Sleep(authSendChunkGap)
		}
		if len(chunk) == 0 {
			continue
		}
		if _, err := ch.Write(chunk); err != nil {
			break
		}
	}
	if h.exitAfterChallenge {
		authSendExit(ch, 1)
		return
	}
	if h.dieAfter != nil {
		select {
		case <-h.dieAfter:
		case <-dead:
		}
		authSendExit(ch, 1)
		return
	}
	if _, err := io.Copy(sendTSink{f: f}, ch); err != nil {
		return
	}
	f.mu.Lock()
	f.stdinEOF = true
	f.mu.Unlock()
	if h.verify {
		if !h.verifyGrant(f, challenge) {
			h.verified.Store(2)
			authSendExit(ch, 1)
			return
		}
		h.verified.Store(1)
	}
	tail := h.tail
	if tail == nil {
		tail = func(f *sendTFixture, _ []byte) string { return sendTReceipt(f.intent(h.pin)) }
	}
	_, _ = io.WriteString(ch, tail(f, challenge))
	_, _ = io.WriteString(ch.Stderr(), h.stderr)
	f.sendExit(ch)
}

// verifyGrant is the helper's own check: exact header, one grant frame whose
// Ed25519 signature verifies under its trusted key for the request it rebuilds
// from its challenge and the decoded header, then exactly the pinned bytes.
func (h *authSendHelper) verifyGrant(f *sendTFixture, challenge []byte) bool {
	intent := f.intent(h.pin)
	header, err := EncodeProbeBinaryStageHeader(intent)
	if err != nil {
		return false
	}
	stdin := f.stdinBytes()
	end := len(header) + ProbeBinaryStageGrantFrameSize
	if len(stdin) < end || !bytes.Equal(stdin[:len(header)], header) || !bytes.Equal(stdin[end:], h.content) {
		return false
	}
	signature, err := DecodeProbeBinaryStageGrantFrame(stdin[len(header):end])
	if err != nil {
		return false
	}
	keyID, nonce, err := ParseProbeBinaryStageChallengeLine(challenge)
	if err != nil {
		return false
	}
	request := domain.ProbeBinaryStageAdmissionRequest{
		Intent:    intent,
		Challenge: domain.ProbeBinaryStageChallenge{AuthorityKeyID: keyID, Nonce: nonce, MarkerSHA256: intent.MarkerSHA256()},
	}
	return domain.ProbeBinaryStageGrant{Request: request, Signature: signature}.ValidFor(h.authority.raw)
}

// waitIdle waits until every exec the fixture accepted has run its helper
// emulation to completion, so stdin observations are final.
func (h *authSendHelper) waitIdle(t *testing.T, f *sendTFixture) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for f.execCalls.Load() == 0 || h.started.Load() < f.execCalls.Load() || h.finished.Load() < h.started.Load() {
		if time.Now().After(deadline) {
			t.Fatalf("helper emulation did not finish: execs %d started %d finished %d", f.execCalls.Load(), h.started.Load(), h.finished.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type authSendHarness struct {
	f       *sendTFixture
	helper  *authSendHelper
	auth    *authSendAuthorizer
	binary  *sendTFakeBinary
	sender  *SSHBinaryStageSender
	intent  domain.ProbeBinaryStageIntent
	content []byte
	reads   atomic.Int32
	closes  atomic.Int32
}

func newAuthSendHarness(t *testing.T, tune func(helper *authSendHelper, auth *authSendAuthorizer)) *authSendHarness {
	t.Helper()
	h := &authSendHarness{content: []byte("phoenix pinned payload")}
	pin := sendTPin(h.content)
	h.binary = sendTBinary(pin, func(context.Context) (io.ReadCloser, error) {
		inner := sendTScripted(h.content, nil, io.EOF)
		return &sendTReader{
			readFn:  func(p []byte) (int, error) { h.reads.Add(1); return inner.Read(p) },
			closeFn: func() error { h.closes.Add(1); return nil },
		}, nil
	})
	authority := newAuthSendAuthority(t)
	h.helper = &authSendHelper{authority: authority, nonce: authSendNonce(), pin: pin, content: h.content, verify: true}
	h.auth = newAuthSendAuthorizer(authority)
	if tune != nil {
		tune(h.helper, h.auth)
	}
	h.f = serveStageSend(t, h.helper.config())
	h.auth.stdinLen = func() int { return len(h.f.stdinBytes()) }
	h.intent = h.f.intent(pin)
	h.sender = h.f.sender(t)
	return h
}

func (h *authSendHarness) sendAsync(ctx context.Context) <-chan sendTResult {
	done := make(chan sendTResult, 1)
	go func() {
		ack, err := h.sender.SendAuthorized(ctx, h.intent, h.binary, h.auth)
		done <- sendTResult{ack: ack, err: err}
	}()
	return done
}

func (h *authSendHarness) send(t *testing.T) sendTResult {
	t.Helper()
	return sendTAwait(t, h.sendAsync(t.Context()), "authorized send")
}

// wantRequest builds the expected admission request independently of the
// sender: the harness intent, the helper's key id and nonce, the intent marker.
func (h *authSendHarness) wantRequest() domain.ProbeBinaryStageAdmissionRequest {
	return domain.ProbeBinaryStageAdmissionRequest{
		Intent: h.intent,
		Challenge: domain.ProbeBinaryStageChallenge{
			AuthorityKeyID: h.helper.authority.keyID,
			Nonce:          h.helper.nonce,
			MarkerSHA256:   h.intent.MarkerSHA256(),
		},
	}
}

func (h *authSendHarness) header(t *testing.T) []byte {
	t.Helper()
	header, err := EncodeProbeBinaryStageHeader(h.intent)
	if err != nil {
		t.Fatal(err)
	}
	return header
}

// wantStdin is header, literal grant magic, the signature, then body.
func (h *authSendHarness) wantStdin(t *testing.T, signature, body []byte) []byte {
	t.Helper()
	want := append([]byte(nil), h.header(t)...)
	want = append(want, authSendGrantMagicLiteral...)
	want = append(want, signature...)
	return append(want, body...)
}

func (h *authSendHarness) genuineSignature() []byte {
	return ed25519.Sign(h.helper.authority.priv, h.wantRequest().SigningMessage())
}

// requireNotStaged asserts the pre-stdin failure class and its server- and
// source-side evidence over rounds sequential calls: no stdin byte reached the
// server, the payload source was never read, each owned reader closed once,
// every call used the single fixed exec on one connection (no retry).
func (h *authSendHarness) requireNotStaged(t *testing.T, res sendTResult, rounds int) {
	t.Helper()
	if res.err != ErrSSHBinaryStageNotStaged || res.ack != (SSHBinaryStageAck{}) {
		t.Fatalf("result %+v %v, want ErrSSHBinaryStageNotStaged", res.ack, res.err)
	}
	h.requireNoStageInput(t, rounds)
}

func (h *authSendHarness) requireNoStageInput(t *testing.T, rounds int) {
	t.Helper()
	h.helper.waitIdle(t, h.f)
	// The channel can also read as EOF when the transport closes, so stdin EOF
	// is not asserted either way: the byte count is the load-bearing evidence.
	if got := h.f.stdinBytes(); len(got) != 0 {
		t.Fatalf("server received %d stdin bytes, want 0", len(got))
	}
	if got, want := h.f.execCommands(), slices.Repeat([]string{authSendCommandLiteral}, rounds); !slices.Equal(got, want) {
		t.Fatalf("exec commands %v, want %v", got, want)
	}
	if got := h.f.connCalls.Load(); got != int32(rounds) {
		t.Fatalf("connections = %d, want %d (no retry)", got, rounds)
	}
	if h.reads.Load() != 0 {
		t.Fatalf("payload source was read %d times before any grant", h.reads.Load())
	}
	if h.closes.Load() != int32(rounds) {
		t.Fatalf("owned reader closes = %d, want %d", h.closes.Load(), rounds)
	}
	if h.binary.snapshotCloses.Load() != 0 {
		t.Fatalf("borrowed snapshot closed %d times", h.binary.snapshotCloses.Load())
	}
}

// requireUnknown asserts the post-stdin failure class and the exact stdin the
// server received.
func (h *authSendHarness) requireUnknown(t *testing.T, res sendTResult, wantStdin []byte) {
	t.Helper()
	if res.err != ErrSSHBinaryStageUnknown || res.ack != (SSHBinaryStageAck{}) {
		t.Fatalf("result %+v %v, want ErrSSHBinaryStageUnknown", res.ack, res.err)
	}
	h.helper.waitIdle(t, h.f)
	if got := h.f.stdinBytes(); !bytes.Equal(got, wantStdin) {
		t.Fatalf("server stdin = %d bytes, want %d", len(got), len(wantStdin))
	}
	if got := h.f.execCommands(); !slices.Equal(got, []string{authSendCommandLiteral}) {
		t.Fatalf("exec commands %v", got)
	}
	if h.f.connCalls.Load() != 1 {
		t.Fatalf("connections = %d, want 1 (no retry)", h.f.connCalls.Load())
	}
}

// authSendDeadAddress returns a loopback address nothing listens on.
func authSendDeadAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func authSendBlockUntilDone(ctx context.Context, _ *authSendAuthorizer, _ domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageGrant, error) {
	<-ctx.Done()
	return domain.ProbeBinaryStageGrant{}, ctx.Err()
}

func TestSSHBinaryStageSenderAuthorizedHappyPath(t *testing.T) {
	h := newAuthSendHarness(t, nil)
	res := h.send(t)
	if res.err != nil {
		t.Fatal(res.err)
	}
	if res.ack != (SSHBinaryStageAck{Intent: h.intent}) {
		t.Fatalf("ack %+v", res.ack)
	}
	h.helper.waitIdle(t, h.f)

	if got := h.f.requestTypes(); !slices.Equal(got, []string{"exec"}) {
		t.Fatalf("session requests %v", got)
	}
	if got := h.f.execCommands(); !slices.Equal(got, []string{authSendCommandLiteral}) {
		t.Fatalf("exec commands %v", got)
	}
	if SSHProbeBinaryStageAuthorizedCommand != authSendCommandLiteral {
		t.Fatalf("authorized command constant drifted: %q", SSHProbeBinaryStageAuthorizedCommand)
	}
	if h.f.connCalls.Load() != 1 {
		t.Fatalf("connections = %d, want 1", h.f.connCalls.Load())
	}

	// Exactly one authorization for the request built from the helper's own
	// challenge and the intent marker.
	if h.auth.calls.Load() != 1 {
		t.Fatalf("authorizer calls = %d, want 1", h.auth.calls.Load())
	}
	requests := h.auth.seenRequests()
	if len(requests) != 1 || requests[0] != h.wantRequest() {
		t.Fatalf("authorizer request %+v, want %+v", requests, h.wantRequest())
	}
	if requests[0].Challenge.Nonce != h.helper.nonce || requests[0].Challenge.AuthorityKeyID != h.helper.authority.keyID ||
		requests[0].Challenge.MarkerSHA256 != h.intent.MarkerSHA256() {
		t.Fatalf("challenge binding %+v", requests[0].Challenge)
	}
	h.auth.mu.Lock()
	stdinAtCall, deadlines := slices.Clone(h.auth.stdinAtCall), slices.Clone(h.auth.deadlines)
	h.auth.mu.Unlock()
	if !slices.Equal(stdinAtCall, []int{0}) {
		t.Fatalf("stdin bytes visible when authorizer ran: %v, want [0]", stdinAtCall)
	}
	if !slices.Equal(deadlines, []bool{true}) {
		t.Fatalf("authorizer context carried no call deadline: %v", deadlines)
	}

	// Server stdin is header + 72-byte grant frame + exact binary, then EOF.
	signature := h.genuineSignature()
	want := h.wantStdin(t, signature, h.content)
	got := h.f.stdinBytes()
	if !bytes.Equal(got, want) {
		t.Fatalf("server stdin = %d bytes, want %d", len(got), len(want))
	}
	if !h.f.stdinEOFSeen() {
		t.Fatal("missing stdin EOF")
	}
	header := h.header(t)
	frame := got[len(header) : len(header)+ProbeBinaryStageGrantFrameSize]
	if len(frame) != 72 || string(frame[:8]) != authSendGrantMagicLiteral {
		t.Fatalf("grant frame %q", frame)
	}
	if !bytes.Equal(got[len(header)+len(frame):], h.content) {
		t.Fatal("binary bytes are not the pinned content")
	}
	// The signature on the wire verifies under the signer's public key over the
	// request's signing message, and is a valid grant for the authority key.
	if !ed25519.Verify(h.helper.authority.pub, h.wantRequest().SigningMessage(), frame[8:]) {
		t.Fatal("grant frame signature does not verify over request.SigningMessage()")
	}
	var onWire [64]byte
	copy(onWire[:], frame[8:])
	if !(domain.ProbeBinaryStageGrant{Request: h.wantRequest(), Signature: onWire}).ValidFor(h.helper.authority.raw) {
		t.Fatal("wire grant is not ValidFor the authority key")
	}
	if h.helper.verified.Load() != 1 {
		t.Fatalf("helper emulation verdict %d, want accepted", h.helper.verified.Load())
	}

	// The borrowed snapshot is opened once, read, and its owned reader closed once.
	if h.binary.opens.Load() != 1 || h.closes.Load() != 1 || h.reads.Load() < 1 {
		t.Fatalf("source opens %d closes %d reads %d", h.binary.opens.Load(), h.closes.Load(), h.reads.Load())
	}
	if h.binary.snapshotCloses.Load() != 0 {
		t.Fatalf("borrowed snapshot closed %d times", h.binary.snapshotCloses.Load())
	}
}

func TestSSHBinaryStageSenderAuthorizedChallengeSplitAcrossWrites(t *testing.T) {
	h := newAuthSendHarness(t, func(helper *authSendHelper, _ *authSendAuthorizer) { helper.split = true })
	res := h.send(t)
	if res.err != nil || res.ack != (SSHBinaryStageAck{Intent: h.intent}) {
		t.Fatalf("split challenge %+v %v", res.ack, res.err)
	}
	if requests := h.auth.seenRequests(); len(requests) != 1 || requests[0] != h.wantRequest() {
		t.Fatalf("authorizer request %+v", requests)
	}
	h.helper.waitIdle(t, h.f)
	if want := h.wantStdin(t, h.genuineSignature(), h.content); !bytes.Equal(h.f.stdinBytes(), want) {
		t.Fatalf("server stdin = %d bytes, want %d", len(h.f.stdinBytes()), len(want))
	}
}

// The sender must not stream any stage input while the authorization is still
// pending: header, grant and payload all wait for the grant.
func TestSSHBinaryStageSenderAuthorizedNoStdinBeforeGrant(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseGrant := func() { releaseOnce.Do(func() { close(release) }) }
	h := newAuthSendHarness(t, func(_ *authSendHelper, auth *authSendAuthorizer) {
		auth.hook = func(ctx context.Context, a *authSendAuthorizer, request domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageGrant, error) {
			select {
			case <-release:
				return a.authority.sign(request), nil
			case <-ctx.Done():
				return domain.ProbeBinaryStageGrant{}, ctx.Err()
			}
		}
	})
	t.Cleanup(releaseGrant)
	done := h.sendAsync(t.Context())
	sendTWaitSignal(t, h.auth.entered, "authorizer call")
	sendTExpectBlocked(t, done, "send while authorization is pending")
	if got := h.f.stdinBytes(); len(got) != 0 {
		t.Fatalf("%d stdin bytes sent before the grant", len(got))
	}
	if h.reads.Load() != 0 {
		t.Fatalf("payload source read %d times before the grant", h.reads.Load())
	}
	releaseGrant()
	res := sendTAwait(t, done, "send after grant")
	if res.err != nil || res.ack != (SSHBinaryStageAck{Intent: h.intent}) {
		t.Fatalf("after grant %+v %v", res.ack, res.err)
	}
	if h.auth.calls.Load() != 1 {
		t.Fatalf("authorizer calls = %d, want 1", h.auth.calls.Load())
	}
}

func TestSSHBinaryStageSenderAuthorizedRefusesNilAuthorizer(t *testing.T) {
	h := newAuthSendHarness(t, nil)
	var typedNilPointer *authSendAuthorizer
	var typedNilFunc authSendFuncAuthorizer
	cases := []struct {
		name       string
		authorizer ProbeBinaryStageAuthorizer
	}{
		{name: "untyped nil", authorizer: nil},
		{name: "typed nil pointer", authorizer: typedNilPointer},
		{name: "typed nil func", authorizer: typedNilFunc},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ack, err := h.sender.SendAuthorized(t.Context(), h.intent, h.binary, tc.authorizer)
			if err != ErrSSHBinaryStageInput || ack != (SSHBinaryStageAck{}) {
				t.Fatalf("%s %+v %v", tc.name, ack, err)
			}
		})
	}
	// A nil authorizer is refused before any I/O, including opening the source.
	if h.f.connCalls.Load() != 0 || h.f.authCalls.Load() != 0 || h.f.execCalls.Load() != 0 {
		t.Fatalf("refusal performed I/O: conn %d auth %d exec %d", h.f.connCalls.Load(), h.f.authCalls.Load(), h.f.execCalls.Load())
	}
	if h.binary.opens.Load() != 0 || h.auth.calls.Load() != 0 {
		t.Fatalf("refusal opened source %d times, authorizer calls %d", h.binary.opens.Load(), h.auth.calls.Load())
	}
}

func TestSSHBinaryStageSenderAuthorizedRefusesInvalidInput(t *testing.T) {
	h := newAuthSendHarness(t, nil)
	ctx := t.Context()
	refused := func(name string, ack SSHBinaryStageAck, err error) {
		t.Helper()
		if err != ErrSSHBinaryStageInput || ack != (SSHBinaryStageAck{}) {
			t.Fatalf("%s %+v %v", name, ack, err)
		}
	}

	var nilSender *SSHBinaryStageSender
	ack, err := nilSender.SendAuthorized(ctx, h.intent, h.binary, h.auth)
	refused("nil sender", ack, err)
	var nilCtx *sendTNilContext
	ack, err = h.sender.SendAuthorized(nilCtx, h.intent, h.binary, h.auth)
	refused("typed nil ctx", ack, err)
	var nilBinary *sendTFakeBinary
	ack, err = h.sender.SendAuthorized(ctx, h.intent, nilBinary, h.auth)
	refused("typed nil binary", ack, err)

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if ack, err := h.sender.SendAuthorized(canceled, h.intent, h.binary, h.auth); err != context.Canceled || ack != (SSHBinaryStageAck{}) {
		t.Fatalf("canceled ctx %+v %v", ack, err)
	}

	badMode := h.intent
	badMode.Identity.Mode = domain.ProbeInstallDocker
	ack, err = h.sender.SendAuthorized(ctx, badMode, h.binary, h.auth)
	refused("invalid intent", ack, err)
	badUser := h.intent
	badUser.Identity.Host.User = "deploy"
	ack, err = h.sender.SendAuthorized(ctx, badUser, h.binary, h.auth)
	refused("non-root user", ack, err)
	_, otherSigner := deploymentKey(t)
	otherFP := ssh.FingerprintSHA256(otherSigner.PublicKey())
	badHostPin := h.intent
	badHostPin.Identity.HostKeyFingerprint = otherFP
	ack, err = h.sender.SendAuthorized(ctx, badHostPin, h.binary, h.auth)
	refused("host pin binding", ack, err)
	badClientPin := h.intent
	badClientPin.Identity.ClientKeyFingerprint = otherFP
	ack, err = h.sender.SendAuthorized(ctx, badClientPin, h.binary, h.auth)
	refused("client pin binding", ack, err)

	other := sendTBytesBinary([]byte("other payload"))
	ack, err = h.sender.SendAuthorized(ctx, h.intent, other, h.auth)
	refused("pin mismatch", ack, err)
	brokenPin := sendTBytesBinary(h.content)
	brokenPin.pin.Size = 0
	ack, err = h.sender.SendAuthorized(ctx, h.intent, brokenPin, h.auth)
	refused("invalid pin", ack, err)

	if h.f.connCalls.Load() != 0 || h.f.authCalls.Load() != 0 || h.f.execCalls.Load() != 0 {
		t.Fatalf("refusal performed I/O: conn %d auth %d exec %d", h.f.connCalls.Load(), h.f.authCalls.Load(), h.f.execCalls.Load())
	}
	if h.auth.calls.Load() != 0 || h.binary.opens.Load() != 0 {
		t.Fatalf("refusal reached authorizer %d times / source %d times", h.auth.calls.Load(), h.binary.opens.Load())
	}
}

// Failures before exec keep Send's sentinels and never reach the authorizer or
// the authorized-mode classes.
func TestSSHBinaryStageSenderAuthorizedPreExecFailuresKeepSendSentinels(t *testing.T) {
	requirePreExec := func(t *testing.T, h *authSendHarness, res sendTResult, want error) {
		t.Helper()
		if res.err != want || res.ack != (SSHBinaryStageAck{}) {
			t.Fatalf("result %+v %v, want %v", res.ack, res.err, want)
		}
		if h.auth.calls.Load() != 0 {
			t.Fatalf("authorizer reached %d times before exec", h.auth.calls.Load())
		}
		if h.f.execCalls.Load() != 0 || len(h.f.stdinBytes()) != 0 {
			t.Fatalf("exec %d or stdin bytes before exec", h.f.execCalls.Load())
		}
		if h.reads.Load() != 0 {
			t.Fatalf("payload source read %d times", h.reads.Load())
		}
	}

	t.Run("source open failure", func(t *testing.T) {
		h := newAuthSendHarness(t, nil)
		h.binary = sendTBinary(h.binary.Pin(), func(context.Context) (io.ReadCloser, error) {
			return nil, errors.New("snapshot unavailable")
		})
		res := h.send(t)
		requirePreExec(t, h, res, domain.ErrProbeArtifactStorage)
		if h.f.connCalls.Load() != 0 {
			t.Fatalf("connected despite an unavailable source: %d", h.f.connCalls.Load())
		}
	})

	t.Run("host key rejected before authentication", func(t *testing.T) {
		h := newAuthSendHarness(t, nil)
		_, otherSigner := deploymentKey(t)
		wrongFP := ssh.FingerprintSHA256(otherSigner.PublicKey())
		sender, err := NewSSHBinaryStageSender(t.Context(), h.f.clientKey, wrongFP, h.f.clientFP(), h.f.policy())
		if err != nil {
			t.Fatal(err)
		}
		h.sender = sender
		h.intent.Identity.HostKeyFingerprint = wrongFP
		res := h.send(t)
		requirePreExec(t, h, res, domain.ErrProbeSSHTrust)
		if h.f.authCalls.Load() != 0 {
			t.Fatalf("client authenticated before host trust: %d", h.f.authCalls.Load())
		}
	})

	t.Run("destination rejected by policy", func(t *testing.T) {
		h := newAuthSendHarness(t, nil)
		sender, err := NewSSHBinaryStageSender(t.Context(), h.f.clientKey, h.f.hostFP(), h.f.clientFP(), probe.EndpointPolicy{})
		if err != nil {
			t.Fatal(err)
		}
		h.sender = sender
		res := h.send(t)
		requirePreExec(t, h, res, domain.ErrProbeSSHDestination)
		if h.f.connCalls.Load() != 0 {
			t.Fatalf("dialed a rejected destination: %d", h.f.connCalls.Load())
		}
	})

	t.Run("connect failure", func(t *testing.T) {
		h := newAuthSendHarness(t, nil)
		h.intent.Identity.Host.Address = authSendDeadAddress(t)
		res := h.send(t)
		requirePreExec(t, h, res, domain.ErrProbeSSHConnect)
		if h.f.connCalls.Load() != 0 {
			t.Fatalf("fixture contacted: %d", h.f.connCalls.Load())
		}
	})

	t.Run("cancel before exec is the bare context sentinel", func(t *testing.T) {
		h := newAuthSendHarness(t, func(helper *authSendHelper, _ *authSendAuthorizer) { helper.stallHandshake = true })
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := h.sendAsync(ctx)
		sendTWaitCount(t, &h.f.connCalls, "connection")
		cancel()
		res := sendTAwait(t, done, "pre-exec cancel")
		requirePreExec(t, h, res, context.Canceled)
	})
}

func TestSSHBinaryStageSenderAuthorizedHelperExitsWithoutChallenge(t *testing.T) {
	for _, status := range []uint32{1, 0} {
		name := "exit 1"
		if status == 0 {
			name = "exit 0"
		}
		t.Run(name, func(t *testing.T) {
			h := newAuthSendHarness(t, func(helper *authSendHelper, _ *authSendAuthorizer) {
				helper.noChallenge = true
				helper.noChallengeStatus = status
			})
			start := time.Now()
			res := h.send(t)
			if elapsed := time.Since(start); elapsed > 10*time.Second {
				t.Fatalf("dead helper took %v to classify; want prompt return", elapsed)
			}
			h.requireNotStaged(t, res, 1)
			if h.auth.calls.Load() != 0 {
				t.Fatalf("authorizer called %d times for a helper that never challenged", h.auth.calls.Load())
			}
		})
	}
}

func TestSSHBinaryStageSenderAuthorizedMalformedChallenge(t *testing.T) {
	flip := func(h *authSendHelper, index int, to byte) []byte {
		line := h.canonical()
		line[index] = to
		return line
	}
	keyIDStart := len(stageChallengePrefix)
	nonceStart := keyIDStart + 64 + len(stageChallengeMiddle)
	cases := []struct {
		name      string
		line      func(h *authSendHelper, f *sendTFixture) []byte
		exitAfter bool
	}{
		{name: "non-hex character in key id", line: func(h *authSendHelper, _ *sendTFixture) []byte { return flip(h, keyIDStart, 'g') }},
		{name: "non-hex character in nonce", line: func(h *authSendHelper, _ *sendTFixture) []byte { return flip(h, nonceStart+5, 'z') }},
		{name: "uppercase hex in key id", line: func(h *authSendHelper, _ *sendTFixture) []byte { return flip(h, keyIDStart+2, 'A') }},
		{name: "uppercase hex in nonce", line: func(h *authSendHelper, _ *sendTFixture) []byte { return flip(h, nonceStart, 'F') }},
		{name: "zero nonce", line: func(h *authSendHelper, _ *sendTFixture) []byte {
			return []byte(stageChallengePrefix + h.authority.keyID + stageChallengeMiddle + strings.Repeat("0", 64) + stageChallengeSuffix)
		}},
		{name: "wrong state value of equal length", line: func(h *authSendHelper, _ *sendTFixture) []byte {
			return bytes.Replace(h.canonical(), []byte("stage_challenge"), []byte("stage_challengX"), 1)
		}},
		{name: "extra byte after the LF in the same write", line: func(h *authSendHelper, _ *sendTFixture) []byte { return append(h.canonical(), 'x') }},
		{name: "two canonical lines in one write", line: func(h *authSendHelper, _ *sendTFixture) []byte {
			return append(h.canonical(), h.canonical()...)
		}},
		{name: "embedded LF inside a full-size line", line: func(h *authSendHelper, _ *sendTFixture) []byte { return flip(h, 10, '\n') }},
		{name: "CRLF terminator", line: func(h *authSendHelper, _ *sendTFixture) []byte {
			line := h.canonical()
			return append(line[:len(line)-1], '\r', '\n')
		}},
		{name: "short junk line", line: func(*authSendHelper, *sendTFixture) []byte { return []byte("x\n") }},
		{name: "bare LF", line: func(*authSendHelper, *sendTFixture) []byte { return []byte("\n") }},
		{name: "receipt in place of the challenge", line: func(h *authSendHelper, f *sendTFixture) []byte {
			return []byte(sendTReceipt(f.intent(h.pin)))
		}},
		{name: "oversize output without LF", line: func(*authSendHelper, *sendTFixture) []byte {
			return bytes.Repeat([]byte("x"), stageSendStdoutMax+ProbeBinaryStageChallengeLineSize+1)
		}},
		{name: "oversize output ending in LF", line: func(*authSendHelper, *sendTFixture) []byte {
			return append(bytes.Repeat([]byte("x"), stageSendStdoutMax+ProbeBinaryStageChallengeLineSize), '\n')
		}},
		{name: "truncated challenge then helper exit", exitAfter: true, line: func(h *authSendHelper, _ *sendTFixture) []byte {
			line := h.canonical()
			return line[:len(line)-1]
		}},
		{name: "terminal LF replaced then helper exit", exitAfter: true, line: func(h *authSendHelper, _ *sendTFixture) []byte {
			return flip(h, ProbeBinaryStageChallengeLineSize-1, 'x')
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newAuthSendHarness(t, func(helper *authSendHelper, _ *authSendAuthorizer) {
				helper.challengeFn = func(f *sendTFixture) []byte { return tc.line(helper, f) }
				helper.exitAfterChallenge = tc.exitAfter
			})
			start := time.Now()
			res := h.send(t)
			if elapsed := time.Since(start); elapsed > 10*time.Second {
				t.Fatalf("malformed challenge took %v to classify; want prompt return", elapsed)
			}
			h.requireNotStaged(t, res, 1)
			if h.auth.calls.Load() != 0 {
				t.Fatalf("authorizer called %d times for a malformed challenge", h.auth.calls.Load())
			}
		})
	}
}

// A helper that dies right after writing its challenge must not cost a
// one-shot admission. The sender can only observe the exit best-effort: there
// is an inherent race between receiving the line and observing the exit, so
// the strict assertions here are the race-independent ones (NotStaged, zero
// stdin, at most one authorizer call per attempt). The authorizer reports an
// error whenever it is reached so the attempt still classifies NotStaged, and
// the rate at which it was reached is logged for the flakiness report.
func TestSSHBinaryStageSenderAuthorizedChallengeThenHelperExit(t *testing.T) {
	const rounds = 40
	h := newAuthSendHarness(t, func(helper *authSendHelper, auth *authSendAuthorizer) {
		helper.exitAfterChallenge = true
		auth.hook = func(context.Context, *authSendAuthorizer, domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageGrant, error) {
			return domain.ProbeBinaryStageGrant{}, errors.New("authorizer reached for a helper that already exited")
		}
	})
	reached := 0
	for round := range rounds {
		before := h.auth.calls.Load()
		start := time.Now()
		res := h.send(t)
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Fatalf("round %d took %v", round, elapsed)
		}
		if res.err != ErrSSHBinaryStageNotStaged || res.ack != (SSHBinaryStageAck{}) {
			t.Fatalf("round %d: %+v %v, want ErrSSHBinaryStageNotStaged", round, res.ack, res.err)
		}
		switch n := h.auth.calls.Load() - before; {
		case n > 1:
			t.Fatalf("round %d: authorizer called %d times, want at most 1", round, n)
		case n == 1:
			reached++
		}
	}
	h.requireNoStageInput(t, rounds)
	t.Logf("authorizer reached before the exit was observed in %d of %d rounds (best-effort guard)", reached, rounds)
	// Statistical, deliberately loose: the guard is best-effort, but if every
	// single round burned an admission it is not doing its job at all.
	if reached == rounds {
		t.Fatalf("authorizer reached in all %d rounds: the already-exited-helper guard is ineffective", rounds)
	}
}

// Same race with an authorizer that grants. Whether the sender observes the
// exit before or after the authorizer call decides the class, and the pair is
// consistent either way: an exit observed before the first stdin byte is
// NotStaged (with or without a consumed grant); an exit only noticed after the
// stage input was attempted into a dead channel is Unknown. Neither class is a
// success and the server never receives a stdin byte.
func TestSSHBinaryStageSenderAuthorizedChallengeThenHelperExitWithGrant(t *testing.T) {
	const rounds = 30
	h := newAuthSendHarness(t, func(helper *authSendHelper, _ *authSendAuthorizer) { helper.exitAfterChallenge = true })
	notStaged, unknown := 0, 0
	for round := range rounds {
		before := h.auth.calls.Load()
		res := h.send(t)
		if res.ack != (SSHBinaryStageAck{}) {
			t.Fatalf("round %d: acknowledged a dead helper: %+v", round, res.ack)
		}
		n := h.auth.calls.Load() - before
		switch res.err {
		case ErrSSHBinaryStageNotStaged:
			// The exit may be observed before or after the grant returns;
			// either way no stdin byte was attempted.
			if n > 1 {
				t.Fatalf("round %d: NotStaged after %d authorizer calls", round, n)
			}
			notStaged++
		case ErrSSHBinaryStageUnknown:
			if n != 1 {
				t.Fatalf("round %d: Unknown without a grant (%d authorizer calls)", round, n)
			}
			unknown++
		default:
			t.Fatalf("round %d: result %v", round, res.err)
		}
	}
	h.helper.waitIdle(t, h.f)
	if got := h.f.stdinBytes(); len(got) != 0 {
		t.Fatalf("server received %d stdin bytes from a helper that had exited", len(got))
	}
	t.Logf("dead helper with granting authorizer: %d NotStaged, %d Unknown of %d rounds", notStaged, unknown, rounds)
	if notStaged == 0 {
		t.Fatalf("every one of %d rounds consumed the grant for an already-exited helper", rounds)
	}
}

// If the helper dies while the grant is being obtained, the sender observes
// the exit after Authorize returns and refuses before any stdin byte: the
// consumed grant is spent, but the outcome is a definite NotStaged.
func TestSSHBinaryStageSenderAuthorizedHelperDiesDuringAuthorization(t *testing.T) {
	var helperRef *authSendHelper
	h := newAuthSendHarness(t, func(helper *authSendHelper, auth *authSendAuthorizer) {
		helperRef = helper
		helper.dieAfter = make(chan struct{})
		auth.hook = func(_ context.Context, a *authSendAuthorizer, request domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageGrant, error) {
			close(helperRef.dieAfter)
			deadline := time.Now().Add(5 * time.Second)
			for helperRef.finished.Load() < 1 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			// Let the closed channel reach the client before the grant returns.
			time.Sleep(200 * time.Millisecond)
			return a.authority.sign(request), nil
		}
	})
	// The helper exited before the grant came back: the sender must notice
	// and refuse before any stdin byte, so the outcome is definite.
	res := h.send(t)
	h.requireNotStaged(t, res, 1)
	if h.auth.calls.Load() != 1 {
		t.Fatalf("authorizer calls = %d, want 1", h.auth.calls.Load())
	}
}

func TestSSHBinaryStageSenderAuthorizedAuthorizerErrorIsNotStaged(t *testing.T) {
	sentinel := errors.New("journal attempt already consumed")
	cases := []struct {
		name string
		err  error
	}{
		{name: "plain error", err: sentinel},
		{name: "context canceled sentinel without a canceled context", err: context.Canceled},
		{name: "deadline exceeded sentinel without an expired context", err: context.DeadlineExceeded},
		{name: "domain sentinel", err: domain.ErrProbeArtifactStorage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newAuthSendHarness(t, func(_ *authSendHelper, auth *authSendAuthorizer) {
				auth.hook = func(context.Context, *authSendAuthorizer, domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageGrant, error) {
					return domain.ProbeBinaryStageGrant{}, tc.err
				}
			})
			res := h.send(t)
			h.requireNotStaged(t, res, 1)
			if errors.Is(res.err, tc.err) {
				t.Fatalf("authorizer error %v leaked through %v", tc.err, res.err)
			}
			if h.auth.calls.Load() != 1 {
				t.Fatalf("authorizer calls = %d, want exactly 1 (no retry)", h.auth.calls.Load())
			}
			if requests := h.auth.seenRequests(); len(requests) != 1 || requests[0] != h.wantRequest() {
				t.Fatalf("authorizer request %+v", requests)
			}
		})
	}
}

func TestSSHBinaryStageSenderAuthorizedGrantForDifferentRequestIsNotStaged(t *testing.T) {
	const otherUUID = "55555555-5555-4555-8555-555555555555"
	otherDigest := strings.Repeat("ab", 32)
	cases := []struct {
		name  string
		grant func(a *authSendAuthorizer, request domain.ProbeBinaryStageAdmissionRequest) domain.ProbeBinaryStageGrant
	}{
		{name: "different nonce, validly signed", grant: func(a *authSendAuthorizer, r domain.ProbeBinaryStageAdmissionRequest) domain.ProbeBinaryStageGrant {
			r.Challenge.Nonce[0] ^= 0xff
			return a.authority.sign(r)
		}},
		{name: "different authority key id, validly signed", grant: func(a *authSendAuthorizer, r domain.ProbeBinaryStageAdmissionRequest) domain.ProbeBinaryStageGrant {
			r.Challenge.AuthorityKeyID = otherDigest
			return a.authority.sign(r)
		}},
		{name: "different attempt id, validly signed", grant: func(a *authSendAuthorizer, r domain.ProbeBinaryStageAdmissionRequest) domain.ProbeBinaryStageGrant {
			r.Intent.AttemptID = otherUUID
			r.Challenge.MarkerSHA256 = r.Intent.MarkerSHA256()
			return a.authority.sign(r)
		}},
		{name: "different owner id, validly signed", grant: func(a *authSendAuthorizer, r domain.ProbeBinaryStageAdmissionRequest) domain.ProbeBinaryStageGrant {
			r.Intent.OwnerID = otherUUID
			r.Challenge.MarkerSHA256 = r.Intent.MarkerSHA256()
			return a.authority.sign(r)
		}},
		{name: "zero grant", grant: func(*authSendAuthorizer, domain.ProbeBinaryStageAdmissionRequest) domain.ProbeBinaryStageGrant {
			return domain.ProbeBinaryStageGrant{}
		}},
		{name: "signature only, request missing", grant: func(a *authSendAuthorizer, r domain.ProbeBinaryStageAdmissionRequest) domain.ProbeBinaryStageGrant {
			return domain.ProbeBinaryStageGrant{Signature: a.authority.sign(r).Signature}
		}},
		{name: "matching request with a zero signature", grant: func(_ *authSendAuthorizer, r domain.ProbeBinaryStageAdmissionRequest) domain.ProbeBinaryStageGrant {
			return domain.ProbeBinaryStageGrant{Request: r}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newAuthSendHarness(t, func(_ *authSendHelper, auth *authSendAuthorizer) {
				auth.hook = func(_ context.Context, a *authSendAuthorizer, request domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageGrant, error) {
					return tc.grant(a, request), nil
				}
			})
			res := h.send(t)
			h.requireNotStaged(t, res, 1)
			if h.auth.calls.Load() != 1 {
				t.Fatalf("authorizer calls = %d, want exactly 1", h.auth.calls.Load())
			}
		})
	}
}

func TestSSHBinaryStageSenderAuthorizedCancelDuringAuthorization(t *testing.T) {
	t.Run("caller cancel while the authorizer blocks", func(t *testing.T) {
		h := newAuthSendHarness(t, func(_ *authSendHelper, auth *authSendAuthorizer) { auth.hook = authSendBlockUntilDone })
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := h.sendAsync(ctx)
		sendTWaitSignal(t, h.auth.entered, "authorizer call")
		sendTExpectBlocked(t, done, "send while the authorizer blocks")
		cancel()
		res := sendTAwait(t, done, "cancel during authorization")
		if res.err == nil || res.ack != (SSHBinaryStageAck{}) {
			t.Fatalf("cancel during authorization succeeded: %+v %v", res.ack, res.err)
		}
		// Post-exec, pre-stdin: the classification is NotStaged, not a bare
		// context error and not Unknown.
		h.requireNotStaged(t, res, 1)
		if h.auth.calls.Load() != 1 {
			t.Fatalf("authorizer calls = %d, want 1", h.auth.calls.Load())
		}
		h.auth.mu.Lock()
		ctxErrs := slices.Clone(h.auth.ctxErrAtEnd)
		h.auth.mu.Unlock()
		if len(ctxErrs) != 1 || ctxErrs[0] == nil {
			t.Fatalf("authorizer returned without observing cancellation: %v", ctxErrs)
		}
	})

	t.Run("caller deadline below the authorization budget never consumes an admission", func(t *testing.T) {
		h := newAuthSendHarness(t, func(_ *authSendHelper, auth *authSendAuthorizer) { auth.hook = authSendBlockUntilDone })
		ctx, cancel := context.WithTimeout(t.Context(), stageSendAuthorizeMinBudget/2)
		defer cancel()
		res := sendTAwait(t, h.sendAsync(ctx), "short deadline before authorization")
		if h.auth.calls.Load() != 0 {
			t.Fatalf("authorizer calls = %d, want 0 (remaining budget below the floor)", h.auth.calls.Load())
		}
		h.requireNotStaged(t, res, 1)
	})

	t.Run("authorizer ignores cancellation and still returns a valid grant", func(t *testing.T) {
		h := newAuthSendHarness(t, func(_ *authSendHelper, auth *authSendAuthorizer) {
			auth.hook = func(ctx context.Context, a *authSendAuthorizer, request domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageGrant, error) {
				<-ctx.Done()
				return a.authority.sign(request), nil
			}
		})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := h.sendAsync(ctx)
		sendTWaitSignal(t, h.auth.entered, "authorizer call")
		cancel()
		res := sendTAwait(t, done, "cancel with a late grant")
		h.requireNotStaged(t, res, 1)
	})

	t.Run("cancel while the challenge never arrives", func(t *testing.T) {
		h := newAuthSendHarness(t, func(helper *authSendHelper, _ *authSendAuthorizer) { helper.silent = true })
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := h.sendAsync(ctx)
		sendTWaitCount(t, &h.f.execCalls, "exec request")
		sendTExpectBlocked(t, done, "send while the helper is silent")
		cancel()
		res := sendTAwait(t, done, "cancel while waiting for the challenge")
		h.requireNotStaged(t, res, 1)
		if h.auth.calls.Load() != 0 {
			t.Fatalf("authorizer called %d times without a challenge", h.auth.calls.Load())
		}
	})
}

// Everything after the first stdin byte is Unknown: the helper may have staged.
func TestSSHBinaryStageSenderAuthorizedPostGrantFailuresAreUnknown(t *testing.T) {
	receipt := func(f *sendTFixture, pin domain.ProbeBinaryPin) string { return sendTReceipt(f.intent(pin)) }
	cases := []struct {
		name string
		tune func(h *authSendHelper)
	}{
		{name: "nonzero exit with a valid receipt", tune: func(h *authSendHelper) { h.exit = sendTExit() }},
		{name: "signal exit with a valid receipt", tune: func(h *authSendHelper) { h.signal = "TERM" }},
		{name: "missing exit status", tune: func(h *authSendHelper) { h.noExit = true }},
		{name: "no receipt", tune: func(h *authSendHelper) { h.tail = func(*sendTFixture, []byte) string { return "" } }},
		{name: "receipt for another operation", tune: func(h *authSendHelper) {
			h.tail = func(f *sendTFixture, _ []byte) string { return sendTOtherOpReceipt(f, h.pin) }
		}},
		{name: "receipt missing its LF", tune: func(h *authSendHelper) {
			h.tail = func(f *sendTFixture, _ []byte) string { return strings.TrimSuffix(receipt(f, h.pin), "\n") }
		}},
		{name: "truncated receipt", tune: func(h *authSendHelper) {
			h.tail = func(f *sendTFixture, _ []byte) string { return receipt(f, h.pin)[:20] }
		}},
		{name: "challenge, receipt, one extra byte", tune: func(h *authSendHelper) {
			h.tail = func(f *sendTFixture, _ []byte) string { return receipt(f, h.pin) + "x" }
		}},
		{name: "challenge, receipt, extra LF", tune: func(h *authSendHelper) {
			h.tail = func(f *sendTFixture, _ []byte) string { return receipt(f, h.pin) + "\n" }
		}},
		{name: "receipt twice", tune: func(h *authSendHelper) {
			h.tail = func(f *sendTFixture, _ []byte) string { return receipt(f, h.pin) + receipt(f, h.pin) }
		}},
		{name: "second challenge line before the receipt", tune: func(h *authSendHelper) {
			h.tail = func(f *sendTFixture, challenge []byte) string { return string(challenge) + receipt(f, h.pin) }
		}},
		{name: "stdout bound exceeded after the challenge", tune: func(h *authSendHelper) {
			h.tail = func(*sendTFixture, []byte) string { return strings.Repeat("x", MaxSSHBinaryStageReceiptSize+1) }
		}},
		{name: "stderr bound exceeded with a valid receipt", tune: func(h *authSendHelper) { h.stderr = strings.Repeat("y", stageSendStderrMax+1) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newAuthSendHarness(t, func(helper *authSendHelper, _ *authSendAuthorizer) { tc.tune(helper) })
			res := h.send(t)
			h.requireUnknown(t, res, h.wantStdin(t, h.genuineSignature(), h.content))
			if h.auth.calls.Load() != 1 {
				t.Fatalf("authorizer calls = %d, want exactly 1", h.auth.calls.Load())
			}
			if h.helper.verified.Load() != 1 {
				t.Fatalf("helper emulation verdict %d, want accepted grant", h.helper.verified.Load())
			}
		})
	}
}

// A grant that the sender accepts structurally but the helper's trusted key
// rejects is only discoverable by the helper: the sender has already crossed
// the boundary, so the outcome is Unknown and carries no acknowledgement.
func TestSSHBinaryStageSenderAuthorizedForgedSignatureRejectedByHelperIsUnknown(t *testing.T) {
	forger := newAuthSendAuthority(t)
	h := newAuthSendHarness(t, func(_ *authSendHelper, auth *authSendAuthorizer) {
		auth.hook = func(_ context.Context, _ *authSendAuthorizer, request domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageGrant, error) {
			return forger.sign(request), nil
		}
	})
	res := h.send(t)
	forged := ed25519.Sign(forger.priv, h.wantRequest().SigningMessage())
	h.requireUnknown(t, res, h.wantStdin(t, forged, h.content))
	if h.helper.verified.Load() != 2 {
		t.Fatalf("helper emulation verdict %d, want rejected", h.helper.verified.Load())
	}
}

func TestSSHBinaryStageSenderAuthorizedSourceFailureAfterGrantIsUnknown(t *testing.T) {
	h := newAuthSendHarness(t, nil)
	partial := h.content[:3]
	h.binary = sendTBinary(h.binary.Pin(), func(context.Context) (io.ReadCloser, error) {
		return sendTScripted(partial, nil, io.EOF), nil
	})
	res := h.send(t)
	h.requireUnknown(t, res, h.wantStdin(t, h.genuineSignature(), partial))
	if h.auth.calls.Load() != 1 {
		t.Fatalf("authorizer calls = %d, want 1", h.auth.calls.Load())
	}
	if h.binary.snapshotCloses.Load() != 0 {
		t.Fatalf("borrowed snapshot closed %d times", h.binary.snapshotCloses.Load())
	}
}

// The legacy Send path is unchanged: it execs only the legacy command, never
// waits for a challenge, never calls an authorizer and never writes a grant
// frame, and it does not accept an authorized helper's transcript.
func TestSSHBinaryStageSenderAuthorizedLegacySendUnchanged(t *testing.T) {
	if SSHProbeBinaryStageAuthorizedCommand == SSHProbeBinaryStageCommand {
		t.Fatal("authorized and legacy commands must differ")
	}
	if SSHProbeBinaryStageCommand != authSendLegacyCommandLiteral || SSHProbeBinaryStageAuthorizedCommand != authSendCommandLiteral {
		t.Fatalf("command constants drifted: %q %q", SSHProbeBinaryStageCommand, SSHProbeBinaryStageAuthorizedCommand)
	}

	t.Run("legacy helper", func(t *testing.T) {
		content := []byte("phoenix pinned payload")
		binary := sendTBytesBinary(content)
		f := serveStageSend(t, sendTConfig{stdout: sendTReceiptFor(binary.Pin())})
		intent := f.intent(binary.Pin())
		res := sendTAwait(t, sendTSendAsync(t.Context(), f.sender(t), intent, binary), "legacy send")
		if res.err != nil || res.ack != (SSHBinaryStageAck{Intent: intent}) {
			t.Fatalf("legacy send %+v %v", res.ack, res.err)
		}
		if got := f.execCommands(); !slices.Equal(got, []string{SSHProbeBinaryStageCommand}) {
			t.Fatalf("exec commands %v", got)
		}
		header, err := EncodeProbeBinaryStageHeader(intent)
		if err != nil {
			t.Fatal(err)
		}
		if want := append(append([]byte(nil), header...), content...); !bytes.Equal(f.stdinBytes(), want) {
			t.Fatalf("legacy stdin = %d bytes, want header + payload only (%d)", len(f.stdinBytes()), len(want))
		}
		if bytes.Contains(f.stdinBytes(), []byte(authSendGrantMagicLiteral)) {
			t.Fatal("legacy Send wrote a grant frame")
		}
	})

	t.Run("legacy sender rejects an authorized transcript", func(t *testing.T) {
		h := newAuthSendHarness(t, func(helper *authSendHelper, _ *authSendAuthorizer) { helper.verify = false })
		ack, err := h.sender.Send(t.Context(), h.intent, h.binary)
		if err != ErrSSHBinaryStageUnknown || ack != (SSHBinaryStageAck{}) {
			t.Fatalf("legacy Send against an authorized helper %+v %v", ack, err)
		}
		h.helper.waitIdle(t, h.f)
		if got := h.f.execCommands(); !slices.Equal(got, []string{SSHProbeBinaryStageCommand}) {
			t.Fatalf("exec commands %v", got)
		}
		want := append(append([]byte(nil), h.header(t)...), h.content...)
		if !bytes.Equal(h.f.stdinBytes(), want) {
			t.Fatalf("legacy stdin = %d bytes, want header + payload only (%d)", len(h.f.stdinBytes()), len(want))
		}
		if h.auth.calls.Load() != 0 {
			t.Fatalf("legacy Send reached the authorizer %d times", h.auth.calls.Load())
		}
	})
}
