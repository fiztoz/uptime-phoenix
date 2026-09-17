package probe

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

const localProbeID = "local"

// HTTP and browser DTOs for PROTOCOL.md sections 7–8. Decoding does not
// register routes, emit WebSocket events, or authorize a caller.

// ProbeView is the admin fleet/detail shape. Runtime tokens are never present.
type ProbeView struct {
	ID                    string     `json:"id"`
	Key                   string     `json:"key"`
	Name                  string     `json:"name"`
	Location              string     `json:"location"`
	Kind                  string     `json:"kind"`
	Enabled               bool       `json:"enabled"`
	EnrollmentState       string     `json:"enrollment_state"`
	ConnectionStatus      string     `json:"connection_status"`
	ExecutionStatus       string     `json:"execution_status"`
	LastSeenAt            *Timestamp `json:"last_seen_at"`
	AgentVersion          *string    `json:"agent_version"`
	ProtocolVersion       *int       `json:"protocol_version"`
	DesiredConfigRevision Decimal    `json:"desired_config_revision"`
	AppliedConfigRevision Decimal    `json:"applied_config_revision"`
	QueueBytes            *int64     `json:"queue_bytes"`
	OldestQueuedAt        *Timestamp `json:"oldest_queued_at"`
	Revision              Decimal    `json:"revision"`
	CreatedAt             Timestamp  `json:"created_at"`
	UpdatedAt             Timestamp  `json:"updated_at"`
	Endpoint              *string    `json:"endpoint"`
	TLSFingerprint        *string    `json:"tls_fingerprint"`
	CertificateExpiresAt  *Timestamp `json:"certificate_expires_at"`
	CredentialVersion     *Decimal   `json:"credential_version"`
	Capabilities          []string   `json:"capabilities"`
}

// ProbeList is the paginated admin fleet response.
type ProbeList struct {
	Items      []ProbeView `json:"items"`
	NextCursor *string     `json:"next_cursor"`
}

// ProbeCreateRequest is the admin create-registration body.
type ProbeCreateRequest struct {
	Key            string `json:"key"`
	Name           string `json:"name"`
	Location       string `json:"location"`
	Endpoint       string `json:"endpoint"`
	TLSFingerprint string `json:"tls_fingerprint"`
}

// ProbePatchRequest updates display fields against an expected revision.
type ProbePatchRequest struct {
	Name     string  `json:"name"`
	Location string  `json:"location"`
	Enabled  bool    `json:"enabled"`
	Revision Decimal `json:"revision"`
}

// AssignmentReplacementRequest is the PUT /api/monitors/:id/probes body.
type AssignmentReplacementRequest struct {
	ExpectedRevision Decimal             `json:"expected_revision"`
	ProbeIDs         []string            `json:"probe_ids"`
	HealthPolicy     string              `json:"health_policy"`
	AlertDelivery    string              `json:"alert_delivery"`
	Bindings         []AssignmentBinding `json:"bindings"`
}

// AssignmentBinding maps a probe onto a declared local resource key.
type AssignmentBinding struct {
	ProbeID    string `json:"probe_id"`
	Kind       string `json:"kind"`
	BindingKey string `json:"binding_key"`
}

// AssignmentReplacementResult is the 200 body after a desired-state save.
type AssignmentReplacementResult struct {
	Revision      Decimal             `json:"revision"`
	HealthPolicy  string              `json:"health_policy"`
	AlertDelivery string              `json:"alert_delivery"`
	Assignments   []AssignmentSummary `json:"assignments"`
}

// AssignmentSummary is one authorized regional assignment without secrets.
type AssignmentSummary struct {
	ProbeID               string  `json:"probe_id"`
	Generation            Decimal `json:"generation"`
	DesiredConfigRevision Decimal `json:"desired_config_revision"`
	AppliedConfigRevision Decimal `json:"applied_config_revision"`
	SyncStatus            string  `json:"sync_status"`
	BindingKey            *string `json:"binding_key"`
}

