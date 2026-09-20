package probe

import (
	"context"
	"encoding/json"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// StreamResetPlan is the closed, nonsecret stopped-operator transfer document.
type StreamResetPlan struct {
	SchemaVersion        int       `json:"schema_version"`
	ResetID              string    `json:"reset_id"`
	HubID                string    `json:"hub_id"`
	ProbeID              string    `json:"probe_id"`
	EnrollmentID         string    `json:"enrollment_id"`
	PreviousStreamID     string    `json:"previous_stream_id"`
	StreamID             string    `json:"stream_id"`
	Fingerprint          string    `json:"certificate_fingerprint"`
	CredentialVersion    Decimal   `json:"credential_version"`
	CertificateVersion   Decimal   `json:"certificate_version"`
	HubCommittedSeq      Decimal   `json:"hub_committed_seq"`
	ConnectionGeneration Decimal   `json:"connection_generation"`
	PreparedAt           Timestamp `json:"prepared_at"`
}

// StreamResetReceipt reports source commit, not authenticated hub confirmation.
// Unobserved work after restore is explicitly unknown; no gap range is invented.
type StreamResetReceipt struct {
	Plan                 StreamResetPlan `json:"plan"`
	State                string          `json:"state"`
	InitialStreamID      string          `json:"initial_stream_id"`
	InitialFingerprint   string          `json:"initial_fingerprint"`
	LastCreatedSeq       Decimal         `json:"source_last_created_seq"`
	CommittedSeq         Decimal         `json:"source_committed_seq"`
	ConnectionGeneration Decimal         `json:"source_connection_generation"`
	ConfigRevision       Decimal         `json:"source_config_revision"`
	ReservedAt           Timestamp       `json:"source_reserved_at"`
	AppliedAt            Timestamp       `json:"source_applied_at"`
	ArchiveBytes         Decimal         `json:"archive_bytes"`
	ArchiveSHA256        string          `json:"archive_sha256"`
	UnobservedCoverage   string          `json:"unobserved_coverage"`
}

// StreamResetPlanView constructs a metadata-only operator view.
func StreamResetPlanView(p domain.ProbeStreamResetPlan) StreamResetPlan {
	return StreamResetPlan{SchemaVersion: 1, ResetID: p.ResetID, HubID: p.HubID, ProbeID: p.ProbeID, EnrollmentID: p.EnrollmentID, PreviousStreamID: p.PreviousStreamID, StreamID: p.StreamID, Fingerprint: p.Fingerprint, CredentialVersion: Decimal(p.CredentialVersion), CertificateVersion: Decimal(p.CertificateVersion), HubCommittedSeq: Decimal(p.HubCommittedSeq), ConnectionGeneration: Decimal(p.ConnectionGeneration), PreparedAt: Timestamp(p.PreparedAt.UTC())}
}

func (p StreamResetPlan) domain() domain.ProbeStreamResetPlan {
	return domain.ProbeStreamResetPlan{ResetID: p.ResetID, HubID: p.HubID, ProbeID: p.ProbeID, EnrollmentID: p.EnrollmentID, PreviousStreamID: p.PreviousStreamID, StreamID: p.StreamID, Fingerprint: p.Fingerprint, CredentialVersion: int64(p.CredentialVersion), CertificateVersion: int64(p.CertificateVersion), HubCommittedSeq: int64(p.HubCommittedSeq), ConnectionGeneration: int64(p.ConnectionGeneration), PreparedAt: time.Time(p.PreparedAt).UTC()}
}

// StreamResetReceiptView constructs a source-only application receipt.
func StreamResetReceiptView(r domain.EdgeStreamResetRecord) StreamResetReceipt {
	return StreamResetReceipt{Plan: StreamResetPlanView(r.Plan), State: "source_applied", InitialStreamID: r.InitialStreamID, InitialFingerprint: r.Source.Fingerprint, LastCreatedSeq: Decimal(r.Source.LastCreatedSeq), CommittedSeq: Decimal(r.Source.CommittedSeq), ConnectionGeneration: Decimal(r.Source.ConnectionGeneration), ConfigRevision: Decimal(r.Source.ConfigRevision), ReservedAt: Timestamp(r.ReservedAt.UTC()), AppliedAt: Timestamp(r.AppliedAt.UTC()), ArchiveBytes: Decimal(r.ArchiveBytes), ArchiveSHA256: r.ArchiveSHA256, UnobservedCoverage: "unknown"}
}

// StreamResetCodec validates operator metadata without performing recovery.
type StreamResetCodec struct{}

var _ ports.ProbeStreamResetCodec = StreamResetCodec{}

// EncodeStreamResetPlan writes a valid exact-identity operator plan.
func (StreamResetCodec) EncodeStreamResetPlan(ctx context.Context, p domain.ProbeStreamResetPlan) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !domain.ValidProbeStreamResetPlan(p) {
		return nil, domain.ErrValidation
	}
	return json.Marshal(StreamResetPlanView(p))
}

