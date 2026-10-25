package services

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const stagePrivateMarker = "sensitive-adapter-marker"

// stagePanicInspector is a typed-nil trap: any method call panics.
type stagePanicInspector struct{}

func (*stagePanicInspector) InspectBinaryStage(context.Context, domain.ProbeBinaryStageIntent) (domain.ProbeBinaryStageObservation, error) {
	panic("typed-nil inspector touched")
}

// stagePanicContext is a typed-nil context trap with the same guarantee.
type stagePanicContext struct{}

func (*stagePanicContext) Deadline() (time.Time, bool) { panic("typed-nil context touched") }
func (*stagePanicContext) Done() <-chan struct{}       { panic("typed-nil context touched") }
func (*stagePanicContext) Err() error                  { panic("typed-nil context touched") }
func (*stagePanicContext) Value(any) any               { panic("typed-nil context touched") }

// stageExpireContext reports a deadline only after the test arms it, without I/O.
type stageExpireContext struct {
	context.Context
	expired bool
}

func (c *stageExpireContext) Err() error {
	if c.expired {
		return fmt.Errorf("%s: %w", stagePrivateMarker, context.DeadlineExceeded)
	}
	return c.Context.Err()
}

type stageInspector struct {
	calls  int
	ctx    context.Context
	intent domain.ProbeBinaryStageIntent
	obs    domain.ProbeBinaryStageObservation
	err    error
	onCall func()
}

func (m *stageInspector) InspectBinaryStage(ctx context.Context, intent domain.ProbeBinaryStageIntent) (domain.ProbeBinaryStageObservation, error) {
	m.calls++
	m.ctx = ctx
	m.intent = intent
	if m.onCall != nil {
		m.onCall()
	}
	return m.obs, m.err
}

func stageFingerprint() string {
	return "SHA256:" + base64.RawStdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
}

func stageIntent() domain.ProbeBinaryStageIntent {
	return domain.ProbeBinaryStageIntent{
		Identity: domain.ProbeInstallIdentity{
			OperationID:          "11111111-1111-4111-8111-111111111111",
			Host:                 domain.ProbeSSHHost{Address: "edge.example:22", User: "root"},
			HostKeyFingerprint:   stageFingerprint(),
			ClientKeyFingerprint: stageFingerprint(),
			RequestedBy:          7,
			LayoutVersion:        domain.ProbeInstallLayoutVersion,
			Mode:                 domain.ProbeInstallSystemd,
			Binary: domain.ProbeBinaryPin{
				Version: "1.2.3", Architecture: "amd64", SHA256: strings.Repeat("ab", 32), Size: 100,
			},
		},
		OwnerID:   "22222222-2222-4222-8222-222222222222",
		AttemptID: "33333333-3333-4333-8333-333333333333",
	}
}

func stageDockerIntent() domain.ProbeBinaryStageIntent {
	intent := stageIntent()
	intent.Identity.Mode = domain.ProbeInstallDocker
	intent.Identity.Binary = domain.ProbeBinaryPin{}
	intent.Identity.Image = domain.ProbeImageSelection{
		Pin: domain.ProbeImagePin{
			Repository: "ghcr.io/example/probe", Version: "1.2.3", Architecture: "amd64",
			IndexSHA256: strings.Repeat("a", 64), IndexSize: 100,
		},
		ManifestSHA256: strings.Repeat("b", 64),
		ConfigSHA256:   strings.Repeat("c", 64),
		LayerCount:     1,
	}
	return intent
}

func stageObservation(intent domain.ProbeBinaryStageIntent, state string) domain.ProbeBinaryStageObservation {
	return domain.ProbeBinaryStageObservation{Intent: intent, State: state}
}

func assertStageZero(t *testing.T, got domain.ProbeBinaryStageObservation, err, want error) {
	t.Helper()
	if err != want || got != (domain.ProbeBinaryStageObservation{}) {
		t.Fatalf("got (%+v, %v), want zero and %v", got, err, want)
	}
	if err != nil && strings.Contains(err.Error(), stagePrivateMarker) {
		t.Fatal("private diagnostic escaped")
	}
}

func TestProbeBinaryStageInspectionServiceConstructor(t *testing.T) {
	if svc, err := NewProbeBinaryStageInspectionService(nil); svc != nil || err != domain.ErrProbeBinaryStageInput {
		t.Fatal("nil inspector accepted", svc, err)
	}
	var typedNil *stagePanicInspector
	if svc, err := NewProbeBinaryStageInspectionService(typedNil); svc != nil || err != domain.ErrProbeBinaryStageInput {
		t.Fatal("typed-nil inspector accepted", svc, err)
	}
	var typedIface ports.ProbeBinaryStageInspector = (*stagePanicInspector)(nil)
	if svc, err := NewProbeBinaryStageInspectionService(typedIface); svc != nil || err != domain.ErrProbeBinaryStageInput {
		t.Fatal("typed-nil inspector interface accepted", svc, err)
	}
	svc, err := NewProbeBinaryStageInspectionService(&stageInspector{})
	if err != nil || svc == nil {
		t.Fatal("valid inspector refused", err)
	}
}

