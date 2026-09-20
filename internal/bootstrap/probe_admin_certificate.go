package bootstrap

import (
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// Explicit operator view: public metadata only, never credentials or key material.
type probeAdminCertificateRotationView struct {
	RotationID          string        `json:"rotation_id"`
	CertificateVersion  probe.Decimal `json:"certificate_version"`
	PreviousVersion     probe.Decimal `json:"previous_version"`
	ValidForDays        int           `json:"valid_for_days"`
	PrepareCommandID    string        `json:"prepare_command_id"`
	ActivateCommandID   string        `json:"activate_command_id"`
	CreatedAt           time.Time     `json:"created_at"`
	OverlapExpiresAt    time.Time     `json:"overlap_expires_at"`
	OverlapClosed       bool          `json:"overlap_closed"`
	State               string        `json:"state"`
	PreparedAt          *time.Time    `json:"prepared_at"`
	ActivatedAt         *time.Time    `json:"activated_at"`
	CertificateNotAfter *time.Time    `json:"certificate_not_after"`
	Fingerprint         string        `json:"fingerprint,omitempty"`
	FailureCode         string        `json:"failure_code,omitempty"`
}

func probeAdminCertificateRotation(r *domain.ProbeCertificateRotation) *probeAdminCertificateRotationView {
	return &probeAdminCertificateRotationView{RotationID: r.RotationID, CertificateVersion: probe.Decimal(r.CertificateVersion), PreviousVersion: probe.Decimal(r.PreviousVersion), ValidForDays: r.ValidForDays, PrepareCommandID: r.PrepareCommandID, ActivateCommandID: r.ActivateCommandID, CreatedAt: r.CreatedAt, OverlapExpiresAt: r.OverlapExpiresAt, OverlapClosed: r.OverlapClosed, State: r.State, PreparedAt: r.PreparedAt, ActivatedAt: r.ActivatedAt, CertificateNotAfter: r.CertificateNotAfter, Fingerprint: r.Fingerprint, FailureCode: r.FailureCode}
}
