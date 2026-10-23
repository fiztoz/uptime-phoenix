package provisioning

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"io"
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

// These tests drive SendHostAction through the real loopback SSH protocol
// fixture of ssh_binary_stage_send_test.go. The remote side is an emulation of
// the host-action helper: challenge line, then stdin to EOF, then a parse of the
// action frame and grant frame with the production decoders, a grant check under
// a generated Ed25519 authority as a domain.ProbeHostActionGrant, the canonical
// receipt and an exit status. It is protocol evidence only: no OpenSSH, no
// installed helper, no systemd, no Docker and no real key is involved, and every
// key is generated per test.

// Independent literals: a changed constant must not silently change what these
// tests accept.
const (
	hostActSendCommandLiteral = "/usr/local/bin/phoenix-probe-admin stage-host-action-authorized"
	hostActSendOtherUUID      = "55555555-5555-4555-8555-555555555555"
	hostActSendContent        = "phoenix pinned payload"
)

type hostActSendResult struct {
	ack SSHHostActionAck
	err error
}

// hostActSendAuthorizer is the fake ProbeHostActionAuthorizer: it signs with the
// generated authority, counts calls and can be steered by hook.
type hostActSendAuthorizer struct {
	authority *authSendAuthority
	hook      func(ctx context.Context, a *hostActSendAuthorizer, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error)
	stdinLen  func() int

	calls   atomic.Int32
	entered chan struct{}
	once    sync.Once

	mu          sync.Mutex
	requests    []domain.ProbeHostActionAdmissionRequest
	stdinAtCall []int
	ctxErrAtEnd []error
}

func newHostActSendAuthorizer(authority *authSendAuthority) *hostActSendAuthorizer {
	return &hostActSendAuthorizer{authority: authority, entered: make(chan struct{})}
}

func hostActSendSign(a *authSendAuthority, request domain.ProbeHostActionAdmissionRequest) domain.ProbeHostActionGrant {
	grant := domain.ProbeHostActionGrant{Request: request}
	copy(grant.Signature[:], ed25519.Sign(a.priv, request.SigningMessage()))
	return grant
}

func (a *hostActSendAuthorizer) Authorize(ctx context.Context, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
	a.calls.Add(1)
	stdin := -1
	if a.stdinLen != nil {
		stdin = a.stdinLen()
	}
	a.mu.Lock()
	a.requests = append(a.requests, request)
	a.stdinAtCall = append(a.stdinAtCall, stdin)
	a.mu.Unlock()
	a.once.Do(func() { close(a.entered) })
	if a.hook == nil {
		return hostActSendSign(a.authority, request), nil
	}
	grant, err := a.hook(ctx, a, request)
	a.mu.Lock()
	a.ctxErrAtEnd = append(a.ctxErrAtEnd, ctx.Err())
	a.mu.Unlock()
	return grant, err
}

func (a *hostActSendAuthorizer) seenRequests() []domain.ProbeHostActionAdmissionRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.requests)
}

// hostActSendFuncAuthorizer lets a nil func value satisfy the authorizer port.
type hostActSendFuncAuthorizer func(ctx context.Context, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error)

func (f hostActSendFuncAuthorizer) Authorize(ctx context.Context, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
	return f(ctx, request)
}

func hostActSendBlockUntilDone(ctx context.Context, _ *hostActSendAuthorizer, _ domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
	<-ctx.Done()
	return domain.ProbeHostActionGrant{}, ctx.Err()
}

// hostActSendIntentFor builds the host-action intent for the fixture's own
// identity, owner and attempt.
func hostActSendIntentFor(f *sendTFixture, kind string) domain.ProbeHostActionIntent {
	stage := f.intent(sendTPin([]byte(hostActSendContent)))
	return domain.ProbeHostActionIntent{Identity: stage.Identity, OwnerID: stage.OwnerID, AttemptID: stage.AttemptID, Kind: kind}
}

// hostActSendHelper emulates the authorized host-action helper behind the exec.
type hostActSendHelper struct {
	authority *authSendAuthority
	nonce     [32]byte
	// kind is the action the helper expects. Empty accepts any host-action kind
	// of the fixture's identity, owner and attempt.
	kind string

	// Challenge phase.
	silent             bool
	noChallenge        bool
	noChallengeStatus  uint32
	challengeFn        func(f *sendTFixture) []byte
	split              bool
	exitAfterChallenge bool
	dieAfter           chan struct{}
	// tail is the stdout after stdin EOF; the default is the canonical receipt.
	tail   func(f *sendTFixture, intent domain.ProbeHostActionIntent, challenge []byte) string
	stderr string

	// Grant phase.
	verify bool

	// Exit behaviour (consumed by the fixture's sendExit).
	exit           *uint32
	signal         string
	noExit         bool
	stallHandshake bool

	started  atomic.Int32
	finished atomic.Int32
	verified atomic.Int32 // 1 accepted, 2 rejected

	mu       sync.Mutex
	parsed   []domain.ProbeHostActionIntent
	commands []string
}

func (h *hostActSendHelper) config() sendTConfig {
	return sendTConfig{
		handle:         h.handle,
		exit:           h.exit,
		signal:         h.signal,
		noExit:         h.noExit,
		stallHandshake: h.stallHandshake,
	}
}

func (h *hostActSendHelper) canonical() []byte {
	return EncodeProbeBinaryStageChallengeLine(h.authority.keyID, h.nonce)
}

func (h *hostActSendHelper) parsedIntents() []domain.ProbeHostActionIntent {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.parsed)
}

