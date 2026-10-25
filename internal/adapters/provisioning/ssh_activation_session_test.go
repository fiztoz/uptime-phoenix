package provisioning

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// These tests cover the SSH activation session factory and the two host steps
// it runs. The remote side is the loopback host-action helper emulation of
// ssh_host_action_send_test.go: protocol evidence only, with generated keys and
// tokens, no OpenSSH, no installed helper and no real probe binary.

// Independent literals: a drifting constant must not silently change a mapping.
var (
	actSessionApplied    = domain.ProbeInstallResourceOutcome{State: "applied"}
	actSessionNotStarted = domain.ProbeInstallResourceOutcome{State: "not_started", ErrorCode: "not_started"}
	actSessionUnknown    = domain.ProbeInstallResourceOutcome{State: "unknown", ErrorCode: "outcome_unknown"}
)

type actSessionHarness struct {
	*hostActSendHarness
	factory  *SSHProbeActivationSessionFactory
	session  ports.ProbeActivationHostSession
	clientFP string
}

// newActSessionHarness serves a helper that accepts any host-action kind of the
// fixture's identity: probe_init is answered with the canonical probe_init
// receipt and every other kind with the plain host-action receipt.
func newActSessionHarness(t *testing.T, tune func(helper *hostActSendHelper, auth *hostActSendAuthorizer)) *actSessionHarness {
	t.Helper()
	h := &actSessionHarness{}
	h.hostActSendHarness = newHostActSendHarness(t, "", func(helper *hostActSendHelper, auth *hostActSendAuthorizer) {
		helper.tail = func(_ *sendTFixture, intent domain.ProbeHostActionIntent, _ []byte) string {
			if intent.Kind == domain.ProbeActivationProbeInit {
				return string(EncodeProbeInitReceipt(intent, probeInitTestResult()))
			}
			return string(EncodeProbeHostActionReceipt(intent))
		}
		if tune != nil {
			tune(helper, auth)
		}
	})
	factory, err := NewSSHProbeActivationSessionFactory(h.f.policy(), h.auth)
	if err != nil {
		t.Fatal(err)
	}
	h.factory = factory
	h.session, h.clientFP = h.newSessionFrom(t, factory, h.f.hostFP())
	return h
}

func (h *actSessionHarness) newSessionFrom(t *testing.T, factory *SSHProbeActivationSessionFactory, hostPin string) (ports.ProbeActivationHostSession, string) {
	t.Helper()
	session, fingerprint, err := factory.NewActivationSession(t.Context(), bytes.Clone(h.f.clientKey), hostPin)
	if err != nil || session == nil {
		t.Fatalf("NewActivationSession = %v %q %v", session, fingerprint, err)
	}
	return session, fingerprint
}

// use selects the intent of kind as the one the harness' expectations describe.
func (h *actSessionHarness) use(kind string) domain.ProbeHostActionIntent {
	h.intent = hostActSendIntentFor(h.f, kind)
	return h.intent
}

func (h *actSessionHarness) requireNoExec(t *testing.T) {
	t.Helper()
	if exec := h.f.execCalls.Load(); exec != 0 {
		t.Fatalf("an exec ran (%d) for an outcome that reached no helper", exec)
	}
	if got := h.f.stdinBytes(); len(got) != 0 {
		t.Fatalf("server received %d stdin bytes", len(got))
	}
}

// requireNotStartedEvidence: not_started is only legitimate when no stdin byte
// ever reached a helper.
func (h *actSessionHarness) requireNotStartedEvidence(t *testing.T) {
	t.Helper()
	if h.f.execCalls.Load() > 0 {
		h.helper.waitIdle(t, h.f)
	}
	if got := h.f.stdinBytes(); len(got) != 0 {
		t.Fatalf("not_started although the helper received %d stdin bytes", len(got))
	}
}

// requireUnknownEvidence: unknown is what a delivered action with an
// unconfirmed receipt must be reported as.
func (h *actSessionHarness) requireUnknownEvidence(t *testing.T, wantStdin []byte) {
	t.Helper()
	h.helper.waitIdle(t, h.f)
	if got := h.f.stdinBytes(); !bytes.Equal(got, wantStdin) {
		t.Fatalf("server stdin = %d bytes, want %d", len(got), len(wantStdin))
	}
}

func (h *actSessionHarness) probeInit(t *testing.T, ctx context.Context) (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts, string) {
	t.Helper()
	type out struct {
		outcome domain.ProbeInstallResourceOutcome
		facts   domain.ProbeActivationFacts
		token   string
	}
	done := make(chan out, 1)
	intent := h.intent
	go func() {
		o, f, tok := h.session.ProbeInit(ctx, intent)
		done <- out{o, f, tok}
	}()
	select {
	case r := <-done:
		return r.outcome, r.facts, r.token
	case <-time.After(30 * time.Second):
		t.Fatal("ProbeInit did not return")
		return domain.ProbeInstallResourceOutcome{}, domain.ProbeActivationFacts{}, ""
	}
}

func (h *actSessionHarness) serviceStart(t *testing.T, ctx context.Context) domain.ProbeInstallResourceOutcome {
	t.Helper()
	done := make(chan domain.ProbeInstallResourceOutcome, 1)
	intent := h.intent
	go func() { done <- h.session.ServiceStart(ctx, intent) }()
	select {
	case r := <-done:
		return r
	case <-time.After(30 * time.Second):
		t.Fatal("ServiceStart did not return")
		return domain.ProbeInstallResourceOutcome{}
	}
}

