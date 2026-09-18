package services

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func localConfigSourceFixture() *domain.LocalProbeConfigSource {
	parent, child, direct, template, proxy := int64(10), int64(11), int64(20), int64(30), int64(40)
	return &domain.LocalProbeConfigSource{
		Probe: domain.Probe{ID: domain.LocalProbeID, Kind: domain.ProbeKindLocal, Enabled: true},
		Assignments: []domain.ProbeConfigAssignment{
			{Generation: 3, Monitor: &domain.Monitor{ID: 2, Name: "paused", GroupID: &child, Owner: "fallback", InheritGroupOwner: true}},
			{Generation: 1, Monitor: &domain.Monitor{ID: 1, GroupID: &child, ProxyID: &proxy}, NotificationLinks: []domain.MonitorNotification{{MonitorID: 1, NotificationID: 101, IncludeTarget: false}}},
		},
		Groups:          map[int64]*domain.MonitorGroup{10: {ID: 10, Owner: "parent owner"}, 11: {ID: 11, ParentID: &parent}},
		MonitorPolicies: map[int64]int64{1: direct}, GroupPolicies: map[int64]int64{10: 21, 11: 22},
		Policies: map[int64]*domain.EscalationPolicy{
			20: {ID: 20, Enabled: false, Steps: []domain.EscalationStep{{StepOrder: 1, NotificationIDs: []int64{102}}}},
			21: {ID: 21, Enabled: true, Steps: []domain.EscalationStep{{StepOrder: 1, NotificationIDs: []int64{999}}}},
			22: {ID: 22, Enabled: true, Steps: []domain.EscalationStep{{StepOrder: 1, NotificationIDs: []int64{103}}}},
		},
		Notifications: map[int64]*domain.Notification{101: {ID: 101, TemplateID: &template}, 102: {ID: 102}, 103: {ID: 103}, 999: {ID: 999}},
		Templates:     map[int64]*domain.NotificationTemplate{30: {ID: 30}, 999: {ID: 999}},
		Proxies:       map[int64]*domain.Proxy{40: {ID: 40}},
		Maintenance: []domain.ProbeConfigMaintenance{
			{Window: &domain.MaintenanceWindow{ID: 5}, MonitorIDs: []int64{2, 1, 999}},
			{Window: &domain.MaintenanceWindow{ID: 6}},
			{Window: &domain.MaintenanceWindow{ID: 7}, MonitorIDs: []int64{999}},
		},
	}
}

func TestLocalConfigResolutionPreservesOwnershipAndClosure(t *testing.T) {
	source := localConfigSourceFixture()
	out, err := resolveLocalProbeConfig(source)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Assignments) != 2 || out.Assignments[0].Monitor.ID != 1 || out.Assignments[1].Generation != 3 || out.Assignments[1].Monitor.Active || out.Assignments[1].EffectiveOwner != "parent owner" {
		t.Fatal("lost identity, pause, ordering or inherited owner")
	}
	if *out.Assignments[0].EscalationPolicyID != 20 || *out.Assignments[1].EscalationPolicyID != 22 || len(out.Policies) != 2 || out.Policies[0].Enabled {
		t.Fatal("changed disabled/direct/nearest policy precedence")
	}
	if len(out.Notifications) != 3 || out.Notifications[2].ID != 103 || len(out.Templates) != 1 || len(out.Proxies) != 1 || out.Assignments[0].NotificationLinks[0].IncludeTarget {
		t.Fatal("wrong channel closure or target preference")
	}
	if len(out.Maintenance) != 1 || !reflect.DeepEqual(out.Maintenance[0].MonitorIDs, []int64{1, 2}) || out.Maintenance[0].Window.Timezone != "UTC" || !reflect.DeepEqual(out.Assignments[1].MaintenanceIDs, []int64{5}) {
		t.Fatal("maintenance was not clipped or an unlinked window expanded")
	}
	if source.Maintenance[0].Window.Timezone != "" || source.Assignments[0].EffectiveOwner != "" || source.Assignments[0].MaintenanceIDs != nil {
		t.Fatal("resolution mutated source state")
	}
	// An empty direct policy remains an explicit inheritance stop.
	source.Policies[20].Steps = nil
	out, err = resolveLocalProbeConfig(source)
	if err != nil || *out.Assignments[0].EscalationPolicyID != 20 || len(out.Notifications) != 2 {
		t.Fatal("empty policy inherited an ancestor")
	}
}

