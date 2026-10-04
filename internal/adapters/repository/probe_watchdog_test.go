package repository_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type hubWatchdogFixture struct {
	replayFixture
	store     *repository.ProbeWatchdogStore
	protector ports.ProbeConfigProtector
	authority domain.ProbeWatchdogAuthority
}

func newHubWatchdogFixture(t *testing.T, engine string) hubWatchdogFixture {
	t.Helper()
	r := newReplayFixture(t, engine)
	protector, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{37}, 32))
	if err != nil {
		t.Fatal(err)
	}
	connector := repository.NewProbeConnectorStore(r.f.db)
	if err := connector.ReleaseConnector(t.Context(), domain.ProbeConnectorLease{ProbeID: r.session.ProbeID, OwnerID: r.session.OwnerID, Generation: r.session.ConnectionGeneration}); err != nil {
		t.Fatal(err)
	}
	owner, err := connector.AcquireRuntime(t.Context(), r.session.ProbeID, r.session.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	child, err := connector.AcquireRuntimeConnector(t.Context(), owner)
	if err != nil {
		t.Fatal(err)
	}
	r.session.ConnectionGeneration = child.Generation
	snapshot, err := repository.NewProbeConfigStore(r.f.db).Latest(t.Context(), r.session.ProbeID)
	if err != nil {
		t.Fatal(err)
	}
	receipt := domain.ProbeActiveConfig{ProbeConfigTarget: snapshot.ProbeConfigTarget, Revision: snapshot.Revision, SHA256: snapshot.SHA256, AppliedAt: r.at, AssignmentCount: 1}
	if err := r.syncer.RecordRemoteApplied(t.Context(), child, receipt); err != nil {
		t.Fatal(err)
	}
	return hubWatchdogFixture{replayFixture: r, store: repository.NewProbeWatchdogStore(r.f.db, protector, probe.EdgeTelemetryEncoder{}), protector: protector, authority: domain.ProbeWatchdogAuthority{HubID: r.session.HubID, ProbeID: r.session.ProbeID, StreamID: r.session.StreamID, RuntimeOwner: owner, HealthGeneration: child.Generation}}
}
func (h hubWatchdogFixture) opening() domain.ProbeWatchdogRecord {
	inc := &domain.RegionalIncident{SourceAlertID: "ed01234f-6b81-4f5b-b423-cf3b32132093", ProbeID: h.authority.ProbeID, Scope: domain.IncidentScopeProbeConnection, SubjectKind: domain.IncidentSubjectWatchdog, Status: domain.AlertStatusFiring, TransitionVersion: 1, ConfigRevision: 1, StartedAt: h.at, Reason: "Probe execution health unavailable"}
	return domain.ProbeWatchdogRecord{At: h.at, ConfigRevision: 1, Status: domain.ProbeWatchdogLost, Checkpoint: domain.ProbeWatchdogCheckpoint{Armed: true, LossElapsed: 90 * time.Second, PendingLoss: true}, Incident: inc, DeliveryIntents: []domain.DeliveryIntent{{DeliveryID: "afcf87c2-803a-4f51-ae42-a7019c2c6089", ProbeID: h.authority.ProbeID, SourceAlertID: inc.SourceAlertID, SourceTransitionVersion: 1, NotificationID: h.channel, NotificationVersion: 1, EventKind: domain.DeliveryEventProbeConnection, AvailableAt: h.at}}}
}
func requireHubWatchdogEmpty(t *testing.T, h hubWatchdogFixture) {
	t.Helper()
	for _, table := range []string{"probe_watchdog_state", "probe_watchdog_events", "probe_hub_watchdog_incidents", "probe_incidents", "probe_delivery_intents", "probe_delivery_events", "probe_telemetry_receipts", "probe_observations", "monitor_probe_state"} {
		if n := replayCount(t, h.f, table); n != 0 {
			t.Fatalf("partial %s=%d", table, n)
		}
	}
	if cursor, err := h.replayFixture.store.GetCursor(t.Context(), h.authority.ProbeID, h.authority.StreamID); err != nil || cursor != 0 {
		t.Fatalf("remote cursor changed: %d %v", cursor, err)
	}
}
func TestHubWatchdogSourceContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for name, test := range map[string]func(*testing.T, hubWatchdogFixture){"AtomicAndOwnedEffects": testHubWatchdogEffects, "RuntimeAndSessionFences": testHubWatchdogFences, "Rollback": testHubWatchdogRollback, "RestartAckRecovery": testHubWatchdogRestart, "ConcurrentCommit": testHubWatchdogConcurrent} {
				t.Run(name, func(t *testing.T) { test(t, newHubWatchdogFixture(t, engine)) })
			}
		})
	}
}
func testHubWatchdogEffects(t *testing.T, h hubWatchdogFixture) {
	ctx := t.Context()
	r := h.opening()
	state, err := h.store.CommitWatchdog(ctx, h.authority, r)
	if err != nil || state.Version != 1 || state.IncidentSeq != 1 {
		t.Fatalf("opening: %+v %v", state, err)
	}
	for _, table := range []string{"probe_watchdog_state", "probe_watchdog_events", "probe_hub_watchdog_incidents", "probe_incidents", "probe_delivery_intents"} {
		if n := replayCount(t, h.f, table); n != 1 {
			t.Fatalf("missing %s: %d", table, n)
		}
	}
	for _, table := range []string{"probe_telemetry_receipts", "probe_observations", "monitor_probe_state", "alerts"} {
		if n := replayCount(t, h.f, table); n != 0 {
			t.Fatalf("watchdog changed %s: %d", table, n)
		}
	}
	if cursor, err := h.replayFixture.store.GetCursor(ctx, h.authority.ProbeID, h.authority.StreamID); err != nil || cursor != 0 {
		t.Fatalf("hub source advanced edge cursor: %d %v", cursor, err)
	}
	var count int
	if err := h.f.db.NewRaw("SELECT COUNT(*) FROM probe_delivery_intents WHERE monitor_id IS NULL AND assignment_generation IS NULL AND stream_id IS NULL").Scan(ctx, &count); err != nil || count != 1 {
		t.Fatalf("fabricated source entity: %d %v", count, err)
	}
	var payload []byte
	if err := h.f.db.NewRaw("SELECT payload FROM probe_watchdog_events WHERE seq=1").Scan(ctx, &payload); err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Kind string                     `json:"kind"`
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil || wire.Kind != "watchdog.transition" || string(wire.Data["monitor_id"]) != "null" {
		t.Fatalf("wrong journal bytes: %s %v", payload, err)
	}
	// Mirroring APIs cannot overwrite hub source identity or report its outcomes.
	forged := *state.Incident
	forged.TransitionVersion++
	forged.Status = domain.AlertStatusResolved
	forged.ResolvedAt = &r.At
	if err := h.f.incidents.PutIncident(ctx, &forged); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("mirror overwrote hub source: %v", err)
	}
	outcome := domain.RegionalDelivery{DeliveryID: "b28fccd6-597b-48d8-8b5c-1d6d203a2c2f", SourceAlertID: r.Incident.SourceAlertID, SourceTransitionVersion: 1, ProbeID: h.authority.ProbeID, NotificationID: h.channel, NotificationVersion: 1, EventKind: domain.DeliveryEventProbeConnection, Attempt: 1, Status: domain.DeliveryStatusSent, ObservedAt: r.At}
	if err := h.f.deliveries.PutDelivery(ctx, &outcome); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("mirror forged hub send: %v", err)
	}
	outbox := repository.NewRegionalCommitStore(h.f.db)
	items, err := outbox.ClaimDeliveries(ctx, h.authority.ProbeID, r.At, time.Minute, 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("watchdog unclaimable: %+v %v", items, err)
	}
	item := items[0]
	if item.MonitorID != 0 || item.AssignmentGeneration != 0 || item.StreamID != "" {
		t.Fatalf("wrong outbox context: %+v", item)
	}
	claim := domain.DeliveryClaim{DeliveryID: item.DeliveryID, ProbeID: item.ProbeID, Attempt: item.Attempt, LeaseToken: item.LeaseToken}
	if err := outbox.FinishDelivery(ctx, claim, domain.DeliveryResult{Status: domain.DeliveryStatusSent, At: r.At.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if n := replayCount(t, h.f, "probe_delivery_events"); n != 1 {
		t.Fatalf("no hub outcome: %d", n)
	}
	r.Incident = nil
	r.DeliveryIntents = nil
	r.ExpectedVersion = state.Version
	state, err = h.store.CommitWatchdog(ctx, h.authority, r)
	if err != nil || state.IncidentSeq != 1 || state.Version != 2 {
		t.Fatalf("checkpoint changed history: %+v %v", state, err)
	}
	if err := runEngineMigration(t, h.f.db, h.f.engine, "059_probe_watchdog_source", "down"); err == nil {
		t.Fatal("downgrade discarded hub authority")
	}
}
func testHubWatchdogFences(t *testing.T, h hubWatchdogFixture) {
	ctx := t.Context()
	for name, mutate := range map[string]func(*domain.ProbeWatchdogAuthority, *domain.ProbeWatchdogRecord){
		"owner": func(a *domain.ProbeWatchdogAuthority, _ *domain.ProbeWatchdogRecord) {
			a.RuntimeOwner.OwnerID = a.HubID
		},
		"epoch":             func(a *domain.ProbeWatchdogAuthority, _ *domain.ProbeWatchdogRecord) { a.RuntimeOwner.Epoch++ },
		"health generation": func(a *domain.ProbeWatchdogAuthority, _ *domain.ProbeWatchdogRecord) { a.HealthGeneration++ },
		"installation":      func(a *domain.ProbeWatchdogAuthority, _ *domain.ProbeWatchdogRecord) { a.HubID = a.ProbeID },
		"stream":            func(a *domain.ProbeWatchdogAuthority, _ *domain.ProbeWatchdogRecord) { a.StreamID = a.ProbeID },
		"config":            func(_ *domain.ProbeWatchdogAuthority, r *domain.ProbeWatchdogRecord) { r.ConfigRevision++ },
		"version":           func(_ *domain.ProbeWatchdogAuthority, r *domain.ProbeWatchdogRecord) { r.ExpectedVersion++ },
		"version overflow": func(_ *domain.ProbeWatchdogAuthority, r *domain.ProbeWatchdogRecord) {
			r.ExpectedVersion = math.MaxInt64
		},
	} {
		t.Run(name, func(t *testing.T) {
			a, r := h.authority, h.opening()
			mutate(&a, &r)
			if _, err := h.store.CommitWatchdog(ctx, a, r); err == nil {
				t.Fatal("invalid authority accepted")
			}
			requireHubWatchdogEmpty(t, h)
		})
	}
	wrong, _ := auth.NewProbeConfigProtector(bytes.Repeat([]byte{38}, 32))
	if _, err := repository.NewProbeWatchdogStore(h.f.db, wrong, probe.EdgeTelemetryEncoder{}).CommitWatchdog(ctx, h.authority, h.opening()); !errors.Is(err, domain.ErrProbeKeyMismatch) {
		t.Fatalf("wrong installation key: %v", err)
	}
	connector := repository.NewProbeConnectorStore(h.f.db)
	child, err := connector.AcquireRuntimeConnector(ctx, h.authority.RuntimeOwner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.CommitWatchdog(ctx, h.authority, h.opening()); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("stale health committed: %v", err)
	}
	h.authority.HealthGeneration = child.Generation
	if _, err := h.f.db.ExecContext(ctx, "UPDATE probe_runtime_owners SET lease_until=0 WHERE probe_id=?", h.authority.ProbeID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.CommitWatchdog(ctx, h.authority, h.opening()); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("expired runtime committed: %v", err)
	}
	replacement, err := connector.AcquireRuntime(ctx, h.authority.ProbeID, h.authority.RuntimeOwner.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.ReadWatchdog(ctx, h.authority); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("old runtime read new ownership: %v", err)
	}
	h.authority.RuntimeOwner = replacement
	h.authority.HealthGeneration = 0
	if _, err := h.f.db.ExecContext(ctx, "UPDATE probes SET enabled=? WHERE id=?", false, h.authority.ProbeID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.CommitWatchdog(ctx, h.authority, h.opening()); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("disabled runtime committed: %v", err)
	}
	if _, err := h.f.db.ExecContext(ctx, "UPDATE probes SET enabled=? WHERE id=?", true, h.authority.ProbeID); err != nil {
		t.Fatal(err)
	}
	requireHubWatchdogEmpty(t, h)
	// Local elapsed loss remains writable without a live socket, under the parent.
	if _, err := h.store.CommitWatchdog(ctx, h.authority, h.opening()); err != nil {
		t.Fatal(err)
	}
}
func testHubWatchdogRollback(t *testing.T, h hubWatchdogFixture) {
	ctx := t.Context()
	for _, table := range []string{"probe_incidents", "probe_hub_watchdog_incidents", "probe_watchdog_events", "probe_delivery_intents", "probe_watchdog_state"} {
		t.Run(table, func(t *testing.T) {
			trigger := "CREATE TRIGGER fail_watchdog BEFORE INSERT ON " + table + " BEGIN SELECT RAISE(ABORT,'watchdog fault'); END"
			if h.f.engine == "mariadb" {
				trigger = "CREATE TRIGGER fail_watchdog BEFORE INSERT ON " + table + " FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='watchdog fault'"
			}
			if _, err := h.f.db.ExecContext(ctx, trigger); err != nil {
				t.Fatal(err)
			}
			_, err := h.store.CommitWatchdog(ctx, h.authority, h.opening())
			if _, dropErr := h.f.db.ExecContext(ctx, "DROP TRIGGER fail_watchdog"); dropErr != nil {
				t.Fatal(dropErr)
			}
			if err == nil {
				t.Fatal("partial source commit")
			}
			requireHubWatchdogEmpty(t, h)
		})
	}
}
func testHubWatchdogRestart(t *testing.T, h hubWatchdogFixture) {
	ctx := t.Context()
	r := h.opening()
	state, err := h.store.CommitWatchdog(ctx, h.authority, r)
	if err != nil {
		t.Fatal(err)
	}
	peer := repository.NewProbeWatchdogStore(reopenConfigDB(t, h.f), h.protector, probe.EdgeTelemetryEncoder{})
	restored, err := peer.ReadWatchdog(ctx, h.authority)
	if err != nil || !reflect.DeepEqual(state, restored) {
		t.Fatalf("restart: %+v %v", restored, err)
	}
	ack := *restored.Incident
	ack.Status = domain.AlertStatusAcked
	ack.TransitionVersion++
	ack.AckedAt = &h.at
	ack.AckCommandID = "b7b0f183-9de1-4456-9dfe-8d83a09de037"
	ack.AckActorDisplayName = "Operator"
	r.ExpectedVersion = restored.Version
	r.Incident = &ack
	r.DeliveryIntents = nil
	state, err = peer.CommitWatchdog(ctx, h.authority, r)
	if err != nil {
		t.Fatal(err)
	}
	resolved := *state.Incident
	resolved.Status = domain.AlertStatusResolved
	resolved.TransitionVersion++
	backward := h.at.Add(-time.Minute)
	resolved.ResolvedAt = &backward
	r.ExpectedVersion = state.Version
	r.Incident = &resolved
	r.At = backward
	r.Status = domain.ProbeWatchdogHealthy
	r.Checkpoint = domain.ProbeWatchdogCheckpoint{Armed: true}
	r.DeliveryIntents = h.opening().DeliveryIntents
	r.DeliveryIntents[0].DeliveryID = "6f80cced-9899-46ec-952f-c609757055f6"
	r.DeliveryIntents[0].SourceTransitionVersion = 3
	state, err = peer.CommitWatchdog(ctx, h.authority, r)
	if err != nil || state.IncidentSeq != 3 || state.Incident.AckCommandID != ack.AckCommandID {
		t.Fatalf("ACKed recovery: %+v %v", state, err)
	}
	if n := replayCount(t, h.f, "probe_incidents"); n != 1 {
		t.Fatalf("restart duplicated incident: %d", n)
	}
	// Hub ownership survives resolution and prevents later replay claiming its UUID.
	forged := *state.Incident
	forged.TransitionVersion++
	if err := h.f.incidents.PutIncident(ctx, &forged); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("resolved source stolen: %v", err)
	}
}
func testHubWatchdogConcurrent(t *testing.T, h hubWatchdogFixture) {
	peer := repository.NewProbeWatchdogStore(reopenConfigDB(t, h.f), h.protector, probe.EdgeTelemetryEncoder{})
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, store := range []*repository.ProbeWatchdogStore{h.store, peer} {
		go func(s *repository.ProbeWatchdogStore) {
			<-start
			_, err := s.CommitWatchdog(t.Context(), h.authority, h.opening())
			results <- err
		}(store)
	}
	close(start)
	success, stale := 0, 0
	for n := 0; n < 2; n++ {
		err := <-results
		if err == nil {
			success++
		} else if errors.Is(err, ports.ErrStaleLocalState) {
			stale++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || stale != 1 {
		t.Fatalf("writers: %d success %d stale", success, stale)
	}
	if n := replayCount(t, h.f, "probe_watchdog_events"); n != 1 {
		t.Fatalf("duplicate transition: %d", n)
	}
}

type delayedWatchdogEncoder struct {
	ports.EdgeTelemetryEncoder
	until time.Time
}

func (e delayedWatchdogEncoder) EncodeIncident(seq int64, at time.Time, inc domain.RegionalIncident) ([]byte, error) {
	time.Sleep(time.Until(e.until))
	return e.EdgeTelemetryEncoder.EncodeIncident(seq, at, inc)
}

func TestHubWatchdogSessionExpiryDuringCommit(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			h := newHubWatchdogFixture(t, engine)
			deadline := time.Now().UTC().Unix() + 2
			if _, err := h.f.db.ExecContext(t.Context(), "UPDATE probe_sessions SET lease_until=? WHERE probe_id=?", deadline, h.authority.ProbeID); err != nil {
				t.Fatal(err)
			}
			store := repository.NewProbeWatchdogStore(h.f.db, h.protector, delayedWatchdogEncoder{EdgeTelemetryEncoder: probe.EdgeTelemetryEncoder{}, until: time.Unix(deadline+1, 0)})
			if _, err := store.CommitWatchdog(t.Context(), h.authority, h.opening()); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("health authority expired during source commit: %v", err)
			}
			requireHubWatchdogEmpty(t, h)
		})
	}
}

