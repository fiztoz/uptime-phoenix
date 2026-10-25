package bootstrap

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// Mock/refusal tests for the authorized host helper. They exercise only the
// injected seams (key loader, parent acquirer, entropy) and stdin/stdout fakes;
// the real-effect lifecycle lives in probe_binary_stage_authorized_integration_test.go.

// authzKeys is a deterministic Ed25519 authority used only by tests.
type authzKeys struct {
	priv ed25519.PrivateKey
	pub  [32]byte
	id   string
}

func authzNewKeys(t *testing.T, seed string) authzKeys {
	t.Helper()
	digest := sha256.Sum256([]byte("authz-test-key/" + seed))
	priv := ed25519.NewKeyFromSeed(digest[:])
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok || len(pub) != 32 {
		t.Fatal("test key is not an Ed25519 public key")
	}
	k := authzKeys{priv: priv}
	copy(k.pub[:], pub)
	k.id = domain.ProbeBinaryStageAuthorityKeyID(k.pub)
	return k
}

func authzNonce(label string) [32]byte { return sha256.Sum256([]byte("authz-test-nonce/" + label)) }

// authzRootIntent is helperIntent with the only host user the grant domain accepts.
func authzRootIntent() domain.ProbeBinaryStageIntent {
	intent := helperIntent()
	intent.Identity.Host.User = "root"
	return intent
}

func authzRequest(k authzKeys, intent domain.ProbeBinaryStageIntent, nonce [32]byte) domain.ProbeBinaryStageAdmissionRequest {
	return domain.ProbeBinaryStageAdmissionRequest{
		Intent: intent,
		Challenge: domain.ProbeBinaryStageChallenge{
			AuthorityKeyID: k.id,
			Nonce:          nonce,
			MarkerSHA256:   intent.MarkerSHA256(),
		},
	}
}

// authzSign signs exactly the message the helper verifies.
func authzSign(t *testing.T, k authzKeys, intent domain.ProbeBinaryStageIntent, nonce [32]byte) [64]byte {
	t.Helper()
	message := authzRequest(k, intent, nonce).SigningMessage()
	if message == nil {
		t.Fatal("test request has no signing message")
	}
	var sig [64]byte
	copy(sig[:], ed25519.Sign(k.priv, message))
	return sig
}

// authzManualMessage mirrors the documented domain-separated message without
// the root/validity gate, so a correctly signed refused request can be built.
func authzManualMessage(k authzKeys, nonce [32]byte, markerSHA256 string) []byte {
	key, _ := hex.DecodeString(k.id)
	marker, _ := hex.DecodeString(markerSHA256)
	message := []byte("phoenix-stage-admission-v1\x00")
	message = append(message, key...)
	message = append(message, nonce[:]...)
	return append(message, marker...)
}

func authzSignRaw(k authzKeys, message []byte) [64]byte {
	var sig [64]byte
	copy(sig[:], ed25519.Sign(k.priv, message))
	return sig
}

// authzRawFrame is the fixed PHXGRT1 magic plus a raw signature, built without
// the production encoder so invalid grants can be framed too.
func authzRawFrame(sig [64]byte) []byte {
	return append([]byte("PHXGRT1\n"), sig[:]...)
}

// authzGrantFrame frames a valid grant through the production encoder.
func authzGrantFrame(t *testing.T, k authzKeys, intent domain.ProbeBinaryStageIntent, nonce [32]byte) []byte {
	t.Helper()
	grant := domain.ProbeBinaryStageGrant{Request: authzRequest(k, intent, nonce), Signature: authzSign(t, k, intent, nonce)}
	frame, err := provisioning.EncodeProbeBinaryStageGrantFrame(grant)
	if err != nil || len(frame) != provisioning.ProbeBinaryStageGrantFrameSize {
		t.Fatal("encode grant frame", err)
	}
	if !bytes.Equal(frame, authzRawFrame(grant.Signature)) {
		t.Fatal("production grant frame differs from the documented fixed layout")
	}
	return frame
}

// authzArgs is the full argument set so nil-refusal cases can break one field.
type authzArgs struct {
	ctx     context.Context
	input   io.ReadCloser
	out     io.Writer
	stderr  io.Writer
	loadKey func(context.Context) ([32]byte, error)
	acquire func(context.Context) (*os.File, error)
	entropy io.Reader
	// hostKeys defaults to authzIntentHost when nil.
	hostKeys func(context.Context) ([]string, error)
}

// authzIntentHost trusts the SSH host-key fingerprint every test intent pins.
func authzIntentHost(context.Context) ([]string, error) {
	return []string{helperIntent().Identity.HostKeyFingerprint}, nil
}

func (a authzArgs) run(args []string) int {
	hostKeys := a.hostKeys
	if hostKeys == nil {
		hostKeys = authzIntentHost
	}
	return runAuthorizedProbeBinaryStageHelper(a.ctx, args, a.input, a.out, a.stderr, a.loadKey, a.acquire, a.entropy, hostKeys)
}

type authzCountingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c authzCountingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

type authzFuncWriter func([]byte) (int, error)

func (f authzFuncWriter) Write(p []byte) (int, error) { return f(p) }

// authzHarness counts every injected dependency call. Defaults are a trusted key,
// a fresh open temp-file parent per acquisition and 64 bytes of fixed entropy.
type authzHarness struct {
	t      *testing.T
	keys   authzKeys
	nonce  [32]byte
	input  *hostMockInput
	out    bytes.Buffer
	stderr bytes.Buffer
	sink   io.Writer

	keyCalls     atomic.Int32
	acquireCalls atomic.Int32
	entropyBytes atomic.Int64

	keyFn     func(context.Context) ([32]byte, error)
	acquireFn func(context.Context) (*os.File, error)
	entropyR  io.Reader
	entropy   *bytes.Reader
	parents   []*os.File
}