func actSessionRequireOutcome(t *testing.T, got, want domain.ProbeInstallResourceOutcome) {
	t.Helper()
	if got != want {
		t.Fatalf("outcome %+v, want %+v", got, want)
	}
	if !got.Valid() {
		t.Fatalf("outcome %+v is not one an executor may submit", got)
	}
}

func actSessionRequireNoSecrets(t *testing.T, facts domain.ProbeActivationFacts, token string) {
	t.Helper()
	if facts != (domain.ProbeActivationFacts{}) || token != "" {
		t.Fatalf("a non-applied outcome carried facts %+v or a token %q", facts, token)
	}
}

// ---- factory ----------------------------------------------------------------

func TestSSHProbeActivationSessionFactoryRequiresAnAuthorizer(t *testing.T) {
	policy := probe.EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
	var typedNilPointer *hostActSendAuthorizer
	var typedNilFunc hostActSendFuncAuthorizer
	for name, authorizer := range map[string]ProbeHostActionAuthorizer{
		"untyped nil":      nil,
		"typed nil struct": typedNilPointer,
		"typed nil func":   typedNilFunc,
	} {
		factory, err := NewSSHProbeActivationSessionFactory(policy, authorizer)
		if factory != nil || err != domain.ErrValidation {
			t.Fatalf("%s: %v %v, want nil and ErrValidation", name, factory, err)
		}
	}
	factory, err := NewSSHProbeActivationSessionFactory(policy, newHostActSendAuthorizer(newAuthSendAuthority(t)))
	if err != nil || factory == nil {
		t.Fatalf("valid factory: %v %v", factory, err)
	}
}

func TestSSHProbeActivationSessionFactoryClonesThePolicy(t *testing.T) {
	prefixes := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32"), netip.MustParsePrefix("10.0.0.0/8")}
	factory, err := NewSSHProbeActivationSessionFactory(probe.EndpointPolicy{AllowedCIDRs: prefixes}, newHostActSendAuthorizer(newAuthSendAuthority(t)))
	if err != nil {
		t.Fatal(err)
	}
	// A caller widening its own slice afterwards must not widen the factory.
	prefixes[0] = netip.MustParsePrefix("0.0.0.0/0")
	prefixes[1] = netip.MustParsePrefix("0.0.0.0/0")
	want := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32"), netip.MustParsePrefix("10.0.0.0/8")}
	if !slices.Equal(factory.policy.AllowedCIDRs, want) {
		t.Fatalf("factory policy %v, want %v", factory.policy.AllowedCIDRs, want)
	}
}

func TestNewActivationSessionRefusals(t *testing.T) {
	h := newActSessionHarness(t, nil)
	ctx := t.Context()

	requireRefused := func(name string, session ports.ProbeActivationHostSession, fingerprint string, err, want error) {
		t.Helper()
		if session != nil || fingerprint != "" || err != want {
			t.Fatalf("%s: %v %q %v, want nil, empty and %v", name, session, fingerprint, err, want)
		}
	}
	good := bytes.Clone(h.f.clientKey)
	pin := h.f.hostFP()

	var nilFactory *SSHProbeActivationSessionFactory
	session, fp, err := nilFactory.NewActivationSession(ctx, good, pin)
	requireRefused("nil factory", session, fp, err, domain.ErrValidation)
	var nilCtx *sendTNilContext
	session, fp, err = h.factory.NewActivationSession(nilCtx, good, pin)
	requireRefused("typed nil ctx", session, fp, err, domain.ErrValidation)

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	session, fp, err = h.factory.NewActivationSession(canceled, good, pin)
	requireRefused("canceled ctx", session, fp, err, context.Canceled)
	expired, stop := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer stop()
	session, fp, err = h.factory.NewActivationSession(expired, good, pin)
	requireRefused("expired ctx", session, fp, err, context.DeadlineExceeded)

	publicRaw, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPublic, err := ssh.NewPublicKey(publicRaw)
	if err != nil {
		t.Fatal(err)
	}
	_, secret, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKeyWithPassphrase(secret, "", []byte("passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	encrypted := pem.EncodeToMemory(block)

	for name, key := range map[string][]byte{
		"nil key":                nil,
		"empty key":              {},
		"text":                   []byte("not a private key"),
		"authorized_keys line":   ssh.MarshalAuthorizedKey(sshPublic),
		"pem of the wrong type":  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("x")}),
		"truncated pem":          good[:len(good)/2],
		"pem armor only":         []byte("-----BEGIN PRIVATE KEY-----\n-----END PRIVATE KEY-----\n"),
		"passphrase protected":   encrypted,
		"valid key, wrong armor": bytes.ReplaceAll(good, []byte("PRIVATE KEY"), []byte("PUBLIC KEY")),
	} {
		session, fp, err = h.factory.NewActivationSession(ctx, key, pin)
		requireRefused(name, session, fp, err, domain.ErrProbeSSHInput)
	}

	for name, badPin := range map[string]string{
		"empty pin":           "",
		"prefix only":         "SHA256:",
		"lowercase prefix":    "sha256:" + strings.TrimPrefix(pin, "SHA256:"),
		"no prefix":           strings.TrimPrefix(pin, "SHA256:"),
		"42 characters":       "SHA256:" + strings.Repeat("A", 42),
		"44 characters":       "SHA256:" + strings.Repeat("A", 44),
		"padded":              pin + "=",
		"leading space":       " " + pin,
		"trailing LF":         pin + "\n",
		"md5 style":           "MD5:" + strings.Repeat("aa:", 15) + "aa",
		"non-canonical bits":  "SHA256:" + strings.Repeat("A", 42) + "B",
		"standard base64 sym": "SHA256:" + strings.Repeat("+", 43),
	} {
		session, fp, err = h.factory.NewActivationSession(ctx, good, badPin)
		requireRefused("pin "+name, session, fp, err, domain.ErrProbeSSHInput)
	}

	if h.f.connCalls.Load() != 0 || h.f.authCalls.Load() != 0 || h.f.execCalls.Load() != 0 {
		t.Fatal("constructing a session performed SSH I/O")
	}
}

