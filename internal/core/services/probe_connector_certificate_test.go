package services

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type connectorCertificates struct {
	ports.ProbeCertificateRotationRepository
	selection  domain.ProbeCertificateSelection
	next       *domain.ProbeCertificateSelection
	calls      int
	confirmed  []string
	confirmErr error
}

func (r *connectorCertificates) SelectCertificateConnection(context.Context, domain.ProbeReplaySession) (domain.ProbeCertificateSelection, error) {
	r.calls++
	if r.calls > 1 && r.next != nil {
		return *r.next, nil
	}
	return r.selection, nil
}
func (r *connectorCertificates) ConfirmCertificateConnection(_ context.Context, _ domain.ProbeReplaySession, metadata domain.ProbeCredentialMetadata, pin string) error {
	if metadata != r.selection.Current {
		return ports.ErrConflict
	}
	r.confirmed = append(r.confirmed, pin)
	return r.confirmErr
}
func TestProbeConnectorCertificateFallback(t *testing.T) {
	candidate := strings.Repeat("b", 64)
	for _, tc := range []struct {
		name                                                      string
		failure                                                   error
		ready, retired, changed, expiredDuringDial, confirmFailed bool
		wantFallback                                              bool
	}{
		{name: "PinMismatchBeforeHTTP", failure: domain.ErrProbeCertificateMismatch, wantFallback: true},
		{name: "NetworkFailure", failure: errors.New("network failed")},
		{name: "HTTP401", failure: domain.ErrProbeCredentialRejected},
		{name: "HealthyCandidate", ready: true},
		{name: "MismatchAfterAdmission", ready: true, failure: domain.ErrProbeCertificateMismatch},
		{name: "RetiredBeforeDial", retired: true, failure: domain.ErrProbeCertificateMismatch},
		{name: "RetiredDuringDial", expiredDuringDial: true, failure: domain.ErrProbeCertificateMismatch},
		{name: "SelectionChangedDuringDial", changed: true, failure: domain.ErrProbeCertificateMismatch},
		{name: "ConfirmationRejected", ready: true, confirmFailed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			connections, leases := &connectorConnections{}, &connectorLeases{}
			var seen []string
			var original domain.ProbeCredentialMetadata
			deadline := time.Now().Add(time.Minute).UTC()
			svc := newConnectorTestService(t, connections, leases, connectorTransport{run: func(ctx context.Context, in domain.ProbeSessionInput, ready func(context.Context) error) error {
				seen = append(seen, in.DialFingerprint)
				if in.Connection != original || in.Generation != 7 {
					t.Fatal("TLS selection changed protected credential scope or lease")
				}
				if in.DialFingerprint != candidate {
					actual, ok := ctx.Deadline()
					if !ok || !actual.Equal(deadline) {
						t.Fatal("fallback escaped fixed retirement deadline")
					}
				}
				if tc.ready || in.DialFingerprint != candidate {
					if err := ready(ctx); err != nil {
						return err
					}
				}
				return tc.failure
			}})
			prepareConnectorTest(t, svc)
			current := *connections.value
			current.State = "active"
			original = current.ProbeCredentialMetadata
			svc.SetCredentialRotation(&connectorRotations{selection: domain.ProbeCredentialSelection{Current: current}})
			certs := &connectorCertificates{selection: domain.ProbeCertificateSelection{Current: original, CandidateFingerprint: candidate, AllowCurrentFallback: !tc.retired, FallbackUntil: deadline}}
			if tc.expiredDuringDial || tc.changed {
				next := certs.selection
				if tc.expiredDuringDial {
					next.AllowCurrentFallback = false
				}
				if tc.changed {
					next.CandidateFingerprint = strings.Repeat("c", 64)
				}
				certs.next = &next
			}
			if tc.confirmFailed {
				certs.confirmErr = ports.ErrConflict
			}
			svc.SetCertificateRotation(certs)
			_ = connectorTestOnce(svc, t.Context(), current.ProbeID)
			want := []string{candidate}
			if tc.wantFallback {
				want = append(want, current.Fingerprint)
			}
			if !reflect.DeepEqual(seen, want) || leases.released.Load() != 7 || connections.activated != 0 {
				t.Fatal("unexpected fallback or lease lifecycle", seen)
			}
			if tc.wantFallback && (certs.calls != 2 || !reflect.DeepEqual(certs.confirmed, []string{current.Fingerprint})) {
				t.Fatal("fallback was not freshly selected and confirmed", certs.calls, certs.confirmed)
			}
		})
	}
}
