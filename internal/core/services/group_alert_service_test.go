package services

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// --- fakes ----------------------------------------------------------------

// galertGroupNotifRepo is an in-memory GroupNotificationRepository.
type galertGroupNotifRepo struct {
	mu    sync.Mutex
	next  int64
	links []*domain.GroupNotification
	notif *fakeNotifRepo // resolves ListNotificationsByGroup, as the SQL join does
}

func newGalertGroupNotifRepo(notif *fakeNotifRepo) *galertGroupNotifRepo {
	return &galertGroupNotifRepo{notif: notif}
}

func (r *galertGroupNotifRepo) Attach(_ context.Context, groupID, notificationID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.links {
		if l.GroupID == groupID && l.NotificationID == notificationID {
			return nil // idempotent, per the port contract
		}
	}
	r.next++
	r.links = append(r.links, &domain.GroupNotification{ID: r.next, GroupID: groupID, NotificationID: notificationID})
	return nil
}

func (r *galertGroupNotifRepo) Detach(_ context.Context, groupID, notificationID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.links[:0]
	for _, l := range r.links {
		if l.GroupID == groupID && l.NotificationID == notificationID {
			continue
		}
		out = append(out, l)
	}
	r.links = out
	return nil
}

func (r *galertGroupNotifRepo) ListByGroup(_ context.Context, groupID int64) ([]*domain.GroupNotification, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domain.GroupNotification
	for _, l := range r.links {
		if l.GroupID == groupID {
			cp := *l
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (r *galertGroupNotifRepo) ListByNotification(_ context.Context, notificationID int64) ([]*domain.GroupNotification, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domain.GroupNotification
	for _, l := range r.links {
		if l.NotificationID == notificationID {
			cp := *l
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (r *galertGroupNotifRepo) ListNotificationsByGroup(ctx context.Context, groupID int64) ([]*domain.Notification, error) {
	links, err := r.ListByGroup(ctx, groupID)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.Notification, 0, len(links))
	for _, l := range links {
		n, err := r.notif.GetByID(ctx, l.NotificationID)
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}

// galertSender records every alert actually handed to a provider. The tests
// assert on THIS — the message that would leave the building — not on a return
// value, so a dispatch path that quietly stops sending fails them.
type galertSender struct {
	mu   sync.Mutex
	sent []domain.AlertContext
}

func (s *galertSender) Type() string                    { return "recorder" }
func (s *galertSender) Validate(_ map[string]any) error { return nil }
func (s *galertSender) Send(_ context.Context, cfg map[string]any, alert domain.AlertContext) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Stamp which provider received it, so a test can prove an alert went to the
	// folder's provider and NOT to a sibling folder's.
	alert.CheckOutput, _ = cfg["tag"].(string)
	s.sent = append(s.sent, alert)
	return nil
}

func (s *galertSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

func (s *galertSender) tags() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.sent))
	for _, a := range s.sent {
		out = append(out, a.CheckOutput)
	}
	return out
}

// galertHarness wires the real NotificationService (with a recording sender) to a
// GroupAlertService over in-memory repos.
type galertHarness struct {
	groups     *grpFakeGroupRepo
	groupNotif *galertGroupNotifRepo
	notifs     *fakeNotifRepo
	monitors   *fakeMonitorRepo
	heartbeats *fakeHeartbeatRepo
	sender     *galertSender
	svc        *GroupAlertService
	notifSvc   *NotificationService
}

func newGalertHarness(t *testing.T) *galertHarness {
	t.Helper()
	notifs := newFakeNotifRepo()
	groupNotif := newGalertGroupNotifRepo(notifs)
	sender := &galertSender{}

	notifSvc := NewNotificationService(notifs, &fakeMonitorNotifLinkRepo{})
	notifSvc.SetGroupNotificationRepo(groupNotif)
	notifSvc.RegisterSender(sender)

	h := &galertHarness{
		groups:     newGrpFakeGroupRepo(),
		groupNotif: groupNotif,
		notifs:     notifs,
		monitors:   &fakeMonitorRepo{},
		heartbeats: newFakeHeartbeatRepo(),
		sender:     sender,
		notifSvc:   notifSvc,
	}
	h.svc = NewGroupAlertService(h.groups, h.groupNotif, h.monitors, h.heartbeats, notifSvc)
	return h
}

// addGroup creates a folder with the given condition and returns it.
func (h *galertHarness) addGroup(t *testing.T, name string, condition domain.GroupCondition, parent *int64) *domain.MonitorGroup {
	t.Helper()
	g := &domain.MonitorGroup{UserID: 1, Name: name, Condition: condition, ParentID: parent}
	if err := h.groups.Create(context.Background(), g); err != nil {
		t.Fatalf("create group %s: %v", name, err)
	}
	return g
}

// addMonitor files a monitor under a folder and returns it.
func (h *galertHarness) addMonitor(id int64, groupID int64) *domain.Monitor {
	m := &domain.Monitor{ID: id, UserID: 1, Name: "m", GroupID: &groupID}
	h.monitors.monitors = append(h.monitors.monitors, m)
	return m
}

// addProvider creates a notification and attaches it to a folder. `tag`
// identifies it in the recorded alerts.
func (h *galertHarness) addProvider(t *testing.T, tag string, groupID int64) {
	t.Helper()
	ctx := context.Background()
	n := &domain.Notification{UserID: 1, Name: tag, Type: "recorder", Active: true, Config: map[string]any{"tag": tag}}
	if err := h.notifs.Create(ctx, n); err != nil {
		t.Fatalf("create notification: %v", err)
	}
	if err := h.groupNotif.Attach(ctx, groupID, n.ID); err != nil {
		t.Fatalf("attach notification: %v", err)
	}
}

// beat records a heartbeat for a monitor and runs the folder evaluation, exactly
// as the dispatcher does on the heartbeat path.
func (h *galertHarness) beat(t *testing.T, m *domain.Monitor, status domain.Status) {
	t.Helper()
	ctx := context.Background()
	if err := h.heartbeats.Save(ctx, &domain.Heartbeat{MonitorID: m.ID, Status: status, Time: time.Now().UTC()}); err != nil {
		t.Fatalf("save heartbeat: %v", err)
	}
	h.svc.OnHeartbeat(ctx, m)
}

// --- tests ----------------------------------------------------------------

// A folder whose rollup trips DOWN alerts its own providers exactly once — and
// does not alert a sibling folder's provider.
func TestGroupAlert_TripsDownAlertsOnceToItsOwnProviders(t *testing.T) {
	h := newGalertHarness(t)
	payments := h.addGroup(t, "payments", domain.GroupConditionWorstOfChildren, nil)
	billing := h.addGroup(t, "billing", domain.GroupConditionWorstOfChildren, nil)
	h.addProvider(t, "payments-pager", payments.ID)
	h.addProvider(t, "billing-pager", billing.ID)

	api := h.addMonitor(1, payments.ID)
	h.addMonitor(2, billing.ID)

	h.beat(t, api, domain.StatusUp)
	if got := h.sender.count(); got != 0 {
		t.Fatalf("healthy folder alerted %d times, want 0", got)
	}

	h.beat(t, api, domain.StatusDown)

	if got := h.sender.count(); got != 1 {
		t.Fatalf("folder trip sent %d alerts, want exactly 1 (tags: %v)", got, h.sender.tags())
	}
	if tags := h.sender.tags(); tags[0] != "payments-pager" {
		t.Errorf("alert went to %q, want the tripped folder's own provider %q", tags[0], "payments-pager")
	}
	alert := h.sender.sent[0]
	if alert.Status != domain.StatusDown || alert.MonitorType != "group" || alert.MonitorName != "payments" {
		t.Errorf("alert = {name:%q type:%q status:%v}, want the folder's identity and DOWN",
			alert.MonitorName, alert.MonitorType, alert.Status)
	}

	// Still down: a folder has no resend interval, so it must not re-alert.
	h.beat(t, api, domain.StatusDown)
	if got := h.sender.count(); got != 1 {
		t.Errorf("still-DOWN folder re-alerted (total %d), want no resend", got)
	}
}

// Recovery alerts once, and only after an actual DOWN.
func TestGroupAlert_RecoveryAlertsOnce(t *testing.T) {
	h := newGalertHarness(t)
	g := h.addGroup(t, "payments", domain.GroupConditionWorstOfChildren, nil)
	h.addProvider(t, "pager", g.ID)
	api := h.addMonitor(1, g.ID)

	h.beat(t, api, domain.StatusDown)
	h.beat(t, api, domain.StatusUp)

	if got := h.sender.count(); got != 2 {
		t.Fatalf("got %d alerts, want 2 (trip + recovery)", got)
	}
	recovery := h.sender.sent[1]
	if recovery.Status != domain.StatusUp || recovery.PreviousStatus != domain.StatusDown {
		t.Errorf("recovery alert = %v <- %v, want UP <- DOWN", recovery.Status, recovery.PreviousStatus)
	}

	// Staying up must not alert again.
	h.beat(t, api, domain.StatusUp)
	if got := h.sender.count(); got != 2 {
		t.Errorf("steady-UP folder alerted again (total %d)", got)
	}
}

// PENDING is the retry window and MAINTENANCE is deliberate downtime: neither
// alerts, whatever the folder does.
func TestGroupAlert_PendingAndMaintenanceNeverAlert(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status domain.Status
	}{
		{"pending", domain.StatusPending},
		{"maintenance", domain.StatusMaintenance},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newGalertHarness(t)
			g := h.addGroup(t, "payments", domain.GroupConditionWorstOfChildren, nil)
			h.addProvider(t, "pager", g.ID)
			api := h.addMonitor(1, g.ID)

			h.beat(t, api, domain.StatusUp)
			h.beat(t, api, tc.status)

			if got := h.sender.count(); got != 0 {
				t.Errorf("%s transition sent %d alerts, want 0", tc.name, got)
			}
		})
	}
}