func TestNewActivationSessionReturnsThePublicFingerprintAndRetainsNoKeyBuffer(t *testing.T) {
	h := newActSessionHarness(t, nil)
	if h.clientFP != h.f.clientFP() || !strings.HasPrefix(h.clientFP, "SHA256:") {
		t.Fatalf("client fingerprint %q, want %q", h.clientFP, h.f.clientFP())
	}
	if h.f.connCalls.Load() != 0 {
		t.Fatalf("constructing a session connected %d times", h.f.connCalls.Load())
	}

	// The borrowed buffer is wiped by the caller right after construction; the
	// session must keep working from its own parsed copy.
	key := bytes.Clone(h.f.clientKey)
	session, fp, err := h.factory.NewActivationSession(t.Context(), key, h.f.hostFP())
	if err != nil || fp != h.f.clientFP() {
		t.Fatalf("NewActivationSession = %q %v", fp, err)
	}
	clear(key)
	h.session = session
	h.use(domain.ProbeActivationProbeInit)
	outcome, facts, token := h.probeInit(t, t.Context())
	actSessionRequireOutcome(t, outcome, actSessionApplied)
	if facts != probeInitTestResult().Facts || token != probeInitTestToken {
		t.Fatalf("facts %+v token %q after wiping the key buffer", facts, token)
	}
	if h.f.authCalls.Load() != 1 {
		t.Fatalf("authenticated %d times", h.f.authCalls.Load())
	}

	// Sessions are independent: a second construction yields its own session.
	other, otherFP, err := h.factory.NewActivationSession(t.Context(), bytes.Clone(h.f.clientKey), h.f.hostFP())
	if err != nil || other == session || otherFP != fp {
		t.Fatalf("second session %v %q %v", other, otherFP, err)
	}
}

// ---- ProbeInit --------------------------------------------------------------

func TestActivationSessionProbeInitAppliedReturnsFactsAndToken(t *testing.T) {
	h := newActSessionHarness(t, nil)
	intent := h.use(domain.ProbeActivationProbeInit)
	outcome, facts, token := h.probeInit(t, t.Context())
	actSessionRequireOutcome(t, outcome, actSessionApplied)
	if facts != probeInitTestResult().Facts || token != probeInitTestToken {
		t.Fatalf("facts %+v token %q", facts, token)
	}
	h.helper.waitIdle(t, h.f)
	if requests := h.auth.seenRequests(); len(requests) != 1 || requests[0] != h.wantRequest() || requests[0].Intent != intent {
		t.Fatalf("authorizer requests %+v", requests)
	}
	if want := h.wantStdin(t, h.genuineSignature()); !bytes.Equal(h.f.stdinBytes(), want) {
		t.Fatalf("server stdin = %d bytes, want %d", len(h.f.stdinBytes()), len(want))
	}
}

