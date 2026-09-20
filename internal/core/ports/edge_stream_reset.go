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

// ProbeStreamResetRepository fences administrative recovery and confirms it only
// from the live, authenticated new-stream session. All transitions are durable.
type ProbeStreamResetRepository interface {
	PrepareStreamReset(context.Context, domain.ProbeStreamResetIssue) (*domain.ProbeStreamResetOperation, error)
	ActivateStreamReset(context.Context, domain.EdgeStreamResetRecord) (*domain.ProbeStreamResetOperation, error)
	GetStreamReset(context.Context, string, string, string) (*domain.ProbeStreamResetOperation, error)
	ConfirmStreamReset(context.Context, domain.ProbeReplaySession, domain.ProbeCredentialMetadata, string) error
}

// ProbeStreamResetCodec maps explicit bounded operator documents to pure types.
// Reading a document does not authorize source mutation or confirm a peer.
type ProbeStreamResetCodec interface {
	EncodeStreamResetPlan(context.Context, domain.ProbeStreamResetPlan) ([]byte, error)
	DecodeStreamResetPlan(context.Context, []byte) (domain.ProbeStreamResetPlan, error)
	EncodeStreamResetReceipt(context.Context, domain.EdgeStreamResetRecord) ([]byte, error)
	DecodeStreamResetReceipt(context.Context, []byte) (domain.EdgeStreamResetRecord, error)
}
