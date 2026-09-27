package handlers

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type probeFleetReader interface {
	List(context.Context, string, int, time.Time) (*services.ProbeFleetPage, error)
	Detail(context.Context, string, time.Time) (*services.ProbeFleetEntry, error)
}

// ProbeFleetHandlers exposes the administrative fleet read contract from
// protocol section 7. Middleware has already resolved an admin principal; the
// service owns read composition and the DTOs below are the only objects
// serialized to clients. A nil service or disabled feature keeps the routes
// registered but unavailable with a typed 503.
type ProbeFleetHandlers struct {
	svc     probeFleetReader
	enabled bool
	now     func() time.Time
}

// NewProbeFleetHandlers builds handlers; disabled reads return a typed 503.
func NewProbeFleetHandlers(svc probeFleetReader, enabled bool) *ProbeFleetHandlers {
	return &ProbeFleetHandlers{svc: svc, enabled: enabled, now: time.Now}
}

// ProbeFleetPageView is the paginated administrative list envelope.
type ProbeFleetPageView struct {
	Items      []ProbeView `json:"items"`
	NextCursor *string     `json:"next_cursor"`
}

// ProbeView is the frozen protocol section 7 projection. Unknown values are
// null except what the M0 client contract requires in place: config revisions
// render the decimal string "0" when unreported, capabilities renders an empty
// array when nothing is advertised, and the three lifecycle statuses always
// carry a bounded vocabulary value. The fields below never carry credential or
// configuration material.
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
	LastSeenAt            *time.Time `json:"last_seen_at"`
	AgentVersion          *string    `json:"agent_version"`
	ProtocolVersion       *int64     `json:"protocol_version"`
	DesiredConfigRevision string     `json:"desired_config_revision"`
	AppliedConfigRevision string     `json:"applied_config_revision"`
	QueueBytes            *int64     `json:"queue_bytes"`
	OldestQueuedAt        *time.Time `json:"oldest_queued_at"`
	Revision              int64      `json:"revision,string"`
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
}

// ProbeDetailView is ProbeView plus the administrative detail fields reserved
// by protocol section 7 and the explicit safe diagnostics of the six evidence
// sources. Endpoint and TLS pin are network selection metadata disclosed to
// admins only; they grant no runtime authentication. Stream identities,
// credential bytes, key hashes and digests are never included.
type ProbeDetailView struct {
	ProbeView
	Endpoint             *string              `json:"endpoint"`
	TLSPin               *string              `json:"tls_fingerprint"`
	CertificateExpiresAt *time.Time           `json:"certificate_expires_at"`
	CredentialVersion    *string              `json:"credential_version"`
	Capabilities         []string             `json:"capabilities"`
	Diagnostics          ProbeDiagnosticsView `json:"diagnostics"`
}

// ProbeDiagnosticsView groups the nonsecret evidence sources. A null section
// means that source has never reported.
type ProbeDiagnosticsView struct {
	Enrollment *ProbeEnrollmentDiagnosticsView `json:"enrollment"`
	Connection *ProbeConnectionDiagnosticsView `json:"connection"`
	Runtime    *ProbeRuntimeDiagnosticsView    `json:"runtime"`
	Watchdog   *ProbeWatchdogDiagnosticsView   `json:"watchdog"`
	Config     ProbeConfigDiagnosticsView      `json:"config"`
}

// ProbeEnrollmentDiagnosticsView is stored enrollment evidence.
type ProbeEnrollmentDiagnosticsView struct {
	State               string     `json:"state"`
	CredentialVersion   string     `json:"credential_version"`
	CertificateVersion  string     `json:"certificate_version"`
	CertificateNotAfter *time.Time `json:"certificate_expires_at"`
	PreparedAt          time.Time  `json:"prepared_at"`
	ActivatedAt         *time.Time `json:"activated_at"`
}