// DecodeStreamResetPlan rejects missing, duplicate, unknown or ambiguous fields.
func (StreamResetCodec) DecodeStreamResetPlan(ctx context.Context, data []byte) (domain.ProbeStreamResetPlan, error) {
	if err := ctx.Err(); err != nil {
		return domain.ProbeStreamResetPlan{}, err
	}
	if len(data) == 0 || len(data) > 8192 {
		return domain.ProbeStreamResetPlan{}, domain.ErrValidation
	}
	fields, err := decodeJSONObject(data)
	if err != nil {
		return domain.ProbeStreamResetPlan{}, domain.ErrValidation
	}
	var p StreamResetPlan
	if err := resetFields(fields, field{"schema_version", &p.SchemaVersion}, field{"reset_id", &p.ResetID}, field{"hub_id", &p.HubID}, field{"probe_id", &p.ProbeID}, field{"enrollment_id", &p.EnrollmentID}, field{"previous_stream_id", &p.PreviousStreamID}, field{"stream_id", &p.StreamID}, field{"certificate_fingerprint", &p.Fingerprint}, field{"credential_version", &p.CredentialVersion}, field{"certificate_version", &p.CertificateVersion}, field{"hub_committed_seq", &p.HubCommittedSeq}, field{"connection_generation", &p.ConnectionGeneration}, field{"prepared_at", &p.PreparedAt}); err != nil {
		return domain.ProbeStreamResetPlan{}, domain.ErrValidation
	}
	d := p.domain()
	if p.SchemaVersion != 1 || !domain.ValidProbeStreamResetPlan(d) {
		return domain.ProbeStreamResetPlan{}, domain.ErrValidation
	}
	return d, nil
}

// EncodeStreamResetReceipt preserves actual bounds without claiming hub completion.
func (StreamResetCodec) EncodeStreamResetReceipt(ctx context.Context, r domain.EdgeStreamResetRecord) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !domain.ValidEdgeStreamResetRecord(r) || r.State != "applied" {
		return nil, domain.ErrValidation
	}
	return json.Marshal(StreamResetReceiptView(r))
}

// DecodeStreamResetReceipt validates the supplied source statement, not its truth.
func (codec StreamResetCodec) DecodeStreamResetReceipt(ctx context.Context, data []byte) (domain.EdgeStreamResetRecord, error) {
	if err := ctx.Err(); err != nil {
		return domain.EdgeStreamResetRecord{}, err
	}
	if len(data) == 0 || len(data) > 8192 {
		return domain.EdgeStreamResetRecord{}, domain.ErrValidation
	}
	fields, err := decodeJSONObject(data)
	if err != nil {
		return domain.EdgeStreamResetRecord{}, domain.ErrValidation
	}
	var v StreamResetReceipt
	var plan json.RawMessage
	if err := resetFields(fields, field{"plan", &plan}, field{"state", &v.State}, field{"initial_stream_id", &v.InitialStreamID}, field{"initial_fingerprint", &v.InitialFingerprint}, field{"source_last_created_seq", &v.LastCreatedSeq}, field{"source_committed_seq", &v.CommittedSeq}, field{"source_connection_generation", &v.ConnectionGeneration}, field{"source_config_revision", &v.ConfigRevision}, field{"source_reserved_at", &v.ReservedAt}, field{"source_applied_at", &v.AppliedAt}, field{"archive_bytes", &v.ArchiveBytes}, field{"archive_sha256", &v.ArchiveSHA256}, field{"unobserved_coverage", &v.UnobservedCoverage}); err != nil {
		return domain.EdgeStreamResetRecord{}, domain.ErrValidation
	}
	p, err := codec.DecodeStreamResetPlan(ctx, plan)
	if err != nil {
		return domain.EdgeStreamResetRecord{}, err
	}
	r := domain.EdgeStreamResetRecord{Plan: p, InitialStreamID: v.InitialStreamID, Source: domain.EdgeIdentity{ProbeID: p.ProbeID, HubID: p.HubID, StreamID: p.PreviousStreamID, Fingerprint: v.InitialFingerprint, LastCreatedSeq: int64(v.LastCreatedSeq), CommittedSeq: int64(v.CommittedSeq), ConnectionGeneration: int64(v.ConnectionGeneration), ConfigRevision: int64(v.ConfigRevision)}, State: "applied", ReservedAt: time.Time(v.ReservedAt).UTC(), AppliedAt: time.Time(v.AppliedAt).UTC(), ArchiveBytes: int64(v.ArchiveBytes), ArchiveSHA256: v.ArchiveSHA256}
	if v.State != "source_applied" || v.UnobservedCoverage != "unknown" || !domain.ValidEdgeStreamResetRecord(r) {
		return domain.EdgeStreamResetRecord{}, domain.ErrValidation
	}
	return r, nil
}

func resetFields(fields map[string]json.RawMessage, destinations ...field) error {
	names := make([]string, 0, len(destinations))
	for _, destination := range destinations {
		names = append(names, destination.name)
	}
	if err := rejectUnknownKeys(fields, names...); err != nil {
		return err
	}
	return decodeRequiredFields(fields, destinations...)
}
