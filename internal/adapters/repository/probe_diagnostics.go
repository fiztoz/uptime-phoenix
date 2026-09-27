package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// probeDiagConnectionModel reads enrollment evidence only. The protected
// credential column is deliberately never selected: a diagnostic read cannot
// disclose what it does not load.
type probeDiagConnectionModel struct {
	bun.BaseModel       `bun:"table:probe_connections,alias:pdc"`
	ProbeID             string `bun:"probe_id"`
	Endpoint            string `bun:"endpoint"`
	TLSPin              string `bun:"fingerprint"`
	CredentialVersion   int64  `bun:"credential_version"`
	CertificateVersion  int64  `bun:"certificate_version"`
	CertificateNotAfter *time.Time
	State               string     `bun:"state"`
	PreparedAt          time.Time  `bun:"prepared_at"`
	ActivatedAt         *time.Time `bun:"activated_at"`
}

func (m probeDiagConnectionModel) facts() *domain.ProbeEnrollmentFacts {
	facts := &domain.ProbeEnrollmentFacts{
		Endpoint: m.Endpoint, TLSPin: m.TLSPin,
		CredentialVersion: m.CredentialVersion, CertificateVersion: m.CertificateVersion,
		State: m.State, PreparedAt: m.PreparedAt.UTC(),
	}
	if m.CertificateNotAfter != nil {
		at := m.CertificateNotAfter.UTC()
		facts.CertificateNotAfter = &at
	}
	if m.ActivatedAt != nil {
		at := m.ActivatedAt.UTC()
		facts.ActivatedAt = &at
	}
	return facts
}

// probeDiagSessionModel reads connector (transport) session evidence.
type probeDiagSessionModel struct {
	bun.BaseModel `bun:"table:probe_sessions,alias:pds"`
	ProbeID       string `bun:"probe_id"`
	OwnerID       string `bun:"owner_id"`
	Generation    int64  `bun:"generation"`
	LeaseUntil    int64  `bun:"lease_until"`
	Connected     bool   `bun:"connected"`
}

func (m probeDiagSessionModel) facts() *domain.ProbeSessionFacts {
	return &domain.ProbeSessionFacts{
		OwnerID: m.OwnerID, Generation: m.Generation,
		LeaseUntil: diagnosticLeaseTime(m.LeaseUntil), Connected: m.Connected,
	}
}

// probeDiagRuntimeModel reads runtime (execution) lease evidence.
type probeDiagRuntimeModel struct {
	bun.BaseModel `bun:"table:probe_runtime_owners,alias:pdro"`
	ProbeID       string `bun:"probe_id"`
	OwnerID       string `bun:"owner_id"`
	Epoch         int64  `bun:"epoch"`
	LeaseUntil    int64  `bun:"lease_until"`
}

func (m probeDiagRuntimeModel) facts() *domain.ProbeRuntimeFacts {
	return &domain.ProbeRuntimeFacts{
		OwnerID: m.OwnerID, Epoch: m.Epoch,
		LeaseUntil: diagnosticLeaseTime(m.LeaseUntil),
	}
}

// diagnosticLeaseTime converts the stored unix-second deadline. Zero and
// released rows map to the zero time so unheld leases render as absent, never
// as a 1970 deadline.
func diagnosticLeaseTime(unixSeconds int64) time.Time {
	if unixSeconds <= 0 {
		return time.Time{}
	}
	return time.Unix(unixSeconds, 0).UTC()
}

// probeDiagWatchdogModel reads the connection watchdog checkpoint. The source
// incident identity is collapsed to presence: only IncidentOpen is disclosed.
type probeDiagWatchdogModel struct {
	bun.BaseModel  `bun:"table:probe_watchdog_state,alias:pdw"`
	ProbeID        string    `bun:"probe_id"`
	Version        int64     `bun:"version"`
	ConfigRevision int64     `bun:"config_revision"`
	Armed          bool      `bun:"armed"`
	LossElapsedNS  int64     `bun:"loss_elapsed_ns"`
	PendingLoss    bool      `bun:"pending_loss"`
	Status         string    `bun:"status"`
	IncidentSeq    int64     `bun:"incident_seq"`
	UpdatedAt      time.Time `bun:"updated_at"`
}

