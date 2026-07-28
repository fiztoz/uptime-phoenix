package services

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// escalationNotifier sends one escalation step to an explicit channel list.
// Satisfied by *NotificationService.
type escalationNotifier interface {
	DispatchToNotificationIDs(ctx context.Context, notificationIDs []int64, alert domain.AlertContext) error
}

// maxEscalationWaitMinutes caps a single step's wait at 7 days. A typo of
// 100000 would otherwise park an escalation past the heat death of the on-call
// rotation, and the row would look pending forever with no way to tell it from
// a stalled worker.
const maxEscalationWaitMinutes = 7 * 24 * 60

// escalationLeaseTTL is how long a claimed step stays owned before another
// worker may steal it. It must comfortably exceed the time to dispatch one
// step's channels; a crashed worker's rows become claimable again after it.
const escalationLeaseTTL = 2 * time.Minute

// EscalationService owns F2.3 escalation policies: CRUD, monitor/group
// assignment, precedence resolution, and the runner that advances a firing
// alert through its ladder.
//
// The two contracts it implements are written down in
// docs/F2.3-ESCALATION-CONTRACTS.md:
//
//  1. Precedence — the monitor's own policy wins; otherwise the nearest
//     ancestor group with one; a disabled or empty policy stops the walk and
//     escalates nothing rather than falling through.
//  2. Step zero — the initial DOWN notification belongs to
//     NotificationDispatcher, never to a policy. Policies own steps 1..N only,
//     so the first alert is neither lost nor duplicated.
type EscalationService struct {
	policies    ports.EscalationPolicyRepository
	assignments ports.EscalationAssignmentRepository
	state       ports.AlertEscalationRepository
	alerts      ports.AlertRepository
	monitors    ports.MonitorRepository
	groups      ports.MonitorGroupRepository
	notifier    escalationNotifier

	// workerID names this process in lease_owner. Empty in single-worker
	// installs, which is fine — the per-claim nonce still makes the token unique.
	workerID string

	now func() time.Time
}

// NewEscalationService creates the service. Every repository is required; the
// service fails closed (returns an error) rather than silently escalating
// nothing if one is missing.
func NewEscalationService(
	policies ports.EscalationPolicyRepository,
	assignments ports.EscalationAssignmentRepository,
	state ports.AlertEscalationRepository,
	alerts ports.AlertRepository,
	monitors ports.MonitorRepository,
	groups ports.MonitorGroupRepository,
	notifier escalationNotifier,
) *EscalationService {
	return &EscalationService{
		policies:    policies,
		assignments: assignments,
		state:       state,
		alerts:      alerts,
		monitors:    monitors,
		groups:      groups,
		notifier:    notifier,
		now:         time.Now,
	}
}

// SetWorkerID names this process in the escalation lease. Optional.
func (s *EscalationService) SetWorkerID(id string) { s.workerID = id }

// ---------------------------------------------------------------------------
// Policy CRUD
// ---------------------------------------------------------------------------

// CreatePolicy validates and persists a policy with its whole step ladder.
func (s *EscalationService) CreatePolicy(ctx context.Context, p *domain.EscalationPolicy) error {
	if s == nil || s.policies == nil {
		return fmt.Errorf("escalation service: create: not configured")
	}
	if err := normalizeEscalationPolicy(p); err != nil {
		return err
	}
	return s.policies.Create(ctx, p)
}

// UpdatePolicy validates and persists a policy, REPLACING its step ladder.
// Steps absent from p.Steps are deleted — send the whole ladder.
func (s *EscalationService) UpdatePolicy(ctx context.Context, p *domain.EscalationPolicy) error {
	if s == nil || s.policies == nil {
		return fmt.Errorf("escalation service: update: not configured")
	}
	if err := normalizeEscalationPolicy(p); err != nil {
		return err
	}
	return s.policies.Update(ctx, p)
}

// GetPolicy returns one policy with its steps.
func (s *EscalationService) GetPolicy(ctx context.Context, id int64) (*domain.EscalationPolicy, error) {
	if s == nil || s.policies == nil {
		return nil, fmt.Errorf("escalation service: get: not configured")
	}
	return s.policies.GetByID(ctx, id)
}

