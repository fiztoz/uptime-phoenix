package repository_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func TestHubWatchdogAppliedConfigReader(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("ColdAppliedGraphBeforePreparedGraph", func(t *testing.T) {
				h := newHubWatchdogFixture(t, engine)
				ctx := t.Context()
				settings := domain.DefaultProbeWatchdogSettings()
				settings.LostAfterSeconds = 120
				if _, err := repository.NewProbeWatchdogSettingsStore(h.f.db).Replace(ctx, h.authority.ProbeID, 0, settings); err != nil {
					t.Fatal(err)
				}
				next, err := h.syncer.RefreshRemote(ctx, domain.ProbeConfigTarget{HubID: h.authority.HubID, ProbeID: h.authority.ProbeID}, h.at)
				if err != nil || next.Revision != 2 {
					t.Fatal("new prepared graph", next, err)
				}
				reader := repository.NewProbeWatchdogConfigReader(repository.NewProbeWatchdogStore(reopenConfigDB(t, h.f), h.protector, probe.EdgeTelemetryEncoder{}), h.authority, probe.NewEdgeConfigDecoder(checker.Get, notifier.Get))
				config, err := reader.Load(ctx)
				if err != nil || config.Metadata.Revision != 1 || config.Watchdog.LostAfterSeconds != 90 {
					t.Fatal("unapplied graph leaked to source owner", err)
				}
				lease := domain.ProbeConnectorLease{ProbeID: h.authority.ProbeID, OwnerID: h.authority.RuntimeOwner.OwnerID, Generation: h.authority.HealthGeneration}
				if err := h.syncer.RecordRemoteApplied(ctx, lease, domain.ProbeActiveConfig{ProbeConfigTarget: next.ProbeConfigTarget, Revision: next.Revision, SHA256: next.SHA256, AppliedAt: h.at, AssignmentCount: 1}); err != nil {
					t.Fatal(err)
				}
				config, err = reader.Load(ctx)
				if err != nil || config.Metadata.Revision != 2 || config.Watchdog.LostAfterSeconds != 120 {
					t.Fatal("cold reader cached old applied graph", err)
				}
				if _, err := h.f.db.NewDelete().Table("probe_active_configs").Where("probe_id=?", h.authority.ProbeID).Exec(ctx); err != nil {
					t.Fatal(err)
				}
				if _, err := reader.Load(ctx); !errors.Is(err, ports.ErrNotFound) {
					t.Fatal("missing applied graph fell back to prepared", err)
				}
			})
			t.Run("AuthorityAndHashFences", func(t *testing.T) {
				h := newHubWatchdogFixture(t, engine)
				ctx := t.Context()
				decoder := probe.NewEdgeConfigDecoder(checker.Get, notifier.Get)
				reader := repository.NewProbeWatchdogConfigReader(h.store, h.authority, decoder)
				for _, mutate := range []func(*domain.ProbeWatchdogAuthority){
					func(a *domain.ProbeWatchdogAuthority) { a.RuntimeOwner.Epoch++ },
					func(a *domain.ProbeWatchdogAuthority) { a.RuntimeOwner.OwnerID = a.HubID },
					func(a *domain.ProbeWatchdogAuthority) { a.HubID = a.ProbeID },
					func(a *domain.ProbeWatchdogAuthority) { a.StreamID = a.ProbeID },
				} {
					a := h.authority
					mutate(&a)
					if _, err := repository.NewProbeWatchdogConfigReader(h.store, a, decoder).Load(ctx); !errors.Is(err, ports.ErrConflict) {
						t.Fatal("unauthorized config read", err)
					}
				}
				wrong, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{38}, 32))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := repository.NewProbeWatchdogConfigReader(repository.NewProbeWatchdogStore(h.f.db, wrong, probe.EdgeTelemetryEncoder{}), h.authority, decoder).Load(ctx); !errors.Is(err, domain.ErrProbeKeyMismatch) {
					t.Fatal("wrong installation key read config", err)
				}
				if _, err := h.f.db.NewUpdate().Table("probe_active_configs").Set("sha256=?", strings.Repeat("f", 64)).Where("probe_id=?", h.authority.ProbeID).Exec(ctx); err != nil {
					t.Fatal(err)
				}
				if _, err := reader.Load(ctx); !errors.Is(err, ports.ErrConflict) {
					t.Fatal("hash mismatch read config", err)
				}
			})
			t.Run("RegistrationAndExpiredOwner", func(t *testing.T) {
				h := newHubWatchdogFixture(t, engine)
				ctx := t.Context()
				reader := repository.NewProbeWatchdogConfigReader(h.store, h.authority, probe.NewEdgeConfigDecoder(checker.Get, notifier.Get))
				if _, err := h.f.db.ExecContext(ctx, "UPDATE probes SET enabled=? WHERE id=?", false, h.authority.ProbeID); err != nil {
					t.Fatal(err)
				}
				if _, err := reader.Load(ctx); !errors.Is(err, ports.ErrConflict) {
					t.Fatal("disabled registration read config", err)
				}
				if _, err := h.f.db.ExecContext(ctx, "UPDATE probes SET enabled=? WHERE id=?", true, h.authority.ProbeID); err != nil {
					t.Fatal(err)
				}
				if _, err := h.f.db.ExecContext(ctx, "UPDATE probe_runtime_owners SET lease_until=0 WHERE probe_id=?", h.authority.ProbeID); err != nil {
					t.Fatal(err)
				}
				if _, err := reader.Load(ctx); !errors.Is(err, ports.ErrConflict) {
					t.Fatal("expired owner read config", err)
				}
			})
		})
	}
}
