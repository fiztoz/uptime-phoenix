package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// Every live probe session polls ClaimCommand four times a second, and almost
// every poll finds nothing to send. An idle poll must not take the probe row
// lock: replay ingest holds that lock for its whole transaction, and a poll
// parked behind it keeps a pooled connection that control-plane requests need.
func TestProbeCommandIdleClaimDoesNotWaitForProbeLock(t *testing.T) {
	f := newCommandFixture(t, "mariadb")
	tx, err := f.f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(t.Context(), "UPDATE probes SET id = id WHERE id = ?", f.session.ProbeID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	start := time.Now()
	got, err := f.commands.ClaimCommand(ctx, f.session, time.Second, domain.ProbeCommandCapabilities{AlertAcknowledgement: true, CredentialRotation: true, CertificateRotation: true, DeliveryPreparation: true, DeliveryCutover: true})
	if elapsed := time.Since(start); err != nil || got != nil || elapsed > time.Second {
		t.Fatalf("idle claim waited for the probe lock: got=%v err=%v after %s", got, err, elapsed)
	}
}

// The lock-free idle check is only a hint: once a command is pending the claim
// still takes the session lock, so it waits for the holder and then dispatches.
func TestProbeCommandPendingClaimStillTakesProbeLock(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newCommandFixture(t, engine)
			caps := domain.ProbeCommandCapabilities{AlertAcknowledgement: true}
			if got, err := f.commands.ClaimCommand(t.Context(), f.session, time.Second, caps); err != nil || got != nil {
				t.Fatalf("empty queue claimed %v: %v", got, err)
			}
			f.issue(t)
			if engine == "mariadb" {
				tx, err := f.f.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), "UPDATE probes SET id = id WHERE id = ?", f.session.ProbeID); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
				_, err = f.commands.ClaimCommand(ctx, f.session, time.Second, caps)
				cancel()
				_ = tx.Rollback()
				if err == nil {
					t.Fatal("pending claim bypassed the probe session lock")
				}
			}
			got, err := f.commands.ClaimCommand(t.Context(), f.session, time.Second, caps)
			if err != nil || got == nil || got.CommandID != f.ack.CommandID {
				t.Fatalf("pending command not claimed: %v %v", got, err)
			}
		})
	}
}
