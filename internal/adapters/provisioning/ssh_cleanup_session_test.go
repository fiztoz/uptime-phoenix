package provisioning

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// The cleanup session reuses the loopback host-action helper emulation of
// ssh_host_action_send_test.go: protocol evidence only, no real host.

func newCleanupSessionHarness(t *testing.T, tune func(helper *hostActSendHelper, auth *hostActSendAuthorizer)) (*hostActSendHarness, *sshProbeCleanupSession, string) {
	t.Helper()
	h := newHostActSendHarness(t, domain.ProbeHostActionCleanup, tune)
	factory, err := NewSSHProbeCleanupSessionFactory(h.f.policy(), h.auth)
	if err != nil {
		t.Fatal(err)
	}
	session, fingerprint, err := factory.NewCleanupSession(t.Context(), bytes.Clone(h.f.clientKey), h.f.hostFP())
	if err != nil || session == nil {
		t.Fatalf("NewCleanupSession = %v %q %v", session, fingerprint, err)
	}
	h.intent = hostActSendIntentFor(h.f, domain.ProbeHostActionCleanup)
	return h, session.(*sshProbeCleanupSession), fingerprint
}

func runCleanupSession(t *testing.T, session *sshProbeCleanupSession, ctx context.Context, intent domain.ProbeHostActionIntent) domain.ProbeInstallResourceOutcome {
	t.Helper()
	done := make(chan domain.ProbeInstallResourceOutcome, 1)
	go func() { done <- session.Cleanup(ctx, intent) }()
	select {
	case r := <-done:
		return r
	case <-time.After(30 * time.Second):
		t.Fatal("Cleanup did not return")
		return domain.ProbeInstallResourceOutcome{}
	}
}

func TestSSHProbeCleanupSessionFactoryRequiresAnAuthorizer(t *testing.T) {
	policy := probe.EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
	var typedNil *hostActSendAuthorizer
	for name, authorizer := range map[string]ProbeHostActionAuthorizer{"untyped nil": nil, "typed nil": typedNil} {
		if factory, err := NewSSHProbeCleanupSessionFactory(policy, authorizer); factory != nil || err != domain.ErrValidation {
			t.Fatalf("%s: %v %v", name, factory, err)
		}
	}
}

func TestCleanupSessionApplied(t *testing.T) {
	h, session, fingerprint := newCleanupSessionHarness(t, nil)
	if fingerprint != h.f.clientFP() {
		t.Fatalf("fingerprint %q", fingerprint)
	}
	actSessionRequireOutcome(t, runCleanupSession(t, session, t.Context(), h.intent), actSessionApplied)
	h.helper.waitIdle(t, h.f)
	if requests := h.auth.seenRequests(); len(requests) != 1 || requests[0].Intent != h.intent || requests[0].Intent.Kind != domain.ProbeHostActionCleanup {
		t.Fatalf("authorizer requests %+v", requests)
	}
	if want := h.wantStdin(t, h.genuineSignature()); !bytes.Equal(h.f.stdinBytes(), want) {
		t.Fatalf("server stdin = %d bytes, want %d", len(h.f.stdinBytes()), len(want))
	}
}

func TestCleanupSessionNotStartedAndUnknown(t *testing.T) {
	t.Run("authorizer refusal reaches no helper input", func(t *testing.T) {
		h, session, _ := newCleanupSessionHarness(t, func(_ *hostActSendHelper, auth *hostActSendAuthorizer) {
			auth.hook = func(context.Context, *hostActSendAuthorizer, domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
				return domain.ProbeHostActionGrant{}, errors.New("cleanup not running")
			}
		})
		actSessionRequireOutcome(t, runCleanupSession(t, session, t.Context(), h.intent), actSessionNotStarted)
		if h.f.execCalls.Load() > 0 {
			h.helper.waitIdle(t, h.f)
		}
		if got := h.f.stdinBytes(); len(got) != 0 {
			t.Fatalf("not_started although the helper received %d stdin bytes", len(got))
		}
	})
	t.Run("helper refusal after the grant is unknown", func(t *testing.T) {
		h, session, _ := newCleanupSessionHarness(t, func(helper *hostActSendHelper, _ *hostActSendAuthorizer) { helper.exit = sendTExit() })
		actSessionRequireOutcome(t, runCleanupSession(t, session, t.Context(), h.intent), actSessionUnknown)
		h.helper.waitIdle(t, h.f)
		if want := h.wantStdin(t, h.genuineSignature()); !bytes.Equal(h.f.stdinBytes(), want) {
			t.Fatalf("server stdin = %d bytes, want %d", len(h.f.stdinBytes()), len(want))
		}
	})
	t.Run("another kind is refused before any I/O", func(t *testing.T) {
		h, session, _ := newCleanupSessionHarness(t, nil)
		intent := hostActSendIntentFor(h.f, domain.ProbeHostActionService)
		actSessionRequireOutcome(t, runCleanupSession(t, session, t.Context(), intent), actSessionNotStarted)
		if h.f.execCalls.Load() != 0 || len(h.auth.seenRequests()) != 0 {
			t.Fatal("a non-cleanup intent reached the host or the authorizer")
		}
	})
}
