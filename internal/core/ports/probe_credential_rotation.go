package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeCredentialCommandCodec maps closed wire commands to digest-only effects.
// The plaintext token is a write-only encoding input and is never in the result.
type ProbeCredentialCommandCodec interface {
	EncodeCredentialCommand(context.Context, domain.ProbeCredentialCommand, string) ([]byte, error)
	DecodeCredentialCommand(context.Context, []byte) (domain.ProbeCredentialCommand, error)
}

// ProbeCredentialRotationRepository persists candidate/requests before dispatch
// and fences selection/confirmation with current connector authority. Connection
// confirmation proves authentication only; source command completion activates.
type ProbeCredentialRotationRepository interface {
	CreateCredentialRotation(context.Context, domain.ProtectedProbeCredentialRotation) (*domain.ProbeCredentialRotation, error)
	GetCredentialRotation(context.Context, string, string, string) (*domain.ProbeCredentialRotation, error)
	SelectCredentialConnection(context.Context, domain.ProbeReplaySession) (domain.ProbeCredentialSelection, error)
	ConfirmCredentialConnection(context.Context, domain.ProbeReplaySession, domain.ProbeCredentialMetadata) error
}
