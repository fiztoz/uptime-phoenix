package bootstrap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// Mock/refusal tests for the authorized host-action helper. They drive only the
// injected seams (key loader, host keys, entropy, apply) plus stdin/stdout
// fakes. The real-effect lifecycle lives in
// probe_host_action_authorized_integration_test.go. Keys, nonces and grants are
// generated in-test; nothing touches the network, root or a real /etc.

// hactOtherFingerprint is an SSH host-key fingerprint that is not the one every
// test intent pins.
var hactOtherFingerprint = "SHA256:" + base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))

var hactKinds = []string{domain.ProbeHostActionDataDirectory, domain.ProbeHostActionUnitFile, domain.ProbeHostActionService}

// hactIntent derives a valid root host-action intent from helperIntent.
func hactIntent(t *testing.T, kind string) domain.ProbeHostActionIntent {
	t.Helper()
	base := authzRootIntent()
	intent := domain.ProbeHostActionIntent{Identity: base.Identity, OwnerID: base.OwnerID, AttemptID: base.AttemptID, Kind: kind}
	if !intent.Valid() || intent.Identity.Host.User != "root" || intent.MarkerSHA256() == "" {
		t.Fatalf("fixture %q intent is not a valid root host action", kind)
	}
	return intent
}

// hactVariant returns a valid intent differing from base by one mutation.
func hactVariant(t *testing.T, base domain.ProbeHostActionIntent, mutate func(*domain.ProbeHostActionIntent)) domain.ProbeHostActionIntent {
	t.Helper()
	v := base
	mutate(&v)
	if v == base || !v.Valid() || v.MarkerSHA256() == base.MarkerSHA256() {
		t.Fatal("fixture variant must be a distinct, valid intent with a distinct marker")
	}
	return v
}

func hactRequest(k authzKeys, intent domain.ProbeHostActionIntent, nonce [32]byte) domain.ProbeHostActionAdmissionRequest {
	return domain.ProbeHostActionAdmissionRequest{
		Intent:    intent,
		Challenge: domain.ProbeBinaryStageChallenge{AuthorityKeyID: k.id, Nonce: nonce, MarkerSHA256: intent.MarkerSHA256()},
	}
}

// hactSign signs exactly the message the helper verifies.
func hactSign(t *testing.T, k authzKeys, intent domain.ProbeHostActionIntent, nonce [32]byte) [64]byte {
	t.Helper()
	message := hactRequest(k, intent, nonce).SigningMessage()
	if message == nil {
		t.Fatal("test request has no signing message")
	}
	return authzSignRaw(k, message)
}

// hactManualMessage mirrors the documented host-action message without the
// validity gate, so a correctly signed but inadmissible request can be built.
func hactManualMessage(k authzKeys, nonce [32]byte, markerSHA256 string) []byte {
	key, _ := hex.DecodeString(k.id)
	marker, _ := hex.DecodeString(markerSHA256)
	message := []byte("phoenix-host-action-admission-v1\x00")
	message = append(message, key...)
	message = append(message, nonce[:]...)
	return append(message, marker...)
}

func hactStageHeader(t *testing.T, intent domain.ProbeHostActionIntent) []byte {
	t.Helper()
	header, err := provisioning.EncodeProbeBinaryStageHeader(domain.ProbeBinaryStageIntent{Identity: intent.Identity, OwnerID: intent.OwnerID, AttemptID: intent.AttemptID})
	if err != nil {
		t.Fatal(err)
	}
	return header
}

// hactRawAction builds an action frame without the production encoder so
// malformed frames can be produced.
func hactRawAction(magic string, lenByte byte, kind string, header []byte) []byte {
	frame := append([]byte(magic), lenByte)
	frame = append(frame, kind...)
	return append(frame, header...)
}

// hactActionFrame frames the intent through the production encoder and checks
// it against the documented layout.
func hactActionFrame(t *testing.T, intent domain.ProbeHostActionIntent) []byte {
	t.Helper()
	frame, err := provisioning.EncodeProbeHostActionFrame(intent)
	if err != nil {
		t.Fatal(err)
	}
	if want := hactRawAction("PHXACT1\n", byte(len(intent.Kind)), intent.Kind, hactStageHeader(t, intent)); !bytes.Equal(frame, want) {
		t.Fatal("production action frame differs from the documented layout")
	}
	return frame
}

// hactGrantFrame frames a valid grant through the production encoder.
func hactGrantFrame(t *testing.T, k authzKeys, intent domain.ProbeHostActionIntent, nonce [32]byte) []byte {
	t.Helper()
	grant := domain.ProbeHostActionGrant{Request: hactRequest(k, intent, nonce), Signature: hactSign(t, k, intent, nonce)}
	if !grant.ValidFor(k.pub) {
		t.Fatal("fixture grant does not verify under its own key")
	}
	frame, err := provisioning.EncodeProbeHostActionGrantFrame(grant)
	if err != nil || len(frame) != provisioning.ProbeBinaryStageGrantFrameSize {
		t.Fatal("encode grant frame", err)
	}
	if !bytes.Equal(frame, authzRawFrame(grant.Signature)) {
		t.Fatal("production grant frame differs from the documented fixed layout")
	}
	return frame
}

func hactJoin(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// hactWantReceipt is built without the production encoder.
func hactWantReceipt(intent domain.ProbeHostActionIntent) []byte {
	return []byte(`{"state":"host_action_applied","kind":"` + intent.Kind + `","marker_sha256":"` + intent.MarkerSHA256() + `"}` + "\n")
}

// hactStdin is the helper's stdin: it counts consumed bytes and closes, and
// records that Close happened so apply can prove it ran afterwards.
type hactStdin struct {
	r        io.Reader
	reads    atomic.Int32
	consumed atomic.Int64
	closes   atomic.Int32
	closed   atomic.Bool
	closeErr error
}

func (s *hactStdin) Read(p []byte) (int, error) {
	s.reads.Add(1)
	n, err := s.r.Read(p)
	if n > 0 {
		s.consumed.Add(int64(n))
	}
	return n, err
}

func (s *hactStdin) Close() error {
	s.closes.Add(1)
	s.closed.Store(true)
	return s.closeErr
}

// hactOut is the helper's stdout. Every Write attempt is recorded; script may
// shape the result of one attempt.
type hactOut struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	attempts [][]byte
	script   func(call int, p []byte) (int, error)
}

func (o *hactOut) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.attempts = append(o.attempts, append([]byte(nil), p...))
	if o.script == nil {
		o.buf.Write(p)
		return len(p), nil
	}
	n, err := o.script(len(o.attempts), p)
	if n > 0 && n <= len(p) {
		o.buf.Write(p[:n])
	}
	return n, err
}

func (o *hactOut) Bytes() []byte {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]byte(nil), o.buf.Bytes()...)
}

func (o *hactOut) Len() int { return len(o.Bytes()) }

func (o *hactOut) attemptCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.attempts)
}

func (o *hactOut) attempt(i int) []byte {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.attempts[i]
}

// hactArgs is the full argument set so nil-refusal cases can break one field.
type hactArgs struct {
	ctx      context.Context
	input    io.ReadCloser
	out      io.Writer
	stderr   io.Writer
	loadKey  func(context.Context) ([32]byte, error)
	hostKeys func(context.Context) ([]string, error)
	entropy  io.Reader
	apply    func(context.Context, domain.ProbeHostActionIntent) error
}

func (a hactArgs) run(args []string) int {
	return runAuthorizedProbeHostActionHelper(a.ctx, args, a.input, a.out, a.stderr, a.loadKey, a.hostKeys, a.entropy, probeHostActionWithoutReceipt(a.apply))
}

type hactApplyCall struct {
	intent      domain.ProbeHostActionIntent
	inputClosed bool
	closes      int32
	stdout      []byte
	ctxErr      error
	hasDeadline bool
	deadlineIn  time.Duration
}

// hactHarness counts every injected dependency call. Defaults are a trusted
// key, a host-key list that contains the intent's key (not first), 64 bytes of
// fixed entropy, an apply that succeeds, and a stdin holding a valid action
// frame and grant for the harness nonce.
type hactHarness struct {
	t      *testing.T
	keys   authzKeys
	nonce  [32]byte
	intent domain.ProbeHostActionIntent
	action []byte
	grant  []byte
	stdin  *hactStdin
	out    *hactOut
	stderr bytes.Buffer

	keyCalls     atomic.Int32
	hostKeyCalls atomic.Int32
	entropyBytes atomic.Int64

	keyFn      func(context.Context) ([32]byte, error)
	hostKeysFn func(context.Context) ([]string, error)
	entropyR   io.Reader
	entropy    *bytes.Reader
	applyFn    func(context.Context, domain.ProbeHostActionIntent) error

	mu      sync.Mutex
	applies []hactApplyCall
}

func newHactHarness(t *testing.T, kind string) *hactHarness {
	t.Helper()
	h := &hactHarness{t: t, keys: authzNewKeys(t, "trusted"), intent: hactIntent(t, kind), out: &hactOut{}}
	a, b := authzNonce("entropy-a"), authzNonce("entropy-b")
	h.nonce = a
	h.entropy = bytes.NewReader(append(append([]byte(nil), a[:]...), b[:]...))
	h.entropyR = h.entropy
	h.keyFn = func(context.Context) ([32]byte, error) { return h.keys.pub, nil }
	h.hostKeysFn = func(context.Context) ([]string, error) {
		return []string{hactOtherFingerprint, h.intent.Identity.HostKeyFingerprint}, nil
	}
	h.applyFn = func(context.Context, domain.ProbeHostActionIntent) error { return nil }
	h.action = hactActionFrame(t, h.intent)
	h.grant = hactGrantFrame(t, h.keys, h.intent, h.nonce)
	h.setStream(hactJoin(h.action, h.grant))
	return h
}

