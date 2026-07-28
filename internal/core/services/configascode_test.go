package services_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/memory"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// --- minimal fakes ---------------------------------------------------------

type cfgTagRepo struct {
	mu   sync.Mutex
	byID map[int64]*domain.Tag
	next atomic.Int64
}

func newCfgTagRepo() *cfgTagRepo { return &cfgTagRepo{byID: map[int64]*domain.Tag{}} }

func (r *cfgTagRepo) Create(_ context.Context, t *domain.Tag) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.next.Add(1)
	t.ID = id
	cp := *t
	r.byID[id] = &cp
	return nil
}
func (r *cfgTagRepo) GetByID(_ context.Context, id int64) (*domain.Tag, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *t
	return &cp, nil
}
func (r *cfgTagRepo) List(context.Context) ([]*domain.Tag, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.Tag, 0, len(r.byID))
	for _, t := range r.byID {
		cp := *t
		out = append(out, &cp)
	}
	return out, nil
}
func (r *cfgTagRepo) Update(_ context.Context, t *domain.Tag) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[t.ID]; !ok {
		return ports.ErrNotFound
	}
	cp := *t
	r.byID[t.ID] = &cp
	return nil
}
func (r *cfgTagRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byID, id)
	return nil
}

type cfgNotifRepo struct {
	mu   sync.Mutex
	byID map[int64]*domain.Notification
	next atomic.Int64
}

func newCfgNotifRepo() *cfgNotifRepo {
	return &cfgNotifRepo{byID: map[int64]*domain.Notification{}}
}
func (r *cfgNotifRepo) Create(_ context.Context, n *domain.Notification) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.next.Add(1)
	n.ID = id
	n.CreatedAt = time.Now().UTC()
	n.UpdatedAt = n.CreatedAt
	cp := *n
	if n.Config != nil {
		cp.Config = copyAnyMap(n.Config)
	}
	r.byID[id] = &cp
	return nil
}
func (r *cfgNotifRepo) GetByID(_ context.Context, id int64) (*domain.Notification, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *n
	if n.Config != nil {
		cp.Config = copyAnyMap(n.Config)
	}
	return &cp, nil
}
func (r *cfgNotifRepo) List(_ context.Context, userID int64) ([]*domain.Notification, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []*domain.Notification{}
	for _, n := range r.byID {
		if n.UserID == userID {
			cp := *n
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (r *cfgNotifRepo) ListAll(context.Context) ([]*domain.Notification, error) {
	return r.List(context.Background(), 0)
}
func (r *cfgNotifRepo) GetByMonitorID(context.Context, int64) ([]*domain.Notification, error) {
	return nil, nil
}
func (r *cfgNotifRepo) Update(_ context.Context, n *domain.Notification) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[n.ID]; !ok {
		return ports.ErrNotFound
	}
	cp := *n
	if n.Config != nil {
		cp.Config = copyAnyMap(n.Config)
	}
	r.byID[n.ID] = &cp
	return nil
}
func (r *cfgNotifRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byID, id)
	return nil
}

func copyAnyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

type cfgMonRepo struct {
	mu   sync.Mutex
	byID map[int64]*domain.Monitor
	next atomic.Int64
}

func newCfgMonRepo() *cfgMonRepo { return &cfgMonRepo{byID: map[int64]*domain.Monitor{}} }
func (r *cfgMonRepo) Create(_ context.Context, m *domain.Monitor) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.next.Add(1)
	m.ID = id
	cp := *m
	if m.Config != nil {
		cp.Config = copyAnyMap(m.Config)
	}
	r.byID[id] = &cp
	return nil
}
func (r *cfgMonRepo) GetByID(_ context.Context, id int64) (*domain.Monitor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *m
	return &cp, nil
}
func (r *cfgMonRepo) GetByPushToken(context.Context, string) (*domain.Monitor, error) {
	return nil, ports.ErrNotFound
}
func (r *cfgMonRepo) List(context.Context, ports.MonitorFilter) ([]*domain.Monitor, error) {
	return nil, nil
}
func (r *cfgMonRepo) ListActive(context.Context) ([]*domain.Monitor, error) { return nil, nil }
func (r *cfgMonRepo) Update(_ context.Context, m *domain.Monitor) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[m.ID]; !ok {
		return ports.ErrNotFound
	}
	cp := *m
	if m.Config != nil {
		cp.Config = copyAnyMap(m.Config)
	}
	r.byID[m.ID] = &cp
	return nil
}
func (r *cfgMonRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byID, id)
	return nil
}
func (r *cfgMonRepo) ClaimBatch(context.Context, string, int, time.Duration) ([]*domain.Monitor, error) {
	return nil, nil
}
func (r *cfgMonRepo) RefreshLease(context.Context, string) (int64, error)  { return 0, nil }
func (r *cfgMonRepo) ReleaseLeases(context.Context, string) (int64, error) { return 0, nil }

