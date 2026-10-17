package services

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const (
	installJournalOpID       = "11111111-1111-4111-8111-111111111111"
	installJournalOpIDTwo    = "22222222-2222-4222-8222-222222222222"
	installJournalOwner      = "33333333-3333-4333-8333-333333333333"
	installJournalOwnerTwo   = "44444444-4444-4444-8444-444444444444"
	installJournalAttempt    = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	installJournalAttemptTwo = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

var errInstallJournalUnexpected = errors.New("unexpected install journal port call")

// installJournalLeakMarker stands in for private remote diagnostics a raw
// repository error might carry; it is composed so no fixture resembles a real
// credential. The facade must never echo repository text.
var installJournalLeakMarker = strings.Join([]string{"raw", "remote", "output", "hunter2", "marker"}, "-")

// installJournalBaseTime is microsecond-precision UTC like the stored stamps.
func installJournalBaseTime() time.Time {
	return time.Date(2026, 3, 4, 5, 6, 7, 123456000, time.UTC)
}

func installPreflightFingerprint(fill byte) string {
	value := make([]byte, 32)
	for i := range value {
		value[i] = fill
	}
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(value)
}

func installJournalIdentity(mode, operationID string) domain.ProbeInstallIdentity {
	identity := domain.ProbeInstallIdentity{
		OperationID:          operationID,
		Host:                 domain.ProbeSSHHost{Address: "edge.example:22", User: "root"},
		HostKeyFingerprint:   installPreflightFingerprint(0x01),
		ClientKeyFingerprint: installPreflightFingerprint(0x02),
		RequestedBy:          7,
		LayoutVersion:        domain.ProbeInstallLayoutVersion,
		Mode:                 mode,
	}
	if mode == domain.ProbeInstallSystemd {
		identity.Binary = domain.ProbeBinaryPin{Version: "1.2.3", Architecture: "amd64", SHA256: strings.Repeat("a", 64), Size: 100}
	} else {
		identity.Image = domain.ProbeImageSelection{
			Pin:            domain.ProbeImagePin{Repository: "ghcr.io/example/probe", Version: "1.2.3", Architecture: "amd64", IndexSHA256: strings.Repeat("a", 64), IndexSize: 100},
			ManifestSHA256: strings.Repeat("b", 64),
			ConfigSHA256:   strings.Repeat("c", 64),
			LayerCount:     1,
		}
	}
	return identity
}

// installJournalQueued builds the admitted, complete planned graph.
func installJournalQueued(identity domain.ProbeInstallIdentity, ownerID string) *domain.ProbeInstallOperation {
	created := installJournalBaseTime()
	operation := &domain.ProbeInstallOperation{
		Identity:   identity,
		OwnerID:    ownerID,
		State:      domain.ProbeInstallQueued,
		CreatedAt:  created,
		UpdatedAt:  created,
		DeadlineAt: created.Add(domain.ProbeInstallLifetime),
	}
	specs := identity.Resources()
	operation.Resources = make([]domain.ProbeInstallResource, len(specs))
	for i, spec := range specs {
		operation.Resources[i] = domain.ProbeInstallResource{Spec: spec, State: domain.ProbeInstallResourcePlanned}
	}
	return operation
}

// installJournalResource mutates one resource by canonical kind.
func installJournalResource(operation *domain.ProbeInstallOperation, kind string) *domain.ProbeInstallResource {
	for i := range operation.Resources {
		if operation.Resources[i].Spec.Kind == kind {
			return &operation.Resources[i]
		}
	}
	return nil
}

// installJournalApplyPrefix commits every resource before kind as applied, as
// the frozen graph requires of a reserve's predecessors.
func installJournalApplyPrefix(operation *domain.ProbeInstallOperation, kind string) {
	for i := range operation.Resources {
		if operation.Resources[i].Spec.Kind == kind {
			return
		}
		resource := &operation.Resources[i]
		resource.State = domain.ProbeInstallResourceApplied
		resource.AttemptID = fmt.Sprintf("cccccccc-cccc-4ccc-8ccc-cccccccccc%02x", i)
		resource.StartedAt = installJournalBaseTime().Add(time.Duration(2*i+1) * time.Second)
		resource.FinishedAt = installJournalBaseTime().Add(time.Duration(2*i+2) * time.Second)
	}
}

// installJournalInFlight marks the data directory as the running frontier.
func installJournalInFlight(operation *domain.ProbeInstallOperation, attemptID string) *domain.ProbeInstallOperation {
	installJournalApplyPrefix(operation, "data_directory")
	operation.State = domain.ProbeInstallRunning
	operation.UpdatedAt = installJournalBaseTime().Add(time.Minute)
	resource := installJournalResource(operation, "data_directory")
	resource.State = domain.ProbeInstallResourceInFlight
	resource.AttemptID = attemptID
	resource.StartedAt = installJournalBaseTime().Add(10 * time.Second)
	return operation
}

// installJournalApplied marks the data directory as committed applied history.
func installJournalApplied(operation *domain.ProbeInstallOperation, attemptID string) *domain.ProbeInstallOperation {
	installJournalApplyPrefix(operation, "data_directory")
	operation.State = domain.ProbeInstallRunning
	operation.UpdatedAt = installJournalBaseTime().Add(12 * time.Second)
	resource := installJournalResource(operation, "data_directory")
	resource.State = domain.ProbeInstallResourceApplied
	resource.AttemptID = attemptID
	resource.StartedAt = installJournalBaseTime().Add(10 * time.Second)
	resource.FinishedAt = installJournalBaseTime().Add(11 * time.Second)
	return operation
}

// installJournalOpRepoMock is a scripted ports.ProbeInstallOperationRepository.
type installJournalOpRepoMock struct {
	mu      sync.Mutex
	calls   struct{ create, observe, begin, cancel, finish int }
	create  func(ctx context.Context, request domain.ProbeInstallIdentity, ownerID string) (*domain.ProbeInstallOperation, bool, error)
	observe func(ctx context.Context, operationID string) (*domain.ProbeInstallOperation, error)
	begin   func(ctx context.Context, operationID, ownerID string) (*domain.ProbeInstallOperation, error)
	cancel  func(ctx context.Context, operationID string) (*domain.ProbeInstallOperation, error)
	finish  func(ctx context.Context, operationID, ownerID string, outcome domain.ProbeInstallOutcome) (*domain.ProbeInstallOperation, error)
}

func (m *installJournalOpRepoMock) total() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls.create + m.calls.observe + m.calls.begin + m.calls.cancel + m.calls.finish
}