func newAuthzHarness(t *testing.T) *authzHarness {
	t.Helper()
	h := &authzHarness{t: t, keys: authzNewKeys(t, "trusted"), input: &hostMockInput{}}
	a, b := authzNonce("entropy-a"), authzNonce("entropy-b")
	h.nonce = a
	h.entropy = bytes.NewReader(append(a[:], b[:]...))
	h.entropyR = h.entropy
	h.keyFn = func(context.Context) ([32]byte, error) { return h.keys.pub, nil }
	h.acquireFn = func(context.Context) (*os.File, error) { return h.newParent(), nil }
	return h
}

// newParent opens a private temp file whose closure proves the helper released it.
func (h *authzHarness) newParent() *os.File {
	h.t.Helper()
	f, err := os.CreateTemp(h.t.TempDir(), "authz-parent")
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = f.Close() })
	h.parents = append(h.parents, f)
	return f
}

func (h *authzHarness) args() authzArgs {
	var out io.Writer = &h.out
	if h.sink != nil {
		out = h.sink
	}
	return authzArgs{
		ctx:     context.Background(),
		input:   h.input,
		out:     out,
		stderr:  &h.stderr,
		loadKey: func(c context.Context) ([32]byte, error) { h.keyCalls.Add(1); return h.keyFn(c) },
		acquire: func(c context.Context) (*os.File, error) { h.acquireCalls.Add(1); return h.acquireFn(c) },
		entropy: authzCountingReader{r: h.entropyR, n: &h.entropyBytes},
	}
}

func (h *authzHarness) run(ctx context.Context, args []string) int {
	a := h.args()
	a.ctx = ctx
	return a.run(args)
}

func (h *authzHarness) challengeLine() []byte {
	return provisioning.EncodeProbeBinaryStageChallengeLine(h.keys.id, h.nonce)
}

// wantRefused asserts the shared pre-challenge refusal contract: exit 1, the one
// exact sanitized stderr line, no stdout, owned input closed once and never read,
// and every acquired parent released.
func (h *authzHarness) wantRefused(code int, want error) {
	h.t.Helper()
	if code != 1 {
		h.t.Fatalf("exit code %d, want 1 (stderr %q)", code, h.stderr.String())
	}
	if h.out.Len() != 0 {
		h.t.Fatalf("stdout %q emitted before refusal", h.out.String())
	}
	if got := h.stderr.String(); got != want.Error()+"\n" {
		h.t.Fatalf("stderr %q, want %q", got, want.Error()+"\n")
	}
	if strings.Contains(h.stderr.String(), "canary") || strings.Contains(h.stderr.String(), "/etc/") {
		h.t.Fatalf("stderr leaked a cause: %q", h.stderr.String())
	}
	if h.input.closes.Load() != 1 || h.input.reads.Load() != 0 {
		h.t.Fatalf("input closes=%d reads=%d, want 1/0", h.input.closes.Load(), h.input.reads.Load())
	}
	h.wantParentsClosed()
}

func (h *authzHarness) wantParentsClosed() {
	h.t.Helper()
	for i, p := range h.parents {
		if _, err := p.Stat(); !errors.Is(err, os.ErrClosed) {
			h.t.Fatalf("acquired parent %d still open: %v", i, err)
		}
	}
}

func (h *authzHarness) wantCalls(key, acquire int32) {
	h.t.Helper()
	if h.keyCalls.Load() != key || h.acquireCalls.Load() != acquire {
		h.t.Fatalf("keyLoads=%d acquires=%d, want %d/%d", h.keyCalls.Load(), h.acquireCalls.Load(), key, acquire)
	}
}

func TestRunAuthorizedProbeBinaryStageHelperUsageNeverEchoesArgsOrLoads(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"--path"}, {"--path=/opt/secret"}, {"sensitive-path-canary"}, {""}, {"a", "b"}} {
		h := newAuthzHarness(t)
		code := h.run(context.Background(), args)
		if code != 2 || h.out.Len() != 0 || h.input.closes.Load() != 1 || h.input.reads.Load() != 0 {
			t.Fatalf("args %q code=%d out=%q closes=%d reads=%d", args, code, h.out.String(), h.input.closes.Load(), h.input.reads.Load())
		}
		h.wantCalls(0, 0)
		if h.entropyBytes.Load() != 0 || len(h.parents) != 0 {
			t.Fatalf("args %q touched entropy/parent", args)
		}
		got := h.stderr.String()
		if got != probeBinaryStageAuthorizedUsage {
			t.Fatalf("args %q stderr %q, want the fixed usage", args, got)
		}
		for _, leak := range []string{"canary", "--path", "/opt/secret", "--help"} {
			if strings.Contains(got, leak) {
				t.Fatalf("args %q leaked %q into usage", args, leak)
			}
		}
	}
	if !strings.Contains(probeBinaryStageAuthorizedUsage, "stage-binary-authorized") || !strings.HasSuffix(probeBinaryStageAuthorizedUsage, "\n") {
		t.Fatalf("usage text drifted: %q", probeBinaryStageAuthorizedUsage)
	}

	// An input close failure on the usage path is a storage refusal, not exit 2.
	h := newAuthzHarness(t)
	h.input.closeErr = errors.New("sensitive-close-canary")
	code := h.run(context.Background(), []string{"--path=/opt/secret"})
	if code != 1 || h.out.Len() != 0 || h.stderr.String() != domain.ErrProbeArtifactStorage.Error()+"\n" ||
		strings.Contains(h.stderr.String(), "canary") || strings.Contains(h.stderr.String(), "/opt/secret") ||
		strings.Contains(h.stderr.String(), "Usage") {
		t.Fatalf("usage close failure code=%d err=%q", code, h.stderr.String())
	}
	h.wantCalls(0, 0)

	// A typed-nil input and a nil stderr still dispatch usage without a panic.
	h = newAuthzHarness(t)
	a := h.args()
	var typed *hostMockInput
	a.input = typed
	if code = a.run([]string{"--help"}); code != 2 || h.stderr.String() != probeBinaryStageAuthorizedUsage {
		t.Fatalf("typed-nil input usage code=%d err=%q", code, h.stderr.String())
	}
	a = h.args()
	a.stderr = (*bytes.Buffer)(nil)
	if code = a.run([]string{"--help"}); code != 2 {
		t.Fatalf("typed-nil stderr usage code=%d", code)
	}
	h.wantCalls(0, 0)

	// An empty, non-nil argument list is not a usage request.
	h = newAuthzHarness(t)
	h.keyFn = func(context.Context) ([32]byte, error) { return [32]byte{}, domain.ErrProbeBinaryStageWriteUnavailable }
	h.wantRefused(h.run(context.Background(), []string{}), domain.ErrProbeBinaryStageWriteUnavailable)
	h.wantCalls(1, 0)
}