func TestActivationSessionProbeInitNotStarted(t *testing.T) {
	type tcase struct {
		name string
		tune func(helper *hostActSendHelper, auth *hostActSendAuthorizer)
	}
	cases := []tcase{
		{"authorizer error", func(_ *hostActSendHelper, auth *hostActSendAuthorizer) {
			auth.hook = func(context.Context, *hostActSendAuthorizer, domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
				return domain.ProbeHostActionGrant{}, errors.New("journal attempt already consumed")
			}
		}},
		{"authorizer returns a grant for another kind", func(_ *hostActSendHelper, auth *hostActSendAuthorizer) {
			auth.hook = func(_ context.Context, a *hostActSendAuthorizer, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
				request.Intent.Kind = domain.ProbeActivationServiceStart
				request.Challenge.MarkerSHA256 = request.Intent.MarkerSHA256()
				return hostActSendSign(a.authority, request), nil
			}
		}},
		{"zero grant", func(_ *hostActSendHelper, auth *hostActSendAuthorizer) {
			auth.hook = func(context.Context, *hostActSendAuthorizer, domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
				return domain.ProbeHostActionGrant{}, nil
			}
		}},
		{"helper exits without a challenge", func(helper *hostActSendHelper, _ *hostActSendAuthorizer) {
			helper.noChallenge = true
			helper.noChallengeStatus = 1
		}},
		{"malformed challenge", func(helper *hostActSendHelper, _ *hostActSendAuthorizer) {
			helper.challengeFn = func(*sendTFixture) []byte { return []byte("this is not a challenge line\n") }
		}},
		{"a probe_init receipt in place of the challenge", func(helper *hostActSendHelper, _ *hostActSendAuthorizer) {
			helper.challengeFn = func(f *sendTFixture) []byte {
				return EncodeProbeInitReceipt(hostActSendIntentFor(f, domain.ProbeActivationProbeInit), probeInitTestResult())
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newActSessionHarness(t, tc.tune)
			h.use(domain.ProbeActivationProbeInit)
			outcome, facts, token := h.probeInit(t, t.Context())
			actSessionRequireOutcome(t, outcome, actSessionNotStarted)
			actSessionRequireNoSecrets(t, facts, token)
			h.requireNotStartedEvidence(t)
		})
	}

	t.Run("cancel during authorization", func(t *testing.T) {
		h := newActSessionHarness(t, func(_ *hostActSendHelper, auth *hostActSendAuthorizer) { auth.hook = hostActSendBlockUntilDone })
		h.use(domain.ProbeActivationProbeInit)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		type out struct {
			outcome domain.ProbeInstallResourceOutcome
			facts   domain.ProbeActivationFacts
			token   string
		}
		done := make(chan out, 1)
		go func() {
			o, f, tok := h.session.ProbeInit(ctx, h.intent)
			done <- out{o, f, tok}
		}()
		select {
		case <-h.auth.entered:
		case <-time.After(10 * time.Second):
			t.Fatal("authorizer call not reached")
		}
		cancel()
		select {
		case r := <-done:
			actSessionRequireOutcome(t, r.outcome, actSessionNotStarted)
			actSessionRequireNoSecrets(t, r.facts, r.token)
		case <-time.After(30 * time.Second):
			t.Fatal("ProbeInit did not return after cancel")
		}
		h.requireNotStartedEvidence(t)
	})

	t.Run("canceled before the call", func(t *testing.T) {
		h := newActSessionHarness(t, nil)
		h.use(domain.ProbeActivationProbeInit)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		outcome, facts, token := h.probeInit(t, ctx)
		actSessionRequireOutcome(t, outcome, actSessionNotStarted)
		actSessionRequireNoSecrets(t, facts, token)
		h.requireNoIO(t)
	})

	t.Run("host key rejected before authentication", func(t *testing.T) {
		h := newActSessionHarness(t, nil)
		_, other := deploymentKey(t)
		wrongPin := ssh.FingerprintSHA256(other.PublicKey())
		h.session, _ = h.newSessionFrom(t, h.factory, wrongPin)
		h.use(domain.ProbeActivationProbeInit)
		h.intent.Identity.HostKeyFingerprint = wrongPin
		outcome, facts, token := h.probeInit(t, t.Context())
		actSessionRequireOutcome(t, outcome, actSessionNotStarted)
		actSessionRequireNoSecrets(t, facts, token)
		if h.f.authCalls.Load() != 0 {
			t.Fatalf("authenticated before host trust: %d", h.f.authCalls.Load())
		}
		h.requireNoExec(t)
	})

	t.Run("a host key that differs from the intent's pin is refused before I/O", func(t *testing.T) {
		h := newActSessionHarness(t, nil)
		h.use(domain.ProbeActivationProbeInit)
		h.intent.Identity.HostKeyFingerprint = hostActFrameFingerprint(9)
		outcome, facts, token := h.probeInit(t, t.Context())
		actSessionRequireOutcome(t, outcome, actSessionNotStarted)
		actSessionRequireNoSecrets(t, facts, token)
		h.requireNoIO(t)
	})

	t.Run("destination rejected by policy", func(t *testing.T) {
		h := newActSessionHarness(t, nil)
		factory, err := NewSSHProbeActivationSessionFactory(probe.EndpointPolicy{}, h.auth)
		if err != nil {
			t.Fatal(err)
		}
		h.session, _ = h.newSessionFrom(t, factory, h.f.hostFP())
		h.use(domain.ProbeActivationProbeInit)
		outcome, facts, token := h.probeInit(t, t.Context())
		actSessionRequireOutcome(t, outcome, actSessionNotStarted)
		actSessionRequireNoSecrets(t, facts, token)
		h.requireNoIO(t)
	})

	t.Run("connect failure", func(t *testing.T) {
		h := newActSessionHarness(t, nil)
		h.use(domain.ProbeActivationProbeInit)
		h.intent.Identity.Host.Address = authSendDeadAddress(t)
		outcome, facts, token := h.probeInit(t, t.Context())
		actSessionRequireOutcome(t, outcome, actSessionNotStarted)
		actSessionRequireNoSecrets(t, facts, token)
		h.requireNoIO(t)
	})

	t.Run("intents that are not probe_init", func(t *testing.T) {
		h := newActSessionHarness(t, nil)
		for _, kind := range []string{
			hostActFrameKindData, hostActFrameKindUnit, hostActFrameKindService, domain.ProbeActivationServiceStart,
			hostActFrameKindArtifactDir, domain.ProbeActivationEnrollment, "",
		} {
			h.use(kind)
			outcome, facts, token := h.probeInit(t, t.Context())
			actSessionRequireOutcome(t, outcome, actSessionNotStarted)
			actSessionRequireNoSecrets(t, facts, token)
		}
		h.requireNoIO(t)
	})

	t.Run("invalid intents", func(t *testing.T) {
		h := newActSessionHarness(t, nil)
		for name, mutate := range map[string]func(*domain.ProbeHostActionIntent){
			"zero":              func(i *domain.ProbeHostActionIntent) { *i = domain.ProbeHostActionIntent{Kind: i.Kind} },
			"docker mode":       func(i *domain.ProbeHostActionIntent) { i.Identity.Mode = domain.ProbeInstallDocker },
			"non-root":          func(i *domain.ProbeHostActionIntent) { i.Identity.Host.User = "deploy" },
			"empty owner":       func(i *domain.ProbeHostActionIntent) { i.OwnerID = "" },
			"malformed attempt": func(i *domain.ProbeHostActionIntent) { i.AttemptID = "not-an-id" },
			"invalid pin":       func(i *domain.ProbeHostActionIntent) { i.Identity.Binary.Size = 0 },
		} {
			h.use(domain.ProbeActivationProbeInit)
			mutate(&h.intent)
			outcome, facts, token := h.probeInit(t, t.Context())
			if outcome != actSessionNotStarted {
				t.Fatalf("%s: %+v", name, outcome)
			}
			actSessionRequireNoSecrets(t, facts, token)
		}
		h.requireNoIO(t)
	})
}

func TestActivationSessionProbeInitUnknown(t *testing.T) {
	forger := newAuthSendAuthority(t)
	otherKindReceipt := func(kind string) func(*hostActSendHelper, *hostActSendAuthorizer) {
		return func(helper *hostActSendHelper, _ *hostActSendAuthorizer) {
			helper.tail = func(_ *sendTFixture, intent domain.ProbeHostActionIntent, _ []byte) string {
				intent.Kind = kind
				return string(EncodeProbeHostActionReceipt(intent))
			}
		}
	}
	tailOf := func(fn func(intent domain.ProbeHostActionIntent) string) func(*hostActSendHelper, *hostActSendAuthorizer) {
		return func(helper *hostActSendHelper, _ *hostActSendAuthorizer) {
			helper.tail = func(_ *sendTFixture, intent domain.ProbeHostActionIntent, _ []byte) string { return fn(intent) }
		}
	}
	good := func(intent domain.ProbeHostActionIntent) string {
		return string(EncodeProbeInitReceipt(intent, probeInitTestResult()))
	}
	cases := []struct {
		name      string
		tune      func(helper *hostActSendHelper, auth *hostActSendAuthorizer)
		forgedSig bool
	}{
		{name: "forged grant rejected by the helper", forgedSig: true, tune: func(_ *hostActSendHelper, auth *hostActSendAuthorizer) {
			auth.hook = func(_ context.Context, _ *hostActSendAuthorizer, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
				return hostActSendSign(forger, request), nil
			}
		}},
		{name: "plain host-action receipt", tune: otherKindReceipt(domain.ProbeActivationProbeInit)},
		{name: "receipt of the service_start action", tune: otherKindReceipt(domain.ProbeActivationServiceStart)},
		{name: "invalid token in an otherwise genuine receipt", tune: tailOf(func(i domain.ProbeHostActionIntent) string {
			return probeInitTestWire(i.MarkerSHA256(), probeInitTestProbeID, probeInitTestStreamID, probeInitTestFingerprint, probeInitTestPrefix+"short")
		})},
		{name: "local probe id", tune: tailOf(func(i domain.ProbeHostActionIntent) string {
			return probeInitTestWire(i.MarkerSHA256(), "local", probeInitTestStreamID, probeInitTestFingerprint, probeInitTestToken)
		})},
		{name: "receipt with an extra byte", tune: tailOf(func(i domain.ProbeHostActionIntent) string { return good(i) + "x" })},
		{name: "receipt missing its LF", tune: tailOf(func(i domain.ProbeHostActionIntent) string { return strings.TrimSuffix(good(i), "\n") })},
		{name: "receipt for another attempt", tune: tailOf(func(i domain.ProbeHostActionIntent) string {
			i.AttemptID = hostActSendOtherUUID
			return good(i)
		})},
		{name: "no receipt", tune: tailOf(func(domain.ProbeHostActionIntent) string { return "" })},
		{name: "stdout bound exceeded", tune: tailOf(func(domain.ProbeHostActionIntent) string {
			return strings.Repeat("x", MaxSSHBinaryStageReceiptSize+1)
		})},
		{name: "nonzero exit with a valid receipt", tune: func(helper *hostActSendHelper, _ *hostActSendAuthorizer) { helper.exit = sendTExit() }},
		{name: "signal exit with a valid receipt", tune: func(helper *hostActSendHelper, _ *hostActSendAuthorizer) { helper.signal = "TERM" }},
		{name: "missing exit status", tune: func(helper *hostActSendHelper, _ *hostActSendAuthorizer) { helper.noExit = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newActSessionHarness(t, tc.tune)
			h.use(domain.ProbeActivationProbeInit)
			outcome, facts, token := h.probeInit(t, t.Context())
			actSessionRequireOutcome(t, outcome, actSessionUnknown)
			actSessionRequireNoSecrets(t, facts, token)
			signature := h.genuineSignature()
			if tc.forgedSig {
				signature = ed25519.Sign(forger.priv, h.wantRequest().SigningMessage())
			}
			h.requireUnknownEvidence(t, h.wantStdin(t, signature))
		})
	}

	t.Run("cancel after stdin", func(t *testing.T) {
		release := make(chan struct{})
		var releaseOnce sync.Once
		releaseHelper := func() { releaseOnce.Do(func() { close(release) }) }
		h := newActSessionHarness(t, func(helper *hostActSendHelper, _ *hostActSendAuthorizer) {
			helper.tail = func(_ *sendTFixture, intent domain.ProbeHostActionIntent, _ []byte) string {
				<-release
				return string(EncodeProbeInitReceipt(intent, probeInitTestResult()))
			}
		})
		t.Cleanup(releaseHelper)
		h.use(domain.ProbeActivationProbeInit)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		type out struct {
			outcome domain.ProbeInstallResourceOutcome
			facts   domain.ProbeActivationFacts
			token   string
		}
		done := make(chan out, 1)
		go func() {
			o, f, tok := h.session.ProbeInit(ctx, h.intent)
			done <- out{o, f, tok}
		}()
		hostActSendWaitEOF(t, h.f)
		cancel()
		select {
		case r := <-done:
			actSessionRequireOutcome(t, r.outcome, actSessionUnknown)
			actSessionRequireNoSecrets(t, r.facts, r.token)
		case <-time.After(30 * time.Second):
			t.Fatal("ProbeInit did not return after cancel")
		}
		releaseHelper()
		h.requireUnknownEvidence(t, h.wantStdin(t, h.genuineSignature()))
	})
}

// ---- ServiceStart -----------------------------------------------------------

func TestActivationSessionServiceStartApplied(t *testing.T) {
	h := newActSessionHarness(t, nil)
	intent := h.use(domain.ProbeActivationServiceStart)
	actSessionRequireOutcome(t, h.serviceStart(t, t.Context()), actSessionApplied)
	h.helper.waitIdle(t, h.f)
	if requests := h.auth.seenRequests(); len(requests) != 1 || requests[0] != h.wantRequest() || requests[0].Intent != intent {
		t.Fatalf("authorizer requests %+v", requests)
	}
	if want := h.wantStdin(t, h.genuineSignature()); !bytes.Equal(h.f.stdinBytes(), want) {
		t.Fatalf("server stdin = %d bytes, want %d", len(h.f.stdinBytes()), len(want))
	}
}

func TestActivationSessionServiceStartNotStarted(t *testing.T) {
	cases := []struct {
		name string
		tune func(helper *hostActSendHelper, auth *hostActSendAuthorizer)
	}{
		{"authorizer error", func(_ *hostActSendHelper, auth *hostActSendAuthorizer) {
			auth.hook = func(context.Context, *hostActSendAuthorizer, domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
				return domain.ProbeHostActionGrant{}, errors.New("step not in flight")
			}
		}},
		{"authorizer returns a grant for probe_init", func(_ *hostActSendHelper, auth *hostActSendAuthorizer) {
			auth.hook = func(_ context.Context, a *hostActSendAuthorizer, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
				request.Intent.Kind = domain.ProbeActivationProbeInit
				request.Challenge.MarkerSHA256 = request.Intent.MarkerSHA256()
				return hostActSendSign(a.authority, request), nil
			}
		}},
		{"helper exits without a challenge", func(helper *hostActSendHelper, _ *hostActSendAuthorizer) {
			helper.noChallenge = true
			helper.noChallengeStatus = 1
		}},
		{"malformed challenge", func(helper *hostActSendHelper, _ *hostActSendAuthorizer) {
			helper.challengeFn = func(*sendTFixture) []byte { return []byte("not a challenge\n") }
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newActSessionHarness(t, tc.tune)
			h.use(domain.ProbeActivationServiceStart)
			actSessionRequireOutcome(t, h.serviceStart(t, t.Context()), actSessionNotStarted)
			h.requireNotStartedEvidence(t)
		})
	}

	t.Run("a probe_init intent is refused before any I/O", func(t *testing.T) {
		h := newActSessionHarness(t, nil)
		h.use(domain.ProbeActivationProbeInit)
		actSessionRequireOutcome(t, h.serviceStart(t, t.Context()), actSessionNotStarted)
		h.requireNoIO(t)
	})
	t.Run("invalid intents are refused before any I/O", func(t *testing.T) {
		h := newActSessionHarness(t, nil)
		for name, mutate := range map[string]func(*domain.ProbeHostActionIntent){
			"zero":              func(i *domain.ProbeHostActionIntent) { *i = domain.ProbeHostActionIntent{Kind: i.Kind} },
			"docker mode":       func(i *domain.ProbeHostActionIntent) { i.Identity.Mode = domain.ProbeInstallDocker },
			"non-root":          func(i *domain.ProbeHostActionIntent) { i.Identity.Host.User = "deploy" },
			"malformed attempt": func(i *domain.ProbeHostActionIntent) { i.AttemptID = "not-an-id" },
		} {
			h.use(domain.ProbeActivationServiceStart)
			mutate(&h.intent)
			if outcome := h.serviceStart(t, t.Context()); outcome != actSessionNotStarted {
				t.Fatalf("%s: %+v", name, outcome)
			}
		}
		h.requireNoIO(t)
	})
	t.Run("canceled before the call", func(t *testing.T) {
		h := newActSessionHarness(t, nil)
		h.use(domain.ProbeActivationServiceStart)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		actSessionRequireOutcome(t, h.serviceStart(t, ctx), actSessionNotStarted)
		h.requireNoIO(t)
	})
	t.Run("host key rejected before authentication", func(t *testing.T) {
		h := newActSessionHarness(t, nil)
		_, other := deploymentKey(t)
		wrongPin := ssh.FingerprintSHA256(other.PublicKey())
		h.session, _ = h.newSessionFrom(t, h.factory, wrongPin)
		h.use(domain.ProbeActivationServiceStart)
		h.intent.Identity.HostKeyFingerprint = wrongPin
		actSessionRequireOutcome(t, h.serviceStart(t, t.Context()), actSessionNotStarted)
		if h.f.authCalls.Load() != 0 {
			t.Fatalf("authenticated before host trust: %d", h.f.authCalls.Load())
		}
		h.requireNoExec(t)
	})
	t.Run("connect failure", func(t *testing.T) {
		h := newActSessionHarness(t, nil)
		h.use(domain.ProbeActivationServiceStart)
		h.intent.Identity.Host.Address = authSendDeadAddress(t)
		actSessionRequireOutcome(t, h.serviceStart(t, t.Context()), actSessionNotStarted)
		h.requireNoIO(t)
	})
	t.Run("cancel during authorization", func(t *testing.T) {
		h := newActSessionHarness(t, func(_ *hostActSendHelper, auth *hostActSendAuthorizer) { auth.hook = hostActSendBlockUntilDone })
		h.use(domain.ProbeActivationServiceStart)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan domain.ProbeInstallResourceOutcome, 1)
		go func() { done <- h.session.ServiceStart(ctx, h.intent) }()
		select {
		case <-h.auth.entered:
		case <-time.After(10 * time.Second):
			t.Fatal("authorizer call not reached")
		}
		cancel()
		select {
		case outcome := <-done:
			actSessionRequireOutcome(t, outcome, actSessionNotStarted)
		case <-time.After(30 * time.Second):
			t.Fatal("ServiceStart did not return after cancel")
		}
		h.requireNotStartedEvidence(t)
	})
}

