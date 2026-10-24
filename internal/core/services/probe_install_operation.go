package services

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// Process-local bounds for asynchronous SSH probe installs. The composition
// root passes 2; only values in [1,4] are admitted. Each run is additionally
// bounded by the runner and the journal's DB-clock deadline.
const (
	probeInstallMinCapacity  = 1
	probeInstallMaxCapacity  = 4
	probeInstallAdmissionTTL = 30 * time.Second
	// probeInstallSubmitTTL bounds a whole install admission: session
	// preparation, the journal lookup and the SSH host preflight (which may
	// learn the pinned host-key family and retry once).
	probeInstallSubmitTTL = 90 * time.Second
	probeInstallCloseTTL  = 10 * time.Second
)

// probeInstallJournal is the subset of ProbeInstallJournalService the API
// service uses directly; the runner owns every other transition.
type probeInstallJournal interface {
	Create(ctx context.Context, request domain.ProbeInstallIdentity, ownerID string) (*domain.ProbeInstallOperation, bool, error)
	Observe(ctx context.Context, operationID string) (*domain.ProbeInstallOperation, error)
	Cancel(ctx context.Context, operationID string) (*domain.ProbeInstallOperation, error)
}

// probeInstallRunner runs one admitted operation to a joined return.
type probeInstallRunner interface {
	Run(ctx context.Context, expected domain.ProbeInstallIdentity, ownerID string, attempts []string) (*domain.ProbeInstallOperation, error)
}

// ProbeInstallOperationService admits admin install requests and runs each
// admitted operation inside this process with bounded concurrency. The durable
// journal is the only authority for owner, state and expiry; the transient
// deployment key and prepared artifact live only in the per-operation session,
// which is closed when its run returns. No DB transaction spans SSH.
type ProbeInstallOperationService struct {
	journal   probeInstallJournal
	newRunner func(ports.ProbeInstallResourceExecutor) (probeInstallRunner, error)
	sessions  ports.ProbeInstallSessionFactory
	ownerID   string
	capacity  int

	rootCtx    context.Context
	rootCancel context.CancelFunc

	mu     sync.Mutex
	closed bool
	active int
	tasks  sync.WaitGroup
}

// NewProbeInstallOperationService binds the journal facade, the session
// factory and the composition-generated owner UUID fencing every run from this
// process. ctx is the root lifetime: canceling it or Close stops admission and
// cancels tracked runs, which the runner then joins and records honestly.
func NewProbeInstallOperationService(ctx context.Context, journal *ProbeInstallJournalService, sessions ports.ProbeInstallSessionFactory, ownerID string, capacity int) (*ProbeInstallOperationService, error) {
	if journal == nil {
		return nil, domain.ErrValidation
	}
	return newProbeInstallOperationService(ctx, journal, func(executor ports.ProbeInstallResourceExecutor) (probeInstallRunner, error) {
		return NewProbeInstallRunner(journal, executor)
	}, sessions, ownerID, capacity)
}

func newProbeInstallOperationService(ctx context.Context, journal probeInstallJournal, newRunner func(ports.ProbeInstallResourceExecutor) (probeInstallRunner, error), sessions ports.ProbeInstallSessionFactory, ownerID string, capacity int) (*ProbeInstallOperationService, error) {
	if ctx == nil || binaryStageAuthorizationNil(journal) || newRunner == nil || binaryStageAuthorizationNil(sessions) ||
		!domain.ValidProbeOperationID(ownerID) || capacity < probeInstallMinCapacity || capacity > probeInstallMaxCapacity {
		return nil, domain.ErrValidation
	}
	rootCtx, rootCancel := context.WithCancel(ctx)
	return &ProbeInstallOperationService{
		journal: journal, newRunner: newRunner, sessions: sessions, ownerID: ownerID, capacity: capacity,
		rootCtx: rootCtx, rootCancel: rootCancel,
	}, nil
}

