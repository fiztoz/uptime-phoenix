package repository

import (
	"context"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// tlsEvidenceModel is an explicit storage DTO; domain structs are never serialized.
type tlsEvidenceModel struct {
	NotAfter      time.Time `json:"not_after"`
	DaysRemaining int64     `json:"days_remaining"`
	Issuer        string    `json:"issuer"`
}

func tlsModel(evidence *domain.TLSObservation) *tlsEvidenceModel {
	if evidence == nil {
		return nil
	}
	return &tlsEvidenceModel{NotAfter: evidence.NotAfter.UTC(), DaysRemaining: evidence.DaysRemaining, Issuer: evidence.Issuer}
}

func (m *tlsEvidenceModel) evidence() *domain.TLSObservation {
	if m == nil {
		return nil
	}
	return &domain.TLSObservation{NotAfter: m.NotAfter.UTC(), DaysRemaining: m.DaysRemaining, Issuer: m.Issuer}
}

// replaceRemoteTLSInfo runs only after current-state ordering and authority have
// succeeded, inside the same transaction. No source alert cursor is manufactured.
func replaceRemoteTLSInfo(ctx context.Context, tx bun.Tx, state domain.RegionalState) error {
	if state.ProbeID == domain.LocalProbeID {
		return nil
	}
	if state.TLS == nil {
		_, err := tx.NewDelete().Model((*TLSInfoModel)(nil)).Where("monitor_id = ? AND probe_id = ? AND assignment_generation = ?", state.MonitorID, state.ProbeID, state.AssignmentGeneration).Exec(ctx)
		return err
	}
	tls := state.TLS
	row := &TLSInfoModel{MonitorID: state.MonitorID, ProbeID: state.ProbeID, AssignmentGeneration: state.AssignmentGeneration, CheckedAt: state.ObservedAt.UTC(), InfoJSON: JSONField{"not_after": tls.NotAfter.UTC().Format(time.RFC3339Nano), "days_remaining": tls.DaysRemaining, "issuer": tls.Issuer}}
	q := tx.NewInsert().Model(row)
	if tx.Dialect().Name() == dialect.MySQL {
		q = q.On("DUPLICATE KEY UPDATE").Set("info_json = VALUES(info_json)").Set("checked_at = VALUES(checked_at)")
	} else {
		q = q.On("CONFLICT (monitor_id, probe_id, assignment_generation) DO UPDATE").Set("info_json = EXCLUDED.info_json").Set("checked_at = EXCLUDED.checked_at")
	}
	_, err := q.Exec(ctx)
	return err
}
