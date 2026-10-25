package provisioning

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// These tests drive SendProbeInit, SendHostAction for service_start and the
// refactored sendHostAction through the real loopback SSH protocol fixture and
// the host-action helper emulation of ssh_host_action_send_test.go. The remote
// side is an emulation: challenge line, stdin to EOF, a parse of the action and
// grant frames with the production decoders, an Ed25519 grant check, then the
// configured receipt and exit status. It is protocol evidence only: no OpenSSH,
// no installed helper, no systemd and no real probe binary, and every key and
// token is generated per test.

// probeInitSendHarness is the host-action harness for the probe_init kind whose
// helper answers with the canonical probe_init receipt for result.
type probeInitSendHarness struct {
	*hostActSendHarness
	result ProbeInitResult
}

type probeInitSendOutcome struct {
	result ProbeInitResult
	err    error
}

func newProbeInitSendHarness(t *testing.T, tune func(helper *hostActSendHelper, auth *hostActSendAuthorizer)) *probeInitSendHarness {
	t.Helper()
	h := &probeInitSendHarness{result: probeInitTestResult()}
	h.hostActSendHarness = newHostActSendHarness(t, domain.ProbeActivationProbeInit, func(helper *hostActSendHelper, auth *hostActSendAuthorizer) {
		helper.tail = func(_ *sendTFixture, intent domain.ProbeHostActionIntent, _ []byte) string {
			return string(EncodeProbeInitReceipt(intent, h.result))
		}
		if tune != nil {
			tune(helper, auth)
		}
	})
	return h
}

func (h *probeInitSendHarness) initAsync(ctx context.Context) <-chan probeInitSendOutcome {
	done := make(chan probeInitSendOutcome, 1)
	go func() {
		result, err := h.sender.SendProbeInit(ctx, h.intent, h.auth)
		done <- probeInitSendOutcome{result: result, err: err}
	}()
	return done
}

func probeInitSendAwait(t *testing.T, done <-chan probeInitSendOutcome, what string) probeInitSendOutcome {
	t.Helper()
	select {
	case out := <-done:
		return out
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not return", what)
		return probeInitSendOutcome{}
	}
}

func (h *probeInitSendHarness) sendInit(t *testing.T) probeInitSendOutcome {
	t.Helper()
	return probeInitSendAwait(t, h.initAsync(t.Context()), "probe init send")
}

func (h *probeInitSendHarness) requireInitNotStaged(t *testing.T, out probeInitSendOutcome) {
	t.Helper()
	if out.result != (ProbeInitResult{}) {
		t.Fatalf("a failed send returned %+v", out.result)
	}
	h.requireNotStaged(t, hostActSendResult{err: out.err})
}

func (h *probeInitSendHarness) requireInitUnknown(t *testing.T, out probeInitSendOutcome, wantStdin []byte) {
	t.Helper()
	if out.result != (ProbeInitResult{}) {
		t.Fatalf("a failed send returned %+v, so the token would escape", out.result)
	}
	h.requireUnknown(t, hostActSendResult{err: out.err}, wantStdin)
}

// requireNoInitMaterial asserts an error carries none of the init material.
func requireNoInitMaterial(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	text := fmt.Sprintf("%v|%+v|%#v|%s", err, err, err, err.Error())
	for _, secret := range []string{probeInitTestToken, probeInitTestProbeID, probeInitTestStreamID, probeInitTestFingerprint, probeInitTestPrefix} {
		if strings.Contains(text, secret) {
			t.Fatalf("error %q carries init material %q", text, secret)
		}
	}
}

