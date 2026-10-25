package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeBinaryStageAdmissionRepository atomically consumes one exact root/systemd
// artifact attempt under current stored authority. Every replay returns a zero
// admission and an error, never historical success. Admission commit is the
// linearization point; it is not continuous remote fencing or a joined effect.
// No transaction spans signing or external I/O. Lost commit acknowledgments must
// not return a usable admission or replenish the consumed attempt.
type ProbeBinaryStageAdmissionRepository interface {
	AdmitBinaryStage(ctx context.Context, request domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageAdmission, error)
}

// ProbeBinaryStageGrantSigner signs only the fixed stage-admission message.
// It is trusted key custody, not an authority lookup: callers must first obtain
// a fresh committed admission. It starts no detached work or external processes.
type ProbeBinaryStageGrantSigner interface {
	PublicKey() [32]byte
	SignStageAdmission(ctx context.Context, admission domain.ProbeBinaryStageAdmission) (domain.ProbeBinaryStageGrant, error)
}