func TestLocalConfigResolutionFailsMissingReferences(t *testing.T) {
	for name, mutate := range map[string]func(*domain.LocalProbeConfigSource){
		"registration": func(s *domain.LocalProbeConfigSource) { s.Probe.Enabled = false },
		"remote":       func(s *domain.LocalProbeConfigSource) { s.Probe.ID = "11111111-1111-4111-8111-111111111111" },
		"generation":   func(s *domain.LocalProbeConfigSource) { s.Assignments[0].Generation = 0 },
		"duplicate":    func(s *domain.LocalProbeConfigSource) { s.Assignments = append(s.Assignments, s.Assignments[0]) },
		"group":        func(s *domain.LocalProbeConfigSource) { delete(s.Groups, 11) },
		"cycle": func(s *domain.LocalProbeConfigSource) {
			id := int64(11)
			s.Groups[10].ParentID = &id
			s.GroupPolicies = nil
		},
		"policy":   func(s *domain.LocalProbeConfigSource) { delete(s.Policies, 20) },
		"channel":  func(s *domain.LocalProbeConfigSource) { delete(s.Notifications, 102) },
		"template": func(s *domain.LocalProbeConfigSource) { delete(s.Templates, 30) },
		"proxy":    func(s *domain.LocalProbeConfigSource) { delete(s.Proxies, 40) },
		"link":     func(s *domain.LocalProbeConfigSource) { s.Assignments[1].NotificationLinks[0].MonitorID = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			s := localConfigSourceFixture()
			mutate(s)
			if _, err := resolveLocalProbeConfig(s); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("invalid graph accepted: %v", err)
			}
		})
	}
}

type localConfigSourceFake struct {
	source *domain.LocalProbeConfigSource
	err    error
	reads  int
}

func (f *localConfigSourceFake) ReadLocal(context.Context) (*domain.LocalProbeConfigSource, error) {
	f.reads++
	return f.source, f.err
}

type localConfigEncoderFake struct {
	definition domain.LocalProbeConfigDefinition
	err        error
	calls      int
}

func (f *localConfigEncoderFake) EncodeLocal(d domain.LocalProbeConfigDefinition) ([]byte, error) {
	f.definition = d
	f.calls++
	return []byte("confidential-document"), f.err
}

func TestLocalConfigBuilderPreparationBoundary(t *testing.T) {
	ctx := context.Background()
	hub := "11111111-1111-4111-8111-111111111111"
	at := time.Date(2026, 9, 18, 12, 0, 0, 123456789, time.FixedZone("offset", 7*3600))
	source, encoder := &localConfigSourceFake{source: localConfigSourceFixture()}, &localConfigEncoderFake{}
	repo, protector := &configServiceRepo{}, &configServiceProtector{}
	b := NewLocalProbeConfigBuilder(source, encoder, NewProbeConfigService(repo, configServiceInspector{}, protector))
	meta, err := b.Prepare(ctx, hub, 6, at, at)
	if err != nil || meta.Revision != 7 || repo.saves != 1 || repo.expected != 6 || encoder.definition.Revision != 7 || encoder.definition.CreatedAt.Location() != time.UTC || encoder.definition.CreatedAt.Nanosecond() != 123456000 {
		t.Fatal("builder did not protect a versioned UTC snapshot")
	}
	for _, phase := range []string{"source", "encoder"} {
		t.Run(phase, func(t *testing.T) {
			source.err, encoder.err = nil, nil
			if phase == "source" {
				source.err = errors.New("secret-password")
			} else {
				encoder.err = errors.New("secret-password")
			}
			before := repo.saves
			got, err := b.Prepare(ctx, hub, 6, at, at)
			if !errors.Is(err, domain.ErrInternal) || strings.Contains(err.Error(), "secret-password") || got.Revision != 0 || repo.saves != before {
				t.Fatal("failed build leaked secret or wrote storage")
			}
		})
	}
	source.err, encoder.err = nil, nil
	reads := source.reads
	if _, err := b.Prepare(ctx, hub, math.MaxInt64, at, at); !errors.Is(err, ports.ErrConflict) || source.reads != reads {
		t.Fatal("revision exhaustion performed work")
	}
	if _, err := b.Prepare(ctx, "bad-hub", 0, at, at); !errors.Is(err, domain.ErrValidation) || source.reads != reads {
		t.Fatal("invalid trusted target performed work")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := b.Prepare(canceled, hub, 0, at, at); !errors.Is(err, context.Canceled) || source.reads != reads {
		t.Fatal("ignored cancellation")
	}
}