func TestRunAuthorizedProbeBinaryStageHelperRejectsNilBeforeAnyDispatch(t *testing.T) {
	var typedCtx context.Context = (*hostMockCtx)(nil)
	var untypedCtx context.Context
	var typedInput io.ReadCloser = (*hostMockInput)(nil)
	var typedEntropy io.Reader = (*bytes.Reader)(nil)
	cases := []struct {
		name        string
		mutate      func(*authzArgs)
		inputLive   bool // the owned input is still non-nil and must be closed
		stderrIsSet bool // stderr is still writable and must carry the refusal
	}{
		{"typed-nil ctx", func(a *authzArgs) { a.ctx = typedCtx }, true, true},
		{"nil ctx", func(a *authzArgs) { a.ctx = untypedCtx }, true, true},
		{"typed-nil input", func(a *authzArgs) { a.input = typedInput }, false, true},
		{"nil input", func(a *authzArgs) { a.input = nil }, false, true},
		{"typed-nil out", func(a *authzArgs) { a.out = (*bytes.Buffer)(nil) }, true, true},
		{"nil out", func(a *authzArgs) { a.out = nil }, true, true},
		{"typed-nil stderr", func(a *authzArgs) { a.stderr = (*bytes.Buffer)(nil) }, true, false},
		{"nil stderr", func(a *authzArgs) { a.stderr = nil }, true, false},
		{"nil loadKey", func(a *authzArgs) { a.loadKey = nil }, true, true},
		{"typed-nil loadKey", func(a *authzArgs) { a.loadKey = (func(context.Context) ([32]byte, error))(nil) }, true, true},
		{"nil acquire", func(a *authzArgs) { a.acquire = nil }, true, true},
		{"nil entropy", func(a *authzArgs) { a.entropy = nil }, true, true},
		{"typed-nil entropy", func(a *authzArgs) { a.entropy = typedEntropy }, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newAuthzHarness(t)
			a := h.args()
			tc.mutate(&a)
			code := a.run(nil)
			if code != 1 || h.out.Len() != 0 {
				t.Fatalf("code=%d out=%q", code, h.out.String())
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
			if h.input.closes.Load() != wantCloses || h.input.reads.Load() != 0 {
				t.Fatalf("input closes=%d reads=%d, want %d/0", h.input.closes.Load(), h.input.reads.Load(), wantCloses)
			}
			h.wantCalls(0, 0)
			if h.entropyBytes.Load() != 0 || len(h.parents) != 0 {
				t.Fatal("a dependency was dispatched despite a nil argument")
			}
		})
	}

	// An input close failure overrides the input refusal with ArtifactStorage.
	h := newAuthzHarness(t)
	h.input.closeErr = errors.New("sensitive-close-canary")
	a := h.args()
	a.ctx = typedCtx
	if code := a.run(nil); code != 1 || h.out.Len() != 0 || h.stderr.String() != domain.ErrProbeArtifactStorage.Error()+"\n" ||
		strings.Contains(h.stderr.String(), "canary") {
		t.Fatalf("close failure code=%d err=%q", code, h.stderr.String())
	}
	h.wantCalls(0, 0)
}

func TestRunAuthorizedProbeBinaryStageHelperCancelBeforeStart(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	h := newAuthzHarness(t)
	h.wantRefused(h.run(canceled, nil), context.Canceled)
	h.wantCalls(0, 0)
	if h.entropyBytes.Load() != 0 || len(h.parents) != 0 {
		t.Fatal("canceled start loaded dependencies")
	}

	expired, stop := context.WithTimeout(context.Background(), 0)
	defer stop()
	<-expired.Done()
	h = newAuthzHarness(t)
	h.wantRefused(h.run(expired, nil), context.DeadlineExceeded)
	h.wantCalls(0, 0)

	// Cancellation arriving inside key loading is preserved and nothing follows it.
	live, abort := context.WithCancel(context.Background())
	h = newAuthzHarness(t)
	h.keyFn = func(c context.Context) ([32]byte, error) { abort(); return [32]byte{}, c.Err() }
	h.wantRefused(h.run(live, nil), context.Canceled)
	h.wantCalls(1, 0)
	if h.entropyBytes.Load() != 0 {
		t.Fatal("entropy read after a canceled key load")
	}
}