// Submit admits one install request. The session (key parse and artifact
// verification) is built before any durable write, so a rejected key or
// artifact creates nothing. An exact retry of a committed operation returns
// the stored receipt without touching the host or dispatching again, and a
// changed identity conflicts. A new operation must then pass the host's
// nonmutating install preflight; any refusal returns a
// *domain.ProbeInstallPreflightError and creates nothing anywhere. Execution
// is dispatched only after Create commits a brand-new operation.
//
// The preflight is not a host lock: two different operations admitted for
// one host at the same moment can both pass it. The second install then
// fails honestly at its own resources or at activation readiness.
func (s *ProbeInstallOperationService) Submit(ctx context.Context, request domain.ProbeInstallSubmission) (*domain.ProbeInstallOperation, error) {
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
	admitCtx, cancelAdmission := context.WithTimeout(ctx, probeInstallSubmitTTL)
	stop := context.AfterFunc(s.rootCtx, cancelAdmission)
	defer stop()
	defer cancelAdmission()

	// The key is borrowed only for this synchronous call; the factory never
	// retains the caller's buffer.
	session, clientFingerprint, err := s.sessions.NewInstallSession(admitCtx, request.PrivateKey, request.HostKeyFingerprint, request.Binary)
	if err != nil || binaryStageAuthorizationNil(session) {
		if s.rootCtx.Err() != nil && ctx.Err() == nil {
			return nil, domain.ErrProbeInstallClosed
		}
		return nil, probeInstallSessionError(err)
	}
	owned := true
	defer func() {
		if owned {
			closeCtx, cancel := context.WithTimeout(context.Background(), probeInstallCloseTTL)
			_ = session.Close(closeCtx)
			cancel()
		}
	}()
	identity := domain.ProbeInstallIdentity{
		OperationID:          request.OperationID,
		Host:                 request.Host,
		HostKeyFingerprint:   request.HostKeyFingerprint,
		ClientKeyFingerprint: clientFingerprint,
		RequestedBy:          request.RequestedBy,
		LayoutVersion:        1,
		Mode:                 domain.ProbeInstallSystemd,
		Binary:               request.Binary,
	}
	if !identity.Valid() {
		return nil, domain.ErrInternal
	}
	if !reserved {
		// Saturation must not break idempotency of committed work: only an
		// exact retry of an existing operation may answer.
		current, err := s.journal.Observe(admitCtx, identity.OperationID)
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
	// Rerun is explicit: committed work is answered from the journal, never
	// re-preflighted (its own resources would read as an existing install).
	current, err := s.journal.Observe(admitCtx, identity.OperationID)
	switch {
	case err == nil:
		if current == nil || current.Identity != identity {
			return nil, ports.ErrConflict
		}
		return current, nil
	case !errors.Is(err, ports.ErrNotFound):
		if s.rootCtx.Err() != nil && ctx.Err() == nil {
			return nil, domain.ErrProbeInstallClosed
		}
		return nil, err
	}
	report, err := session.Preflight(admitCtx, identity.Host)
	if err != nil {
		if s.rootCtx.Err() != nil && ctx.Err() == nil {
			return nil, domain.ErrProbeInstallClosed
		}
		return nil, probeInstallPreflightError(err)
	}
	if code := report.Refusal(identity); code != "" {
		return nil, &domain.ProbeInstallPreflightError{Code: code}
	}
	operation, created, err := s.journal.Create(admitCtx, identity, s.ownerID)
	if err != nil {
		if s.rootCtx.Err() != nil && ctx.Err() == nil {
			return nil, domain.ErrProbeInstallClosed
		}
		return nil, err
	}
	if operation == nil || operation.Identity != identity {
		return nil, domain.ErrInternal
	}
	if !created {
		return operation, nil
	}
	runner, err := s.newRunner(session)
	if err != nil || binaryStageAuthorizationNil(runner) {
		// The committed queued row honestly expires to interrupted.
		return nil, domain.ErrInternal
	}
	attempts, err := probeInstallAttempts(len(identity.Resources()))
	if err != nil {
		return nil, domain.ErrInternal
	}
	dispatched, owned = true, false
	go s.execute(identity, runner, session, attempts)
	return operation, nil
}

// Observe reads the durable receipt; storage reconciles cold expiry.
func (s *ProbeInstallOperationService) Observe(ctx context.Context, id string) (*domain.ProbeInstallOperation, error) {
	if ctx == nil || !domain.ValidProbeOperationID(id) {
		return nil, domain.ErrValidation
	}
	return s.journal.Observe(ctx, id)
}

// Cancel persists cancellation intent on any replica. The owning runner's
// watcher observes it; this is never a claim that remote work stopped.
// A resources_ready receipt has no remaining installer work, so it is returned
// unchanged rather than demoted to a cancel_requested that nothing would ever
// finish. A run reaching resources_ready between that read and the journal's
// cancellation still ends honestly: the stored deadline interrupts it.
func (s *ProbeInstallOperationService) Cancel(ctx context.Context, id string) (*domain.ProbeInstallOperation, error) {
	if ctx == nil || !domain.ValidProbeOperationID(id) {
		return nil, domain.ErrValidation
	}
	current, err := s.journal.Observe(ctx, id)
	if err != nil {
		return nil, err
	}
	if current != nil && current.State == domain.ProbeInstallResourcesReady {
		return current, nil
	}
	return s.journal.Cancel(ctx, id)
}

// Close stops admission, cancels every tracked run and joins them. Each
// runner records its own honest outcome before returning.
func (s *ProbeInstallOperationService) Close(ctx context.Context) error {
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

func (s *ProbeInstallOperationService) execute(identity domain.ProbeInstallIdentity, runner probeInstallRunner, session ports.ProbeInstallSession, attempts []string) {
	// release also marks the task done that reserve added.
	defer s.release()
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), probeInstallCloseTTL)
		if err := session.Close(closeCtx); err != nil {
			slog.Warn("probe install session close failed", "operation_id", identity.OperationID)
		}
		cancel()
	}()
	operation, err := runner.Run(s.rootCtx, identity, s.ownerID, attempts)
	state := ""
	if operation != nil {
		state = operation.State
	}
	if err != nil {
		slog.Warn("probe install run ended with an error", "operation_id", identity.OperationID, "state", state)
		return
	}
	slog.Info("probe install run joined", "operation_id", identity.OperationID, "state", state)
}