func (m *installJournalOpRepoMock) CreateInstall(ctx context.Context, request domain.ProbeInstallIdentity, ownerID string) (*domain.ProbeInstallOperation, bool, error) {
	m.mu.Lock()
	m.calls.create++
	hook := m.create
	m.mu.Unlock()
	if hook == nil {
		return nil, false, errInstallJournalUnexpected
	}
	return hook(ctx, request, ownerID)
}

func (m *installJournalOpRepoMock) ObserveInstall(ctx context.Context, operationID string) (*domain.ProbeInstallOperation, error) {
	m.mu.Lock()
	m.calls.observe++
	hook := m.observe
	m.mu.Unlock()
	if hook == nil {
		return nil, errInstallJournalUnexpected
	}
	return hook(ctx, operationID)
}

func (m *installJournalOpRepoMock) BeginInstall(ctx context.Context, operationID, ownerID string) (*domain.ProbeInstallOperation, error) {
	m.mu.Lock()
	m.calls.begin++
	hook := m.begin
	m.mu.Unlock()
	if hook == nil {
		return nil, errInstallJournalUnexpected
	}
	return hook(ctx, operationID, ownerID)
}

func (m *installJournalOpRepoMock) RequestInstallCancellation(ctx context.Context, operationID string) (*domain.ProbeInstallOperation, error) {
	m.mu.Lock()
	m.calls.cancel++
	hook := m.cancel
	m.mu.Unlock()
	if hook == nil {
		return nil, errInstallJournalUnexpected
	}
	return hook(ctx, operationID)
}

func (m *installJournalOpRepoMock) FinishInstall(ctx context.Context, operationID, ownerID string, outcome domain.ProbeInstallOutcome) (*domain.ProbeInstallOperation, error) {
	m.mu.Lock()
	m.calls.finish++
	hook := m.finish
	m.mu.Unlock()
	if hook == nil {
		return nil, errInstallJournalUnexpected
	}
	return hook(ctx, operationID, ownerID, outcome)
}

// installJournalResourceMock is a scripted ports.ProbeInstallResourceJournal.
type installJournalResourceMock struct {
	mu      sync.Mutex
	calls   struct{ reserve, record int }
	reserve func(ctx context.Context, operationID, ownerID, kind, attemptID string) (*domain.ProbeInstallOperation, bool, error)
	record  func(ctx context.Context, operationID, ownerID, kind, attemptID string, outcome domain.ProbeInstallResourceOutcome) (*domain.ProbeInstallOperation, error)
}

func (m *installJournalResourceMock) total() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls.reserve + m.calls.record
}

func (m *installJournalResourceMock) ReserveInstallResource(ctx context.Context, operationID, ownerID, kind, attemptID string) (*domain.ProbeInstallOperation, bool, error) {
	m.mu.Lock()
	m.calls.reserve++
	hook := m.reserve
	m.mu.Unlock()
	if hook == nil {
		return nil, false, errInstallJournalUnexpected
	}
	return hook(ctx, operationID, ownerID, kind, attemptID)
}

func (m *installJournalResourceMock) RecordInstallResource(ctx context.Context, operationID, ownerID, kind, attemptID string, outcome domain.ProbeInstallResourceOutcome) (*domain.ProbeInstallOperation, error) {
	m.mu.Lock()
	m.calls.record++
	hook := m.record
	m.mu.Unlock()
	if hook == nil {
		return nil, errInstallJournalUnexpected
	}
	return hook(ctx, operationID, ownerID, kind, attemptID, outcome)
}

func installJournalService(t *testing.T) (*ProbeInstallJournalService, *installJournalOpRepoMock, *installJournalResourceMock) {
	t.Helper()
	operations := &installJournalOpRepoMock{}
	resources := &installJournalResourceMock{}
	service, err := NewProbeInstallJournalService(operations, resources)
	if err != nil || service == nil {
		t.Fatalf("constructor failed: %v", err)
	}
	return service, operations, resources
}

func TestProbeInstallJournalConstruction(t *testing.T) {
	if _, err := NewProbeInstallJournalService(nil, &installJournalResourceMock{}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("nil operations accepted: %v", err)
	}
	if _, err := NewProbeInstallJournalService(&installJournalOpRepoMock{}, nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("nil resources accepted: %v", err)
	}
	if service, err := NewProbeInstallJournalService(&installJournalOpRepoMock{}, &installJournalResourceMock{}); err != nil || service == nil {
		t.Fatalf("valid constructor refused: %v", err)
	}
}