func (h *hactHarness) setStream(b []byte)    { h.setReader(bytes.NewReader(b)) }
func (h *hactHarness) setReader(r io.Reader) { h.stdin = &hactStdin{r: r} }

func (h *hactHarness) wholeStreamLen() int { return len(h.action) + len(h.grant) }

func (h *hactHarness) args() hactArgs {
	return hactArgs{
		ctx:     context.Background(),
		input:   h.stdin,
		out:     h.out,
		stderr:  &h.stderr,
		loadKey: func(c context.Context) ([32]byte, error) { h.keyCalls.Add(1); return h.keyFn(c) },
		hostKeys: func(c context.Context) ([]string, error) {
			h.hostKeyCalls.Add(1)
			return h.hostKeysFn(c)
		},
		entropy: authzCountingReader{r: h.entropyR, n: &h.entropyBytes},
		apply: func(ctx context.Context, intent domain.ProbeHostActionIntent) error {
			call := hactApplyCall{
				intent:      intent,
				inputClosed: h.stdin.closed.Load(),
				closes:      h.stdin.closes.Load(),
				stdout:      h.out.Bytes(),
				ctxErr:      ctx.Err(),
			}
			if dl, ok := ctx.Deadline(); ok {
				call.hasDeadline, call.deadlineIn = true, time.Until(dl)
			}
			h.mu.Lock()
			h.applies = append(h.applies, call)
			h.mu.Unlock()
			return h.applyFn(ctx, intent)
		},
	}
}

func (h *hactHarness) run(ctx context.Context, args []string) int {
	a := h.args()
	a.ctx = ctx
	return a.run(args)
}

func (h *hactHarness) applyCalls() []hactApplyCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]hactApplyCall(nil), h.applies...)
}

func (h *hactHarness) challengeLine() []byte {
	return provisioning.EncodeProbeBinaryStageChallengeLine(h.keys.id, h.nonce)
}

func (h *hactHarness) wantCalls(key, hostKeys int32) {
	h.t.Helper()
	if h.keyCalls.Load() != key || h.hostKeyCalls.Load() != hostKeys {
		h.t.Fatalf("keyLoads=%d hostKeyLoads=%d, want %d/%d", h.keyCalls.Load(), h.hostKeyCalls.Load(), key, hostKeys)
	}
}

func (h *hactHarness) wantNoLeak() {
	h.t.Helper()
	got := h.stderr.String()
	for _, leak := range []string{"canary", "/etc/", "/var/lib", "SHA256:"} {
		if strings.Contains(got, leak) {
			h.t.Fatalf("stderr leaked %q: %q", leak, got)
		}
	}
	if strings.Count(got, "\n") > 1 {
		h.t.Fatalf("stderr is more than one line: %q", got)
	}
}

// wantRefusedPreChallenge asserts the refusal contract for failures before the
// challenge: exit 1, one exact sanitized stderr line, no stdout, owned input
// closed once and never read, and apply never run.
func (h *hactHarness) wantRefusedPreChallenge(code int, want error) {
	h.t.Helper()
	if code != 1 {
		h.t.Fatalf("exit code %d, want 1 (stderr %q)", code, h.stderr.String())
	}
	if h.out.Len() != 0 {
		h.t.Fatalf("stdout %q emitted before refusal", h.out.Bytes())
	}
	if got := h.stderr.String(); got != want.Error()+"\n" {
		h.t.Fatalf("stderr %q, want %q", got, want.Error()+"\n")
	}
	h.wantNoLeak()
	if h.stdin.closes.Load() != 1 || h.stdin.reads.Load() != 0 {
		h.t.Fatalf("input closes=%d reads=%d, want 1/0", h.stdin.closes.Load(), h.stdin.reads.Load())
	}
	if n := len(h.applyCalls()); n != 0 {
		h.t.Fatalf("apply ran %d times for a pre-challenge refusal", n)
	}
}

// wantFailedAfterChallenge asserts a failure after the challenge was emitted:
// exit 1, the exact stderr line, stdout holding only the challenge (never a
// receipt), the owned input closed exactly once, an exact stdin byte count
// (-1 skips it) and the exact number of apply calls.
func (h *hactHarness) wantFailedAfterChallenge(code int, want error, consumed, applies int) {
	h.t.Helper()
	if code != 1 || h.stderr.String() != want.Error()+"\n" {
		h.t.Fatalf("code=%d stderr=%q, want exit 1 with %q", code, h.stderr.String(), want.Error())
	}
	h.wantNoLeak()
	if got := h.out.Bytes(); !bytes.Equal(got, h.challengeLine()) {
		h.t.Fatalf("stdout %q, want only the challenge line", got)
	}
	if n := len(h.applyCalls()); n != applies {
		h.t.Fatalf("apply ran %d times, want %d", n, applies)
	}
	if consumed >= 0 && h.stdin.consumed.Load() != int64(consumed) {
		h.t.Fatalf("helper consumed %d stdin bytes, want exactly %d", h.stdin.consumed.Load(), consumed)
	}
	if h.stdin.closes.Load() != 1 {
		h.t.Fatalf("stdin closed %d times, want 1", h.stdin.closes.Load())
	}
}

// wantApplied asserts a full success: exit 0, silent stderr, apply once with
// the exact decoded intent after input was closed and before any receipt, and
// stdout equal to the challenge line followed by the canonical receipt.
func (h *hactHarness) wantApplied(code int) {
	h.t.Helper()
	if code != 0 || h.stderr.Len() != 0 {
		h.t.Fatalf("code=%d stderr=%q", code, h.stderr.String())
	}
	calls := h.applyCalls()
	if len(calls) != 1 {
		h.t.Fatalf("apply ran %d times, want exactly once", len(calls))
	}
	c := calls[0]
	if c.intent != h.intent {
		h.t.Fatalf("apply got intent %+v, want %+v", c.intent, h.intent)
	}
	if !c.inputClosed || c.closes != 1 {
		h.t.Fatalf("apply ran before stdin was closed (closed=%v closes=%d)", c.inputClosed, c.closes)
	}
	if !bytes.Equal(c.stdout, h.challengeLine()) {
		h.t.Fatalf("stdout before apply %q, want only the challenge line", c.stdout)
	}
	if c.ctxErr != nil || !c.hasDeadline || c.deadlineIn <= 0 || c.deadlineIn > probeBinaryStageHelperTimeout {
		h.t.Fatalf("apply ctx must be live with a bounded deadline: err=%v deadline=%v in %v", c.ctxErr, c.hasDeadline, c.deadlineIn)
	}
	want := append(h.challengeLine(), hactWantReceipt(h.intent)...)
	if got := h.out.Bytes(); !bytes.Equal(got, want) {
		h.t.Fatalf("stdout %q, want challenge line then the receipt line", got)
	}
	if n := h.out.attemptCount(); n != 2 || !bytes.Equal(h.out.attempt(0), h.challengeLine()) || !bytes.Equal(h.out.attempt(1), hactWantReceipt(h.intent)) {
		h.t.Fatalf("stdout must be exactly two writes: challenge, receipt (got %d)", n)
	}
	if h.stdin.consumed.Load() != int64(h.wholeStreamLen()) || h.stdin.closes.Load() != 1 {
		h.t.Fatalf("stdin consumed=%d want=%d closes=%d", h.stdin.consumed.Load(), h.wholeStreamLen(), h.stdin.closes.Load())
	}
	if h.entropy.Len() != 32 {
		h.t.Fatalf("entropy remaining %d, want exactly the 32 surplus bytes", h.entropy.Len())
	}
}

func TestRunAuthorizedProbeHostActionHelperUsageNeverEchoesArgsOrLoads(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"--kind=unit_file"}, {"--path=/etc/secret"}, {"sensitive-path-canary"}, {""}, {"a", "b"}} {
		h := newHactHarness(t, domain.ProbeHostActionDataDirectory)
		code := h.run(context.Background(), args)
		if code != 2 || h.out.Len() != 0 || h.stdin.closes.Load() != 1 || h.stdin.reads.Load() != 0 {
			t.Fatalf("args %q code=%d out=%q closes=%d reads=%d", args, code, h.out.Bytes(), h.stdin.closes.Load(), h.stdin.reads.Load())
		}
		h.wantCalls(0, 0)
		if h.entropyBytes.Load() != 0 || len(h.applyCalls()) != 0 {
			t.Fatalf("args %q touched entropy/apply", args)
		}
		got := h.stderr.String()
		if got != probeHostActionAuthorizedUsage {
			t.Fatalf("args %q stderr %q, want the fixed usage", args, got)
		}
		for _, leak := range []string{"canary", "--kind", "--path", "/etc/secret", "--help"} {
			if strings.Contains(got, leak) {
				t.Fatalf("args %q leaked %q into usage", args, leak)
			}
		}
	}
	if !strings.Contains(probeHostActionAuthorizedUsage, "stage-host-action-authorized") ||
		!strings.Contains(probeHostActionAuthorizedUsage, ProbeBinaryStageAuthorityKeyPath) ||
		!strings.Contains(probeHostActionAuthorizedUsage, "NOT installation") || !strings.HasSuffix(probeHostActionAuthorizedUsage, "\n") {
		t.Fatalf("usage text drifted: %q", probeHostActionAuthorizedUsage)
	}
	// Cleanup is an action this helper applies, so the usage must not
	// disclaim it, and must name every action the helper accepts.
	for _, kind := range []string{"data_directory", "unit_file", "service", "probe_init", "service_start", "cleanup"} {
		if !strings.Contains(probeHostActionAuthorizedUsage, kind) {
			t.Fatalf("usage text omits the %s action: %q", kind, probeHostActionAuthorizedUsage)
		}
	}
	if strings.Contains(probeHostActionAuthorizedUsage, "NOT installation, enrollment, service start, cleanup") {
		t.Fatalf("usage text still disclaims cleanup: %q", probeHostActionAuthorizedUsage)
	}

	// An input close failure on the usage path is a storage refusal, not exit 2.
	h := newHactHarness(t, domain.ProbeHostActionUnitFile)
	h.stdin.closeErr = errors.New("sensitive-close-canary")
	code := h.run(context.Background(), []string{"--path=/etc/secret"})
	if code != 1 || h.out.Len() != 0 || h.stderr.String() != domain.ErrProbeArtifactStorage.Error()+"\n" ||
		strings.Contains(h.stderr.String(), "canary") || strings.Contains(h.stderr.String(), "Usage") {
		t.Fatalf("usage close failure code=%d err=%q", code, h.stderr.String())
	}
	h.wantCalls(0, 0)

	// A typed-nil input and a nil stderr still dispatch usage without a panic.
	h = newHactHarness(t, domain.ProbeHostActionService)
	a := h.args()
	var typed *hactStdin
	a.input = typed
	if code = a.run([]string{"--help"}); code != 2 || h.stderr.String() != probeHostActionAuthorizedUsage {
		t.Fatalf("typed-nil input usage code=%d err=%q", code, h.stderr.String())
	}
	a = h.args()
	a.stderr = (*bytes.Buffer)(nil)
	if code = a.run([]string{"--help"}); code != 2 {
		t.Fatalf("typed-nil stderr usage code=%d", code)
	}
	h.wantCalls(0, 0)

	// An empty, non-nil argument list is not a usage request.
	h = newHactHarness(t, domain.ProbeHostActionDataDirectory)
	h.keyFn = func(context.Context) ([32]byte, error) { return [32]byte{}, domain.ErrProbeBinaryStageWriteUnavailable }
	h.wantRefusedPreChallenge(h.run(context.Background(), []string{}), domain.ErrProbeBinaryStageWriteUnavailable)
	h.wantCalls(1, 0)
}

