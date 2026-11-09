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

// T15: a revoked probe must not get back in by reconnecting. The same
// reconnect steps succeed just before the revocation, so each refusal after it
// is caused by the revocation and not by a held lease. The live session loses
// its lease, telemetry from it is refused without touching history, and no new
// connector, runtime owner or credential activation is admitted.
func TestProbeRevokedReconnectIsRefused(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			r := newReplayFixture(t, engine)
			ctx := t.Context()
			connections := repository.NewProbeConnectorStore(r.f.db)
			const enrollmentID = "55555555-5555-4555-8555-555555555555"
			lease := domain.ProbeConnectorLease{ProbeID: r.session.ProbeID, OwnerID: r.session.OwnerID, Generation: r.session.ConnectionGeneration}
			if result := r.ingest(t, r.batch(r.observation(1))); result.AcceptedCount != 1 {
				t.Fatalf("control ingest: %+v", result)
			}

			// Control: before revocation the probe can drop and reconnect.
			if err := connections.ReleaseConnector(ctx, lease); err != nil {
				t.Fatal("control release:", err)
			}
			again, err := connections.AcquireConnector(ctx, r.session.ProbeID, probeRegistryID3)
			if err != nil {
				t.Fatal("control reconnect:", err)
			}
			if err := connections.ActivateConnection(ctx, r.session.ProbeID, enrollmentID, 1, time.Now()); err != nil {
				t.Fatal("control activation:", err)
			}
			session := r.session
			session.OwnerID, session.ConnectionGeneration = again.OwnerID, again.Generation
			if result, err := r.store.IngestReplayBatch(ctx, session, r.batch(r.observation(2)), &services.AccessService{}); err != nil || result.AcceptedCount != 1 {
				t.Fatalf("control ingest after reconnect: %+v %v", result, err)
			}

			if _, err := repository.NewProbeLifecycleStore(r.f.db).RevokeProbe(ctx, r.session.ProbeID, uuid.NewString(), time.Now()); err != nil {
				t.Fatal(err)
			}
			// Turning the probe back on is refused; revocation is permanent.
			revoked, err := r.f.registry.GetByID(ctx, r.session.ProbeID)
			if err != nil || revoked.RevokedAt == nil {
				t.Fatalf("revocation not recorded: %+v %v", revoked, err)
			}
			revoked.Enabled = true
			if err := r.f.registry.Update(ctx, revoked, revoked.Revision); err == nil {
				t.Fatal("revoked probe re-enabled")
			}
			if after, err := r.f.registry.GetByID(ctx, r.session.ProbeID); err != nil || after.Enabled || after.RevokedAt == nil {
				t.Fatalf("re-enable changed the revoked registration: %+v %v", after, err)
			}
			observations := replayCount(t, r.f, "probe_observations")
			receipts := replayCount(t, r.f, "probe_telemetry_receipts")

			if _, err := connections.RenewConnector(ctx, again); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("revoked session renewed its lease: %v", err)
			}
			if _, err := r.store.IngestReplayBatch(ctx, session, r.batch(r.observation(3)), &services.AccessService{}); err == nil {
				t.Fatal("revoked session accepted telemetry")
			}
			// Revocation already fenced the session, so its late release is a
			// harmless no-op or a conflict, never an error that wedges cleanup.
			if err := connections.ReleaseConnector(ctx, again); err != nil && !errors.Is(err, ports.ErrConflict) {
				t.Fatal("release after revocation:", err)
			}
			if _, err := connections.AcquireConnector(ctx, r.session.ProbeID, probeRegistryID3); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("revoked probe reconnected: %v", err)
			}
			if _, err := connections.AcquireConnector(ctx, r.session.ProbeID, probeRegistryID2); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("revoked probe reconnected through another hub worker: %v", err)
			}
			if _, err := connections.AcquireRuntime(ctx, r.session.ProbeID, probeRegistryID3); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("revoked probe got a runtime owner: %v", err)
			}
			if err := connections.ActivateConnection(ctx, r.session.ProbeID, enrollmentID, 1, time.Now()); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("revoked probe credential activated: %v", err)
			}
			if _, err := r.store.IngestReplayBatch(ctx, session, r.batch(r.observation(3)), &services.AccessService{}); err == nil {
				t.Fatal("stale session accepted telemetry after reconnect attempts")
			}
			if got := replayCount(t, r.f, "probe_observations"); got != observations {
				t.Fatalf("observations %d -> %d after revocation", observations, got)
			}
			if got := replayCount(t, r.f, "probe_telemetry_receipts"); got != receipts {
				t.Fatalf("receipts %d -> %d after revocation", receipts, got)
			}
			if cursor, err := r.store.GetCursor(ctx, r.session.ProbeID, r.session.StreamID); err != nil || cursor != 2 {
				t.Fatalf("cursor %d after revocation, want 2: %v", cursor, err)
			}
		})
	}
}