func TestProbeInstallJournalCreateAdmission(t *testing.T) {
	for _, mode := range []string{domain.ProbeInstallSystemd, domain.ProbeInstallDocker} {
		t.Run(mode, func(t *testing.T) {
			identity := installJournalIdentity(mode, installJournalOpID)
			t.Run("created matches admitted owner and queued state", func(t *testing.T) {
				service, operations, _ := installJournalService(t)
				operations.create = func(_ context.Context, request domain.ProbeInstallIdentity, ownerID string) (*domain.ProbeInstallOperation, bool, error) {
					return installJournalQueued(request, ownerID), true, nil
				}
				operation, created, err := service.Create(context.Background(), identity, installJournalOwner)
				if err != nil || !created || operation == nil {
					t.Fatalf("create failed: created=%v err=%v", created, err)
				}
				if operation.OwnerID != installJournalOwner || operation.State != domain.ProbeInstallQueued || !operation.Valid() {
					t.Fatalf("admitted receipt malformed: %+v", operation)
				}
			})
			t.Run("created=false preserves the original owner", func(t *testing.T) {
				service, operations, _ := installJournalService(t)
				operations.create = func(_ context.Context, request domain.ProbeInstallIdentity, _ string) (*domain.ProbeInstallOperation, bool, error) {
					return installJournalQueued(request, installJournalOwnerTwo), false, nil
				}
				operation, created, err := service.Create(context.Background(), identity, installJournalOwner)
				if err != nil || created || operation == nil {
					t.Fatalf("exact retry failed: created=%v err=%v", created, err)
				}
				if operation.OwnerID != installJournalOwnerTwo {
					t.Fatalf("retry rebound owner to %q", operation.OwnerID)
				}
			})
			t.Run("malformed admission is never a grant", func(t *testing.T) {
				for name, hook := range map[string]func(context.Context, domain.ProbeInstallIdentity, string) (*domain.ProbeInstallOperation, bool, error){
					"nil receipt": func(context.Context, domain.ProbeInstallIdentity, string) (*domain.ProbeInstallOperation, bool, error) {
						return nil, true, nil
					},
					"corrupt graph": func(_ context.Context, request domain.ProbeInstallIdentity, ownerID string) (*domain.ProbeInstallOperation, bool, error) {
						operation := installJournalQueued(request, ownerID)
						operation.Resources = operation.Resources[:1]
						return operation, true, nil
					},
					"identity mismatch": func(_ context.Context, request domain.ProbeInstallIdentity, ownerID string) (*domain.ProbeInstallOperation, bool, error) {
						other := request
						other.OperationID = installJournalOpIDTwo
						return installJournalQueued(other, ownerID), true, nil
					},
					"created under a foreign owner": func(_ context.Context, request domain.ProbeInstallIdentity, _ string) (*domain.ProbeInstallOperation, bool, error) {
						return installJournalQueued(request, installJournalOwnerTwo), true, nil
					},
					"created past the queued state": func(_ context.Context, request domain.ProbeInstallIdentity, ownerID string) (*domain.ProbeInstallOperation, bool, error) {
						return installJournalInFlight(installJournalQueued(request, ownerID), installJournalAttempt), true, nil
					},
				} {
					service, operations, _ := installJournalService(t)
					operations.create = hook
					operation, created, err := service.Create(context.Background(), identity, installJournalOwner)
					if !errors.Is(err, domain.ErrInternal) || created || operation != nil {
						t.Fatalf("%s: got op=%v created=%v err=%v", name, operation != nil, created, err)
					}
				}
			})
		})
	}
	t.Run("invalid input never reaches the repository", func(t *testing.T) {
		service, operations, _ := installJournalService(t)
		bad := installJournalIdentity(domain.ProbeInstallSystemd, installJournalOpID)
		bad.Mode = "podman"
		if _, _, err := service.Create(context.Background(), bad, installJournalOwner); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("invalid request accepted: %v", err)
		}
		if _, _, err := service.Create(context.Background(), installJournalIdentity(domain.ProbeInstallSystemd, installJournalOpID), "not-a-uuid"); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("invalid owner accepted: %v", err)
		}
		if _, _, err := service.Create(nil, installJournalIdentity(domain.ProbeInstallSystemd, installJournalOpID), installJournalOwner); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("nil context accepted: %v", err)
		}
		if operations.total() != 0 {
			t.Fatal("invalid input reached the repository")
		}
	})
}

