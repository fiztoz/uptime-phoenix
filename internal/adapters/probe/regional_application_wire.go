package probe

import (
	"encoding/json"
	"errors"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

const (
	// RegionalApplicationCapability advertises truthful reporting of already
	// persisted effective regional facts for one exact retained document. It
	// promises no aggregate/both application, no cutover evidence and no send,
	// suppression or lease authority.
	RegionalApplicationCapability = "availability.regional_application.v1"
	// MaxRegionalApplications bounds one delivery.regional_applied frame.
	MaxRegionalApplications = 256
)

// RegionalApplication reports one monitor's already-persisted effective
// regional facts inside one RegionalApplicationReport. Every field is explicit;
// nothing here is inferred from desired configuration.
type RegionalApplication struct {
	// MonitorID is the positive monitor identity inside the report's document.
	MonitorID int64 `json:"monitor_id"`
	// AssignmentRevision is the positive build-time assignment-set binding.
	AssignmentRevision Decimal `json:"assignment_revision"`
	// Generation is the positive execution generation of that assignment.
	Generation Decimal `json:"generation"`
	// AlertDelivery is explicit and strictly regional on this wire.
	AlertDelivery domain.AlertDelivery `json:"alert_delivery"`
}

// RegionalApplicationReport is the probe-to-hub delivery.regional_applied
// payload. It reports durable effective regional facts for one exact retained
// document; it is never desired configuration, an application receipt, or a
// cutover/send authority, and it carries no source clock or cursor.
type RegionalApplicationReport struct {
	SessionIdentity
	// Revision is the positive revision of the exact retained document.
	Revision Decimal `json:"revision"`
	// SHA256 is the exact retained document digest.
	SHA256 string `json:"sha256"`
	// Applications holds 1..MaxRegionalApplications unique monitor entries.
	Applications []RegionalApplication `json:"applications"`
}

// DecodeRegionalApplicationReport validates bounded frame shape, canonical
// scalars and unique membership, not authenticity. Unknown optional fields
// remain compatible with later revisions.
func DecodeRegionalApplicationReport(data []byte) (Envelope, RegionalApplicationReport, error) {
	var report RegionalApplicationReport
	envelope, fields, err := telemetryFields(data, "delivery.regional_applied")
	if err != nil {
		return envelope, report, err
	}
	if err := decodeSessionIdentity(fields, &report.SessionIdentity); err != nil {
		return envelope, report, err
	}
	if err := decodeRequiredFields(fields, field{"revision", &report.Revision}, field{"sha256", &report.SHA256}); err != nil {
		return envelope, report, err
	}
	if report.Revision <= 0 || !validSHA256(report.SHA256) {
		return envelope, report, errors.New("invalid regional application document identity")
	}
	var entries []json.RawMessage
	if err := required(fields, "applications", &entries); err != nil {
		return envelope, report, err
	}
	if len(entries) == 0 || len(entries) > MaxRegionalApplications {
		return envelope, report, errors.New("regional application batch exceeds bounds")
	}
	report.Applications = make([]RegionalApplication, 0, len(entries))
	seen := make(map[int64]struct{}, len(entries))
	for _, entry := range entries {
		application, err := decodeRegionalApplication(entry)
		if err != nil {
			return envelope, report, err
		}
		if _, duplicate := seen[application.MonitorID]; duplicate {
			return envelope, report, errors.New("duplicate regional application monitor")
		}
		seen[application.MonitorID] = struct{}{}
		report.Applications = append(report.Applications, application)
	}
	return envelope, report, nil
}

// decodeRegionalApplication strictly decodes one entry. Recognized fields are
// canonical and non-null; unknown optional fields remain compatible.
func decodeRegionalApplication(data []byte) (RegionalApplication, error) {
	var application RegionalApplication
	fields, err := objectFields(data)
	if err != nil {
		return application, err
	}
	if err := decodeRequiredFields(fields,
		field{"monitor_id", &application.MonitorID}, field{"assignment_revision", &application.AssignmentRevision},
		field{"generation", &application.Generation}, field{"alert_delivery", &application.AlertDelivery},
	); err != nil {
		return application, err
	}
	if application.MonitorID <= 0 || application.AssignmentRevision <= 0 || application.Generation <= 0 ||
		application.AlertDelivery != domain.AlertDeliveryRegional {
		return application, errors.New("invalid regional application entry")
	}
	return application, nil
}

// regionalApplicationReports converts durable facts into bounded wire batches
// bound to one session identity and one exact retained document. Mixed,
// duplicated, unbound or nonregional facts fail the whole conversion so no
// partial or synthesized report can leave the source. Empty facts report
// nothing.
func regionalApplicationReports(identity SessionIdentity, facts []domain.ProbeDeliveryModeApplication) ([]RegionalApplicationReport, error) {
	if err := canonicalUUID("hub_id", identity.HubID); err != nil {
		return nil, err
	}
	if err := canonicalUUID("probe_id", identity.ProbeID); err != nil {
		return nil, err
	}
	if err := canonicalUUID("stream_id", identity.StreamID); err != nil {
		return nil, err
	}
	if len(facts) == 0 {
		return nil, nil
	}
	target := domain.ProbeConfigTarget{HubID: identity.HubID, ProbeID: identity.ProbeID}
	seen := make(map[int64]struct{}, len(facts))
	for _, fact := range facts {
		if fact.ProbeConfigTarget != target || fact.StreamID != identity.StreamID ||
			fact.ConfigRevision != facts[0].ConfigRevision || fact.SHA256 != facts[0].SHA256 ||
			fact.AlertDelivery != domain.AlertDeliveryRegional || !domain.ValidProbeDeliveryModeApplication(fact) {
			return nil, errors.New("incoherent regional application facts")
		}
		if _, duplicate := seen[fact.MonitorID]; duplicate {
			return nil, errors.New("duplicate regional application fact")
		}
		seen[fact.MonitorID] = struct{}{}
	}
	reports := make([]RegionalApplicationReport, 0, (len(facts)+MaxRegionalApplications-1)/MaxRegionalApplications)
	for start := 0; start < len(facts); start += MaxRegionalApplications {
		end := min(start+MaxRegionalApplications, len(facts))
		report := RegionalApplicationReport{SessionIdentity: identity, Revision: Decimal(facts[0].ConfigRevision), SHA256: facts[0].SHA256, Applications: make([]RegionalApplication, 0, end-start)}
		for _, fact := range facts[start:end] {
			report.Applications = append(report.Applications, RegionalApplication{MonitorID: fact.MonitorID, AssignmentRevision: Decimal(fact.AssignmentRevision), Generation: Decimal(fact.Generation), AlertDelivery: domain.AlertDeliveryRegional})
		}
		reports = append(reports, report)
	}
	return reports, nil
}

// regionalApplicationFacts converts one decoded report into durable domain
// facts bound to the report's identity and retained document digest. This
// rechecks bounded shape only; negotiation, authenticity, lease authority and
// committed hub receipts remain with the caller and the repository.
func regionalApplicationFacts(report RegionalApplicationReport) ([]domain.ProbeDeliveryModeApplication, error) {
	facts := make([]domain.ProbeDeliveryModeApplication, 0, len(report.Applications))
	for _, application := range report.Applications {
		if application.AlertDelivery != domain.AlertDeliveryRegional {
			return nil, errors.New("nonregional application report entry")
		}
		fact := domain.ProbeDeliveryModeApplication{ProbeConfigTarget: domain.ProbeConfigTarget{HubID: report.HubID, ProbeID: report.ProbeID}, MonitorID: application.MonitorID, AssignmentRevision: int64(application.AssignmentRevision), Generation: int64(application.Generation), ConfigRevision: int64(report.Revision), SHA256: report.SHA256, StreamID: report.StreamID, AlertDelivery: application.AlertDelivery}
		if !domain.ValidProbeDeliveryModeApplication(fact) {
			return nil, errors.New("invalid regional application fact")
		}
		facts = append(facts, fact)
	}
	return facts, nil
}
