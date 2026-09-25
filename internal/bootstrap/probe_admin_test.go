package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func TestProbeAdminUnknownCommandCannotReportSuccess(t *testing.T) {
	for _, command := range []string{"", "register|enroll", "future", "status|"} {
		var out, stderr bytes.Buffer
		if code := RunProbeAdmin(t.Context(), Config{}, []string{command}, &out, &stderr); code != 2 || out.Len() != 0 {
			t.Fatalf("unknown command %q reported success or produced a receipt", command)
		}
	}
}

func TestProbeRuntimeOptInRequiresKey(t *testing.T) {
	if err := Run(Config{ProbesEnabled: true, JWTExpireH: 24}); err == nil {
		t.Fatal("remote runtime started without protected installation authority")
	}
}

func TestProbeAdminWatchdogSettingsRejectTruncationAndAmbiguity(t *testing.T) {
	for _, test := range []struct {
		lost, recovery, resend int64
		channels               string
	}{
		{1<<32 + 90, 30, 0, ""}, {90, -1, 0, ""}, {90, 30, 1 << 32, ""}, {90, 30, 153722868, ""},
		{90, 30, 0, "1,1"}, {90, 30, 0, "01"}, {90, 30, 0, "1,"}, {90, 30, 0, "-1"},
	} {
		if _, err := probeAdminWatchdogSettings(true, test.lost, test.recovery, test.resend, test.channels); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("bad settings accepted: %+v %v", test, err)
		}
	}
	s, err := probeAdminWatchdogSettings(true, 90, 30, 5, "2,1")
	if err != nil || !s.Enabled || s.ResendInterval != 5 || len(s.NotificationIDs) != 2 {
		t.Fatal("valid complete settings rejected", err)
	}
}

// --- probe registration fake ------------------------------------------------

type fakeProbeRegistry struct {
	byID      map[string]*domain.Probe
	updateErr error
	creates   int
}

func newFakeProbeRegistry() *fakeProbeRegistry {
	return &fakeProbeRegistry{byID: map[string]*domain.Probe{}}
}

func (r *fakeProbeRegistry) Create(_ context.Context, p *domain.Probe) error {
	if p.ID == "" || p.Key == "" {
		return domain.ErrValidation
	}
	if _, ok := r.byID[p.ID]; ok {
		return ports.ErrConflict
	}
	cp := *p
	cp.Revision = 1
	r.byID[cp.ID] = &cp
	r.creates++
	*p = cp
	return nil
}

func (r *fakeProbeRegistry) GetByID(_ context.Context, id string) (*domain.Probe, error) {
	p, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *p
	return &cp, nil
}

func (r *fakeProbeRegistry) GetByKey(_ context.Context, key string) (*domain.Probe, error) {
	for _, p := range r.byID {
		if p.Key == key {
			cp := *p
			return &cp, nil
		}
	}
	return nil, ports.ErrNotFound
}

func (r *fakeProbeRegistry) List(context.Context) ([]domain.Probe, error) {
	out := make([]domain.Probe, 0, len(r.byID))
	for _, p := range r.byID {
		out = append(out, *p)
	}
	return out, nil
}

func (r *fakeProbeRegistry) Update(_ context.Context, p *domain.Probe, expectedRevision int64) error {
	if r.updateErr != nil {
		return r.updateErr
	}
	cur, ok := r.byID[p.ID]
	if !ok || cur.Revision != expectedRevision {
		return ports.ErrConflict
	}
	cp := *p
	cp.Revision = expectedRevision + 1
	r.byID[cp.ID] = &cp
	*p = cp
	return nil
}

func TestProbeAdminRegister_ReenrollsRestoredIdentity(t *testing.T) {
	const probeID = "9a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	ctx := context.Background()

	t.Run("creates and enables a new registration", func(t *testing.T) {
		registry := newFakeProbeRegistry()
		p, err := probeAdminRegistration(ctx, registry, probeID, "us-east", "US East", "ashburn")
		if err != nil || !p.Enabled || registry.creates != 1 {
			t.Fatalf("register: %+v %v", p, err)
		}
	})

	t.Run("adopts and re-enables a restored disabled identity", func(t *testing.T) {
		registry := newFakeProbeRegistry()
		restored := &domain.Probe{ID: probeID, Key: "us-east", Name: "US East", Location: "ashburn", Kind: domain.ProbeKindRemote, Enabled: false}
		if err := registry.Create(ctx, restored); err != nil {
			t.Fatal(err)
		}
		registry.creates = 0 // seeding is not the operator's registration
		p, err := probeAdminRegistration(ctx, registry, probeID, "us-east", "US East", "ashburn")
		if err != nil {
			t.Fatalf("reenroll: %v", err)
		}
		if !p.Enabled || p.Revision != 2 || registry.creates != 0 {
			t.Fatalf("restored identity not adopted: %+v creates=%d", p, registry.creates)
		}
	})

	t.Run("keeps an enabled registration untouched", func(t *testing.T) {
		registry := newFakeProbeRegistry()
		_ = registry.Create(ctx, &domain.Probe{ID: probeID, Key: "us-east", Name: "US East", Location: "ashburn", Kind: domain.ProbeKindRemote, Enabled: true})
		p, err := probeAdminRegistration(ctx, registry, probeID, "us-east", "US East", "ashburn")
		if err != nil || p.Revision != 1 {
			t.Fatalf("unchanged registration was rewritten: %+v %v", p, err)
		}
	})

	t.Run("refuses an identity mismatch", func(t *testing.T) {
		registry := newFakeProbeRegistry()
		_ = registry.Create(ctx, &domain.Probe{ID: probeID, Key: "us-east", Name: "US East", Location: "ashburn", Kind: domain.ProbeKindRemote, Enabled: true})
		for _, tc := range []struct{ key, name, location string }{
			{"eu-west", "US East", "ashburn"},
			{"us-east", "Other", "ashburn"},
			{"us-east", "US East", "frankfurt"},
		} {
			if _, err := probeAdminRegistration(ctx, registry, probeID, tc.key, tc.name, tc.location); err == nil {
				t.Fatalf("mismatch %+v accepted", tc)
			}
		}
	})

	t.Run("reports a failed re-enable", func(t *testing.T) {
		registry := newFakeProbeRegistry()
		_ = registry.Create(ctx, &domain.Probe{ID: probeID, Key: "us-east", Name: "US East", Location: "ashburn", Kind: domain.ProbeKindRemote, Enabled: false})
		registry.updateErr = errors.New("storage unavailable")
		if _, err := probeAdminRegistration(ctx, registry, probeID, "us-east", "US East", "ashburn"); err == nil || !strings.Contains(err.Error(), "re-enabled") {
			t.Fatalf("failed adoption not reported: %v", err)
		}
	})
}