func TestProbeInstallJournalReserveGrant(t *testing.T) {
	for _, mode := range []string{domain.ProbeInstallSystemd, domain.ProbeInstallDocker} {
		t.Run(mode, func(t *testing.T) {
			identity := installJournalIdentity(mode, installJournalOpID)
			t.Run("created=true is exactly one in-flight grant on the running owner", func(t *testing.T) {
				service, _, resources := installJournalService(t)
				resources.reserve = func(_ context.Context, _ string, ownerID, _ string, attemptID string) (*domain.ProbeInstallOperation, bool, error) {
					return installJournalInFlight(installJournalQueued(identity, ownerID), attemptID), true, nil
				}
				operation, created, err := service.ReserveResource(context.Background(), installJournalOpID, installJournalOwner, "data_directory", installJournalAttempt)
				if err != nil || !created || operation == nil {
					t.Fatalf("reserve failed: created=%v err=%v", created, err)
				}
				resource := installJournalResource(operation, "data_directory")
				if operation.State != domain.ProbeInstallRunning || resource.State != domain.ProbeInstallResourceInFlight || resource.AttemptID != installJournalAttempt {
					t.Fatalf("grant receipt mismatch: %+v", resource)
				}
			})
			t.Run("created=false never grants, even in flight", func(t *testing.T) {
				service, _, resources := installJournalService(t)
				resources.reserve = func(_ context.Context, _ string, ownerID, _ string, attemptID string) (*domain.ProbeInstallOperation, bool, error) {
					return installJournalInFlight(installJournalQueued(identity, ownerID), attemptID), false, nil
				}
				operation, created, err := service.ReserveResource(context.Background(), installJournalOpID, installJournalOwner, "data_directory", installJournalAttempt)
				if err != nil || created || operation == nil {
					t.Fatalf("historical retry granted: created=%v err=%v", created, err)
				}
			})
			t.Run("created=false keeps immutable applied history", func(t *testing.T) {
				service, _, resources := installJournalService(t)
				resources.reserve = func(_ context.Context, _ string, ownerID, _ string, attemptID string) (*domain.ProbeInstallOperation, bool, error) {
					return installJournalApplied(installJournalQueued(identity, ownerID), attemptID), false, nil
				}
				operation, created, err := service.ReserveResource(context.Background(), installJournalOpID, installJournalOwner, "data_directory", installJournalAttempt)
				if err != nil || created || operation == nil {
					t.Fatalf("applied retry rejected: created=%v err=%v", created, err)
				}
				if resource := installJournalResource(operation, "data_directory"); resource.State != domain.ProbeInstallResourceApplied {
					t.Fatalf("history mutated: %+v", resource)
				}
			})
			t.Run("malformed grants are refused", func(t *testing.T) {
				for name, hook := range map[string]func(context.Context, string, string, string, string) (*domain.ProbeInstallOperation, bool, error){
					"grant for an applied resource": func(_ context.Context, _ string, ownerID, _ string, attemptID string) (*domain.ProbeInstallOperation, bool, error) {
						return installJournalApplied(installJournalQueued(identity, ownerID), attemptID), true, nil
					},
					"grant outside the running state": func(_ context.Context, _ string, ownerID, _ string, attemptID string) (*domain.ProbeInstallOperation, bool, error) {
						operation := installJournalInFlight(installJournalQueued(identity, ownerID), attemptID)
						operation.State = domain.ProbeInstallCancelRequested
						return operation, true, nil
					},
					"grant under a foreign owner": func(_ context.Context, _ string, _ string, _ string, attemptID string) (*domain.ProbeInstallOperation, bool, error) {
						return installJournalInFlight(installJournalQueued(identity, installJournalOwnerTwo), attemptID), true, nil
					},
					"receipt with a different attempt": func(_ context.Context, _ string, ownerID, _ string, _ string) (*domain.ProbeInstallOperation, bool, error) {
						return installJournalInFlight(installJournalQueued(identity, ownerID), installJournalAttemptTwo), true, nil
					},
					"receipt without the resource": func(_ context.Context, _ string, ownerID, _ string, attemptID string) (*domain.ProbeInstallOperation, bool, error) {
						operation := installJournalInFlight(installJournalQueued(identity, ownerID), attemptID)
						installJournalResource(operation, "data_directory").Spec.Kind = "unit_file"
						return operation, true, nil
					},
					"corrupt receipt": func(_ context.Context, _ string, ownerID, _ string, attemptID string) (*domain.ProbeInstallOperation, bool, error) {
						operation := installJournalInFlight(installJournalQueued(identity, ownerID), attemptID)
						operation.Resources = nil
						return operation, true, nil
					},
				} {
					service, _, resources := installJournalService(t)
					resources.reserve = hook
					operation, created, err := service.ReserveResource(context.Background(), installJournalOpID, installJournalOwner, "data_directory", installJournalAttempt)
					if !errors.Is(err, domain.ErrInternal) || created || operation != nil {
						t.Fatalf("%s: got op=%v created=%v err=%v", name, operation != nil, created, err)
					}
				}
			})
		})
	}
	t.Run("invalid reservation input never reaches the journal", func(t *testing.T) {
		service, _, resources := installJournalService(t)
		for _, args := range [][]string{
			{"not-a-uuid", installJournalOwner, "data_directory", installJournalAttempt},
			{installJournalOpID, "not-a-uuid", "data_directory", installJournalAttempt},
			{installJournalOpID, installJournalOwner, "tmp_dir", installJournalAttempt},
			{installJournalOpID, installJournalOwner, "data_directory", "not-a-uuid"},
		} {
			if _, _, err := service.ReserveResource(context.Background(), args[0], args[1], args[2], args[3]); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("invalid reservation %v accepted: %v", args, err)
			}
		}
		if resources.total() != 0 {
			t.Fatal("invalid reservation input reached the journal")
		}
	})
}

