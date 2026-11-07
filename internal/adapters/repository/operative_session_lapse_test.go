package repository_test

import (
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// activateRemote drives the single remote member through the real barrier
// stages to ACTIVE.
func (o operativeRemoteFixture) activateRemote(t *testing.T) {
	t.Helper()
	if err := o.store.SetOperativeSourceCapability(t.Context(), o.f.session, true); err != nil {
		t.Fatal(err)
	}
	if got := o.run(t, true); got.Phase != "quiescing" {
		t.Fatalf("capability did not admit the member: %+v", got)
	}
	o.complete(t, domain.DeliveryQuiesceKind)
	if got := o.run(t, true); got.Phase != "quiesced" {
		t.Fatalf("quiesce receipt did not quiesce: %+v", got)
	}
	if got := o.run(t, true); got.Phase != "applying" {
		t.Fatalf("quiesced did not issue application: %+v", got)
	}
	o.complete(t, domain.DeliveryApplyKind)
	if got := o.run(t, true); got.Phase != "active" {
		t.Fatalf("application receipt did not activate: %+v", got)
	}
}

func (o operativeRemoteFixture) setSession(t *testing.T, leaseUntil time.Time, bumpGeneration bool) {
	t.Helper()
	q := "UPDATE probe_sessions SET lease_until=? WHERE probe_id=?"
	if bumpGeneration {
		q = "UPDATE probe_sessions SET lease_until=?, generation=generation+1 WHERE probe_id=?"
	}
	if _, err := o.f.f.db.ExecContext(t.Context(), q, leaseUntil.Unix(), o.f.session.ProbeID); err != nil {
		t.Fatal(err)
	}
}

// release ends the session the way the hub does on a disconnect, through the
// real ReleaseConnector, after recording lastContact (zero leaves it unset).
func (o operativeRemoteFixture) release(t *testing.T, lastContact time.Time) {
	t.Helper()
	if !lastContact.IsZero() {
		if _, err := o.f.f.db.ExecContext(t.Context(), "UPDATE probe_sessions SET last_contact_at=? WHERE probe_id=?", lastContact.Unix(), o.f.session.ProbeID); err != nil {
			t.Fatal(err)
		}
	}
	var row struct {
		OwnerID    string
		Generation int64
	}
	if err := o.f.f.db.NewSelect().Table("probe_sessions").Column("owner_id", "generation").Where("probe_id = ?", o.f.session.ProbeID).Scan(t.Context(), &row); err != nil {
		t.Fatal(err)
	}
	lease := domain.ProbeConnectorLease{ProbeID: o.f.session.ProbeID, OwnerID: row.OwnerID, Generation: row.Generation}
	if err := repository.NewProbeConnectorStore(o.f.f.db).ReleaseConnector(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
}

func (o operativeRemoteFixture) phase(t *testing.T) (string, string) {
	t.Helper()
	var row struct {
		Phase  string
		Reason string
	}
	if err := o.f.f.db.NewSelect().Table("operative_cutover_state").Column("phase", "reason").Scan(t.Context(), &row); err != nil {
		t.Fatal(err)
	}
	return row.Phase, row.Reason
}

// TestOperativeSessionLapse covers #213: a hub restart lets a source's session
// lease lapse (same attested generation) or reconnect before re-attesting. That
// is not lost evidence. Draining on it closed the open incident
// administratively, dropped its recovery page and re-paged the same outage
// from a replacement barrier. A lapse holds the ACTIVE barrier for a bounded
// grace; a withdrawn capability or a lapse beyond the grace still drains.
func TestOperativeSessionLapse(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("ExpiredLeaseHoldsActive", func(t *testing.T) {
				o := newOperativeRemoteFixture(t, engine)
				o.activateRemote(t)
				o.setSession(t, time.Now().Add(-10*time.Second), false)
				if got := o.run(t, true); got.Phase != "active" {
					t.Fatalf("a lapsed session drained the barrier: %+v", got)
				}
				if phase, reason := o.phase(t); phase != "active" || reason != "" {
					t.Fatalf("state = %s/%q, want active", phase, reason)
				}
				if o.remoteResumes(t) != 0 {
					t.Fatal("a lapsed session released the source hold")
				}
				o.setSession(t, time.Now().Add(time.Minute), false)
				if got := o.run(t, true); got.Phase != "active" {
					t.Fatalf("a renewed session did not keep the barrier active: %+v", got)
				}
			})
			t.Run("ReconnectBeforeAttestHoldsActive", func(t *testing.T) {
				o := newOperativeRemoteFixture(t, engine)
				o.activateRemote(t)
				o.setSession(t, time.Now().Add(time.Minute), true)
				if got := o.run(t, true); got.Phase != "active" {
					t.Fatalf("a reconnect before re-attestation drained the barrier: %+v", got)
				}
			})
			t.Run("ReleasedSessionHoldsActive", func(t *testing.T) {
				// A transient hub error closes the probe connection and
				// releases its lease before the probe reconnects.
				o := newOperativeRemoteFixture(t, engine)
				o.activateRemote(t)
				o.release(t, time.Now().Add(-5*time.Second))
				if got := o.run(t, true); got.Phase != "active" {
					t.Fatalf("a released session drained the barrier: %+v", got)
				}
				if o.remoteResumes(t) != 0 {
					t.Fatal("a released session released the source hold")
				}
				o.setSession(t, time.Now().Add(time.Minute), true)
				if got := o.run(t, true); got.Phase != "active" {
					t.Fatalf("a reconnect after release did not keep the barrier active: %+v", got)
				}
			})
			t.Run("ReleasedBeyondGraceDrains", func(t *testing.T) {
				o := newOperativeRemoteFixture(t, engine)
				o.activateRemote(t)
				o.release(t, time.Now().Add(-10*time.Minute))
				if got := o.run(t, true); got.Phase != "draining" {
					t.Fatalf("a source released long ago did not drain: %+v", got)
				}
			})
			t.Run("ReleasedWithoutContactDrains", func(t *testing.T) {
				o := newOperativeRemoteFixture(t, engine)
				o.activateRemote(t)
				if _, err := o.f.f.db.ExecContext(t.Context(), "UPDATE probe_sessions SET last_contact_at=NULL WHERE probe_id=?", o.f.session.ProbeID); err != nil {
					t.Fatal(err)
				}
				o.release(t, time.Time{})
				if got := o.run(t, true); got.Phase != "draining" {
					t.Fatalf("a released session with no contact did not drain: %+v", got)
				}
			})
			t.Run("LapseBeyondGraceDrains", func(t *testing.T) {
				o := newOperativeRemoteFixture(t, engine)
				o.activateRemote(t)
				o.setSession(t, time.Now().Add(-10*time.Minute), false)
				if got := o.run(t, true); got.Phase != "draining" {
					t.Fatalf("a long-gone source did not drain: %+v", got)
				}
				if _, reason := o.phase(t); reason != "cutover_evidence_incomplete" {
					t.Fatalf("drain reason = %q", reason)
				}
			})
			t.Run("FailedReconnectAttemptsHoldActive", func(t *testing.T) {
				// While the probe is down every dial takes a generation and
				// releases it; none of them is a session that withheld
				// attestation.
				o := newOperativeRemoteFixture(t, engine)
				o.activateRemote(t)
				o.release(t, time.Now().Add(-5*time.Second))
				for range 3 {
					o.setSession(t, time.Now().Add(time.Minute), true)
					o.release(t, time.Time{})
				}
				if got := o.run(t, true); got.Phase != "active" {
					t.Fatalf("failed reconnect attempts drained the barrier: %+v", got)
				}
			})
			t.Run("RepeatedReconnectsBeyondGraceDrain", func(t *testing.T) {
				o := newOperativeRemoteFixture(t, engine)
				o.activateRemote(t)
				o.setSession(t, time.Now().Add(time.Minute), true)
				o.setSession(t, time.Now().Add(-10*time.Minute), true)
				if got := o.run(t, true); got.Phase != "draining" {
					t.Fatalf("a source gone past the grace did not drain: %+v", got)
				}
			})
			t.Run("ConnectedUnattestedBeyondGraceDrains", func(t *testing.T) {
				// #221 (RC4-C01): a probe that reconnects on a new session
				// generation and keeps renewing its live lease and last_contact_at
				// without re-attesting capability must not hold the barrier past
				// operativeSessionLapseGrace.
				o := newOperativeRemoteFixture(t, engine)
				o.activateRemote(t)
				past := time.Now().UTC().Add(-10 * time.Minute)
				if _, err := o.f.f.db.ExecContext(t.Context(), "UPDATE operative_source_capabilities SET attested_at=? WHERE probe_id=?", past, o.f.session.ProbeID); err != nil {
					t.Fatal(err)
				}
				o.setSession(t, time.Now().Add(time.Minute), true)
				if _, err := o.f.f.db.ExecContext(t.Context(), "UPDATE probe_sessions SET last_contact_at=? WHERE probe_id=?", time.Now().Unix(), o.f.session.ProbeID); err != nil {
					t.Fatal(err)
				}
				if got := o.run(t, true); got.Phase != "draining" {
					t.Fatalf("connected unattested session held barrier past grace: %+v", got)
				}
				if _, reason := o.phase(t); reason != "cutover_evidence_incomplete" {
					t.Fatalf("drain reason = %q, want cutover_evidence_incomplete", reason)
				}
			})
			t.Run("WithdrawnCapabilityDrains", func(t *testing.T) {
				o := newOperativeRemoteFixture(t, engine)
				o.activateRemote(t)
				if err := o.store.SetOperativeSourceCapability(t.Context(), o.f.session, false); err != nil {
					t.Fatal(err)
				}
				if got := o.run(t, true); got.Phase != "draining" {
					t.Fatalf("a withdrawn capability did not drain: %+v", got)
				}
			})
			t.Run("ReconnectReleasesBackedOffDrain", func(t *testing.T) {
				// #222: when a barrier enters draining (e.g. configuration_changed),
				// a delivery.resume claimed on a dying connection backs off
				// next_attempt_at, and a retained source-scope provider attempt may
				// also be in_flight. Reconnecting and re-attesting capability must
				// make delivery.resume claimable immediately and allow the drain to
				// release without waiting on source-scope attempts.
				o := newOperativeRemoteFixture(t, engine)
				o.activateRemote(t)
				if got := o.run(t, false); got.Phase != "draining" {
					t.Fatalf("expected draining, got %+v", got)
				}
				if got := o.run(t, false); got.Phase != "draining" || o.openResumes(t) != 1 {
					t.Fatalf("expected 1 open resume command, got %+v (%d)", got, o.openResumes(t))
				}
				// Claim delivery.resume on the dying session so next_attempt_at is pushed into the future.
				commands := o.f.commands(t)
				firstClaim, err := commands.ClaimCommand(t.Context(), o.f.session, time.Second, domain.ProbeCommandCapabilities{DeliveryCutover: true})
				if err != nil || firstClaim == nil || firstClaim.Kind != domain.DeliveryResumeKind {
					t.Fatalf("initial resume claim failed: %v %+v", err, firstClaim)
				}
				// Insert an in_flight source-scope provider attempt (as happens in both mode).
				now := time.Now().UTC().Truncate(time.Microsecond)
				if _, err := o.f.f.db.ExecContext(t.Context(),
					`INSERT INTO operative_provider_attempts (attempt_id, scope, monitor_id, probe_id, delivery_id, claim_attempt, notification_id, budget_until, state, created_at) VALUES (?, 'source', 1, 'local', ?, 1, 1, ?, 'in_flight', ?)`,
					probeRegistryID2, probeRegistryID3, now.Add(time.Minute), now); err != nil {
					t.Fatal(err)
				}
				// Reconnect on a new generation and re-attest capability.
				o.setSession(t, time.Now().Add(time.Minute), true)
				o.f.session.ConnectionGeneration++
				if err := o.store.SetOperativeSourceCapability(t.Context(), o.f.session, true); err != nil {
					t.Fatal(err)
				}
				// Resume must be immediately claimable on the new session and release the barrier.
				o.complete(t, domain.DeliveryResumeKind)
				if got := o.run(t, true); got.Phase != "released" {
					t.Fatalf("expected drain to release after reconnect (#222), got %+v", got)
				}
			})
		})
	}
}
