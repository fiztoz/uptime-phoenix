package probe

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// cutoverExecutorSpy is the narrowest ports.EdgeDeliveryCutoverExecutor: it
// records the exact decoded commands and returns caller-chosen outcomes.
type cutoverExecutorSpy struct {
	mu    sync.Mutex
	calls []domain.DeliveryCutoverCommand
	auths []domain.EdgeCommandAuthority
	out   func(domain.DeliveryCutoverCommand) (domain.ProbeCommandOutcome, error)
}

var _ ports.EdgeDeliveryCutoverExecutor = (*cutoverExecutorSpy)(nil)

func (s *cutoverExecutorSpy) ExecuteDeliveryCutover(_ context.Context, a domain.EdgeCommandAuthority, c domain.DeliveryCutoverCommand) (domain.ProbeCommandOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, c)
	s.auths = append(s.auths, a)
	if s.out != nil {
		return s.out(c)
	}
	return cutoverSuccessOutcome(c), nil
}

func (s *cutoverExecutorSpy) lastAuthority() domain.EdgeCommandAuthority {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.auths) == 0 {
		return domain.EdgeCommandAuthority{}
	}
	return s.auths[len(s.auths)-1]
}

func (s *cutoverExecutorSpy) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *cutoverExecutorSpy) last() domain.DeliveryCutoverCommand {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		return domain.DeliveryCutoverCommand{}
	}
	return s.calls[len(s.calls)-1]
}

func cutoverSuccessOutcome(c domain.DeliveryCutoverCommand) domain.ProbeCommandOutcome {
	at := c.CreatedAt.Add(time.Second).UTC()
	return domain.ProbeCommandOutcome{CommandID: c.CommandID, Status: "applied", AppliedAt: &at, Code: domain.DeliveryCutoverSuccessCode(c.Kind), Message: "applied"}
}

func cutoverEnvelope(t *testing.T, kind string) (domain.DeliveryCutoverCommand, Envelope) {
	t.Helper()
	c, payload := deliveryCutoverFixture(t, kind)
	decoded, err := (DeliveryCutoverCodec{}).DecodeDeliveryCutover(t.Context(), payload)
	if err != nil {
		t.Fatal(err)
	}
	c.PayloadHash = decoded.PayloadHash
	return c, Envelope{Type: "command.request", ConnectionGeneration: Decimal(2), Payload: payload}
}

var cutoverAuthority = domain.EdgeCommandAuthority{HubID: "11111111-2222-4333-8444-555555555555", ProbeID: "21111111-2222-4333-8444-555555555555", StreamID: "31111111-2222-4333-8444-555555555555", ConnectionGeneration: 2}