func (h *hostActSendHelper) handle(f *sendTFixture, ch ssh.Channel, command string, dead <-chan struct{}) {
	h.started.Add(1)
	defer h.finished.Add(1)
	h.mu.Lock()
	h.commands = append(h.commands, command)
	h.mu.Unlock()
	if h.silent {
		_, _ = io.Copy(sendTSink{f: f}, ch)
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
	var local bytes.Buffer
	if _, err := io.Copy(io.MultiWriter(sendTSink{f: f}, &local), ch); err != nil {
		return
	}
	f.mu.Lock()
	f.stdinEOF = true
	f.mu.Unlock()
	intent, ok := h.verifyStdin(f, local.Bytes(), challenge)
	if h.verify {
		if !ok {
			h.verified.Store(2)
			authSendExit(ch, 1)
			return
		}
		h.verified.Store(1)
	}
	tail := h.tail
	if tail == nil {
		tail = func(_ *sendTFixture, intent domain.ProbeHostActionIntent, _ []byte) string {
			return string(EncodeProbeHostActionReceipt(intent))
		}
	}
	_, _ = io.WriteString(ch, tail(f, intent, challenge))
	_, _ = io.WriteString(ch.Stderr(), h.stderr)
	f.sendExit(ch)
}

// verifyStdin is the helper's own check, built from the production decoders:
// the exact action frame, one grant frame whose Ed25519 signature verifies as a
// ProbeHostActionGrant under its trusted key for the request it rebuilds from
// its challenge and the decoded intent, and nothing after the grant frame.
func (h *hostActSendHelper) verifyStdin(f *sendTFixture, stdin, challenge []byte) (domain.ProbeHostActionIntent, bool) {
	var zero domain.ProbeHostActionIntent
	intent, tail, err := hostActFrameParse(stdin)
	if err != nil {
		return zero, false
	}
	h.mu.Lock()
	h.parsed = append(h.parsed, intent)
	h.mu.Unlock()
	expected := hostActSendIntentFor(f, intent.Kind)
	if h.kind != "" {
		expected.Kind = h.kind
	}
	if intent != expected || len(tail) != ProbeBinaryStageGrantFrameSize {
		return intent, false
	}
	signature, err := DecodeProbeBinaryStageGrantFrame(tail)
	if err != nil {
		return intent, false
	}
	keyID, nonce, err := ParseProbeBinaryStageChallengeLine(challenge)
	if err != nil {
		return intent, false
	}
	request := domain.ProbeHostActionAdmissionRequest{
		Intent:    intent,
		Challenge: domain.ProbeBinaryStageChallenge{AuthorityKeyID: keyID, Nonce: nonce, MarkerSHA256: intent.MarkerSHA256()},
	}
	return intent, domain.ProbeHostActionGrant{Request: request, Signature: signature}.ValidFor(h.authority.raw)
}

// waitIdle waits until every exec the fixture accepted has run its helper
// emulation to completion, so stdin observations are final.
func (h *hostActSendHelper) waitIdle(t *testing.T, f *sendTFixture) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for f.execCalls.Load() == 0 || h.started.Load() < f.execCalls.Load() || h.finished.Load() < h.started.Load() {
		if time.Now().After(deadline) {
			t.Fatalf("helper emulation did not finish: execs %d started %d finished %d", f.execCalls.Load(), h.started.Load(), h.finished.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type hostActSendHarness struct {
	f      *sendTFixture
	helper *hostActSendHelper
	auth   *hostActSendAuthorizer
	sender *SSHBinaryStageSender
	intent domain.ProbeHostActionIntent
}

func newHostActSendHarness(t *testing.T, kind string, tune func(helper *hostActSendHelper, auth *hostActSendAuthorizer)) *hostActSendHarness {
	t.Helper()
	h := &hostActSendHarness{}
	authority := newAuthSendAuthority(t)
	h.helper = &hostActSendHelper{authority: authority, nonce: authSendNonce(), kind: kind, verify: true}
	h.auth = newHostActSendAuthorizer(authority)
	if tune != nil {
		tune(h.helper, h.auth)
	}
	h.f = serveStageSend(t, h.helper.config())
	h.auth.stdinLen = func() int { return len(h.f.stdinBytes()) }
	h.intent = hostActSendIntentFor(h.f, kind)
	h.sender = h.f.sender(t)
	return h
}

func (h *hostActSendHarness) sendAsync(ctx context.Context) <-chan hostActSendResult {
	done := make(chan hostActSendResult, 1)
	go func() {
		ack, err := h.sender.SendHostAction(ctx, h.intent, h.auth)
		done <- hostActSendResult{ack: ack, err: err}
	}()
	return done
}

func hostActSendAwait(t *testing.T, done <-chan hostActSendResult, what string) hostActSendResult {
	t.Helper()
	select {
	case res := <-done:
		return res
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not return", what)
		return hostActSendResult{}
	}
}

func hostActSendExpectBlocked(t *testing.T, done <-chan hostActSendResult, what string) {
	t.Helper()
	select {
	case res := <-done:
		t.Fatalf("%s returned early: %+v %v", what, res.ack, res.err)
	case <-time.After(250 * time.Millisecond):
	}
}

func (h *hostActSendHarness) send(t *testing.T) hostActSendResult {
	t.Helper()
	return hostActSendAwait(t, h.sendAsync(t.Context()), "host action send")
}

// wantRequest builds the expected admission request independently of the
// sender: the harness intent, the helper's key id and nonce, the intent marker.
func (h *hostActSendHarness) wantRequest() domain.ProbeHostActionAdmissionRequest {
	return domain.ProbeHostActionAdmissionRequest{
		Intent: h.intent,
		Challenge: domain.ProbeBinaryStageChallenge{
			AuthorityKeyID: h.helper.authority.keyID,
			Nonce:          h.helper.nonce,
			MarkerSHA256:   h.intent.MarkerSHA256(),
		},
	}
}

func (h *hostActSendHarness) actionFrame(t *testing.T) []byte {
	t.Helper()
	frame, err := EncodeProbeHostActionFrame(h.intent)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

// wantStdin is the action frame, the literal grant magic and the signature.
// There is no payload after the grant frame.
func (h *hostActSendHarness) wantStdin(t *testing.T, signature []byte) []byte {
	t.Helper()
	want := append([]byte(nil), h.actionFrame(t)...)
	want = append(want, hostActFrameGrantMagic...)
	return append(want, signature...)
}

func (h *hostActSendHarness) genuineSignature() []byte {
	return ed25519.Sign(h.helper.authority.priv, h.wantRequest().SigningMessage())
}

// requireNotStaged asserts the pre-stdin failure class and its server-side
// evidence over rounds sequential calls: zero stdin bytes, one fixed exec on
// one connection per call (no retry).
func (h *hostActSendHarness) requireNotStaged(t *testing.T, res hostActSendResult, rounds int) {
	t.Helper()
	if res.err != ErrSSHBinaryStageNotStaged || res.ack != (SSHHostActionAck{}) {
		t.Fatalf("result %+v %v, want ErrSSHBinaryStageNotStaged", res.ack, res.err)
	}
	h.requireNoStageInput(t, rounds)
}

func (h *hostActSendHarness) requireNoStageInput(t *testing.T, rounds int) {
	t.Helper()
	h.helper.waitIdle(t, h.f)
	if got := h.f.stdinBytes(); len(got) != 0 {
		t.Fatalf("server received %d stdin bytes, want 0", len(got))
	}
	if got, want := h.f.execCommands(), slices.Repeat([]string{hostActSendCommandLiteral}, rounds); !slices.Equal(got, want) {
		t.Fatalf("exec commands %v, want %v", got, want)
	}
	if got := h.f.connCalls.Load(); got != int32(rounds) {
		t.Fatalf("connections = %d, want %d (no retry)", got, rounds)
	}
}

// requireUnknown asserts the post-stdin failure class and the exact stdin the
// server received.
func (h *hostActSendHarness) requireUnknown(t *testing.T, res hostActSendResult, wantStdin []byte) {
	t.Helper()
	if res.err != ErrSSHBinaryStageUnknown || res.ack != (SSHHostActionAck{}) {
		t.Fatalf("result %+v %v, want ErrSSHBinaryStageUnknown", res.ack, res.err)
	}
	h.helper.waitIdle(t, h.f)
	if got := h.f.stdinBytes(); !bytes.Equal(got, wantStdin) {
		t.Fatalf("server stdin = %d bytes, want %d", len(got), len(wantStdin))
	}
	if got := h.f.execCommands(); !slices.Equal(got, []string{hostActSendCommandLiteral}) {
		t.Fatalf("exec commands %v", got)
	}
	if h.f.connCalls.Load() != 1 {
		t.Fatalf("connections = %d, want 1 (no retry)", h.f.connCalls.Load())
	}
}

func (h *hostActSendHarness) requireNoIO(t *testing.T) {
	t.Helper()
	if conn, auth, exec := h.f.connCalls.Load(), h.f.authCalls.Load(), h.f.execCalls.Load(); conn != 0 || auth != 0 || exec != 0 {
		t.Fatalf("refusal performed I/O: conn %d auth %d exec %d", conn, auth, exec)
	}
	if got := h.f.stdinBytes(); len(got) != 0 {
		t.Fatalf("server received %d stdin bytes, want 0", len(got))
	}
	if h.auth.calls.Load() != 0 {
		t.Fatalf("authorizer reached %d times, want 0", h.auth.calls.Load())
	}
}

func hostActSendWaitEOF(t *testing.T, f *sendTFixture) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !f.stdinEOFSeen() {
		if time.Now().After(deadline) {
			t.Fatal("helper never saw the end of the stage input")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSendHostActionCommandConstant(t *testing.T) {
	if SSHProbeHostActionCommand != hostActSendCommandLiteral {
		t.Fatalf("host-action command constant drifted: %q", SSHProbeHostActionCommand)
	}
	for _, other := range []string{SSHProbeBinaryStageCommand, SSHProbeBinaryStageAuthorizedCommand} {
		if other == SSHProbeHostActionCommand {
			t.Fatalf("host-action command equals %q", other)
		}
	}
	if strings.ContainsAny(SSHProbeHostActionCommand, "\n;|&$`'\"<>") {
		t.Fatalf("host-action command contains shell metacharacters: %q", SSHProbeHostActionCommand)
	}
}

func TestSendHostActionHappyPath(t *testing.T) {
	for _, kind := range hostActFrameKinds {
		t.Run(kind, func(t *testing.T) {
			h := newHostActSendHarness(t, kind, nil)
			res := h.send(t)
			if res.err != nil {
				t.Fatal(res.err)
			}
			if res.ack != (SSHHostActionAck{Intent: h.intent}) {
				t.Fatalf("ack %+v", res.ack)
			}
			h.helper.waitIdle(t, h.f)

			if got := h.f.requestTypes(); !slices.Equal(got, []string{"exec"}) {
				t.Fatalf("session requests %v", got)
			}
			if got := h.f.execCommands(); !slices.Equal(got, []string{hostActSendCommandLiteral}) {
				t.Fatalf("exec commands %v", got)
			}
			if h.f.connCalls.Load() != 1 {
				t.Fatalf("connections = %d, want 1", h.f.connCalls.Load())
			}

			// Exactly one authorization, for the request rebuilt from the
			// helper's own challenge, before any stdin byte existed.
			if h.auth.calls.Load() != 1 {
				t.Fatalf("authorizer calls = %d, want 1", h.auth.calls.Load())
			}
			if requests := h.auth.seenRequests(); len(requests) != 1 || requests[0] != h.wantRequest() {
				t.Fatalf("authorizer request %+v, want %+v", requests, h.wantRequest())
			}
			h.auth.mu.Lock()
			stdinAtCall := slices.Clone(h.auth.stdinAtCall)
			h.auth.mu.Unlock()
			if !slices.Equal(stdinAtCall, []int{0}) {
				t.Fatalf("stdin bytes visible when authorizer ran: %v, want [0]", stdinAtCall)
			}

			// Server stdin is exactly the action frame and the 72-byte grant
			// frame, then EOF: no payload of any kind.
			want := h.wantStdin(t, h.genuineSignature())
			got := h.f.stdinBytes()
			if !bytes.Equal(got, want) {
				t.Fatalf("server stdin = %d bytes, want %d", len(got), len(want))
			}
			if !bytes.HasPrefix(got, []byte(hostActFrameMagicLiteral+string(rune(len(kind)))+kind)) || bytes.HasPrefix(got, []byte(hostActFrameStageMagic)) {
				t.Fatal("stdin does not start with the action prefix and kind")
			}
			frame := h.actionFrame(t)
			if len(got) != len(frame)+ProbeBinaryStageGrantFrameSize {
				t.Fatalf("stdin carries %d bytes beyond the action and grant frames", len(got)-len(frame)-ProbeBinaryStageGrantFrameSize)
			}
			if !h.f.stdinEOFSeen() {
				t.Fatal("missing stdin EOF")
			}
			if h.helper.verified.Load() != 1 {
				t.Fatalf("helper emulation verdict %d, want accepted grant", h.helper.verified.Load())
			}
			if parsed := h.helper.parsedIntents(); len(parsed) != 1 || parsed[0] != h.intent {
				t.Fatalf("helper decoded %+v, want the intent", parsed)
			}
		})
	}
}

func TestSendHostActionChallengeSplitAcrossWrites(t *testing.T) {
	h := newHostActSendHarness(t, hostActFrameKindUnit, func(helper *hostActSendHelper, _ *hostActSendAuthorizer) { helper.split = true })
	res := h.send(t)
	if res.err != nil || res.ack != (SSHHostActionAck{Intent: h.intent}) {
		t.Fatalf("split challenge %+v %v", res.ack, res.err)
	}
	if requests := h.auth.seenRequests(); len(requests) != 1 || requests[0] != h.wantRequest() {
		t.Fatalf("authorizer request %+v", requests)
	}
	h.helper.waitIdle(t, h.f)
	if want := h.wantStdin(t, h.genuineSignature()); !bytes.Equal(h.f.stdinBytes(), want) {
		t.Fatalf("server stdin = %d bytes, want %d", len(h.f.stdinBytes()), len(want))
	}
}

// The sender must not stream any input while the authorization is pending:
// the action frame and the grant both wait for the grant.
func TestSendHostActionNoStdinBeforeGrant(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseGrant := func() { releaseOnce.Do(func() { close(release) }) }
	h := newHostActSendHarness(t, hostActFrameKindService, func(_ *hostActSendHelper, auth *hostActSendAuthorizer) {
		auth.hook = func(ctx context.Context, a *hostActSendAuthorizer, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
			select {
			case <-release:
				return hostActSendSign(a.authority, request), nil
			case <-ctx.Done():
				return domain.ProbeHostActionGrant{}, ctx.Err()
			}
		}
	})
	t.Cleanup(releaseGrant)
	done := h.sendAsync(t.Context())
	select {
	case <-h.auth.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("authorizer call not reached")
	}
	hostActSendExpectBlocked(t, done, "send while authorization is pending")
	if got := h.f.stdinBytes(); len(got) != 0 {
		t.Fatalf("%d stdin bytes sent before the grant", len(got))
	}
	releaseGrant()
	res := hostActSendAwait(t, done, "send after grant")
	if res.err != nil || res.ack != (SSHHostActionAck{Intent: h.intent}) {
		t.Fatalf("after grant %+v %v", res.ack, res.err)
	}
	if h.auth.calls.Load() != 1 {
		t.Fatalf("authorizer calls = %d, want 1", h.auth.calls.Load())
	}
}

func TestSendHostActionRefusesInvalidInputWithZeroConnections(t *testing.T) {
	h := newHostActSendHarness(t, hostActFrameKindService, nil)
	ctx := t.Context()
	refused := func(name string, ack SSHHostActionAck, err error) {
		t.Helper()
		if err != ErrSSHBinaryStageInput || ack != (SSHHostActionAck{}) {
			t.Fatalf("%s %+v %v", name, ack, err)
		}
	}

	var nilSender *SSHBinaryStageSender
	ack, err := nilSender.SendHostAction(ctx, h.intent, h.auth)
	refused("nil sender", ack, err)
	ack, err = (&SSHBinaryStageSender{}).SendHostAction(ctx, h.intent, h.auth)
	refused("zero sender", ack, err)

	var nilCtx *sendTNilContext
	ack, err = h.sender.SendHostAction(nilCtx, h.intent, h.auth)
	refused("typed nil ctx", ack, err)

	var typedNilPointer *hostActSendAuthorizer
	var typedNilFunc hostActSendFuncAuthorizer
	for name, authorizer := range map[string]ProbeHostActionAuthorizer{
		"untyped nil authorizer":      nil,
		"typed nil pointer authorize": typedNilPointer,
		"typed nil func authorizer":   typedNilFunc,
	} {
		ack, err = h.sender.SendHostAction(ctx, h.intent, authorizer)
		refused(name, ack, err)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if ack, err := h.sender.SendHostAction(canceled, h.intent, h.auth); err != context.Canceled || ack != (SSHHostActionAck{}) {
		t.Fatalf("canceled ctx %+v %v", ack, err)
	}
	expired, stop := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer stop()
	if ack, err := h.sender.SendHostAction(expired, h.intent, h.auth); err != context.DeadlineExceeded || ack != (SSHHostActionAck{}) {
		t.Fatalf("expired ctx %+v %v", ack, err)
	}

	_, otherSigner := deploymentKey(t)
	otherFP := ssh.FingerprintSHA256(otherSigner.PublicKey())
	mutations := map[string]func(*domain.ProbeHostActionIntent){
		"artifact_directory kind": func(i *domain.ProbeHostActionIntent) { i.Kind = hostActFrameKindArtifactDir },
		"image_cache kind":        func(i *domain.ProbeHostActionIntent) { i.Kind = "image_cache" },
		"container kind":          func(i *domain.ProbeHostActionIntent) { i.Kind = "container" },
		"empty kind":              func(i *domain.ProbeHostActionIntent) { i.Kind = "" },
		"near-miss kind":          func(i *domain.ProbeHostActionIntent) { i.Kind = "Service" },
		"docker mode":             func(i *domain.ProbeHostActionIntent) { i.Identity.Mode = domain.ProbeInstallDocker },
		"non-root user":           func(i *domain.ProbeHostActionIntent) { i.Identity.Host.User = "deploy" },
		"empty owner":             func(i *domain.ProbeHostActionIntent) { i.OwnerID = "" },
		"malformed attempt":       func(i *domain.ProbeHostActionIntent) { i.AttemptID = "not-an-id" },
		"invalid binary pin":      func(i *domain.ProbeHostActionIntent) { i.Identity.Binary.Size = 0 },
		"host key pin binding":    func(i *domain.ProbeHostActionIntent) { i.Identity.HostKeyFingerprint = otherFP },
		"client key pin binding":  func(i *domain.ProbeHostActionIntent) { i.Identity.ClientKeyFingerprint = otherFP },
	}
	for name, mutate := range mutations {
		intent := h.intent
		mutate(&intent)
		ack, err := h.sender.SendHostAction(ctx, intent, h.auth)
		refused(name, ack, err)
	}
	ack, err = h.sender.SendHostAction(ctx, domain.ProbeHostActionIntent{}, h.auth)
	refused("zero intent", ack, err)

	h.requireNoIO(t)
}

// Failures before exec keep the sender's pre-exec sentinels and never reach
// the authorizer.
func TestSendHostActionPreExecFailuresKeepSendSentinels(t *testing.T) {
	requirePreExec := func(t *testing.T, h *hostActSendHarness, res hostActSendResult, want error) {
		t.Helper()
		if res.err != want || res.ack != (SSHHostActionAck{}) {
			t.Fatalf("result %+v %v, want %v", res.ack, res.err, want)
		}
		if h.auth.calls.Load() != 0 {
			t.Fatalf("authorizer reached %d times before exec", h.auth.calls.Load())
		}
		if h.f.execCalls.Load() != 0 || len(h.f.stdinBytes()) != 0 {
			t.Fatalf("exec %d or stdin bytes before exec", h.f.execCalls.Load())
		}
	}

	t.Run("host key rejected before authentication", func(t *testing.T) {
		h := newHostActSendHarness(t, hostActFrameKindData, nil)
		_, otherSigner := deploymentKey(t)
		wrongFP := ssh.FingerprintSHA256(otherSigner.PublicKey())
		sender, err := NewSSHBinaryStageSender(t.Context(), h.f.clientKey, wrongFP, h.f.clientFP(), h.f.policy())
		if err != nil {
			t.Fatal(err)
		}
		h.sender = sender
		h.intent.Identity.HostKeyFingerprint = wrongFP
		requirePreExec(t, h, h.send(t), domain.ErrProbeSSHTrust)
		if h.f.authCalls.Load() != 0 {
			t.Fatalf("client authenticated before host trust: %d", h.f.authCalls.Load())
		}
	})

	t.Run("destination rejected by policy", func(t *testing.T) {
		h := newHostActSendHarness(t, hostActFrameKindData, nil)
		sender, err := NewSSHBinaryStageSender(t.Context(), h.f.clientKey, h.f.hostFP(), h.f.clientFP(), probe.EndpointPolicy{})
		if err != nil {
			t.Fatal(err)
		}
		h.sender = sender
		requirePreExec(t, h, h.send(t), domain.ErrProbeSSHDestination)
		if h.f.connCalls.Load() != 0 {
			t.Fatalf("dialed a rejected destination: %d", h.f.connCalls.Load())
		}
	})

	t.Run("connect failure", func(t *testing.T) {
		h := newHostActSendHarness(t, hostActFrameKindData, nil)
		h.intent.Identity.Host.Address = authSendDeadAddress(t)
		requirePreExec(t, h, h.send(t), domain.ErrProbeSSHConnect)
		if h.f.connCalls.Load() != 0 {
			t.Fatalf("fixture contacted: %d", h.f.connCalls.Load())
		}
	})

	t.Run("cancel before exec is the bare context sentinel", func(t *testing.T) {
		h := newHostActSendHarness(t, hostActFrameKindData, func(helper *hostActSendHelper, _ *hostActSendAuthorizer) { helper.stallHandshake = true })
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := h.sendAsync(ctx)
		sendTWaitCount(t, &h.f.connCalls, "connection")
		cancel()
		requirePreExec(t, h, hostActSendAwait(t, done, "pre-exec cancel"), context.Canceled)
	})
}

// Failures after exec but before the first stdin byte are NotStaged for every
// kind: the helper was started but was given nothing to apply.
func TestSendHostActionHelperRefusalBeforeStdinIsNotStaged(t *testing.T) {
	keyIDStart := len(stageChallengePrefix)
	cases := []struct {
		name string
		tune func(helper *hostActSendHelper)
		// racy marks the one case where a helper that exits right after a valid
		// challenge can be observed dead before or after the authorizer runs.
		// NotStaged and zero stdin hold either way.
		racy bool
	}{
		{name: "helper exits 1 without a challenge", tune: func(helper *hostActSendHelper) {
			helper.noChallenge = true
			helper.noChallengeStatus = 1
		}},
		{name: "helper exits 0 without a challenge", tune: func(helper *hostActSendHelper) { helper.noChallenge = true }},
		{name: "malformed challenge line", tune: func(helper *hostActSendHelper) {
			helper.challengeFn = func(*sendTFixture) []byte { return []byte("this is not a challenge line\n") }
		}},
		{name: "non-hex key id", tune: func(helper *hostActSendHelper) {
			helper.challengeFn = func(*sendTFixture) []byte {
				line := helper.canonical()
				line[keyIDStart] = 'g'
				return line
			}
		}},
		{name: "zero nonce", tune: func(helper *hostActSendHelper) {
			helper.challengeFn = func(*sendTFixture) []byte {
				return []byte(stageChallengePrefix + helper.authority.keyID + stageChallengeMiddle + strings.Repeat("0", 64) + stageChallengeSuffix)
			}
		}},
		{name: "extra byte after the challenge line", tune: func(helper *hostActSendHelper) {
			helper.challengeFn = func(*sendTFixture) []byte { return append(helper.canonical(), 'x') }
		}},
		{name: "a host-action receipt in place of the challenge", tune: func(helper *hostActSendHelper) {
			helper.challengeFn = func(f *sendTFixture) []byte {
				return EncodeProbeHostActionReceipt(hostActSendIntentFor(f, hostActFrameKindService))
			}
		}},
		{name: "helper exits right after the challenge", racy: true, tune: func(helper *hostActSendHelper) { helper.exitAfterChallenge = true }},
	}
	for _, kind := range hostActFrameKinds {
		for _, tc := range cases {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				h := newHostActSendHarness(t, kind, func(helper *hostActSendHelper, _ *hostActSendAuthorizer) { tc.tune(helper) })
				start := time.Now()
				res := h.send(t)
				if elapsed := time.Since(start); elapsed > 10*time.Second {
					t.Fatalf("classification took %v; want prompt return", elapsed)
				}
				h.requireNotStaged(t, res, 1)
				if !tc.racy && h.auth.calls.Load() != 0 {
					t.Fatalf("authorizer called %d times for a refused challenge", h.auth.calls.Load())
				}
			})
		}
	}
}

func TestSendHostActionAuthorizerErrorIsNotStaged(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{name: "plain error", err: errors.New("journal attempt already consumed")},
		{name: "context canceled sentinel without a canceled context", err: context.Canceled},
		{name: "deadline exceeded sentinel without an expired context", err: context.DeadlineExceeded},
		{name: "domain sentinel", err: domain.ErrProbeArtifactStorage},
		{name: "write conflict sentinel", err: domain.ErrProbeBinaryStageWriteConflict},
	}
	for _, kind := range hostActFrameKinds {
		for _, tc := range cases {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				h := newHostActSendHarness(t, kind, func(_ *hostActSendHelper, auth *hostActSendAuthorizer) {
					auth.hook = func(context.Context, *hostActSendAuthorizer, domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
						return domain.ProbeHostActionGrant{}, tc.err
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
}

func TestSendHostActionGrantForDifferentRequestIsNotStaged(t *testing.T) {
	otherDigest := strings.Repeat("ab", 32)
	otherKind := map[string]string{
		hostActFrameKindData:    hostActFrameKindService,
		hostActFrameKindUnit:    hostActFrameKindData,
		hostActFrameKindService: hostActFrameKindUnit,
	}
	type grantFn func(a *hostActSendAuthorizer, r domain.ProbeHostActionAdmissionRequest) domain.ProbeHostActionGrant
	cases := []struct {
		name  string
		grant grantFn
	}{
		{"different nonce, validly signed", func(a *hostActSendAuthorizer, r domain.ProbeHostActionAdmissionRequest) domain.ProbeHostActionGrant {
			r.Challenge.Nonce[0] ^= 0xff
			return hostActSendSign(a.authority, r)
		}},
		{"different authority key id, validly signed", func(a *hostActSendAuthorizer, r domain.ProbeHostActionAdmissionRequest) domain.ProbeHostActionGrant {
			r.Challenge.AuthorityKeyID = otherDigest
			return hostActSendSign(a.authority, r)
		}},
		{"different attempt id, validly signed", func(a *hostActSendAuthorizer, r domain.ProbeHostActionAdmissionRequest) domain.ProbeHostActionGrant {
			r.Intent.AttemptID = hostActSendOtherUUID
			r.Challenge.MarkerSHA256 = r.Intent.MarkerSHA256()
			return hostActSendSign(a.authority, r)
		}},
		{"different owner id, validly signed", func(a *hostActSendAuthorizer, r domain.ProbeHostActionAdmissionRequest) domain.ProbeHostActionGrant {
			r.Intent.OwnerID = hostActSendOtherUUID
			r.Challenge.MarkerSHA256 = r.Intent.MarkerSHA256()
			return hostActSendSign(a.authority, r)
		}},
		{"different operation id, validly signed", func(a *hostActSendAuthorizer, r domain.ProbeHostActionAdmissionRequest) domain.ProbeHostActionGrant {
			r.Intent.Identity.OperationID = hostActSendOtherUUID
			r.Challenge.MarkerSHA256 = r.Intent.MarkerSHA256()
			return hostActSendSign(a.authority, r)
		}},
		{"zero grant", func(*hostActSendAuthorizer, domain.ProbeHostActionAdmissionRequest) domain.ProbeHostActionGrant {
			return domain.ProbeHostActionGrant{}
		}},
		{"signature only, request missing", func(a *hostActSendAuthorizer, r domain.ProbeHostActionAdmissionRequest) domain.ProbeHostActionGrant {
			return domain.ProbeHostActionGrant{Signature: hostActSendSign(a.authority, r).Signature}
		}},
		{"matching request with a zero signature", func(_ *hostActSendAuthorizer, r domain.ProbeHostActionAdmissionRequest) domain.ProbeHostActionGrant {
			return domain.ProbeHostActionGrant{Request: r}
		}},
	}
	for _, kind := range hostActFrameKinds {
		cases := append(slices.Clone(cases), struct {
			name  string
			grant grantFn
		}{"grant for another kind of the same operation, validly signed", func(a *hostActSendAuthorizer, r domain.ProbeHostActionAdmissionRequest) domain.ProbeHostActionGrant {
			r.Intent.Kind = otherKind[kind]
			r.Challenge.MarkerSHA256 = r.Intent.MarkerSHA256()
			return hostActSendSign(a.authority, r)
		}})
		for _, tc := range cases {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				h := newHostActSendHarness(t, kind, func(_ *hostActSendHelper, auth *hostActSendAuthorizer) {
					auth.hook = func(_ context.Context, a *hostActSendAuthorizer, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
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
}

func TestSendHostActionCancelDuringAuthorization(t *testing.T) {
	t.Run("caller cancel while the authorizer blocks", func(t *testing.T) {
		h := newHostActSendHarness(t, hostActFrameKindData, func(_ *hostActSendHelper, auth *hostActSendAuthorizer) { auth.hook = hostActSendBlockUntilDone })
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := h.sendAsync(ctx)
		select {
		case <-h.auth.entered:
		case <-time.After(10 * time.Second):
			t.Fatal("authorizer call not reached")
		}
		hostActSendExpectBlocked(t, done, "send while the authorizer blocks")
		cancel()
		res := hostActSendAwait(t, done, "cancel during authorization")
		// Post-exec, pre-stdin: NotStaged, neither a bare context error nor Unknown.
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
		h := newHostActSendHarness(t, hostActFrameKindService, func(_ *hostActSendHelper, auth *hostActSendAuthorizer) { auth.hook = hostActSendBlockUntilDone })
		ctx, cancel := context.WithTimeout(t.Context(), stageSendAuthorizeMinBudget/2)
		defer cancel()
		res := hostActSendAwait(t, h.sendAsync(ctx), "short deadline before authorization")
		if h.auth.calls.Load() != 0 {
			t.Fatalf("authorizer calls = %d, want 0 (remaining budget below the floor)", h.auth.calls.Load())
		}
		h.requireNotStaged(t, res, 1)
	})

	t.Run("authorizer ignores cancellation and still returns a valid grant", func(t *testing.T) {
		h := newHostActSendHarness(t, hostActFrameKindUnit, func(_ *hostActSendHelper, auth *hostActSendAuthorizer) {
			auth.hook = func(ctx context.Context, a *hostActSendAuthorizer, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
				<-ctx.Done()
				return hostActSendSign(a.authority, request), nil
			}
		})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := h.sendAsync(ctx)
		select {
		case <-h.auth.entered:
		case <-time.After(10 * time.Second):
			t.Fatal("authorizer call not reached")
		}
		cancel()
		h.requireNotStaged(t, hostActSendAwait(t, done, "cancel with a late grant"), 1)
	})

	t.Run("cancel while the challenge never arrives", func(t *testing.T) {
		h := newHostActSendHarness(t, hostActFrameKindData, func(helper *hostActSendHelper, _ *hostActSendAuthorizer) { helper.silent = true })
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := h.sendAsync(ctx)
		sendTWaitCount(t, &h.f.execCalls, "exec request")
		hostActSendExpectBlocked(t, done, "send while the helper is silent")
		cancel()
		h.requireNotStaged(t, hostActSendAwait(t, done, "cancel while waiting for the challenge"), 1)
		if h.auth.calls.Load() != 0 {
			t.Fatalf("authorizer called %d times without a challenge", h.auth.calls.Load())
		}
	})
}

// A helper that dies while the grant is being obtained is observed after
// Authorize returns and refused before any stdin byte: a definite NotStaged.
func TestSendHostActionHelperDiesDuringAuthorization(t *testing.T) {
	var helperRef *hostActSendHelper
	h := newHostActSendHarness(t, hostActFrameKindService, func(helper *hostActSendHelper, auth *hostActSendAuthorizer) {
		helperRef = helper
		helper.dieAfter = make(chan struct{})
		auth.hook = func(_ context.Context, a *hostActSendAuthorizer, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
			close(helperRef.dieAfter)
			deadline := time.Now().Add(5 * time.Second)
			for helperRef.finished.Load() < 1 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			// Let the closed channel reach the client before the grant returns.
			time.Sleep(200 * time.Millisecond)
			return hostActSendSign(a.authority, request), nil
		}
	})
	res := h.send(t)
	h.requireNotStaged(t, res, 1)
	if h.auth.calls.Load() != 1 {
		t.Fatalf("authorizer calls = %d, want 1", h.auth.calls.Load())
	}
}

// Everything after the first stdin byte is Unknown: the helper may have applied.
func TestSendHostActionPostGrantFailuresAreUnknown(t *testing.T) {
	type tuner func(h *hostActSendHelper)
	receiptFor := func(kind string) func(*sendTFixture, domain.ProbeHostActionIntent, []byte) string {
		return func(f *sendTFixture, _ domain.ProbeHostActionIntent, _ []byte) string {
			return string(EncodeProbeHostActionReceipt(hostActSendIntentFor(f, kind)))
		}
	}
	otherKind := map[string]string{
		hostActFrameKindData:    hostActFrameKindService,
		hostActFrameKindUnit:    hostActFrameKindData,
		hostActFrameKindService: hostActFrameKindUnit,
	}
	for _, kind := range hostActFrameKinds {
		cases := []struct {
			name string
			tune tuner
		}{
			{"nonzero exit with a valid receipt", func(h *hostActSendHelper) { h.exit = sendTExit() }},
			{"nonzero exit with no receipt", func(h *hostActSendHelper) {
				h.exit = sendTExit()
				h.tail = func(*sendTFixture, domain.ProbeHostActionIntent, []byte) string { return "" }
			}},
			{"signal exit with a valid receipt", func(h *hostActSendHelper) { h.signal = "TERM" }},
			{"missing exit status", func(h *hostActSendHelper) { h.noExit = true }},
			{"no receipt", func(h *hostActSendHelper) {
				h.tail = func(*sendTFixture, domain.ProbeHostActionIntent, []byte) string { return "" }
			}},
			{"receipt for another kind of the same operation", func(h *hostActSendHelper) { h.tail = receiptFor(otherKind[kind]) }},
			{"receipt for another operation", func(h *hostActSendHelper) {
				h.tail = func(f *sendTFixture, intent domain.ProbeHostActionIntent, _ []byte) string {
					intent.Identity.OperationID = hostActSendOtherUUID
					return string(EncodeProbeHostActionReceipt(intent))
				}
			}},
			{"receipt for another attempt", func(h *hostActSendHelper) {
				h.tail = func(f *sendTFixture, intent domain.ProbeHostActionIntent, _ []byte) string {
					intent.AttemptID = hostActSendOtherUUID
					return string(EncodeProbeHostActionReceipt(intent))
				}
			}},
			{"a binary-stage receipt", func(h *hostActSendHelper) {
				h.tail = func(f *sendTFixture, _ domain.ProbeHostActionIntent, _ []byte) string {
					return sendTReceipt(f.intent(sendTPin([]byte(hostActSendContent))))
				}
			}},
			{"receipt missing its LF", func(h *hostActSendHelper) {
				h.tail = func(_ *sendTFixture, intent domain.ProbeHostActionIntent, _ []byte) string {
					return strings.TrimSuffix(string(EncodeProbeHostActionReceipt(intent)), "\n")
				}
			}},
			{"truncated receipt", func(h *hostActSendHelper) {
				h.tail = func(_ *sendTFixture, intent domain.ProbeHostActionIntent, _ []byte) string {
					return string(EncodeProbeHostActionReceipt(intent))[:20]
				}
			}},
			{"receipt followed by an extra byte", func(h *hostActSendHelper) {
				h.tail = func(_ *sendTFixture, intent domain.ProbeHostActionIntent, _ []byte) string {
					return string(EncodeProbeHostActionReceipt(intent)) + "x"
				}
			}},
			{"receipt followed by an extra LF", func(h *hostActSendHelper) {
				h.tail = func(_ *sendTFixture, intent domain.ProbeHostActionIntent, _ []byte) string {
					return string(EncodeProbeHostActionReceipt(intent)) + "\n"
				}
			}},
			{"receipt twice", func(h *hostActSendHelper) {
				h.tail = func(_ *sendTFixture, intent domain.ProbeHostActionIntent, _ []byte) string {
					r := string(EncodeProbeHostActionReceipt(intent))
					return r + r
				}
			}},
			{"second challenge line before the receipt", func(h *hostActSendHelper) {
				h.tail = func(_ *sendTFixture, intent domain.ProbeHostActionIntent, challenge []byte) string {
					return string(challenge) + string(EncodeProbeHostActionReceipt(intent))
				}
			}},
			{"uppercase marker in the receipt", func(h *hostActSendHelper) {
				h.tail = func(_ *sendTFixture, intent domain.ProbeHostActionIntent, _ []byte) string {
					r := string(EncodeProbeHostActionReceipt(intent))
					i := strings.Index(r, hostActFrameReceiptMiddle) + len(hostActFrameReceiptMiddle)
					return r[:i] + strings.ToUpper(r[i:])
				}
			}},
			{"stdout bound exceeded after the challenge", func(h *hostActSendHelper) {
				h.tail = func(*sendTFixture, domain.ProbeHostActionIntent, []byte) string {
					return strings.Repeat("x", MaxSSHBinaryStageReceiptSize+1)
				}
			}},
			{"stderr bound exceeded with a valid receipt", func(h *hostActSendHelper) { h.stderr = strings.Repeat("y", stageSendStderrMax+1) }},
		}
		for _, tc := range cases {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				h := newHostActSendHarness(t, kind, func(helper *hostActSendHelper, _ *hostActSendAuthorizer) { tc.tune(helper) })
				res := h.send(t)
				h.requireUnknown(t, res, h.wantStdin(t, h.genuineSignature()))
				if h.auth.calls.Load() != 1 {
					t.Fatalf("authorizer calls = %d, want exactly 1", h.auth.calls.Load())
				}
				if h.helper.verified.Load() != 1 {
					t.Fatalf("helper emulation verdict %d, want accepted grant", h.helper.verified.Load())
				}
			})
		}
	}
}

// A grant the sender accepts structurally but the helper's trusted key rejects
// is discoverable only by the helper, after the input was sent: Unknown.
func TestSendHostActionForgedGrantRejectedByHelperIsUnknown(t *testing.T) {
	forger := newAuthSendAuthority(t)
	h := newHostActSendHarness(t, hostActFrameKindUnit, func(_ *hostActSendHelper, auth *hostActSendAuthorizer) {
		auth.hook = func(_ context.Context, _ *hostActSendAuthorizer, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
			return hostActSendSign(forger, request), nil
		}
	})
	res := h.send(t)
	forged := ed25519.Sign(forger.priv, h.wantRequest().SigningMessage())
	h.requireUnknown(t, res, h.wantStdin(t, forged))
	if h.helper.verified.Load() != 2 {
		t.Fatalf("helper emulation verdict %d, want rejected", h.helper.verified.Load())
	}
}

// End to end cross-domain check: a signature by the genuine authority over the
// binary-stage message for the same identity, owner and attempt is delivered as
// a host-action grant and is refused by the helper.
func TestSendHostActionBinaryStageSignatureIsRejectedByHelper(t *testing.T) {
	for _, kind := range hostActFrameKinds {
		t.Run(kind, func(t *testing.T) {
			h := newHostActSendHarness(t, kind, func(_ *hostActSendHelper, auth *hostActSendAuthorizer) {
				auth.hook = func(_ context.Context, a *hostActSendAuthorizer, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
					stage := domain.ProbeBinaryStageIntent{Identity: request.Intent.Identity, OwnerID: request.Intent.OwnerID, AttemptID: request.Intent.AttemptID}
					stageRequest := domain.ProbeBinaryStageAdmissionRequest{Intent: stage, Challenge: domain.ProbeBinaryStageChallenge{
						AuthorityKeyID: request.Challenge.AuthorityKeyID, Nonce: request.Challenge.Nonce, MarkerSHA256: stage.MarkerSHA256(),
					}}
					grant := domain.ProbeHostActionGrant{Request: request}
					copy(grant.Signature[:], ed25519.Sign(a.authority.priv, stageRequest.SigningMessage()))
					return grant, nil
				}
			})
			res := h.send(t)
			stageSignature := func() []byte {
				stage := domain.ProbeBinaryStageIntent{Identity: h.intent.Identity, OwnerID: h.intent.OwnerID, AttemptID: h.intent.AttemptID}
				stageRequest := domain.ProbeBinaryStageAdmissionRequest{Intent: stage, Challenge: domain.ProbeBinaryStageChallenge{
					AuthorityKeyID: h.helper.authority.keyID, Nonce: h.helper.nonce, MarkerSHA256: stage.MarkerSHA256(),
				}}
				return ed25519.Sign(h.helper.authority.priv, stageRequest.SigningMessage())
			}()
			h.requireUnknown(t, res, h.wantStdin(t, stageSignature))
			if h.helper.verified.Load() != 2 {
				t.Fatalf("helper emulation verdict %d, want the cross-domain signature rejected", h.helper.verified.Load())
			}
		})
	}
}

// Once the first stdin byte has been delivered, cancellation can no longer
// prove the host untouched: Unknown, never NotStaged.
func TestSendHostActionCancelAfterStdinIsUnknown(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHelper := func() { releaseOnce.Do(func() { close(release) }) }
	h := newHostActSendHarness(t, hostActFrameKindService, func(helper *hostActSendHelper, _ *hostActSendAuthorizer) {
		helper.tail = func(_ *sendTFixture, intent domain.ProbeHostActionIntent, _ []byte) string {
			<-release
			return string(EncodeProbeHostActionReceipt(intent))
		}
	})
	// Registered after the fixture's own cleanup, so it runs first and lets the
	// held helper emulation finish before the fixture is torn down.
	t.Cleanup(releaseHelper)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := h.sendAsync(ctx)
	hostActSendWaitEOF(t, h.f)
	hostActSendExpectBlocked(t, done, "send while the helper holds its receipt")
	cancel()
	res := hostActSendAwait(t, done, "cancel after stdin")
	releaseHelper() // let the held emulation finish so stdin observations are final
	h.requireUnknown(t, res, h.wantStdin(t, h.genuineSignature()))
}

// Two consecutive sends are independent calls: each connects, execs once,
// authorizes once and sends its own frame; nothing is cached or retried.
func TestSendHostActionSequentialCallsAreIndependent(t *testing.T) {
	h := newHostActSendHarness(t, "", nil)
	var wantStdin []byte
	for i, kind := range hostActFrameKinds {
		intent := hostActSendIntentFor(h.f, kind)
		ack, err := h.sender.SendHostAction(t.Context(), intent, h.auth)
		if err != nil || ack != (SSHHostActionAck{Intent: intent}) {
			t.Fatalf("send %d (%s): %+v %v", i, kind, ack, err)
		}
		frame, err := EncodeProbeHostActionFrame(intent)
		if err != nil {
			t.Fatal(err)
		}
		request := h.auth.seenRequests()[i]
		wantRequest := domain.ProbeHostActionAdmissionRequest{Intent: intent, Challenge: domain.ProbeBinaryStageChallenge{
			AuthorityKeyID: h.helper.authority.keyID, Nonce: h.helper.nonce, MarkerSHA256: intent.MarkerSHA256(),
		}}
		if request != wantRequest {
			t.Fatalf("send %d authorizer request %+v", i, request)
		}
		wantStdin = append(wantStdin, frame...)
		wantStdin = append(wantStdin, hostActFrameGrantMagic...)
		wantStdin = append(wantStdin, ed25519.Sign(h.helper.authority.priv, wantRequest.SigningMessage())...)
	}
	h.helper.waitIdle(t, h.f)
	if got := h.f.stdinBytes(); !bytes.Equal(got, wantStdin) {
		t.Fatalf("stdin across sends = %d bytes, want %d", len(got), len(wantStdin))
	}
	if got := h.f.connCalls.Load(); got != int32(len(hostActFrameKinds)) {
		t.Fatalf("connections = %d, want one per send", got)
	}
	if got, want := h.f.execCommands(), slices.Repeat([]string{hostActSendCommandLiteral}, len(hostActFrameKinds)); !slices.Equal(got, want) {
		t.Fatalf("exec commands %v", got)
	}
	if h.auth.calls.Load() != int32(len(hostActFrameKinds)) {
		t.Fatalf("authorizer calls = %d, want one per send", h.auth.calls.Load())
	}
}
