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

type localPreparationReaderFake struct {
	values []domain.LocalDeliveryPreparation
	err    error
	calls  int
	want   domain.ProbeActiveConfig
}

var _ ports.LocalDeliveryPreparationReader = (*localPreparationReaderFake)(nil)

func (r *localPreparationReaderFake) ReadLocalDeliveryPreparations(ctx context.Context, active domain.ProbeActiveConfig) ([]domain.LocalDeliveryPreparation, error) {
	r.calls++
	r.want = active
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return r.values, r.err
}

func localPreparationCompletenessFixture() (domain.ProbeActiveConfig, *domain.LocalProbeConfigDefinition, []domain.LocalDeliveryPreparation) {
	active := domain.ProbeActiveConfig{ProbeConfigTarget: domain.ProbeConfigTarget{HubID: "11111111-2222-4333-8444-555555555555", ProbeID: domain.LocalProbeID}, Revision: 3, SHA256: strings.Repeat("1", 64), AppliedAt: time.Now().UTC(), AssignmentCount: 2}
	definition := &domain.LocalProbeConfigDefinition{Target: active.ProbeConfigTarget, Revision: 3, Assignments: []domain.ProbeConfigAssignment{{Monitor: &domain.Monitor{ID: 7}, Generation: 1, AssignmentRevision: 9007199254740993, AlertDelivery: domain.AlertDeliveryBoth}, {Monitor: &domain.Monitor{ID: 8}, Generation: 2, AssignmentRevision: 7, AlertDelivery: domain.AlertDeliveryRegional}}}
	p := domain.LocalDeliveryPreparation{ProbeDeliveryModeApplication: domain.ProbeDeliveryModeApplication{ProbeConfigTarget: active.ProbeConfigTarget, MonitorID: 7, AssignmentRevision: 9007199254740993, Generation: 1, ConfigRevision: 3, SHA256: active.SHA256, StreamID: domain.LocalStreamID, AlertDelivery: domain.AlertDeliveryBoth}, PreparedAt: time.Now().UTC().Truncate(time.Microsecond)}
	return active, definition, []domain.LocalDeliveryPreparation{p}
}

func TestLocalDeliveryPreparationCompleteness(t *testing.T) {
	active, definition, ps := localPreparationCompletenessFixture()
	before := append([]domain.LocalDeliveryPreparation(nil), ps...)
	if ok, err := completeLocalDeliveryPreparations(active, definition, ps); !ok || err != nil {
		t.Fatal("independent large set binding rejected", err)
	}
	if !reflect.DeepEqual(before, ps) {
		t.Fatal("borrowed request slice changed")
	}
	if ok, err := completeLocalDeliveryPreparations(active, definition, nil); ok || err != nil {
		t.Fatal("missing request not a new-publication upgrade", err)
	}
	for name, change := range map[string]func(*domain.LocalDeliveryPreparation){
		"target":     func(p *domain.LocalDeliveryPreparation) { p.HubID = "22222222-3333-4444-8555-666666666666" },
		"probe":      func(p *domain.LocalDeliveryPreparation) { p.ProbeID = p.HubID },
		"monitor":    func(p *domain.LocalDeliveryPreparation) { p.MonitorID++ },
		"binding":    func(p *domain.LocalDeliveryPreparation) { p.AssignmentRevision++ },
		"generation": func(p *domain.LocalDeliveryPreparation) { p.Generation++ },
		"document":   func(p *domain.LocalDeliveryPreparation) { p.ConfigRevision++ },
		"hash":       func(p *domain.LocalDeliveryPreparation) { p.SHA256 = strings.Repeat("2", 64) },
		"stream":     func(p *domain.LocalDeliveryPreparation) { p.StreamID = p.HubID },
		"mode":       func(p *domain.LocalDeliveryPreparation) { p.AlertDelivery = domain.AlertDeliveryAggregate },
		"time":       func(p *domain.LocalDeliveryPreparation) { p.PreparedAt = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			q := ps[0]
			change(&q)
			if ok, err := completeLocalDeliveryPreparations(active, definition, []domain.LocalDeliveryPreparation{q}); ok || !errors.Is(err, ports.ErrConflict) {
				t.Fatal("present corruption silently accepted/repaired", err)
			}
		})
	}
	if ok, err := completeLocalDeliveryPreparations(active, definition, append(ps, ps[0])); ok || !errors.Is(err, ports.ErrConflict) {
		t.Fatal("duplicate accepted", err)
	}
	definition.Assignments[0].AlertDelivery = domain.AlertDeliveryRegional
	if ok, err := completeLocalDeliveryPreparations(active, definition, nil); !ok || err != nil {
		t.Fatal("regional member needs fabricated request", err)
	}
	empty := &domain.LocalProbeConfigDefinition{Target: active.ProbeConfigTarget, Revision: active.Revision}
	if ok, err := completeLocalDeliveryPreparations(active, empty, nil); !ok || err != nil {
		t.Fatal("empty membership needs request", err)
	}
}

