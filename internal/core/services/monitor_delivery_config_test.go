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

const deliveryEvidenceHub = "22222222-2222-4222-8222-222222222222"
const deliveryEvidenceRemote = "11111111-1111-4111-8111-111111111111"

type deliveryEvidenceReader struct {
	document *domain.AcknowledgedProbeDeliveryConfig
	err      error
	targets  []domain.ProbeConfigTarget
	onRead   func()
}

func (r *deliveryEvidenceReader) ReadAcknowledgedRemoteDelivery(_ context.Context, target domain.ProbeConfigTarget) (*domain.AcknowledgedProbeDeliveryConfig, error) {
	r.targets = append(r.targets, target)
	if r.onRead != nil {
		r.onRead()
	}
	return r.document, r.err
}

func deliveryEvidenceFixture(t *testing.T) (*MonitorRegionalService, *deliveryEvidenceReader, *domain.MonitorProbeAssignments, *healthAccess) {
	t.Helper()
	set := &domain.MonitorProbeAssignments{MonitorID: 7, Revision: 3, HealthPolicy: domain.HealthPolicyAnyDown, AlertDelivery: domain.AlertDeliveryAggregate,
		Assignments: []domain.ProbeAssignment{{MonitorID: 7, ProbeID: deliveryEvidenceRemote, Generation: 2}, {MonitorID: 7, ProbeID: domain.LocalProbeID, Generation: 1}}}
	access := &healthAccess{allow: map[int64]bool{7: true}}
	health := NewMonitorHealthService(healthMonitorRepo{monitors: map[int64]*domain.Monitor{7: {ID: 7, Active: true, Interval: 60, Timeout: 5}}},
		healthAssignmentRepo{sets: map[int64]*domain.MonitorProbeAssignments{7: set}}, &healthRegionalRepo{}, access)
	svc := NewMonitorRegionalService(health, &regionalLabelRepo{})
	reader := &deliveryEvidenceReader{document: &domain.AcknowledgedProbeDeliveryConfig{Target: domain.ProbeConfigTarget{HubID: deliveryEvidenceHub, ProbeID: deliveryEvidenceRemote}, Revision: 9007199254740993,
		Assignments: []domain.AcknowledgedAssignmentDelivery{{MonitorID: 8, Generation: 99, AlertDelivery: domain.AlertDeliveryBoth}, {MonitorID: 7, Generation: 2, AlertDelivery: domain.AlertDeliveryRegional}}}}
	if err := svc.SetDeliveryConfigReader(deliveryEvidenceHub, reader); err != nil {
		t.Fatal(err)
	}
	return svc, reader, set, access
}

func TestAcknowledgedDeliveryConfigsSelection(t *testing.T) {
	svc, reader, set, _ := deliveryEvidenceFixture(t)
	before := append([]domain.AcknowledgedAssignmentDelivery(nil), reader.document.Assignments...)
	result, err := svc.AcknowledgedDeliveryConfigs(t.Context(), 1, 7, 3, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	remote := result[deliveryEvidenceRemote]
	if len(result) != 2 || remote.State != DeliveryConfigAcknowledged || remote.ConfigRevision != 9007199254740993 || remote.Generation != 2 || remote.AlertDelivery != domain.AlertDeliveryRegional {
		t.Fatalf("wrong scoped document evidence: %+v", result)
	}
	if result[domain.LocalProbeID].State != DeliveryConfigLocalUnavailable || len(reader.targets) != 1 || reader.targets[0] != reader.document.Target {
		t.Fatal("local or unrelated source was read")
	}
	if set.AlertDelivery != domain.AlertDeliveryAggregate || !reflect.DeepEqual(before, reader.document.Assignments) {
		t.Fatal("read changed desired state or reader-owned evidence")
	}
}

func TestAcknowledgedDeliveryConfigsEvidenceStates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*deliveryEvidenceReader)
		state  string
		err    error
	}{
		{"unreported", func(r *deliveryEvidenceReader) { r.err = ports.ErrNotFound }, DeliveryConfigUnreported, nil},
		{"unverified", func(r *deliveryEvidenceReader) { r.err = ports.ErrConflict }, DeliveryConfigUnverified, nil},
		{"assignment missing", func(r *deliveryEvidenceReader) { r.document.Assignments = r.document.Assignments[:1] }, DeliveryConfigAssignmentMissing, nil},
		{"generation mismatch", func(r *deliveryEvidenceReader) { r.document.Assignments[1].Generation++ }, DeliveryConfigGenerationMismatch, nil},
		{"foreign target", func(r *deliveryEvidenceReader) { r.document.Target.HubID = deliveryEvidenceRemote }, "", domain.ErrInternal},
		{"zero revision", func(r *deliveryEvidenceReader) { r.document.Revision = 0 }, "", domain.ErrInternal},
		{"nil document", func(r *deliveryEvidenceReader) { r.document = nil }, "", domain.ErrInternal},
		{"invalid mode", func(r *deliveryEvidenceReader) { r.document.Assignments[1].AlertDelivery = "private-invalid-mode" }, "", domain.ErrInternal},
		{"duplicate identity", func(r *deliveryEvidenceReader) {
			r.document.Assignments = append(r.document.Assignments, r.document.Assignments[1])
		}, "", domain.ErrInternal},
		{"confidential error", func(r *deliveryEvidenceReader) { r.err = errors.New("private-decoder-details") }, "", domain.ErrInternal},
		{"canceled source", func(r *deliveryEvidenceReader) { r.err = context.Canceled }, "", context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, reader, _, _ := deliveryEvidenceFixture(t)
			tc.change(reader)
			result, err := svc.AcknowledgedDeliveryConfigs(t.Context(), 1, 7, 3, time.Now().UTC())
			if tc.err != nil {
				if !errors.Is(err, tc.err) || result != nil || strings.Contains(err.Error(), "private") {
					t.Fatal("invalid document/error did not fail closed with redaction")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			entry := result[deliveryEvidenceRemote]
			if entry.State != tc.state || entry.ConfigRevision != 0 || entry.Generation != 0 || entry.AlertDelivery != "" {
				t.Fatalf("unproven evidence claimed acknowledgement: %+v", entry)
			}
		})
	}
}