// RegionalHeartbeat is the regional history row. It keeps message, not Msg.
type RegionalHeartbeat struct {
	ID                   int64     `json:"id"`
	MonitorID            int64     `json:"monitor_id"`
	Status               string    `json:"status"`
	Ping                 int       `json:"ping"`
	Message              string    `json:"message"`
	Time                 Timestamp `json:"time"`
	Important            bool      `json:"important"`
	ProbeID              string    `json:"probe_id"`
	ReceivedAt           Timestamp `json:"received_at"`
	AssignmentGeneration Decimal   `json:"assignment_generation"`
	ConfigRevision       Decimal   `json:"config_revision"`
}

// HealthView is the overall monitor health projection for authorized readers.
type HealthView struct {
	MonitorID          int64              `json:"monitor_id"`
	Status             string             `json:"status"`
	HealthPolicy       string             `json:"health_policy"`
	ProjectionVersion  Decimal            `json:"projection_version"`
	AsOf               Timestamp          `json:"as_of"`
	UptimePercent      *float64           `json:"uptime_percent"`
	CoveragePercent    *float64           `json:"coverage_percent"`
	KnownSeconds       int64              `json:"known_seconds"`
	UnknownSeconds     int64              `json:"unknown_seconds"`
	MaintenanceSeconds int64              `json:"maintenance_seconds"`
	ProbeCounts        ProbeCountView     `json:"probe_counts"`
	Regions            []HealthRegionView `json:"regions"`
}

// ProbeCountView partitions the assigned set, including paused members.
type ProbeCountView struct {
	Assigned    int `json:"assigned"`
	Up          int `json:"up"`
	Down        int `json:"down"`
	Pending     int `json:"pending"`
	Unknown     int `json:"unknown"`
	Maintenance int `json:"maintenance"`
	Paused      int `json:"paused"`
}

// HealthRegionView is one region's public health row. Fleet totals are omitted.
type HealthRegionView struct {
	ProbeID          string    `json:"probe_id"`
	Name             string    `json:"name"`
	Location         string    `json:"location"`
	Status           string    `json:"status"`
	ConnectionStatus string    `json:"connection_status"`
	ObservedAt       Timestamp `json:"observed_at"`
	ReceivedAt       Timestamp `json:"received_at"`
	FreshUntil       Timestamp `json:"fresh_until"`
	ConfigSyncStatus string    `json:"config_sync_status"`
	Reason           *string   `json:"reason"`
}

// BrowserEvent is a hub-to-browser WebSocket frame, not the probe transport.
type BrowserEvent struct {
	Type    string `json:"type"`
	Payload any    `json:"payload"`
}

// ProbeStatusEvent is the admin-safe fleet subset of ProbeView.
type ProbeStatusEvent struct {
	ID               string     `json:"id"`
	Key              string     `json:"key"`
	Name             string     `json:"name"`
	Location         string     `json:"location"`
	Kind             string     `json:"kind"`
	Enabled          bool       `json:"enabled"`
	EnrollmentState  string     `json:"enrollment_state"`
	ConnectionStatus string     `json:"connection_status"`
	ExecutionStatus  string     `json:"execution_status"`
	LastSeenAt       *Timestamp `json:"last_seen_at"`
	Revision         Decimal    `json:"revision"`
}

// MonitorProbeStatusEvent reports one region's freshness without fleet data.
type MonitorProbeStatusEvent struct {
	MonitorID        int64     `json:"monitor_id"`
	ProbeID          string    `json:"probe_id"`
	Status           string    `json:"status"`
	ConnectionStatus string    `json:"connection_status"`
	ObservedAt       Timestamp `json:"observed_at"`
	ReceivedAt       Timestamp `json:"received_at"`
	FreshUntil       Timestamp `json:"fresh_until"`
	Reason           *string   `json:"reason"`
}

// ProbeConfigStatusEvent reports sync state with redacted validation errors.
type ProbeConfigStatusEvent struct {
	ProbeID  string        `json:"probe_id"`
	Revision Decimal       `json:"revision"`
	Status   string        `json:"status"`
	Errors   []ConfigError `json:"errors"`
}