func TestProbeInstallJournalRecordOutcome(t *testing.T) {
	identity := installJournalIdentity(domain.ProbeInstallSystemd, installJournalOpID)
	for _, tc := range []struct {
		name    string
		outcome domain.ProbeInstallResourceOutcome
		stored  func(*domain.ProbeInstallOperation)
	}{
		{
			name:    "applied report",
			outcome: domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied},
			stored: func(operation *domain.ProbeInstallOperation) {
				resource := installJournalResource(operation, "data_directory")
				resource.State = domain.ProbeInstallResourceApplied
				resource.FinishedAt = installJournalBaseTime().Add(11 * time.Second)
			},
		},
		{
			name:    "not_started report",
			outcome: domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceNotStarted, ErrorCode: "not_started"},
			stored: func(operation *domain.ProbeInstallOperation) {
				resource := installJournalResource(operation, "data_directory")
				resource.State = domain.ProbeInstallResourceNotStarted
				resource.ErrorCode = "not_started"
				resource.FinishedAt = installJournalBaseTime().Add(11 * time.Second)
			},
		},
		{
			name:    "unknown report interrupts",
			outcome: domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceUnknown, ErrorCode: "outcome_unknown"},
			stored: func(operation *domain.ProbeInstallOperation) {
				resource := installJournalResource(operation, "data_directory")
				resource.State = domain.ProbeInstallResourceUnknown
				resource.ErrorCode = "outcome_unknown"
				resource.FinishedAt = installJournalBaseTime().Add(11 * time.Second)
				operation.State = domain.ProbeInstallInterrupted
				operation.ErrorCode = "outcome_unknown"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, _, resources := installJournalService(t)
			resources.record = func(_ context.Context, _ string, ownerID, _ string, attemptID string, _ domain.ProbeInstallResourceOutcome) (*domain.ProbeInstallOperation, error) {
				operation := installJournalInFlight(installJournalQueued(identity, ownerID), attemptID)
				tc.stored(operation)
				return operation, nil
			}
			operation, err := service.RecordResource(context.Background(), installJournalOpID, installJournalOwner, "data_directory", installJournalAttempt, tc.outcome)
			if err != nil || operation == nil {
				t.Fatalf("record failed: %v", err)
			}
			resource := installJournalResource(operation, "data_directory")
			if resource.State != tc.outcome.State || resource.ErrorCode != tc.outcome.ErrorCode {
				t.Fatalf("recorded outcome = %+v, want %+v", *resource, tc.outcome)
			}
		})
	}
	t.Run("altered outcomes are refused", func(t *testing.T) {
		for name, hook := range map[string]func(context.Context, string, string, string, string, domain.ProbeInstallResourceOutcome) (*domain.ProbeInstallOperation, error){
			"stored outcome replaced": func(_ context.Context, _ string, ownerID, _ string, attemptID string, _ domain.ProbeInstallResourceOutcome) (*domain.ProbeInstallOperation, error) {
				operation := installJournalApplied(installJournalQueued(identity, ownerID), attemptID)
				resource := installJournalResource(operation, "data_directory")
				resource.State = domain.ProbeInstallResourceUnknown
				resource.ErrorCode = "outcome_unknown"
				operation.State = domain.ProbeInstallInterrupted
				operation.ErrorCode = "outcome_unknown"
				return operation, nil
			},
			"stored code disagrees": func(_ context.Context, _ string, ownerID, _ string, attemptID string, _ domain.ProbeInstallResourceOutcome) (*domain.ProbeInstallOperation, error) {
				operation := installJournalInFlight(installJournalQueued(identity, ownerID), attemptID)
				resource := installJournalResource(operation, "data_directory")
				resource.State = domain.ProbeInstallResourceNotStarted
				resource.ErrorCode = "not_started"
				resource.FinishedAt = installJournalBaseTime().Add(11 * time.Second)
				return operation, nil
			},
			"attempt replaced": func(_ context.Context, _ string, ownerID, _ string, _ string, _ domain.ProbeInstallResourceOutcome) (*domain.ProbeInstallOperation, error) {
				return installJournalInFlight(installJournalQueued(identity, ownerID), installJournalAttemptTwo), nil
			},
			"owner replaced": func(_ context.Context, _ string, _ string, _ string, attemptID string, _ domain.ProbeInstallResourceOutcome) (*domain.ProbeInstallOperation, error) {
				return installJournalInFlight(installJournalQueued(identity, installJournalOwnerTwo), attemptID), nil
			},
		} {
			service, _, resources := installJournalService(t)
			resources.record = hook
			operation, err := service.RecordResource(context.Background(), installJournalOpID, installJournalOwner, "data_directory", installJournalAttempt,
				domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied})
			if !errors.Is(err, domain.ErrInternal) || operation != nil {
				t.Fatalf("%s: got op=%v err=%v", name, operation != nil, err)
			}
		}
	})
	t.Run("invalid reports never reach the journal", func(t *testing.T) {
		service, _, resources := installJournalService(t)
		for _, outcome := range []domain.ProbeInstallResourceOutcome{
			{State: domain.ProbeInstallResourcePlanned},
			{State: domain.ProbeInstallResourceInFlight},
			{State: domain.ProbeInstallResourceApplied, ErrorCode: "internal"},
			{State: domain.ProbeInstallResourceUnknown, ErrorCode: "deadline_expired"},
			{State: domain.ProbeInstallResourceUnknown, ErrorCode: "raw remote stderr"},
		} {
			if _, err := service.RecordResource(context.Background(), installJournalOpID, installJournalOwner, "data_directory", installJournalAttempt, outcome); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("invalid report %+v accepted: %v", outcome, err)
			}
		}
		if _, err := service.RecordResource(context.Background(), installJournalOpID, installJournalOwner, "shim", installJournalAttempt,
			domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied}); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("noncanonical kind accepted: %v", err)
		}
		if resources.total() != 0 {
			t.Fatal("invalid report input reached the journal")
		}
	})
	t.Run("canonical kind absent from the graph is refused", func(t *testing.T) {
		service, _, resources := installJournalService(t)
		resources.record = func(_ context.Context, _ string, ownerID, _ string, attemptID string, _ domain.ProbeInstallResourceOutcome) (*domain.ProbeInstallOperation, error) {
			return installJournalInFlight(installJournalQueued(identity, ownerID), attemptID), nil
		}
		operation, err := service.RecordResource(context.Background(), installJournalOpID, installJournalOwner, "container", installJournalAttempt,
			domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied})
		if !errors.Is(err, domain.ErrInternal) || operation != nil {
			t.Fatalf("absent resource accepted: op=%v err=%v", operation != nil, err)
		}
	})
}

