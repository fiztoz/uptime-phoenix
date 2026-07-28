package services

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// --- fakes -----------------------------------------------------------------

type certFakeTLSRepo struct {
	mu   sync.Mutex
	info map[int64]*ports.TLSInfo
}

func newCertFakeTLSRepo() *certFakeTLSRepo {
	return &certFakeTLSRepo{info: make(map[int64]*ports.TLSInfo)}
}

func (r *certFakeTLSRepo) Upsert(_ context.Context, info *ports.TLSInfo) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *info
	r.info[info.MonitorID] = &cp
	return nil
}

func (r *certFakeTLSRepo) GetByMonitorID(_ context.Context, monitorID int64) (*ports.TLSInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	info, ok := r.info[monitorID]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *info
	return &cp, nil
}

type certFakeNotifier struct {
	mu    sync.Mutex
	calls []domain.AlertContext
	err   error
}

func (n *certFakeNotifier) Dispatch(_ context.Context, _ *domain.Monitor, alert domain.AlertContext) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, alert)
	return n.err
}

func (n *certFakeNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.calls)
}

func (n *certFakeNotifier) last() domain.AlertContext {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.calls) == 0 {
		return domain.AlertContext{}
	}
	return n.calls[len(n.calls)-1]
}

type certFakeMaint struct {
	active bool
	err    error
}

func (m *certFakeMaint) IsActive(_ context.Context, _ int64) (bool, error) {
	return m.active, m.err
}

func certMeta(days int, notAfter time.Time, issuer string) map[string]string {
	return map[string]string{
		"tls_days_remaining": strconv.Itoa(days),
		"tls_not_after":      notAfter.UTC().Format(time.RFC3339),
		"tls_issuer":         issuer,
	}
}

func testMonitor(notify bool) *domain.Monitor {
	return &domain.Monitor{
		ID:               1,
		Name:             "api",
		Type:             "http",
		CertExpiryNotify: notify,
		Config:           map[string]any{"url": "https://example.com"},
	}
}

// --- tests -----------------------------------------------------------------

func TestCertificateAlert_FirstObservationAt30(t *testing.T) {
	tls := newCertFakeTLSRepo()
	notif := &certFakeNotifier{}
	svc := NewCertificateAlertService(tls, notif, &certFakeMaint{})
	notAfter := time.Now().UTC().Add(20 * 24 * time.Hour)

	svc.OnCheck(context.Background(), testMonitor(true), certMeta(20, notAfter, "CN=Test"))

	if notif.count() != 1 {
		t.Fatalf("dispatch count = %d, want 1", notif.count())
	}
	got := notif.last()
	if got.EventKind != domain.AlertEventCertificateExpiry {
		t.Errorf("EventKind = %q, want certificate_expiry", got.EventKind)
	}
	if got.CertThreshold != 30 {
		t.Errorf("CertThreshold = %d, want 30", got.CertThreshold)
	}
	stored, err := tls.GetByMonitorID(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if stored.LastCertAlertThreshold != 30 {
		t.Errorf("stored threshold = %d, want 30", stored.LastCertAlertThreshold)
	}
}

func TestCertificateAlert_Progression30to14to7(t *testing.T) {
	tls := newCertFakeTLSRepo()
	notif := &certFakeNotifier{}
	svc := NewCertificateAlertService(tls, notif, &certFakeMaint{})
	notAfter := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)
	m := testMonitor(true)

	svc.OnCheck(context.Background(), m, certMeta(20, notAfter, "CN=Test")) // → 30
	svc.OnCheck(context.Background(), m, certMeta(13, notAfter, "CN=Test")) // → 14
	svc.OnCheck(context.Background(), m, certMeta(6, notAfter, "CN=Test"))  // → 7

	if notif.count() != 3 {
		t.Fatalf("dispatch count = %d, want 3", notif.count())
	}
	want := []int{30, 14, 7}
	for i, thr := range want {
		if notif.calls[i].CertThreshold != thr {
			t.Errorf("call %d threshold = %d, want %d", i, notif.calls[i].CertThreshold, thr)
		}
	}
}

