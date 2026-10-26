package services

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// probeCleanupRunTTL bounds one cleanup's host work inside its 5-minute
// durable lifetime, leaving time to record the outcome before the deadline.
const probeCleanupRunTTL = 4 * time.Minute

// ProbeCleanupSubmission is one admin request to remove an install
// operation's host resources. The SSH host and its pinned host key come from
// the stored install, never from this request. PrivateKey is a transient
// deployment key.
type ProbeCleanupSubmission struct {
	CleanupID   string
	OperationID string
	PrivateKey  []byte
	RequestedBy int64
}

// Valid checks shape only.
func (r ProbeCleanupSubmission) Valid() bool {
	return domain.ValidProbeOperationID(r.CleanupID) && domain.ValidProbeOperationID(r.OperationID) && r.RequestedBy > 0 &&
		len(r.PrivateKey) > 0 && len(r.PrivateKey) <= domain.MaxProbeInstallPrivateKeySize
}

// ProbeCleanupOperationService admits cleanup requests and runs each admitted
// cleanup inside this process with bounded concurrency, like the install and
// activation services. A cleanup is one authorized host action: Begin reserves
// its attempt, the host session applies it, and Record settles the receipt.
// The transient key lives only in the per-cleanup host session.
type ProbeCleanupOperationService struct {
	repo     ports.ProbeCleanupRepository
	installs probeActivationInstalls
	sessions ports.ProbeCleanupSessionFactory
	ownerID  string
	capacity int

	rootCtx    context.Context
	rootCancel context.CancelFunc

	mu     sync.Mutex
	closed bool
	active int
	tasks  sync.WaitGroup
}

// NewProbeCleanupOperationService binds the cleanup store, the install
// journal, the session factory and the owner UUID fencing every run.
func NewProbeCleanupOperationService(ctx context.Context, repo ports.ProbeCleanupRepository, installs *ProbeInstallJournalService, sessions ports.ProbeCleanupSessionFactory, ownerID string, capacity int) (*ProbeCleanupOperationService, error) {
	if installs == nil {
		return nil, domain.ErrValidation
	}
	return newProbeCleanupOperationService(ctx, repo, installs, sessions, ownerID, capacity)
}

func newProbeCleanupOperationService(ctx context.Context, repo ports.ProbeCleanupRepository, installs probeActivationInstalls, sessions ports.ProbeCleanupSessionFactory, ownerID string, capacity int) (*ProbeCleanupOperationService, error) {
	if ctx == nil || binaryStageAuthorizationNil(repo) || binaryStageAuthorizationNil(installs) || binaryStageAuthorizationNil(sessions) ||
		!domain.ValidProbeOperationID(ownerID) || capacity < probeInstallMinCapacity || capacity > probeInstallMaxCapacity {
		return nil, domain.ErrValidation
	}
	rootCtx, rootCancel := context.WithCancel(ctx)
	return &ProbeCleanupOperationService{
		repo: repo, installs: installs, sessions: sessions, ownerID: ownerID, capacity: capacity,
		rootCtx: rootCtx, rootCancel: rootCancel,
	}, nil
}