type fallbackPreparationActivation struct {
	*refFakeAppliedReaderActivationRepo
	current domain.ProbeActiveConfig
	delayed bool
	calls   int
}

func (r *fallbackPreparationActivation) GetActive(context.Context, string) (*domain.ProbeActiveConfig, error) {
	r.calls++
	if r.delayed && r.calls <= 3 {
		return nil, ports.ErrNotFound
	}
	return &r.current, nil
}

// TestLocalDeliveryPreparationRefreshFastPaths refuses missing requests at both
// the ordinary early return and the final concurrent-writer fallback.
func TestLocalDeliveryPreparationRefreshFastPaths(t *testing.T) {
	for _, delayed := range []bool{false, true} {
		name := "ordinary"
		if delayed {
			name = "fallback"
		}
		t.Run(name, func(t *testing.T) {
			active, definition, _ := localPreparationCompletenessFixture()
			for _, member := range definition.Assignments {
				definition.RegionalDeliveryApplications = append(definition.RegionalDeliveryApplications, domain.ProbeDeliveryModeApplication{ProbeConfigTarget: active.ProbeConfigTarget, MonitorID: member.Monitor.ID, AssignmentRevision: member.AssignmentRevision, Generation: member.Generation, ConfigRevision: active.Revision, SHA256: active.SHA256, StreamID: domain.LocalStreamID, AlertDelivery: domain.AlertDeliveryRegional})
			}
			activation := &fallbackPreparationActivation{refFakeAppliedReaderActivationRepo: &refFakeAppliedReaderActivationRepo{actFakeActivationRepo: newActFakeActivationRepo(), applied: definition}, current: active, delayed: delayed}
			prepared := NewProbeConfigService(&actFakeConfigRepo{}, &actFakeInspector{}, actFakeProtector{})
			validation := NewLocalProbeConfigValidationService(prepared, actFakeValidator{})
			s := NewLocalProbeConfigRefreshService(&refFakeSourceRepo{err: ports.ErrConflict}, &refFakeEncoder{}, prepared, NewLocalProbeConfigActivationService(validation, activation), activation, &refFakeInstallationRepo{inst: &domain.ProbeInstallation{HubID: active.HubID}})
			reader := &localPreparationReaderFake{}
			s.SetDeliveryPreparations(reader)
			if got, err := s.Refresh(t.Context()); got != nil || !errors.Is(err, ports.ErrConflict) {
				t.Fatal("missing pending request returned a complete configuration", err)
			}
			want := 4
			if delayed {
				want = 1
			}
			if reader.calls != want {
				t.Fatal("required refresh paths did not consult pending state")
			}
		})
	}
}

func TestLocalDeliveryPreparationReaderSeam(t *testing.T) {
	active, definition, ps := localPreparationCompletenessFixture()
	s := &LocalProbeConfigRefreshService{}
	if ok, err := s.localPreparationsComplete(t.Context(), active, definition); !ok || err != nil {
		t.Fatal("unconfigured caller changed", err)
	}
	fake := &localPreparationReaderFake{values: ps}
	s.SetDeliveryPreparations(fake)
	if ok, err := s.localPreparationsComplete(t.Context(), active, definition); !ok || err != nil || fake.calls != 1 || fake.want != active {
		t.Fatal("reader not bound to exact active tuple", err)
	}
	fake.err = ports.ErrConflict
	if ok, err := s.localPreparationsComplete(t.Context(), active, definition); ok || !errors.Is(err, ports.ErrConflict) {
		t.Fatal("corruption hidden", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if ok, err := s.localPreparationsComplete(canceled, active, definition); ok || !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored", err)
	}
}
