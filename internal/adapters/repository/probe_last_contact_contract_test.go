package repository_test

import (
	"errors"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// TestProbeLastContactContract pins #133 on both engines: a remote probe's last
// seen is persisted hub-observed contact. Connection state, lease renewal and
// release never write it; only the live session generation's owner can, with
// the hub DB clock at whole-second precision; it never moves backward; it
// freezes across disconnect and survives a restart; reconnecting advances it.
func TestProbeLastContactContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			f.remote(t, probeRegistryID1, "last-contact")
			ctx := t.Context()
			store := repository.NewProbeConnectorStore(f.db)
			diagnostics := repository.NewProbeDiagnosticsStore(f.db)
			read := func(t *testing.T, db *bun.DB) *domain.ProbeDiagnostics {
				t.Helper()
				d, err := repository.NewProbeDiagnosticsStore(db).GetProbeDiagnostics(ctx, probeRegistryID1)
				if err != nil {
					t.Fatal(err)
				}
				return d
			}
			contact := func(t *testing.T) *time.Time {
				t.Helper()
				d := read(t, f.db)
				if d.Session == nil {
					t.Fatal("session facts missing")
				}
				return d.Session.LastContactAt
			}
			setRaw := func(t *testing.T, value int64) {
				t.Helper()
				if _, err := f.db.ExecContext(ctx, "UPDATE probe_sessions SET last_contact_at = ? WHERE probe_id = ?", value, probeRegistryID1); err != nil {
					t.Fatal(err)
				}
			}
			mustConflict := func(t *testing.T, name string, lease domain.ProbeConnectorLease) {
				t.Helper()
				before := contact(t)
				if err := store.RecordConnectorContact(ctx, lease); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("%s recorded contact: %v", name, err)
				}
				if after := contact(t); !equalTimePtr(before, after) {
					t.Fatalf("%s changed last contact: %v -> %v", name, before, after)
				}
			}

			if _, err := diagnostics.GetProbeDiagnostics(ctx, probeRegistryID1); err != nil {
				t.Fatal(err)
			}
			first, err := store.AcquireConnector(ctx, probeRegistryID1, probeRegistryID2)
			if err != nil {
				t.Fatal(err)
			}

			// Phase: connection state and hub maintenance are not contact.
			if err := store.SetConnectorConnected(ctx, first, true); err != nil {
				t.Fatal(err)
			}
			if _, err := store.RenewConnector(ctx, first); err != nil {
				t.Fatal(err)
			}
			if got := contact(t); got != nil {
				t.Fatalf("connect/renew invented contact: %v", got)
			}
			summary := services.ProbeDiagnosticsSummary(time.Now(), *read(t, f.db))
			if summary.ConnectionStatus != services.ProbeConnectionOnline || summary.LastSeenAt != nil {
				t.Fatalf("online without contact: %+v", summary)
			}

			// Phase: accepted contact stamps the hub DB clock in whole seconds.
			lower := time.Now().UTC().Add(-5 * time.Second).Truncate(time.Second)
			if err := store.RecordConnectorContact(ctx, first); err != nil {
				t.Fatal(err)
			}
			upper := time.Now().UTC().Add(5 * time.Second)
			stamped := contact(t)
			if stamped == nil || stamped.Location() != time.UTC || stamped.Nanosecond() != 0 || stamped.Before(lower) || stamped.After(upper) {
				t.Fatalf("contact not stamped from the hub clock: %v (window %v..%v)", stamped, lower, upper)
			}
			summary = services.ProbeDiagnosticsSummary(time.Now(), *read(t, f.db))
			if summary.LastSeenAt == nil || !summary.LastSeenAt.Equal(*stamped) {
				t.Fatalf("summary last seen %v, want %v", summary.LastSeenAt, stamped)
			}

			// Phase: never backward. A later stored value wins over a new record.
			future := time.Now().Add(time.Hour).Unix()
			setRaw(t, future)
			if err := store.RecordConnectorContact(ctx, first); err != nil {
				t.Fatal(err)
			}
			if got := contact(t); got == nil || got.Unix() != future {
				t.Fatalf("contact moved backward: %v want %d", got, future)
			}
			past := time.Now().Add(-10 * time.Minute).Unix()
			setRaw(t, past)
			if err := store.RecordConnectorContact(ctx, first); err != nil {
				t.Fatal(err)
			}
			if got := contact(t); got == nil || got.Unix() <= past {
				t.Fatalf("contact did not advance: %v", got)
			}

			// Phase: disconnect freezes the value; a closed session cannot write.
			setRaw(t, past)
			if err := store.SetConnectorConnected(ctx, first, false); err != nil {
				t.Fatal(err)
			}
			if err := store.ReleaseConnector(ctx, first); err != nil {
				t.Fatal(err)
			}
			if got := contact(t); got == nil || got.Unix() != past {
				t.Fatalf("release changed last contact: %v", got)
			}
			mustConflict(t, "released session", first)
			summary = services.ProbeDiagnosticsSummary(time.Now(), *read(t, f.db))
			if summary.ConnectionStatus != services.ProbeConnectionDisconnected || summary.LastSeenAt == nil || summary.LastSeenAt.Unix() != past {
				t.Fatalf("disconnected summary: %+v", summary)
			}

			// Phase: a reconnect keeps the frozen value until accepted contact,
			// and the superseded generation stays fenced out.
			second, err := store.AcquireConnector(ctx, probeRegistryID1, probeRegistryID2)
			if err != nil || second.Generation != first.Generation+1 {
				t.Fatalf("reacquire: %+v %v", second, err)
			}
			if got := contact(t); got == nil || got.Unix() != past {
				t.Fatalf("acquisition changed last contact: %v", got)
			}
			mustConflict(t, "older generation", first)
			mustConflict(t, "foreign owner", domain.ProbeConnectorLease{ProbeID: probeRegistryID1, OwnerID: probeRegistryID3, Generation: second.Generation})
			if err := store.RecordConnectorContact(ctx, second); err != nil {
				t.Fatal(err)
			}
			advanced := contact(t)
			if advanced == nil || advanced.Unix() <= past {
				t.Fatalf("reconnect contact did not advance: %v", advanced)
			}

			// Phase: an expired lease cannot write, even for its own generation.
			if _, err := f.db.ExecContext(ctx, "UPDATE probe_sessions SET lease_until = 1 WHERE probe_id = ?", probeRegistryID1); err != nil {
				t.Fatal(err)
			}
			mustConflict(t, "expired lease", second)

			// Phase: the value survives a restart (a fresh connection pool).
			var peer *bun.DB
			if engine == "sqlite" {
				peer, err = sqlite.NewDB(f.dsn)
			} else {
				peer, err = mariadb.NewDB(f.dsn)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = peer.Close() }()
			if d := read(t, peer); d.Session == nil || !equalTimePtr(d.Session.LastContactAt, advanced) {
				t.Fatalf("restart lost last contact: %+v", d.Session)
			}
		})
	}
}

func equalTimePtr(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}
