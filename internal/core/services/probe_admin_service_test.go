package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const adminTestProbe = "e645246b-b176-4422-8ae5-b79629ee6a29"
const adminTestOperation = "d1123604-32c5-40aa-89f9-b62f93dceac2"

type fakeOperationRepo struct {
	ports.ProbeOperationRepository
	rows map[string]domain.ProbeOperation
	err  error
}

func (f *fakeOperationRepo) CreateOperation(_ context.Context, operation domain.ProbeOperation) error {
	if f.err != nil {
		return f.err
	}
	if !domain.ValidProbeOperation(operation) {
		return domain.ErrValidation
	}
	if f.rows == nil {
		f.rows = map[string]domain.ProbeOperation{}
	}
	if _, exists := f.rows[operation.OperationID]; exists {
		return ports.ErrConflict
	}
	f.rows[operation.OperationID] = operation
	return nil
}

func (f *fakeOperationRepo) GetOperation(_ context.Context, operationID string) (*domain.ProbeOperation, error) {
	row, ok := f.rows[operationID]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return &row, nil
}

func (f *fakeOperationRepo) FinishOperation(_ context.Context, operationID string, status, phase string, opErr *domain.ProbeOperationError, at time.Time) error {
	row, ok := f.rows[operationID]
	if !ok {
		return ports.ErrNotFound
	}
	if row.Status == domain.ProbeOperationSucceeded || row.Status == domain.ProbeOperationFailed {
		return ports.ErrConflict
	}
	if status != domain.ProbeOperationSucceeded && status != domain.ProbeOperationFailed {
		return domain.ErrValidation
	}
	row.Status, row.Phase, row.Error, row.UpdatedAt = status, phase, opErr, at
	f.rows[operationID] = row
	return nil
}

type fakeRegistrationRepo struct {
	ports.ProbeRegistryRepository
	rows      map[string]domain.Probe
	updated   *domain.Probe
	expected  int64
	createErr error
}

func (f *fakeRegistrationRepo) Create(_ context.Context, probe *domain.Probe) error {
	if f.createErr != nil {
		return f.createErr
	}
	if f.rows == nil {
		f.rows = map[string]domain.Probe{}
	}
	probe.ID, probe.Revision = adminTestProbe, 1
	f.rows[probe.ID] = *probe
	return nil
}

func (f *fakeRegistrationRepo) GetByID(_ context.Context, id string) (*domain.Probe, error) {
	row, ok := f.rows[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return &row, nil
}

func (f *fakeRegistrationRepo) Update(_ context.Context, probe *domain.Probe, expectedRevision int64) error {
	f.updated, f.expected = probe, expectedRevision
	current, ok := f.rows[probe.ID]
	if !ok {
		return ports.ErrNotFound
	}
	if expectedRevision != current.Revision {
		return ports.ErrConflict
	}
	probe.Revision = current.Revision + 1
	f.rows[probe.ID] = *probe
	return nil
}

type fakeInstallationRepo struct {
	ports.ProbeInstallationRepository
	hubID string
}

func (f fakeInstallationRepo) Get(context.Context) (*domain.ProbeInstallation, error) {
	if f.hubID == "" {
		return nil, ports.ErrNotFound
	}
	return &domain.ProbeInstallation{HubID: f.hubID}, nil
}

type fakeConnectionRepo struct {
	ports.ProbeConnectionRepository
	connection *domain.ProbeConnection
}

func (f fakeConnectionRepo) GetConnection(_ context.Context, probeID string) (*domain.ProbeConnection, error) {
	if f.connection == nil || f.connection.ProbeID != probeID {
		return nil, ports.ErrNotFound
	}
	return f.connection, nil
}

type fakeEnrollmentRunner struct {
	prepared  []string
	presented string
	err       error
}

func (f *fakeEnrollmentRunner) Prepare(_ context.Context, probeID, streamID, endpoint, pin string) (*domain.ProbeConnection, error) {
	f.prepared = []string{probeID, streamID, endpoint, pin}
	return &domain.ProbeConnection{ProbeCredentialMetadata: domain.ProbeCredentialMetadata{ProbeID: probeID, StreamID: streamID, Endpoint: endpoint, Fingerprint: pin}, State: "prepared"}, nil
}

func (f *fakeEnrollmentRunner) Enroll(_ context.Context, _ string, operatorAuthorization string) error {
	f.presented = operatorAuthorization
	return f.err
}

type fakeRotationIssuer struct {
	issue domain.ProbeCredentialRotationIssue
	err   error
}

func (f *fakeRotationIssuer) Issue(_ context.Context, issue domain.ProbeCredentialRotationIssue) (*domain.ProbeCredentialRotation, error) {
	f.issue = issue
	if f.err != nil {
		return nil, f.err
	}
	return &domain.ProbeCredentialRotation{RotationID: issue.RotationID}, nil
}

type fakeResetRepo struct {
	ports.ProbeStreamResetRepository
	issue domain.ProbeStreamResetIssue
	err   error
}

func (f *fakeResetRepo) PrepareStreamReset(_ context.Context, issue domain.ProbeStreamResetIssue) (*domain.ProbeStreamResetOperation, error) {
	f.issue = issue
	if f.err != nil {
		return nil, f.err
	}
	return &domain.ProbeStreamResetOperation{}, nil
}

func adminTestService() (*ProbeAdminService, *fakeOperationRepo, *fakeRegistrationRepo, *fakeConnectionRepo, *fakeEnrollmentRunner, *fakeRotationIssuer, *fakeResetRepo) {
	operations := &fakeOperationRepo{}
	registry := &fakeRegistrationRepo{rows: map[string]domain.Probe{}}
	connections := &fakeConnectionRepo{}
	enroll := &fakeEnrollmentRunner{}
	rotations := &fakeRotationIssuer{}
	resets := &fakeResetRepo{}
	svc := NewProbeAdminService(operations, registry, fakeInstallationRepo{hubID: "22222222-2222-4222-8222-222222222222"}, connections, enroll, rotations, resets)
	svc.now = func() time.Time { return time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC) }
	return svc, operations, registry, connections, enroll, rotations, resets
}