func TestRunAuthorizedProbeHostActionHelperRejectsNilBeforeAnyDispatch(t *testing.T) {
	var typedCtx context.Context = (*hostMockCtx)(nil)
	var untypedCtx context.Context
	var typedInput io.ReadCloser = (*hactStdin)(nil)
	var typedEntropy io.Reader = (*bytes.Reader)(nil)
	cases := []struct {
		name        string
		mutate      func(*hactArgs)
		inputLive   bool // the owned input is still non-nil and must be closed
		stderrIsSet bool // stderr is still writable and must carry the refusal
	}{
		{"typed-nil ctx", func(a *hactArgs) { a.ctx = typedCtx }, true, true},
		{"nil ctx", func(a *hactArgs) { a.ctx = untypedCtx }, true, true},
		{"typed-nil input", func(a *hactArgs) { a.input = typedInput }, false, true},
		{"nil input", func(a *hactArgs) { a.input = nil }, false, true},
		{"typed-nil out", func(a *hactArgs) { a.out = (*bytes.Buffer)(nil) }, true, true},
		{"nil out", func(a *hactArgs) { a.out = nil }, true, true},
		{"typed-nil stderr", func(a *hactArgs) { a.stderr = (*bytes.Buffer)(nil) }, true, false},
		{"nil stderr", func(a *hactArgs) { a.stderr = nil }, true, false},
		{"nil loadKey", func(a *hactArgs) { a.loadKey = nil }, true, true},
		{"typed-nil loadKey", func(a *hactArgs) { a.loadKey = (func(context.Context) ([32]byte, error))(nil) }, true, true},
		{"nil hostKeys", func(a *hactArgs) { a.hostKeys = nil }, true, true},
		{"typed-nil hostKeys", func(a *hactArgs) { a.hostKeys = (func(context.Context) ([]string, error))(nil) }, true, true},
		{"nil entropy", func(a *hactArgs) { a.entropy = nil }, true, true},
		{"typed-nil entropy", func(a *hactArgs) { a.entropy = typedEntropy }, true, true},
		{"nil apply", func(a *hactArgs) { a.apply = nil }, true, true},
		{"typed-nil apply", func(a *hactArgs) { a.apply = (func(context.Context, domain.ProbeHostActionIntent) error)(nil) }, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHactHarness(t, domain.ProbeHostActionDataDirectory)
			a := h.args()
			tc.mutate(&a)
			code := a.run(nil)
			if code != 1 || h.out.Len() != 0 {
				t.Fatalf("code=%d out=%q", code, h.out.Bytes())
			}
			if tc.stderrIsSet && h.stderr.String() != domain.ErrProbeBinaryStageWriteInput.Error()+"\n" {
				t.Fatalf("stderr %q, want the input refusal", h.stderr.String())
			}
			if !tc.stderrIsSet && h.stderr.Len() != 0 {
				t.Fatalf("stderr %q written to the real buffer despite a nil stderr", h.stderr.String())
			}
			wantCloses := int32(0)
			if tc.inputLive {
				wantCloses = 1
			}
			if h.stdin.closes.Load() != wantCloses || h.stdin.reads.Load() != 0 {
				t.Fatalf("input closes=%d reads=%d, want %d/0", h.stdin.closes.Load(), h.stdin.reads.Load(), wantCloses)
			}
			h.wantCalls(0, 0)
			if h.entropyBytes.Load() != 0 || len(h.applyCalls()) != 0 {
				t.Fatal("a dependency was dispatched despite a nil argument")
			}
		})
	}

	// An input close failure overrides the input refusal with ArtifactStorage.
	h := newHactHarness(t, domain.ProbeHostActionDataDirectory)
	h.stdin.closeErr = errors.New("sensitive-close-canary")
	a := h.args()
	a.ctx = typedCtx
	if code := a.run(nil); code != 1 || h.out.Len() != 0 || h.stderr.String() != domain.ErrProbeArtifactStorage.Error()+"\n" ||
		strings.Contains(h.stderr.String(), "canary") {
		t.Fatalf("close failure code=%d err=%q", code, h.stderr.String())
	}
	h.wantCalls(0, 0)
}

func TestRunAuthorizedProbeHostActionHelperCancelBeforeStart(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	h := newHactHarness(t, domain.ProbeHostActionDataDirectory)
	h.wantRefusedPreChallenge(h.run(canceled, nil), context.Canceled)
	h.wantCalls(0, 0)
	if h.entropyBytes.Load() != 0 {
		t.Fatal("canceled start loaded dependencies")
	}

	expired, stop := context.WithTimeout(context.Background(), 0)
	defer stop()
	<-expired.Done()
	h = newHactHarness(t, domain.ProbeHostActionUnitFile)
	h.wantRefusedPreChallenge(h.run(expired, nil), context.DeadlineExceeded)
	h.wantCalls(0, 0)

	// Cancellation arriving inside key loading (reported as the error) is
	// preserved and nothing follows it.
	live, abort := context.WithCancel(context.Background())
	h = newHactHarness(t, domain.ProbeHostActionService)
	h.keyFn = func(c context.Context) ([32]byte, error) { abort(); return [32]byte{}, c.Err() }
	h.wantRefusedPreChallenge(h.run(live, nil), context.Canceled)
	h.wantCalls(1, 0)
	if h.entropyBytes.Load() != 0 {
		t.Fatal("entropy read after a canceled key load")
	}

	// The same for host-key loading.
	live, abort = context.WithCancel(context.Background())
	h = newHactHarness(t, domain.ProbeHostActionService)
	h.hostKeysFn = func(c context.Context) ([]string, error) { abort(); return nil, c.Err() }
	h.wantRefusedPreChallenge(h.run(live, nil), context.Canceled)
	h.wantCalls(1, 1)
	if h.entropyBytes.Load() != 0 {
		t.Fatal("entropy read after a canceled host-key load")
	}
}

// hactCancelAfterPrerequisite builds a harness whose context dies inside one
// prerequisite that otherwise succeeds (nil error, valid result).
func hactCancelAfterPrerequisite(t *testing.T, where string) (*hactHarness, context.Context) {
	t.Helper()
	ctx, abort := context.WithCancel(context.Background())
	h := newHactHarness(t, domain.ProbeHostActionDataDirectory)
	switch where {
	case "key":
		h.keyFn = func(context.Context) ([32]byte, error) { abort(); return h.keys.pub, nil }
	case "hostkeys":
		h.hostKeysFn = func(context.Context) ([]string, error) {
			abort()
			return []string{h.intent.Identity.HostKeyFingerprint}, nil
		}
	case "entropy":
		nonce := h.nonce
		h.entropyR = authzFuncReader(func(p []byte) (int, error) { abort(); return copy(p, nonce[:]), nil })
	default:
		t.Fatalf("unknown prerequisite %q", where)
	}
	return h, ctx
}

// This is the intended contract, mirrored from the binary-stage sibling
// (TestRunAuthorizedProbeBinaryStageHelperCancelAfterPrerequisitesEmitsNoChallenge):
// a context that dies while a prerequisite succeeds must not leak a challenge
// for a helper that can no longer act, because the sender consumes a one-shot
// DB admission as soon as it sees one.
func TestRunAuthorizedProbeHostActionHelperCancelAfterPrerequisitesEmitsNoChallenge(t *testing.T) {
	for _, where := range []string{"key", "hostkeys", "entropy"} {
		t.Run(where, func(t *testing.T) {
			h, ctx := hactCancelAfterPrerequisite(t, where)
			h.wantRefusedPreChallenge(h.run(ctx, nil), context.Canceled)
		})
	}
}

