package services

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

const regionalApplicationStream = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"

func regionalApplicationConfig(mode domain.AlertDelivery, binding int64) *domain.EdgeResolvedConfig {
	return &domain.EdgeResolvedConfig{Metadata: domain.ProbeConfigMetadata{
		ProbeConfigTarget: domain.ProbeConfigTarget{HubID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", ProbeID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc"},
		Revision:          2, SchemaVersion: 1, SHA256: strings.Repeat("a", 64), CreatedAt: time.Unix(100, 0).UTC(), EffectiveAt: time.Unix(101, 0).UTC(),
	}, Assignments: []domain.EdgeResolvedAssignment{{Monitor: &domain.Monitor{ID: 7, Active: true}, Generation: 3, AssignmentRevision: binding, DesiredAlertDelivery: mode}}}
}

type regionalApplicationPorts struct {
	config *domain.EdgeResolvedConfig
	active domain.EdgeActiveConfig
	calls  int
}

func (p *regionalApplicationPorts) ReadIdentity(context.Context) (domain.EdgeIdentity, error) {
	return domain.EdgeIdentity{HubID: p.config.Metadata.HubID, ProbeID: p.config.Metadata.ProbeID, StreamID: regionalApplicationStream, ConnectionGeneration: 4}, nil
}
func (*regionalApplicationPorts) AcceptConnectionGeneration(context.Context, string, int64) error {
	return nil
}
func (p *regionalApplicationPorts) DecodeEdge(context.Context, []byte, domain.ProbeConfigTarget) (*domain.EdgeResolvedConfig, error) {
	return p.config, nil
}
func (p *regionalApplicationPorts) ActivateConfig(_ context.Context, config domain.EdgeActiveConfig) error {
	p.active = config
	p.calls++
	return nil
}
func (p *regionalApplicationPorts) ReadActiveConfig(context.Context) (domain.EdgeActiveConfig, error) {
	return p.active, nil
}
func (*regionalApplicationPorts) Seal(context.Context, domain.ProbeConfigMetadata, []byte) ([]byte, error) {
	return []byte("fixture protected bytes"), nil
}
func (*regionalApplicationPorts) Open(context.Context, domain.ProbeConfigMetadata, []byte) ([]byte, error) {
	return []byte("fixture document bytes"), nil
}
func (*regionalApplicationPorts) KeyHash(string) string { return strings.Repeat("b", 64) }

func TestEdgeRegionalApplicationServiceDesiredModes(t *testing.T) {
	for _, mode := range []domain.AlertDelivery{"", domain.AlertDeliveryRegional, domain.AlertDeliveryAggregate, domain.AlertDeliveryBoth} {
		t.Run("desired_"+string(mode), func(t *testing.T) {
			p := &regionalApplicationPorts{config: regionalApplicationConfig(mode, 9007199254740993)}
			svc := NewEdgeConfigService(p, p, p, p)
			if _, err := svc.Apply(t.Context(), []byte("document"), 4, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			facts := p.active.RegionalDeliveryApplications
			if p.calls != 1 || len(facts) != 1 {
				t.Fatal("real Apply did not persist source application")
			}
			want := domain.ProbeDeliveryModeApplication{ProbeConfigTarget: p.config.Metadata.ProbeConfigTarget, MonitorID: 7, AssignmentRevision: 9007199254740993, Generation: 3, ConfigRevision: 2, SHA256: p.config.Metadata.SHA256, StreamID: regionalApplicationStream, AlertDelivery: domain.AlertDeliveryRegional}
			if facts[0] != want || p.config.Assignments[0].DesiredAlertDelivery != mode {
				t.Fatal("effective regional mode was inferred from desired mode or mutated the document")
			}
			if _, err := svc.Load(t.Context()); err != nil {
				t.Fatal("cold Load rejected exact application", err)
			}
		})
	}
	for _, mode := range []domain.AlertDelivery{domain.AlertDeliveryRegional, domain.AlertDeliveryAggregate, domain.AlertDeliveryBoth} {
		p := &regionalApplicationPorts{config: regionalApplicationConfig(mode, 0)}
		if _, err := NewEdgeConfigService(p, p, p, p).Apply(t.Context(), []byte("document"), 4, time.Now().UTC()); err != nil || len(p.active.RegionalDeliveryApplications) != 0 {
			t.Fatal("legacy binding was fabricated", err)
		}
	}
}

func TestEdgeRegionalApplicationColdValidation(t *testing.T) {
	mutations := map[string]func(*domain.ProbeDeliveryModeApplication){
		"hub":               func(a *domain.ProbeDeliveryModeApplication) { a.HubID = regionalApplicationStream },
		"probe":             func(a *domain.ProbeDeliveryModeApplication) { a.ProbeID = regionalApplicationStream },
		"monitor":           func(a *domain.ProbeDeliveryModeApplication) { a.MonitorID++ },
		"set_revision":      func(a *domain.ProbeDeliveryModeApplication) { a.AssignmentRevision++ },
		"generation":        func(a *domain.ProbeDeliveryModeApplication) { a.Generation++ },
		"document_revision": func(a *domain.ProbeDeliveryModeApplication) { a.ConfigRevision++ },
		"hash":              func(a *domain.ProbeDeliveryModeApplication) { a.SHA256 = strings.Repeat("c", 64) },
		"aggregate":         func(a *domain.ProbeDeliveryModeApplication) { a.AlertDelivery = domain.AlertDeliveryAggregate },
		"both":              func(a *domain.ProbeDeliveryModeApplication) { a.AlertDelivery = domain.AlertDeliveryBoth },
		"unbound":           func(a *domain.ProbeDeliveryModeApplication) { a.AssignmentRevision = 0 },
		"invalid_stream":    func(a *domain.ProbeDeliveryModeApplication) { a.StreamID = "invalid" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			p := &regionalApplicationPorts{config: regionalApplicationConfig(domain.AlertDeliveryAggregate, 3)}
			svc := NewEdgeConfigService(p, p, p, p)
			if _, err := svc.Apply(t.Context(), []byte("document"), 4, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			mutate(&p.active.RegionalDeliveryApplications[0])
			if _, err := svc.Load(t.Context()); !errors.Is(err, domain.ErrValidation) {
				t.Fatal("cold Load accepted foreign or corrupt application", err)
			}
		})
	}
	p := &regionalApplicationPorts{config: regionalApplicationConfig(domain.AlertDeliveryBoth, 3)}
	svc := NewEdgeConfigService(p, p, p, p)
	if _, err := svc.Apply(t.Context(), []byte("document"), 4, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	p.active.RegionalDeliveryApplications = nil
	if _, err := svc.Load(t.Context()); err != nil {
		t.Fatal("pre-upgrade absence must stay regional and unreported", err)
	}
}
