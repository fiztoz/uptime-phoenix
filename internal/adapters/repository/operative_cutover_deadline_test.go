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

// age moves the open barrier's creation, preparing start and every member's
// resume receipt back by d, as if that much DB-clock time had passed.
func (o operativeRemoteFixture) age(t *testing.T, d time.Duration) {
	t.Helper()
	for _, stmt := range []string{
		"UPDATE operative_cutover_state SET created_at = ?",
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
			t.Run("StalledPreparingDrains", func(t *testing.T) { testCutoverDeadlinePreparing(t, engine) })
			t.Run("DisabledPreparingArmsOnEnable", func(t *testing.T) { testCutoverDeadlineDisabledPreparing(t, engine) })
			t.Run("DisabledPreparingCompletesOnEnable", func(t *testing.T) { testCutoverDeadlineDisabledPreparingCompletes(t, engine) })
			t.Run("StalledQuiesceDrains", func(t *testing.T) { testCutoverDeadlineQuiescing(t, engine) })
			t.Run("StalledApplyReleasesQuiescedMember", func(t *testing.T) { testCutoverDeadlineApplying(t, engine) })
			t.Run("ReentryBacksOff", func(t *testing.T) { testCutoverDeadlineBackoff(t, engine) })
			t.Run("RefusalBlockDrains", func(t *testing.T) { testCutoverDeadlineRefusalBlock(t, engine) })
		})
	}
}

// A monitor stuck in preparing because a member probe never completes
// preparation or capability attestation must also hit the activation deadline
// and exponential re-entry backoff (#217).
func testCutoverDeadlinePreparing(t *testing.T, engine string) {
	o := newOperativeRemoteFixture(t, engine)
	// Remote probe has not attested cutover capability, so preparing stays pending.
	if got := o.run(t, true); got.Phase != "preparing" || got.Reason != "" {
		t.Fatalf("within deadline preparing = %+v, want preparing", got)
	}
	if o.gates(t) != 0 {
		t.Fatalf("expected 0 barriers before deadline, got %d", o.gates(t))
	}
	o.age(t, 6*time.Minute)
	got := o.run(t, true)
	if got.Phase != "released" || got.Reason != domain.CutoverReasonDeadlineExceeded {
		t.Fatalf("stalled preparing past its deadline = %+v, want released %s", got, domain.CutoverReasonDeadlineExceeded)
	}
	// Backoff holds the monitor in released before re-entering preparing.
	if got := o.run(t, true); got.Phase != "released" || got.Reason != domain.CutoverReasonDeadlineExceeded {
		t.Fatalf("re-armed immediately after preparing deadline: %+v", got)
	}
	o.age(t, 16*time.Minute)
	if got := o.run(t, true); got.Phase != "preparing" || got.Reason != "" {
		t.Fatalf("first preparing backoff did not end: %+v", got)
	}
	// A second consecutive preparing timeout doubles the backoff to 30m.
	o.age(t, 6*time.Minute)
	if got := o.run(t, true); got.Phase != "released" || got.Reason != domain.CutoverReasonDeadlineExceeded {
		t.Fatalf("second stalled preparing past deadline = %+v, want released %s", got, domain.CutoverReasonDeadlineExceeded)
	}
	o.age(t, 16*time.Minute)
	if got := o.run(t, true); got.Phase != "released" {
		t.Fatalf("second preparing failure did not double backoff: %+v", got)
	}
	o.age(t, 31*time.Minute)
	if got := o.run(t, true); got.Phase != "preparing" {
		t.Fatalf("doubled preparing backoff did not end: %+v", got)
	}
}

// Time spent claimed while the opt-in is disabled is not preparation time. A
// monitor claimed into preparing while disabled for longer than the deadline
// used to release with cutover_deadline_exceeded on its very first enabled
// pass and sit out the 15-minute backoff (#217).
func testCutoverDeadlineDisabledPreparing(t *testing.T, engine string) {
	o := newOperativeRemoteFixture(t, engine)
	if got := o.run(t, false); got.Phase != "preparing" || got.Reason != "feature_disabled" {
		t.Fatalf("disabled preparing = %+v, want preparing feature_disabled", got)
	}
	o.age(t, 6*time.Minute)
	if got := o.run(t, false); got.Phase != "preparing" {
		t.Fatalf("disabled preparing hit a deadline: %+v", got)
	}
	o.age(t, 6*time.Minute)
	// Enabled with capability evidence still incomplete: a fresh window opens.
	if got := o.run(t, true); got.Phase != "preparing" || got.Reason != "" {
		t.Fatalf("first enabled pass = %+v, want preparing with a fresh deadline", got)
	}
	if o.gates(t) != 0 {
		t.Fatalf("expected 0 barriers inside the fresh window, got %d", o.gates(t))
	}
	if got := o.run(t, true); got.Phase != "preparing" {
		t.Fatalf("second enabled pass inside the window = %+v", got)
	}
	// The re-armed window is still bounded.
	o.age(t, 6*time.Minute)
	if got := o.run(t, true); got.Phase != "released" || got.Reason != domain.CutoverReasonDeadlineExceeded {
		t.Fatalf("re-armed preparing past its deadline = %+v, want released %s", got, domain.CutoverReasonDeadlineExceeded)
	}
}

// A monitor whose evidence is complete by the first enabled pass opens its
// barrier, however long it sat disabled.
func testCutoverDeadlineDisabledPreparingCompletes(t *testing.T, engine string) {
	o := newOperativeRemoteFixture(t, engine)
	o.run(t, false)
	o.age(t, 6*time.Minute)
	if err := o.store.SetOperativeSourceCapability(t.Context(), o.f.session, true); err != nil {
		t.Fatal(err)
	}
	if got := o.run(t, true); got.Phase != "quiescing" {
		t.Fatalf("prepared monitor did not open its barrier after enabling: %+v", got)
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