// DecodeProbeView validates an admin ProbeView. local is a valid hub identity.
func DecodeProbeView(data []byte) (ProbeView, error) {
	var view ProbeView
	if err := rejectSecretPayload(data); err != nil {
		return ProbeView{}, err
	}
	fields, err := decodeJSONObject(data)
	if err != nil {
		return ProbeView{}, err
	}
	if err := requiredProbeID(fields, "id", &view.ID); err != nil {
		return ProbeView{}, err
	}
	if err := decodeRequiredFields(fields,
		field{"key", &view.Key}, field{"name", &view.Name}, field{"location", &view.Location},
		field{"kind", &view.Kind}, field{"enabled", &view.Enabled},
		field{"enrollment_state", &view.EnrollmentState}, field{"connection_status", &view.ConnectionStatus},
		field{"execution_status", &view.ExecutionStatus},
		field{"desired_config_revision", &view.DesiredConfigRevision},
		field{"applied_config_revision", &view.AppliedConfigRevision},
		field{"revision", &view.Revision}, field{"created_at", &view.CreatedAt}, field{"updated_at", &view.UpdatedAt},
	); err != nil {
		return ProbeView{}, err
	}
	if err := nullable(fields, "last_seen_at", &view.LastSeenAt); err != nil {
		return ProbeView{}, err
	}
	if err := nullable(fields, "agent_version", &view.AgentVersion); err != nil {
		return ProbeView{}, err
	}
	if err := nullable(fields, "protocol_version", &view.ProtocolVersion); err != nil {
		return ProbeView{}, err
	}
	if err := nullable(fields, "queue_bytes", &view.QueueBytes); err != nil {
		return ProbeView{}, err
	}
	if err := nullable(fields, "oldest_queued_at", &view.OldestQueuedAt); err != nil {
		return ProbeView{}, err
	}
	if err := nullable(fields, "endpoint", &view.Endpoint); err != nil {
		return ProbeView{}, err
	}
	if err := nullable(fields, "tls_fingerprint", &view.TLSFingerprint); err != nil {
		return ProbeView{}, err
	}
	if err := nullable(fields, "certificate_expires_at", &view.CertificateExpiresAt); err != nil {
		return ProbeView{}, err
	}
	if err := nullable(fields, "credential_version", &view.CredentialVersion); err != nil {
		return ProbeView{}, err
	}
	if err := required(fields, "capabilities", &view.Capabilities); err != nil {
		return ProbeView{}, err
	}
	if view.Kind != "local" && view.Kind != "remote" {
		return ProbeView{}, errors.New("invalid probe kind")
	}
	if (view.ID == localProbeID) != (view.Kind == "local") || (view.Key == localProbeID) != (view.Kind == "local") {
		return ProbeView{}, errors.New("local identity and kind must agree")
	}
	if !validEnrollmentState(view.EnrollmentState) || !validConnectionStatus(view.ConnectionStatus) || !validExecutionStatus(view.ExecutionStatus) {
		return ProbeView{}, errors.New("invalid probe lifecycle status")
	}
	if view.Revision <= 0 {
		return ProbeView{}, errors.New("probe revision must be positive")
	}
	if view.TLSFingerprint != nil && !validSHA256(*view.TLSFingerprint) {
		return ProbeView{}, errors.New("invalid TLS fingerprint")
	}
	if view.Capabilities == nil {
		view.Capabilities = []string{}
	}
	if err := validateCapabilities(view.Capabilities); err != nil {
		return ProbeView{}, err
	}
	return view, nil
}