// stubs for unused repos in focused tests
type cfgNilProxy struct{}

func (cfgNilProxy) Create(context.Context, *domain.Proxy) error { return nil }
func (cfgNilProxy) GetByID(context.Context, int64) (*domain.Proxy, error) {
	return nil, ports.ErrNotFound
}
func (cfgNilProxy) List(context.Context, int64) ([]*domain.Proxy, error) { return nil, nil }
func (cfgNilProxy) Update(context.Context, *domain.Proxy) error          { return nil }
func (cfgNilProxy) Delete(context.Context, int64) error                  { return nil }

type cfgNilGroup struct{}

func (cfgNilGroup) Create(context.Context, *domain.MonitorGroup) error { return nil }
func (cfgNilGroup) GetByID(context.Context, int64) (*domain.MonitorGroup, error) {
	return nil, ports.ErrNotFound
}
func (cfgNilGroup) List(context.Context, int64) ([]*domain.MonitorGroup, error) { return nil, nil }
func (cfgNilGroup) ListAll(context.Context) ([]*domain.MonitorGroup, error)     { return nil, nil }
func (cfgNilGroup) Update(context.Context, *domain.MonitorGroup) error          { return nil }
func (cfgNilGroup) Delete(context.Context, int64) error                         { return nil }
func (cfgNilGroup) ClaimStatusTransition(context.Context, int64, *domain.Status, domain.Status) (bool, error) {
	return false, nil
}

type cfgNilMT struct{}

func (cfgNilMT) Assign(context.Context, int64, int64, string) error { return nil }
func (cfgNilMT) Remove(context.Context, int64, int64) error         { return nil }
func (cfgNilMT) ListByMonitor(context.Context, int64) ([]*domain.MonitorTag, error) {
	return nil, nil
}
func (cfgNilMT) ListByMonitors(context.Context, []int64) (map[int64][]*domain.MonitorTag, error) {
	return map[int64][]*domain.MonitorTag{}, nil
}

type cfgNilMN struct{}

func (cfgNilMN) Attach(context.Context, int64, int64) error { return nil }
func (cfgNilMN) Detach(context.Context, int64, int64) error { return nil }
func (cfgNilMN) ListByMonitor(context.Context, int64) ([]*domain.MonitorNotification, error) {
	return nil, nil
}
func (cfgNilMN) ListByNotification(context.Context, int64) ([]*domain.MonitorNotification, error) {
	return nil, nil
}

type cfgNilGN struct{}

func (cfgNilGN) Attach(context.Context, int64, int64) error { return nil }
func (cfgNilGN) Detach(context.Context, int64, int64) error { return nil }
func (cfgNilGN) ListByGroup(context.Context, int64) ([]*domain.GroupNotification, error) {
	return nil, nil
}
func (cfgNilGN) ListByNotification(context.Context, int64) ([]*domain.GroupNotification, error) {
	return nil, nil
}
func (cfgNilGN) ListNotificationsByGroup(context.Context, int64) ([]*domain.Notification, error) {
	return nil, nil
}

type cfgNilSP struct{}