func TestRunAuthorizedProbeBinaryStageHelperCancelAfterPrerequisitesEmitsNoChallenge(t *testing.T) {
	// A context that dies while the key loads (without an error) or while the
	// parent is acquired must not leak a challenge for a helper that cannot stage.
	for _, where := range []string{"key", "acquire"} {
		t.Run(where, func(t *testing.T) {
			ctx, abort := context.WithCancel(context.Background())
			h := newAuthzHarness(t)
			if where == "key" {
				h.keyFn = func(context.Context) ([32]byte, error) { abort(); return h.keys.pub, nil }
			} else {
				h.acquireFn = func(context.Context) (*os.File, error) { abort(); return h.newParent(), nil }
			}
			h.wantRefused(h.run(ctx, nil), context.Canceled)
			h.wantCalls(1, 1)
			if len(h.parents) != 1 {
				t.Fatalf("acquired %d parents", len(h.parents))
			}
		})
	}
}

func TestRunAuthorizedProbeBinaryStageHelperKeyLoadRefusalSanitizesAndEmitsNothing(t *testing.T) {
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
			h := newAuthzHarness(t)
			h.keyFn = func(context.Context) ([32]byte, error) { return tc.key, tc.err }
			h.wantRefused(h.run(context.Background(), nil), tc.want)
			h.wantCalls(1, 0)
			if h.entropyBytes.Load() != 0 || len(h.parents) != 0 {
				t.Fatalf("entropy=%d parents=%d after a failed key load", h.entropyBytes.Load(), len(h.parents))
			}
		})
	}
	// An input close failure outranks the sanitized key refusal.
	h := newAuthzHarness(t)
	h.input.closeErr = errors.New("sensitive-close-canary")
	h.keyFn = func(context.Context) ([32]byte, error) { return [32]byte{}, domain.ErrProbeBinaryStageWriteUnavailable }
	code := h.run(context.Background(), nil)
	if code != 1 || h.out.Len() != 0 || h.stderr.String() != domain.ErrProbeArtifactStorage.Error()+"\n" || h.acquireCalls.Load() != 0 {
		t.Fatalf("close over key refusal code=%d err=%q", code, h.stderr.String())
	}
}

func TestRunAuthorizedProbeBinaryStageHelperEntropyRefusalEmitsNothing(t *testing.T) {
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
			h := newAuthzHarness(t)
			h.entropyR = tc.r
			h.wantRefused(h.run(context.Background(), nil), domain.ErrProbeBinaryStageWriteUnavailable)
			// The key is loaded first, but no parent is acquired for a helper
			// that cannot make a challenge.
			h.wantCalls(1, 0)
			if len(h.parents) != 0 {
				t.Fatal("parent acquired after an entropy failure")
			}
		})
	}
}

func TestRunAuthorizedProbeBinaryStageHelperAcquireRefusalEmitsNothing(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"nil success", nil, domain.ErrProbeBinaryStageWriteUnavailable},
		{"unknown canary", errors.New("sensitive-acquire-canary /opt/secret"), domain.ErrProbeBinaryStageWriteUnavailable},
		{"wrapped unavailable", fmt.Errorf("open /opt/secret: %w", domain.ErrProbeBinaryStageWriteUnavailable), domain.ErrProbeBinaryStageWriteUnavailable},
		{"input sentinel is not echoed", domain.ErrProbeBinaryStageWriteInput, domain.ErrProbeBinaryStageWriteUnavailable},
		{"storage", domain.ErrProbeArtifactStorage, domain.ErrProbeArtifactStorage},
		{"wrapped storage", fmt.Errorf("dup parent: %w", domain.ErrProbeArtifactStorage), domain.ErrProbeArtifactStorage},
		{"canceled sentinel", context.Canceled, context.Canceled},
		{"deadline sentinel", context.DeadlineExceeded, context.DeadlineExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newAuthzHarness(t)
			h.acquireFn = func(context.Context) (*os.File, error) { return nil, tc.err }
			h.wantRefused(h.run(context.Background(), nil), tc.want)
			h.wantCalls(1, 1)
		})
	}

	// An error-bearing but non-nil parent is defensively closed, and the real
	// descriptor is released without echoing the cause.
	h := newAuthzHarness(t)
	h.acquireFn = func(context.Context) (*os.File, error) { return h.newParent(), errors.New("sensitive-acquire-canary") }
	h.wantRefused(h.run(context.Background(), nil), domain.ErrProbeBinaryStageWriteUnavailable)
	h.wantCalls(1, 1)
	if len(h.parents) != 1 {
		t.Fatalf("parents=%d", len(h.parents))
	}

	// A zero os.File cannot close: that louder fact wins over every other cause,
	// including a live caller's cancellation.
	h = newAuthzHarness(t)
	h.acquireFn = func(context.Context) (*os.File, error) { return &os.File{}, errors.New("sensitive-acquire-canary") }
	h.wantRefused(h.run(context.Background(), nil), domain.ErrProbeArtifactStorage)
	live, abort := context.WithCancel(context.Background())
	h = newAuthzHarness(t)
	h.acquireFn = func(c context.Context) (*os.File, error) { abort(); return &os.File{}, c.Err() }
	h.wantRefused(h.run(live, nil), domain.ErrProbeArtifactStorage)

	// Cancellation during acquisition is preserved after cleanup, even when the
	// acquirer reports some other failure.
	live, abort = context.WithCancel(context.Background())
	h = newAuthzHarness(t)
	h.acquireFn = func(context.Context) (*os.File, error) {
		abort()
		return nil, domain.ErrProbeBinaryStageWriteUnavailable
	}
	h.wantRefused(h.run(live, nil), context.Canceled)

	// An input close failure outranks the sanitized acquisition reason.
	h = newAuthzHarness(t)
	h.input.closeErr = errors.New("sensitive-close-canary")
	h.acquireFn = func(context.Context) (*os.File, error) { return nil, domain.ErrProbeBinaryStageWriteUnavailable }
	code := h.run(context.Background(), nil)
	if code != 1 || h.out.Len() != 0 || h.stderr.String() != domain.ErrProbeArtifactStorage.Error()+"\n" ||
		strings.Contains(h.stderr.String(), "canary") {
		t.Fatalf("close over acquire refusal code=%d err=%q", code, h.stderr.String())
	}
}