// ListPolicies returns every policy with its steps.
func (s *EscalationService) ListPolicies(ctx context.Context) ([]*domain.EscalationPolicy, error) {
	if s == nil || s.policies == nil {
		return nil, fmt.Errorf("escalation service: list: not configured")
	}
	return s.policies.List(ctx)
}

// DeletePolicy removes a policy. Steps, channel links and assignments cascade;
// in-flight escalation rows referencing it cascade too, which is correct — a
// deleted ladder must not keep paging.
func (s *EscalationService) DeletePolicy(ctx context.Context, id int64) error {
	if s == nil || s.policies == nil {
		return fmt.Errorf("escalation service: delete: not configured")
	}
	return s.policies.Delete(ctx, id)
}

// normalizeEscalationPolicy validates and renumbers a policy in place.
//
// Step order is renumbered to a dense 1..N from the caller's supplied order, so
// the UI can reorder by moving array elements without having to keep the
// numbers contiguous itself. Step 0 is reserved for the dispatcher's initial
// notification and can never be created here.
func normalizeEscalationPolicy(p *domain.EscalationPolicy) error {
	if p == nil {
		return domain.ErrValidation
	}
	p.Name = strings.TrimSpace(p.Name)
	p.Description = strings.TrimSpace(p.Description)
	if p.Name == "" {
		return fmt.Errorf("%w: name is required", domain.ErrValidation)
	}
	if len(p.Name) > 255 {
		return fmt.Errorf("%w: name is too long", domain.ErrValidation)
	}
	if len(p.Steps) > 20 {
		return fmt.Errorf("%w: a policy may have at most 20 steps", domain.ErrValidation)
	}

	sort.SliceStable(p.Steps, func(i, j int) bool { return p.Steps[i].StepOrder < p.Steps[j].StepOrder })
	for i := range p.Steps {
		st := &p.Steps[i]
		if st.WaitMinutes < 0 {
			return fmt.Errorf("%w: wait_minutes cannot be negative", domain.ErrValidation)
		}
		if st.WaitMinutes > maxEscalationWaitMinutes {
			return fmt.Errorf("%w: wait_minutes exceeds the 7 day maximum", domain.ErrValidation)
		}
		if len(st.NotificationIDs) == 0 {
			return fmt.Errorf("%w: step %d has no notification channels", domain.ErrValidation, i+1)
		}
		st.StepOrder = i + 1
		st.NotificationIDs = dedupeInt64(st.NotificationIDs)
	}
	return nil
}

