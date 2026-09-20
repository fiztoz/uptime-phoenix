package ports

import (
	"context"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// EdgeCertificateProtector separates locally retained TLS keys from every other
// use of the protection key. Opening authenticates all immutable metadata.
type EdgeCertificateProtector interface {
	SealCertificate(context.Context, domain.EdgeCertificateMetadata, []byte) ([]byte, error)
	OpenCertificate(context.Context, domain.EdgeCertificateMetadata, []byte) ([]byte, error)
}

// EdgeCertificateMaterial performs bounded local cryptography, never network or
// file I/O. The repository invokes it within the original preparation transaction.
type EdgeCertificateMaterial interface {
	PrepareCertificate(context.Context, domain.EdgeCommandAuthority, domain.ProbeCertificateCommand, time.Time) (domain.ProtectedEdgeCertificate, error)
	ValidateCertificate(context.Context, domain.ProtectedEdgeCertificate, time.Time) error
}

// EdgeCertificateRepository persists certificate effects and original receipts
// together. ReadActiveCertificate never substitutes bootstrap for a corrupt row.
type EdgeCertificateRepository interface {
	ApplyCertificateCommand(context.Context, domain.EdgeCommandAuthority, domain.ProbeCertificateCommand) (domain.ProbeCommandOutcome, error)
	ReadActiveCertificate(context.Context) (domain.EdgeCertificateState, error)
}

// ProbeCertificateCommandCodec translates closed wire requests without granting
// issuance or execution authority. Private keys are never wire inputs.
type ProbeCertificateCommandCodec interface {
	EncodeCertificateCommand(context.Context, domain.ProbeCertificateCommand) ([]byte, error)
	DecodeCertificateCommand(context.Context, []byte) (domain.ProbeCertificateCommand, error)
}
