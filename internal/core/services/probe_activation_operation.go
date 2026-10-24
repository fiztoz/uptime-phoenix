package services

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ProbeActivationSubmission is one admin request to activate a resources_ready
// systemd install. The SSH host and its pinned host key come from the stored
// install, never from this request. PrivateKey is a transient deployment key.
type ProbeActivationSubmission struct {
	OperationID string
	PrivateKey  []byte
	ProbeKey    string
	Name        string
	Location    string
	ListenPort  int
	RequestedBy int64
}

// probeActivationInstalls is the install read the service needs.
type probeActivationInstalls interface {
	Observe(ctx context.Context, operationID string) (*domain.ProbeInstallOperation, error)
}

// probeActivationRunFunc runs one admitted activation to a joined return.
type probeActivationRunFunc func(ctx context.Context, executor ports.ProbeActivationStepExecutor, identity domain.ProbeActivationIdentity, ownerID string, attempts []string) (*domain.ProbeActivation, error)

// ProbeActivationOperationService admits activation requests and runs each
// admitted activation inside this process with bounded concurrency, like
// ProbeInstallOperationService. The transient key lives only in the
// per-activation host session; the enrollment token only in its executor.
type ProbeActivationOperationService struct {
	repo     ports.ProbeActivationRepository
	installs probeActivationInstalls
	sessions ports.ProbeActivationSessionFactory
	admin    probeActivationAdmin
	run      probeActivationRunFunc
	ownerID  string
	capacity int

	rootCtx    context.Context
	rootCancel context.CancelFunc

	mu     sync.Mutex
	closed bool
	active int
	tasks  sync.WaitGroup
}

// NewProbeActivationOperationService binds the activation store, the install
// journal, the session factory, the probe admin service and the owner UUID.
func NewProbeActivationOperationService(ctx context.Context, store interface {
	ports.ProbeActivationRepository
	ports.ProbeActivationStepJournal
}, installs *ProbeInstallJournalService, sessions ports.ProbeActivationSessionFactory, admin *ProbeAdminService, ownerID string, capacity int) (*ProbeActivationOperationService, error) {
	if binaryStageAuthorizationNil(store) || installs == nil || admin == nil {
		return nil, domain.ErrValidation
	}
	run := func(runCtx context.Context, executor ports.ProbeActivationStepExecutor, identity domain.ProbeActivationIdentity, owner string, attempts []string) (*domain.ProbeActivation, error) {
		runner, err := NewProbeActivationRunner(store, store, executor)
		if err != nil {
			return nil, err
		}
		return runner.Run(runCtx, identity, owner, attempts)
	}
	return newProbeActivationOperationService(ctx, store, installs, sessions, admin, run, ownerID, capacity)
}

func newProbeActivationOperationService(ctx context.Context, repo ports.ProbeActivationRepository, installs probeActivationInstalls, sessions ports.ProbeActivationSessionFactory, admin probeActivationAdmin, run probeActivationRunFunc, ownerID string, capacity int) (*ProbeActivationOperationService, error) {
	if ctx == nil || binaryStageAuthorizationNil(repo) || binaryStageAuthorizationNil(installs) || binaryStageAuthorizationNil(sessions) ||
		binaryStageAuthorizationNil(admin) || run == nil || !domain.ValidProbeOperationID(ownerID) ||
		capacity < probeInstallMinCapacity || capacity > probeInstallMaxCapacity {
		return nil, domain.ErrValidation
	}
	rootCtx, rootCancel := context.WithCancel(ctx)
	return &ProbeActivationOperationService{
		repo: repo, installs: installs, sessions: sessions, admin: admin, run: run,
		ownerID: ownerID, capacity: capacity, rootCtx: rootCtx, rootCancel: rootCancel,
	}, nil
}

