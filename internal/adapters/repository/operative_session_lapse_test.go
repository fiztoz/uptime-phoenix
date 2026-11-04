package repository_test

import (
	"testing"
	"time"

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
			t.Run("SecondReconnectDrains", func(t *testing.T) {
				o := newOperativeRemoteFixture(t, engine)
				o.activateRemote(t)
				o.setSession(t, time.Now().Add(time.Minute), true)
				o.setSession(t, time.Now().Add(time.Minute), true)
				if got := o.run(t, true); got.Phase != "draining" {
					t.Fatalf("a source that never re-attested did not drain: %+v", got)
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
		})
	}
}
