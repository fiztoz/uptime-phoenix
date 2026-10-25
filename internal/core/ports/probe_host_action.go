package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeHostActionAdmissionRepository atomically consumes one exact root/systemd
// data_directory, unit_file or service attempt under current stored authority,
// with the same contract as ProbeBinaryStageAdmissionRepository: every replay
// is refused, admission commit is the linearization point, and a lost commit
// acknowledgement never returns a usable admission or replenishes the attempt.
type ProbeHostActionAdmissionRepository interface {
	AdmitHostAction(ctx context.Context, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionAdmission, error)
}

// ProbeHostActionGrantSigner signs only the fixed host-action admission
// message. Callers must first obtain a fresh committed admission.
type ProbeHostActionGrantSigner interface {
	PublicKey() [32]byte
	SignHostActionAdmission(ctx context.Context, admission domain.ProbeHostActionAdmission) (domain.ProbeHostActionGrant, error)
}