func TestHubWatchdogSourceTimestampPrecision(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			h := newHubWatchdogFixture(t, engine)
			r := h.opening()
			r.Incident.StartedAt = r.Incident.StartedAt.Add(731 * time.Nanosecond)
			state, err := h.store.CommitWatchdog(t.Context(), h.authority, r)
			if err != nil {
				t.Fatal(err)
			}
			// A retrying caller may retain the original source timestamp rather than
			// round-trip the DB snapshot. Shared lifecycle validation permits this same
			// microsecond identity; the adapter must not reject it at nanosecond precision.
			resolved := *r.Incident
			resolved.Status = domain.AlertStatusResolved
			resolved.TransitionVersion++
			resolved.ResolvedAt = &r.At
			r.Incident = &resolved
			r.ExpectedVersion = state.Version
			r.Status = domain.ProbeWatchdogHealthy
			r.Checkpoint = domain.ProbeWatchdogCheckpoint{Armed: true}
			r.DeliveryIntents = nil
			if _, err := h.store.CommitWatchdog(t.Context(), h.authority, r); err != nil {
				t.Fatalf("same stored source identity rejected: %v", err)
			}
		})
	}
}
func TestHubWatchdogPendingDeliveryCollision(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			h := newHubWatchdogFixture(t, engine)
			r := h.opening()
			state, err := h.store.CommitWatchdog(t.Context(), h.authority, r)
			if err != nil {
				t.Fatal(err)
			}
			r.ExpectedVersion = state.Version
			r.Incident = nil
			if _, err := h.store.CommitWatchdog(t.Context(), h.authority, r); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("pending delivery collision must be typed conflict: %v", err)
			}
			after, err := h.store.ReadWatchdog(t.Context(), h.authority)
			if err != nil || !reflect.DeepEqual(state, after) {
				t.Fatalf("collision changed state: %+v %v", after, err)
			}
		})
	}
}
func TestHubWatchdogCannotStealEdgeIncident(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			h := newHubWatchdogFixture(t, engine)
			r := h.opening()
			original := *r.Incident
			// Edge source watchdogs are valid mirror entities for the same probe. Each
			// source owns its own random UUID; neither may adopt the other's UUID.
			if err := h.f.incidents.PutIncident(t.Context(), &original); err != nil {
				t.Fatal(err)
			}
			if _, err := h.store.CommitWatchdog(t.Context(), h.authority, r); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("hub claimed edge identity: %v", err)
			}
			for _, table := range []string{"probe_watchdog_state", "probe_watchdog_events", "probe_hub_watchdog_incidents", "probe_delivery_intents"} {
				if n := replayCount(t, h.f, table); n != 0 {
					t.Fatalf("adoption committed %s", table)
				}
			}
			after, err := h.f.incidents.GetIncident(t.Context(), original.SourceAlertID)
			if err != nil || !reflect.DeepEqual(original, *after) {
				t.Fatalf("edge identity mutated: %+v %v", after, err)
			}
		})
	}
}