func TestProbeBinaryStageInspectionServiceRefusesBeforeIO(t *testing.T) {
	intent := stageIntent()
	if !intent.Valid() || !stageDockerIntent().Identity.Valid() {
		t.Fatal("fixtures are not valid identities")
	}
	kinds := []string{
		"nil_service", "nil_inspector", "typed_nil_inspector", "nil_context", "typed_nil_context",
		"docker_intent", "missing_owner", "missing_attempt", "bad_attempt", "already_canceled",
		"already_expired", "wrapped_expired", "invalid_and_canceled",
	}
	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			adapter := &stageInspector{obs: stageObservation(intent, domain.ProbeBinaryStageContentMatches)}
			svc, err := NewProbeBinaryStageInspectionService(adapter)
			if err != nil {
				t.Fatal(err)
			}
			callCtx, cancel := context.WithCancel(t.Context())
			defer cancel()
			request := intent
			target := svc
			want := domain.ErrProbeBinaryStageInput
			switch kind {
			case "nil_service":
				target = nil
			case "nil_inspector":
				target = &ProbeBinaryStageInspectionService{}
			case "typed_nil_inspector":
				target = &ProbeBinaryStageInspectionService{inspector: (*stagePanicInspector)(nil)}
			case "nil_context":
				callCtx = nil
			case "typed_nil_context":
				var nilCtx *stagePanicContext
				callCtx = nilCtx
			case "docker_intent":
				request = stageDockerIntent()
			case "missing_owner":
				request.OwnerID = ""
			case "missing_attempt":
				request.AttemptID = ""
			case "bad_attempt":
				request.AttemptID = strings.ToUpper("abcdefab-cdef-4abc-8abc-abcdefabcdef")
			case "already_canceled":
				cancel()
				want = context.Canceled
			case "already_expired":
				expired, stop := context.WithDeadline(t.Context(), time.Unix(1, 0))
				defer stop()
				callCtx = expired
				want = context.DeadlineExceeded
			case "wrapped_expired":
				callCtx = &stageExpireContext{Context: t.Context(), expired: true}
				want = context.DeadlineExceeded
			case "invalid_and_canceled":
				cancel()
				request.OwnerID = ""
			}
			got, err := target.Inspect(callCtx, request)
			assertStageZero(t, got, err, want)
			if adapter.calls != 0 {
				t.Fatal("refused request called inspector", adapter.calls)
			}
		})
	}
}

func TestProbeBinaryStageInspectionServiceObservesExactIntent(t *testing.T) {
	intent := stageIntent()
	for _, state := range []string{domain.ProbeBinaryStageAbsent, domain.ProbeBinaryStageConflict, domain.ProbeBinaryStageContentMatches} {
		t.Run(state, func(t *testing.T) {
			want := stageObservation(intent, state)
			adapter := &stageInspector{obs: want}
			svc, err := NewProbeBinaryStageInspectionService(adapter)
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			got, err := svc.Inspect(ctx, intent)
			if err != nil || got != want || adapter.calls != 1 || adapter.ctx != ctx || adapter.intent != intent {
				t.Fatalf("calls=%d err=%v got=%+v delegated=%+v", adapter.calls, err, got, adapter.intent)
			}
			adapter.obs.State = "mutated"
			adapter.obs.Intent.AttemptID = "44444444-4444-4444-8444-444444444444"
			if got != want {
				t.Fatal("returned observation aliased the adapter")
			}
		})
	}
}

func TestProbeBinaryStageInspectionServiceRejectsObservation(t *testing.T) {
	intent := stageIntent()
	foreign := intent
	foreign.AttemptID = "44444444-4444-4444-8444-444444444444"
	hidden := intent
	hidden.Identity.Image = stageDockerIntent().Identity.Image
	cases := []domain.ProbeBinaryStageObservation{
		{},
		{Intent: intent, State: "applied"},
		{Intent: intent, State: "ABSENT"},
		{Intent: foreign, State: domain.ProbeBinaryStageAbsent},
		{Intent: hidden, State: domain.ProbeBinaryStageContentMatches},
	}
	for i, obs := range cases {
		t.Run(fmt.Sprintf("case_%d", i), func(t *testing.T) {
			adapter := &stageInspector{obs: obs}
			svc, err := NewProbeBinaryStageInspectionService(adapter)
			if err != nil {
				t.Fatal(err)
			}
			got, err := svc.Inspect(t.Context(), intent)
			assertStageZero(t, got, err, domain.ErrProbeBinaryStageInspection)
			if adapter.calls != 1 || adapter.intent != intent {
				t.Fatalf("calls=%d delegated=%+v", adapter.calls, adapter.intent)
			}
		})
	}
}