func TestRunAuthorizedProbeHostActionHelperKeyLoadRefusalSanitizesAndEmitsNothing(t *testing.T) {
	cases := []struct {
		name string
		key  [32]byte
		err  error
		want error
	}{
		{"unavailable", [32]byte{}, domain.ErrProbeBinaryStageWriteUnavailable, domain.ErrProbeBinaryStageWriteUnavailable},
		{"storage", [32]byte{}, domain.ErrProbeArtifactStorage, domain.ErrProbeArtifactStorage},
		{"wrapped storage", [32]byte{}, fmt.Errorf("read key: %w", domain.ErrProbeArtifactStorage), domain.ErrProbeArtifactStorage},
		{"canceled sentinel", [32]byte{}, context.Canceled, context.Canceled},
		{"deadline sentinel", [32]byte{}, context.DeadlineExceeded, context.DeadlineExceeded},
		{"input sentinel is not echoed", [32]byte{}, domain.ErrProbeBinaryStageWriteInput, domain.ErrProbeBinaryStageWriteUnavailable},
		{"unauthorized sentinel is not echoed", [32]byte{}, domain.ErrProbeBinaryStageWriteUnauthorized, domain.ErrProbeBinaryStageWriteUnavailable},
		{"arbitrary canary", [32]byte{}, errors.New("sensitive-key-canary /etc/phoenix-probe/stage-authority.pub"), domain.ErrProbeBinaryStageWriteUnavailable},
		{"nil error zero key", [32]byte{}, nil, domain.ErrProbeBinaryStageWriteUnavailable},
		{"error beside a nonzero key", authzNewKeys(t, "ignored").pub, errors.New("sensitive-key-canary"), domain.ErrProbeBinaryStageWriteUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHactHarness(t, domain.ProbeHostActionDataDirectory)
			h.keyFn = func(context.Context) ([32]byte, error) { return tc.key, tc.err }
			h.wantRefusedPreChallenge(h.run(context.Background(), nil), tc.want)
			h.wantCalls(1, 0)
			if h.entropyBytes.Load() != 0 {
				t.Fatalf("entropy=%d after a failed key load", h.entropyBytes.Load())
			}
		})
	}
	// An input close failure outranks the sanitized key refusal.
	h := newHactHarness(t, domain.ProbeHostActionDataDirectory)
	h.stdin.closeErr = errors.New("sensitive-close-canary")
	h.keyFn = func(context.Context) ([32]byte, error) { return [32]byte{}, domain.ErrProbeBinaryStageWriteUnavailable }
	code := h.run(context.Background(), nil)
	if code != 1 || h.out.Len() != 0 || h.stderr.String() != domain.ErrProbeArtifactStorage.Error()+"\n" || h.hostKeyCalls.Load() != 0 {
		t.Fatalf("close over key refusal code=%d err=%q", code, h.stderr.String())
	}
}

func TestRunAuthorizedProbeHostActionHelperHostKeysRefusalSanitizesAndEmitsNothing(t *testing.T) {
	fp := hactIntent(t, domain.ProbeHostActionDataDirectory).Identity.HostKeyFingerprint
	cases := []struct {
		name string
		keys []string
		err  error
		want error
	}{
		{"unavailable", nil, domain.ErrProbeBinaryStageWriteUnavailable, domain.ErrProbeBinaryStageWriteUnavailable},
		{"storage", nil, domain.ErrProbeArtifactStorage, domain.ErrProbeArtifactStorage},
		{"wrapped storage", nil, fmt.Errorf("parse key: %w", domain.ErrProbeArtifactStorage), domain.ErrProbeArtifactStorage},
		{"canceled sentinel", nil, context.Canceled, context.Canceled},
		{"deadline sentinel", nil, context.DeadlineExceeded, context.DeadlineExceeded},
		{"input sentinel is not echoed", nil, domain.ErrProbeBinaryStageWriteInput, domain.ErrProbeBinaryStageWriteUnavailable},
		{"unauthorized sentinel is not echoed", nil, domain.ErrProbeBinaryStageWriteUnauthorized, domain.ErrProbeBinaryStageWriteUnavailable},
		{"arbitrary canary", nil, errors.New("sensitive-hostkey-canary /etc/ssh/ssh_host_rsa_key.pub"), domain.ErrProbeBinaryStageWriteUnavailable},
		{"nil error empty list", []string{}, nil, domain.ErrProbeBinaryStageWriteUnavailable},
		{"nil error nil list", nil, nil, domain.ErrProbeBinaryStageWriteUnavailable},
		{"error beside a populated list", []string{fp}, errors.New("sensitive-hostkey-canary"), domain.ErrProbeBinaryStageWriteUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHactHarness(t, domain.ProbeHostActionDataDirectory)
			h.hostKeysFn = func(context.Context) ([]string, error) { return tc.keys, tc.err }
			h.wantRefusedPreChallenge(h.run(context.Background(), nil), tc.want)
			h.wantCalls(1, 1)
			if h.entropyBytes.Load() != 0 {
				t.Fatalf("entropy=%d after failed host keys", h.entropyBytes.Load())
			}
		})
	}
	// An input close failure outranks the sanitized host-key refusal.
	h := newHactHarness(t, domain.ProbeHostActionDataDirectory)
	h.stdin.closeErr = errors.New("sensitive-close-canary")
	h.hostKeysFn = func(context.Context) ([]string, error) { return nil, domain.ErrProbeBinaryStageWriteUnavailable }
	code := h.run(context.Background(), nil)
	if code != 1 || h.out.Len() != 0 || h.stderr.String() != domain.ErrProbeArtifactStorage.Error()+"\n" || h.entropyBytes.Load() != 0 {
		t.Fatalf("close over host-key refusal code=%d err=%q", code, h.stderr.String())
	}
}

func TestRunAuthorizedProbeHostActionHelperEntropyRefusalEmitsNothing(t *testing.T) {
	zero := make([]byte, 32)
	cases := []struct {
		name string
		r    io.Reader
	}{
		{"empty", bytes.NewReader(nil)},
		{"short read", bytes.NewReader(bytes.Repeat([]byte{7}, 16))},
		{"one byte short", bytes.NewReader(bytes.Repeat([]byte{7}, 31))},
		{"error immediately", iotest.ErrReader(errors.New("sensitive-entropy-canary"))},
		{"error after partial", io.MultiReader(bytes.NewReader(bytes.Repeat([]byte{7}, 20)), iotest.ErrReader(errors.New("sensitive-entropy-canary")))},
		{"all zero nonce", bytes.NewReader(zero)},
		{"all zero nonce with surplus", bytes.NewReader(append(append([]byte{}, zero...), bytes.Repeat([]byte{9}, 32)...))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHactHarness(t, domain.ProbeHostActionUnitFile)
			h.entropyR = tc.r
			h.wantRefusedPreChallenge(h.run(context.Background(), nil), domain.ErrProbeBinaryStageWriteUnavailable)
			// Key and host keys load first, but no challenge follows a helper that
			// cannot make a nonce.
			h.wantCalls(1, 1)
		})
	}
}

func TestRunAuthorizedProbeHostActionHelperChallengeWriteFailureRefuses(t *testing.T) {
	cases := []struct {
		name string
		fn   func(p []byte) (int, error)
	}{
		{"error with canary", func([]byte) (int, error) { return 0, errors.New("sensitive-write-canary") }},
		{"partial write with error", func([]byte) (int, error) { return 5, errors.New("sensitive-write-canary") }},
		{"short write without error", func(p []byte) (int, error) { return len(p) - 1, nil }},
		{"zero write without error", func([]byte) (int, error) { return 0, nil }},
		{"closed pipe", func([]byte) (int, error) { return 0, io.ErrClosedPipe }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHactHarness(t, domain.ProbeHostActionDataDirectory)
			h.out.script = func(_ int, p []byte) (int, error) { return tc.fn(p) }
			code := h.run(context.Background(), nil)
			if code != 1 || h.stderr.String() != domain.ErrProbeBinaryStageWriteUnavailable.Error()+"\n" || strings.Contains(h.stderr.String(), "canary") {
				t.Fatalf("code=%d stderr=%q", code, h.stderr.String())
			}
			if h.stdin.closes.Load() != 1 || h.stdin.reads.Load() != 0 {
				t.Fatalf("input closes=%d reads=%d, want 1/0", h.stdin.closes.Load(), h.stdin.reads.Load())
			}
			h.wantCalls(1, 1)
			if n := h.out.attemptCount(); n != 1 || !bytes.Equal(h.out.attempt(0), h.challengeLine()) {
				t.Fatalf("writes=%d, want exactly one canonical challenge", n)
			}
			if len(h.applyCalls()) != 0 {
				t.Fatal("apply ran after a failed challenge write")
			}
		})
	}
	// An input that cannot be closed is the louder storage fact.
	h := newHactHarness(t, domain.ProbeHostActionDataDirectory)
	h.out.script = func(int, []byte) (int, error) { return 0, errors.New("sensitive-write-canary") }
	h.stdin.closeErr = errors.New("sensitive-close-canary")
	if code := h.run(context.Background(), nil); code != 1 || h.stderr.String() != domain.ErrProbeArtifactStorage.Error()+"\n" ||
		strings.Contains(h.stderr.String(), "canary") || h.stdin.reads.Load() != 0 || len(h.applyCalls()) != 0 {
		t.Fatalf("input close failure code=%d err=%q", code, h.stderr.String())
	}
}

