package services

import (
	"context"
	"log/slog"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// groupAlertNotifier dispatches an alert to a GROUP's assigned providers.
// Satisfied by *NotificationService.
type groupAlertNotifier interface {
	NotifyGroup(ctx context.Context, group *domain.MonitorGroup, status, prevStatus domain.Status) error
}

// GroupAlertService turns a monitor's heartbeat into an alert on the FOLDER that
// contains it, when the folder's own derived status transitions.
//
// A group is an alerting entity in its own right. Attaching a notification to a
// folder means "tell me when this folder as a whole trips", per the folder's
// Condition (worst_of_children / all_down / threshold / ignore) — it is NOT an
// inheritance shortcut that alerts once per monitor inside it. The monitors keep
// alerting through their own attachments, independently.
//
// It runs inside the heartbeat path (via NotificationDispatcher), NOT off the
// event bus: under Redis fan-out every worker receives every event, so a
// bus-subscribed alerter would send one alert per worker.
//
// That still leaves a race the heartbeat path cannot close on its own: two
// monitors in the same folder are checked by two different sharded workers at
// the same instant, both recompute the folder's rollup as DOWN, and both try to
// alert. So the transition is CLAIMED with a compare-and-set against the group's
// persisted last_status (ports.MonitorGroupRepository.ClaimStatusTransition) —
// exactly one worker's UPDATE matches, and only that worker sends.
type GroupAlertService struct {
	groups     ports.MonitorGroupRepository
	groupNotif ports.GroupNotificationRepository
	monitors   ports.MonitorRepository
	heartbeats ports.HeartbeatRepository
	notifier   groupAlertNotifier
}

// NewGroupAlertService creates the folder-alerting evaluator.
func NewGroupAlertService(
	groups ports.MonitorGroupRepository,
	groupNotif ports.GroupNotificationRepository,
	monitors ports.MonitorRepository,
	heartbeats ports.HeartbeatRepository,
	notifier groupAlertNotifier,
) *GroupAlertService {
	return &GroupAlertService{
		groups:     groups,
		groupNotif: groupNotif,
		monitors:   monitors,
		heartbeats: heartbeats,
		notifier:   notifier,
	}
}

// OnHeartbeat re-evaluates every ancestor folder of the monitor that just
// reported, and alerts on any whose derived status transitioned.
//
// Errors are logged, never returned: a failure to evaluate a folder must not
// fail the heartbeat that triggered it.
func (s *GroupAlertService) OnHeartbeat(ctx context.Context, monitor *domain.Monitor) {
	if monitor == nil || monitor.GroupID == nil {
		return // a monitor in no folder can never move one
	}

	all, err := s.groups.ListAll(ctx)
	if err != nil {
		slog.Error("group alert: list groups failed", "monitor_id", monitor.ID, "error", err)
		return
	}

	ev := newGroupEval(s, all)
	for _, g := range ev.ancestorsOf(*monitor.GroupID) {
		// Cheap guard first: a folder with no providers attached can never alert,
		// so don't pay for its rollup. Its last_status stays untouched, which is
		// harmless — nothing reads it but this path.
		links, err := s.groupNotif.ListByGroup(ctx, g.ID)
		if err != nil {
			slog.Error("group alert: list group notifications failed", "group_id", g.ID, "error", err)
			continue
		}
		if len(links) == 0 {
			continue
		}

		status, ok := ev.resolve(ctx, g)
		if !ok {
			// No derived status: an "ignore" folder, or one with no children that
			// have a status yet. It has nothing to alert about — by design, an
			// ignore folder never alerts even with providers attached.
			continue
		}
		s.claimAndAlert(ctx, g, status)
	}
}

// claimAndAlert moves the folder's persisted status and, if this worker won the
// move and the transition is alert-worthy, sends.
func (s *GroupAlertService) claimAndAlert(ctx context.Context, g *domain.MonitorGroup, status domain.Status) {
	prev := g.LastStatus
	if prev != nil && *prev == status {
		return // nothing moved. Also keeps the CAS from being called with to == *from.
	}

	won, err := s.groups.ClaimStatusTransition(ctx, g.ID, prev, status)
	if err != nil {
		slog.Error("group alert: claim transition failed", "group_id", g.ID, "error", err)
		return
	}
	if !won {
		// Another worker moved this folder first and is sending (or deliberately
		// not sending) the alert. Staying quiet here is the whole point.
		return
	}

	if !groupTransitionAlerts(prev, status) {
		return // claimed, so last_status is current — but this transition is not alert-worthy.
	}

	// The monitor path treats a first-ever observation as a transition from UP;
	// mirror that so the alert reads "UP -> DOWN" rather than "PENDING -> DOWN".
	prevStatus := domain.StatusUp
	if prev != nil {
		prevStatus = *prev
	}
	if err := s.notifier.NotifyGroup(ctx, g, status, prevStatus); err != nil {
		slog.Error("group alert: notify failed",
			"group_id", g.ID, "status", status.String(), "error", err)
	}
}

// groupTransitionAlerts reports whether a folder's status change is worth an
// alert. It mirrors the monitor rules in NotificationDispatcher:
//
//   - trip (-> DOWN) and recovery (DOWN -> UP) alert;
//   - PENDING alerts never — that is the retry window, and a folder inherits it
//     from the children still inside it;
//   - MAINTENANCE alerts never — deliberate downtime is not an incident. A folder
//     whose children are all in maintenance is itself MAINTENANCE (domain.Rollup),
//     and going into or out of that state is not news.
//
// prev == nil is the folder's first-ever evaluation.
func groupTransitionAlerts(prev *domain.Status, cur domain.Status) bool {
	switch cur {
	case domain.StatusDown:
		return prev == nil || *prev != domain.StatusDown
	case domain.StatusUp:
		return prev != nil && *prev == domain.StatusDown
	default:
		return false
	}
}

// groupEval resolves folder statuses for ONE heartbeat, caching everything it
// reads so a folder shared by several ancestors is rolled up once.
//
// It deliberately does not reuse MonitorGroupService.ResolveStatuses: that
// resolves every folder in the install (and reads every monitor's latest
// heartbeat to do it), which is the right shape for rendering a page and much
// too heavy to run on every heartbeat. This walks only the ancestors of the one
// monitor that reported, and reads heartbeats lazily.
type groupEval struct {
	svc *GroupAlertService

	byID       map[int64]*domain.MonitorGroup
	childGroup map[int64][]*domain.MonitorGroup

	// Lazily populated on the first rollup — a heartbeat for a monitor whose
	// folders have no providers attached must not pay for a monitor list.
	monitorsLoaded  bool
	monitorsByGroup map[int64][]*domain.Monitor

	resolved map[int64]domain.Status // folders that HAVE a derived status
	noStatus map[int64]bool          // folders resolved to "no status" (ignore/childless)
	visiting map[int64]bool          // cycle guard for bad data
}

func newGroupEval(s *GroupAlertService, all []*domain.MonitorGroup) *groupEval {
	e := &groupEval{
		svc:        s,
		byID:       make(map[int64]*domain.MonitorGroup, len(all)),
		childGroup: make(map[int64][]*domain.MonitorGroup),
		resolved:   make(map[int64]domain.Status),
		noStatus:   make(map[int64]bool),
		visiting:   make(map[int64]bool),
	}
	for _, g := range all {
		e.byID[g.ID] = g
		if g.ParentID != nil {
			e.childGroup[*g.ParentID] = append(e.childGroup[*g.ParentID], g)
		}
	}
	return e
}

// ancestorsOf returns the folder chain from the monitor's own folder up to the
// root, nearest first. Bounded by maxGroupWalkDepth so a pre-existing cycle in
// the data (which the service itself never creates — see validateParent) cannot
// spin forever on the heartbeat path.
func (e *groupEval) ancestorsOf(groupID int64) []*domain.MonitorGroup {
	var chain []*domain.MonitorGroup
	seen := make(map[int64]bool)
	cur, ok := e.byID[groupID]
	for depth := 0; ok && depth < maxGroupWalkDepth; depth++ {
		if seen[cur.ID] {
			break
		}
		seen[cur.ID] = true
		chain = append(chain, cur)
		if cur.ParentID == nil {
			break
		}
		cur, ok = e.byID[*cur.ParentID]
	}
	return chain
}

// resolve rolls a folder up from its direct children — the monitors filed under
// it plus its subfolders, resolved first (bottom-up). ok is false when the
// folder has no derived status at all.
func (e *groupEval) resolve(ctx context.Context, g *domain.MonitorGroup) (domain.Status, bool) {
	if status, done := e.resolved[g.ID]; done {
		return status, true
	}
	if e.noStatus[g.ID] {
		return domain.StatusPending, false
	}
	if e.visiting[g.ID] {
		// Defensive: a cycle in the stored data. Contribute nothing rather than recurse.
		return domain.StatusPending, false
	}
	e.visiting[g.ID] = true
	defer delete(e.visiting, g.ID)

	if err := e.loadMonitors(ctx); err != nil {
		slog.Error("group alert: list monitors failed", "group_id", g.ID, "error", err)
		return domain.StatusPending, false
	}

	children := make([]domain.Status, 0, len(e.monitorsByGroup[g.ID])+len(e.childGroup[g.ID]))
	for _, m := range e.monitorsByGroup[g.ID] {
		hb, err := e.svc.heartbeats.GetLatest(ctx, m.ID)
		if err != nil {
			continue // never checked yet — contributes no status, exactly as ResolveStatuses does
		}
		children = append(children, hb.Status)
	}
	for _, sub := range e.childGroup[g.ID] {
		if status, ok := e.resolve(ctx, sub); ok {
			children = append(children, status)
		}
	}

	status, ok := g.Rollup(children)
	if ok {
		e.resolved[g.ID] = status
	} else {
		e.noStatus[g.ID] = true
	}
	return status, ok
}

func (e *groupEval) loadMonitors(ctx context.Context) error {
	if e.monitorsLoaded {
		return nil
	}
	// UserID 0 = the whole install. A folder's status must be derived from ALL of
	// its children, not just those owned by or visible to anyone in particular —
	// see MonitorGroupService.ResolveStatuses for the same reasoning.
	monitors, err := e.svc.monitors.List(ctx, ports.MonitorFilter{})
	if err != nil {
		return err
	}
	e.monitorsByGroup = make(map[int64][]*domain.Monitor)
	for _, m := range monitors {
		if m.GroupID == nil {
			continue
		}
		e.monitorsByGroup[*m.GroupID] = append(e.monitorsByGroup[*m.GroupID], m)
	}
	e.monitorsLoaded = true
	return nil
}
