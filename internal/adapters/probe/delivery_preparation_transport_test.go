package probe

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type preparationIssuerSpy struct {
	calls  atomic.Int64
	called chan struct{}
}

var _ ports.RemoteDeliveryPreparationIssuer = (*preparationIssuerSpy)(nil)

func (s *preparationIssuerSpy) IssueRemoteDeliveryPreparations(context.Context, domain.ProbeReplaySession, []domain.ProbeDeliveryModeApplication) error {
	s.calls.Add(1)
	select {
	case s.called <- struct{}{}:
	default:
	}
	return nil
}

type preparationDispatchSpy struct{ calls atomic.Int64 }

var _ ports.ProbeCommandDispatcher = (*preparationDispatchSpy)(nil)

func (s *preparationDispatchSpy) NextCommand(context.Context, domain.ProbeReplaySession, time.Duration, domain.ProbeCommandCapabilities) (*domain.ProbeCommandDispatch, error) {
	s.calls.Add(1)
	return nil, nil
}
func (*preparationDispatchSpy) RecordCommandResult(context.Context, domain.ProbeReplaySession, domain.ProbeCommandOutcome) (bool, error) {
	return false, domain.ErrValidation
}

// TestDeliveryPreparationTransportGates isolates the real two-sided report and
// command negotiation from repository authority, which real-engine tests cover.
func TestDeliveryPreparationTransportGates(t *testing.T) {
	for _, tc := range []struct {
		name              string
		prepare, commands bool
	}{
		{"no_prepare_capability", false, true}, {"no_command_dispatcher", true, false}, {"negotiated", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caps := append(regionalBaseCapabilities(), RegionalApplicationCapability)
			if tc.prepare {
				caps = append(caps, DeliveryPreparationCapability)
			}
			report := &regionalRepoCapture{called: make(chan struct{}, 1)}
			h := newRegionalTransportHarness(t, domain.AlertDeliveryAggregate, caps, report)
			issuer := &preparationIssuerSpy{called: make(chan struct{}, 1)}
			h.transport.SetDeliveryPreparations(issuer)
			if tc.commands {
				h.transport.SetCommands(&preparationDispatchSpy{})
			}
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			done := h.run(ctx)
			joined := false
			defer func() {
				cancel()
				if !joined {
					select {
					case <-done:
					case <-time.After(3 * time.Second):
						t.Error("transport did not join")
					}
				}
			}()
			h.awaitReceipt(done)
			select {
			case <-report.called:
			case <-done:
				joined = true
				t.Fatal("session ended before regional report")
			case <-ctx.Done():
				t.Fatal("report missing")
			}
			// Exercise post-ACK application traffic to prove absent issuance is not a
			// canceled/dead session. These spies make no database acceptance claim.
			h.verifyPostACKTraffic()
			if tc.prepare && tc.commands {
				select {
				case <-issuer.called:
				case <-ctx.Done():
					t.Fatal("negotiated issuer not reached")
				}
			} else {
				time.Sleep(100 * time.Millisecond)
				if issuer.calls.Load() != 0 {
					t.Fatal("unnegotiated preparation issued")
				}
			}
			cancel()
			select {
			case <-done:
				joined = true
			case <-time.After(3 * time.Second):
				t.Fatal("transport did not join")
			}
		})
	}
}