func TestAcknowledgedDeliveryConfigsScopeAndConcurrentChanges(t *testing.T) {
	for _, scenario := range []string{"hidden", "stale initial revision", "revision changes", "grant revoked", "cancellation"} {
		t.Run(scenario, func(t *testing.T) {
			svc, reader, set, access := deliveryEvidenceFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			wantErr := ports.ErrNotFound
			expected := int64(3)
			switch scenario {
			case "hidden":
				access.allow[7] = false
			case "stale initial revision":
				expected++
				wantErr = ErrAssignmentReadChanged
			case "revision changes":
				reader.onRead = func() { set.Revision++ }
				wantErr = ErrAssignmentReadChanged
			case "grant revoked":
				reader.onRead = func() { access.allow[7] = false }
			case "cancellation":
				reader.onRead = cancel
				wantErr = context.Canceled
			}
			result, err := svc.AcknowledgedDeliveryConfigs(ctx, 1, 7, expected, time.Now().UTC())
			if !errors.Is(err, wantErr) || result != nil {
				t.Fatalf("mixed/unauthorized read escaped: %v", err)
			}
			if (scenario == "hidden" || scenario == "stale initial revision") && len(reader.targets) != 0 {
				t.Fatal("denied/stale read touched a configuration document")
			}
		})
	}
}

func TestAcknowledgedDeliveryConfigsUnavailableAndLegacyMode(t *testing.T) {
	svc, reader, _, access := deliveryEvidenceFixture(t)
	svc.delivery = nil
	if result, err := svc.AcknowledgedDeliveryConfigs(t.Context(), 1, 7, 3, time.Now().UTC()); result != nil || !errors.Is(err, domain.ErrInternal) {
		t.Fatal("unwired evidence returned success")
	}
	access.allow[7] = false
	if _, err := svc.AcknowledgedDeliveryConfigs(t.Context(), 1, 7, 3, time.Now().UTC()); !errors.Is(err, ports.ErrNotFound) {
		t.Fatal("unavailable wiring disclosed a hidden monitor")
	}
	access.allow[7] = true
	if err := svc.SetDeliveryConfigReader("invalid-hub", reader); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("untrusted hub identity accepted")
	}
	if err := svc.SetDeliveryConfigReader(deliveryEvidenceHub, reader); err != nil {
		t.Fatal(err)
	}
	reader.document.Assignments[1].AlertDelivery = ""
	result, err := svc.AcknowledgedDeliveryConfigs(t.Context(), 1, 7, 3, time.Now().UTC())
	if err != nil || result[deliveryEvidenceRemote].AlertDelivery != domain.AlertDeliveryRegional {
		t.Fatal("legacy absence did not remain regional", err)
	}
}