func TestSendProbeInitHappyPath(t *testing.T) {
	h := newProbeInitSendHarness(t, nil)
	out := h.sendInit(t)
	if out.err != nil {
		t.Fatal(out.err)
	}
	if out.result != probeInitTestResult() || !out.result.Valid() {
		t.Fatalf("result %+v, want %+v", out.result, probeInitTestResult())
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
	// Exactly one authorization, for the request rebuilt from the helper's own
	// challenge and the probe_init intent, before any stdin byte existed.
	if h.auth.calls.Load() != 1 {
		t.Fatalf("authorizer calls = %d, want 1", h.auth.calls.Load())
	}
	if requests := h.auth.seenRequests(); len(requests) != 1 || requests[0] != h.wantRequest() || requests[0].Intent.Kind != "probe_init" {
		t.Fatalf("authorizer request %+v, want %+v", requests, h.wantRequest())
	}
	h.auth.mu.Lock()
	stdinAtCall := slices.Clone(h.auth.stdinAtCall)
	h.auth.mu.Unlock()
	if !slices.Equal(stdinAtCall, []int{0}) {
		t.Fatalf("stdin bytes visible when authorizer ran: %v, want [0]", stdinAtCall)
	}

	// Stdin is exactly the probe_init action frame and the grant frame, then EOF.
	want := h.wantStdin(t, h.genuineSignature())
	got := h.f.stdinBytes()
	if !bytes.Equal(got, want) {
		t.Fatalf("server stdin = %d bytes, want %d", len(got), len(want))
	}
	if !bytes.HasPrefix(got, []byte(hostActFrameMagicLiteral+string(rune(len("probe_init")))+"probe_init")) {
		t.Fatal("stdin does not start with the probe_init action prefix")
	}
	if !h.f.stdinEOFSeen() {
		t.Fatal("missing stdin EOF")
	}
	// The token is only ever on the way back: nothing sent contains it.
	if bytes.Contains(got, []byte(probeInitTestToken)) || strings.Contains(fmt.Sprintf("%+v", h.auth.seenRequests()), probeInitTestToken) {
		t.Fatal("the enrollment token traveled toward the host")
	}
	if h.helper.verified.Load() != 1 {
		t.Fatalf("helper emulation verdict %d, want accepted grant", h.helper.verified.Load())
	}
	if parsed := h.helper.parsedIntents(); len(parsed) != 1 || parsed[0] != h.intent {
		t.Fatalf("helper decoded %+v, want the intent", parsed)
	}
}

func TestSendProbeInitChallengeSplitAcrossWritesStillWorks(t *testing.T) {
	h := newProbeInitSendHarness(t, func(helper *hostActSendHelper, _ *hostActSendAuthorizer) { helper.split = true })
	out := h.sendInit(t)
	if out.err != nil || out.result != probeInitTestResult() {
		t.Fatalf("split challenge: %+v %v", out.result, out.err)
	}
}

func TestSendProbeInitSequentialCallsAreIndependent(t *testing.T) {
	var mu sync.Mutex
	served := 0
	h := newProbeInitSendHarness(t, func(helper *hostActSendHelper, _ *hostActSendAuthorizer) {
		helper.kind = ""
		helper.tail = func(_ *sendTFixture, intent domain.ProbeHostActionIntent, _ []byte) string {
			mu.Lock()
			served++
			seed := byte(served)
			mu.Unlock()
			result := probeInitTestResult()
			result.EnrollmentToken = probeInitTestMakeToken(seed)
			return string(EncodeProbeInitReceipt(intent, result))
		}
	})
	var wantStdin []byte
	for i := 1; i <= 3; i++ {
		result, err := h.sender.SendProbeInit(t.Context(), h.intent, h.auth)
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		want := probeInitTestResult()
		want.EnrollmentToken = probeInitTestMakeToken(byte(i))
		if result != want {
			t.Fatalf("send %d returned %+v, want its own receipt %+v", i, result, want)
		}
		wantStdin = append(wantStdin, h.wantStdin(t, h.genuineSignature())...)
	}
	h.helper.waitIdle(t, h.f)
	if got := h.f.connCalls.Load(); got != 3 {
		t.Fatalf("connections = %d, want one per send", got)
	}
	if h.auth.calls.Load() != 3 {
		t.Fatalf("authorizer calls = %d, want one per send", h.auth.calls.Load())
	}
	if got := h.f.stdinBytes(); !bytes.Equal(got, wantStdin) {
		t.Fatalf("stdin across sends = %d bytes, want %d", len(got), len(wantStdin))
	}
}

// Everything the helper can say wrongly after the grant was delivered is
// Unknown: the init may have run and created identity material.
func TestSendProbeInitWrongOrForgedReceiptsAreUnknown(t *testing.T) {
	type tcase struct {
		name string
		tune func(helper *hostActSendHelper)
	}
	tailOf := func(fn func(intent domain.ProbeHostActionIntent, challenge []byte) string) func(*hostActSendHelper) {
		return func(helper *hostActSendHelper) {
			helper.tail = func(_ *sendTFixture, intent domain.ProbeHostActionIntent, challenge []byte) string {
				return fn(intent, challenge)
			}
		}
	}
	good := func(intent domain.ProbeHostActionIntent) string {
		return string(EncodeProbeInitReceipt(intent, probeInitTestResult()))
	}
	wire := func(intent domain.ProbeHostActionIntent, mutate func(*ProbeInitResult)) string {
		r := probeInitTestResult()
		mutate(&r)
		return probeInitTestWire(intent.MarkerSHA256(), r.Facts.ProbeID, r.Facts.StreamID, r.Facts.CertificateFingerprint, r.EnrollmentToken)
	}
	with := func(fn func(intent domain.ProbeHostActionIntent) string) func(*hostActSendHelper) {
		return tailOf(func(intent domain.ProbeHostActionIntent, _ []byte) string { return fn(intent) })
	}
	otherIntent := func(intent domain.ProbeHostActionIntent, mutate func(*domain.ProbeHostActionIntent)) domain.ProbeHostActionIntent {
		mutate(&intent)
		return intent
	}

	cases := []tcase{
		// Wrong receipt kind or binding.
		{"plain host-action receipt", with(func(i domain.ProbeHostActionIntent) string { return string(EncodeProbeHostActionReceipt(i)) })},
		{"receipt of another operation", with(func(i domain.ProbeHostActionIntent) string {
			return good(otherIntent(i, func(o *domain.ProbeHostActionIntent) { o.Identity.OperationID = hostActSendOtherUUID }))
		})},
		{"receipt of another attempt", with(func(i domain.ProbeHostActionIntent) string {
			return good(otherIntent(i, func(o *domain.ProbeHostActionIntent) { o.AttemptID = hostActSendOtherUUID }))
		})},
		{"receipt of another owner", with(func(i domain.ProbeHostActionIntent) string {
			return good(otherIntent(i, func(o *domain.ProbeHostActionIntent) { o.OwnerID = hostActSendOtherUUID }))
		})},
		{"marker of the data_directory action", with(func(i domain.ProbeHostActionIntent) string {
			other := otherIntent(i, func(o *domain.ProbeHostActionIntent) { o.Kind = hostActFrameKindData })
			return strings.Replace(good(i), i.MarkerSHA256(), other.MarkerSHA256(), 1)
		})},
		{"marker of the service_start action", with(func(i domain.ProbeHostActionIntent) string {
			other := otherIntent(i, func(o *domain.ProbeHostActionIntent) { o.Kind = domain.ProbeActivationServiceStart })
			return strings.Replace(good(i), i.MarkerSHA256(), other.MarkerSHA256(), 1)
		})},
		{"kind service_start in the receipt", with(func(i domain.ProbeHostActionIntent) string {
			return strings.Replace(good(i), `"kind":"probe_init"`, `"kind":"service_start"`, 1)
		})},
		{"state host_action_applied", with(func(i domain.ProbeHostActionIntent) string {
			return strings.Replace(good(i), "probe_initialized", "host_action_applied", 1)
		})},
		{"a binary-stage receipt", func(helper *hostActSendHelper) {
			helper.tail = func(f *sendTFixture, _ domain.ProbeHostActionIntent, _ []byte) string {
				return sendTReceipt(f.intent(sendTPin([]byte(hostActSendContent))))
			}
		}},
		{"uppercase marker", with(func(i domain.ProbeHostActionIntent) string {
			return strings.Replace(good(i), i.MarkerSHA256(), strings.ToUpper(i.MarkerSHA256()), 1)
		})},
		{"forged marker", with(func(i domain.ProbeHostActionIntent) string {
			return strings.Replace(good(i), i.MarkerSHA256(), strings.Repeat("ab", 32), 1)
		})},

		// Invalid facts or token the helper must never be believed about.
		{"local probe id", with(func(i domain.ProbeHostActionIntent) string {
			return wire(i, func(r *ProbeInitResult) { r.Facts.ProbeID = "local" })
		})},
		{"non-uuid probe id", with(func(i domain.ProbeHostActionIntent) string {
			return wire(i, func(r *ProbeInitResult) { r.Facts.ProbeID = "probe-1" })
		})},
		{"empty stream id", with(func(i domain.ProbeHostActionIntent) string {
			return wire(i, func(r *ProbeInitResult) { r.Facts.StreamID = "" })
		})},
		{"short fingerprint", with(func(i domain.ProbeHostActionIntent) string {
			return wire(i, func(r *ProbeInitResult) { r.Facts.CertificateFingerprint = r.Facts.CertificateFingerprint[:63] })
		})},
		{"uppercase fingerprint", with(func(i domain.ProbeHostActionIntent) string {
			return wire(i, func(r *ProbeInitResult) {
				r.Facts.CertificateFingerprint = strings.ToUpper(r.Facts.CertificateFingerprint)
			})
		})},
		{"empty token", with(func(i domain.ProbeHostActionIntent) string {
			return wire(i, func(r *ProbeInitResult) { r.EnrollmentToken = "" })
		})},
		{"short token", with(func(i domain.ProbeHostActionIntent) string {
			return wire(i, func(r *ProbeInitResult) { r.EnrollmentToken = probeInitTestPrefix + strings.Repeat("A", 42) })
		})},
		{"wrong prefix token", with(func(i domain.ProbeHostActionIntent) string {
			return wire(i, func(r *ProbeInitResult) { r.EnrollmentToken = "tok_" + strings.Repeat("A", 60) })
		})},
		{"bad character token", with(func(i domain.ProbeHostActionIntent) string {
			return wire(i, func(r *ProbeInitResult) { r.EnrollmentToken = probeInitTestPrefix + strings.Repeat("A", 42) + "+" })
		})},
		{"oversize token", with(func(i domain.ProbeHostActionIntent) string {
			return wire(i, func(r *ProbeInitResult) { r.EnrollmentToken = probeInitTestPrefix + strings.Repeat("A", 240) })
		})},

		// Non-canonical spellings of an otherwise genuine receipt.
		{"space after colon", with(func(i domain.ProbeHostActionIntent) string {
			return strings.Replace(good(i), `"state":"`, `"state": "`, 1)
		})},
		{"pretty printed", with(func(i domain.ProbeHostActionIntent) string {
			var dto probeInitReceiptDTO
			if err := json.Unmarshal([]byte(good(i)), &dto); err != nil {
				return ""
			}
			body, _ := json.MarshalIndent(dto, "", "  ")
			return string(body) + "\n"
		})},
		{"members reordered", with(func(i domain.ProbeHostActionIntent) string {
			r := probeInitTestResult()
			return `{"kind":"probe_init","state":"probe_initialized","marker_sha256":"` + i.MarkerSHA256() + `","probe_id":"` + r.Facts.ProbeID +
				`","stream_id":"` + r.Facts.StreamID + `","certificate_fingerprint":"` + r.Facts.CertificateFingerprint + `","enrollment_token":"` + r.EnrollmentToken + `"}` + "\n"
		})},
		{"extra member", with(func(i domain.ProbeHostActionIntent) string {
			return strings.TrimSuffix(good(i), "}\n") + `,"expires_at":"2026-01-01T00:00:00Z"}` + "\n"
		})},
		{"missing token member", with(func(i domain.ProbeHostActionIntent) string {
			return strings.Replace(good(i), `,"enrollment_token":"`+probeInitTestToken+`"`, "", 1)
		})},
		{"duplicate member with another token", with(func(i domain.ProbeHostActionIntent) string {
			return strings.TrimSuffix(good(i), "}\n") + `,"enrollment_token":"` + probeInitTestMakeToken(9) + `"}` + "\n"
		})},

		// Framing.
		{"receipt missing its LF", with(func(i domain.ProbeHostActionIntent) string { return strings.TrimSuffix(good(i), "\n") })},
		{"receipt with CRLF", with(func(i domain.ProbeHostActionIntent) string { return strings.TrimSuffix(good(i), "\n") + "\r\n" })},
		{"receipt followed by an extra byte", with(func(i domain.ProbeHostActionIntent) string { return good(i) + "x" })},
		{"receipt followed by an extra LF", with(func(i domain.ProbeHostActionIntent) string { return good(i) + "\n" })},
		{"receipt twice", with(func(i domain.ProbeHostActionIntent) string { return good(i) + good(i) })},
		{"receipt then a second receipt with another token", with(func(i domain.ProbeHostActionIntent) string {
			other := probeInitTestResult()
			other.EnrollmentToken = probeInitTestMakeToken(9)
			return good(i) + string(EncodeProbeInitReceipt(i, other))
		})},
		{"leading space", with(func(i domain.ProbeHostActionIntent) string { return " " + good(i) })},
		{"truncated receipt", with(func(i domain.ProbeHostActionIntent) string { return good(i)[:60] })},
		{"empty output", with(func(domain.ProbeHostActionIntent) string { return "" })},
		{"only LF", with(func(domain.ProbeHostActionIntent) string { return "\n" })},
		{"second challenge line before the receipt", tailOf(func(i domain.ProbeHostActionIntent, challenge []byte) string {
			return string(challenge) + good(i)
		})},
		{"stdout bound exceeded after the challenge", with(func(domain.ProbeHostActionIntent) string {
			return strings.Repeat("x", MaxSSHBinaryStageReceiptSize+1)
		})},
		{"receipt larger than the stdout bound", with(func(i domain.ProbeHostActionIntent) string {
			return strings.TrimSuffix(good(i), "}\n") + strings.Repeat(" ", MaxSSHBinaryStageReceiptSize) + "}\n"
		})},

		// A genuine receipt is not enough without a clean exit.
		{"nonzero exit with a valid receipt", func(helper *hostActSendHelper) { helper.exit = sendTExit() }},
		{"signal exit with a valid receipt", func(helper *hostActSendHelper) { helper.signal = "TERM" }},
		{"missing exit status with a valid receipt", func(helper *hostActSendHelper) { helper.noExit = true }},
		{"stderr bound exceeded with a valid receipt", func(helper *hostActSendHelper) { helper.stderr = strings.Repeat("y", stageSendStderrMax+1) }},
		{"nonzero exit with the token on stderr", func(helper *hostActSendHelper) {
			helper.exit = sendTExit()
			helper.stderr = probeInitTestToken
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newProbeInitSendHarness(t, func(helper *hostActSendHelper, _ *hostActSendAuthorizer) { tc.tune(helper) })
			out := h.sendInit(t)
			h.requireInitUnknown(t, out, h.wantStdin(t, h.genuineSignature()))
			requireNoInitMaterial(t, out.err)
			if h.auth.calls.Load() != 1 {
				t.Fatalf("authorizer calls = %d, want exactly 1", h.auth.calls.Load())
			}
			if h.helper.verified.Load() != 1 {
				t.Fatalf("helper emulation verdict %d, want accepted grant", h.helper.verified.Load())
			}
		})
	}
}

// A grant the sender accepts structurally but the helper's trusted key rejects
// is discoverable only by the helper, after the input was sent: Unknown, and no
// receipt, hence no token, ever existed.
func TestSendProbeInitForgedGrantRejectedByHelperIsUnknown(t *testing.T) {
	forger := newAuthSendAuthority(t)
	h := newProbeInitSendHarness(t, func(_ *hostActSendHelper, auth *hostActSendAuthorizer) {
		auth.hook = func(_ context.Context, _ *hostActSendAuthorizer, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
			return hostActSendSign(forger, request), nil
		}
	})
	out := h.sendInit(t)
	forged := ed25519.Sign(forger.priv, h.wantRequest().SigningMessage())
	h.requireInitUnknown(t, out, h.wantStdin(t, forged))
	if h.helper.verified.Load() != 2 {
		t.Fatalf("helper emulation verdict %d, want rejected", h.helper.verified.Load())
	}
}

// A signature by the genuine authority over the binary-stage message, or over
// the host-action message of another kind, is refused by the helper.
func TestSendProbeInitCrossDomainSignaturesAreRejectedByHelper(t *testing.T) {
	t.Run("binary-stage message", func(t *testing.T) {
		var stageSignature []byte
		h := newProbeInitSendHarness(t, func(_ *hostActSendHelper, auth *hostActSendAuthorizer) {
			auth.hook = func(_ context.Context, a *hostActSendAuthorizer, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
				stage := domain.ProbeBinaryStageIntent{Identity: request.Intent.Identity, OwnerID: request.Intent.OwnerID, AttemptID: request.Intent.AttemptID}
				stageRequest := domain.ProbeBinaryStageAdmissionRequest{Intent: stage, Challenge: domain.ProbeBinaryStageChallenge{
					AuthorityKeyID: request.Challenge.AuthorityKeyID, Nonce: request.Challenge.Nonce, MarkerSHA256: stage.MarkerSHA256(),
				}}
				grant := domain.ProbeHostActionGrant{Request: request}
				copy(grant.Signature[:], ed25519.Sign(a.authority.priv, stageRequest.SigningMessage()))
				stageSignature = grant.Signature[:]
				return grant, nil
			}
		})
		out := h.sendInit(t)
		h.requireInitUnknown(t, out, h.wantStdin(t, stageSignature))
		if h.helper.verified.Load() != 2 {
			t.Fatalf("helper emulation verdict %d, want the cross-domain signature rejected", h.helper.verified.Load())
		}
	})

	// The grant a data_directory or service_start action would receive, signed by
	// the genuine authority but placed in a probe_init request.
	for _, signedKind := range []string{hostActFrameKindData, hostActFrameKindUnit, hostActFrameKindService, domain.ProbeActivationServiceStart} {
		t.Run("signature of "+signedKind, func(t *testing.T) {
			var signature []byte
			h := newProbeInitSendHarness(t, func(_ *hostActSendHelper, auth *hostActSendAuthorizer) {
				auth.hook = func(_ context.Context, a *hostActSendAuthorizer, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
					other := request
					other.Intent.Kind = signedKind
					other.Challenge.MarkerSHA256 = other.Intent.MarkerSHA256()
					grant := domain.ProbeHostActionGrant{Request: request}
					copy(grant.Signature[:], ed25519.Sign(a.authority.priv, other.SigningMessage()))
					signature = grant.Signature[:]
					return grant, nil
				}
			})
			out := h.sendInit(t)
			h.requireInitUnknown(t, out, h.wantStdin(t, signature))
			if h.helper.verified.Load() != 2 {
				t.Fatalf("helper emulation verdict %d, want the other kind's signature rejected", h.helper.verified.Load())
			}
		})
	}
}

func TestSendProbeInitRefusesNonProbeInitIntentsWithZeroConnections(t *testing.T) {
	h := newProbeInitSendHarness(t, nil)
	ctx := t.Context()
	refused := func(name string, result ProbeInitResult, err, want error) {
		t.Helper()
		if err != want || result != (ProbeInitResult{}) {
			t.Fatalf("%s: %+v %v, want %v", name, result, err, want)
		}
	}

	for _, kind := range []string{
		hostActFrameKindData, hostActFrameKindUnit, hostActFrameKindService, domain.ProbeActivationServiceStart,
		hostActFrameKindArtifactDir, domain.ProbeActivationRegistration, domain.ProbeActivationEnrollment, "", "Probe_Init", "probe_init ",
	} {
		result, err := h.sender.SendProbeInit(ctx, hostActSendIntentFor(h.f, kind), h.auth)
		refused("kind "+kind, result, err, ErrSSHBinaryStageInput)
	}

	var nilSender *SSHBinaryStageSender
	result, err := nilSender.SendProbeInit(ctx, h.intent, h.auth)
	refused("nil sender", result, err, ErrSSHBinaryStageInput)
	result, err = (&SSHBinaryStageSender{}).SendProbeInit(ctx, h.intent, h.auth)
	refused("zero sender", result, err, ErrSSHBinaryStageInput)
	var nilCtx *sendTNilContext
	result, err = h.sender.SendProbeInit(nilCtx, h.intent, h.auth)
	refused("typed nil ctx", result, err, ErrSSHBinaryStageInput)

	var typedNilPointer *hostActSendAuthorizer
	var typedNilFunc hostActSendFuncAuthorizer
	for name, authorizer := range map[string]ProbeHostActionAuthorizer{
		"untyped nil authorizer":    nil,
		"typed nil pointer":         typedNilPointer,
		"typed nil func authorizer": typedNilFunc,
	} {
		result, err = h.sender.SendProbeInit(ctx, h.intent, authorizer)
		refused(name, result, err, ErrSSHBinaryStageInput)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	result, err = h.sender.SendProbeInit(canceled, h.intent, h.auth)
	refused("canceled ctx", result, err, context.Canceled)
	expired, stop := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer stop()
	result, err = h.sender.SendProbeInit(expired, h.intent, h.auth)
	refused("expired ctx", result, err, context.DeadlineExceeded)

	mutations := map[string]func(*domain.ProbeHostActionIntent){
		"docker mode":            func(i *domain.ProbeHostActionIntent) { i.Identity.Mode = domain.ProbeInstallDocker },
		"non-root user":          func(i *domain.ProbeHostActionIntent) { i.Identity.Host.User = "deploy" },
		"empty owner":            func(i *domain.ProbeHostActionIntent) { i.OwnerID = "" },
		"malformed attempt":      func(i *domain.ProbeHostActionIntent) { i.AttemptID = "not-an-id" },
		"invalid binary pin":     func(i *domain.ProbeHostActionIntent) { i.Identity.Binary.Size = 0 },
		"host key pin binding":   func(i *domain.ProbeHostActionIntent) { i.Identity.HostKeyFingerprint = hostActFrameFingerprint(9) },
		"client key pin binding": func(i *domain.ProbeHostActionIntent) { i.Identity.ClientKeyFingerprint = hostActFrameFingerprint(9) },
	}
	for name, mutate := range mutations {
		intent := h.intent
		mutate(&intent)
		result, err = h.sender.SendProbeInit(ctx, intent, h.auth)
		refused(name, result, err, ErrSSHBinaryStageInput)
	}
	result, err = h.sender.SendProbeInit(ctx, domain.ProbeHostActionIntent{}, h.auth)
	refused("zero intent", result, err, ErrSSHBinaryStageInput)

	h.requireNoIO(t)
}

// SendHostAction no longer carries probe_init: its plain receipt would drop the
// facts and the token, so the kind is refused outright before any I/O.
func TestSendHostActionRefusesProbeInit(t *testing.T) {
	// The helper would happily answer a plain host-action receipt.
	h := newHostActSendHarness(t, domain.ProbeActivationProbeInit, nil)
	ctx := t.Context()

	ack, err := h.sender.SendHostAction(ctx, h.intent, h.auth)
	if err != ErrSSHBinaryStageInput || ack != (SSHHostActionAck{}) {
		t.Fatalf("SendHostAction(probe_init) = %+v %v, want ErrSSHBinaryStageInput", ack, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if ack, err := h.sender.SendHostAction(canceled, h.intent, h.auth); err != ErrSSHBinaryStageInput || ack != (SSHHostActionAck{}) {
		t.Fatalf("SendHostAction(probe_init) with a canceled ctx = %+v %v", ack, err)
	}
	var nilSender *SSHBinaryStageSender
	if ack, err := nilSender.SendHostAction(ctx, h.intent, h.auth); err != ErrSSHBinaryStageInput || ack != (SSHHostActionAck{}) {
		t.Fatalf("nil sender = %+v %v", ack, err)
	}
	h.requireNoIO(t)
}

func TestSendProbeInitNotStagedBeforeFirstStdinByte(t *testing.T) {
	keyIDStart := len(stageChallengePrefix)
	initReceipt := func(f *sendTFixture) []byte {
		return EncodeProbeInitReceipt(hostActSendIntentFor(f, domain.ProbeActivationProbeInit), probeInitTestResult())
	}
	cases := []struct {
		name string
		tune func(helper *hostActSendHelper)
		// racy marks the one case where a helper that exits right after a valid
		// challenge can be observed dead before or after the authorizer runs.
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
		{name: "a probe_init receipt in place of the challenge", tune: func(helper *hostActSendHelper) {
			helper.challengeFn = initReceipt
		}},
		{name: "a plain host-action receipt in place of the challenge", tune: func(helper *hostActSendHelper) {
			helper.challengeFn = func(f *sendTFixture) []byte {
				return EncodeProbeHostActionReceipt(hostActSendIntentFor(f, domain.ProbeActivationProbeInit))
			}
		}},
		{name: "helper exits right after the challenge", racy: true, tune: func(helper *hostActSendHelper) { helper.exitAfterChallenge = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newProbeInitSendHarness(t, func(helper *hostActSendHelper, _ *hostActSendAuthorizer) { tc.tune(helper) })
			start := time.Now()
			out := h.sendInit(t)
			if elapsed := time.Since(start); elapsed > 10*time.Second {
				t.Fatalf("classification took %v; want prompt return", elapsed)
			}
			requireNoInitMaterial(t, out.err)
			if tc.racy {
				if out.result != (ProbeInitResult{}) {
					t.Fatalf("a dead helper returned %+v", out.result)
				}
				h.requireExitRaceOutcome(t, hostActSendResult{err: out.err})
				return
			}
			h.requireInitNotStaged(t, out)
			if h.auth.calls.Load() != 0 {
				t.Fatalf("authorizer called %d times for a refused challenge", h.auth.calls.Load())
			}
		})
	}
}

func TestSendProbeInitAuthorizerErrorIsNotStaged(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{name: "plain error", err: errors.New("journal attempt already consumed")},
		{name: "error carrying a token", err: errors.New("denied " + probeInitTestToken)},
		{name: "context canceled sentinel without a canceled context", err: context.Canceled},
		{name: "deadline exceeded sentinel without an expired context", err: context.DeadlineExceeded},
		{name: "domain sentinel", err: domain.ErrProbeArtifactStorage},
		{name: "write conflict sentinel", err: domain.ErrProbeBinaryStageWriteConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newProbeInitSendHarness(t, func(_ *hostActSendHelper, auth *hostActSendAuthorizer) {
				auth.hook = func(context.Context, *hostActSendAuthorizer, domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
					return domain.ProbeHostActionGrant{}, tc.err
				}
			})
			out := h.sendInit(t)
			h.requireInitNotStaged(t, out)
			if errors.Is(out.err, tc.err) {
				t.Fatalf("authorizer error %v leaked through %v", tc.err, out.err)
			}
			requireNoInitMaterial(t, out.err)
			if h.auth.calls.Load() != 1 {
				t.Fatalf("authorizer calls = %d, want exactly 1 (no retry)", h.auth.calls.Load())
			}
			if requests := h.auth.seenRequests(); len(requests) != 1 || requests[0] != h.wantRequest() {
				t.Fatalf("authorizer request %+v", requests)
			}
		})
	}
}

func TestSendProbeInitGrantForDifferentRequestIsNotStaged(t *testing.T) {
	type grantFn func(a *hostActSendAuthorizer, r domain.ProbeHostActionAdmissionRequest) domain.ProbeHostActionGrant
	reKind := func(kind string) grantFn {
		return func(a *hostActSendAuthorizer, r domain.ProbeHostActionAdmissionRequest) domain.ProbeHostActionGrant {
			r.Intent.Kind = kind
			r.Challenge.MarkerSHA256 = r.Intent.MarkerSHA256()
			return hostActSendSign(a.authority, r)
		}
	}
	cases := []struct {
		name  string
		grant grantFn
	}{
		{"different nonce, validly signed", func(a *hostActSendAuthorizer, r domain.ProbeHostActionAdmissionRequest) domain.ProbeHostActionGrant {
			r.Challenge.Nonce[0] ^= 0xff
			return hostActSendSign(a.authority, r)
		}},
		{"different authority key id, validly signed", func(a *hostActSendAuthorizer, r domain.ProbeHostActionAdmissionRequest) domain.ProbeHostActionGrant {
			r.Challenge.AuthorityKeyID = strings.Repeat("ab", 32)
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
		{"grant for service_start of the same operation, validly signed", reKind(domain.ProbeActivationServiceStart)},
		{"grant for data_directory of the same operation, validly signed", reKind(hostActFrameKindData)},
		{"grant for unit_file of the same operation, validly signed", reKind(hostActFrameKindUnit)},
		{"grant for service of the same operation, validly signed", reKind(hostActFrameKindService)},
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
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newProbeInitSendHarness(t, func(_ *hostActSendHelper, auth *hostActSendAuthorizer) {
				auth.hook = func(_ context.Context, a *hostActSendAuthorizer, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
					return tc.grant(a, request), nil
				}
			})
			out := h.sendInit(t)
			h.requireInitNotStaged(t, out)
			if h.auth.calls.Load() != 1 {
				t.Fatalf("authorizer calls = %d, want exactly 1", h.auth.calls.Load())
			}
		})
	}
}

func TestSendProbeInitCancelDuringAuthorizationIsNotStaged(t *testing.T) {
	h := newProbeInitSendHarness(t, func(_ *hostActSendHelper, auth *hostActSendAuthorizer) { auth.hook = hostActSendBlockUntilDone })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := h.initAsync(ctx)
	select {
	case <-h.auth.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("authorizer call not reached")
	}
	select {
	case out := <-done:
		t.Fatalf("returned while the authorizer blocks: %+v", out)
	case <-time.After(250 * time.Millisecond):
	}
	if got := h.f.stdinBytes(); len(got) != 0 {
		t.Fatalf("%d stdin bytes sent while authorizing", len(got))
	}
	cancel()
	out := probeInitSendAwait(t, done, "cancel during authorization")
	h.requireInitNotStaged(t, out)
	if h.auth.calls.Load() != 1 {
		t.Fatalf("authorizer calls = %d, want 1", h.auth.calls.Load())
	}
}

// Once the first stdin byte has been delivered, cancellation can no longer
// prove the host untouched: Unknown, and the token the helper writes afterwards
// is never delivered to the caller.
func TestSendProbeInitCancelAfterStdinIsUnknown(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHelper := func() { releaseOnce.Do(func() { close(release) }) }
	h := newProbeInitSendHarness(t, func(helper *hostActSendHelper, _ *hostActSendAuthorizer) {
		helper.tail = func(_ *sendTFixture, intent domain.ProbeHostActionIntent, _ []byte) string {
			<-release
			return string(EncodeProbeInitReceipt(intent, probeInitTestResult()))
		}
	})
	// Registered after the fixture's own cleanup, so it runs first and lets the
	// held helper emulation finish before the fixture is torn down.
	t.Cleanup(releaseHelper)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := h.initAsync(ctx)
	hostActSendWaitEOF(t, h.f)
	select {
	case out := <-done:
		t.Fatalf("returned while the helper holds its receipt: %+v", out)
	case <-time.After(250 * time.Millisecond):
	}
	cancel()
	out := probeInitSendAwait(t, done, "cancel after stdin")
	releaseHelper()
	h.requireInitUnknown(t, out, h.wantStdin(t, h.genuineSignature()))
}

func TestSendProbeInitHelperDiesDuringAuthorizationIsNotStaged(t *testing.T) {
	var helperRef *hostActSendHelper
	h := newProbeInitSendHarness(t, func(helper *hostActSendHelper, auth *hostActSendAuthorizer) {
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
	h.requireInitNotStaged(t, h.sendInit(t))
	if h.auth.calls.Load() != 1 {
		t.Fatalf("authorizer calls = %d, want 1", h.auth.calls.Load())
	}
}

// ---- service_start over SendHostAction -------------------------------------

func TestSendHostActionServiceStartHappyPath(t *testing.T) {
	h := newHostActSendHarness(t, domain.ProbeActivationServiceStart, nil)
	res := h.send(t)
	if res.err != nil || res.ack != (SSHHostActionAck{Intent: h.intent}) {
		t.Fatalf("result %+v %v", res.ack, res.err)
	}
	h.helper.waitIdle(t, h.f)
	if h.auth.calls.Load() != 1 {
		t.Fatalf("authorizer calls = %d, want 1", h.auth.calls.Load())
	}
	if requests := h.auth.seenRequests(); len(requests) != 1 || requests[0] != h.wantRequest() || requests[0].Intent.Kind != "service_start" {
		t.Fatalf("authorizer request %+v", requests)
	}
	if want := h.wantStdin(t, h.genuineSignature()); !bytes.Equal(h.f.stdinBytes(), want) {
		t.Fatalf("server stdin = %d bytes, want %d", len(h.f.stdinBytes()), len(want))
	}
	if got := h.f.execCommands(); !slices.Equal(got, []string{hostActSendCommandLiteral}) {
		t.Fatalf("exec commands %v", got)
	}
	if h.helper.verified.Load() != 1 {
		t.Fatalf("helper emulation verdict %d, want accepted grant", h.helper.verified.Load())
	}
}

func TestSendHostActionServiceStartWrongReceiptsAreUnknown(t *testing.T) {
	probeInit := func(i domain.ProbeHostActionIntent) domain.ProbeHostActionIntent {
		i.Kind = domain.ProbeActivationProbeInit
		return i
	}
	cases := []struct {
		name string
		tune func(helper *hostActSendHelper)
	}{
		{"nonzero exit with a valid receipt", func(helper *hostActSendHelper) { helper.exit = sendTExit() }},
		{"signal exit with a valid receipt", func(helper *hostActSendHelper) { helper.signal = "TERM" }},
		{"missing exit status", func(helper *hostActSendHelper) { helper.noExit = true }},
		{"no receipt", func(helper *hostActSendHelper) {
			helper.tail = func(*sendTFixture, domain.ProbeHostActionIntent, []byte) string { return "" }
		}},
		{"a probe_init receipt with facts and token", func(helper *hostActSendHelper) {
			helper.tail = func(_ *sendTFixture, intent domain.ProbeHostActionIntent, _ []byte) string {
				return string(EncodeProbeInitReceipt(probeInit(intent), probeInitTestResult()))
			}
		}},
		{"the plain receipt of the probe_init action", func(helper *hostActSendHelper) {
			helper.tail = func(_ *sendTFixture, intent domain.ProbeHostActionIntent, _ []byte) string {
				return string(EncodeProbeHostActionReceipt(probeInit(intent)))
			}
		}},
		{"the receipt of the service action", func(helper *hostActSendHelper) {
			helper.tail = func(_ *sendTFixture, intent domain.ProbeHostActionIntent, _ []byte) string {
				intent.Kind = hostActFrameKindService
				return string(EncodeProbeHostActionReceipt(intent))
			}
		}},
		{"a receipt for another operation", func(helper *hostActSendHelper) {
			helper.tail = func(_ *sendTFixture, intent domain.ProbeHostActionIntent, _ []byte) string {
				intent.Identity.OperationID = hostActSendOtherUUID
				return string(EncodeProbeHostActionReceipt(intent))
			}
		}},
		{"a receipt for another attempt", func(helper *hostActSendHelper) {
			helper.tail = func(_ *sendTFixture, intent domain.ProbeHostActionIntent, _ []byte) string {
				intent.AttemptID = hostActSendOtherUUID
				return string(EncodeProbeHostActionReceipt(intent))
			}
		}},
		{"receipt followed by an extra byte", func(helper *hostActSendHelper) {
			helper.tail = func(_ *sendTFixture, intent domain.ProbeHostActionIntent, _ []byte) string {
				return string(EncodeProbeHostActionReceipt(intent)) + "x"
			}
		}},
		{"receipt missing its LF", func(helper *hostActSendHelper) {
			helper.tail = func(_ *sendTFixture, intent domain.ProbeHostActionIntent, _ []byte) string {
				return strings.TrimSuffix(string(EncodeProbeHostActionReceipt(intent)), "\n")
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHostActSendHarness(t, domain.ProbeActivationServiceStart, func(helper *hostActSendHelper, _ *hostActSendAuthorizer) { tc.tune(helper) })
			res := h.send(t)
			h.requireUnknown(t, res, h.wantStdin(t, h.genuineSignature()))
			if h.auth.calls.Load() != 1 {
				t.Fatalf("authorizer calls = %d, want exactly 1", h.auth.calls.Load())
			}
		})
	}
}

func TestSendHostActionServiceStartNotStagedCases(t *testing.T) {
	t.Run("authorizer error", func(t *testing.T) {
		h := newHostActSendHarness(t, domain.ProbeActivationServiceStart, func(_ *hostActSendHelper, auth *hostActSendAuthorizer) {
			auth.hook = func(context.Context, *hostActSendAuthorizer, domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
				return domain.ProbeHostActionGrant{}, errors.New("journal step is not in flight")
			}
		})
		h.requireNotStaged(t, h.send(t))
	})
	for _, kind := range []string{domain.ProbeActivationProbeInit, hostActFrameKindService, hostActFrameKindData, hostActFrameKindUnit} {
		t.Run("grant for "+kind+" validly signed", func(t *testing.T) {
			h := newHostActSendHarness(t, domain.ProbeActivationServiceStart, func(_ *hostActSendHelper, auth *hostActSendAuthorizer) {
				auth.hook = func(_ context.Context, a *hostActSendAuthorizer, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
					request.Intent.Kind = kind
					request.Challenge.MarkerSHA256 = request.Intent.MarkerSHA256()
					return hostActSendSign(a.authority, request), nil
				}
			})
			h.requireNotStaged(t, h.send(t))
		})
	}
	t.Run("grant signed by an unknown authority is rejected by the helper", func(t *testing.T) {
		forger := newAuthSendAuthority(t)
		h := newHostActSendHarness(t, domain.ProbeActivationServiceStart, func(_ *hostActSendHelper, auth *hostActSendAuthorizer) {
			auth.hook = func(_ context.Context, _ *hostActSendAuthorizer, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
				return hostActSendSign(forger, request), nil
			}
		})
		res := h.send(t)
		h.requireUnknown(t, res, h.wantStdin(t, ed25519.Sign(forger.priv, h.wantRequest().SigningMessage())))
		if h.helper.verified.Load() != 2 {
			t.Fatalf("helper emulation verdict %d, want rejected", h.helper.verified.Load())
		}
	})
}

// ---- sendHostAction's receipt callback --------------------------------------

func TestSendHostActionReceiptCallbackIsTheOnlyAuthority(t *testing.T) {
	t.Run("a nil callback never acknowledges", func(t *testing.T) {
		h := newHostActSendHarness(t, domain.ProbeActivationServiceStart, nil)
		ack, err := h.sender.sendHostAction(t.Context(), h.intent, h.auth, nil, nil)
		if err != ErrSSHBinaryStageUnknown || ack != (SSHHostActionAck{}) {
			t.Fatalf("nil callback = %+v %v, want Unknown", ack, err)
		}
		h.helper.waitIdle(t, h.f)
		if want := h.wantStdin(t, h.genuineSignature()); !bytes.Equal(h.f.stdinBytes(), want) {
			t.Fatal("the helper did not receive the action before the refusal")
		}
	})

	t.Run("a refusing callback overrides a perfect receipt", func(t *testing.T) {
		h := newHostActSendHarness(t, domain.ProbeActivationServiceStart, nil)
		calls := 0
		ack, err := h.sender.sendHostAction(t.Context(), h.intent, h.auth, nil, func([]byte) bool { calls++; return false })
		if err != ErrSSHBinaryStageUnknown || ack != (SSHHostActionAck{}) || calls != 1 {
			t.Fatalf("refusing callback = %+v %v after %d calls", ack, err, calls)
		}
	})

	t.Run("the callback sees exactly the bytes after the challenge", func(t *testing.T) {
		h := newProbeInitSendHarness(t, nil)
		var seen [][]byte
		ack, err := h.sender.sendHostAction(t.Context(), h.intent, h.auth, nil, func(body []byte) bool {
			seen = append(seen, bytes.Clone(body))
			return true
		})
		if err != nil || ack != (SSHHostActionAck{Intent: h.intent}) {
			t.Fatalf("callback true = %+v %v", ack, err)
		}
		want := EncodeProbeInitReceipt(h.intent, probeInitTestResult())
		if len(seen) != 1 || !bytes.Equal(seen[0], want) {
			t.Fatalf("callback saw %q, want exactly the receipt %q", seen, want)
		}
	})

	t.Run("the callback never runs after a failed exit", func(t *testing.T) {
		h := newProbeInitSendHarness(t, func(helper *hostActSendHelper, _ *hostActSendAuthorizer) { helper.exit = sendTExit() })
		calls := 0
		ack, err := h.sender.sendHostAction(t.Context(), h.intent, h.auth, nil, func([]byte) bool { calls++; return true })
		if err != ErrSSHBinaryStageUnknown || ack != (SSHHostActionAck{}) {
			t.Fatalf("failed exit = %+v %v, want Unknown", ack, err)
		}
		if calls != 0 {
			t.Fatalf("the receipt was parsed %d times although the helper failed", calls)
		}
	})

	t.Run("the callback never runs when the helper never answered the challenge", func(t *testing.T) {
		h := newProbeInitSendHarness(t, func(helper *hostActSendHelper, _ *hostActSendAuthorizer) {
			helper.noChallenge = true
			helper.noChallengeStatus = 1
		})
		calls := 0
		_, err := h.sender.sendHostAction(t.Context(), h.intent, h.auth, nil, func([]byte) bool { calls++; return true })
		if err != ErrSSHBinaryStageNotStaged || calls != 0 {
			t.Fatalf("no challenge = %v after %d callback calls, want NotStaged and 0", err, calls)
		}
	})
}