func TestProbeInstallJournalObserveBeginCancelFinish(t *testing.T) {
	identity := installJournalIdentity(domain.ProbeInstallDocker, installJournalOpID)
	t.Run("observe returns the complete valid graph", func(t *testing.T) {
		service, operations, _ := installJournalService(t)
		operations.observe = func(_ context.Context, _ string) (*domain.ProbeInstallOperation, error) {
			return installJournalQueued(identity, installJournalOwner), nil
		}
		operation, err := service.Observe(context.Background(), installJournalOpID)
		if err != nil || operation == nil || !operation.Valid() {
			t.Fatalf("observe failed: %v", err)
		}
	})
	t.Run("observe refuses malformed receipts", func(t *testing.T) {
		for name, hook := range map[string]func(context.Context, string) (*domain.ProbeInstallOperation, error){
			"nil receipt": func(context.Context, string) (*domain.ProbeInstallOperation, error) { return nil, nil },
			"foreign operation": func(_ context.Context, _ string) (*domain.ProbeInstallOperation, error) {
				return installJournalQueued(installJournalIdentity(domain.ProbeInstallDocker, installJournalOpIDTwo), installJournalOwner), nil
			},
			"corrupt graph": func(_ context.Context, _ string) (*domain.ProbeInstallOperation, error) {
				operation := installJournalQueued(identity, installJournalOwner)
				operation.Resources = nil
				return operation, nil
			},
		} {
			service, operations, _ := installJournalService(t)
			operations.observe = hook
			operation, err := service.Observe(context.Background(), installJournalOpID)
			if !errors.Is(err, domain.ErrInternal) || operation != nil {
				t.Fatalf("%s: got op=%v err=%v", name, operation != nil, err)
			}
		}
	})
	t.Run("begin is the single running dispatch grant", func(t *testing.T) {
		service, operations, _ := installJournalService(t)
		operations.begin = func(_ context.Context, _ string, ownerID string) (*domain.ProbeInstallOperation, error) {
			return installJournalInFlight(installJournalQueued(identity, ownerID), installJournalAttempt), nil
		}
		operation, err := service.Begin(context.Background(), installJournalOpID, installJournalOwner)
		if err != nil || operation == nil || operation.State != domain.ProbeInstallRunning || operation.OwnerID != installJournalOwner {
			t.Fatalf("begin failed: %v", err)
		}
		for name, hook := range map[string]func(context.Context, string, string) (*domain.ProbeInstallOperation, error){
			"begin left the operation queued": func(_ context.Context, _ string, ownerID string) (*domain.ProbeInstallOperation, error) {
				return installJournalQueued(identity, ownerID), nil
			},
			"begin under a foreign owner": func(_ context.Context, _ string, _ string) (*domain.ProbeInstallOperation, error) {
				return installJournalInFlight(installJournalQueued(identity, installJournalOwnerTwo), installJournalAttempt), nil
			},
		} {
			service, operations, _ := installJournalService(t)
			operations.begin = hook
			operation, err := service.Begin(context.Background(), installJournalOpID, installJournalOwner)
			if !errors.Is(err, domain.ErrInternal) || operation != nil {
				t.Fatalf("%s: got op=%v err=%v", name, operation != nil, err)
			}
		}
		operations.begin = func(context.Context, string, string) (*domain.ProbeInstallOperation, error) {
			return nil, ports.ErrConflict
		}
		if _, err := service.Begin(context.Background(), installJournalOpID, installJournalOwner); err != ports.ErrConflict {
			t.Fatalf("second begin conflict swallowed: %v", err)
		}
	})
	t.Run("cancel records intent or terminal, never active states", func(t *testing.T) {
		for _, state := range []string{domain.ProbeInstallCancelRequested, domain.ProbeInstallCanceled, domain.ProbeInstallFailed, domain.ProbeInstallInterrupted} {
			service, operations, _ := installJournalService(t)
			operations.cancel = func(_ context.Context, _ string) (*domain.ProbeInstallOperation, error) {
				operation := installJournalQueued(identity, installJournalOwner)
				operation.State = state
				if state == domain.ProbeInstallCanceled {
					operation.ErrorCode = "canceled"
				}
				if state == domain.ProbeInstallFailed {
					operation.ErrorCode = "execution_failed"
				}
				if state == domain.ProbeInstallInterrupted {
					operation.ErrorCode = "owner_interrupted"
					operation.UpdatedAt = installJournalBaseTime().Add(time.Minute)
				}
				return operation, nil
			}
			operation, err := service.Cancel(context.Background(), installJournalOpID)
			if err != nil || operation == nil || operation.State != state {
				t.Fatalf("cancel(%s) failed: %v", state, err)
			}
		}
		service, operations, _ := installJournalService(t)
		operations.cancel = func(_ context.Context, _ string) (*domain.ProbeInstallOperation, error) {
			operation := installJournalQueued(identity, installJournalOwner)
			operation.UpdatedAt = installJournalBaseTime().Add(time.Minute)
			return installJournalInFlight(operation, installJournalAttempt), nil
		}
		if operation, err := service.Cancel(context.Background(), installJournalOpID); !errors.Is(err, domain.ErrInternal) || operation != nil {
			t.Fatalf("cancellation left active work as a receipt: op=%v err=%v", operation != nil, err)
		}
	})
	t.Run("finish records exactly the requested terminal outcome", func(t *testing.T) {
		outcome := domain.ProbeInstallOutcome{State: domain.ProbeInstallFailed, ErrorCode: "execution_failed"}
		service, operations, _ := installJournalService(t)
		operations.finish = func(_ context.Context, _ string, ownerID string, got domain.ProbeInstallOutcome) (*domain.ProbeInstallOperation, error) {
			operation := installJournalQueued(identity, ownerID)
			operation.State = got.State
			operation.ErrorCode = got.ErrorCode
			operation.UpdatedAt = installJournalBaseTime().Add(time.Minute)
			return operation, nil
		}
		operation, err := service.Finish(context.Background(), installJournalOpID, installJournalOwner, outcome)
		if err != nil || operation == nil || !operation.Terminal() ||
			operation.State != outcome.State || operation.ErrorCode != outcome.ErrorCode {
			t.Fatalf("finish failed: %v", err)
		}
		for name, hook := range map[string]func(context.Context, string, string, domain.ProbeInstallOutcome) (*domain.ProbeInstallOperation, error){
			"outcome replaced": func(_ context.Context, _ string, ownerID string, _ domain.ProbeInstallOutcome) (*domain.ProbeInstallOperation, error) {
				operation := installJournalQueued(identity, ownerID)
				operation.State = domain.ProbeInstallFailed
				operation.ErrorCode = "permission_denied"
				operation.UpdatedAt = installJournalBaseTime().Add(time.Minute)
				return operation, nil
			},
			"non-terminal receipt": func(_ context.Context, _ string, ownerID string, _ domain.ProbeInstallOutcome) (*domain.ProbeInstallOperation, error) {
				return installJournalInFlight(installJournalQueued(identity, ownerID), installJournalAttempt), nil
			},
			"owner replaced": func(_ context.Context, _ string, _ string, got domain.ProbeInstallOutcome) (*domain.ProbeInstallOperation, error) {
				operation := installJournalQueued(identity, installJournalOwnerTwo)
				operation.State = got.State
				operation.ErrorCode = got.ErrorCode
				operation.UpdatedAt = installJournalBaseTime().Add(time.Minute)
				return operation, nil
			},
		} {
			service, operations, _ := installJournalService(t)
			operations.finish = hook
			operation, err := service.Finish(context.Background(), installJournalOpID, installJournalOwner, outcome)
			if !errors.Is(err, domain.ErrInternal) || operation != nil {
				t.Fatalf("%s: got op=%v err=%v", name, operation != nil, err)
			}
		}
	})
	t.Run("no success or installed outcome is ever admitted", func(t *testing.T) {
		service, operations, _ := installJournalService(t)
		for _, outcome := range []domain.ProbeInstallOutcome{
			{State: "installed"},
			{State: "succeeded", ErrorCode: "ok"},
			{State: domain.ProbeInstallResourcesReady},
			{State: domain.ProbeInstallFailed, ErrorCode: "installed"},
			{State: domain.ProbeInstallFailed, ErrorCode: "raw remote output"},
			{State: domain.ProbeInstallCanceled, ErrorCode: ""},
			{State: domain.ProbeInstallInterrupted, ErrorCode: "deadline_expired"},
		} {
			if _, err := service.Finish(context.Background(), installJournalOpID, installJournalOwner, outcome); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("outcome %+v admitted: %v", outcome, err)
			}
		}
		if operations.total() != 0 {
			t.Fatal("success-like outcome reached the repository")
		}
	})
	t.Run("invalid identifiers never reach the repository", func(t *testing.T) {
		service, operations, _ := installJournalService(t)
		if _, err := service.Observe(context.Background(), "not-a-uuid"); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("invalid observe id accepted")
		}
		if _, err := service.Begin(context.Background(), installJournalOpID, "not-a-uuid"); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("invalid begin owner accepted")
		}
		if _, err := service.Cancel(context.Background(), "not-a-uuid"); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("invalid cancel id accepted")
		}
		if _, err := service.Finish(context.Background(), installJournalOpID, installJournalOwner, domain.ProbeInstallOutcome{}); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("empty finish outcome accepted")
		}
		if operations.total() != 0 {
			t.Fatal("invalid identifiers reached the repository")
		}
	})
}