func (cfgNilSP) Create(context.Context, *domain.StatusPage) error { return nil }
func (cfgNilSP) GetByID(context.Context, int64) (*domain.StatusPage, error) {
	return nil, ports.ErrNotFound
}
func (cfgNilSP) GetBySlug(context.Context, string) (*domain.StatusPage, error) {
	return nil, ports.ErrNotFound
}
func (cfgNilSP) List(context.Context) ([]*domain.StatusPage, error) { return nil, nil }
func (cfgNilSP) Update(context.Context, *domain.StatusPage) error   { return nil }
func (cfgNilSP) Delete(context.Context, int64) error                { return nil }

type cfgNilSPM struct{}

func (cfgNilSPM) AddMonitor(context.Context, int64, int64, int) error   { return nil }
func (cfgNilSPM) RemoveMonitor(context.Context, int64, int64) error     { return nil }
func (cfgNilSPM) ReorderMonitors(context.Context, int64, []int64) error { return nil }
func (cfgNilSPM) ListByStatusPage(context.Context, int64) ([]*domain.StatusPageMonitor, error) {
	return nil, nil
}

type cfgNilMaint struct{}

func (cfgNilMaint) Create(context.Context, *domain.MaintenanceWindow) error { return nil }
func (cfgNilMaint) GetByID(context.Context, int64) (*domain.MaintenanceWindow, error) {
	return nil, ports.ErrNotFound
}
func (cfgNilMaint) List(context.Context, int64) ([]*domain.MaintenanceWindow, error) {
	return nil, nil
}
func (cfgNilMaint) ListAll(context.Context) ([]*domain.MaintenanceWindow, error) { return nil, nil }
func (cfgNilMaint) Update(context.Context, *domain.MaintenanceWindow) error      { return nil }
func (cfgNilMaint) Delete(context.Context, int64) error                          { return nil }

type cfgNilMM struct{}

func (cfgNilMM) Assign(context.Context, int64, int64) error { return nil }
func (cfgNilMM) Remove(context.Context, int64, int64) error { return nil }
func (cfgNilMM) ListByMaintenance(context.Context, int64) ([]int64, error) {
	return nil, nil
}
func (cfgNilMM) ListByMonitor(context.Context, int64) ([]*domain.MaintenanceWindow, error) {
	return nil, nil
}

type cfgPass struct{}

func (cfgPass) Hash(p string) (string, error) { return "h:" + p, nil }
func (cfgPass) Verify(h, p string) error {
	if h != "h:"+p {
		return errors.New("mismatch")
	}
	return nil
}

func newConfigSvc(t *testing.T) (
	*services.ConfigService,
	*memory.ConfigKeyRepo,
	*cfgTagRepo,
	*cfgNotifRepo,
	*cfgMonRepo,
) {
	t.Helper()
	keys := memory.NewConfigKeyRepo()
	tags := newCfgTagRepo()
	notifs := newCfgNotifRepo()
	mons := newCfgMonRepo()
	svc := services.NewConfigService(
		keys, tags, cfgNilProxy{}, notifs, cfgNilGroup{}, mons,
		cfgNilMT{}, cfgNilMN{}, cfgNilGN{}, cfgNilSP{}, cfgNilSPM{},
		cfgNilMaint{}, cfgNilMM{}, cfgPass{},
	)
	return svc, keys, tags, notifs, mons
}

func sampleDoc() *services.ConfigDocument {
	active := true
	return &services.ConfigDocument{
		APIVersion: services.ConfigAPIVersion,
		Kind:       services.ConfigKind,
		Spec: services.ConfigSpec{
			Tags: []services.ConfigTag{
				{Key: "prod", Name: "Production", Color: "#e11"},
			},
			Notifications: []services.ConfigNotification{
				{
					Key: "slack-ops", Name: "Slack Ops", Type: "slack", Active: &active,
					Config: map[string]any{"webhook_url": "https://hooks.slack.com/secret"},
				},
			},
			Monitors: []services.ConfigMonitor{
				{
					Key: "api-health", Name: "API Health", Type: "http", Active: &active,
					Interval: 60, Config: map[string]any{"url": "https://api.example.com/health"},
				},
			},
			MonitorNotifications: []services.ConfigMonitorNotification{
				{Monitor: "api-health", Notification: "slack-ops"},
			},
		},
	}
}