// DecodeProbeList validates a paginated fleet list.
func DecodeProbeList(data []byte) (ProbeList, error) {
	var list ProbeList
	if err := rejectSecretPayload(data); err != nil {
		return ProbeList{}, err
	}
	fields, err := decodeJSONObject(data)
	if err != nil {
		return ProbeList{}, err
	}
	if err := rejectUnknownKeys(fields, "items", "next_cursor"); err != nil {
		return ProbeList{}, err
	}
	var rawItems []json.RawMessage
	if err := required(fields, "items", &rawItems); err != nil {
		return ProbeList{}, err
	}
	if err := nullable(fields, "next_cursor", &list.NextCursor); err != nil {
		return ProbeList{}, err
	}
	list.Items = make([]ProbeView, 0, len(rawItems))
	for _, raw := range rawItems {
		item, err := DecodeProbeView(raw)
		if err != nil {
			return ProbeList{}, err
		}
		list.Items = append(list.Items, item)
	}
	return list, nil
}

// DecodeProbeCreateRequest validates create-registration input.
func DecodeProbeCreateRequest(data []byte) (ProbeCreateRequest, error) {
	var request ProbeCreateRequest
	fields, err := decodeJSONObject(data)
	if err != nil {
		return ProbeCreateRequest{}, err
	}
	if err := rejectUnknownKeys(fields, "key", "name", "location", "endpoint", "tls_fingerprint"); err != nil {
		return ProbeCreateRequest{}, err
	}
	if err := decodeRequiredFields(fields,
		field{"key", &request.Key}, field{"name", &request.Name}, field{"location", &request.Location},
		field{"endpoint", &request.Endpoint}, field{"tls_fingerprint", &request.TLSFingerprint},
	); err != nil {
		return ProbeCreateRequest{}, err
	}
	if request.Key == localProbeID || strings.TrimSpace(request.Name) == "" || !validSHA256(request.TLSFingerprint) {
		return ProbeCreateRequest{}, errors.New("invalid probe create request")
	}
	return request, nil
}

// DecodeProbePatchRequest validates a revisioned display update.
func DecodeProbePatchRequest(data []byte) (ProbePatchRequest, error) {
	var request ProbePatchRequest
	fields, err := decodeJSONObject(data)
	if err != nil {
		return ProbePatchRequest{}, err
	}
	if err := rejectUnknownKeys(fields, "name", "location", "enabled", "revision"); err != nil {
		return ProbePatchRequest{}, err
	}
	if err := decodeRequiredFields(fields,
		field{"name", &request.Name}, field{"location", &request.Location},
		field{"enabled", &request.Enabled}, field{"revision", &request.Revision},
	); err != nil {
		return ProbePatchRequest{}, err
	}
	if strings.TrimSpace(request.Name) == "" || request.Revision <= 0 {
		return ProbePatchRequest{}, errors.New("invalid probe patch request")
	}
	return request, nil
}

// DecodeAssignmentReplacementRequest validates complete assignment replacement.
func DecodeAssignmentReplacementRequest(data []byte) (AssignmentReplacementRequest, error) {
	var request AssignmentReplacementRequest
	fields, err := decodeJSONObject(data)
	if err != nil {
		return AssignmentReplacementRequest{}, err
	}
	if err := decodeRequiredFields(fields,
		field{"expected_revision", &request.ExpectedRevision}, field{"probe_ids", &request.ProbeIDs},
		field{"health_policy", &request.HealthPolicy}, field{"alert_delivery", &request.AlertDelivery},
	); err != nil {
		return AssignmentReplacementRequest{}, err
	}
	if request.ExpectedRevision <= 0 || len(request.ProbeIDs) == 0 {
		return AssignmentReplacementRequest{}, errors.New("assignment replacement requires a positive revision and at least one probe")
	}
	if request.HealthPolicy != "any_down" && request.HealthPolicy != "all_down" {
		return AssignmentReplacementRequest{}, errors.New("invalid health policy")
	}
	if request.AlertDelivery != "regional" && request.AlertDelivery != "aggregate" && request.AlertDelivery != "both" {
		return AssignmentReplacementRequest{}, errors.New("invalid alert delivery")
	}
	seen := make(map[string]struct{}, len(request.ProbeIDs))
	for _, id := range request.ProbeIDs {
		if err := validHubProbeID(id); err != nil {
			return AssignmentReplacementRequest{}, err
		}
		if _, dup := seen[id]; dup {
			return AssignmentReplacementRequest{}, errors.New("duplicate probe id")
		}
		seen[id] = struct{}{}
	}
	if raw, ok := fields["bindings"]; ok {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return AssignmentReplacementRequest{}, errors.New("bindings must be an array when present")
		}
		var bindings []json.RawMessage
		if err := json.Unmarshal(raw, &bindings); err != nil {
			return AssignmentReplacementRequest{}, err
		}
		request.Bindings = make([]AssignmentBinding, 0, len(bindings))
		for _, rawBinding := range bindings {
			binding, err := decodeAssignmentBinding(rawBinding)
			if err != nil {
				return AssignmentReplacementRequest{}, err
			}
			request.Bindings = append(request.Bindings, binding)
		}
	}
	return request, nil
}