func dedupeInt64(in []int64) []int64 {
	seen := make(map[int64]struct{}, len(in))
	out := make([]int64, 0, len(in))
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// ---------------------------------------------------------------------------
// Assignment
// ---------------------------------------------------------------------------

// AssignMonitor points a monitor at a policy, replacing any prior assignment.
func (s *EscalationService) AssignMonitor(ctx context.Context, monitorID, policyID int64) error {
	if s == nil || s.assignments == nil {
		return fmt.Errorf("escalation service: assign monitor: not configured")
	}
	if _, err := s.policies.GetByID(ctx, policyID); err != nil {
		return err
	}
	return s.assignments.AssignMonitor(ctx, monitorID, policyID)
}

// UnassignMonitor clears a monitor's policy assignment.
func (s *EscalationService) UnassignMonitor(ctx context.Context, monitorID int64) error {
	if s == nil || s.assignments == nil {
		return fmt.Errorf("escalation service: unassign monitor: not configured")
	}
	return s.assignments.UnassignMonitor(ctx, monitorID)
}

// AssignGroup points a monitor group at a policy, replacing any prior one.
func (s *EscalationService) AssignGroup(ctx context.Context, groupID, policyID int64) error {
	if s == nil || s.assignments == nil {
		return fmt.Errorf("escalation service: assign group: not configured")
	}
	if _, err := s.policies.GetByID(ctx, policyID); err != nil {
		return err
	}
	return s.assignments.AssignGroup(ctx, groupID, policyID)
}

// UnassignGroup clears a group's policy assignment.
func (s *EscalationService) UnassignGroup(ctx context.Context, groupID int64) error {
	if s == nil || s.assignments == nil {
		return fmt.Errorf("escalation service: unassign group: not configured")
	}
	return s.assignments.UnassignGroup(ctx, groupID)
}

// PolicyIDForMonitor returns the monitor's DIRECT assignment (no ancestor
// walk), or 0 when it has none. This is what the monitor edit form round-trips;
// showing an inherited policy in that field would make saving the form silently
// convert inheritance into a direct assignment.
func (s *EscalationService) PolicyIDForMonitor(ctx context.Context, monitorID int64) (int64, error) {
	if s == nil || s.assignments == nil {
		return 0, nil
	}
	id, err := s.assignments.PolicyIDForMonitor(ctx, monitorID)
	if errors.Is(err, ports.ErrNotFound) {
		return 0, nil
	}
	return id, err
}

// PolicyIDForGroup returns the group's DIRECT assignment, or 0 when it has none.
func (s *EscalationService) PolicyIDForGroup(ctx context.Context, groupID int64) (int64, error) {
	if s == nil || s.assignments == nil {
		return 0, nil
	}
	id, err := s.assignments.PolicyIDForGroup(ctx, groupID)
	if errors.Is(err, ports.ErrNotFound) {
		return 0, nil
	}
	return id, err
}

// ---------------------------------------------------------------------------
// Contract 1 — precedence
// ---------------------------------------------------------------------------

// ResolvePolicy returns the single policy that escalates this monitor's alerts,
// or nil when none applies.
//
// Nearest wins: the monitor's own assignment first, then the first assigned
// group walking upward from its immediate parent to the root. A disabled policy
// or one with no steps STOPS the walk and returns nil — it does not fall
// through to the parent. Silencing a subtree must not silently reroute its
// pages to a different set of humans.
func (s *EscalationService) ResolvePolicy(ctx context.Context, monitor *domain.Monitor) (*domain.EscalationPolicy, error) {
	if s == nil || s.assignments == nil || s.policies == nil || monitor == nil {
		return nil, nil
	}

	policyID, err := s.assignments.PolicyIDForMonitor(ctx, monitor.ID)
	switch {
	case err == nil:
		return s.loadRunnablePolicy(ctx, policyID)
	case !errors.Is(err, ports.ErrNotFound):
		return nil, fmt.Errorf("escalation service: monitor assignment: %w", err)
	}

	// Walk ancestors. A visited set guards against a parent_id cycle:
	// MonitorGroupService rejects them today, but a resolver on the alerting
	// path must terminate whatever the data says.
	visited := make(map[int64]struct{})
	groupID := monitor.GroupID
	for groupID != nil {
		if _, seen := visited[*groupID]; seen {
			slog.Warn("escalation service: monitor group cycle detected while resolving policy",
				"monitor_id", monitor.ID, "group_id", *groupID)
			return nil, nil
		}
		visited[*groupID] = struct{}{}

		pid, gErr := s.assignments.PolicyIDForGroup(ctx, *groupID)
		if gErr == nil {
			return s.loadRunnablePolicy(ctx, pid)
		}
		if !errors.Is(gErr, ports.ErrNotFound) {
			return nil, fmt.Errorf("escalation service: group assignment: %w", gErr)
		}

		if s.groups == nil {
			return nil, nil
		}
		g, gErr := s.groups.GetByID(ctx, *groupID)
		if gErr != nil {
			if errors.Is(gErr, ports.ErrNotFound) {
				return nil, nil
			}
			return nil, fmt.Errorf("escalation service: group lookup: %w", gErr)
		}
		groupID = g.ParentID
	}
	return nil, nil
}

// loadRunnablePolicy fetches a policy and reports it only when it can actually
// escalate. A disabled or step-less policy resolves to nil, which STOPS the
// precedence walk in ResolvePolicy — see the contract doc.
func (s *EscalationService) loadRunnablePolicy(ctx context.Context, policyID int64) (*domain.EscalationPolicy, error) {
	p, err := s.policies.GetByID(ctx, policyID)
	if err != nil {
		if errors.Is(err, ports.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("escalation service: load policy: %w", err)
	}
	if !p.Enabled || len(p.Steps) == 0 {
		return nil, nil
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// Contract 2 — starting after step zero
// ---------------------------------------------------------------------------

// StartForAlert begins the ladder for a freshly opened alert.
//
// It is called by NotificationDispatcher AFTER the initial DOWN notification
// has been dispatched. The escalation therefore always starts at step 1, due at
// firedAt + step1.WaitMinutes. Step 0 does not exist here and the runner never
// sends it: that notification is the dispatcher's, so it can be neither lost
// nor duplicated by a policy.
//
// Starting is idempotent. A UNIQUE(alert_id) conflict means another worker got
// there first and is not an error.
func (s *EscalationService) StartForAlert(ctx context.Context, alert *domain.Alert, monitor *domain.Monitor) error {
	if s == nil || s.state == nil || alert == nil || monitor == nil {
		return nil
	}
	policy, err := s.ResolvePolicy(ctx, monitor)
	if err != nil {
		return err
	}
	if policy == nil {
		return nil
	}

	first := policy.Steps[0]
	e := &domain.AlertEscalation{
		AlertID:   alert.ID,
		MonitorID: monitor.ID,
		PolicyID:  policy.ID,
		NextStep:  first.StepOrder,
		// UTC at the service boundary: a local-zoned bound reaching the
		// repository shifts the due window by the host's offset and an
		// in-memory fake cannot catch it (AGENTS.md rule 6).
		NextRunAt: alert.FiredAt.UTC().Add(time.Duration(first.WaitMinutes) * time.Minute),
		Status:    domain.EscalationStatePending,
	}
	if err := s.state.Create(ctx, e); err != nil {
		if errors.Is(err, ports.ErrConflict) {
			return nil
		}
		return fmt.Errorf("escalation service: start: %w", err)
	}
	return nil
}

// CancelForAlert stops any pending escalation for an alert. Called on
// acknowledgement and on resolution.
//
// Acked means "stop escalating", not "resolved": this cancels the ladder and
// deliberately touches nothing else on the alert — OpenMonitorID must stay set
// so the outage remains open and resends stay suppressed (handoff §4.8).
func (s *EscalationService) CancelForAlert(ctx context.Context, alertID int64) error {
	if s == nil || s.state == nil {
		return nil
	}
	if err := s.state.CancelByAlertID(ctx, alertID); err != nil {
		return fmt.Errorf("escalation service: cancel: %w", err)
	}
	return nil
}

// StatesForAlerts returns each alert's escalation progress, keyed by alert id.
// Alerts with no escalation are absent from the map. One query, whatever the
// number of alerts.
func (s *EscalationService) StatesForAlerts(ctx context.Context, alertIDs []int64) (map[int64]*domain.AlertEscalation, error) {
	if s == nil || s.state == nil || len(alertIDs) == 0 {
		return map[int64]*domain.AlertEscalation{}, nil
	}
	states, err := s.state.ListByAlertIDs(ctx, alertIDs)
	if err != nil {
		return nil, fmt.Errorf("escalation service: states for alerts: %w", err)
	}
	return states, nil
}

// ---------------------------------------------------------------------------
// The runner
// ---------------------------------------------------------------------------

// RunDue claims and processes every escalation step that is due, returning the
// number of steps actually sent.
//
// Called on a ticker by the worker. It is safe to run concurrently in several
// processes: each row is claimed with a compare-and-set lease, so exactly one
// worker owns a step.
func (s *EscalationService) RunDue(ctx context.Context) (int, error) {
	if s == nil || s.state == nil {
		return 0, nil
	}
	// UTC at the service boundary — see StartForAlert.
	now := s.now().UTC()
	token, err := newClaimToken(s.workerID)
	if err != nil {
		return 0, fmt.Errorf("escalation service: claim token: %w", err)
	}

	claimed, err := s.state.ClaimDue(ctx, token, now, now.Add(escalationLeaseTTL))
	if err != nil {
		return 0, fmt.Errorf("escalation service: claim due: %w", err)
	}

	sent := 0
	for _, e := range claimed {
		ok, err := s.runOne(ctx, e, token, now)
		if err != nil {
			slog.Error("escalation service: step failed",
				"alert_id", e.AlertID, "policy_id", e.PolicyID, "step", e.NextStep, "error", err)
			continue
		}
		if ok {
			sent++
		}
	}
	return sent, nil
}

// runOne processes a single claimed escalation. It reports whether a step was
// actually dispatched.
func (s *EscalationService) runOne(ctx context.Context, e *domain.AlertEscalation, token string, now time.Time) (bool, error) {
	// Re-read the alert INSIDE the claim. Cancellation alone does not close the
	// window: an ack landing between ClaimDue and the send would otherwise still
	// page someone. This second check is what makes acknowledgement authoritative.
	alert, err := s.alerts.GetByID(ctx, e.AlertID)
	if err != nil {
		if errors.Is(err, ports.ErrNotFound) {
			_, _ = s.state.Finish(ctx, e.ID, token, domain.EscalationStateCanceled)
			return false, nil
		}
		return false, fmt.Errorf("read alert: %w", err)
	}
	if alert.Status != domain.AlertStatusFiring {
		// acked or resolved — stop escalating, keep the row as the audit trail.
		_, _ = s.state.Finish(ctx, e.ID, token, domain.EscalationStateCanceled)
		return false, nil
	}

	policy, err := s.policies.GetByID(ctx, e.PolicyID)
	if err != nil {
		if errors.Is(err, ports.ErrNotFound) {
			_, _ = s.state.Finish(ctx, e.ID, token, domain.EscalationStateCanceled)
			return false, nil
		}
		return false, fmt.Errorf("read policy: %w", err)
	}
	if !policy.Enabled {
		_, _ = s.state.Finish(ctx, e.ID, token, domain.EscalationStateCanceled)
		return false, nil
	}

	step, next := splitEscalationStep(policy.Steps, e.NextStep)
	if step == nil {
		// The ladder was edited shorter underneath a running escalation.
		_, _ = s.state.Finish(ctx, e.ID, token, domain.EscalationStateDone)
		return false, nil
	}

	monitor, err := s.monitors.GetByID(ctx, e.MonitorID)
	if err != nil {
		return false, fmt.Errorf("read monitor: %w", err)
	}

	dispatched := true
	if err := s.notifier.DispatchToNotificationIDs(ctx, step.NotificationIDs, escalationAlertContext(monitor, alert, policy, step)); err != nil {
		// A send failure must not stall the ladder: log it and advance, exactly
		// as NotificationService.Dispatch does for a per-provider failure. A
		// step that could not reach anyone still consumed its wait, and holding
		// the row would freeze every later step behind a dead channel.
		slog.Error("escalation service: dispatch failed, advancing anyway",
			"alert_id", e.AlertID, "policy_id", policy.ID, "step", step.StepOrder, "error", err)
		dispatched = false
	}

	if next == nil {
		if _, err := s.state.Finish(ctx, e.ID, token, domain.EscalationStateDone); err != nil {
			return dispatched, fmt.Errorf("finish: %w", err)
		}
		return dispatched, nil
	}
	// Waits are cumulative from the previous step, so the next due time is
	// measured from now — not from the alert's FiredAt.
	if _, err := s.state.Advance(ctx, e.ID, token, next.StepOrder, now.Add(time.Duration(next.WaitMinutes)*time.Minute)); err != nil {
		return dispatched, fmt.Errorf("advance: %w", err)
	}
	return dispatched, nil
}

// splitEscalationStep finds the step with the given order and the one after it.
func splitEscalationStep(steps []domain.EscalationStep, order int) (current, next *domain.EscalationStep) {
	for i := range steps {
		if steps[i].StepOrder != order {
			continue
		}
		current = &steps[i]
		if i+1 < len(steps) {
			next = &steps[i+1]
		}
		return current, next
	}
	return nil, nil
}

func escalationAlertContext(monitor *domain.Monitor, alert *domain.Alert, policy *domain.EscalationPolicy, step *domain.EscalationStep) domain.AlertContext {
	return domain.AlertContext{
		MonitorID:      monitor.ID,
		MonitorName:    monitor.Name,
		MonitorType:    monitor.Type,
		MonitorTarget:  monitor.Target(),
		Status:         domain.StatusDown,
		PreviousStatus: domain.StatusDown,
		Message: fmt.Sprintf("ESCALATION step %d (%s): %s is still DOWN and unacknowledged",
			step.StepOrder, policy.Name, monitor.Name),
		StartedAt:   alert.FiredAt,
		EventKind:   domain.AlertEventStatusChange,
		CheckOutput: alert.Message,
	}
}

// newClaimToken produces a lease owner unique to one ClaimDue call. The worker
// id makes an orphaned lease attributable in the table; the nonce is what makes
// the claim safe, so a single-worker install with an empty id is still correct.
func newClaimToken(workerID string) (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	if workerID == "" {
		workerID = "worker"
	}
	token := workerID + ":" + hex.EncodeToString(b[:])
	if len(token) > 255 {
		token = token[:255]
	}
	return token, nil
}