func TestProbeInstallJournalContextAndGrantRevocation(t *testing.T) {
	identity := installJournalIdentity(domain.ProbeInstallSystemd, installJournalOpID)
	t.Run("canceled contexts never reach the repository", func(t *testing.T) {
		service, operations, resources := installJournalService(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, _, err := service.Create(ctx, identity, installJournalOwner); !errors.Is(err, context.Canceled) {
			t.Fatalf("create: %v", err)
		}
		if _, err := service.Observe(ctx, installJournalOpID); !errors.Is(err, context.Canceled) {
			t.Fatalf("observe: %v", err)
		}
		if _, err := service.Begin(ctx, installJournalOpID, installJournalOwner); !errors.Is(err, context.Canceled) {
			t.Fatalf("begin: %v", err)
		}
		if _, err := service.Cancel(ctx, installJournalOpID); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
		if _, err := service.Finish(ctx, installJournalOpID, installJournalOwner, domain.ProbeInstallOutcome{State: domain.ProbeInstallCanceled, ErrorCode: "canceled"}); !errors.Is(err, context.Canceled) {
			t.Fatalf("finish: %v", err)
		}
		if _, _, err := service.ReserveResource(ctx, installJournalOpID, installJournalOwner, "data_directory", installJournalAttempt); !errors.Is(err, context.Canceled) {
			t.Fatalf("reserve: %v", err)
		}
		if _, err := service.RecordResource(ctx, installJournalOpID, installJournalOwner, "data_directory", installJournalAttempt, domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied}); !errors.Is(err, context.Canceled) {
			t.Fatalf("record: %v", err)
		}
		if operations.total() != 0 || resources.total() != 0 {
			t.Fatal("canceled context reached the repository")
		}
	})
	t.Run("post-commit cancellation never returns a fresh grant", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		service, operations, resources := installJournalService(t)
		operations.create = func(_ context.Context, request domain.ProbeInstallIdentity, ownerID string) (*domain.ProbeInstallOperation, bool, error) {
			cancel() // the acknowledgement races a caller cancellation
			return installJournalQueued(request, ownerID), true, nil
		}
		operation, created, err := service.Create(ctx, identity, installJournalOwner)
		if !errors.Is(err, context.Canceled) || created || operation != nil {
			t.Fatalf("create returned a grant after cancel: op=%v created=%v err=%v", operation != nil, created, err)
		}
		ctx, cancel = context.WithCancel(context.Background())
		defer cancel()
		resources.reserve = func(_ context.Context, _ string, ownerID, _ string, attemptID string) (*domain.ProbeInstallOperation, bool, error) {
			cancel()
			return installJournalInFlight(installJournalQueued(identity, ownerID), attemptID), true, nil
		}
		operation, created, err = service.ReserveResource(ctx, installJournalOpID, installJournalOwner, "data_directory", installJournalAttempt)
		if !errors.Is(err, context.Canceled) || created || operation != nil {
			t.Fatalf("reserve returned a grant after cancel: op=%v created=%v err=%v", operation != nil, created, err)
		}
	})
	t.Run("post-commit failure never returns a fresh grant", func(t *testing.T) {
		service, operations, resources := installJournalService(t)
		operations.create = func(_ context.Context, request domain.ProbeInstallIdentity, ownerID string) (*domain.ProbeInstallOperation, bool, error) {
			return installJournalQueued(request, ownerID), true, fmt.Errorf("commit acknowledged twice: %w", errInstallJournalUnexpected)
		}
		operation, created, err := service.Create(context.Background(), identity, installJournalOwner)
		if !errors.Is(err, domain.ErrInternal) || created || operation != nil {
			t.Fatalf("create granted past a failed acknowledgement: op=%v created=%v err=%v", operation != nil, created, err)
		}
		resources.reserve = func(_ context.Context, _ string, ownerID, _ string, attemptID string) (*domain.ProbeInstallOperation, bool, error) {
			return installJournalInFlight(installJournalQueued(identity, ownerID), attemptID), true, ports.ErrConflict
		}
		operation, created, err = service.ReserveResource(context.Background(), installJournalOpID, installJournalOwner, "data_directory", installJournalAttempt)
		if !errors.Is(err, ports.ErrConflict) || created || operation != nil {
			t.Fatalf("reserve granted past a failed acknowledgement: op=%v created=%v err=%v", operation != nil, created, err)
		}
	})
}