// DecodeAssignmentReplacementResult validates a desired-state save receipt.
func DecodeAssignmentReplacementResult(data []byte) (AssignmentReplacementResult, error) {
	var result AssignmentReplacementResult
	if err := rejectSecretPayload(data); err != nil {
		return AssignmentReplacementResult{}, err
	}
	fields, err := decodeJSONObject(data)
	if err != nil {
		return AssignmentReplacementResult{}, err
	}
	if err := decodeRequiredFields(fields,
		field{"revision", &result.Revision}, field{"health_policy", &result.HealthPolicy},
		field{"alert_delivery", &result.AlertDelivery},
	); err != nil {
		return AssignmentReplacementResult{}, err
	}
	if result.Revision <= 0 || (result.HealthPolicy != "any_down" && result.HealthPolicy != "all_down") {
		return AssignmentReplacementResult{}, errors.New("invalid assignment result policy or revision")
	}
	var raw []json.RawMessage
	if err := required(fields, "assignments", &raw); err != nil {
		return AssignmentReplacementResult{}, err
	}
	if len(raw) == 0 {
		return AssignmentReplacementResult{}, errors.New("assignment result requires members")
	}
	result.Assignments = make([]AssignmentSummary, 0, len(raw))
	for _, item := range raw {
		summary, err := decodeAssignmentSummary(item)
		if err != nil {
			return AssignmentReplacementResult{}, err
		}
		result.Assignments = append(result.Assignments, summary)
	}
	return result, nil
}

// DecodeRegionalHeartbeat validates a regional history row.
func DecodeRegionalHeartbeat(data []byte) (RegionalHeartbeat, error) {
	var row RegionalHeartbeat
	if err := rejectSecretPayload(data); err != nil {
		return RegionalHeartbeat{}, err
	}
	fields, err := decodeJSONObject(data)
	if err != nil {
		return RegionalHeartbeat{}, err
	}
	if err := decodeRequiredFields(fields,
		field{"id", &row.ID}, field{"monitor_id", &row.MonitorID}, field{"status", &row.Status},
		field{"ping", &row.Ping}, field{"message", &row.Message}, field{"time", &row.Time},
		field{"important", &row.Important}, field{"received_at", &row.ReceivedAt},
		field{"assignment_generation", &row.AssignmentGeneration}, field{"config_revision", &row.ConfigRevision},
	); err != nil {
		return RegionalHeartbeat{}, err
	}
	if err := requiredProbeID(fields, "probe_id", &row.ProbeID); err != nil {
		return RegionalHeartbeat{}, err
	}
	if row.ID <= 0 || row.MonitorID <= 0 || row.Ping < 0 || row.AssignmentGeneration <= 0 {
		return RegionalHeartbeat{}, errors.New("invalid regional heartbeat identity")
	}
	if !validHTTPStatus(row.Status) {
		return RegionalHeartbeat{}, errors.New("invalid regional heartbeat status")
	}
	return row, nil
}