func adminTestRegistrationRequest() ProbeRegistrationRequest {
	return ProbeRegistrationRequest{Key: "vm-sg", Name: "Singapore", Location: "ap-southeast-1", Endpoint: "probe.example.test:443", TLSPin: testTLSPin()}
}

func TestProbeAdminRegistrationLifecycle(t *testing.T) {
	svc, _, registry, _, _, _, _ := adminTestService()
	created, err := svc.CreateRegistration(t.Context(), adminTestRegistrationRequest())
	if err != nil || created.ID != adminTestProbe || created.Endpoint != "probe.example.test:443" || created.TLSPin != testTLSPin() {
		t.Fatalf("create: %+v %v", created, err)
	}
	for _, tc := range []struct {
		name string
		req  ProbeRegistrationRequest
	}{
		{"missing endpoint", ProbeRegistrationRequest{Key: "k", Name: "n", TLSPin: testTLSPin()}},
		{"bad fingerprint", ProbeRegistrationRequest{Key: "k", Name: "n", Endpoint: "host:443", TLSPin: "not-hex"}},
		{"endpoint with space", ProbeRegistrationRequest{Key: "k", Name: "n", Endpoint: "bad host:443", TLSPin: testTLSPin()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.CreateRegistration(t.Context(), tc.req); !errors.Is(err, ErrInvalidRegistration) {
				t.Fatalf("accepted %v", err)
			}
		})
	}

	// Endpoint and pin are immutable identity: a patch preserves them.
	patched, err := svc.UpdateRegistration(t.Context(), adminTestProbe, ProbeRegistrationPatch{Name: "Singapore 2", Location: "sg", Enabled: false, ExpectedRevision: 1})
	if err != nil || patched.Revision != 2 || patched.Name != "Singapore 2" || patched.Enabled {
		t.Fatalf("patch: %+v %v", patched, err)
	}
	if registry.updated == nil || registry.updated.Endpoint != "probe.example.test:443" || registry.updated.TLSPin != testTLSPin() {
		t.Fatalf("patch mutated the frozen network trust: %+v", registry.updated)
	}
	if _, err := svc.UpdateRegistration(t.Context(), adminTestProbe, ProbeRegistrationPatch{Name: "x", Location: "y", Enabled: true, ExpectedRevision: 1}); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("stale patch: %v", err)
	}
	if _, err := svc.UpdateRegistration(t.Context(), domain.LocalProbeID, ProbeRegistrationPatch{Name: "x", Location: "y", ExpectedRevision: 1}); !errors.Is(err, ErrInvalidRegistration) {
		t.Fatalf("local mutation accepted: %v", err)
	}
}
