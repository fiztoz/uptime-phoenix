package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type localDeliveryEvidenceReader struct{ deliveryEvidenceReader }

func (r *localDeliveryEvidenceReader) ReadAcknowledgedLocalDelivery(ctx context.Context, target domain.ProbeConfigTarget) (*domain.AcknowledgedProbeDeliveryConfig, error) {
	return r.ReadAcknowledgedRemoteDelivery(ctx, target)
}

func TestAcknowledgedLocalDeliveryConfigs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*localDeliveryEvidenceReader)
		state  string
		err    error
	}{
		{"acknowledged", func(*localDeliveryEvidenceReader) {}, DeliveryConfigAcknowledged, nil},
		{"unreported", func(r *localDeliveryEvidenceReader) { r.err = ports.ErrNotFound }, DeliveryConfigUnreported, nil},
		{"unverified", func(r *localDeliveryEvidenceReader) { r.err = ports.ErrConflict }, DeliveryConfigUnverified, nil},
		{"generation mismatch", func(r *localDeliveryEvidenceReader) { r.document.Assignments[0].Generation++ }, DeliveryConfigGenerationMismatch, nil},
		{"assignment missing", func(r *localDeliveryEvidenceReader) { r.document.Assignments = nil }, DeliveryConfigAssignmentMissing, nil},
		{"private error", func(r *localDeliveryEvidenceReader) { r.err = errors.New("private-local-decoder-details") }, "", domain.ErrInternal},
		{"canceled", func(r *localDeliveryEvidenceReader) { r.err = context.Canceled }, "", context.Canceled},
		{"wrong target", func(r *localDeliveryEvidenceReader) { r.document.Target.ProbeID = deliveryEvidenceRemote }, "", domain.ErrInternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, remote, set, access := deliveryEvidenceFixture(t)
			set.Assignments = []domain.ProbeAssignment{{MonitorID: 7, ProbeID: domain.LocalProbeID, Generation: 1}}
			local := &localDeliveryEvidenceReader{deliveryEvidenceReader: deliveryEvidenceReader{document: &domain.AcknowledgedProbeDeliveryConfig{
				Target: domain.ProbeConfigTarget{HubID: deliveryEvidenceHub, ProbeID: domain.LocalProbeID}, Revision: 9007199254740993,
				Assignments: []domain.AcknowledgedAssignmentDelivery{{MonitorID: 7, Generation: 1, AlertDelivery: domain.AlertDeliveryRegional}},
			}}}
			tc.change(local)
			if err := svc.SetLocalDeliveryConfigReader(local); err != nil {
				t.Fatal(err)
			}
			result, err := svc.AcknowledgedDeliveryConfigs(t.Context(), 1, 7, 3, time.Now().UTC())
			if tc.err != nil {
				if !errors.Is(err, tc.err) || result != nil {
					t.Fatal("local failure escaped as evidence", err)
				}
			} else {
				if err != nil || len(result) != 1 || result[domain.LocalProbeID].State != tc.state {
					t.Fatal("wrong local state", err)
				}
				e := result[domain.LocalProbeID]
				if tc.state == DeliveryConfigAcknowledged {
					if e.Generation != 1 || e.ConfigRevision != 9007199254740993 || e.AlertDelivery != domain.AlertDeliveryRegional || set.AlertDelivery != domain.AlertDeliveryAggregate {
						t.Fatal("desired edits replaced local acknowledged facts")
					}
				} else if e.Generation != 0 || e.ConfigRevision != 0 || e.AlertDelivery != "" {
					t.Fatal("unproven local tuple was exposed")
				}
			}
			if len(remote.targets) != 0 || len(local.targets) != 1 || local.targets[0].ProbeID != domain.LocalProbeID {
				t.Fatal("local lookup crossed into the remote reader")
			}
			access.allow[7] = false
			before := len(local.targets)
			if _, err := svc.AcknowledgedDeliveryConfigs(t.Context(), 1, 7, 3, time.Now().UTC()); !errors.Is(err, ports.ErrNotFound) || len(local.targets) != before {
				t.Fatal("hidden local assignment performed a document read")
			}
		})
	}
}