func TestRunAuthorizedProbeBinaryStageHelperChallengeWriteFailureRefuses(t *testing.T) {
	cases := []struct {
		name string
		fn   func(p []byte) (int, error)
	}{
		{"error with canary", func([]byte) (int, error) { return 0, errors.New("sensitive-write-canary") }},
		{"partial write with error", func(p []byte) (int, error) { return 5, errors.New("sensitive-write-canary") }},
		{"short write without error", func(p []byte) (int, error) { return len(p) - 1, nil }},
		{"zero write without error", func([]byte) (int, error) { return 0, nil }},
		{"closed pipe", func([]byte) (int, error) { return 0, io.ErrClosedPipe }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newAuthzHarness(t)
			var writes atomic.Int32
			var written []byte
			h.sink = authzFuncWriter(func(p []byte) (int, error) {
				writes.Add(1)
				written = append(written, p...)
				return tc.fn(p)
			})
			code := h.run(context.Background(), nil)
			// Nothing reached the (unused) real stdout buffer, so assert the
			// refusal contract directly: exit 1, Unavailable, parent and input
			// released, and no stdin byte ever read.
			if code != 1 || h.stderr.String() != domain.ErrProbeBinaryStageWriteUnavailable.Error()+"\n" ||
				strings.Contains(h.stderr.String(), "canary") {
				t.Fatalf("code=%d stderr=%q", code, h.stderr.String())
			}
			if h.input.closes.Load() != 1 || h.input.reads.Load() != 0 {
				t.Fatalf("input closes=%d reads=%d, want 1/0", h.input.closes.Load(), h.input.reads.Load())
			}
			h.wantCalls(1, 1)
			if len(h.parents) != 1 {
				t.Fatalf("parents=%d", len(h.parents))
			}
			h.wantParentsClosed()
			if writes.Load() != 1 || !bytes.Equal(written, h.challengeLine()) {
				t.Fatalf("writes=%d wrote %q, want exactly one canonical challenge", writes.Load(), written)
			}
		})
	}

	failing := func(h *authzHarness) {
		h.sink = authzFuncWriter(func([]byte) (int, error) { return 0, errors.New("sensitive-write-canary") })
	}
	// A parent that cannot be closed is the louder storage fact.
	h := newAuthzHarness(t)
	failing(h)
	h.acquireFn = func(context.Context) (*os.File, error) { return &os.File{}, nil }
	if code := h.run(context.Background(), nil); code != 1 || h.stderr.String() != domain.ErrProbeArtifactStorage.Error()+"\n" ||
		h.input.closes.Load() != 1 || h.input.reads.Load() != 0 {
		t.Fatalf("parent close failure code=%d err=%q", code, h.stderr.String())
	}
	// So is an input that cannot be closed, and the parent is still released.
	h = newAuthzHarness(t)
	failing(h)
	h.input.closeErr = errors.New("sensitive-close-canary")
	if code := h.run(context.Background(), nil); code != 1 || h.stderr.String() != domain.ErrProbeArtifactStorage.Error()+"\n" ||
		strings.Contains(h.stderr.String(), "canary") || h.input.reads.Load() != 0 {
		t.Fatalf("input close failure code=%d err=%q", code, h.stderr.String())
	}
	h.wantParentsClosed()
	// A nil/typed-nil parent from a nil-error acquirer is refused like any failure.
	h = newAuthzHarness(t)
	h.acquireFn = func(context.Context) (*os.File, error) { return nil, nil }
	h.wantRefused(h.run(context.Background(), nil), domain.ErrProbeBinaryStageWriteUnavailable)
}