func TestActivationSessionServiceStartUnknown(t *testing.T) {
	forger := newAuthSendAuthority(t)
	tailOf := func(fn func(intent domain.ProbeHostActionIntent) string) func(*hostActSendHelper, *hostActSendAuthorizer) {
		return func(helper *hostActSendHelper, _ *hostActSendAuthorizer) {
			helper.tail = func(_ *sendTFixture, intent domain.ProbeHostActionIntent, _ []byte) string { return fn(intent) }
		}
	}
	cases := []struct {
		name      string
		tune      func(helper *hostActSendHelper, auth *hostActSendAuthorizer)
		forgedSig bool
	}{
		{name: "forged grant rejected by the helper", forgedSig: true, tune: func(_ *hostActSendHelper, auth *hostActSendAuthorizer) {
			auth.hook = func(_ context.Context, _ *hostActSendAuthorizer, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
				return hostActSendSign(forger, request), nil
			}
		}},
		{name: "a probe_init receipt carrying facts and a token", tune: tailOf(func(i domain.ProbeHostActionIntent) string {
			i.Kind = domain.ProbeActivationProbeInit
			return string(EncodeProbeInitReceipt(i, probeInitTestResult()))
		})},
		{name: "receipt of the service action", tune: tailOf(func(i domain.ProbeHostActionIntent) string {
			i.Kind = hostActFrameKindService
			return string(EncodeProbeHostActionReceipt(i))
		})},
		{name: "receipt of another operation", tune: tailOf(func(i domain.ProbeHostActionIntent) string {
			i.Identity.OperationID = hostActSendOtherUUID
			return string(EncodeProbeHostActionReceipt(i))
		})},
		{name: "receipt with an extra byte", tune: tailOf(func(i domain.ProbeHostActionIntent) string {
			return string(EncodeProbeHostActionReceipt(i)) + "x"
		})},
		{name: "no receipt", tune: tailOf(func(domain.ProbeHostActionIntent) string { return "" })},
		{name: "nonzero exit with a valid receipt", tune: func(helper *hostActSendHelper, _ *hostActSendAuthorizer) { helper.exit = sendTExit() }},
		{name: "signal exit with a valid receipt", tune: func(helper *hostActSendHelper, _ *hostActSendAuthorizer) { helper.signal = "TERM" }},
		{name: "missing exit status", tune: func(helper *hostActSendHelper, _ *hostActSendAuthorizer) { helper.noExit = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newActSessionHarness(t, tc.tune)
			h.use(domain.ProbeActivationServiceStart)
			actSessionRequireOutcome(t, h.serviceStart(t, t.Context()), actSessionUnknown)
			signature := h.genuineSignature()
			if tc.forgedSig {
				signature = ed25519.Sign(forger.priv, h.wantRequest().SigningMessage())
			}
			h.requireUnknownEvidence(t, h.wantStdin(t, signature))
		})
	}

	t.Run("cancel after stdin", func(t *testing.T) {
		release := make(chan struct{})
		var releaseOnce sync.Once
		releaseHelper := func() { releaseOnce.Do(func() { close(release) }) }
		h := newActSessionHarness(t, func(helper *hostActSendHelper, _ *hostActSendAuthorizer) {
			helper.tail = func(_ *sendTFixture, intent domain.ProbeHostActionIntent, _ []byte) string {
				<-release
				return string(EncodeProbeHostActionReceipt(intent))
			}
		})
		t.Cleanup(releaseHelper)
		h.use(domain.ProbeActivationServiceStart)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan domain.ProbeInstallResourceOutcome, 1)
		go func() { done <- h.session.ServiceStart(ctx, h.intent) }()
		hostActSendWaitEOF(t, h.f)
		cancel()
		select {
		case outcome := <-done:
			actSessionRequireOutcome(t, outcome, actSessionUnknown)
		case <-time.After(30 * time.Second):
			t.Fatal("ServiceStart did not return after cancel")
		}
		releaseHelper()
		h.requireUnknownEvidence(t, h.wantStdin(t, h.genuineSignature()))
	})
}