func (m probeDiagWatchdogModel) facts() *domain.ProbeWatchdogFacts {
	return &domain.ProbeWatchdogFacts{
		Status: m.Status, Version: m.Version, ConfigRevision: m.ConfigRevision,
		Armed: m.Armed, PendingLoss: m.PendingLoss, IncidentOpen: m.IncidentSeq > 0,
		UpdatedAt: m.UpdatedAt.UTC(),
	}
}

// probeDiagSnapshotModel reads prepared snapshot metadata. The protected
// payload column is deliberately never selected.
type probeDiagSnapshotModel struct {
	bun.BaseModel   `bun:"table:probe_config_snapshots,alias:pdcs"`
	ProbeID         string    `bun:"probe_id"`
	Revision        int64     `bun:"revision"`
	SchemaVersion   int       `bun:"schema_version"`
	SHA256          string    `bun:"sha256"`
	SourceCreatedAt time.Time `bun:"source_created_at"`
	EffectiveAt     time.Time `bun:"effective_at"`
	StoredAt        time.Time `bun:"stored_at"`
}

func (m probeDiagSnapshotModel) facts() *domain.ProbeConfigPublicationFacts {
	return &domain.ProbeConfigPublicationFacts{
		Revision: m.Revision, SchemaVersion: m.SchemaVersion, SHA256: m.SHA256,
		SourceCreatedAt: m.SourceCreatedAt.UTC(), EffectiveAt: m.EffectiveAt.UTC(), StoredAt: m.StoredAt.UTC(),
	}
}

// probeDiagActiveModel reads the durable application pointer.
type probeDiagActiveModel struct {
	bun.BaseModel   `bun:"table:probe_active_configs,alias:pdac"`
	ProbeID         string    `bun:"probe_id"`
	Revision        int64     `bun:"revision"`
	SHA256          string    `bun:"sha256"`
	AppliedAt       time.Time `bun:"applied_at"`
	AssignmentCount int       `bun:"assignment_count"`
}

func (m probeDiagActiveModel) facts() *domain.ProbeConfigAppliedFacts {
	return &domain.ProbeConfigAppliedFacts{
		Revision: m.Revision, SHA256: m.SHA256,
		AppliedAt: m.AppliedAt.UTC(), AssignmentCount: m.AssignmentCount,
	}
}

// ProbeDiagnosticsStore implements the nonsecret fleet read port on the shared
// dialect-neutral Bun layer, so one implementation serves MariaDB and SQLite
// with identical semantics. Each read runs in a single transaction, giving one
// coherent view across all six evidence sources on both engines.
type ProbeDiagnosticsStore struct{ db *bun.DB }

// NewProbeDiagnosticsStore creates a Bun-backed diagnostics reader.
func NewProbeDiagnosticsStore(db *bun.DB) *ProbeDiagnosticsStore {
	return &ProbeDiagnosticsStore{db: db}
}

var _ ports.ProbeDiagnosticsRepository = (*ProbeDiagnosticsStore)(nil)

// maxProbeDiagnosticsPage bounds a single store-level page independently of the
// HTTP-facing limit.
const maxProbeDiagnosticsPage = 500