func TestRunAuthorizedProbeBinaryStageHelperChallengeIsBoundAndPrecedesAnyStdinRead(t *testing.T) {
	h := newAuthzHarness(t)
	type snapshot struct {
		line         []byte
		stdinReads   int32
		stdinCloses  int32
		keyLoads     int32
		acquires     int32
		entropyBytes int64
		parentOpen   bool
	}
	var snaps []snapshot
	h.sink = authzFuncWriter(func(p []byte) (int, error) {
		s := snapshot{
			line:         append([]byte(nil), p...),
			stdinReads:   h.input.reads.Load(),
			stdinCloses:  h.input.closes.Load(),
			keyLoads:     h.keyCalls.Load(),
			acquires:     h.acquireCalls.Load(),
			entropyBytes: h.entropyBytes.Load(),
		}
		if len(h.parents) == 1 {
			_, err := h.parents[0].Stat()
			s.parentOpen = err == nil
		}
		snaps = append(snaps, s)
		return 0, errors.New("stop after the challenge")
	})
	if code := h.run(context.Background(), nil); code != 1 {
		t.Fatalf("code=%d stderr=%q", code, h.stderr.String())
	}
	if len(snaps) != 1 {
		t.Fatalf("challenge written %d times", len(snaps))
	}
	s := snaps[0]
	if s.stdinReads != 0 || s.stdinCloses != 0 || s.keyLoads != 1 || s.acquires != 1 || s.entropyBytes != 32 || !s.parentOpen {
		t.Fatalf("challenge was not emitted after exactly key+entropy+parent and before stdin: %+v", s)
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

func TestProbeBinaryStageAuthorityReadKeyAcceptsOnlyCanonicalBody(t *testing.T) {
	k := authzNewKeys(t, "trusted")
	good := provisioning.EncodeProbeBinaryStageAuthorityKey(k.pub)
	if len(good) != provisioning.ProbeBinaryStageAuthorityKeyFileSize {
		t.Fatalf("canonical body is %d bytes", len(good))
	}
	for name, wrap := range map[string]func(io.Reader) io.Reader{
		"plain":      func(r io.Reader) io.Reader { return r },
		"one byte":   iotest.OneByteReader,
		"half reads": iotest.HalfReader,
		"data+eof":   iotest.DataErrReader,
	} {
		t.Run("accepts "+name, func(t *testing.T) {
			got, err := probeBinaryStageAuthorityReadKey(wrap(bytes.NewReader(good)))
			if err != nil || got != k.pub {
				t.Fatalf("canonical body refused: %v", err)
			}
		})
	}

	upper := []byte(strings.ToUpper(string(good[:64])) + "\n")
	if bytes.Equal(upper[:64], good[:64]) {
		t.Fatal("fixture key has no hex letters; uppercase case would be vacuous")
	}
	zeroKey := append(bytes.Repeat([]byte{'0'}, 64), '\n')
	rejections := map[string][]byte{
		"empty":              nil,
		"only LF":            {'\n'},
		"64 bytes no LF":     good[:64],
		"65 bytes no LF":     append(append([]byte{}, good[:64]...), 'x'),
		"63 hex + LF":        append(append([]byte{}, good[:63]...), '\n'),
		"66 bytes extra LF":  append(append([]byte{}, good...), '\n'),
		"66 bytes CRLF":      append(append([]byte{}, good[:64]...), '\r', '\n'),
		"66 bytes trailing":  append(append([]byte{}, good...), 'x'),
		"uppercase":          upper,
		"zero key":           zeroKey,
		"non hex":            append(bytes.Repeat([]byte{'g'}, 64), '\n'),
		"leading space":      append([]byte{' '}, good[:63]...),
		"0x prefix":          append([]byte("0x"), good[:63]...),
		"embedded NUL":       append(append([]byte{}, good[:10]...), append([]byte{0}, good[11:]...)...),
		"doubled":            append(append([]byte{}, good...), good...),
		"large file":         bytes.Repeat([]byte{'a'}, 1<<16),
		"large with real LF": append(bytes.Repeat([]byte{'a'}, 1<<16), '\n'),
	}
	for name, body := range rejections {
		t.Run("rejects "+name, func(t *testing.T) {
			got, err := probeBinaryStageAuthorityReadKey(bytes.NewReader(body))
			if !errors.Is(err, domain.ErrProbeBinaryStageWriteUnavailable) || got != ([32]byte{}) {
				t.Fatalf("body %q: key=%x err=%v", body, got, err)
			}
		})
	}

	// Read faults are refused without echoing the cause, and the read is bounded.
	fault := errors.New("sensitive-read-canary")
	for name, r := range map[string]io.Reader{
		"immediate error":     iotest.ErrReader(fault),
		"error after partial": io.MultiReader(bytes.NewReader(good[:10]), iotest.ErrReader(fault)),
		"error after full":    io.MultiReader(bytes.NewReader(good), iotest.ErrReader(fault)),
		"timeout":             iotest.TimeoutReader(bytes.NewReader(good)),
	} {
		t.Run("fault "+name, func(t *testing.T) {
			// "error after full" delivers the 65 canonical bytes first; the fault
			// on the 66-byte bounded probe must still refuse the key.
			got, err := probeBinaryStageAuthorityReadKey(r)
			if err == nil || got != ([32]byte{}) || err != domain.ErrProbeBinaryStageWriteUnavailable ||
				strings.Contains(err.Error(), "canary") {
				t.Fatalf("fault not sanitized: key=%x err=%v", got, err)
			}
		})
	}
	var consumed atomic.Int64
	endless := authzCountingReader{r: iotest.OneByteReader(strings.NewReader(strings.Repeat("a", 1<<20))), n: &consumed}
	if _, err := probeBinaryStageAuthorityReadKey(endless); !errors.Is(err, domain.ErrProbeBinaryStageWriteUnavailable) {
		t.Fatalf("oversized stream err=%v", err)
	}
	if consumed.Load() > int64(provisioning.ProbeBinaryStageAuthorityKeyFileSize)+1 {
		t.Fatalf("read %d bytes of an oversized key file; the read must stay bounded", consumed.Load())
	}
}

func TestVerifyProbeBinaryStageGrantAcceptsExactFrameAndLeavesPayloadUnread(t *testing.T) {
	trusted := authzNewKeys(t, "trusted")
	nonce := authzNonce("grant-nonce")
	intent := authzRootIntent()
	if !intent.Valid() || !authzRequest(trusted, intent, nonce).Valid() {
		t.Fatal("fixture intent/request is not valid")
	}
	// The hand-built message equals the domain one, so the non-root refusal
	// test below really signs "the correct message".
	if !bytes.Equal(authzManualMessage(trusted, nonce, intent.MarkerSHA256()), authzRequest(trusted, intent, nonce).SigningMessage()) {
		t.Fatal("manual signing message drifted from the domain signing message")
	}
	frame := authzGrantFrame(t, trusted, intent, nonce)
	payload := []byte("RAW-BINARY-PAYLOAD-MUST-STAY-UNREAD")

	for name, wrap := range map[string]func(io.Reader) io.Reader{
		"whole reads": func(r io.Reader) io.Reader { return r },
		"one byte":    iotest.OneByteReader,
		"half reads":  iotest.HalfReader,
	} {
		t.Run(name, func(t *testing.T) {
			inner := bytes.NewReader(append(append([]byte{}, frame...), payload...))
			if err := verifyProbeBinaryStageGrant(context.Background(), wrap(inner), trusted.pub, nonce, intent); err != nil {
				t.Fatalf("valid grant refused: %v", err)
			}
			if inner.Len() != len(payload) {
				t.Fatalf("consumed %d bytes, want exactly %d", len(frame)+len(payload)-inner.Len(), provisioning.ProbeBinaryStageGrantFrameSize)
			}
			rest, _ := io.ReadAll(inner)
			if !bytes.Equal(rest, payload) {
				t.Fatalf("payload disturbed: %q", rest)
			}
		})
	}

	// The frame's last byte arriving together with io.EOF is still a full frame.
	if err := verifyProbeBinaryStageGrant(context.Background(), iotest.DataErrReader(bytes.NewReader(frame)), trusted.pub, nonce, intent); err != nil {
		t.Fatalf("frame with EOF on its last read refused: %v", err)
	}

	// The verification is bound to the nonce, key and intent passed in: the same
	// frame is refused for any other helper process or authority.
	for name, tc := range map[string]struct {
		key    [32]byte
		nonce  [32]byte
		intent domain.ProbeBinaryStageIntent
	}{
		"other nonce": {trusted.pub, authzNonce("another-process"), intent},
		"other key":   {authzNewKeys(t, "other").pub, nonce, intent},
		"other intent": {trusted.pub, nonce, func() domain.ProbeBinaryStageIntent {
			i := intent
			i.OwnerID = "66666666-6666-4666-8666-666666666666"
			return i
		}()},
	} {
		err := verifyProbeBinaryStageGrant(context.Background(), bytes.NewReader(frame), tc.key, tc.nonce, tc.intent)
		if err != domain.ErrProbeBinaryStageWriteUnauthorized {
			t.Fatalf("%s: err=%v, want unauthorized", name, err)
		}
	}
}

func TestVerifyProbeBinaryStageGrantRefusesUnauthorizedGrants(t *testing.T) {
	trusted := authzNewKeys(t, "trusted")
	other := authzNewKeys(t, "other")
	nonce := authzNonce("grant-nonce")
	intent := authzRootIntent()
	payload := []byte("RAW-BINARY-PAYLOAD-MUST-STAY-UNREAD")

	nonRoot := authzRootIntent()
	nonRoot.Identity.Host.User = "deploy"
	if !nonRoot.Valid() || authzRequest(trusted, nonRoot, nonce).Valid() || authzRequest(trusted, nonRoot, nonce).SigningMessage() != nil {
		t.Fatal("fixture: a non-root intent must be a valid intent but an invalid admission request")
	}
	otherAttempt := authzRootIntent()
	otherAttempt.AttemptID = "44444444-4444-4444-8444-444444444444"
	otherHost := authzRootIntent()
	otherHost.Identity.Host.Address = "elsewhere.test:22"

	good := authzSign(t, trusted, intent, nonce)
	flipFirst, flipLast := good, good
	flipFirst[0] ^= 0x01
	flipLast[63] ^= 0x80
	wrongMagic := authzRawFrame(good)
	wrongMagic[6] = '2'
	crMagic := authzRawFrame(good)
	crMagic[7] = '\r'
	lowerMagic := authzRawFrame(good)
	copy(lowerMagic, "phxgrt1\n")
	shortMagicShifted := append([]byte("PHXGRT\n"), authzRawFrame(good)[8:]...)
	shortMagicShifted = append(shortMagicShifted, 0)

	exactMessage := authzRequest(trusted, intent, nonce).SigningMessage()
	cases := []struct {
		name   string
		frame  []byte
		intent domain.ProbeBinaryStageIntent
	}{
		{"wrong magic version", wrongMagic, intent},
		{"magic with CR terminator", crMagic, intent},
		{"lowercase magic", lowerMagic, intent},
		{"shifted short magic", shortMagicShifted[:provisioning.ProbeBinaryStageGrantFrameSize], intent},
		{"zero signature", authzRawFrame([64]byte{}), intent},
		{"signature bit flip first byte", authzRawFrame(flipFirst), intent},
		{"signature bit flip last byte", authzRawFrame(flipLast), intent},
		{"other key over its own key id", authzRawFrame(authzSign(t, other, intent, nonce)), intent},
		{"other key forging the exact message", authzRawFrame(authzSignRaw(other, exactMessage)), intent},
		{"signature over a different nonce", authzRawFrame(authzSign(t, trusted, intent, authzNonce("replayed-from-an-earlier-run"))), intent},
		{"signature over a different attempt marker", authzRawFrame(authzSign(t, trusted, otherAttempt, nonce)), intent},
		{"signature over a different host marker", authzRawFrame(authzSign(t, trusted, otherHost, nonce)), intent},
		{"signature for the intent but a different intent decoded", authzRawFrame(good), otherAttempt},
		{"root signature replayed against a non-root header", authzRawFrame(good), nonRoot},
		{"non-root intent correctly signed", authzRawFrame(authzSignRaw(trusted, authzManualMessage(trusted, nonce, nonRoot.MarkerSHA256()))), nonRoot},
		{"message without the domain separator", authzRawFrame(authzSignRaw(trusted, exactMessage[len("phoenix-stage-admission-v1\x00"):])), intent},
		{"zero-value intent", authzRawFrame(good), domain.ProbeBinaryStageIntent{}},
	}
	// Precondition: the untampered grant is accepted, so every refusal below is
	// caused by its one defect and not by a broken fixture.
	if err := verifyProbeBinaryStageGrant(context.Background(), bytes.NewReader(authzRawFrame(good)), trusted.pub, nonce, intent); err != nil {
		t.Fatalf("fixture grant is not valid: %v", err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inner := bytes.NewReader(append(append([]byte{}, tc.frame...), payload...))
			err := verifyProbeBinaryStageGrant(context.Background(), inner, trusted.pub, nonce, tc.intent)
			if err != domain.ErrProbeBinaryStageWriteUnauthorized {
				t.Fatalf("err=%v, want the definite unauthorized refusal", err)
			}
			if inner.Len() != len(payload) {
				t.Fatalf("refusal consumed %d bytes, want exactly the %d-byte frame", len(tc.frame)+len(payload)-inner.Len(), provisioning.ProbeBinaryStageGrantFrameSize)
			}
		})
	}
}

func TestVerifyProbeBinaryStageGrantShortOrFaultedStream(t *testing.T) {
	trusted := authzNewKeys(t, "trusted")
	nonce := authzNonce("grant-nonce")
	intent := authzRootIntent()
	frame := authzGrantFrame(t, trusted, intent, nonce)

	for _, n := range []int{0, 1, 7, 8, 9, 40, len(frame) - 1} {
		for name, wrap := range map[string]func(io.Reader) io.Reader{
			"eof":      func(r io.Reader) io.Reader { return r },
			"data+eof": iotest.DataErrReader,
			"one byte": iotest.OneByteReader,
		} {
			err := verifyProbeBinaryStageGrant(context.Background(), wrap(bytes.NewReader(frame[:n])), trusted.pub, nonce, intent)
			if err != domain.ErrProbeBinaryStageWriteInput {
				t.Fatalf("%d bytes (%s): err=%v, want the input refusal", n, name, err)
			}
		}
	}
	err := verifyProbeBinaryStageGrant(context.Background(), io.MultiReader(bytes.NewReader(frame[:10]), iotest.ErrReader(io.ErrUnexpectedEOF)), trusted.pub, nonce, intent)
	if err != domain.ErrProbeBinaryStageWriteInput {
		t.Fatalf("unexpected EOF: err=%v", err)
	}

	// An arbitrary read fault is neither a grant nor echoed.
	fault := io.MultiReader(bytes.NewReader(frame[:10]), iotest.ErrReader(errors.New("sensitive-read-canary")))
	err = verifyProbeBinaryStageGrant(context.Background(), fault, trusted.pub, nonce, intent)
	if err != domain.ErrProbeArtifactStorage || strings.Contains(err.Error(), "canary") {
		t.Fatalf("read fault err=%v, want the bare storage refusal", err)
	}
	// A reader that never makes progress is bounded instead of spinning forever.
	noProgress := authzFuncReader(func([]byte) (int, error) { return 0, nil })
	if err = verifyProbeBinaryStageGrant(context.Background(), noProgress, trusted.pub, nonce, intent); err != domain.ErrProbeArtifactStorage {
		t.Fatalf("no-progress reader err=%v, want the bounded storage refusal", err)
	}
	// A reader that lies about its byte count cannot overrun the frame.
	liar := authzFuncReader(func(p []byte) (int, error) { return len(p) + 1, nil })
	if err = verifyProbeBinaryStageGrant(context.Background(), liar, trusted.pub, nonce, intent); err != domain.ErrProbeArtifactStorage {
		t.Fatalf("over-reporting reader err=%v, want the storage refusal", err)
	}
}

type authzFuncReader func([]byte) (int, error)

func (f authzFuncReader) Read(p []byte) (int, error) { return f(p) }

func TestVerifyProbeBinaryStageGrantRefusesBadArgumentsWithoutReading(t *testing.T) {
	trusted := authzNewKeys(t, "trusted")
	nonce := authzNonce("grant-nonce")
	intent := authzRootIntent()
	frame := authzGrantFrame(t, trusted, intent, nonce)
	var typedCtx context.Context = (*hostMockCtx)(nil)
	var untypedCtx context.Context
	cases := map[string]struct {
		ctx    context.Context
		reader func(*atomic.Int32) io.Reader
		key    [32]byte
		nonce  [32]byte
	}{
		"zero key":         {context.Background(), nil, [32]byte{}, nonce},
		"zero nonce":       {context.Background(), nil, trusted.pub, [32]byte{}},
		"nil ctx":          {untypedCtx, nil, trusted.pub, nonce},
		"typed nil ctx":    {typedCtx, nil, trusted.pub, nonce},
		"nil reader":       {context.Background(), func(*atomic.Int32) io.Reader { return nil }, trusted.pub, nonce},
		"typed nil reader": {context.Background(), func(*atomic.Int32) io.Reader { return (*bytes.Reader)(nil) }, trusted.pub, nonce},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var reads atomic.Int32
			var r io.Reader = authzFuncReader(func(p []byte) (int, error) {
				reads.Add(1)
				return copy(p, frame), nil
			})
			if tc.reader != nil {
				r = tc.reader(&reads)
			}
			err := verifyProbeBinaryStageGrant(tc.ctx, r, tc.key, tc.nonce, intent)
			if err != domain.ErrProbeBinaryStageWriteInput || reads.Load() != 0 {
				t.Fatalf("err=%v reads=%d, want the input refusal with no read", err, reads.Load())
			}
		})
	}

	// A dead context is a context refusal, not a grant decision, and reads nothing.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	inner := bytes.NewReader(frame)
	if err := verifyProbeBinaryStageGrant(canceled, inner, trusted.pub, nonce, intent); err != context.Canceled || inner.Len() != len(frame) {
		t.Fatalf("canceled ctx err=%v remaining=%d", err, inner.Len())
	}
	expired, stop := context.WithTimeout(context.Background(), 0)
	defer stop()
	<-expired.Done()
	if err := verifyProbeBinaryStageGrant(expired, bytes.NewReader(frame), trusted.pub, nonce, intent); err != context.DeadlineExceeded {
		t.Fatalf("expired ctx err=%v", err)
	}
	// Cancellation landing after the last frame byte still withholds the grant.
	live, abort := context.WithCancel(context.Background())
	complete := authzFuncReader(func(p []byte) (int, error) { abort(); return copy(p, frame), nil })
	if err := verifyProbeBinaryStageGrant(live, complete, trusted.pub, nonce, intent); err != context.Canceled {
		t.Fatalf("cancel after a valid grant err=%v, want context.Canceled", err)
	}
}