// ServiceStart pins its kind: any other host-action intent is refused as
// not_started before any authorization or connection.
func TestActivationSessionServiceStartRefusesOtherKinds(t *testing.T) {
	for _, kind := range []string{hostActFrameKindData, hostActFrameKindUnit, hostActFrameKindService} {
		t.Run(kind, func(t *testing.T) {
			h := newActSessionHarness(t, nil)
			h.use(kind)
			outcome := h.serviceStart(t, t.Context())
			if outcome.State != domain.ProbeInstallResourceNotStarted {
				t.Fatalf("outcome %+v", outcome)
			}
			if requests := h.auth.seenRequests(); len(requests) != 0 {
				t.Fatalf("authorizer reached: %+v", requests)
			}
		})
	}
}

// ---- lifecycle and mapping --------------------------------------------------

func TestActivationSessionRunsInitThenStartOnOneKey(t *testing.T) {
	h := newActSessionHarness(t, nil)
	ctx := t.Context()

	initIntent := h.use(domain.ProbeActivationProbeInit)
	initRequest, initSignature := h.wantRequest(), h.genuineSignature()
	initStdin := h.wantStdin(t, initSignature)
	outcome, facts, token := h.session.ProbeInit(ctx, initIntent)
	actSessionRequireOutcome(t, outcome, actSessionApplied)
	if facts != probeInitTestResult().Facts || token != probeInitTestToken {
		t.Fatalf("init returned %+v %q", facts, token)
	}

	startIntent := h.use(domain.ProbeActivationServiceStart)
	startRequest := h.wantRequest()
	startStdin := h.wantStdin(t, h.genuineSignature())
	actSessionRequireOutcome(t, h.session.ServiceStart(ctx, startIntent), actSessionApplied)

	h.helper.waitIdle(t, h.f)
	if got := h.f.connCalls.Load(); got != 2 {
		t.Fatalf("connections = %d, want one per step", got)
	}
	if want := slices.Concat(initStdin, startStdin); !bytes.Equal(h.f.stdinBytes(), want) {
		t.Fatalf("stdin across steps = %d bytes, want %d", len(h.f.stdinBytes()), len(want))
	}
	requests := h.auth.seenRequests()
	if len(requests) != 2 || requests[0] != initRequest || requests[1] != startRequest ||
		requests[0].Intent.Kind != "probe_init" || requests[1].Intent.Kind != "service_start" {
		t.Fatalf("authorizer requests %+v", requests)
	}
	if got := h.f.execCommands(); !slices.Equal(got, []string{hostActSendCommandLiteral, hostActSendCommandLiteral}) {
		t.Fatalf("exec commands %v", got)
	}
}