func TestCertificateAlert_DuplicateHeartbeatNoResend(t *testing.T) {
	tls := newCertFakeTLSRepo()
	notif := &certFakeNotifier{}
	svc := NewCertificateAlertService(tls, notif, &certFakeMaint{})
	notAfter := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)
	m := testMonitor(true)
	meta := certMeta(20, notAfter, "CN=Test")

	svc.OnCheck(context.Background(), m, meta)
	svc.OnCheck(context.Background(), m, meta)
	svc.OnCheck(context.Background(), m, meta)

	if notif.count() != 1 {
		t.Fatalf("dispatch count = %d, want 1 (once-per-threshold)", notif.count())
	}
}

func TestCertificateAlert_RestartUsesPersistedState(t *testing.T) {
	tls := newCertFakeTLSRepo()
	notif1 := &certFakeNotifier{}
	svc1 := NewCertificateAlertService(tls, notif1, &certFakeMaint{})
	notAfter := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)
	m := testMonitor(true)
	meta := certMeta(20, notAfter, "CN=Test")

	svc1.OnCheck(context.Background(), m, meta)
	if notif1.count() != 1 {
		t.Fatalf("first service count = %d, want 1", notif1.count())
	}

	// Simulate worker restart: new service, same persisted TLS row.
	notif2 := &certFakeNotifier{}
	svc2 := NewCertificateAlertService(tls, notif2, &certFakeMaint{})
	svc2.OnCheck(context.Background(), m, meta)
	if notif2.count() != 0 {
		t.Fatalf("restart service re-sent: count = %d, want 0", notif2.count())
	}
}

func TestCertificateAlert_RenewalResets(t *testing.T) {
	tls := newCertFakeTLSRepo()
	notif := &certFakeNotifier{}
	svc := NewCertificateAlertService(tls, notif, &certFakeMaint{})
	m := testMonitor(true)
	oldCert := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	newCert := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)

	svc.OnCheck(context.Background(), m, certMeta(20, oldCert, "CN=Old"))
	svc.OnCheck(context.Background(), m, certMeta(20, newCert, "CN=New"))

	if notif.count() != 2 {
		t.Fatalf("dispatch count = %d, want 2 (renewal resets)", notif.count())
	}
}

func TestCertificateAlert_DisabledToggle(t *testing.T) {
	tls := newCertFakeTLSRepo()
	notif := &certFakeNotifier{}
	svc := NewCertificateAlertService(tls, notif, &certFakeMaint{})
	notAfter := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)

	svc.OnCheck(context.Background(), testMonitor(false), certMeta(5, notAfter, "CN=Test"))
	if notif.count() != 0 {
		t.Fatalf("disabled toggle still dispatched: count = %d", notif.count())
	}
}

func TestCertificateAlert_NoTLSMetadata(t *testing.T) {
	tls := newCertFakeTLSRepo()
	notif := &certFakeNotifier{}
	svc := NewCertificateAlertService(tls, notif, &certFakeMaint{})

	svc.OnCheck(context.Background(), testMonitor(true), map[string]string{"other": "x"})
	svc.OnCheck(context.Background(), testMonitor(true), nil)
	if notif.count() != 0 {
		t.Fatalf("no-TLS still dispatched: count = %d", notif.count())
	}
}

func TestCertificateAlert_FirstObservationAt5SendsOnly7(t *testing.T) {
	tls := newCertFakeTLSRepo()
	notif := &certFakeNotifier{}
	svc := NewCertificateAlertService(tls, notif, &certFakeMaint{})
	notAfter := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)

	svc.OnCheck(context.Background(), testMonitor(true), certMeta(5, notAfter, "CN=Test"))
	if notif.count() != 1 {
		t.Fatalf("count = %d, want 1", notif.count())
	}
	if notif.last().CertThreshold != 7 {
		t.Errorf("threshold = %d, want 7", notif.last().CertThreshold)
	}
}

