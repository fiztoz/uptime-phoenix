package services

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type connectorRotations struct {
	ports.ProbeCredentialRotationRepository
	selection             domain.ProbeCredentialSelection
	selectErr, confirmErr error
	selected              domain.ProbeReplaySession
	confirmed             []int64
}

func (r *connectorRotations) SelectCredentialConnection(_ context.Context, session domain.ProbeReplaySession) (domain.ProbeCredentialSelection, error) {
	r.selected = session
	return r.selection, r.selectErr
}

func (r *connectorRotations) ConfirmCredentialConnection(_ context.Context, session domain.ProbeReplaySession, m domain.ProbeCredentialMetadata) error {
	if session != r.selected {
		return ports.ErrConflict
	}
	r.confirmed = append(r.confirmed, m.CredentialVersion)
	return r.confirmErr
}

func TestProbeConnectorCredentialFallback(t *testing.T) {
	for _, tc := range []struct {
		name       string
		failure    error
		ready      bool
		confirmErr error
		want       []int64
		confirmed  []int64
	}{
		{name: "HTTP401BeforeAdmission", failure: domain.ErrProbeCredentialRejected, want: []int64{2, 1}, confirmed: []int64{1}},
		{name: "NetworkFailure", failure: errors.New("network failed"), want: []int64{2}},
		{name: "GenericAuthorizationFailure", failure: domain.ErrUnauthorized, want: []int64{2}},
		{name: "HealthyCandidate", ready: true, want: []int64{2}, confirmed: []int64{2}},
		{name: "UnauthorizedConfirmation", ready: true, confirmErr: domain.ErrUnauthorized, want: []int64{2}, confirmed: []int64{2}},
		{name: "RejectedAfterEstablishment", ready: true, failure: domain.ErrProbeCredentialRejected, want: []int64{2}, confirmed: []int64{2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			connections, leases := &connectorConnections{}, &connectorLeases{}
			var seen []int64
			svc := newConnectorTestService(t, connections, leases, connectorTransport{run: func(ctx context.Context, input domain.ProbeSessionInput, ready func(context.Context) error) error {
				seen = append(seen, input.Connection.CredentialVersion)
				if input.Generation != 7 {
					t.Fatal("fallback escaped lease generation")
				}
				if tc.ready || input.Connection.CredentialVersion == 1 {
					if err := ready(ctx); err != nil {
						return err
					}
				}
				return tc.failure
			}})
			prepareConnectorTest(t, svc)
			current := *connections.value
			current.State = "active"
			candidate := current
			candidate.CredentialVersion = 2
			rotations := &connectorRotations{selection: domain.ProbeCredentialSelection{Current: current, Candidate: &candidate, RotationID: "abababab-abab-4bab-8bab-abababababab"}, confirmErr: tc.confirmErr}
			svc.SetCredentialRotation(rotations)
			_ = connectorTestOnce(svc, t.Context(), current.ProbeID)
			if !reflect.DeepEqual(seen, tc.want) || !reflect.DeepEqual(rotations.confirmed, tc.confirmed) || connections.activated != 0 || leases.released.Load() != 7 {
				t.Fatalf("incorrect candidate/fallback lifecycle: calls=%v confirmations=%v activation=%d", seen, rotations.confirmed, connections.activated)
			}
		})
	}
}

func TestProbeConnectorCredentialSelectionFailureStopsNetwork(t *testing.T) {
	connections, leases := &connectorConnections{}, &connectorLeases{}
	svc := newConnectorTestService(t, connections, leases, connectorTransport{run: func(context.Context, domain.ProbeSessionInput, func(context.Context) error) error {
		t.Fatal("failed fenced selection reached network")
		return nil
	}})
	prepareConnectorTest(t, svc)
	svc.SetCredentialRotation(&connectorRotations{selectErr: ports.ErrConflict})
	if err := connectorTestOnce(svc, t.Context(), connections.value.ProbeID); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("selection failure lost", err)
	}
	if leases.released.Load() != 7 {
		t.Fatal("selection failure leaked lease")
	}
}