func TestHubWatchdogMigrationPreservesLegacyLeases(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := t.Context()
			id := localMonitor(t, f)
			commit := queuedLocalCommit(id)
			if _, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, commit); err != nil {
				t.Fatal(err)
			}
			item := claimOne(t, outbox(f), commit.Heartbeat.Time)
			if err := runEngineMigration(t, f.db, engine, "059_probe_watchdog_source", "down"); err != nil {
				t.Fatal(err)
			}
			if engine == "sqlite" {
				data, err := os.ReadFile(filepath.Join(engine, "migrations", "059_probe_watchdog_source.up.sql"))
				if err != nil {
					t.Fatal(err)
				}
				err = f.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
					_, err := tx.ExecContext(ctx, string(data)+"\nSELECT * FROM injected_watchdog_migration_fault;")
					return err
				})
				if err == nil {
					t.Fatal("late migration fault accepted")
				}
				var leftovers int
				if err := f.db.NewRaw("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('probe_delivery_intents_v059','probe_hub_watchdog_incidents','probe_watchdog_events','probe_watchdog_state')").Scan(ctx, &leftovers); err != nil || leftovers != 0 {
					t.Fatalf("failed migration left partial schema: %d %v", leftovers, err)
				}
			}
			if err := runEngineMigration(t, f.db, engine, "059_probe_watchdog_source", "up"); err != nil {
				t.Fatal(err)
			}
			// MariaDB startup can resume after the atomic rename but before bookkeeping.
			// This second run must preserve the complete lease and immutable context too.
			if err := runEngineMigration(t, f.db, engine, "059_probe_watchdog_source", "up"); err != nil {
				t.Fatal(err)
			}
			after, err := outbox(f).GetDeliveryIntent(ctx, domain.LocalProbeID, item.DeliveryID)
			if err != nil || !reflect.DeepEqual(item, *after) {
				t.Fatalf("upgrade changed source lease: %+v %v", after, err)
			}
			if err := outbox(f).FinishDelivery(ctx, receipt(item), domain.DeliveryResult{Status: domain.DeliveryStatusSent, At: commit.Heartbeat.Time.Add(time.Second)}); err != nil {
				t.Fatal(err)
			}
			if engine == "sqlite" {
				var failures []struct {
					Table  string
					Rowid  int64
					Parent string
					Fkid   int
				}
				if err := f.db.NewRaw("PRAGMA foreign_key_check").Scan(ctx, &failures); err != nil || len(failures) > 0 {
					t.Fatalf("migration foreign keys: %+v %v", failures, err)
				}
			}
		})
	}
}

func TestHubWatchdogMigrationResumeBeforeRename(t *testing.T) {
	f := newProbeRegistryFixture(t, "mariadb")
	ctx := t.Context()
	id := localMonitor(t, f)
	commit := queuedLocalCommit(id)
	if _, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, commit); err != nil {
		t.Fatal(err)
	}
	item := claimOne(t, outbox(f), commit.Heartbeat.Time)
	if err := runEngineMigration(t, f.db, f.engine, "059_probe_watchdog_source", "down"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("mariadb", "migrations", "059_probe_watchdog_source.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	// Stop after creating/copying the replacement, before the atomic rename.
	prefix := strings.SplitN(string(data), "RENAME TABLE", 2)[0]
	for _, statement := range strings.Split(prefix, ";") {
		if strings.TrimSpace(statement) != "" {
			if _, err := f.db.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := runEngineMigration(t, f.db, f.engine, "059_probe_watchdog_source", "up"); err != nil {
		t.Fatal(err)
	}
	after, err := outbox(f).GetDeliveryIntent(ctx, domain.LocalProbeID, item.DeliveryID)
	if err != nil || !reflect.DeepEqual(item, *after) {
		t.Fatalf("resume changed lease: %+v %v", after, err)
	}
}
