package repository_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// F2.3 escalation repository contract. It runs on BOTH engines, because SQLite
// silently accepts things MariaDB rejects and stores higher-precision
// timestamps than MariaDB's second-granularity TIMESTAMP — the two facts that
// have produced the last several shipped repository bugs in this project.

func TestEscalationContract_SQLite(t *testing.T) {
	runEscalationContract(t, sqliteFactory)
}

func TestEscalationContract_MariaDB(t *testing.T) {
	if os.Getenv("TEST_MARIADB_DSN") == "" {
		t.Skip("TEST_MARIADB_DSN is unset; skipping MariaDB escalation contract")
	}
	runEscalationContract(t, mariadbFactory)
}

func runEscalationContract(t *testing.T, factory repositoryFactory) {
	t.Helper()

	t.Run("PolicyRoundTripWithSteps", func(t *testing.T) {
		repos := factory(t)
		ctx := context.Background()
		user := createUser(t, ctx, repos, "esc-crud")
		n1 := createEscNotification(t, ctx, repos, user.ID, "primary")
		n2 := createEscNotification(t, ctx, repos, user.ID, "backup")

		p := &domain.EscalationPolicy{
			UserID: user.ID, Name: "Payments ladder", Description: "on-call", Enabled: true,
			Steps: []domain.EscalationStep{
				{StepOrder: 1, WaitMinutes: 5, NotificationIDs: []int64{n1.ID}},
				{StepOrder: 2, WaitMinutes: 10, NotificationIDs: []int64{n1.ID, n2.ID}},
			},
		}
		if err := repos.escalationPolicies.Create(ctx, p); err != nil {
			t.Fatalf("Create policy: %v", err)
		}
		if p.ID == 0 || p.CreatedAt.IsZero() || p.UpdatedAt.IsZero() {
			t.Fatalf("created policy missing generated fields: %+v", p)
		}

		got, err := repos.escalationPolicies.GetByID(ctx, p.ID)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if got.Name != "Payments ladder" || !got.Enabled {
			t.Fatalf("policy round trip = %+v", got)
		}
		if len(got.Steps) != 2 {
			t.Fatalf("steps = %d; want 2", len(got.Steps))
		}
		if got.Steps[0].StepOrder != 1 || got.Steps[1].StepOrder != 2 {
			t.Fatalf("steps out of order: %d then %d", got.Steps[0].StepOrder, got.Steps[1].StepOrder)
		}
		if len(got.Steps[0].NotificationIDs) != 1 || got.Steps[0].NotificationIDs[0] != n1.ID {
			t.Fatalf("step 1 channels = %v; want [%d]", got.Steps[0].NotificationIDs, n1.ID)
		}
		if len(got.Steps[1].NotificationIDs) != 2 {
			t.Fatalf("step 2 channels = %v; want 2", got.Steps[1].NotificationIDs)
		}

		list, err := repos.escalationPolicies.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list) != 1 || len(list[0].Steps) != 2 {
			t.Fatalf("List did not populate steps: %+v", list)
		}
	})

	// Update is a REPLACE-SET on steps. A partial list drops the rest, exactly
	// like PUT /api/status-pages/:spId/monitors — pinned here so nobody
	// "optimizes" it into a merge.
	t.Run("UpdateReplacesTheWholeStepLadder", func(t *testing.T) {
		repos := factory(t)
		ctx := context.Background()
		user := createUser(t, ctx, repos, "esc-update")
		n1 := createEscNotification(t, ctx, repos, user.ID, "primary")

		p := &domain.EscalationPolicy{
			UserID: user.ID, Name: "L", Enabled: true,
			Steps: []domain.EscalationStep{
				{StepOrder: 1, WaitMinutes: 5, NotificationIDs: []int64{n1.ID}},
				{StepOrder: 2, WaitMinutes: 10, NotificationIDs: []int64{n1.ID}},
				{StepOrder: 3, WaitMinutes: 15, NotificationIDs: []int64{n1.ID}},
			},
		}
		if err := repos.escalationPolicies.Create(ctx, p); err != nil {
			t.Fatalf("Create: %v", err)
		}

		p.Name = "L2"
		p.Enabled = false
		p.Steps = []domain.EscalationStep{{StepOrder: 1, WaitMinutes: 1, NotificationIDs: []int64{n1.ID}}}
		if err := repos.escalationPolicies.Update(ctx, p); err != nil {
			t.Fatalf("Update: %v", err)
		}

		got, err := repos.escalationPolicies.GetByID(ctx, p.ID)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if got.Name != "L2" || got.Enabled {
			t.Fatalf("policy fields not updated: %+v", got)
		}
		if len(got.Steps) != 1 || got.Steps[0].WaitMinutes != 1 {
			t.Fatalf("steps after replace-set = %+v; want exactly one 1-minute step", got.Steps)
		}
	})

	t.Run("UpdateUnknownPolicyIsNotFound", func(t *testing.T) {
		repos := factory(t)
		ctx := context.Background()
		err := repos.escalationPolicies.Update(ctx, &domain.EscalationPolicy{ID: 987654, Name: "ghost"})
		if !errors.Is(err, ports.ErrNotFound) {
			t.Fatalf("Update unknown policy err = %v; want ErrNotFound", err)
		}
	})

	// The UNIQUE is on the entity column, so re-assigning REPLACES rather than
	// accumulating. That is contract 1's "at most one policy" enforced by the
	// schema, not by application code.
	t.Run("AssignmentIsAtMostOnePerEntity", func(t *testing.T) {
		repos := factory(t)
		ctx := context.Background()
		user := createUser(t, ctx, repos, "esc-assign")
		monitor := createMonitor(t, ctx, repos, user.ID, "assigned")
		group := createEscGroup(t, ctx, repos, user.ID, "folder")
		n1 := createEscNotification(t, ctx, repos, user.ID, "primary")
		p1 := createEscPolicy(t, ctx, repos, user.ID, "one", n1.ID)
		p2 := createEscPolicy(t, ctx, repos, user.ID, "two", n1.ID)

		if _, err := repos.escalationAssignments.PolicyIDForMonitor(ctx, monitor.ID); !errors.Is(err, ports.ErrNotFound) {
			t.Fatalf("unassigned monitor err = %v; want ErrNotFound", err)
		}

		if err := repos.escalationAssignments.AssignMonitor(ctx, monitor.ID, p1.ID); err != nil {
			t.Fatalf("AssignMonitor: %v", err)
		}
		if err := repos.escalationAssignments.AssignMonitor(ctx, monitor.ID, p2.ID); err != nil {
			t.Fatalf("re-AssignMonitor: %v", err)
		}
		got, err := repos.escalationAssignments.PolicyIDForMonitor(ctx, monitor.ID)
		if err != nil {
			t.Fatalf("PolicyIDForMonitor: %v", err)
		}
		if got != p2.ID {
			t.Fatalf("monitor assignment = %d; want the replacement %d", got, p2.ID)
		}

		if assignErr := repos.escalationAssignments.AssignGroup(ctx, group.ID, p1.ID); assignErr != nil {
			t.Fatalf("AssignGroup: %v", assignErr)
		}
		if assignErr := repos.escalationAssignments.AssignGroup(ctx, group.ID, p2.ID); assignErr != nil {
			t.Fatalf("re-AssignGroup: %v", assignErr)
		}
		gotGroup, err := repos.escalationAssignments.PolicyIDForGroup(ctx, group.ID)
		if err != nil {
			t.Fatalf("PolicyIDForGroup: %v", err)
		}
		if gotGroup != p2.ID {
			t.Fatalf("group assignment = %d; want %d", gotGroup, p2.ID)
		}

		if err := repos.escalationAssignments.UnassignMonitor(ctx, monitor.ID); err != nil {
			t.Fatalf("UnassignMonitor: %v", err)
		}
		if _, err := repos.escalationAssignments.PolicyIDForMonitor(ctx, monitor.ID); !errors.Is(err, ports.ErrNotFound) {
			t.Fatalf("after unassign err = %v; want ErrNotFound", err)
		}
		// Unassigning twice is not an error.
		if err := repos.escalationAssignments.UnassignMonitor(ctx, monitor.ID); err != nil {
			t.Fatalf("second UnassignMonitor: %v", err)
		}
		if err := repos.escalationAssignments.UnassignGroup(ctx, group.ID); err != nil {
			t.Fatalf("UnassignGroup: %v", err)
		}
	})

	t.Run("StateCreateIsUniquePerAlert", func(t *testing.T) {
		repos := factory(t)
		ctx := context.Background()
		user := createUser(t, ctx, repos, "esc-state")
		monitor := createMonitor(t, ctx, repos, user.ID, "state")
		n1 := createEscNotification(t, ctx, repos, user.ID, "primary")
		p := createEscPolicy(t, ctx, repos, user.ID, "ladder", n1.ID)
		alert := createEscAlert(t, ctx, repos, monitor.ID, "tok-unique")

		due := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
		e := &domain.AlertEscalation{
			AlertID: alert.ID, MonitorID: monitor.ID, PolicyID: p.ID,
			NextStep: 1, NextRunAt: due, Status: domain.EscalationStatePending,
		}
		if err := repos.alertEscalations.Create(ctx, e); err != nil {
			t.Fatalf("Create escalation: %v", err)
		}
		dup := &domain.AlertEscalation{
			AlertID: alert.ID, MonitorID: monitor.ID, PolicyID: p.ID,
			NextStep: 1, NextRunAt: due, Status: domain.EscalationStatePending,
		}
		if err := repos.alertEscalations.Create(ctx, dup); !errors.Is(err, ports.ErrConflict) {
			t.Fatalf("duplicate escalation err = %v; want ErrConflict", err)
		}

		got, err := repos.alertEscalations.GetByAlertID(ctx, alert.ID)
		if err != nil {
			t.Fatalf("GetByAlertID: %v", err)
		}
		if got.NextStep != 1 || got.Status != domain.EscalationStatePending {
			t.Fatalf("state round trip = %+v", got)
		}
	})

	// The core safety property: two claim tokens racing the same due row, and
	// exactly one wins. Run against real SQL rather than a fake, because the
	// whole point is the engine's UPDATE semantics.
	t.Run("ClaimDueIsExactlyOnceAcrossWorkers", func(t *testing.T) {
		repos := factory(t)
		ctx := context.Background()
		user := createUser(t, ctx, repos, "esc-claim")
		monitor := createMonitor(t, ctx, repos, user.ID, "claim")
		n1 := createEscNotification(t, ctx, repos, user.ID, "primary")
		p := createEscPolicy(t, ctx, repos, user.ID, "ladder", n1.ID)
		alert := createEscAlert(t, ctx, repos, monitor.ID, "tok-claim")

		now := time.Now().UTC().Truncate(time.Second)
		e := &domain.AlertEscalation{
			AlertID: alert.ID, MonitorID: monitor.ID, PolicyID: p.ID,
			NextStep: 1, NextRunAt: now.Add(-time.Minute), Status: domain.EscalationStatePending,
		}
		if err := repos.alertEscalations.Create(ctx, e); err != nil {
			t.Fatalf("Create escalation: %v", err)
		}

		var mu sync.Mutex
		wins := 0
		var wg sync.WaitGroup
		for _, token := range []string{"worker-a:aaa", "worker-b:bbb"} {
			wg.Add(1)
			go func(tok string) {
				defer wg.Done()
				claimed, err := repos.alertEscalations.ClaimDue(ctx, tok, now, now.Add(2*time.Minute))
				if err != nil {
					t.Errorf("ClaimDue(%s): %v", tok, err)
					return
				}
				mu.Lock()
				wins += len(claimed)
				mu.Unlock()
			}(token)
		}
		wg.Wait()

		if wins != 1 {
			t.Fatalf("rows claimed across two workers = %d; want exactly 1", wins)
		}
	})

	t.Run("ClaimSkipsFutureAndRespectsLiveLease", func(t *testing.T) {
		repos := factory(t)
		ctx := context.Background()
		user := createUser(t, ctx, repos, "esc-lease")
		monitor := createMonitor(t, ctx, repos, user.ID, "lease")
		n1 := createEscNotification(t, ctx, repos, user.ID, "primary")
		p := createEscPolicy(t, ctx, repos, user.ID, "ladder", n1.ID)

		now := time.Now().UTC().Truncate(time.Second)

		future := createEscAlert(t, ctx, repos, monitor.ID, "tok-future")
		if err := repos.alertEscalations.Create(ctx, &domain.AlertEscalation{
			AlertID: future.ID, MonitorID: monitor.ID, PolicyID: p.ID, NextStep: 1,
			NextRunAt: now.Add(10 * time.Minute), Status: domain.EscalationStatePending,
		}); err != nil {
			t.Fatalf("Create future escalation: %v", err)
		}
		claimed, err := repos.alertEscalations.ClaimDue(ctx, "w:1", now, now.Add(time.Minute))
		if err != nil {
			t.Fatalf("ClaimDue: %v", err)
		}
		if len(claimed) != 0 {
			t.Fatalf("claimed %d not-yet-due rows; want 0", len(claimed))
		}

		// Resolve the future alert so the next one can open on the same monitor.
		resolveEscAlert(t, ctx, repos, future)

		dueAlert := createEscAlert(t, ctx, repos, monitor.ID, "tok-due")
		if createErr := repos.alertEscalations.Create(ctx, &domain.AlertEscalation{
			AlertID: dueAlert.ID, MonitorID: monitor.ID, PolicyID: p.ID, NextStep: 1,
			NextRunAt: now.Add(-time.Minute), Status: domain.EscalationStatePending,
		}); createErr != nil {
			t.Fatalf("Create due escalation: %v", createErr)
		}
		first, err := repos.alertEscalations.ClaimDue(ctx, "w:1", now, now.Add(5*time.Minute))
		if err != nil {
			t.Fatalf("first ClaimDue: %v", err)
		}
		if len(first) != 1 {
			t.Fatalf("first claim = %d rows; want 1", len(first))
		}
		second, err := repos.alertEscalations.ClaimDue(ctx, "w:2", now, now.Add(5*time.Minute))
		if err != nil {
			t.Fatalf("second ClaimDue: %v", err)
		}
		if len(second) != 0 {
			t.Fatalf("second worker stole a live lease: %d rows", len(second))
		}

		// Once the lease expires the row becomes claimable again — this is what
		// recovers a crashed worker's rows.
		later := now.Add(6 * time.Minute)
		third, err := repos.alertEscalations.ClaimDue(ctx, "w:3", later, later.Add(5*time.Minute))
		if err != nil {
			t.Fatalf("third ClaimDue: %v", err)
		}
		if len(third) != 1 {
			t.Fatalf("expired lease was not reclaimable: %d rows", len(third))
		}
	})

	t.Run("AdvanceAndFinishAreGuardedByTheClaimToken", func(t *testing.T) {
		repos := factory(t)
		ctx := context.Background()
		user := createUser(t, ctx, repos, "esc-guard")
		monitor := createMonitor(t, ctx, repos, user.ID, "guard")
		n1 := createEscNotification(t, ctx, repos, user.ID, "primary")
		p := createEscPolicy(t, ctx, repos, user.ID, "ladder", n1.ID)
		alert := createEscAlert(t, ctx, repos, monitor.ID, "tok-guard")

		now := time.Now().UTC().Truncate(time.Second)
		e := &domain.AlertEscalation{
			AlertID: alert.ID, MonitorID: monitor.ID, PolicyID: p.ID, NextStep: 1,
			NextRunAt: now.Add(-time.Minute), Status: domain.EscalationStatePending,
		}
		if err := repos.alertEscalations.Create(ctx, e); err != nil {
			t.Fatalf("Create: %v", err)
		}
		claimed, err := repos.alertEscalations.ClaimDue(ctx, "owner:1", now, now.Add(5*time.Minute))
		if err != nil || len(claimed) != 1 {
			t.Fatalf("ClaimDue = %v, %v", claimed, err)
		}

		// A worker whose lease expired mid-send must not be able to write.
		ok, err := repos.alertEscalations.Advance(ctx, e.ID, "stale-owner", 2, now.Add(time.Minute))
		if err != nil {
			t.Fatalf("Advance with stale token: %v", err)
		}
		if ok {
			t.Fatal("Advance succeeded with a stale claim token")
		}

		ok, err = repos.alertEscalations.Advance(ctx, e.ID, "owner:1", 2, now.Add(time.Minute))
		if err != nil {
			t.Fatalf("Advance: %v", err)
		}
		if !ok {
			t.Fatal("Advance with the owning token failed")
		}
		got, err := repos.alertEscalations.GetByAlertID(ctx, alert.ID)
		if err != nil {
			t.Fatalf("GetByAlertID: %v", err)
		}
		if got.NextStep != 2 {
			t.Fatalf("NextStep = %d; want 2", got.NextStep)
		}
		if got.LeaseOwner != nil {
			t.Fatalf("lease not released: %v", *got.LeaseOwner)
		}

		// Finish needs a fresh claim, and rejects a stale token the same way.
		claimed, err = repos.alertEscalations.ClaimDue(ctx, "owner:2", now.Add(2*time.Minute), now.Add(7*time.Minute))
		if err != nil || len(claimed) != 1 {
			t.Fatalf("re-claim = %v, %v", claimed, err)
		}
		if ok, err := repos.alertEscalations.Finish(ctx, e.ID, "owner:1", domain.EscalationStateDone); err != nil || ok {
			t.Fatalf("Finish with a stale token = %v, %v; want false, nil", ok, err)
		}
		if ok, err := repos.alertEscalations.Finish(ctx, e.ID, "owner:2", domain.EscalationStateDone); err != nil || !ok {
			t.Fatalf("Finish = %v, %v; want true, nil", ok, err)
		}
		got, _ = repos.alertEscalations.GetByAlertID(ctx, alert.ID)
		if got.Status != domain.EscalationStateDone {
			t.Fatalf("status = %s; want done", got.Status)
		}
	})

	// Cancellation must beat a live lease: an ack always wins the race against a
	// worker mid-step.
	t.Run("CancelBeatsALiveLease", func(t *testing.T) {
		repos := factory(t)
		ctx := context.Background()
		user := createUser(t, ctx, repos, "esc-cancel")
		monitor := createMonitor(t, ctx, repos, user.ID, "cancel")
		n1 := createEscNotification(t, ctx, repos, user.ID, "primary")
		p := createEscPolicy(t, ctx, repos, user.ID, "ladder", n1.ID)
		alert := createEscAlert(t, ctx, repos, monitor.ID, "tok-cancel")

		now := time.Now().UTC().Truncate(time.Second)
		e := &domain.AlertEscalation{
			AlertID: alert.ID, MonitorID: monitor.ID, PolicyID: p.ID, NextStep: 1,
			NextRunAt: now.Add(-time.Minute), Status: domain.EscalationStatePending,
		}
		if err := repos.alertEscalations.Create(ctx, e); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := repos.alertEscalations.ClaimDue(ctx, "owner:1", now, now.Add(5*time.Minute)); err != nil {
			t.Fatalf("ClaimDue: %v", err)
		}
		if err := repos.alertEscalations.CancelByAlertID(ctx, alert.ID); err != nil {
			t.Fatalf("CancelByAlertID: %v", err)
		}
		got, err := repos.alertEscalations.GetByAlertID(ctx, alert.ID)
		if err != nil {
			t.Fatalf("GetByAlertID: %v", err)
		}
		if got.Status != domain.EscalationStateCanceled {
			t.Fatalf("status = %s; want canceled", got.Status)
		}
		// The row survives as the audit trail of how far the ladder got.
		if got.NextStep != 1 {
			t.Fatalf("NextStep = %d; the canceled row must keep its progress", got.NextStep)
		}
		// The lease holder can no longer advance it.
		if ok, err := repos.alertEscalations.Advance(ctx, e.ID, "owner:1", 2, now); err != nil || ok {
			t.Fatalf("Advance after cancel = %v, %v; want false, nil", ok, err)
		}
		// Canceling again is a no-op, not an error.
		if err := repos.alertEscalations.CancelByAlertID(ctx, alert.ID); err != nil {
			t.Fatalf("second CancelByAlertID: %v", err)
		}
	})

	// MariaDB stores TIMESTAMP at second precision, so two rows scheduled inside
	// the same second carry the identical stored value. A batch a human reads as
	// a sequence needs a tie-break on id (AGENTS.md rule 8) — this is the same
	// class of bug that once made a DOWN monitor read back as PENDING.
	t.Run("ClaimOrderBreaksTimestampTieByID", func(t *testing.T) {
		repos := factory(t)
		ctx := context.Background()
		user := createUser(t, ctx, repos, "esc-tie")
		n1 := createEscNotification(t, ctx, repos, user.ID, "primary")
		p := createEscPolicy(t, ctx, repos, user.ID, "ladder", n1.ID)

		now := time.Now().UTC().Truncate(time.Second)
		due := now.Add(-time.Minute)

		var wantIDs []int64
		for i := 0; i < 3; i++ {
			monitor := createMonitor(t, ctx, repos, user.ID, "tie-"+string(rune('a'+i)))
			alert := createEscAlert(t, ctx, repos, monitor.ID, "tok-tie-"+string(rune('a'+i)))
			e := &domain.AlertEscalation{
				AlertID: alert.ID, MonitorID: monitor.ID, PolicyID: p.ID, NextStep: 1,
				// Identical to the second on both engines: a deliberately
				// constructed tie, per rule 8.
				NextRunAt: due, Status: domain.EscalationStatePending,
			}
			if err := repos.alertEscalations.Create(ctx, e); err != nil {
				t.Fatalf("Create escalation %d: %v", i, err)
			}
			wantIDs = append(wantIDs, e.ID)
		}

		claimed, err := repos.alertEscalations.ClaimDue(ctx, "tie:1", now, now.Add(5*time.Minute))
		if err != nil {
			t.Fatalf("ClaimDue: %v", err)
		}
		if len(claimed) != 3 {
			t.Fatalf("claimed %d rows; want 3", len(claimed))
		}
		for i, e := range claimed {
			if e.ID != wantIDs[i] {
				t.Fatalf("claim order = %d at index %d; want %d (ties must break by id)", e.ID, i, wantIDs[i])
			}
		}
	})

	t.Run("DeletingAPolicyCascades", func(t *testing.T) {
		repos := factory(t)
		ctx := context.Background()
		user := createUser(t, ctx, repos, "esc-cascade")
		monitor := createMonitor(t, ctx, repos, user.ID, "cascade")
		n1 := createEscNotification(t, ctx, repos, user.ID, "primary")
		p := createEscPolicy(t, ctx, repos, user.ID, "ladder", n1.ID)

		if err := repos.escalationAssignments.AssignMonitor(ctx, monitor.ID, p.ID); err != nil {
			t.Fatalf("AssignMonitor: %v", err)
		}
		if err := repos.escalationPolicies.Delete(ctx, p.ID); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := repos.escalationPolicies.GetByID(ctx, p.ID); !errors.Is(err, ports.ErrNotFound) {
			t.Fatalf("GetByID after delete = %v; want ErrNotFound", err)
		}
		if _, err := repos.escalationAssignments.PolicyIDForMonitor(ctx, monitor.ID); !errors.Is(err, ports.ErrNotFound) {
			t.Fatalf("assignment survived a policy delete: %v", err)
		}
	})
}