// An "ignore" folder derives no status at all, so it can never alert — even with
// a provider attached to it.
func TestGroupAlert_IgnoreConditionNeverAlerts(t *testing.T) {
	h := newGalertHarness(t)
	g := h.addGroup(t, "archive", domain.GroupConditionIgnore, nil)
	h.addProvider(t, "pager", g.ID)
	api := h.addMonitor(1, g.ID)

	h.beat(t, api, domain.StatusUp)
	h.beat(t, api, domain.StatusDown)

	if got := h.sender.count(); got != 0 {
		t.Errorf("ignore-condition folder alerted %d times, want 0", got)
	}
}

// The folder's condition — not "any monitor is down" — decides the alert. An
// all_down folder with one surviving child stays quiet.
func TestGroupAlert_RespectsCondition(t *testing.T) {
	h := newGalertHarness(t)
	g := h.addGroup(t, "pool", domain.GroupConditionAllDown, nil)
	h.addProvider(t, "pager", g.ID)
	a := h.addMonitor(1, g.ID)
	b := h.addMonitor(2, g.ID)

	h.beat(t, a, domain.StatusUp)
	h.beat(t, b, domain.StatusUp)
	h.beat(t, a, domain.StatusDown) // one down, one up — the pool survives

	if got := h.sender.count(); got != 0 {
		t.Fatalf("all_down folder alerted with a survivor still UP (%d alerts)", got)
	}

	h.beat(t, b, domain.StatusDown) // now the whole pool is down

	if got := h.sender.count(); got != 1 {
		t.Errorf("all_down folder sent %d alerts once every child was DOWN, want 1", got)
	}
}