func TestProbeBinaryStageInspectionServiceSanitizesPortErrors(t *testing.T) {
	intent := stageIntent()
	success := stageObservation(intent, domain.ProbeBinaryStageContentMatches)
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"bare_input", domain.ErrProbeBinaryStageInput, domain.ErrProbeBinaryStageInput},
		{"bare_inspection", domain.ErrProbeBinaryStageInspection, domain.ErrProbeBinaryStageInspection},
		{"bare_ssh_input", domain.ErrProbeSSHInput, domain.ErrProbeSSHInput},
		{"bare_trust", domain.ErrProbeSSHTrust, domain.ErrProbeSSHTrust},
		{"bare_destination", domain.ErrProbeSSHDestination, domain.ErrProbeSSHDestination},
		{"bare_connect", domain.ErrProbeSSHConnect, domain.ErrProbeSSHConnect},
		{"wrapped_input", fmt.Errorf("%s: %w", stagePrivateMarker, domain.ErrProbeBinaryStageInput), domain.ErrProbeBinaryStageInput},
		{"wrapped_inspection", fmt.Errorf("path /opt/secret: %w", domain.ErrProbeBinaryStageInspection), domain.ErrProbeBinaryStageInspection},
		{"wrapped_ssh_input", errors.Join(domain.ErrProbeSSHInput, errors.New(stagePrivateMarker)), domain.ErrProbeSSHInput},
		{"wrapped_trust", fmt.Errorf("host key %s: %w", stagePrivateMarker, domain.ErrProbeSSHTrust), domain.ErrProbeSSHTrust},
		{"wrapped_destination", fmt.Errorf("dial 10.1.2.3:22: %w", domain.ErrProbeSSHDestination), domain.ErrProbeSSHDestination},
		{"wrapped_connect", fmt.Errorf("auth %s: %w", stagePrivateMarker, domain.ErrProbeSSHConnect), domain.ErrProbeSSHConnect},
		{"wrapped_canceled", fmt.Errorf("%s: %w", stagePrivateMarker, context.Canceled), context.Canceled},
		{"wrapped_deadline", errors.Join(errors.New(stagePrivateMarker), context.DeadlineExceeded), context.DeadlineExceeded},
		{"discovery_not_allowlisted", domain.ErrProbeSSHDiscovery, domain.ErrProbeBinaryStageInspection},
		{"platform_not_allowlisted", domain.ErrProbeHostPlatform, domain.ErrProbeBinaryStageInspection},
		{"unknown", errors.New(stagePrivateMarker), domain.ErrProbeBinaryStageInspection},
		{"joined_unknown_and_discovery", errors.Join(domain.ErrProbeSSHDiscovery, errors.New(stagePrivateMarker)), domain.ErrProbeBinaryStageInspection},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter := &stageInspector{obs: success, err: tc.err}
			svc, err := NewProbeBinaryStageInspectionService(adapter)
			if err != nil {
				t.Fatal(err)
			}
			got, err := svc.Inspect(t.Context(), intent)
			assertStageZero(t, got, err, tc.want)
			if adapter.calls != 1 {
				t.Fatal("error path retried or skipped", adapter.calls)
			}
			if err.Error() != tc.want.Error() {
				t.Fatal("returned error is not the bare sentinel", err)
			}
		})
	}
}

func TestProbeBinaryStageInspectionServiceRevokesAfterReturn(t *testing.T) {
	intent := stageIntent()
	success := stageObservation(intent, domain.ProbeBinaryStageAbsent)
	t.Run("cancel_overrides_success", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		adapter := &stageInspector{obs: success, onCall: cancel}
		svc, err := NewProbeBinaryStageInspectionService(adapter)
		if err != nil {
			t.Fatal(err)
		}
		got, err := svc.Inspect(ctx, intent)
		assertStageZero(t, got, err, context.Canceled)
		if adapter.calls != 1 {
			t.Fatal(adapter.calls)
		}
	})
	t.Run("cancel_overrides_private_error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		adapter := &stageInspector{obs: success, err: errors.New(stagePrivateMarker), onCall: cancel}
		svc, err := NewProbeBinaryStageInspectionService(adapter)
		if err != nil {
			t.Fatal(err)
		}
		got, err := svc.Inspect(ctx, intent)
		assertStageZero(t, got, err, context.Canceled)
	})
	t.Run("deadline_overrides_success", func(t *testing.T) {
		base := &stageExpireContext{Context: t.Context()}
		adapter := &stageInspector{obs: success, onCall: func() { base.expired = true }}
		svc, err := NewProbeBinaryStageInspectionService(adapter)
		if err != nil {
			t.Fatal(err)
		}
		got, err := svc.Inspect(base, intent)
		assertStageZero(t, got, err, context.DeadlineExceeded)
		if adapter.calls != 1 || adapter.ctx != context.Context(base) {
			t.Fatal("service wrapped or skipped the caller context", adapter.calls)
		}
	})
}
