package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

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

// fakeCLIReadiness is a controllable ports.HubWorkerReadiness double for the
// operator CLI gate.
type fakeCLIReadiness struct {
	unaware []string
	err     error
	calls   int
}

func (f *fakeCLIReadiness) DeclareWorker(context.Context, string, int, time.Duration) error {
	return nil
}

func (f *fakeCLIReadiness) UnawareWorkers(context.Context, int, time.Duration) ([]string, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.unaware, nil
}

// TestProbeAdminAssignGate proves the operator CLI's `assign` command is gated by
// the same T34 rule as the admin API. The CLI writes the desired set directly
// through the assignment store, so without this it would be an activation path
// the fleet guarantee does not cover — and it is precisely the tool an operator
// reaches for during a rollout.
func TestProbeAdminAssignGate(t *testing.T) {
	ctx := context.Background()
	cfg := Config{ShardLeaseTTL: 300}
	remote := []string{"9a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"}
	localOnly := []string{domain.LocalProbeID}

	t.Run("local only is never gated", func(t *testing.T) {
		f := &fakeCLIReadiness{unaware: []string{"worker-old"}}
		if msg := probeAdminAssignGate(ctx, f, cfg, localOnly); msg != "" {
			t.Fatalf("local-only assign refused: %q", msg)
		}
		if f.calls != 0 {
			t.Fatalf("local-only assign consulted readiness %d times", f.calls)
		}
	})

	t.Run("aware fleet is allowed", func(t *testing.T) {
		if msg := probeAdminAssignGate(ctx, &fakeCLIReadiness{}, cfg, remote); msg != "" {
			t.Fatalf("aware fleet refused: %q", msg)
		}
	})

	t.Run("unaware fleet is refused and names the worker", func(t *testing.T) {
		msg := probeAdminAssignGate(ctx, &fakeCLIReadiness{unaware: []string{"worker-old-7d9f"}}, cfg, remote)
		if msg == "" {
			t.Fatal("unaware fleet was allowed to activate remote monitoring")
		}
		if !strings.Contains(msg, "worker-old-7d9f") {
			t.Fatalf("CLI refusal must name the straggler for the operator: %q", msg)
		}
		if !strings.Contains(msg, "rollout") {
			t.Fatalf("CLI refusal must say what to do: %q", msg)
		}
	})

	// A CLI invoked on a hub whose readiness table cannot be read must not
	// conclude the fleet is fine. This also covers a nil store, which is what an
	// older or minimal composition produces.
	t.Run("unreadable readiness fails closed", func(t *testing.T) {
		msg := probeAdminAssignGate(ctx, &fakeCLIReadiness{err: errors.New("readiness table unreadable")}, cfg, remote)
		if msg == "" || !strings.Contains(msg, "readiness") {
			t.Fatalf("unreadable readiness allowed activation: %q", msg)
		}
		if msg := probeAdminAssignGate(ctx, nil, cfg, remote); msg == "" {
			t.Fatal("a nil readiness store allowed remote activation")
		}
		// Local-only still works with no store at all, so a probes-disabled
		// install keeps its CLI.
		if msg := probeAdminAssignGate(ctx, nil, cfg, localOnly); msg != "" {
			t.Fatalf("local-only assign refused with no readiness store: %q", msg)
		}
	})

	t.Run("a very short lease TTL cannot blind the gate", func(t *testing.T) {
		// fleetLeaseLookback floors the window; a sub-minute SHARD_LEASE_TTL must
		// not make a running unaware worker look dead.
		if got := fleetLeaseLookback(Config{ShardLeaseTTL: 1}); got != time.Minute {
			t.Fatalf("lookback for a 1s TTL = %v, want the 1m floor", got)
		}
		if got := fleetLeaseLookback(Config{ShardLeaseTTL: 900}); got != 15*time.Minute {
			t.Fatalf("lookback for a 900s TTL = %v, want 15m", got)
		}
		// 300 is the documented SHARD_LEASE_TTL default, applied by the env loader.
		if got := fleetLeaseLookback(Config{ShardLeaseTTL: 300}); got != 5*time.Minute {
			t.Fatalf("lookback for the 300s default TTL = %v, want 5m", got)
		}
		// A bare zero-value Config carries no TTL at all (envDefault is applied by
		// the loader, not by the struct), which must land on the floor rather than
		// produce a zero window that sees no live worker.
		if got := fleetLeaseLookback(Config{}); got != time.Minute {
			t.Fatalf("lookback for an unset TTL = %v, want the 1m floor", got)
		}
	})
}

// TestProbeAdminAssignConsultsTheFleetGate guards the CALL SITE, which the
// decision-logic test above cannot: deleting the gate invocation from the assign
// command would leave every other test green while silently reopening the
// activation path. It asserts the ordering in source, the same technique this
// repository already uses for route-registration guards, because driving
// `RunProbeAdmin assign` end to end needs a provisioned key file, an initialized
// installation and an enrolled probe.
//
// Order matters, not just presence: the gate must run BEFORE the write, or a
// refusal would arrive after the desired set had already changed.
func TestProbeAdminAssignConsultsTheFleetGate(t *testing.T) {
	source, err := os.ReadFile("probe_admin.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(source)
	start := strings.Index(body, `case "assign":`)
	if start < 0 {
		t.Fatal("the assign command disappeared from probe_admin.go")
	}
	end := strings.Index(body[start:], "\n\tcase \"")
	if end < 0 {
		end = len(body) - start
	}
	block := body[start : start+end]

	gate := strings.Index(block, "probeAdminAssignGate(")
	write := strings.Index(block, "ReplaceWithBindings(")
	if gate < 0 {
		t.Fatal("the assign command no longer consults probeAdminAssignGate; remote activation via the CLI is ungated")
	}
	if write < 0 {
		t.Fatal("the assign command no longer writes a desired set; this guard is stale, update it")
	}
	if gate > write {
		t.Fatal("the fleet gate runs after ReplaceWithBindings, so a refusal would arrive after the desired set already changed")
	}
	if !strings.Contains(block, "repos.hubWorkerReadiness") {
		t.Fatal("the assign gate is not wired to the readiness store")
	}
}
