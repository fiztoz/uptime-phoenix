package ports

import (
	"context"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// EdgeStreamResetProtector authenticates the local reset journal and archive
// manifest under a purpose distinct from config, credentials and certificate PEM.
type EdgeStreamResetProtector interface {
	SealStreamReset(context.Context, domain.EdgeStreamResetRecord) ([]byte, error)
	VerifyStreamReset(context.Context, domain.EdgeStreamResetRecord, []byte) error
}

// EdgeCertificateRebinder authenticates active PEM under its old scope before
// resealing the same key and certificate for an explicitly authorized new epoch.
type EdgeCertificateRebinder interface {
	RebindCertificate(context.Context, domain.ProtectedEdgeCertificate, string, time.Time) (domain.ProtectedEdgeCertificate, error)
}
