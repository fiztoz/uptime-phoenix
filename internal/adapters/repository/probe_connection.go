package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

var _ ports.ProbeConnectionRepository = (*ProbeConnectorStore)(nil)

type probeConnectionRow struct {
	bun.BaseModel        `bun:"table:probe_connections,alias:pc"`
	ProbeID              string `bun:",pk"`
	HubID                string
	StreamID             string
	EnrollmentID         string
	CredentialVersion    int64
	CredentialHighWater  int64
	CertificateVersion   int64 `bun:",nullzero,default:1"`
	CertificateHighWater int64 `bun:",nullzero,default:1"`
	CertificateNotAfter  *time.Time
	Endpoint             string
	Fingerprint          string
	ProtectedCredential  []byte
	State                string
	PreparedAt           time.Time
	ActivatedAt          *time.Time
}

func (r probeConnectionRow) connection() *domain.ProbeConnection {
	return &domain.ProbeConnection{ProbeCredentialMetadata: domain.ProbeCredentialMetadata{HubID: r.HubID, ProbeID: r.ProbeID, StreamID: r.StreamID, EnrollmentID: r.EnrollmentID, CredentialVersion: r.CredentialVersion, Endpoint: r.Endpoint, Fingerprint: r.Fingerprint}, CertificateVersion: r.CertificateVersion, CertificateNotAfter: utcTimePtr(r.CertificateNotAfter), ProtectedCredential: r.ProtectedCredential, State: r.State, PreparedAt: r.PreparedAt.UTC(), ActivatedAt: utcTimePtr(r.ActivatedAt)}
}