func TestProbeInstallJournalErrorNormalizationAndSecrecy(t *testing.T) {
	identity := installJournalIdentity(domain.ProbeInstallDocker, installJournalOpID)
	t.Run("unknown and corrupt repository errors collapse to fixed ErrInternal", func(t *testing.T) {
		for _, raw := range []error{
			errors.New(installJournalLeakMarker),
			fmt.Errorf("row corrupt: %w", domain.ErrValidation),
			errors.Join(domain.ErrInternal, errors.New(installJournalLeakMarker)),
		} {
			service, operations, resources := installJournalService(t)
			operations.create = func(context.Context, domain.ProbeInstallIdentity, string) (*domain.ProbeInstallOperation, bool, error) {
				return nil, false, raw
			}
			operations.observe = func(context.Context, string) (*domain.ProbeInstallOperation, error) { return nil, raw }
			operations.begin = func(context.Context, string, string) (*domain.ProbeInstallOperation, error) { return nil, raw }
			operations.cancel = func(context.Context, string) (*domain.ProbeInstallOperation, error) { return nil, raw }
			operations.finish = func(context.Context, string, string, domain.ProbeInstallOutcome) (*domain.ProbeInstallOperation, error) {
				return nil, raw
			}
			resources.reserve = func(context.Context, string, string, string, string) (*domain.ProbeInstallOperation, bool, error) {
				return nil, false, raw
			}
			resources.record = func(context.Context, string, string, string, string, domain.ProbeInstallResourceOutcome) (*domain.ProbeInstallOperation, error) {
				return nil, raw
			}
			for name, call := range map[string]func() error{
				"create": func() error {
					_, _, err := service.Create(context.Background(), identity, installJournalOwner)
					return err
				},
				"observe": func() error { _, err := service.Observe(context.Background(), installJournalOpID); return err },
				"begin": func() error {
					_, err := service.Begin(context.Background(), installJournalOpID, installJournalOwner)
					return err
				},
				"cancel": func() error { _, err := service.Cancel(context.Background(), installJournalOpID); return err },
				"finish": func() error {
					_, err := service.Finish(context.Background(), installJournalOpID, installJournalOwner, domain.ProbeInstallOutcome{State: domain.ProbeInstallCanceled, ErrorCode: "canceled"})
					return err
				},
				"reserve": func() error {
					_, _, err := service.ReserveResource(context.Background(), installJournalOpID, installJournalOwner, "image_cache", installJournalAttempt)
					return err
				},
				"record": func() error {
					_, err := service.RecordResource(context.Background(), installJournalOpID, installJournalOwner, "image_cache", installJournalAttempt, domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied})
					return err
				},
			} {
				err := call()
				if !errors.Is(err, domain.ErrInternal) {
					t.Fatalf("%s: err=%v, want ErrInternal", name, err)
				}
				if err.Error() != domain.ErrInternal.Error() {
					t.Fatalf("%s: leaked repository text: %q", name, err.Error())
				}
			}
		}
	})
	t.Run("fixed sentinels survive without repository text", func(t *testing.T) {
		for _, tc := range []struct {
			raw  error
			want error
		}{
			{fmt.Errorf("lookup %s: %w", installJournalLeakMarker, ports.ErrNotFound), ports.ErrNotFound},
			{fmt.Errorf("state %s: %w", installJournalLeakMarker, ports.ErrConflict), ports.ErrConflict},
			{fmt.Errorf("call %s: %w", installJournalLeakMarker, context.Canceled), context.Canceled},
			{fmt.Errorf("call %s: %w", installJournalLeakMarker, context.DeadlineExceeded), context.DeadlineExceeded},
		} {
			service, operations, _ := installJournalService(t)
			operations.observe = func(context.Context, string) (*domain.ProbeInstallOperation, error) { return nil, tc.raw }
			_, err := service.Observe(context.Background(), installJournalOpID)
			if !errors.Is(err, tc.want) || err != tc.want {
				t.Fatalf("sentinel %v normalized to %v", tc.want, err)
			}
			if err.Error() != tc.want.Error() {
				t.Fatalf("sentinel %v leaked text: %q", tc.want, err.Error())
			}
		}
	})
}

func TestProbeInstallJournalDefensiveSnapshotCopies(t *testing.T) {
	identity := installJournalIdentity(domain.ProbeInstallSystemd, installJournalOpID)
	stored := installJournalQueued(identity, installJournalOwner)
	service, operations, _ := installJournalService(t)
	operations.create = func(_ context.Context, request domain.ProbeInstallIdentity, ownerID string) (*domain.ProbeInstallOperation, bool, error) {
		return stored, true, nil
	}
	first, _, err := service.Create(context.Background(), identity, installJournalOwner)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	// The caller's mutation must not reach the repository receipt.
	first.Resources[0].State = domain.ProbeInstallResourceUnknown
	first.Resources[0].Spec.Reference = "/var/lib/attacker"
	first.State = domain.ProbeInstallInterrupted
	second, _, err := service.Create(context.Background(), identity, installJournalOwner)
	if err != nil {
		t.Fatalf("second create failed: %v", err)
	}
	if second.State != domain.ProbeInstallQueued || second.Resources[0].State != domain.ProbeInstallResourcePlanned ||
		second.Resources[0].Spec.Reference != stored.Resources[0].Spec.Reference {
		t.Fatalf("caller mutation leaked into the stored graph: %+v", second.Resources[0])
	}
	if &second.Resources[0] == &first.Resources[0] {
		t.Fatal("receipts share backing storage")
	}
	// A repository-side mutation must not corrupt an already returned receipt.
	marker := second.Resources[0].Spec
	stored.Resources[0].Spec.Reference = "/var/lib/other"
	stored.Resources[0].State = domain.ProbeInstallResourceUnknown
	if second.Resources[0].Spec != marker || second.Resources[0].State != domain.ProbeInstallResourcePlanned {
		t.Fatalf("repository mutation leaked into the returned snapshot: %+v", second.Resources[0])
	}
}