// TestApplyEdgeCommandDeliveryCutoverGates pins the negotiated execution gate:
// new quiesce/apply effects require negotiated welcome support, delivery.resume
// runs on a configured executor alone so a feature-disable drain can still
// release source holds, and every refusal is silent or an error — never a
// fabricated success receipt.
func TestApplyEdgeCommandDeliveryCutoverGates(t *testing.T) {
	for _, kind := range []string{CommandDeliveryQuiesce, CommandDeliveryApply, CommandDeliveryResume} {
		for _, tc := range []struct {
			name              string
			negotiated, wired bool
			wantCalls         int
			wantSkip          bool
		}{
			{name: "negotiated", negotiated: true, wired: true, wantCalls: 1},
			{name: "unnegotiated", wired: true, wantSkip: kind != CommandDeliveryResume, wantCalls: boolInt(kind == CommandDeliveryResume)},
			{name: "no_executor_negotiated", negotiated: true},
			{name: "no_executor_unnegotiated"},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				c, envelope := cutoverEnvelope(t, kind)
				spy := &cutoverExecutorSpy{}
				gate := deliveryCutoverGate{negotiated: tc.negotiated}
				if tc.wired {
					gate.executor = spy
				}
				frame, reconnect, err := applyEdgeCommand(t.Context(), nil, nil, nil, cutoverAuthority, envelope, gate)
				if !tc.wired {
					if err == nil || frame != nil {
						t.Fatal("unbacked cutover command produced work", err, frame)
					}
					return
				}
				if tc.wantSkip {
					if !errors.Is(err, errDeliveryCutoverUnnegotiated) || frame != nil {
						t.Fatalf("unnegotiated %s was not refused without a receipt: %v %v", kind, err, frame)
					}
					if spy.count() != 0 {
						t.Fatal("executor ran without negotiation")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if reconnect != (edgeCommandReconnect{}) {
					t.Fatalf("cutover command invented a reconnect deadline: %+v", reconnect)
				}
				if spy.count() != tc.wantCalls {
					t.Fatalf("executor called %d times, want %d", spy.count(), tc.wantCalls)
				}
				got := spy.last()
				if !sameDeliveryCutoverCommand(got, c) || got.Kind != c.Kind {
					t.Fatalf("executor received a changed command: %+v", got)
				}
				_, result, err := DecodeCommandResult(frame)
				if err != nil || result.CommandID != c.CommandID || result.Code == nil || *result.Code != domain.DeliveryCutoverSuccessCode(kind) || result.Details != nil {
					t.Fatalf("receipt shape changed: %+v %v", result, err)
				}
			})
		}
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// TestApplyEdgeCommandDeliveryCutoverRefusesBadOutcomes proves the wire layer
// never frames a fabricated or cross-claimed receipt even if an executor
// misbehaves: no retry delay is invented to compensate either.
func TestApplyEdgeCommandDeliveryCutoverRefusesBadOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  func(c domain.DeliveryCutoverCommand) domain.ProbeCommandOutcome
	}{
		{"wrong_kind_code", func(c domain.DeliveryCutoverCommand) domain.ProbeCommandOutcome {
			o := cutoverSuccessOutcome(c)
			o.Code = domain.DeliveryResumedCode
			return o
		}},
		{"code_empty_success", func(c domain.DeliveryCutoverCommand) domain.ProbeCommandOutcome {
			o := cutoverSuccessOutcome(c)
			o.Code = ""
			return o
		}},
		{"details_on_success", func(c domain.DeliveryCutoverCommand) domain.ProbeCommandOutcome {
			o := cutoverSuccessOutcome(c)
			o.CredentialVersion = 3
			return o
		}},
		{"already_resolved", func(c domain.DeliveryCutoverCommand) domain.ProbeCommandOutcome {
			o := cutoverSuccessOutcome(c)
			o.Status = "already_resolved"
			return o
		}},
		{"identity_mismatch", func(c domain.DeliveryCutoverCommand) domain.ProbeCommandOutcome {
			o := cutoverSuccessOutcome(c)
			o.CommandID = "71111111-2222-4333-8444-555555555555"
			return o
		}},
		{"fabricated_applied_without_time", func(c domain.DeliveryCutoverCommand) domain.ProbeCommandOutcome {
			o := cutoverSuccessOutcome(c)
			o.AppliedAt = nil
			return o
		}},
		{"success_code_on_rejection", func(c domain.DeliveryCutoverCommand) domain.ProbeCommandOutcome {
			return domain.ProbeCommandOutcome{CommandID: c.CommandID, Status: "rejected", Code: domain.DeliveryQuiescedCode, Message: "refused"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, envelope := cutoverEnvelope(t, CommandDeliveryQuiesce)
			spy := &cutoverExecutorSpy{out: func(c domain.DeliveryCutoverCommand) (domain.ProbeCommandOutcome, error) {
				return tc.out(c), nil
			}}
			frame, _, err := applyEdgeCommand(t.Context(), nil, nil, nil, cutoverAuthority, envelope, deliveryCutoverGate{executor: spy, negotiated: true})
			if err == nil || frame != nil {
				t.Fatal("fabricated outcome was framed", err, frame)
			}
			if spy.count() != 1 {
				t.Fatalf("executor called %d times, want exactly one", spy.count())
			}
		})
	}
	t.Run("executor_error_is_not_completion", func(t *testing.T) {
		_, envelope := cutoverEnvelope(t, CommandDeliveryApply)
		spy := &cutoverExecutorSpy{out: func(domain.DeliveryCutoverCommand) (domain.ProbeCommandOutcome, error) {
			return domain.ProbeCommandOutcome{}, errors.New("injected storage refusal")
		}}
		frame, _, err := applyEdgeCommand(t.Context(), nil, nil, nil, cutoverAuthority, envelope, deliveryCutoverGate{executor: spy, negotiated: true})
		if err == nil || frame != nil {
			t.Fatal("failed execution produced a receipt")
		}
	})
	t.Run("honest_rejection_receipt_transports", func(t *testing.T) {
		_, envelope := cutoverEnvelope(t, CommandDeliveryQuiesce)
		spy := &cutoverExecutorSpy{out: func(c domain.DeliveryCutoverCommand) (domain.ProbeCommandOutcome, error) {
			return domain.ProbeCommandOutcome{CommandID: c.CommandID, Status: "rejected", AppliedAt: nil, Code: "busy_source", Message: "hold refused"}, nil
		}}
		frame, _, err := applyEdgeCommand(t.Context(), nil, nil, nil, cutoverAuthority, envelope, deliveryCutoverGate{executor: spy, negotiated: true})
		if err != nil {
			t.Fatal(err)
		}
		_, result, err := DecodeCommandResult(frame)
		if err != nil || result.Status != "rejected" || result.Code == nil || *result.Code != "busy_source" || result.AppliedAt != nil {
			t.Fatalf("rejection receipt changed: %+v %v", result, err)
		}
	})
}

type cutoverIdentityStub struct{}

var _ ports.EdgeIdentityRepository = cutoverIdentityStub{}

func (cutoverIdentityStub) ReadIdentity(context.Context) (domain.EdgeIdentity, error) {
	return domain.EdgeIdentity{}, nil
}
func (cutoverIdentityStub) AcceptConnectionGeneration(context.Context, string, int64) error {
	return nil
}

// TestEdgeRuntimeDeliveryCutoverCapabilityRequiresExecutor pins the strong
// capability rule: it is advertised only with a real executor, and a configured
// executor is never silently dropped from the inventory.
func TestEdgeRuntimeDeliveryCutoverCapabilityRequiresExecutor(t *testing.T) {
	newRuntime := func(t *testing.T, cfg EdgeRuntimeConfig) error {
		t.Helper()
		_, err := NewEdgeRuntime(func(context.Context) (domain.EdgeIdentity, int64, error) {
			return domain.EdgeIdentity{}, 0, nil
		}, cutoverIdentityStub{}, &services.EdgeConfigService{}, cfg, func(context.Context) (Health, error) {
			return Health{}, nil
		})
		return err
	}
	base := EdgeRuntimeConfig{AgentVersion: "test", Capabilities: []string{"snapshot.v1"}}
	if err := newRuntime(t, base); err != nil {
		t.Fatal("plain runtime construction refused", err)
	}
	unbacked := base
	unbacked.Capabilities = []string{"snapshot.v1", DeliveryCutoverCapability}
	if err := newRuntime(t, unbacked); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("strong capability advertised without a real executor", err)
	}
	unadvertised := base
	unadvertised.DeliveryCutoverExecutor = &cutoverExecutorSpy{}
	if err := newRuntime(t, unadvertised); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("configured executor silently dropped from the inventory", err)
	}
	backed := unadvertised
	backed.Capabilities = []string{"snapshot.v1", DeliveryCutoverCapability}
	if err := newRuntime(t, backed); err != nil {
		t.Fatal("backed strong capability refused", err)
	}
}