// ProbeConnectionDiagnosticsView is connector (transport) session evidence.
// live is observational display state at the read instant, never lease
// authority for a write.
type ProbeConnectionDiagnosticsView struct {
	OwnerID    string     `json:"owner_id"`
	Generation string     `json:"generation"`
	LeaseUntil *time.Time `json:"lease_until"`
	Connected  bool       `json:"connected"`
	Live       bool       `json:"live"`
}

// ProbeRuntimeDiagnosticsView is runtime (execution) lease evidence.
type ProbeRuntimeDiagnosticsView struct {
	OwnerID    string     `json:"owner_id"`
	Epoch      string     `json:"epoch"`
	LeaseUntil *time.Time `json:"lease_until"`
	Live       bool       `json:"live"`
}

// ProbeWatchdogDiagnosticsView is the connection watchdog checkpoint. The
// source incident identity is collapsed to incident_open.
type ProbeWatchdogDiagnosticsView struct {
	Status         string    `json:"status"`
	Version        string    `json:"version"`
	ConfigRevision string    `json:"config_revision"`
	Armed          bool      `json:"armed"`
	PendingLoss    bool      `json:"pending_loss"`
	IncidentOpen   bool      `json:"incident_open"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// ProbeConfigDiagnosticsView separates publication from application. The
// sync_status vocabulary is pending/applied/rejected; null means unreported.
type ProbeConfigDiagnosticsView struct {
	Desired    *ProbeConfigPublicationView `json:"desired"`
	Applied    *ProbeConfigApplicationView `json:"applied"`
	SyncStatus *string                     `json:"sync_status"`
}

// ProbeConfigPublicationView is the latest prepared snapshot metadata.
type ProbeConfigPublicationView struct {
	Revision    string    `json:"revision"`
	EffectiveAt time.Time `json:"effective_at"`
}

// ProbeConfigApplicationView is the durable application receipt.
type ProbeConfigApplicationView struct {
	Revision        string    `json:"revision"`
	AppliedAt       time.Time `json:"applied_at"`
	AssignmentCount int       `json:"assignment_count"`
}

// List handles GET /api/probes with exclusive-cursor pagination.
func (h *ProbeFleetHandlers) List(c echo.Context) error {
	if ok := h.prepare(c); !ok {
		return nil
	}
	limit := services.DefaultProbeFleetPageLimit
	if raw, exists := c.QueryParams()["limit"]; exists {
		if len(raw) != 1 {
			return fleetError(c, http.StatusBadRequest, "invalid_limit", "limit must be an integer between 1 and 100")
		}
		parsed, err := strconv.Atoi(raw[0])
		if err != nil || parsed < 1 || parsed > services.MaxProbeFleetPageLimit {
			return fleetError(c, http.StatusBadRequest, "invalid_limit", "limit must be an integer between 1 and 100")
		}
		limit = parsed
	}
	cursor := ""
	if raw, exists := c.QueryParams()["cursor"]; exists {
		if len(raw) != 1 {
			return fleetError(c, http.StatusBadRequest, "invalid_cursor", "cursor must be a probe id")
		}
		cursor = raw[0]
	}
	page, err := h.svc.List(c.Request().Context(), cursor, limit, h.now().UTC())
	if err != nil {
		if errors.Is(err, domain.ErrValidation) {
			return fleetError(c, http.StatusBadRequest, "invalid_cursor", "cursor must be a probe id")
		}
		return fleetReadError(c, err)
	}
	out := ProbeFleetPageView{Items: make([]ProbeView, 0, len(page.Items)), NextCursor: page.NextCursor}
	for _, entry := range page.Items {
		out.Items = append(out.Items, toProbeView(entry))
	}
	return c.JSON(http.StatusOK, out)
}

// Detail handles GET /api/probes/:probe_id.
func (h *ProbeFleetHandlers) Detail(c echo.Context) error {
	if ok := h.prepare(c); !ok {
		return nil
	}
	probeID := c.Param("probe_id")
	entry, err := h.svc.Detail(c.Request().Context(), probeID, h.now().UTC())
	if err != nil {
		if errors.Is(err, domain.ErrValidation) {
			return fleetError(c, http.StatusBadRequest, "invalid_probe_id", "invalid probe id")
		}
		return fleetReadError(c, err)
	}
	return c.JSON(http.StatusOK, toProbeDetailView(*entry))
}

func (h *ProbeFleetHandlers) prepare(c echo.Context) bool {
	c.Response().Header().Set("Cache-Control", "no-store")
	userID, ok := userIDFromContext(c)
	if !ok || userID <= 0 {
		_ = fleetError(c, http.StatusUnauthorized, "unauthenticated", "authentication required")
		return false
	}
	if !h.enabled {
		_ = fleetError(c, http.StatusServiceUnavailable, "probes_disabled", "multi-region probes are disabled")
		return false
	}
	if h.svc == nil {
		_ = fleetError(c, http.StatusServiceUnavailable, "fleet_unavailable", "fleet data unavailable")
		return false
	}
	return true
}

func toProbeView(entry services.ProbeFleetEntry) ProbeView {
	registration := entry.Registration
	view := ProbeView{
		ID: registration.ID, Key: registration.Key, Name: registration.Name,
		Location: registration.Location, Kind: string(registration.Kind),
		Enabled: registration.Enabled, Revision: registration.Revision,
		CreatedAt: registration.CreatedAt.UTC(), UpdatedAt: registration.UpdatedAt.UTC(),
	}
	summary := entry.Summary
	view.EnrollmentState = summary.EnrollmentState
	view.ConnectionStatus = summary.ConnectionStatus
	view.ExecutionStatus = summary.ExecutionStatus
	view.LastSeenAt = optionalTime(summary.LastSeenAt)
	view.DesiredConfigRevision = fleetCounter(summary.DesiredConfigRevision)
	view.AppliedConfigRevision = fleetCounter(summary.AppliedConfigRevision)
	// agent_version, protocol_version, queue_bytes and oldest_queued_at have no
	// safe evidence source yet. They stay null; null is unreported, never an
	// implicit healthy default.
	return view
}

func toProbeDetailView(entry services.ProbeFleetEntry) ProbeDetailView {
	view := ProbeDetailView{ProbeView: toProbeView(entry), Capabilities: []string{}, Diagnostics: ProbeDiagnosticsView{Config: ProbeConfigDiagnosticsView{}}}
	facts := entry.Diagnostics
	// The registration owns the operator-declared network trust (frozen home);
	// prepared connections from before that freeze report theirs instead.
	view.Endpoint = optionalString(entry.Registration.Endpoint)
	view.TLSPin = optionalString(entry.Registration.TLSPin)
	if facts.Enrollment != nil {
		if view.Endpoint == nil {
			view.Endpoint = optionalString(facts.Enrollment.Endpoint)
		}
		if view.TLSPin == nil {
			view.TLSPin = optionalString(facts.Enrollment.TLSPin)
		}
		view.CertificateExpiresAt = optionalTime(facts.Enrollment.CertificateNotAfter)
		view.CredentialVersion = fleetString(strconv.FormatInt(facts.Enrollment.CredentialVersion, 10))
		view.Diagnostics.Enrollment = &ProbeEnrollmentDiagnosticsView{
			State:               facts.Enrollment.State,
			CredentialVersion:   strconv.FormatInt(facts.Enrollment.CredentialVersion, 10),
			CertificateVersion:  strconv.FormatInt(facts.Enrollment.CertificateVersion, 10),
			CertificateNotAfter: view.CertificateExpiresAt,
			PreparedAt:          facts.Enrollment.PreparedAt.UTC(),
			ActivatedAt:         optionalTime(facts.Enrollment.ActivatedAt),
		}
	}
	// capabilities has no safe evidence source yet; it renders an empty list.
	if facts.Session != nil {
		view.Diagnostics.Connection = &ProbeConnectionDiagnosticsView{
			OwnerID: facts.Session.OwnerID, Generation: strconv.FormatInt(facts.Session.Generation, 10),
			LeaseUntil: optionalTime(&facts.Session.LeaseUntil), Connected: facts.Session.Connected,
			Live: entry.Summary.ConnectionStatus == services.ProbeConnectionOnline || entry.Summary.ConnectionStatus == services.ProbeConnectionSuspect,
		}
	}
	if facts.Runtime != nil {
		view.Diagnostics.Runtime = &ProbeRuntimeDiagnosticsView{
			OwnerID: facts.Runtime.OwnerID, Epoch: strconv.FormatInt(facts.Runtime.Epoch, 10),
			LeaseUntil: optionalTime(&facts.Runtime.LeaseUntil),
			Live:       entry.Summary.ExecutionStatus == services.ProbeExecutionReady,
		}
	}
	if facts.Watchdog != nil {
		view.Diagnostics.Watchdog = &ProbeWatchdogDiagnosticsView{
			Status: facts.Watchdog.Status, Version: strconv.FormatInt(facts.Watchdog.Version, 10),
			ConfigRevision: strconv.FormatInt(facts.Watchdog.ConfigRevision, 10),
			Armed:          facts.Watchdog.Armed, PendingLoss: facts.Watchdog.PendingLoss,
			IncidentOpen: facts.Watchdog.IncidentOpen, UpdatedAt: facts.Watchdog.UpdatedAt.UTC(),
		}
	}
	if facts.Publication != nil {
		view.Diagnostics.Config.Desired = &ProbeConfigPublicationView{
			Revision: strconv.FormatInt(facts.Publication.Revision, 10), EffectiveAt: facts.Publication.EffectiveAt.UTC(),
		}
	}
	if facts.Applied != nil {
		view.Diagnostics.Config.Applied = &ProbeConfigApplicationView{
			Revision: strconv.FormatInt(facts.Applied.Revision, 10), AppliedAt: facts.Applied.AppliedAt.UTC(),
			AssignmentCount: facts.Applied.AssignmentCount,
		}
	}
	view.Diagnostics.Config.SyncStatus = optionalString(entry.Summary.ConfigSyncStatus)
	return view
}

// optionalString maps an empty derived value to the unreported null. Every
// diagnostic vocabulary value is lowercase bounded ASCII.
func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func fleetString(value string) *string { return &value }

// fleetRevision renders a revision counter as a decimal string and maps zero
// (no evidence) to null. The string form survives values beyond 2^53-1.
func fleetRevision(revision int64) *string {
	if revision <= 0 {
		return nil
	}
	value := strconv.FormatInt(revision, 10)
	return &value
}

// fleetCounter renders a required counter as a decimal string, "0" when
// unreported, per the frozen ProbeView contract.
func fleetCounter(value int64) string {
	if value < 0 {
		value = 0
	}
	return strconv.FormatInt(value, 10)
}

// optionalTime renders a timestamp in UTC and maps absent or zero times to the
// unreported null (a released lease deadline is zero and must not render 1970).
func optionalTime(at *time.Time) *time.Time {
	if at == nil || at.IsZero() {
		return nil
	}
	value := at.UTC()
	return &value
}

func fleetReadError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, ports.ErrNotFound), errors.Is(err, domain.ErrNotFound):
		return fleetError(c, http.StatusNotFound, "probe_not_found", "probe not found")
	case errors.Is(err, domain.ErrValidation):
		return fleetError(c, http.StatusBadRequest, "invalid_probe_id", "invalid probe id")
	default:
		return fleetError(c, http.StatusServiceUnavailable, "fleet_unavailable", "fleet data unavailable")
	}
}

func fleetError(c echo.Context, status int, code, message string) error {
	return c.JSON(status, regionalErrorView{Error: message, Code: code})
}