// ListProbeDiagnostics returns up to limit registrations after the cursor in
// probe-ID order with each probe's coherent nonsecret diagnostic picture.
func (s *ProbeDiagnosticsStore) ListProbeDiagnostics(ctx context.Context, after string, limit int) ([]domain.ProbeDiagnostics, error) {
	if s == nil || s.db == nil || limit < 1 || limit > maxProbeDiagnosticsPage {
		return nil, domain.ErrValidation
	}
	if after != "" && !validDiagnosticProbeID(after) {
		return nil, domain.ErrValidation
	}
	var rows []domain.ProbeDiagnostics
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var registrations []probeRegistrationModel
		q := tx.NewSelect().Model(&registrations).OrderExpr("probe.id ASC").Limit(limit)
		if after != "" {
			q = q.Where("probe.id > ?", after)
		}
		if err := q.Scan(ctx); err != nil {
			return fmt.Errorf("list probe registrations: %w", err)
		}
		rows = make([]domain.ProbeDiagnostics, 0, len(registrations))
		for _, registration := range registrations {
			diagnostics, err := readProbeDiagnostics(ctx, tx, registration)
			if err != nil {
				return err
			}
			rows = append(rows, *diagnostics)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// GetProbeDiagnostics returns one probe's coherent nonsecret picture. A missing
// registration returns ErrNotFound; absent evidence sources stay nil.
func (s *ProbeDiagnosticsStore) GetProbeDiagnostics(ctx context.Context, probeID string) (*domain.ProbeDiagnostics, error) {
	if s == nil || s.db == nil || !validDiagnosticProbeID(probeID) {
		return nil, domain.ErrValidation
	}
	var result *domain.ProbeDiagnostics
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		registration := new(probeRegistrationModel)
		if err := tx.NewSelect().Model(registration).Where("probe.id = ?", probeID).Scan(ctx); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ports.ErrNotFound
			}
			return fmt.Errorf("read probe registration: %w", err)
		}
		diagnostics, err := readProbeDiagnostics(ctx, tx, *registration)
		if err != nil {
			return err
		}
		result = diagnostics
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// readProbeDiagnostics assembles one probe's picture inside the caller's
// transaction. Every query selects safe columns only and an absent row is
// unreported evidence, never an error.
func readProbeDiagnostics(ctx context.Context, tx bun.Tx, registration probeRegistrationModel) (*domain.ProbeDiagnostics, error) {
	probeID := registration.ID
	diagnostics := &domain.ProbeDiagnostics{Registration: registration.domain()}

	connection := new(probeDiagConnectionModel)
	err := tx.NewSelect().Model(connection).
		Column("probe_id", "endpoint", "fingerprint", "credential_version", "certificate_version", "certificate_not_after", "state", "prepared_at", "activated_at").
		Where("probe_id = ?", probeID).Scan(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read probe enrollment: %w", err)
	}
	if err == nil {
		diagnostics.Enrollment = connection.facts()
	}

	session := new(probeDiagSessionModel)
	err = tx.NewSelect().Model(session).Where("probe_id = ?", probeID).Scan(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read probe session: %w", err)
	}
	if err == nil {
		diagnostics.Session = session.facts()
	}

	runtime := new(probeDiagRuntimeModel)
	err = tx.NewSelect().Model(runtime).Where("probe_id = ?", probeID).Scan(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read probe runtime lease: %w", err)
	}
	if err == nil {
		diagnostics.Runtime = runtime.facts()
	}

	watchdog := new(probeDiagWatchdogModel)
	err = tx.NewSelect().Model(watchdog).
		Column("probe_id", "version", "config_revision", "armed", "loss_elapsed_ns", "pending_loss", "status", "incident_seq", "updated_at").
		Where("probe_id = ?", probeID).Scan(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read probe watchdog: %w", err)
	}
	if err == nil {
		diagnostics.Watchdog = watchdog.facts()
	}

	publication := new(probeDiagSnapshotModel)
	err = tx.NewSelect().Model(publication).
		Column("probe_id", "revision", "schema_version", "sha256", "source_created_at", "effective_at", "stored_at").
		Where("probe_id = ?", probeID).OrderExpr("revision DESC").Limit(1).Scan(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read probe config publication: %w", err)
	}
	if err == nil {
		diagnostics.Publication = publication.facts()
	}

	applied := new(probeDiagActiveModel)
	err = tx.NewSelect().Model(applied).Where("probe_id = ?", probeID).Scan(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read probe applied receipt: %w", err)
	}
	if err == nil {
		diagnostics.Applied = applied.facts()
	}
	return diagnostics, nil
}

// validDiagnosticProbeID accepts the reserved local identity and canonical
// probe UUIDs only.
func validDiagnosticProbeID(probeID string) bool {
	return probeID == domain.LocalProbeID || domain.ValidHubID(probeID)
}
