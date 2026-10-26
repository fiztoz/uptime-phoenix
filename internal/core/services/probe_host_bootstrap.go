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

// probeHostBootstrapTTL bounds one whole bootstrap: host-key verification, the
// password login, the helper upload, the root writes and the restricted-key
// verification. It is synchronous because the password must never outlive the
// request that carried it: there is no journal, queue or retry.
const probeHostBootstrapTTL = 150 * time.Second

// ProbeHostBootstrapService admits one-time password-authenticated host
// preparations for the M8 restricted-key installer. It owns no durable state:
// the password and deployment key exist only for the duration of one call,
// and both borrowed buffers are cleared before Bootstrap returns.
type ProbeHostBootstrapService struct {
	bootstrapper ports.ProbeHostBootstrapper
	slots        chan struct{}
	mu           sync.Mutex
	closed       bool
	active       sync.WaitGroup
}

// NewProbeHostBootstrapService builds the service. capacity bounds concurrent
// bootstraps per API process and must be within 1..4.
func NewProbeHostBootstrapService(bootstrapper ports.ProbeHostBootstrapper, capacity int) (*ProbeHostBootstrapService, error) {
	if bootstrapper == nil || capacity < 1 || capacity > 4 {
		return nil, domain.ErrValidation
	}
	return &ProbeHostBootstrapService{bootstrapper: bootstrapper, slots: make(chan struct{}, capacity)}, nil
}

// Bootstrap validates and runs one bootstrap. It consumes req.Password and
// req.PrivateKey: both buffers are zeroed before it returns, whatever the
// outcome. Errors are domain.ErrValidation (malformed request),
// domain.ErrProbeSSHInput (unparsable key), domain.ErrProbeInstallArtifact
// (helper pin rejected), ErrProbeHostBootstrapBusy/Closed, a context error or a
// *domain.ProbeHostBootstrapError with one fixed code. No error text ever
// includes the password, the key, the address or host output.
func (s *ProbeHostBootstrapService) Bootstrap(ctx context.Context, req domain.ProbeHostBootstrapRequest) (domain.ProbeHostBootstrapResult, error) {
	defer clear(req.Password)
	defer clear(req.PrivateKey)
	var zero domain.ProbeHostBootstrapResult
	if s == nil || ctx == nil {
		return zero, domain.ErrValidation
	}
	if !req.Valid() {
		return zero, domain.ErrValidation
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if !s.enter() {
		return zero, domain.ErrProbeHostBootstrapClosed
	}
	defer s.active.Done()
	select {
	case s.slots <- struct{}{}:
	default:
		return zero, domain.ErrProbeHostBootstrapBusy
	}
	defer func() { <-s.slots }()
	runCtx, cancel := context.WithTimeout(ctx, probeHostBootstrapTTL)
	defer cancel()
	result, err := s.bootstrapper.Bootstrap(runCtx, req)
	err = probeHostBootstrapError(ctx, err)
	if err == nil && !result.Valid() {
		// A bootstrapper that reports success without verification, or with an
		// unknown outcome, is not success.
		err = &domain.ProbeHostBootstrapError{Code: domain.ProbeHostBootstrapFailed}
	}
	outcome := "verified"
	if err != nil {
		outcome = probeHostBootstrapOutcome(err)
		result = zero
	}
	// Audit line: actor, pin and fixed outcome only. Never the address, user,
	// fingerprints, password, key or host output.
	slog.Info("probe host bootstrap", "requested_by", req.RequestedBy,
		"helper_version", req.Helper.Version, "helper_architecture", req.Helper.Architecture,
		"outcome", outcome)
	return result, err
}

// Close stops admission and waits for in-flight bootstraps (each is bounded
// by probeHostBootstrapTTL) or for ctx.
func (s *ProbeHostBootstrapService) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.active.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *ProbeHostBootstrapService) enter() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.active.Add(1)
	return true
}

// probeHostBootstrapError keeps the fixed, safe error set and folds anything
// else (which could carry adapter detail) into one generic code.
func probeHostBootstrapError(parent context.Context, err error) error {
	if err == nil {
		return nil
	}
	var refusal *domain.ProbeHostBootstrapError
	switch {
	case errors.As(err, &refusal) && refusal != nil && domain.ValidProbeHostBootstrapCode(refusal.Code):
		return &domain.ProbeHostBootstrapError{Code: refusal.Code}
	case errors.Is(err, domain.ErrProbeInstallArtifact):
		return domain.ErrProbeInstallArtifact
	case errors.Is(err, domain.ErrProbeSSHInput):
		return domain.ErrProbeSSHInput
	case parent.Err() != nil:
		return parent.Err()
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, context.Canceled):
		return context.Canceled
	}
	return &domain.ProbeHostBootstrapError{Code: domain.ProbeHostBootstrapFailed}
}

func probeHostBootstrapOutcome(err error) string {
	var refusal *domain.ProbeHostBootstrapError
	switch {
	case errors.As(err, &refusal):
		return refusal.Code
	case errors.Is(err, domain.ErrProbeInstallArtifact):
		return "artifact_rejected"
	case errors.Is(err, domain.ErrProbeSSHInput):
		return "invalid_key"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	return "failed"
}