func TestRunAuthorizedProbeHostActionHelperChallengeIsBoundAndPrecedesAnyStdinRead(t *testing.T) {
	h := newHactHarness(t, domain.ProbeHostActionUnitFile)
	type snapshot struct {
		line         []byte
		stdinReads   int32
		stdinCloses  int32
		keyLoads     int32
		hostKeyLoads int32
		entropyBytes int64
		applies      int
	}
	var snaps []snapshot
	h.out.script = func(_ int, p []byte) (int, error) {
		snaps = append(snaps, snapshot{
			line:         append([]byte(nil), p...),
			stdinReads:   h.stdin.reads.Load(),
			stdinCloses:  h.stdin.closes.Load(),
			keyLoads:     h.keyCalls.Load(),
			hostKeyLoads: h.hostKeyCalls.Load(),
			entropyBytes: h.entropyBytes.Load(),
			applies:      len(h.applyCalls()),
		})
		return 0, errors.New("stop after the challenge")
	}
	if code := h.run(context.Background(), nil); code != 1 {
		t.Fatalf("code=%d stderr=%q", code, h.stderr.String())
	}
	if len(snaps) != 1 {
		t.Fatalf("challenge written %d times", len(snaps))
	}
	s := snaps[0]
	if s.stdinReads != 0 || s.stdinCloses != 0 || s.keyLoads != 1 || s.hostKeyLoads != 1 || s.entropyBytes != 32 || s.applies != 0 {
		t.Fatalf("challenge was not emitted after exactly key+hostkeys+entropy and before stdin/apply: %+v", s)
	}
	// Exactly 32 entropy bytes become the nonce; the surplus is untouched.
	if h.entropy.Len() != 32 {
		t.Fatalf("entropy remaining %d, want 32", h.entropy.Len())
	}
	if len(s.line) != provisioning.ProbeBinaryStageChallengeLineSize || bytes.Count(s.line, []byte{'\n'}) != 1 || s.line[len(s.line)-1] != '\n' {
		t.Fatalf("challenge is not one canonical LF-terminated line: %q", s.line)
	}
	keyID, nonce, err := provisioning.ParseProbeBinaryStageChallengeLine(s.line)
	if err != nil || keyID != h.keys.id || nonce != h.nonce {
		t.Fatalf("challenge not bound to the trusted key and entropy nonce: %v", err)
	}
	if !bytes.Equal(s.line, h.challengeLine()) {
		t.Fatal("challenge differs from the canonical encoding")
	}
	// The challenge names the key's SHA-256 identity, never the key itself, and
	// carries no marker because the intent has not been read yet.
	if strings.Contains(string(s.line), hex.EncodeToString(h.keys.pub[:])) || strings.Contains(string(s.line), "marker") {
		t.Fatalf("challenge leaked the public key or a marker: %q", s.line)
	}
}

func TestRunAuthorizedProbeHostActionHelperAppliesEachKindExactlyOnceAfterInputClosed(t *testing.T) {
	wrappers := map[string]func(io.Reader) io.Reader{
		"whole reads": func(r io.Reader) io.Reader { return r },
		"one byte":    iotest.OneByteReader,
		"half reads":  iotest.HalfReader,
		// The grant's final byte (or the EOF) may arrive together with data.
		"data with eof": iotest.DataErrReader,
	}
	markers := map[string]string{}
	for _, kind := range hactKinds {
		markers[kind] = hactIntent(t, kind).MarkerSHA256()
	}
	if len(map[string]bool{markers[hactKinds[0]]: true, markers[hactKinds[1]]: true, markers[hactKinds[2]]: true}) != 3 {
		t.Fatal("the three kinds must have three distinct markers, otherwise their grants are interchangeable")
	}
	for _, kind := range hactKinds {
		for name, wrap := range wrappers {
			t.Run(kind+"/"+name, func(t *testing.T) {
				h := newHactHarness(t, kind)
				h.setReader(wrap(bytes.NewReader(hactJoin(h.action, h.grant))))
				h.wantApplied(h.run(context.Background(), nil))
				// The receipt is a fixed JSON shape built independently of the encoder.
				var view map[string]string
				if err := json.Unmarshal(hactWantReceipt(h.intent), &view); err != nil || len(view) != 3 ||
					view["state"] != "host_action_applied" || view["kind"] != kind {
					t.Fatalf("receipt shape drifted: %v %v", view, err)
				}
				sum := sha256.Sum256([]byte(h.intent.Marker()))
				if view["marker_sha256"] != hex.EncodeToString(sum[:]) {
					t.Fatal("receipt marker is not the SHA-256 of the intent marker")
				}
				if !provisioning.ProbeHostActionReceiptMatches(h.out.attempt(1), h.intent) {
					t.Fatal("receipt is not accepted by the sender-side matcher")
				}
				if bytes.Count(h.out.Bytes(), []byte{'\n'}) != 2 {
					t.Fatalf("stdout has %d lines, want 2", bytes.Count(h.out.Bytes(), []byte{'\n'}))
				}
			})
		}
	}
}

// hactRefusal is one stdin stream the helper must refuse after the challenge.
type hactRefusal struct {
	name     string
	stream   []byte
	want     error
	consumed int      // exact stdin bytes the helper may have read; -1 skips
	hostKeys []string // nil keeps the harness default
}

func hactRefusalCases(t *testing.T, h *hactHarness) []hactRefusal {
	t.Helper()
	A, G, N, K, I := h.action, h.grant, h.nonce, h.keys, h.intent
	kindLen := len(I.Kind)
	unauthorized, input := domain.ErrProbeBinaryStageWriteUnauthorized, domain.ErrProbeBinaryStageWriteInput
	other := authzNewKeys(t, "other")
	fp := I.Identity.HostKeyFingerprint

	flip := func(i int) []byte {
		g := append([]byte(nil), G...)
		g[i] ^= 0x01
		return g
	}
	magicG := append([]byte(nil), G...)
	magicG[6] = '2'
	otherKind := hactKinds[0]
	for i, k := range hactKinds {
		if k == I.Kind {
			otherKind = hactKinds[(i+1)%len(hactKinds)]
		}
	}
	forKind := hactVariant(t, I, func(i *domain.ProbeHostActionIntent) { i.Kind = otherKind })
	forAttempt := hactVariant(t, I, func(i *domain.ProbeHostActionIntent) { i.AttemptID = "44444444-4444-4444-8444-444444444444" })
	forOwner := hactVariant(t, I, func(i *domain.ProbeHostActionIntent) { i.OwnerID = "66666666-6666-4666-8666-666666666666" })
	forOperation := hactVariant(t, I, func(i *domain.ProbeHostActionIntent) {
		i.Identity.OperationID = "55555555-5555-4555-8555-555555555555"
	})
	forBinary := hactVariant(t, I, func(i *domain.ProbeHostActionIntent) { i.Identity.Binary.SHA256 = strings.Repeat("b", 64) })

	// A binary-stage grant for the very same identity/owner/attempt, key and nonce.
	stage := authzRootIntent()
	stageReq := authzRequest(K, stage, N)
	stageSig := authzSignRaw(K, stageReq.SigningMessage())
	if !(domain.ProbeBinaryStageGrant{Request: stageReq, Signature: stageSig}).ValidFor(K.pub) {
		t.Fatal("fixture: the binary-stage grant must be valid in its own domain")
	}
	if (domain.ProbeHostActionGrant{Request: hactRequest(K, I, N), Signature: stageSig}).ValidFor(K.pub) {
		t.Fatal("fixture: a binary-stage signature must not verify as a host-action grant")
	}
	// The reverse separation: the host-action domain over a binary-stage marker.
	reverse := authzSignRaw(K, hactManualMessage(K, N, stage.MarkerSHA256()))

	nonRootStage := helperIntent() // Host.User "deploy"
	nonRootHeader := hactStageHeader(t, domain.ProbeHostActionIntent{Identity: nonRootStage.Identity, OwnerID: nonRootStage.OwnerID, AttemptID: nonRootStage.AttemptID})
	header := hactStageHeader(t, I)
	nonRootFrame := hactRawAction("PHXACT1\n", byte(kindLen), I.Kind, nonRootHeader)

	cases := []hactRefusal{
		// Host binding is checked before the grant frame is read.
		{"foreign host key", hactJoin(A, G), unauthorized, len(A), []string{hactOtherFingerprint}},
		{"foreign host key, several keys", hactJoin(A, G), unauthorized, len(A), []string{hactOtherFingerprint, "SHA256:another"}},
		{"near-miss host key: trailing space", hactJoin(A, G), unauthorized, len(A), []string{fp + " "}},
		{"near-miss host key: truncated", hactJoin(A, G), unauthorized, len(A), []string{fp[:len(fp)-1]}},
		{"near-miss host key: lowercased", hactJoin(A, G), unauthorized, len(A), []string{strings.ToLower(fp)}},
		{"only an empty fingerprint is trusted", hactJoin(A, G), unauthorized, len(A), []string{""}},

		// The grant itself.
		{"signature bit flip first byte", hactJoin(A, flip(8)), unauthorized, len(A) + len(G), nil},
		{"signature bit flip last byte", hactJoin(A, flip(len(G)-1)), unauthorized, len(A) + len(G), nil},
		{"zero signature", hactJoin(A, authzRawFrame([64]byte{})), unauthorized, len(A) + len(G), nil},
		{"wrong grant magic", hactJoin(A, magicG), unauthorized, len(A) + len(G), nil},
		{"signature by another key over its own key id", hactJoin(A, authzRawFrame(hactSign(t, other, I, N))), unauthorized, len(A) + len(G), nil},
		{"signature by another key forging the exact message", hactJoin(A, authzRawFrame(authzSignRaw(other, hactRequest(K, I, N).SigningMessage()))), unauthorized, len(A) + len(G), nil},
		{"binary-stage grant for the same key and nonce", hactJoin(A, authzRawFrame(stageSig)), unauthorized, len(A) + len(G), nil},
		{"binary-stage domain over the host-action marker", hactJoin(A, authzRawFrame(authzSignRaw(K, authzManualMessage(K, N, I.MarkerSHA256())))), unauthorized, len(A) + len(G), nil},
		{"host-action domain over a binary-stage marker", hactJoin(A, authzRawFrame(reverse)), unauthorized, len(A) + len(G), nil},
		{"message without any domain separator", hactJoin(A, authzRawFrame(authzSignRaw(K, hactRequest(K, I, N).SigningMessage()[len("phoenix-host-action-admission-v1\x00"):]))), unauthorized, len(A) + len(G), nil},
		{"grant for a different nonce (replayed from an earlier run)", hactJoin(A, authzRawFrame(hactSign(t, K, I, authzNonce("replayed-from-an-earlier-run")))), unauthorized, len(A) + len(G), nil},
		{"grant for a different kind", hactJoin(A, authzRawFrame(hactSign(t, K, forKind, N))), unauthorized, len(A) + len(G), nil},
		{"grant for a different attempt", hactJoin(A, authzRawFrame(hactSign(t, K, forAttempt, N))), unauthorized, len(A) + len(G), nil},
		{"grant for a different owner", hactJoin(A, authzRawFrame(hactSign(t, K, forOwner, N))), unauthorized, len(A) + len(G), nil},
		{"grant for a different operation", hactJoin(A, authzRawFrame(hactSign(t, K, forOperation, N))), unauthorized, len(A) + len(G), nil},
		{"grant for a different binary pin", hactJoin(A, authzRawFrame(hactSign(t, K, forBinary, N))), unauthorized, len(A) + len(G), nil},
		{"correctly signed zero nonce", hactJoin(A, authzRawFrame(authzSignRaw(K, hactManualMessage(K, [32]byte{}, I.MarkerSHA256())))), unauthorized, len(A) + len(G), nil},

		// The action frame.
		{"non-root intent", hactJoin(nonRootFrame, G), input, len(nonRootFrame), nil},
		{"unknown kind", hactJoin(hactRawAction("PHXACT1\n", 5, "bogus", header), G), input, 9 + 5 + len(header), nil},
		{"artifact_directory is not a host action", hactJoin(hactRawAction("PHXACT1\n", 18, "artifact_directory", header), G), input, 9 + 18 + len(header), nil},
		{"kind is case sensitive", hactJoin(hactRawAction("PHXACT1\n", byte(kindLen), strings.ToUpper(I.Kind), header), G), input, 9 + kindLen + len(header), nil},
		{"kind with trailing space", hactJoin(hactRawAction("PHXACT1\n", byte(kindLen+1), I.Kind+" ", header), G), input, 9 + kindLen + 1 + len(header), nil},
		{"kind with NUL suffix", hactJoin(hactRawAction("PHXACT1\n", byte(kindLen+1), I.Kind+"\x00", header), G), input, 9 + kindLen + 1 + len(header), nil},
		{"kind length zero", hactJoin(hactRawAction("PHXACT1\n", 0, "", header), G), input, 9, nil},
		{"kind length above the bound", hactJoin(hactRawAction("PHXACT1\n", 33, strings.Repeat("k", 33), header), G), input, 9, nil},
		{"kind length 255", hactJoin(hactRawAction("PHXACT1\n", 255, I.Kind, header), G), input, 9, nil},
		{"kind length longer than the kind", hactJoin(hactRawAction("PHXACT1\n", byte(kindLen+1), I.Kind, header), G), input, -1, nil},
		{"kind length shorter than the kind", hactJoin(hactRawAction("PHXACT1\n", byte(kindLen-1), I.Kind, header), G), input, -1, nil},
		{"wrong action magic version", hactJoin(hactRawAction("PHXACT2\n", byte(kindLen), I.Kind, header), G), input, 9, nil},
		{"action magic with CR terminator", hactJoin(hactRawAction("PHXACT1\r", byte(kindLen), I.Kind, header), G), input, 9, nil},
		{"lowercase action magic", hactJoin(hactRawAction("phxact1\n", byte(kindLen), I.Kind, header), G), input, 9, nil},
		{"plain stage header instead of an action frame", hactJoin(header, G), input, 9, nil},
		{"grant before the action frame", hactJoin(G, A), input, 9, nil},
		{"empty stdin", nil, input, 0, nil},

		// Stream shape after the grant: real EOF or nothing.
		{"grant missing entirely", A, input, len(A), nil},
		{"one extra byte after the grant", hactJoin(A, G, []byte("x")), input, len(A) + len(G) + 1, nil},
		{"a payload after the grant", hactJoin(A, G, bytes.Repeat([]byte("RAW-BINARY-PAYLOAD"), 100)), input, len(A) + len(G) + 1, nil},
		{"LF after the grant", hactJoin(A, G, []byte("\n")), input, len(A) + len(G) + 1, nil},
		{"CRLF after the grant", hactJoin(A, G, []byte("\r\n")), input, len(A) + len(G) + 1, nil},
		{"NUL after the grant", hactJoin(A, G, []byte{0}), input, len(A) + len(G) + 1, nil},
		{"duplicate grant", hactJoin(A, G, G), input, len(A) + len(G) + 1, nil},
		{"second action frame after the grant", hactJoin(A, G, A), input, len(A) + len(G) + 1, nil},
	}
	// Every strict prefix of the action frame is a short stream.
	for _, cut := range []int{1, 8, 9, 9 + kindLen - 1, 9 + kindLen, 9 + kindLen + 5, 9 + kindLen + 12, len(A) - 1} {
		cases = append(cases, hactRefusal{fmt.Sprintf("action frame truncated at %d", cut), A[:cut], input, cut, nil})
	}
	// Every strict prefix of the grant frame is a short stream.
	for _, cut := range []int{1, 7, 8, 9, 40, len(G) - 1} {
		cases = append(cases, hactRefusal{fmt.Sprintf("grant frame truncated at %d", cut), hactJoin(A, G[:cut]), input, len(A) + cut, nil})
	}
	return cases
}