// Submit admits one activation. The install must be resources_ready; the
// session is built from its stored host pin before any durable write.
// Execution is dispatched only after CreateActivation commits a new row.
func (s *ProbeActivationOperationService) Submit(ctx context.Context, request ProbeActivationSubmission) (*domain.ProbeActivation, error) {
	if ctx == nil {
		return nil, domain.ErrValidation
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !domain.ValidProbeOperationID(request.OperationID) || request.RequestedBy <= 0 ||
		len(request.PrivateKey) == 0 || len(request.PrivateKey) > domain.MaxProbeInstallPrivateKeySize {
		return nil, domain.ErrValidation
	}
	admissionErr := s.reserve()
	if admissionErr != nil && !errors.Is(admissionErr, domain.ErrProbeInstallBusy) {
		return nil, admissionErr
	}
	reserved := admissionErr == nil
	dispatched := false
	defer func() {
		if reserved && !dispatched {
			s.release()
		}
	}()
	admitCtx, cancelAdmission := context.WithTimeout(ctx, probeInstallAdmissionTTL)
	stop := context.AfterFunc(s.rootCtx, cancelAdmission)
	defer stop()
	defer cancelAdmission()

	install, err := s.installs.Observe(admitCtx, request.OperationID)
	if err != nil {
		return nil, err
	}
	if install == nil || install.State != domain.ProbeInstallResourcesReady || install.Identity.Mode != domain.ProbeInstallSystemd {
		return nil, ports.ErrConflict
	}
	session, clientFingerprint, err := s.sessions.NewActivationSession(admitCtx, request.PrivateKey, install.Identity.HostKeyFingerprint)
	if err != nil || binaryStageAuthorizationNil(session) {
		if s.rootCtx.Err() != nil && ctx.Err() == nil {
			return nil, domain.ErrProbeInstallClosed
		}
		return nil, probeInstallSessionError(err)
	}
	identity := domain.ProbeActivationIdentity{
		Install: install.Identity, ProbeKey: request.ProbeKey, Name: request.Name, Location: request.Location,
		ListenPort: request.ListenPort, ClientKeyFingerprint: clientFingerprint, RequestedBy: request.RequestedBy,
	}
	if identity.ListenPort == 0 {
		identity.ListenPort = domain.ProbeActivationDefaultPort
	}
	if !identity.Valid() {
		return nil, domain.ErrValidation
	}
	if !reserved {
		current, err := s.repo.ObserveActivation(admitCtx, request.OperationID)
		if errors.Is(err, ports.ErrNotFound) {
			return nil, domain.ErrProbeInstallBusy
		}
		if err != nil {
			return nil, err
		}
		if current == nil || current.Identity != identity {
			return nil, ports.ErrConflict
		}
		return current, nil
	}
	activation, created, err := s.repo.CreateActivation(admitCtx, identity, s.ownerID)
	if err != nil {
		if s.rootCtx.Err() != nil && ctx.Err() == nil {
			return nil, domain.ErrProbeInstallClosed
		}
		return nil, err
	}
	if activation == nil || activation.Identity != identity {
		return nil, domain.ErrInternal
	}
	if !created {
		return activation, nil
	}
	executor, err := newProbeActivationExecutor(session, s.admin)
	if err != nil {
		return nil, domain.ErrInternal
	}
	attempts, err := probeInstallAttempts(len(identity.Steps()))
	if err != nil {
		return nil, domain.ErrInternal
	}
	dispatched = true
	go s.execute(identity, executor, attempts)
	return activation, nil
}

// Observe reads the durable activation; storage reconciles cold expiry.
func (s *ProbeActivationOperationService) Observe(ctx context.Context, id string) (*domain.ProbeActivation, error) {
	if ctx == nil || !domain.ValidProbeOperationID(id) {
		return nil, domain.ErrValidation
	}
	return s.repo.ObserveActivation(ctx, id)
}

// Cancel persists cancellation intent; terminal activations (including
// active) are returned unchanged. It never claims remote work stopped.
func (s *ProbeActivationOperationService) Cancel(ctx context.Context, id string) (*domain.ProbeActivation, error) {
	if ctx == nil || !domain.ValidProbeOperationID(id) {
		return nil, domain.ErrValidation
	}
	current, err := s.repo.ObserveActivation(ctx, id)
	if err != nil {
		return nil, err
	}
	if current != nil && current.Terminal() {
		return current, nil
	}
	return s.repo.RequestActivationCancellation(ctx, id)
}

// Close stops admission, cancels tracked runs and joins them.
func (s *ProbeActivationOperationService) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.rootCancel()
	done := make(chan struct{})
	go func() {
		s.tasks.Wait()
		close(done)
	}()
	if ctx == nil {
		<-done
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *ProbeActivationOperationService) execute(identity domain.ProbeActivationIdentity, executor *ProbeActivationExecutor, attempts []string) {
	// release also marks the task done that reserve added.
	defer s.release()
	defer executor.Forget()
	activation, err := s.run(s.rootCtx, executor, identity, s.ownerID, attempts)
	state := ""
	if activation != nil {
		state = activation.State
	}
	if err != nil {
		slog.Warn("probe activation ended with an error", "operation_id", identity.Install.OperationID, "state", state)
		return
	}
	slog.Info("probe activation joined", "operation_id", identity.Install.OperationID, "state", state)
}

func (s *ProbeActivationOperationService) reserve() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.rootCtx.Err() != nil {
		return domain.ErrProbeInstallClosed
	}
	if s.active >= s.capacity {
		return domain.ErrProbeInstallBusy
	}
	s.active++
	s.tasks.Add(1)
	return nil
}

func (s *ProbeActivationOperationService) release() {
	s.mu.Lock()
	s.active--
	s.mu.Unlock()
	s.tasks.Done()
}