// A monitor deep in a subfolder trips its ancestors too: the parent's rollup sees
// the subfolder's derived status.
func TestGroupAlert_NestedSubfolderTripsAncestor(t *testing.T) {
	h := newGalertHarness(t)
	parent := h.addGroup(t, "platform", domain.GroupConditionWorstOfChildren, nil)
	child := h.addGroup(t, "payments", domain.GroupConditionWorstOfChildren, &parent.ID)
	h.addProvider(t, "platform-pager", parent.ID)
	h.addProvider(t, "payments-pager", child.ID)

	api := h.addMonitor(1, child.ID)
	h.beat(t, api, domain.StatusUp)
	h.beat(t, api, domain.StatusDown)

	tags := h.sender.tags()
	if len(tags) != 2 {
		t.Fatalf("got %d alerts (%v), want 2 — the subfolder and its parent", len(tags), tags)
	}
	seen := map[string]bool{tags[0]: true, tags[1]: true}
	if !seen["payments-pager"] || !seen["platform-pager"] {
		t.Errorf("alerts went to %v, want both the subfolder's and the parent's provider", tags)
	}
}

// THE RACE. Two monitors in one folder are checked by two workers at the same
// instant; both recompute the rollup as DOWN. Exactly one alert may leave the
// building — the compare-and-set decides which worker sends.
func TestGroupAlert_ConcurrentHeartbeatsAlertOnce(t *testing.T) {
	h := newGalertHarness(t)
	g := h.addGroup(t, "payments", domain.GroupConditionWorstOfChildren, nil)
	h.addProvider(t, "pager", g.ID)
	a := h.addMonitor(1, g.ID)
	b := h.addMonitor(2, g.ID)

	ctx := context.Background()
	for _, m := range []*domain.Monitor{a, b} {
		if err := h.heartbeats.Save(ctx, &domain.Heartbeat{MonitorID: m.ID, Status: domain.StatusDown, Time: time.Now().UTC()}); err != nil {
			t.Fatalf("save heartbeat: %v", err)
		}
	}

	var wg sync.WaitGroup
	for _, m := range []*domain.Monitor{a, b} {
		wg.Add(1)
		go func(m *domain.Monitor) {
			defer wg.Done()
			h.svc.OnHeartbeat(ctx, m)
		}(m)
	}
	wg.Wait()

	if got := h.sender.count(); got != 1 {
		t.Errorf("concurrent heartbeats in one folder sent %d alerts, want exactly 1 "+
			"(the compare-and-set on last_status is what makes this 1, not 2)", got)
	}
}