func TestRunAuthorizedProbeHostActionHelperRefusesUnauthorizedOrMalformedStreamsWithoutApplying(t *testing.T) {
	for _, kind := range hactKinds {
		// Precondition: the untampered stream is accepted, so every refusal
		// below is caused by its one defect and not by a broken fixture.
		control := newHactHarness(t, kind)
		control.wantApplied(control.run(context.Background(), nil))

		for _, tc := range hactRefusalCases(t, newHactHarness(t, kind)) {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				h := newHactHarness(t, kind)
				h.setStream(tc.stream)
				if tc.hostKeys != nil {
					keys := tc.hostKeys
					h.hostKeysFn = func(context.Context) ([]string, error) { return keys, nil }
				}
				code := h.run(context.Background(), nil)
				h.wantFailedAfterChallenge(code, tc.want, tc.consumed, 0)
				h.wantCalls(1, 1)
				if h.entropy.Len() != 32 {
					t.Fatalf("entropy remaining %d, want 32", h.entropy.Len())
				}
			})
		}
	}
}

func TestRunAuthorizedProbeHostActionHelperForeignHostKeyNeverReadsTheGrantFrame(t *testing.T) {
	for _, kind := range hactKinds {
		h := newHactHarness(t, kind)
		h.hostKeysFn = func(context.Context) ([]string, error) { return []string{hactOtherFingerprint}, nil }
		var tail atomic.Int64
		// Everything after the action frame is a tripwire: any read of it counts.
		h.setReader(io.MultiReader(bytes.NewReader(h.action), authzFuncReader(func(p []byte) (int, error) {
			tail.Add(int64(len(p)))
			return copy(p, h.grant), nil
		})))
		code := h.run(context.Background(), nil)
		h.wantFailedAfterChallenge(code, domain.ErrProbeBinaryStageWriteUnauthorized, len(h.action), 0)
		if tail.Load() != 0 {
			t.Fatalf("%s: a grant-frame read of %d bytes was attempted after a foreign host key", kind, tail.Load())
		}
	}
}

func TestRunAuthorizedProbeHostActionHelperSnapshotsHostKeys(t *testing.T) {
	// A caller-owned slice mutated after the helper took its snapshot (here
	// during the challenge write) must not turn a foreign key into a trusted one.
	h := newHactHarness(t, domain.ProbeHostActionDataDirectory)
	trusted := []string{hactOtherFingerprint}
	h.hostKeysFn = func(context.Context) ([]string, error) { return trusted, nil }
	h.out.script = func(_ int, p []byte) (int, error) {
		trusted[0] = h.intent.Identity.HostKeyFingerprint
		return len(p), nil
	}
	h.wantFailedAfterChallenge(h.run(context.Background(), nil), domain.ErrProbeBinaryStageWriteUnauthorized, len(h.action), 0)
}

func TestRunAuthorizedProbeHostActionHelperAcceptsHostKeyAnywhereInList(t *testing.T) {
	for _, position := range []string{"first", "middle", "last"} {
		h := newHactHarness(t, domain.ProbeHostActionUnitFile)
		fp := h.intent.Identity.HostKeyFingerprint
		list := map[string][]string{
			"first":  {fp, hactOtherFingerprint, "SHA256:x"},
			"middle": {hactOtherFingerprint, fp, "SHA256:x"},
			"last":   {hactOtherFingerprint, "SHA256:x", fp},
		}[position]
		h.hostKeysFn = func(context.Context) ([]string, error) { return list, nil }
		h.wantApplied(h.run(context.Background(), nil))
	}
}

func TestRunAuthorizedProbeHostActionHelperStdinFaultsAreSanitizedAndNeverApply(t *testing.T) {
	canary := errors.New("sensitive-read-canary")
	cases := []struct {
		name string
		mk   func(h *hactHarness) io.Reader
		want error
	}{
		{"read fault before the action frame", func(h *hactHarness) io.Reader { return iotest.ErrReader(canary) }, domain.ErrProbeArtifactStorage},
		{"read fault inside the action frame", func(h *hactHarness) io.Reader {
			return io.MultiReader(bytes.NewReader(h.action[:20]), iotest.ErrReader(canary))
		}, domain.ErrProbeArtifactStorage},
		{"read fault inside the grant frame", func(h *hactHarness) io.Reader {
			return io.MultiReader(bytes.NewReader(hactJoin(h.action, h.grant[:10])), iotest.ErrReader(canary))
		}, domain.ErrProbeArtifactStorage},
		{"read fault where EOF should be", func(h *hactHarness) io.Reader {
			return io.MultiReader(bytes.NewReader(hactJoin(h.action, h.grant)), iotest.ErrReader(canary))
		}, domain.ErrProbeArtifactStorage},
		{"unexpected EOF where EOF should be", func(h *hactHarness) io.Reader {
			return io.MultiReader(bytes.NewReader(hactJoin(h.action, h.grant)), iotest.ErrReader(io.ErrUnexpectedEOF))
		}, domain.ErrProbeArtifactStorage},
		{"context error from the reader inside the grant", func(h *hactHarness) io.Reader {
			return io.MultiReader(bytes.NewReader(hactJoin(h.action, h.grant[:10])), iotest.ErrReader(context.Canceled))
		}, context.Canceled},
		{"reader that never makes progress", func(h *hactHarness) io.Reader {
			return authzFuncReader(func([]byte) (int, error) { return 0, nil })
		}, domain.ErrProbeArtifactStorage},
		{"reader that over-reports its byte count", func(h *hactHarness) io.Reader {
			return authzFuncReader(func(p []byte) (int, error) { return len(p) + 1, nil })
		}, domain.ErrProbeArtifactStorage},
		{"reader that stalls at EOF time", func(h *hactHarness) io.Reader {
			return io.MultiReader(bytes.NewReader(hactJoin(h.action, h.grant)), authzFuncReader(func([]byte) (int, error) { return 0, nil }))
		}, domain.ErrProbeArtifactStorage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHactHarness(t, domain.ProbeHostActionService)
			h.setReader(tc.mk(h))
			code := h.run(context.Background(), nil)
			h.wantFailedAfterChallenge(code, tc.want, -1, 0)
		})
	}
}

