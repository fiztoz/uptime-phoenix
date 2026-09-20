package services

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// EdgeEnrollmentService owns short-lived local authorization and inbound auth.
// A token is returned only from Issue; no read path reconstructs plaintext.
type EdgeEnrollmentService struct {
	repo ports.EdgeEnrollmentRepository
}

// NewEdgeEnrollmentService wires the dedicated edge store.
func NewEdgeEnrollmentService(repo ports.EdgeEnrollmentRepository) *EdgeEnrollmentService {
	return &EdgeEnrollmentService{repo: repo}
}

// Issue creates a fresh ten-minute enrollment token for the local operator.
func (s *EdgeEnrollmentService) Issue(ctx context.Context, at time.Time) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if at.IsZero() {
		return "", domain.ErrValidation
	}
	var entropy [32]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", errors.New("generate enrollment token failed")
	}
	token := "phx_probe_enroll_" + base64.RawURLEncoding.EncodeToString(entropy[:])
	hash := sha256.Sum256([]byte(token))
	if err := s.repo.IssueEnrollmentToken(ctx, domain.EdgeEnrollmentToken{Hash: hash, IssuedAt: at.UTC(), ExpiresAt: at.UTC().Add(10 * time.Minute)}); err != nil {
		return "", err
	}
	return token, nil
}

// AuthenticateEnrollment checks a header token without consuming authorization.
func (s *EdgeEnrollmentService) AuthenticateEnrollment(ctx context.Context, token string, at time.Time) (bool, error) {
	if !validEdgeToken(token, "phx_probe_enroll_") {
		return false, nil
	}
	return s.repo.EnrollmentTokenValid(ctx, sha256.Sum256([]byte(token)), at.UTC())
}

// Accept persists an offered runtime credential and consumes local authorization.
// Binding metadata comes from the validated protocol; time comes from the server.
func (s *EdgeEnrollmentService) Accept(ctx context.Context, enrollmentToken, runtimeToken string, binding domain.EdgeEnrollment, at time.Time) error {
	if !validEdgeToken(enrollmentToken, "phx_probe_enroll_") || strings.HasPrefix(runtimeToken, "phx_probe_enroll_") || !validEdgeToken(runtimeToken, "phx_probe_") || at.IsZero() {
		return domain.ErrValidation
	}
	binding.TokenHash, binding.AppliedAt = sha256.Sum256([]byte(runtimeToken)), at.UTC()
	if !domain.ValidEdgeEnrollment(binding) {
		return domain.ErrValidation
	}
	return s.repo.CommitEnrollment(ctx, sha256.Sum256([]byte(enrollmentToken)), binding)
}

// AuthenticateRuntime validates only the current credential and returns its
// trusted binding. Missing, malformed and wrong tokens have the same outcome.
func (s *EdgeEnrollmentService) AuthenticateRuntime(ctx context.Context, token string) (domain.EdgeEnrollment, bool, error) {
	if strings.HasPrefix(token, "phx_probe_enroll_") || !validEdgeToken(token, "phx_probe_") {
		return domain.EdgeEnrollment{}, false, nil
	}
	binding, err := s.repo.ReadEnrollment(ctx)
	if errors.Is(err, ports.ErrNotFound) {
		return domain.EdgeEnrollment{}, false, nil
	}
	if err != nil {
		return domain.EdgeEnrollment{}, false, err
	}
	hash := sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare(hash[:], binding.TokenHash[:]) != 1 {
		return domain.EdgeEnrollment{}, false, nil
	}
	return binding, true, nil
}

func validEdgeToken(token, prefix string) bool {
	if !strings.HasPrefix(token, prefix) || len(token) > 256 {
		return false
	}
	rest := strings.TrimPrefix(token, prefix)
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(rest)
	return err == nil && len(decoded) >= 32
}