// A monitor filed in no folder must not blow up — or reach for a folder.
func TestGroupAlert_MonitorWithoutGroupIsNoOp(t *testing.T) {
	h := newGalertHarness(t)
	m := &domain.Monitor{ID: 1, UserID: 1, Name: "loose"}

	h.svc.OnHeartbeat(context.Background(), m)

	if got := h.sender.count(); got != 0 {
		t.Errorf("ungrouped monitor produced %d folder alerts, want 0", got)
	}
}

// THE ASK. A notification flagged is_default ("auto-attach to new monitors") must
// attach to a new MONITOR and never to a new FOLDER.
//
// This is the test that would catch the feature being wrong: a version that
// pre-ticked default providers on folders would still create the group, still
// return 201, and still pass every other test in this file.
func TestGroupCreate_DefaultNotificationNeverAutoAttaches(t *testing.T) {
	ctx := context.Background()
	notifs := newFakeNotifRepo()
	groupNotif := newGalertGroupNotifRepo(notifs)
	monitorNotif := &fakeMonitorNotifLinkRepo{}

	def := &domain.Notification{UserID: 1, Name: "default-pager", Type: "recorder", Active: true, IsDefault: true}
	if err := notifs.Create(ctx, def); err != nil {
		t.Fatalf("create notification: %v", err)
	}

	groupRepo := newGrpFakeGroupRepo()
	groupSvc := NewMonitorGroupService(groupRepo, &fakeMonitorRepo{}, newFakeHeartbeatRepo(), testLogger())

	g := &domain.MonitorGroup{UserID: 1, Name: "payments", Condition: domain.GroupConditionWorstOfChildren}
	if err := groupSvc.Create(ctx, g); err != nil {
		t.Fatalf("create group: %v", err)
	}

	links, err := groupNotif.ListByGroup(ctx, g.ID)
	if err != nil {
		t.Fatalf("list group notifications: %v", err)
	}
	if len(links) != 0 {
		t.Errorf("creating a folder auto-attached %d default notification(s); folders must never auto-attach — "+
			"is_default means 'attach to new MONITORS'", len(links))
	}

	// The same flag must still do its job on a monitor — this is the control, and
	// it is what keeps the fix from being "delete the auto-attach feature".
	monitorSvc := NewMonitorService(newCloneFakeMonitorRepo(), newFakeBus())
	monitorSvc.SetDefaultNotificationLinker(notifs, monitorNotif)
	m := &domain.Monitor{UserID: 1, Name: "api", Type: "http", Interval: 60}
	if err := monitorSvc.Create(ctx, m); err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	monLinks, err := monitorNotif.ListByMonitor(ctx, m.ID)
	if err != nil {
		t.Fatalf("list monitor notifications: %v", err)
	}
	if len(monLinks) != 1 {
		t.Errorf("creating a monitor attached %d default notifications, want 1 — "+
			"the monitor auto-attach must keep working", len(monLinks))
	}
}