func TestCertificateAlert_MaintenanceSuppressesAndDoesNotMark(t *testing.T) {
	tls := newCertFakeTLSRepo()
	notif := &certFakeNotifier{}
	maint := &certFakeMaint{active: true}
	svc := NewCertificateAlertService(tls, notif, maint)
	notAfter := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)
	m := testMonitor(true)
	meta := certMeta(20, notAfter, "CN=Test")

	svc.OnCheck(context.Background(), m, meta)
	if notif.count() != 0 {
		t.Fatalf("maintenance still dispatched: count = %d", notif.count())
	}
	// No threshold marked — release maintenance and the next check should fire.
	maint.active = false
	svc.OnCheck(context.Background(), m, meta)
	if notif.count() != 1 {
		t.Fatalf("after maintenance release count = %d, want 1", notif.count())
	}
}

func TestCertificateAlert_ProviderFailureDoesNotMarkSent(t *testing.T) {
	tls := newCertFakeTLSRepo()
	notif := &certFakeNotifier{err: errors.New("smtp down")}
	svc := NewCertificateAlertService(tls, notif, &certFakeMaint{})
	notAfter := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)
	m := testMonitor(true)
	meta := certMeta(20, notAfter, "CN=Test")

	svc.OnCheck(context.Background(), m, meta)
	// Dispatch was attempted once; state must not be marked so a retry works.
	// Seed empty prior so Get returns not-found or zero threshold.
	if stored, err := tls.GetByMonitorID(context.Background(), 1); err == nil && stored.LastCertAlertThreshold != 0 {
		t.Fatalf("threshold marked after failure: %d", stored.LastCertAlertThreshold)
	}

	notif.err = nil
	svc.OnCheck(context.Background(), m, meta)
	if notif.count() != 2 {
		t.Fatalf("retry after failure count = %d, want 2 attempts", notif.count())
	}
}

func TestCertificateAlert_Above30NoAlert(t *testing.T) {
	tls := newCertFakeTLSRepo()
	notif := &certFakeNotifier{}
	svc := NewCertificateAlertService(tls, notif, &certFakeMaint{})
	notAfter := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)

	svc.OnCheck(context.Background(), testMonitor(true), certMeta(45, notAfter, "CN=Test"))
	if notif.count() != 0 {
		t.Fatalf("above-threshold alert count = %d, want 0", notif.count())
	}
}

func TestMostUrgentThreshold(t *testing.T) {
	cases := []struct {
		days int
		want int
		ok   bool
	}{
		{45, 0, false},
		{30, 30, true},
		{20, 30, true},
		{14, 14, true},
		{13, 14, true},
		{7, 7, true},
		{5, 7, true},
		{0, 7, true},
	}
	for _, tc := range cases {
		got, ok := mostUrgentThreshold(tc.days)
		if ok != tc.ok || got != tc.want {
			t.Errorf("mostUrgentThreshold(%d) = (%d, %v), want (%d, %v)", tc.days, got, ok, tc.want, tc.ok)
		}
	}
}

// Mutation-style check: once-per-threshold must break if the guard is removed.
func TestCertificateAlert_OncePerThresholdMutationGuard(t *testing.T) {
	tls := newCertFakeTLSRepo()
	notif := &certFakeNotifier{}
	svc := NewCertificateAlertService(tls, notif, &certFakeMaint{})
	notAfter := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)
	m := testMonitor(true)
	meta := certMeta(20, notAfter, "CN=Test")

	for i := 0; i < 5; i++ {
		svc.OnCheck(context.Background(), m, meta)
	}
	if notif.count() != 1 {
		t.Fatalf("once-per-threshold mutation: count = %d, want 1", notif.count())
	}
}