// --- helpers ---------------------------------------------------------------

func createEscNotification(t *testing.T, ctx context.Context, repos repositorySet, userID int64, name string) *domain.Notification {
	t.Helper()
	n := &domain.Notification{
		UserID: userID, Name: name, Type: "webhook", Active: true,
		Config: map[string]any{"url": "https://example.test/hook"},
	}
	if err := repos.notifications.Create(ctx, n); err != nil {
		t.Fatalf("Create notification %q: %v", name, err)
	}
	return n
}

func createEscGroup(t *testing.T, ctx context.Context, repos repositorySet, userID int64, name string) *domain.MonitorGroup {
	t.Helper()
	g := &domain.MonitorGroup{UserID: userID, Name: name, Condition: domain.GroupConditionWorstOfChildren}
	if err := repos.monitorGroups.Create(ctx, g); err != nil {
		t.Fatalf("Create group %q: %v", name, err)
	}
	return g
}

func createEscPolicy(t *testing.T, ctx context.Context, repos repositorySet, userID int64, name string, notificationID int64) *domain.EscalationPolicy {
	t.Helper()
	p := &domain.EscalationPolicy{
		UserID: userID, Name: name, Enabled: true,
		Steps: []domain.EscalationStep{
			{StepOrder: 1, WaitMinutes: 5, NotificationIDs: []int64{notificationID}},
			{StepOrder: 2, WaitMinutes: 10, NotificationIDs: []int64{notificationID}},
		},
	}
	if err := repos.escalationPolicies.Create(ctx, p); err != nil {
		t.Fatalf("Create policy %q: %v", name, err)
	}
	return p
}

func createEscAlert(t *testing.T, ctx context.Context, repos repositorySet, monitorID int64, token string) *domain.Alert {
	t.Helper()
	mid := monitorID
	a := &domain.Alert{
		MonitorID: monitorID,
		Status:    domain.AlertStatusFiring,
		Message:   "down",
		// Second-truncated: MariaDB TIMESTAMP truncates rather than rounds, and
		// a zero time.Time would be rejected outright with ERROR 1292.
		FiredAt:       time.Now().UTC().Truncate(time.Second),
		AckToken:      token,
		OpenMonitorID: &mid,
	}
	if err := repos.alerts.Create(ctx, a); err != nil {
		t.Fatalf("Create alert %q: %v", token, err)
	}
	return a
}

func resolveEscAlert(t *testing.T, ctx context.Context, repos repositorySet, a *domain.Alert) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	a.Status = domain.AlertStatusResolved
	a.ResolvedAt = &now
	a.OpenMonitorID = nil
	if err := repos.alerts.Update(ctx, a); err != nil {
		t.Fatalf("resolve alert: %v", err)
	}
}
