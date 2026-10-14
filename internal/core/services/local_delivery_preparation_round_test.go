package services

import (
	"context"
	"errors"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type localRoundReconcilerFake struct {
	calls  int
	active domain.ProbeActiveConfig
	err    error
}

func (r *localRoundReconcilerFake) ReconcileLocalDeliveryPreparationRounds(ctx context.Context, a domain.ProbeActiveConfig) error {
	r.calls++
	r.active = a
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return r.err
}
func TestLocalDeliveryPreparationRoundFastPaths(t *testing.T) {
	for _, delayed := range []bool{false, true} {
		name := "ordinary"
		if delayed {
			name = "fallback"
		}
		for _, fail := range []bool{false, true} {
			suffix := "success"
			if fail {
				suffix = "refusal"
			}
			t.Run(name+"/"+suffix, func(t *testing.T) {
				active, definition, ps := localPreparationCompletenessFixture()
				for _, member := range definition.Assignments {
					definition.RegionalDeliveryApplications = append(definition.RegionalDeliveryApplications, domain.ProbeDeliveryModeApplication{ProbeConfigTarget: active.ProbeConfigTarget, MonitorID: member.Monitor.ID, AssignmentRevision: member.AssignmentRevision, Generation: member.Generation, ConfigRevision: active.Revision, SHA256: active.SHA256, StreamID: domain.LocalStreamID, AlertDelivery: domain.AlertDeliveryRegional})
				}
				activation := &fallbackPreparationActivation{refFakeAppliedReaderActivationRepo: &refFakeAppliedReaderActivationRepo{actFakeActivationRepo: newActFakeActivationRepo(), applied: definition}, current: active, delayed: delayed}
				prepared := NewProbeConfigService(&actFakeConfigRepo{}, &actFakeInspector{}, actFakeProtector{})
				validation := NewLocalProbeConfigValidationService(prepared, actFakeValidator{})
				s := NewLocalProbeConfigRefreshService(&refFakeSourceRepo{err: ports.ErrConflict}, &refFakeEncoder{}, prepared, NewLocalProbeConfigActivationService(validation, activation), activation, &refFakeInstallationRepo{inst: &domain.ProbeInstallation{HubID: active.HubID}})
				s.SetDeliveryPreparations(&localPreparationReaderFake{values: ps})
				rounds := &localRoundReconcilerFake{}
				if fail {
					rounds.err = ports.ErrConflict
				}
				s.SetPreparationRounds(rounds)
				got, err := s.Refresh(t.Context())
				if fail {
					if got != nil || !errors.Is(err, ports.ErrConflict) {
						t.Fatal("request refusal disguised as success", err)
					}
				} else if err != nil || got == nil || *got != active {
					t.Fatal("round reconciliation changed config", err)
				}
				if rounds.calls != 1 || rounds.active != active {
					t.Fatal("fast path skipped independently checked round selection")
				}
			})
		}
	}
}