// PrepareConnection commits recoverable credentials and trusted stream identity
// before enrollment I/O. Matching retries reuse the already-prepared ciphertext.
func (s *ProbeConnectorStore) PrepareConnection(ctx context.Context, c domain.ProbeConnection) (*domain.ProbeConnection, error) {
	if !domain.ValidProbeCredentialMetadata(c.ProbeCredentialMetadata) || len(c.ProtectedCredential) < 30 || len(c.ProtectedCredential) > 300 || c.State != "prepared" || c.ActivatedAt != nil || c.PreparedAt.IsZero() {
		return nil, domain.ErrValidation
	}
	var out *domain.ProbeConnection
	err := s.transaction(ctx, c.ProbeID, func(ctx context.Context, tx bun.Tx, enabled bool, _ int64) error {
		if !enabled {
			return ports.ErrConflict
		}
		var hubID string
		if err := tx.NewRaw("SELECT hub_id FROM probe_installation WHERE id = 1").Scan(ctx, &hubID); err != nil {
			return err
		}
		if hubID != c.HubID {
			return ports.ErrConflict
		}
		var existing probeConnectionRow
		err := tx.NewSelect().Model(&existing).Where("probe_id = ?", c.ProbeID).Scan(ctx)
		if err == nil {
			out = existing.connection()
			if out.ProbeCredentialMetadata != c.ProbeCredentialMetadata {
				return ports.ErrConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var count int
		if err := tx.NewRaw("SELECT COUNT(*) FROM probe_connections").Scan(ctx, &count); err != nil {
			return err
		}
		if count >= 1000 {
			return domain.ErrValidation
		}
		if err := tx.NewRaw("SELECT COUNT(*) FROM probe_streams WHERE probe_id = ? OR stream_id = ?", c.ProbeID, c.StreamID).Scan(ctx, &count); err != nil {
			return err
		}
		if count != 0 {
			return ports.ErrConflict
		}
		row := probeConnectionRow{CertificateVersion: 1, CertificateHighWater: 1, HubID: c.HubID, ProbeID: c.ProbeID, StreamID: c.StreamID, EnrollmentID: c.EnrollmentID, CredentialVersion: c.CredentialVersion, Endpoint: c.Endpoint, Fingerprint: c.Fingerprint, ProtectedCredential: c.ProtectedCredential, State: c.State, PreparedAt: c.PreparedAt.UTC().Truncate(time.Microsecond)}
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO probe_streams (probe_id, stream_id, committed_seq, created_at, updated_at) VALUES (?, ?, 0, ?, ?)", c.ProbeID, c.StreamID, row.PreparedAt, row.PreparedAt); err != nil {
			return err
		}
		out = row.connection()
		return nil
	})
	if err != nil {
		return nil, probeConnectionError(ctx, err)
	}
	return out, nil
}

// GetConnection returns protected management material to an authorized service.
func (s *ProbeConnectorStore) GetConnection(ctx context.Context, probeID string) (*domain.ProbeConnection, error) {
	if !validRemoteProbeID(probeID) {
		return nil, domain.ErrValidation
	}
	var row probeConnectionRow
	if err := s.db.NewSelect().Model(&row).Where("probe_id = ?", probeID).Scan(ctx); err != nil {
		return nil, probeConnectionError(ctx, err)
	}
	return row.connection(), nil
}

// ListConnections returns at most the supported fleet bound for enabled probes.
func (s *ProbeConnectorStore) ListConnections(ctx context.Context) ([]domain.ProbeConnection, error) {
	var rows []probeConnectionRow
	err := s.db.NewSelect().Model(&rows).Join("JOIN probes AS p ON p.id = pc.probe_id").Where("p.enabled = ?", true).OrderExpr("pc.probe_id ASC").Limit(1001).Scan(ctx)
	if err != nil {
		return nil, probeConnectionError(ctx, err)
	}
	if len(rows) > 1000 {
		return nil, domain.ErrValidation
	}
	out := make([]domain.ProbeConnection, 0, len(rows))
	for _, row := range rows {
		out = append(out, *row.connection())
	}
	return out, nil
}

// ActivateConnection confirms a prepared identity without replacing its secret.
// Lost enrollment results recover by authenticating with this prepared credential.
func (s *ProbeConnectorStore) ActivateConnection(ctx context.Context, probeID, enrollmentID string, version int64, at time.Time) error {
	if !domain.ValidHubID(enrollmentID) || version <= 0 || at.IsZero() {
		return domain.ErrValidation
	}
	return probeConnectionError(ctx, s.transaction(ctx, probeID, func(ctx context.Context, tx bun.Tx, enabled bool, _ int64) error {
		if !enabled {
			return ports.ErrConflict
		}
		var row probeConnectionRow
		if err := tx.NewSelect().Model(&row).Where("probe_id = ?", probeID).Scan(ctx); err != nil {
			return err
		}
		if row.EnrollmentID != enrollmentID || row.CredentialVersion != version {
			return ports.ErrConflict
		}
		if row.State == "active" {
			return nil
		}
		_, err := tx.NewUpdate().Model(&row).Set("state = 'active'").Set("activated_at = ?", at.UTC().Truncate(time.Microsecond)).WherePK().Exec(ctx)
		return err
	}))
}

func probeConnectionError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ports.ErrNotFound) {
		return ports.ErrNotFound
	}
	for _, sentinel := range []error{ports.ErrConflict, domain.ErrValidation} {
		if errors.Is(err, sentinel) {
			return sentinel
		}
	}
	return errors.New("probe connection storage failed")
}

// GetConnectionCursor never invents an unknown or retired stream. Management
// sessions must match the explicitly prepared registration before negotiating.
func (s *ProbeConnectorStore) GetConnectionCursor(ctx context.Context, probeID, streamID string) (int64, error) {
	if !validRemoteProbeID(probeID) || !domain.ValidHubID(streamID) {
		return 0, domain.ErrValidation
	}
	var cursor int64
	err := s.db.NewRaw("SELECT s.committed_seq FROM probe_streams s JOIN probe_connections c ON c.probe_id = s.probe_id AND c.stream_id = s.stream_id WHERE s.probe_id = ? AND s.stream_id = ? AND s.retired_at IS NULL", probeID, streamID).Scan(ctx, &cursor)
	return cursor, probeConnectionError(ctx, err)
}
