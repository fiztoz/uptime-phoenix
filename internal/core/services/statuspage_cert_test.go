package services

import (
	"context"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type certFakeTLS struct {
	info map[int64]*ports.TLSInfo
}

func (r *certFakeTLS) Upsert(_ context.Context, info *ports.TLSInfo) error {
	if r.info == nil {
		r.info = make(map[int64]*ports.TLSInfo)
	}
	cp := *info
	r.info[info.MonitorID] = &cp
	return nil
}

func (r *certFakeTLS) GetByMonitorID(_ context.Context, monitorID int64) (*ports.TLSInfo, error) {
	info, ok := r.info[monitorID]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *info
	return &cp, nil
}

// certSPService builds a StatusPageService with the minimum fakes needed to
// exercise public cert emission via GetPublicStatus.
func certSPService(sp *domain.StatusPage, mon *domain.Monitor, tls ports.TLSInfoRepository) *StatusPageService {
	svc := &StatusPageService{
		repo:         &certFakeSPRepo{sp: sp},
		incidentRepo: certFakeIncident{},
		cnameRepo:    certFakeCNAME{},
		spMonitorRepo: &certFakeSPMon{
			links: []*domain.StatusPageMonitor{{StatusPageID: sp.ID, MonitorID: mon.ID}},
		},
		monitorRepo: &certFakeMon{m: mon},
		hbRepo:      &spUptimeHeartbeatRepo{},
		tlsInfo:     tls,
	}
	return svc
}

type certFakeSPRepo struct{ sp *domain.StatusPage }

func (r *certFakeSPRepo) Create(context.Context, *domain.StatusPage) error { return nil }
func (r *certFakeSPRepo) GetByID(_ context.Context, id int64) (*domain.StatusPage, error) {
	if r.sp == nil || r.sp.ID != id {
		return nil, ports.ErrNotFound
	}
	cp := *r.sp
	return &cp, nil
}
func (r *certFakeSPRepo) GetBySlug(_ context.Context, slug string) (*domain.StatusPage, error) {
	if r.sp == nil || r.sp.Slug != slug {
		return nil, ports.ErrNotFound
	}
	cp := *r.sp
	return &cp, nil
}
func (r *certFakeSPRepo) List(context.Context) ([]*domain.StatusPage, error) { return nil, nil }
func (r *certFakeSPRepo) Update(context.Context, *domain.StatusPage) error   { return nil }
func (r *certFakeSPRepo) Delete(context.Context, int64) error                { return nil }

type certFakeSPMon struct {
	links []*domain.StatusPageMonitor
}

func (r *certFakeSPMon) AddMonitor(context.Context, int64, int64, int) error { return nil }
func (r *certFakeSPMon) RemoveMonitor(context.Context, int64, int64) error   { return nil }
func (r *certFakeSPMon) ReorderMonitors(context.Context, int64, []int64) error {
	return nil
}
func (r *certFakeSPMon) ListByStatusPage(context.Context, int64) ([]*domain.StatusPageMonitor, error) {
	return r.links, nil
}

type certFakeMon struct{ m *domain.Monitor }

func (r *certFakeMon) Create(context.Context, *domain.Monitor) error { return nil }
func (r *certFakeMon) GetByID(_ context.Context, id int64) (*domain.Monitor, error) {
	if r.m == nil || r.m.ID != id {
		return nil, ports.ErrNotFound
	}
	cp := *r.m
	return &cp, nil
}
func (r *certFakeMon) GetByPushToken(context.Context, string) (*domain.Monitor, error) {
	return nil, ports.ErrNotFound
}
func (r *certFakeMon) List(context.Context, ports.MonitorFilter) ([]*domain.Monitor, error) {
	return nil, nil
}
func (r *certFakeMon) ListActive(context.Context) ([]*domain.Monitor, error) { return nil, nil }
func (r *certFakeMon) Update(context.Context, *domain.Monitor) error         { return nil }
func (r *certFakeMon) Delete(context.Context, int64) error                   { return nil }
func (r *certFakeMon) ClaimBatch(context.Context, string, int, time.Duration) ([]*domain.Monitor, error) {
	return nil, nil
}
func (r *certFakeMon) RefreshLease(context.Context, string) (int64, error) { return 0, nil }
func (r *certFakeMon) ReleaseLeases(context.Context, string) (int64, error) {
	return 0, nil
}

type certFakeIncident struct{}

func (certFakeIncident) Create(context.Context, *domain.Incident) error { return nil }
func (certFakeIncident) GetByID(context.Context, int64) (*domain.Incident, error) {
	return nil, ports.ErrNotFound
}
func (certFakeIncident) ListByStatusPage(context.Context, int64) ([]*domain.Incident, error) {
	return nil, nil
}
func (certFakeIncident) ListAll(context.Context) ([]*domain.Incident, error) { return nil, nil }
func (certFakeIncident) Update(context.Context, *domain.Incident) error      { return nil }
func (certFakeIncident) Delete(context.Context, int64) error                 { return nil }

type certFakeCNAME struct{}

func (certFakeCNAME) Create(context.Context, *domain.StatusPageCNAME) error { return nil }
func (certFakeCNAME) Delete(context.Context, int64) error                   { return nil }
func (certFakeCNAME) ListByStatusPage(context.Context, int64) ([]*domain.StatusPageCNAME, error) {
	return nil, nil
}
func (certFakeCNAME) GetByDomain(context.Context, string) (*domain.StatusPageCNAME, error) {
	return nil, ports.ErrNotFound
}

func TestPublicMonitorStatus_EmitsCertWhenTLSExists(t *testing.T) {
	sp := &domain.StatusPage{ID: 1, Slug: "acme", Title: "Acme", Published: true}
	mon := &domain.Monitor{ID: 9, Name: "web", Type: "http"}
	notAfter := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	tls := &certFakeTLS{info: map[int64]*ports.TLSInfo{
		9: {MonitorID: 9, DaysRemaining: 13, NotAfter: notAfter, Issuer: "Let's Encrypt"},
	}}
	svc := certSPService(sp, mon, tls)

	resp, err := svc.GetPublicStatus(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Monitors) != 1 {
		t.Fatalf("monitors: %d", len(resp.Monitors))
	}
	m := resp.Monitors[0]
	if m.CertExpiryDate == nil || *m.CertExpiryDate != notAfter.Format(time.RFC3339) {
		t.Fatalf("cert_expiry_date: %v", m.CertExpiryDate)
	}
	if m.CertDaysLeft == nil || *m.CertDaysLeft != 13 {
		t.Fatalf("cert_days_left: %v", m.CertDaysLeft)
	}
}

func TestPublicMonitorStatus_OmitsCertWhenNoTLS(t *testing.T) {
	sp := &domain.StatusPage{ID: 1, Slug: "acme", Title: "Acme", Published: true}
	mon := &domain.Monitor{ID: 9, Name: "ping", Type: "ping"}
	svc := certSPService(sp, mon, &certFakeTLS{info: map[int64]*ports.TLSInfo{}})

	resp, err := svc.GetPublicStatus(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	m := resp.Monitors[0]
	if m.CertExpiryDate != nil || m.CertDaysLeft != nil {
		t.Fatalf("must not invent cert fields: expiry=%v days=%v", m.CertExpiryDate, m.CertDaysLeft)
	}
}

func TestPublicMonitorStatus_OmitsCertWhenTLSRepoNil(t *testing.T) {
	sp := &domain.StatusPage{ID: 1, Slug: "acme", Title: "Acme", Published: true}
	mon := &domain.Monitor{ID: 9, Name: "web", Type: "http"}
	svc := certSPService(sp, mon, nil)

	resp, err := svc.GetPublicStatus(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Monitors[0].CertExpiryDate != nil {
		t.Fatal("nil TLS repo must not invent cert fields")
	}
}