func (s *ProbeInstallOperationService) reserve() error {
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

func (s *ProbeInstallOperationService) release() {
	s.mu.Lock()
	s.active--
	s.mu.Unlock()
	s.tasks.Done()
}

// probeInstallSessionError keeps only fixed sentinels from session creation.
func probeInstallSessionError(err error) error {
	for _, safe := range []error{context.Canceled, context.DeadlineExceeded, domain.ErrProbeInstallArtifact, domain.ErrProbeSSHInput, domain.ErrValidation} {
		if errors.Is(err, safe) {
			return safe
		}
	}
	return domain.ErrInternal
}

// probeInstallPreflightError keeps context errors and fixed refusal codes;
// anything else from a session is an internal failure.
func probeInstallPreflightError(err error) error {
	for _, ctxErr := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, ctxErr) {
			return ctxErr
		}
	}
	var refusal *domain.ProbeInstallPreflightError
	if errors.As(err, &refusal) && refusal != nil && domain.ValidProbeInstallPreflightCode(refusal.Code) {
		return &domain.ProbeInstallPreflightError{Code: refusal.Code}
	}
	return domain.ErrInternal
}

// probeInstallAttempts returns n fresh canonical v4 UUIDs.
func probeInstallAttempts(n int) ([]string, error) {
	out := make([]string, n)
	for i := range out {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		b[6] = b[6]&0x0f | 0x40
		b[8] = b[8]&0x3f | 0x80
		h := hex.EncodeToString(b[:])
		out[i] = h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
		if !domain.ValidProbeOperationID(out[i]) {
			return nil, domain.ErrInternal
		}
	}
	return out, nil
}
