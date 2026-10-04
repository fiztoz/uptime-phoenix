package repository_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func TestM5RevocationFencesAndRetainsHistory(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newCommandFixture(t, engine)
			ctx := t.Context()
			activateReplayConfig(t, f.replayFixture)
			f.ingest(t, f.batch(f.observation(2)))
			rows, err := repository.NewRegionalCommitStore(f.f.db).LatestObservations(ctx, f.monitor)
			if err != nil || len(rows) != 1 || rows[0].Seq != 2 || rows[0].ID <= 0 {
				t.Fatalf("latest committed browser sample: %+v %v", rows, err)
			}
			lifecycle := repository.NewProbeLifecycleStore(f.f.db)
			if err := lifecycle.DeleteProbe(ctx, f.session.ProbeID, uuid.NewString(), time.Now()); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("deleted assigned source: %v", err)
			}
			before, err := f.f.registry.GetByID(ctx, f.session.ProbeID)
			if err != nil {
				t.Fatal(err)
			}
			if before.RevokedAt != nil {
				t.Fatal("failed delete changed authority")
			}
			cleanup := injectOutboxFailure(t, f.f, "probe_revocations", "INSERT")
			if _, err := lifecycle.RevokeProbe(ctx, f.session.ProbeID, uuid.NewString(), time.Now()); err == nil {
				t.Fatal("failed receipt reported success")
			}
			cleanup()
			after, err := f.f.registry.GetByID(ctx, f.session.ProbeID)
			if err != nil || after.Revision != before.Revision || after.RevokedAt != nil {
				t.Fatal("failed revocation changed registration", err)
			}
			f.issue(t)
			if _, err := f.commands.ClaimCommand(ctx, f.session, time.Second, domain.ProbeCommandCapabilities{AlertAcknowledgement: true}); err != nil {
				t.Fatal(err)
			}
			receipt, err := lifecycle.RevokeProbe(ctx, f.session.ProbeID, uuid.NewString(), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.IngestReplayBatch(ctx, f.session, f.batch(f.observation(3)), &services.AccessService{}); err == nil {
				t.Fatal("revoked session accepted telemetry")
			}
			at := time.Now().UTC()
			if err := f.commands.CompleteCommand(ctx, f.session, domain.ProbeCommandOutcome{CommandID: f.ack.CommandID, Status: "applied", AppliedAt: &at, Message: "Incident acknowledged"}); err == nil {
				t.Fatal("revoked session confirmed command")
			}
			if err := runNamedMigration(t, f.f, "072_probe_revocation", "down"); err == nil {
				t.Fatal("downgrade discarded revocation")
			}
			set, err := f.f.assignments.GetByMonitorID(ctx, f.monitor)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.f.assignments.Replace(ctx, f.monitor, set.Revision, []string{domain.LocalProbeID}, domain.HealthPolicyAnyDown); err != nil {
				t.Fatal(err)
			}
			if err := lifecycle.DeleteProbe(ctx, f.session.ProbeID, uuid.NewString(), time.Now()); err != nil {
				t.Fatal(err)
			}
			retained, err := f.f.incidents.GetIncident(ctx, f.ack.SourceAlertID)
			if err != nil || retained.ProbeID != f.session.ProbeID {
				t.Fatal("deletion erased source incident", err)
			}
			if _, err := lifecycle.GetRevocation(ctx, receipt.OperationID); err != nil {
				t.Fatal("deletion erased receipt", err)
			}
			set, err = f.f.assignments.GetByMonitorID(ctx, f.monitor)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.f.assignments.Replace(ctx, f.monitor, set.Revision, []string{f.session.ProbeID}, domain.HealthPolicyAnyDown); err == nil {
				t.Fatal("deleted source reassigned")
			}
		})
	}
}
