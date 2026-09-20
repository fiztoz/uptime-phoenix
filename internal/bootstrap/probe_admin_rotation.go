package bootstrap

import (
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// The operator view deliberately excludes candidate ciphertext and wire bodies.
type probeAdminRotationView struct {
	RotationID        string        `json:"rotation_id"`
	CredentialVersion probe.Decimal `json:"credential_version"`
	PreviousVersion   probe.Decimal `json:"previous_version"`
	PrepareCommandID  string        `json:"prepare_command_id"`
	ActivateCommandID string        `json:"activate_command_id"`
	CreatedAt         time.Time     `json:"created_at"`
	OverlapExpiresAt  time.Time     `json:"overlap_expires_at"`
	State             string        `json:"state"`
	PreparedAt        *time.Time    `json:"prepared_at"`
	ActivatedAt       *time.Time    `json:"activated_at"`
	FailureCode       string        `json:"failure_code,omitempty"`
}

func probeAdminRotation(r *domain.ProbeCredentialRotation) *probeAdminRotationView {
	return &probeAdminRotationView{RotationID: r.RotationID, CredentialVersion: probe.Decimal(r.Candidate.CredentialVersion), PreviousVersion: probe.Decimal(r.PreviousVersion), PrepareCommandID: r.PrepareCommandID, ActivateCommandID: r.ActivateCommandID, CreatedAt: r.CreatedAt, OverlapExpiresAt: r.OverlapExpiresAt, State: r.State, PreparedAt: r.PreparedAt, ActivatedAt: r.ActivatedAt, FailureCode: r.FailureCode}
}