// DecodeHealthView validates overall health without fleet secrets.
func DecodeHealthView(data []byte) (HealthView, error) {
	var view HealthView
	if err := rejectSecretPayload(data); err != nil {
		return HealthView{}, err
	}
	fields, err := decodeJSONObject(data)
	if err != nil {
		return HealthView{}, err
	}
	if err := decodeRequiredFields(fields,
		field{"monitor_id", &view.MonitorID}, field{"status", &view.Status},
		field{"health_policy", &view.HealthPolicy}, field{"projection_version", &view.ProjectionVersion},
		field{"as_of", &view.AsOf}, field{"known_seconds", &view.KnownSeconds},
		field{"unknown_seconds", &view.UnknownSeconds}, field{"maintenance_seconds", &view.MaintenanceSeconds},
	); err != nil {
		return HealthView{}, err
	}
	if err := nullable(fields, "uptime_percent", &view.UptimePercent); err != nil {
		return HealthView{}, err
	}
	if err := nullable(fields, "coverage_percent", &view.CoveragePercent); err != nil {
		return HealthView{}, err
	}
	var countsRaw json.RawMessage
	if err := required(fields, "probe_counts", &countsRaw); err != nil {
		return HealthView{}, err
	}
	if err := json.Unmarshal(countsRaw, &view.ProbeCounts); err != nil {
		return HealthView{}, err
	}
	if view.MonitorID <= 0 || view.ProjectionVersion <= 0 || !validHTTPStatus(view.Status) {
		return HealthView{}, errors.New("invalid health view identity")
	}
	if view.HealthPolicy != "any_down" && view.HealthPolicy != "all_down" {
		return HealthView{}, errors.New("invalid health policy")
	}
	if view.KnownSeconds < 0 || view.UnknownSeconds < 0 || view.MaintenanceSeconds < 0 {
		return HealthView{}, errors.New("health durations must be nonnegative")
	}
	if view.ProbeCounts.Assigned < 1 {
		return HealthView{}, errors.New("health view requires a nonempty assignment set")
	}
	var regions []json.RawMessage
	if err := required(fields, "regions", &regions); err != nil {
		return HealthView{}, err
	}
	view.Regions = make([]HealthRegionView, 0, len(regions))
	for _, raw := range regions {
		region, err := decodeHealthRegion(raw)
		if err != nil {
			return HealthView{}, err
		}
		view.Regions = append(view.Regions, region)
	}
	return view, nil
}

// DecodeBrowserEvent validates a hub browser WebSocket event envelope.
func DecodeBrowserEvent(data []byte) (BrowserEvent, error) {
	var event BrowserEvent
	if err := rejectSecretPayload(data); err != nil {
		return BrowserEvent{}, err
	}
	fields, err := decodeJSONObject(data)
	if err != nil {
		return BrowserEvent{}, err
	}
	if err := required(fields, "type", &event.Type); err != nil {
		return BrowserEvent{}, err
	}
	payload, ok := fields["payload"]
	if !ok || bytes.Equal(bytes.TrimSpace(payload), []byte("null")) {
		return BrowserEvent{}, errors.New("payload is required and cannot be null")
	}
	var decoded any
	switch event.Type {
	case "probe.status":
		decoded, err = decodeProbeStatusEvent(payload)
	case "monitor.probe.heartbeat":
		decoded, err = DecodeRegionalHeartbeat(payload)
	case "monitor.probe.status":
		decoded, err = decodeMonitorProbeStatusEvent(payload)
	case "monitor.health":
		decoded, err = DecodeHealthView(payload)
	case "probe.config.status":
		decoded, err = decodeProbeConfigStatusEvent(payload)
	case "probe.command.status":
		decoded, err = DecodeCommandReceipt(payload)
	default:
		return BrowserEvent{}, ErrUnsupportedPayload
	}
	if err != nil {
		return BrowserEvent{}, err
	}
	event.Payload = decoded
	return event, nil
}