func TestActivationSessionHasNoStickyFailureState(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	h := newActSessionHarness(t, func(_ *hostActSendHelper, auth *hostActSendAuthorizer) {
		auth.hook = func(_ context.Context, a *hostActSendAuthorizer, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n == 1 {
				return domain.ProbeHostActionGrant{}, errors.New("transient authorization failure")
			}
			return hostActSendSign(a.authority, request), nil
		}
	})
	intent := h.use(domain.ProbeActivationProbeInit)
	outcome, facts, token := h.session.ProbeInit(t.Context(), intent)
	actSessionRequireOutcome(t, outcome, actSessionNotStarted)
	actSessionRequireNoSecrets(t, facts, token)
	outcome, facts, token = h.session.ProbeInit(t.Context(), intent)
	actSessionRequireOutcome(t, outcome, actSessionApplied)
	if facts != probeInitTestResult().Facts || token != probeInitTestToken {
		t.Fatalf("second init returned %+v %q", facts, token)
	}
}

func TestActivationSessionIsSafeForConcurrentUse(t *testing.T) {
	h := newActSessionHarness(t, nil)
	intent := h.use(domain.ProbeActivationProbeInit)
	const workers = 4
	var wg sync.WaitGroup
	results := make([]struct {
		outcome domain.ProbeInstallResourceOutcome
		facts   domain.ProbeActivationFacts
		token   string
	}, workers)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i].outcome, results[i].facts, results[i].token = h.session.ProbeInit(t.Context(), intent)
		}()
	}
	wg.Wait()
	for i, r := range results {
		if r.outcome != actSessionApplied || r.facts != probeInitTestResult().Facts || r.token != probeInitTestToken {
			t.Fatalf("worker %d: %+v %+v %q", i, r.outcome, r.facts, r.token)
		}
	}
	if h.auth.calls.Load() != workers || h.f.connCalls.Load() != workers {
		t.Fatalf("authorizer calls %d, connections %d, want %d each", h.auth.calls.Load(), h.f.connCalls.Load(), workers)
	}
}