func TestReadAuthorizedProbeHostActionReturnsExactIntentAndZeroOnEveryRefusal(t *testing.T) {
	var zero domain.ProbeHostActionIntent
	for _, kind := range hactKinds {
		h := newHactHarness(t, kind)
		trusted := []string{hactOtherFingerprint, h.intent.Identity.HostKeyFingerprint}
		for name, wrap := range map[string]func(io.Reader) io.Reader{
			"whole reads": func(r io.Reader) io.Reader { return r },
			"one byte":    iotest.OneByteReader,
			"half reads":  iotest.HalfReader,
			"data+eof":    iotest.DataErrReader,
		} {
			inner := bytes.NewReader(hactJoin(h.action, h.grant))
			got, err := readAuthorizedProbeHostAction(context.Background(), wrap(inner), h.keys.pub, h.nonce, trusted)
			if err != nil || got != h.intent || inner.Len() != 0 {
				t.Fatalf("%s/%s: intent=%+v err=%v remaining=%d", kind, name, got, err, inner.Len())
			}
		}

		// The binding is to the key, nonce and host keys passed in.
		for name, tc := range map[string]struct {
			key    [32]byte
			nonce  [32]byte
			finger []string
			want   error
		}{
			"other nonce":   {h.keys.pub, authzNonce("another-process"), trusted, domain.ErrProbeBinaryStageWriteUnauthorized},
			"other key":     {authzNewKeys(t, "other").pub, h.nonce, trusted, domain.ErrProbeBinaryStageWriteUnauthorized},
			"zero key":      {[32]byte{}, h.nonce, trusted, domain.ErrProbeBinaryStageWriteUnauthorized},
			"zero nonce":    {h.keys.pub, [32]byte{}, trusted, domain.ErrProbeBinaryStageWriteUnauthorized},
			"foreign host":  {h.keys.pub, h.nonce, []string{hactOtherFingerprint}, domain.ErrProbeBinaryStageWriteUnauthorized},
			"no host keys":  {h.keys.pub, h.nonce, nil, domain.ErrProbeBinaryStageWriteUnauthorized},
			"empty host fp": {h.keys.pub, h.nonce, []string{""}, domain.ErrProbeBinaryStageWriteUnauthorized},
		} {
			got, err := readAuthorizedProbeHostAction(context.Background(), bytes.NewReader(hactJoin(h.action, h.grant)), tc.key, tc.nonce, tc.finger)
			if err != tc.want || got != zero {
				t.Fatalf("%s/%s: intent=%+v err=%v, want zero and %v", kind, name, got, err, tc.want)
			}
		}

		// Every case the helper table refuses also returns the zero intent here.
		for _, tc := range hactRefusalCases(t, h) {
			fingerprints := trusted
			if tc.hostKeys != nil {
				fingerprints = tc.hostKeys
			}
			inner := bytes.NewReader(tc.stream)
			got, err := readAuthorizedProbeHostAction(context.Background(), inner, h.keys.pub, h.nonce, fingerprints)
			if err != tc.want || got != zero {
				t.Fatalf("%s/%s: intent=%+v err=%v, want zero and %v", kind, tc.name, got, err, tc.want)
			}
			if tc.consumed >= 0 && len(tc.stream)-inner.Len() != tc.consumed {
				t.Fatalf("%s/%s: consumed %d, want %d", kind, tc.name, len(tc.stream)-inner.Len(), tc.consumed)
			}
		}

		// A dead context is a context refusal and reads nothing.
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		inner := bytes.NewReader(hactJoin(h.action, h.grant))
		if got, err := readAuthorizedProbeHostAction(canceled, inner, h.keys.pub, h.nonce, trusted); err != context.Canceled || got != zero || inner.Len() != h.wholeStreamLen() {
			t.Fatalf("%s: canceled ctx intent=%+v err=%v remaining=%d", kind, got, err, inner.Len())
		}
		// Cancellation landing during the last grant byte withholds the intent.
		live, abort := context.WithCancel(context.Background())
		src := bytes.NewReader(hactJoin(h.action, h.grant))
		cancelOnEOF := authzFuncReader(func(p []byte) (int, error) {
			n, err := src.Read(p)
			if src.Len() == 0 {
				abort()
			}
			return n, err
		})
		if got, err := readAuthorizedProbeHostAction(live, cancelOnEOF, h.keys.pub, h.nonce, trusted); err != context.Canceled || got != zero {
			t.Fatalf("%s: cancel after a valid grant intent=%+v err=%v, want zero and context.Canceled", kind, got, err)
		}
	}
}

func TestProbeHostActionRequireEOF(t *testing.T) {
	ctx := context.Background()
	canary := errors.New("sensitive-read-canary")

	for name, r := range map[string]io.Reader{
		"empty":         bytes.NewReader(nil),
		"one byte read": iotest.OneByteReader(bytes.NewReader(nil)),
		"data+eof none": iotest.DataErrReader(bytes.NewReader(nil)),
	} {
		if err := probeHostActionRequireEOF(ctx, r); err != nil {
			t.Fatalf("%s: clean EOF refused: %v", name, err)
		}
	}

	// Empty reads are tolerated up to a bound, then refused as a stall.
	var reads atomic.Int32
	stall := func(before int, after func() (int, error)) io.Reader {
		reads.Store(0)
		return authzFuncReader(func([]byte) (int, error) {
			if int(reads.Add(1)) <= before {
				return 0, nil
			}
			return after()
		})
	}
	if err := probeHostActionRequireEOF(ctx, stall(probeBinaryStageHelperMaxEmptyReads-1, func() (int, error) { return 0, io.EOF })); err != nil {
		t.Fatalf("99 empty reads then EOF refused: %v", err)
	}
	if err := probeHostActionRequireEOF(ctx, stall(1<<30, func() (int, error) { return 0, io.EOF })); err != domain.ErrProbeArtifactStorage {
		t.Fatalf("endless empty reads err=%v, want the bounded storage refusal", err)
	}
	if got := reads.Load(); got != probeBinaryStageHelperMaxEmptyReads {
		t.Fatalf("stalled reader was read %d times, want exactly %d", got, probeBinaryStageHelperMaxEmptyReads)
	}

	// Any byte is an input refusal, however it arrives.
	for name, r := range map[string]io.Reader{
		"one byte then EOF":           bytes.NewReader([]byte("x")),
		"byte together with EOF":      authzFuncReader(func(p []byte) (int, error) { p[0] = 'x'; return 1, io.EOF }),
		"byte with nil error":         authzFuncReader(func(p []byte) (int, error) { p[0] = 'x'; return 1, nil }),
		"NUL byte":                    bytes.NewReader([]byte{0}),
		"byte after empty reads":      stall(10, func() (int, error) { return 1, nil }),
		"byte together with an error": authzFuncReader(func(p []byte) (int, error) { return 1, canary }),
	} {
		if err := probeHostActionRequireEOF(ctx, r); err != domain.ErrProbeBinaryStageWriteInput {
			t.Fatalf("%s: err=%v, want the input refusal", name, err)
		}
	}

	// Faults are sanitized, and only real context errors surface as such.
	for name, tc := range map[string]struct {
		err  error
		want error
	}{
		"arbitrary canary":    {canary, domain.ErrProbeArtifactStorage},
		"unexpected EOF":      {io.ErrUnexpectedEOF, domain.ErrProbeArtifactStorage},
		"closed pipe":         {io.ErrClosedPipe, domain.ErrProbeArtifactStorage},
		"storage sentinel":    {domain.ErrProbeArtifactStorage, domain.ErrProbeArtifactStorage},
		"context canceled":    {context.Canceled, context.Canceled},
		"deadline exceeded":   {context.DeadlineExceeded, context.DeadlineExceeded},
		"wrapped canary only": {fmt.Errorf("pipe: %w", canary), domain.ErrProbeArtifactStorage},
	} {
		err := probeHostActionRequireEOF(ctx, iotest.ErrReader(tc.err))
		if err != tc.want || strings.Contains(fmt.Sprint(err), "canary") {
			t.Fatalf("%s: err=%v, want %v", name, err, tc.want)
		}
	}

	// A dead context reads nothing.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	var dead atomic.Int32
	if err := probeHostActionRequireEOF(canceled, authzFuncReader(func([]byte) (int, error) { dead.Add(1); return 0, io.EOF })); err != context.Canceled || dead.Load() != 0 {
		t.Fatalf("canceled ctx err=%v reads=%d", err, dead.Load())
	}
	expired, stop := context.WithTimeout(context.Background(), 0)
	defer stop()
	<-expired.Done()
	if err := probeHostActionRequireEOF(expired, bytes.NewReader(nil)); err != context.DeadlineExceeded {
		t.Fatalf("expired ctx err=%v", err)
	}
	// Cancellation between two empty reads stops the loop before another read.
	live, abort := context.WithCancel(context.Background())
	var between atomic.Int32
	if err := probeHostActionRequireEOF(live, authzFuncReader(func([]byte) (int, error) { between.Add(1); abort(); return 0, nil })); err != context.Canceled || between.Load() != 1 {
		t.Fatalf("cancel between reads err=%v reads=%d", err, between.Load())
	}
}