func decodeProbeStatusEvent(data []byte) (ProbeStatusEvent, error) {
	var event ProbeStatusEvent
	fields, err := objectFields(data)
	if err != nil {
		return ProbeStatusEvent{}, err
	}
	if err := requiredProbeID(fields, "id", &event.ID); err != nil {
		return ProbeStatusEvent{}, err
	}
	if err := decodeRequiredFields(fields,
		field{"key", &event.Key}, field{"name", &event.Name}, field{"location", &event.Location},
		field{"kind", &event.Kind}, field{"enabled", &event.Enabled},
		field{"enrollment_state", &event.EnrollmentState}, field{"connection_status", &event.ConnectionStatus},
		field{"execution_status", &event.ExecutionStatus}, field{"revision", &event.Revision},
	); err != nil {
		return ProbeStatusEvent{}, err
	}
	if err := nullable(fields, "last_seen_at", &event.LastSeenAt); err != nil {
		return ProbeStatusEvent{}, err
	}
	if event.Kind != "local" && event.Kind != "remote" || !validEnrollmentState(event.EnrollmentState) || !validConnectionStatus(event.ConnectionStatus) || !validExecutionStatus(event.ExecutionStatus) || event.Revision <= 0 {
		return ProbeStatusEvent{}, errors.New("invalid probe status event")
	}
	return event, nil
}

func decodeMonitorProbeStatusEvent(data []byte) (MonitorProbeStatusEvent, error) {
	var event MonitorProbeStatusEvent
	fields, err := objectFields(data)
	if err != nil {
		return MonitorProbeStatusEvent{}, err
	}
	if err := decodeRequiredFields(fields,
		field{"monitor_id", &event.MonitorID}, field{"status", &event.Status},
		field{"connection_status", &event.ConnectionStatus}, field{"observed_at", &event.ObservedAt},
		field{"received_at", &event.ReceivedAt}, field{"fresh_until", &event.FreshUntil},
	); err != nil {
		return MonitorProbeStatusEvent{}, err
	}
	if err := requiredProbeID(fields, "probe_id", &event.ProbeID); err != nil {
		return MonitorProbeStatusEvent{}, err
	}
	if err := nullable(fields, "reason", &event.Reason); err != nil {
		return MonitorProbeStatusEvent{}, err
	}
	if event.MonitorID <= 0 || !validHTTPStatus(event.Status) || !validConnectionStatus(event.ConnectionStatus) {
		return MonitorProbeStatusEvent{}, errors.New("invalid monitor probe status event")
	}
	return event, nil
}

func decodeProbeConfigStatusEvent(data []byte) (ProbeConfigStatusEvent, error) {
	var event ProbeConfigStatusEvent
	fields, err := objectFields(data)
	if err != nil {
		return ProbeConfigStatusEvent{}, err
	}
	if err := requiredProbeID(fields, "probe_id", &event.ProbeID); err != nil {
		return ProbeConfigStatusEvent{}, err
	}
	if err := decodeRequiredFields(fields, field{"revision", &event.Revision}, field{"status", &event.Status}); err != nil {
		return ProbeConfigStatusEvent{}, err
	}
	var raw []json.RawMessage
	if err := required(fields, "errors", &raw); err != nil {
		return ProbeConfigStatusEvent{}, err
	}
	if event.Revision <= 0 || (event.Status != "pending" && event.Status != "applied" && event.Status != "rejected") {
		return ProbeConfigStatusEvent{}, errors.New("invalid probe config status")
	}
	event.Errors = make([]ConfigError, 0, len(raw))
	for _, item := range raw {
		detail, err := decodeConfigError(item)
		if err != nil {
			return ProbeConfigStatusEvent{}, err
		}
		event.Errors = append(event.Errors, detail)
	}
	return event, nil
}