func TestConfigValidate_RejectsUnknownVersion(t *testing.T) {
	svc, _, _, _, _ := newConfigSvc(t)
	doc := sampleDoc()
	doc.APIVersion = "phoenix.dev/v99"
	errs := svc.Validate(context.Background(), doc)
	if len(errs) == 0 {
		t.Fatal("expected version error")
	}
	if !strings.Contains(errs[0], "apiVersion") {
		t.Fatalf("got %v", errs)
	}
}

func TestConfigApply_CreatesAndKeys(t *testing.T) {
	svc, keys, tags, notifs, mons := newConfigSvc(t)
	ctx := context.Background()
	doc := sampleDoc()
	res, err := svc.Apply(ctx, 1, doc, services.ConfigApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Creates < 3 {
		t.Fatalf("creates=%d applied=%+v", res.Creates, res.Applied)
	}
	if _, err := keys.GetByKey(ctx, domain.ConfigResourceTag, "prod"); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.GetByKey(ctx, domain.ConfigResourceNotification, "slack-ops"); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.GetByKey(ctx, domain.ConfigResourceMonitor, "api-health"); err != nil {
		t.Fatal(err)
	}
	// Effects: rows exist
	if len(tags.byID) != 1 || len(notifs.byID) != 1 || len(mons.byID) != 1 {
		t.Fatalf("tags=%d notifs=%d mons=%d", len(tags.byID), len(notifs.byID), len(mons.byID))
	}
}

func TestConfigApply_IdempotentSecondRun(t *testing.T) {
	svc, _, _, _, _ := newConfigSvc(t)
	ctx := context.Background()
	doc := sampleDoc()
	if _, err := svc.Apply(ctx, 1, doc, services.ConfigApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Apply(ctx, 1, doc, services.ConfigApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Creates != 0 || res.Updates != 0 || res.Deletes != 0 {
		t.Fatalf("second apply not idempotent: creates=%d updates=%d deletes=%d applied=%+v",
			res.Creates, res.Updates, res.Deletes, res.Applied)
	}
	if res.Unchanged < 3 {
		t.Fatalf("unchanged=%d", res.Unchanged)
	}
}

func TestConfigPlan_ReportsCreatesUpdatesUnchanged(t *testing.T) {
	svc, _, _, _, _ := newConfigSvc(t)
	ctx := context.Background()
	doc := sampleDoc()
	plan, err := svc.Plan(ctx, 1, doc, services.ConfigApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Valid || plan.Creates != 3 {
		t.Fatalf("plan=%+v", plan)
	}
	if _, err := svc.Apply(ctx, 1, doc, services.ConfigApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	// Change a tag name → update
	doc.Spec.Tags[0].Name = "Prod"
	plan2, err := svc.Plan(ctx, 1, doc, services.ConfigApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if plan2.Updates < 1 {
		t.Fatalf("expected update, plan=%+v", plan2)
	}
	if plan2.Unchanged < 2 {
		t.Fatalf("expected other resources unchanged, plan=%+v", plan2)
	}
}

func TestConfigApply_PruneOnlyKeyedMissing(t *testing.T) {
	svc, keys, tags, _, _ := newConfigSvc(t)
	ctx := context.Background()
	doc := sampleDoc()
	if _, err := svc.Apply(ctx, 1, doc, services.ConfigApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	// Unkeyed tag (admin UI) must survive prune.
	orphan := &domain.Tag{Name: "orphan", Color: "#000"}
	if err := tags.Create(ctx, orphan); err != nil {
		t.Fatal(err)
	}
	// Remove prod from document and prune
	doc.Spec.Tags = nil
	res, err := svc.Apply(ctx, 1, doc, services.ConfigApplyOptions{Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Deletes < 1 {
		t.Fatalf("expected prune delete, res=%+v", res)
	}
	if _, err := keys.GetByKey(ctx, domain.ConfigResourceTag, "prod"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("prod key should be gone: %v", err)
	}
	if _, err := tags.GetByID(ctx, orphan.ID); err != nil {
		t.Fatal("unkeyed orphan tag was pruned")
	}
}

func TestConfigExport_RedactsSecrets(t *testing.T) {
	svc, _, _, _, _ := newConfigSvc(t)
	ctx := context.Background()
	doc := sampleDoc()
	if _, err := svc.Apply(ctx, 1, doc, services.ConfigApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	out, err := svc.Export(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Spec.Notifications) != 1 {
		t.Fatalf("notifs=%d", len(out.Spec.Notifications))
	}
	url, _ := out.Spec.Notifications[0].Config["webhook_url"].(string)
	if url != services.ConfigSecretRedacted {
		t.Fatalf("webhook not redacted: %q", url)
	}
	if strings.Contains(url, "hooks.slack.com") {
		t.Fatal("plaintext secret leaked")
	}
}

func TestConfigApply_PreservesRedactedSecrets(t *testing.T) {
	svc, keys, _, notifs, _ := newConfigSvc(t)
	ctx := context.Background()
	doc := sampleDoc()
	if _, err := svc.Apply(ctx, 1, doc, services.ConfigApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	ck, _ := keys.GetByKey(ctx, domain.ConfigResourceNotification, "slack-ops")
	before, _ := notifs.GetByID(ctx, ck.ResourceID)
	secret, _ := before.Config["webhook_url"].(string)

	// Re-apply with redacted export shape.
	doc.Spec.Notifications[0].Config["webhook_url"] = services.ConfigSecretRedacted
	if _, err := svc.Apply(ctx, 1, doc, services.ConfigApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	after, _ := notifs.GetByID(ctx, ck.ResourceID)
	got, _ := after.Config["webhook_url"].(string)
	if got != secret {
		t.Fatalf("secret not preserved: before=%q after=%q", secret, got)
	}
}

func TestConfigApply_OverwritesExplicitSecrets(t *testing.T) {
	svc, keys, _, notifs, _ := newConfigSvc(t)
	ctx := context.Background()
	doc := sampleDoc()
	if _, err := svc.Apply(ctx, 1, doc, services.ConfigApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	doc.Spec.Notifications[0].Config["webhook_url"] = "https://hooks.slack.com/new"
	if _, err := svc.Apply(ctx, 1, doc, services.ConfigApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	ck, _ := keys.GetByKey(ctx, domain.ConfigResourceNotification, "slack-ops")
	after, _ := notifs.GetByID(ctx, ck.ResourceID)
	if after.Config["webhook_url"] != "https://hooks.slack.com/new" {
		t.Fatalf("got %v", after.Config["webhook_url"])
	}
}

func TestConfigExport_OmitsUnkeyed(t *testing.T) {
	svc, _, tags, _, _ := newConfigSvc(t)
	ctx := context.Background()
	// Only create unkeyed tag
	_ = tags.Create(ctx, &domain.Tag{Name: "ui-only", Color: "#111"})
	out, err := svc.Export(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Spec.Tags) != 0 {
		t.Fatalf("expected no tags, got %+v", out.Spec.Tags)
	}
}

func TestConfigApply_ResolvesKeyRefs(t *testing.T) {
	// Monitor → notification link is validated; apply succeeds when keys resolve.
	svc, _, _, _, _ := newConfigSvc(t)
	ctx := context.Background()
	doc := sampleDoc()
	// Bad ref must fail validation
	doc.Spec.MonitorNotifications[0].Notification = "missing"
	errs := svc.Validate(ctx, doc)
	if len(errs) == 0 {
		t.Fatal("expected ref error")
	}
}