func cutoverHandshakeFrames(t *testing.T, helloCaps []string, deliveryCutover bool) ([]byte, []byte) {
	t.Helper()
	identity := SessionIdentity{HubID: "11111111-2222-4333-8444-555555555555", ProbeID: "21111111-2222-4333-8444-555555555555", StreamID: "31111111-2222-4333-8444-555555555555"}
	hello := Hello{SessionIdentity: identity, AgentVersion: "test", ProtocolMin: 1, ProtocolMax: 1, ConfigRevision: Decimal(4), FirstRetainedSeq: 0, LastCreatedSeq: Decimal(10), Capabilities: helloCaps, ResourceBindings: []ResourceBinding{}}
	helloFrame, err := encodeFrame("hello", 0, hello)
	if err != nil {
		t.Fatal(err)
	}
	welcome := Welcome{SessionIdentity: identity, SelectedProtocol: 1, ConnectionGeneration: Decimal(1), CommittedSeq: Decimal(3), DesiredConfigRevision: Decimal(4), HeartbeatSeconds: HeartbeatSeconds, MaxFrameBytes: MaxFrameBytes, MaxBatchEvents: MaxBatchEvents, MaxBatchBytes: MaxBatchBytes, HubTime: Timestamp(time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)), DeliveryCutover: deliveryCutover}
	welcomeFrame, err := encodeFrame("welcome", Decimal(1), welcome)
	if err != nil {
		t.Fatal(err)
	}
	return helloFrame, welcomeFrame
}