// Submit admits one cleanup. The install must exist in a cleanup-eligible
// state; the store re-checks every eligibility rule under its lock. The
// session is built from the install's stored host pin before any durable
// write. Execution is dispatched only after CreateCleanup commits a new row;
// an exact retry returns the stored receipt without a second dispatch.
func (s *ProbeCleanupOperationService) Submit(ctx context.Context, request ProbeCleanupSubmission) (*domain.ProbeCleanup, error) {
	if ctx == nil {
		return nil, domain.ErrValidation
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !request.Valid() {
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
	if install == nil || (install.Identity.Mode != domain.ProbeInstallSystemd && install.Identity.Mode != domain.ProbeInstallDocker) || !domain.ProbeInstallCleanupEligible(install.State) {
		return nil, domain.ErrProbeCleanupIneligible
	}
	session, clientFingerprint, err := s.sessions.NewCleanupSession(admitCtx, request.PrivateKey, install.Identity.HostKeyFingerprint)
	if err != nil || binaryStageAuthorizationNil(session) {
		if s.rootCtx.Err() != nil && ctx.Err() == nil {
			return nil, domain.ErrProbeInstallClosed
		}
		return nil, probeInstallSessionError(err)
	}
	identity := domain.ProbeCleanupIdentity{
		CleanupID: request.CleanupID, Install: install.Identity, ClientKeyFingerprint: clientFingerprint, RequestedBy: request.RequestedBy,
	}
	if !identity.Valid() {
		return nil, domain.ErrValidation
	}
	if !reserved {
		current, err := s.repo.ObserveCleanup(admitCtx, request.CleanupID)
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
	cleanup, created, err := s.repo.CreateCleanup(admitCtx, identity, s.ownerID)
	if err != nil {
		if s.rootCtx.Err() != nil && ctx.Err() == nil {
			return nil, domain.ErrProbeInstallClosed
		}
		return nil, err
	}
	if cleanup == nil || cleanup.Identity != identity {
		return nil, domain.ErrInternal
	}
	if !created {
		return cleanup, nil
	}
	attempts, err := probeInstallAttempts(1)
	if err != nil {
		// The committed queued row honestly expires to interrupted.
		return nil, domain.ErrInternal
	}
	dispatched = true
	go s.execute(identity, session, attempts[0])
	return cleanup, nil
}

// Observe reads one durable cleanup; storage reconciles cold expiry.
func (s *ProbeCleanupOperationService) Observe(ctx context.Context, cleanupID string) (*domain.ProbeCleanup, error) {
	if ctx == nil || !domain.ValidProbeOperationID(cleanupID) {
		return nil, domain.ErrValidation
	}
	return s.repo.ObserveCleanup(ctx, cleanupID)
}

// Latest reads an install operation's newest cleanup.
func (s *ProbeCleanupOperationService) Latest(ctx context.Context, operationID string) (*domain.ProbeCleanup, error) {
	if ctx == nil || !domain.ValidProbeOperationID(operationID) {
		return nil, domain.ErrValidation
	}
	return s.repo.LatestCleanup(ctx, operationID)
}

// Close stops admission, cancels tracked runs and joins them. Each run
// records its own honest outcome before returning.
func (s *ProbeCleanupOperationService) Close(ctx context.Context) error {
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

// execute begins, applies and records one cleanup. Store writes after the
// host action use a context detached from cancellation (bounded), so a
// closing process still records what it knows. A Begin that did not
// confirm running abandons the still-queued row; if Begin in fact committed,
// the abandon is refused and the stored deadline interrupts it honestly.
func (s *ProbeCleanupOperationService) execute(identity domain.ProbeCleanupIdentity, session ports.ProbeCleanupHostSession, attemptID string) {
	// release also marks the task done that reserve added.
	defer s.release()
	id := identity.CleanupID
	runCtx, cancel := context.WithTimeout(s.rootCtx, probeCleanupRunTTL)
	defer cancel()
	settle := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(s.rootCtx), probeInstallCloseTTL)
	}
	begun, err := s.repo.BeginCleanup(runCtx, id, s.ownerID, attemptID)
	if err != nil || begun == nil || begun.State != domain.ProbeCleanupRunning || begun.AttemptID != attemptID || begun.Identity != identity {
		abandonCtx, abandonCancel := settle()
		final, abandonErr := s.repo.AbandonCleanup(abandonCtx, id, s.ownerID)
		abandonCancel()
		slog.Warn("probe cleanup did not begin", "cleanup_id", id, "abandoned", abandonErr == nil && final != nil && final.Terminal())
		return
	}
	outcome := session.Cleanup(runCtx, identity.Intent(s.ownerID, attemptID))
	if !outcome.Valid() {
		outcome = domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceUnknown, ErrorCode: "outcome_unknown"}
	}
	recordCtx, recordCancel := settle()
	final, err := s.repo.RecordCleanup(recordCtx, id, s.ownerID, attemptID, outcome)
	recordCancel()
	state := ""
	if final != nil {
		state = final.State
	}
	if err != nil {
		slog.Warn("probe cleanup outcome was not recorded", "cleanup_id", id, "outcome", outcome.State)
		return
	}
	slog.Info("probe cleanup joined", "cleanup_id", id, "operation_id", identity.Install.OperationID, "state", state)
}

func (s *ProbeCleanupOperationService) reserve() error {
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

func (s *ProbeCleanupOperationService) release() {
	s.mu.Lock()
	s.active--
	s.mu.Unlock()
	s.tasks.Done()
}
