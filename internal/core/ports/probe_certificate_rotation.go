package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeCertificateRotationRepository stores issuance and selects trust under the
// current connector fence. Only a durable command receipt promotes a new pin.
type ProbeCertificateRotationRepository interface {
	CreateCertificateRotation(context.Context, domain.ProtectedProbeCertificateRotation) (*domain.ProbeCertificateRotation, error)
	GetCertificateRotation(context.Context, string, string, string) (*domain.ProbeCertificateRotation, error)
	SelectCertificateConnection(context.Context, domain.ProbeReplaySession) (domain.ProbeCertificateSelection, error)
	ConfirmCertificateConnection(context.Context, domain.ProbeReplaySession, domain.ProbeCredentialMetadata, string) error
}