// TestWelcomeDeliveryCutoverIsStrictOptionalBool pins the hub welcome contract:
// absence means false and a present value is an explicit JSON boolean — null and
// every other JSON type are refused, never coerced.
func TestWelcomeDeliveryCutoverIsStrictOptionalBool(t *testing.T) {
	_, welcomeFrame := cutoverHandshakeFrames(t, []string{"snapshot.v1"}, false)
	if !strings.Contains(string(welcomeFrame), `"delivery_cutover"`) {
		// omitempty hides false: absence must decode as false, never as an error.
		_, welcome, err := DecodeWelcome(welcomeFrame)
		if err != nil || welcome.DeliveryCutover {
			t.Fatalf("absent opt-in changed meaning: %v %+v", err, welcome)
		}
	}
	_, explicit := cutoverHandshakeFrames(t, []string{"snapshot.v1"}, true)
	_, welcome, err := DecodeWelcome(explicit)
	if err != nil || !welcome.DeliveryCutover {
		t.Fatalf("explicit opt-in lost: %v %+v", err, welcome)
	}
	for _, tc := range []struct {
		name  string
		value any
	}{
		{"null", nil},
		{"string", "true"},
		{"number", 1},
		{"object", map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := mutateJSON(t, explicit, func(frame map[string]any) {
				frame["payload"].(map[string]any)["delivery_cutover"] = tc.value
			})
			if _, _, err := DecodeWelcome(mutated); err == nil {
				t.Fatalf("non-boolean %s decoded", tc.name)
			}
		})
	}
}

// TestValidateHandshakeDeliveryCutoverIsTwoSided pins that a truthful welcome
// opt-in requires the peer's exact compiled strong capability; preparation and
// regional reporting never substitute for it.
func TestValidateHandshakeDeliveryCutoverIsTwoSided(t *testing.T) {
	expect := HandshakeExpectation{HubID: "11111111-2222-4333-8444-555555555555", ProbeID: "21111111-2222-4333-8444-555555555555", StreamID: "31111111-2222-4333-8444-555555555555", ConnectionGeneration: Decimal(1), RequiredCapabilities: []string{"snapshot.v1"}}
	hello, welcome := cutoverHandshakeFrames(t, []string{"snapshot.v1", DeliveryPreparationCapability, RegionalApplicationCapability}, true)
	if _, err := ValidateHandshake(hello, welcome, expect); !errors.Is(err, ErrUnsupportedCapability) {
		t.Fatalf("opt-in without the strong capability: %v", err)
	}
	hello, welcome = cutoverHandshakeFrames(t, []string{"snapshot.v1", DeliveryCutoverCapability}, true)
	if _, err := ValidateHandshake(hello, welcome, expect); err != nil {
		t.Fatal("negotiated handshake refused", err)
	}
	hello, welcome = cutoverHandshakeFrames(t, []string{"snapshot.v1", DeliveryCutoverCapability}, false)
	if _, err := ValidateHandshake(hello, welcome, expect); err != nil {
		t.Fatal("unnegotiated drain handshake refused", err)
	}
}

// TestProbeCommandMetadataCutoverKinds pins the immutable control scope of the
// new kinds: assignment-scoped, incident-free metadata matching the target
// generation, exactly like delivery.prepare.
func TestProbeCommandMetadataCutoverKinds(t *testing.T) {
	generation := int64(7)
	for _, kind := range []string{domain.DeliveryQuiesceKind, domain.DeliveryApplyKind, domain.DeliveryResumeKind} {
		metadata := domain.ProbeCommandMetadata{CommandID: "41111111-2222-4333-8444-555555555555", HubID: "11111111-2222-4333-8444-555555555555", ProbeID: "21111111-2222-4333-8444-555555555555", StreamID: "31111111-2222-4333-8444-555555555555", Kind: kind, AssignmentGeneration: &generation, CreatedAt: time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC), ExpiresAt: time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC), PayloadSHA256: strings.Repeat("ab", 32)}
		if !domain.ValidProbeCommandMetadata(metadata) {
			t.Fatalf("%s metadata refused", kind)
		}
		incident := "71111111-2222-4333-8444-555555555555"
		withIncident := metadata
		withIncident.SourceAlertID = &incident
		if domain.ValidProbeCommandMetadata(withIncident) {
			t.Fatalf("%s metadata accepted a source incident", kind)
		}
		withoutGeneration := metadata
		withoutGeneration.AssignmentGeneration = nil
		if domain.ValidProbeCommandMetadata(withoutGeneration) {
			t.Fatalf("%s metadata accepted no assignment generation", kind)
		}
	}
}
