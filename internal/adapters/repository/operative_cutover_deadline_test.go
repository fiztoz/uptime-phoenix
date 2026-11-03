package repository_test

import (
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func (o operativeRemoteFixture) state(t *testing.T) operativeStateRow {
	t.Helper()
	var row operativeStateRow
	if err := o.f.f.db.NewSelect().Table("operative_cutover_state").Column("phase", "barrier_id", "reason").Scan(t.Context(), &row); err != nil {
		t.Fatal(err)
	}
	return row
}

// age moves the open barrier's creation and every member's resume receipt back
// by d, as if that much DB-clock time had passed.
func (o operativeRemoteFixture) age(t *testing.T, d time.Duration) {
	t.Helper()
	for _, stmt := range []string{
		"UPDATE operative_cutover_barriers SET created_at = ?",
		"UPDATE operative_cutover_members SET resumed_at = ? WHERE resumed_at IS NOT NULL",
	} {
		if _, err := o.f.f.db.ExecContext(t.Context(), stmt, time.Now().UTC().Add(-d).Truncate(time.Microsecond)); err != nil {
			t.Fatal(stmt, err)
		}
	}
}

func (o operativeRemoteFixture) gates(t *testing.T) int64 {
	t.Helper()
	n, err := o.f.f.db.NewSelect().Table("operative_cutover_barriers").ColumnExpr("COUNT(*)").Count(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (o operativeRemoteFixture) openResumes(t *testing.T) int64 {
	t.Helper()
	n, err := o.f.f.db.NewSelect().Table("operative_cutover_commands").ColumnExpr("COUNT(*)").
		Where("kind = ? AND outcome_status = ''", domain.DeliveryResumeKind).Count(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// drainToReleased issues and completes the member's resume.
func (o operativeRemoteFixture) drainToReleased(t *testing.T) {
	t.Helper()
	for i := 0; i < 3 && o.openResumes(t) == 0; i++ {
		o.run(t, true)
	}
	o.complete(t, domain.DeliveryResumeKind)
	if got := o.run(t, true); got.Phase != "released" {
		t.Fatalf("resumed barrier did not release: %+v", got)
	}
}

// A barrier that cannot reach ACTIVE must not hold its members forever (#209).
// A member that quiesced keeps suppressing its regional paging until the
// barrier drains, so a peer that never finishes (offline, slow, or refusing)
// left every quiesced member unpaged indefinitely.
func TestOperativeCutoverDeadline(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("StalledQuiesceDrains", func(t *testing.T) { testCutoverDeadlineQuiescing(t, engine) })
			t.Run("StalledApplyReleasesQuiescedMember", func(t *testing.T) { testCutoverDeadlineApplying(t, engine) })
			t.Run("ReentryBacksOff", func(t *testing.T) { testCutoverDeadlineBackoff(t, engine) })
			t.Run("RefusalBlockDrains", func(t *testing.T) { testCutoverDeadlineRefusalBlock(t, engine) })
		})
	}
}

func newDeadlineFixture(t *testing.T, engine string) operativeRemoteFixture {
	t.Helper()
	o := newOperativeRemoteFixture(t, engine)
	if err := o.store.SetOperativeSourceCapability(t.Context(), o.f.session, true); err != nil {
		t.Fatal(err)
	}
	if got := o.run(t, true); got.Phase != "quiescing" {
		t.Fatalf("barrier did not open: %+v", got)
	}
	return o
}

func testCutoverDeadlineQuiescing(t *testing.T, engine string) {
	o := newDeadlineFixture(t, engine)
	// Within the deadline an unanswered quiesce just keeps waiting.
	if got := o.run(t, true); got.Phase != "quiescing" {
		t.Fatalf("barrier drained before its deadline: %+v", got)
	}
	o.age(t, 6*time.Minute)
	got := o.run(t, true)
	if got.Phase != "draining" || got.Reason != domain.CutoverReasonDeadlineExceeded {
		t.Fatalf("stalled quiesce past its deadline = %+v, want draining %s", got, domain.CutoverReasonDeadlineExceeded)
	}
	o.drainToReleased(t)
}

func testCutoverDeadlineApplying(t *testing.T, engine string) {
	o := newDeadlineFixture(t, engine)
	o.complete(t, domain.DeliveryQuiesceKind)
	for _, phase := range []string{"quiesced", "applying"} {
		if got := o.run(t, true); got.Phase != phase {
			t.Fatalf("lifecycle stage %s, got %+v", phase, got)
		}
	}
	// The member is quiesced and held; its application never completes.
	o.age(t, 6*time.Minute)
	if got := o.run(t, true); got.Phase != "draining" || got.Reason != domain.CutoverReasonDeadlineExceeded {
		t.Fatalf("stalled apply past its deadline = %+v, want draining", got)
	}
	for i := 0; i < 3 && o.remoteResumes(t) == 0; i++ {
		o.run(t, true)
	}
	if o.remoteResumes(t) != 1 {
		t.Fatal("deadline drain did not send the quiesced member its resume")
	}
}

func testCutoverDeadlineBackoff(t *testing.T, engine string) {
	o := newDeadlineFixture(t, engine)
	o.age(t, 6*time.Minute)
	o.run(t, true)
	o.drainToReleased(t)
	if got := o.state(t); got.Reason != domain.CutoverReasonDeadlineExceeded {
		t.Fatalf("released barrier lost its failure reason: %+v", got)
	}
	// Still desired and still enabled, but it just failed: no new barrier yet.
	for i := 0; i < 2; i++ {
		if got := o.run(t, true); got.Phase != "released" {
			t.Fatalf("re-armed immediately after a deadline drain: %+v", got)
		}
	}
	if o.gates(t) != 1 {
		t.Fatal("a new barrier opened inside the backoff")
	}
	// After the first backoff it tries again, with a fresh barrier.
	o.age(t, 16*time.Minute)
	if got := o.run(t, true); got.Phase != "preparing" {
		t.Fatalf("backoff did not end: %+v", got)
	}
	if got := o.run(t, true); got.Phase != "quiescing" {
		t.Fatalf("retry did not open a new barrier: %+v", got)
	}
	if o.gates(t) != 2 {
		t.Fatal("retry reused the failed barrier")
	}
	// A second consecutive failure doubles the wait.
	o.age(t, 6*time.Minute)
	o.run(t, true)
	o.drainToReleased(t)
	o.age(t, 16*time.Minute)
	if got := o.run(t, true); got.Phase != "released" {
		t.Fatalf("second failure did not double the backoff: %+v", got)
	}
	o.age(t, 31*time.Minute)
	if got := o.run(t, true); got.Phase != "preparing" {
		t.Fatalf("doubled backoff did not end: %+v", got)
	}
}

// reject refuses the member's one open command of kind with a non-busy code.
func (o operativeRemoteFixture) reject(t *testing.T, kind, code string) {
	t.Helper()
	commands := o.f.commands(t)
	claimed, err := commands.ClaimCommand(t.Context(), o.f.session, time.Second, domain.ProbeCommandCapabilities{DeliveryCutover: true})
	if err != nil || claimed == nil || claimed.Kind != kind {
		t.Fatal("cutover command not claimable", err, claimed)
	}
	if err := commands.CompleteCommand(t.Context(), o.f.session, domain.ProbeCommandOutcome{CommandID: claimed.CommandID, Status: "rejected", Code: code}); err != nil {
		t.Fatal(err)
	}
}

// A permanent (non-busy) refusal blocks the barrier for review, but a blocked
// pre-active barrier still holds every member that already quiesced, and only
// disabling the opt-in used to drain it.
func testCutoverDeadlineRefusalBlock(t *testing.T, engine string) {
	o := newDeadlineFixture(t, engine)
	o.reject(t, domain.DeliveryQuiesceKind, "delivery_document_mismatch")
	if got := o.run(t, true); got.Phase != "blocked" || got.Reason != "quiesce_refused" {
		t.Fatalf("permanent refusal = %+v, want blocked quiesce_refused", got)
	}
	if got := o.run(t, true); got.Phase != "blocked" {
		t.Fatalf("refusal block drained before its deadline: %+v", got)
	}
	o.age(t, 6*time.Minute)
	if got := o.run(t, true); got.Phase != "draining" || got.Reason != domain.CutoverReasonDeadlineExceeded {
		t.Fatalf("refusal block past its deadline = %+v, want draining", got)
	}
	o.drainToReleased(t)
}