func decodeAssignmentBinding(data []byte) (AssignmentBinding, error) {
	var binding AssignmentBinding
	fields, err := objectFields(data)
	if err != nil {
		return AssignmentBinding{}, err
	}
	if err := rejectUnknownKeys(fields, "probe_id", "kind", "binding_key"); err != nil {
		return AssignmentBinding{}, err
	}
	if err := requiredProbeID(fields, "probe_id", &binding.ProbeID); err != nil {
		return AssignmentBinding{}, err
	}
	if err := decodeRequiredFields(fields, field{"kind", &binding.Kind}, field{"binding_key", &binding.BindingKey}); err != nil {
		return AssignmentBinding{}, err
	}
	if (binding.Kind != "docker_socket" && binding.Kind != "docker_api") || !validBindingKey(binding.BindingKey) {
		return AssignmentBinding{}, errors.New("invalid assignment binding")
	}
	return binding, nil
}

func decodeAssignmentSummary(data []byte) (AssignmentSummary, error) {
	var summary AssignmentSummary
	fields, err := objectFields(data)
	if err != nil {
		return AssignmentSummary{}, err
	}
	if err := requiredProbeID(fields, "probe_id", &summary.ProbeID); err != nil {
		return AssignmentSummary{}, err
	}
	if err := decodeRequiredFields(fields,
		field{"generation", &summary.Generation}, field{"desired_config_revision", &summary.DesiredConfigRevision},
		field{"applied_config_revision", &summary.AppliedConfigRevision}, field{"sync_status", &summary.SyncStatus},
	); err != nil {
		return AssignmentSummary{}, err
	}
	if err := nullable(fields, "binding_key", &summary.BindingKey); err != nil {
		return AssignmentSummary{}, err
	}
	if summary.Generation <= 0 || (summary.SyncStatus != "pending" && summary.SyncStatus != "applied" && summary.SyncStatus != "rejected") {
		return AssignmentSummary{}, errors.New("invalid assignment summary")
	}
	if summary.BindingKey != nil && !validBindingKey(*summary.BindingKey) {
		return AssignmentSummary{}, errors.New("invalid assignment binding key")
	}
	return summary, nil
}

func decodeHealthRegion(data []byte) (HealthRegionView, error) {
	var region HealthRegionView
	fields, err := objectFields(data)
	if err != nil {
		return HealthRegionView{}, err
	}
	if err := requiredProbeID(fields, "probe_id", &region.ProbeID); err != nil {
		return HealthRegionView{}, err
	}
	if err := decodeRequiredFields(fields,
		field{"name", &region.Name}, field{"location", &region.Location}, field{"status", &region.Status},
		field{"connection_status", &region.ConnectionStatus}, field{"observed_at", &region.ObservedAt},
		field{"received_at", &region.ReceivedAt}, field{"fresh_until", &region.FreshUntil},
		field{"config_sync_status", &region.ConfigSyncStatus},
	); err != nil {
		return HealthRegionView{}, err
	}
	if err := nullable(fields, "reason", &region.Reason); err != nil {
		return HealthRegionView{}, err
	}
	if !validHTTPStatus(region.Status) || !validConnectionStatus(region.ConnectionStatus) || (region.ConfigSyncStatus != "pending" && region.ConfigSyncStatus != "applied" && region.ConfigSyncStatus != "rejected") {
		return HealthRegionView{}, errors.New("invalid health region")
	}
	return region, nil
}

func requiredProbeID(fields map[string]json.RawMessage, name string, target *string) error {
	if err := required(fields, name, target); err != nil {
		return err
	}
	return validHubProbeID(*target)
}

func validHubProbeID(id string) error {
	if id == localProbeID {
		return nil
	}
	return canonicalUUID("probe_id", id)
}

func validEnrollmentState(value string) bool {
	switch value {
	case "unconfigured", "pending", "active", "failed":
		return true
	default:
		return false
	}
}

func validConnectionStatus(value string) bool {
	switch value {
	case "never_connected", "online", "suspect", "disconnected", "revoked":
		return true
	default:
		return false
	}
}

func validExecutionStatus(value string) bool {
	switch value {
	case "unconfigured", "ready", "degraded", "paused", "revoked":
		return true
	default:
		return false
	}
}

func validHTTPStatus(value string) bool {
	switch value {
	case "up", "down", "pending", "maintenance", "unknown":
		return true
	default:
		return false
	}
}