func TestProbeActivationSendOutcomeMapping(t *testing.T) {
	unknown := map[string]error{
		"sentinel":           ErrSSHBinaryStageUnknown,
		"wrapped":            fmt.Errorf("sending: %w", ErrSSHBinaryStageUnknown),
		"joined with others": errors.Join(errors.New("other"), ErrSSHBinaryStageUnknown),
	}
	for name, err := range unknown {
		if got := probeActivationSendOutcome(err); got != actSessionUnknown || !got.Valid() {
			t.Fatalf("%s: %+v, want unknown", name, got)
		}
	}
	notStarted := map[string]error{
		"not staged":           ErrSSHBinaryStageNotStaged,
		"input":                ErrSSHBinaryStageInput,
		"context canceled":     context.Canceled,
		"deadline exceeded":    context.DeadlineExceeded,
		"ssh trust":            domain.ErrProbeSSHTrust,
		"ssh connect":          domain.ErrProbeSSHConnect,
		"ssh destination":      domain.ErrProbeSSHDestination,
		"ssh input":            domain.ErrProbeSSHInput,
		"artifact storage":     domain.ErrProbeArtifactStorage,
		"write conflict":       domain.ErrProbeBinaryStageWriteConflict,
		"write unknown":        domain.ErrProbeBinaryStageWriteUnknown,
		"validation":           domain.ErrValidation,
		"unrelated":            errors.New("anything else"),
		"wrapped not staged":   fmt.Errorf("sending: %w", ErrSSHBinaryStageNotStaged),
		"wrapped cancellation": fmt.Errorf("sending: %w", context.Canceled),
	}
	for name, err := range notStarted {
		if got := probeActivationSendOutcome(err); got != actSessionNotStarted || !got.Valid() {
			t.Fatalf("%s: %+v, want not_started", name, got)
		}
	}
}