func TestRunAuthorizedProbeHostActionHelperApplyErrorMapping(t *testing.T) {
	input, conflict, unavailable, unknown := domain.ErrProbeBinaryStageWriteInput, domain.ErrProbeBinaryStageWriteConflict,
		domain.ErrProbeBinaryStageWriteUnavailable, domain.ErrProbeBinaryStageWriteUnknown
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"input", input, input},
		{"conflict", conflict, conflict},
		{"unavailable", unavailable, unavailable},
		{"wrapped input", fmt.Errorf("decode: %w", input), input},
		{"wrapped conflict", fmt.Errorf("mkdirat /var/lib/x: %w", conflict), conflict},
		{"wrapped unavailable", fmt.Errorf("open parent: %w", unavailable), unavailable},
		{"unknown", unknown, unknown},
		{"wrapped unknown", fmt.Errorf("readback: %w", unknown), unknown},
		{"unauthorized is not echoed", domain.ErrProbeBinaryStageWriteUnauthorized, unknown},
		{"artifact storage is not echoed", domain.ErrProbeArtifactStorage, unknown},
		{"context canceled is not a definite refusal", context.Canceled, unknown},
		{"deadline exceeded is not a definite refusal", context.DeadlineExceeded, unknown},
		{"arbitrary canary", errors.New("sensitive-apply-canary /etc/systemd/system/uptime-phoenix-probe-x.service"), unknown},
	}
	for _, kind := range hactKinds {
		for _, tc := range cases {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				h := newHactHarness(t, kind)
				h.applyFn = func(context.Context, domain.ProbeHostActionIntent) error { return tc.err }
				code := h.run(context.Background(), nil)
				h.wantFailedAfterChallenge(code, tc.want, h.wholeStreamLen(), 1)
				c := h.applyCalls()[0]
				if c.intent != h.intent || !c.inputClosed {
					t.Fatalf("apply intent=%+v inputClosed=%v", c.intent, c.inputClosed)
				}
				if bytes.Contains(h.out.Bytes(), []byte("host_action_applied")) {
					t.Fatal("a receipt was written for a failed apply")
				}
			})
		}
	}
}

// The intended contract: any error that is Unknown (possible effect) must stay
// Unknown even if it also carries a definite pre-effect sentinel.
func TestRunAuthorizedProbeHostActionHelperJoinedUnknownApplyErrorStaysUnknown(t *testing.T) {
	for _, definite := range []error{domain.ErrProbeBinaryStageWriteInput, domain.ErrProbeBinaryStageWriteConflict, domain.ErrProbeBinaryStageWriteUnavailable} {
		h := newHactHarness(t, domain.ProbeHostActionDataDirectory)
		h.applyFn = func(context.Context, domain.ProbeHostActionIntent) error {
			return errors.Join(domain.ErrProbeBinaryStageWriteUnknown, definite)
		}
		h.wantFailedAfterChallenge(h.run(context.Background(), nil), domain.ErrProbeBinaryStageWriteUnknown, h.wholeStreamLen(), 1)
	}
}

func TestRunAuthorizedProbeHostActionHelperContextCanceledDuringApplyIsNeverSuccess(t *testing.T) {
	cases := map[string]func(c context.Context) error{
		"returns nil after the cancel":             func(context.Context) error { return nil },
		"returns the context error":                func(c context.Context) error { return c.Err() },
		"returns unknown":                          func(context.Context) error { return domain.ErrProbeBinaryStageWriteUnknown },
		"returns an arbitrary error with a canary": func(context.Context) error { return errors.New("sensitive-apply-canary") },
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, abort := context.WithCancel(context.Background())
			h := newHactHarness(t, domain.ProbeHostActionUnitFile)
			h.applyFn = func(c context.Context, _ domain.ProbeHostActionIntent) error {
				abort()
				if c.Err() == nil {
					t.Error("the parent cancellation did not reach the apply context")
				}
				return fn(c)
			}
			code := h.run(ctx, nil)
			h.wantFailedAfterChallenge(code, domain.ErrProbeBinaryStageWriteUnknown, h.wholeStreamLen(), 1)
			if bytes.Contains(h.out.Bytes(), []byte("host_action_applied")) {
				t.Fatal("a receipt was written after the context died")
			}
		})
	}

	// A cancellation that arrives while apply is blocked unblocks the helper,
	// which reports the unknown outcome and never a receipt.
	ctx, abort := context.WithCancel(context.Background())
	h := newHactHarness(t, domain.ProbeHostActionService)
	started := make(chan struct{})
	h.applyFn = func(c context.Context, _ domain.ProbeHostActionIntent) error {
		close(started)
		<-c.Done()
		return c.Err()
	}
	done := make(chan int, 1)
	go func() { done <- h.run(ctx, nil) }()
	select {
	case <-started:
	case <-time.After(authzIOTimeout):
		t.Fatal("apply never started")
	}
	abort()
	select {
	case code := <-done:
		h.wantFailedAfterChallenge(code, domain.ErrProbeBinaryStageWriteUnknown, h.wholeStreamLen(), 1)
	case <-time.After(authzIOTimeout):
		t.Fatal("helper did not stop after the cancellation")
	}
}

func TestRunAuthorizedProbeHostActionHelperReceiptWriteFailureAfterApplyIsUnknown(t *testing.T) {
	cases := []struct {
		name string
		fn   func(p []byte) (int, error)
	}{
		{"error with canary", func([]byte) (int, error) { return 0, errors.New("sensitive-write-canary") }},
		{"partial write with error", func([]byte) (int, error) { return 5, errors.New("sensitive-write-canary") }},
		{"short write without error", func(p []byte) (int, error) { return len(p) - 1, nil }},
		{"zero write without error", func([]byte) (int, error) { return 0, nil }},
		{"closed pipe", func([]byte) (int, error) { return 0, io.ErrClosedPipe }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHactHarness(t, domain.ProbeHostActionDataDirectory)
			h.out.script = func(call int, p []byte) (int, error) {
				if call == 1 {
					return len(p), nil
				}
				return tc.fn(p)
			}
			code := h.run(context.Background(), nil)
			if code != 1 || h.stderr.String() != domain.ErrProbeBinaryStageWriteUnknown.Error()+"\n" || strings.Contains(h.stderr.String(), "canary") {
				t.Fatalf("code=%d stderr=%q", code, h.stderr.String())
			}
			if len(h.applyCalls()) != 1 || h.out.attemptCount() != 2 || !bytes.Equal(h.out.attempt(1), hactWantReceipt(h.intent)) {
				t.Fatalf("applies=%d writes=%d, want one apply then one attempted receipt", len(h.applyCalls()), h.out.attemptCount())
			}
		})
	}
}

func TestRunAuthorizedProbeHostActionHelperInputCloseFailureBlocksApply(t *testing.T) {
	h := newHactHarness(t, domain.ProbeHostActionDataDirectory)
	h.stdin.closeErr = errors.New("sensitive-close-canary")
	code := h.run(context.Background(), nil)
	// The whole stream was read and verified, yet an input that cannot be
	// proven closed means no effect is allowed.
	h.wantFailedAfterChallenge(code, domain.ErrProbeArtifactStorage, h.wholeStreamLen(), 0)
}

func TestRunAuthorizedProbeHostActionHelperApplyContextIsBoundedAndDerivedFromCaller(t *testing.T) {
	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "marker")
	h := newHactHarness(t, domain.ProbeHostActionDataDirectory)
	var seen any
	h.applyFn = func(c context.Context, _ domain.ProbeHostActionIntent) error {
		seen = c.Value(ctxKey{})
		return nil
	}
	h.wantApplied(h.run(ctx, nil))
	if seen != "marker" {
		t.Fatal("the apply context is not derived from the caller's context")
	}
}

func TestRunAuthorizedProbeHostActionHelperPublicEntryUsageNeverLoadsHostAuthority(t *testing.T) {
	input := &hactStdin{r: bytes.NewReader(nil)}
	var out, stderr bytes.Buffer
	code := RunAuthorizedProbeHostActionHelper(context.Background(), []string{"--help", "sensitive-path-canary"}, input, &out, &stderr)
	if code != 2 || out.Len() != 0 || input.closes.Load() != 1 || input.reads.Load() != 0 ||
		stderr.String() != probeHostActionAuthorizedUsage || strings.Contains(stderr.String(), "canary") {
		t.Fatalf("code=%d out=%q err=%q closes=%d", code, out.String(), stderr.String(), input.closes.Load())
	}
}

func TestRunAuthorizedProbeHostActionHelperPublicEntryRefusesWithoutHostAuthority(t *testing.T) {
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		t.Skip("running as root on linux would consult the real /etc/phoenix-probe; the public entry is exercised through injected seams instead")
	}
	input := &hactStdin{r: bytes.NewReader(nil)}
	var out, stderr bytes.Buffer
	code := RunAuthorizedProbeHostActionHelper(context.Background(), nil, input, &out, &stderr)
	// Not uid 0 on Linux, or not Linux at all: the key cannot be trusted, so
	// the verb refuses before any challenge and before any stdin read.
	if code != 1 || out.Len() != 0 || stderr.String() != domain.ErrProbeBinaryStageWriteUnavailable.Error()+"\n" ||
		input.closes.Load() != 1 || input.reads.Load() != 0 {
		t.Fatalf("code=%d out=%q err=%q closes=%d reads=%d", code, out.String(), stderr.String(), input.closes.Load(), input.reads.Load())
	}
}
