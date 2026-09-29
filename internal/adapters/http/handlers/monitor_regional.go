package handlers

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type monitorRegionalReader interface {
	Assignments(context.Context, int64, int64, time.Time) (*services.MonitorRegionalAssignments, error)
	Health(context.Context, int64, int64, int, time.Time) (*services.MonitorRegionalHealth, error)
	RegionalHistory(context.Context, int64, int64, string, time.Time, time.Time) ([]domain.RegionalObservation, error)
}

// assignmentWriter is the optional revisioned write surface behind
// PUT /api/monitors/:id/probes. Without it the route stays registered but
// unavailable, exactly like a router built without the read service.
type assignmentWriter interface {
	Replace(context.Context, int64, services.ProbeAssignmentRequest) (*services.ProbeAssignmentWriteResult, error)
}

// MonitorRegionalHandlers exposes the opt-in M5 read contract. The service owns
// authorization; DTOs below are the only objects serialized to clients.
type MonitorRegionalHandlers struct {
	browser *RegionalBrowserPublisher
	alerts  *services.ProbeAlertService
	svc     monitorRegionalReader
	writer  assignmentWriter
	enabled bool
	now     func() time.Time
}

// NewMonitorRegionalHandlers builds handlers; disabled reads return a typed 503.
func NewMonitorRegionalHandlers(svc monitorRegionalReader, enabled bool) *MonitorRegionalHandlers {
	return &MonitorRegionalHandlers{svc: svc, enabled: enabled, now: time.Now}
}

// SetAssignments wires the revisioned desired-set write surface.
func (h *MonitorRegionalHandlers) SetAssignments(writer assignmentWriter) {
	if h != nil {
		h.writer = writer
	}
}

// MonitorProbeAssignmentsView is desired state only. Null synchronization fields
// mean unreported, never applied. Bindings contain keys, never local endpoints.
type MonitorProbeAssignmentsView struct {
	Revision      int64                        `json:"revision,string"`
	HealthPolicy  string                       `json:"health_policy"`
	AlertDelivery string                       `json:"alert_delivery"`
	Assignments   []MonitorProbeAssignmentView `json:"assignments"`
}

// MonitorProbeAssignmentView contains safe metadata for one assigned region.
// The synchronization fields carry only evidence from the safe diagnostics
// port: desired/applied revisions are per-probe published and applied config
// revisions, and sync_status is applied only when the application receipt
// matches the published document revision AND digest. Null stays unreported.
type MonitorProbeAssignmentView struct {
	ProbeID               string                    `json:"probe_id"`
	Name                  string                    `json:"name"`
	Location              string                    `json:"location"`
	Generation            int64                     `json:"generation,string"`
	DesiredConfigRevision *string                   `json:"desired_config_revision"`
	AppliedConfigRevision *string                   `json:"applied_config_revision"`
	SyncStatus            *string                   `json:"sync_status"`
	Bindings              []MonitorProbeBindingView `json:"bindings"`
}

// MonitorProbeBindingView discloses only the logical resource reference.
type MonitorProbeBindingView struct {
	Kind       string `json:"kind"`
	BindingKey string `json:"binding_key"`
}

// MonitorHealthView keeps overall availability separate from coverage and regions.
type MonitorHealthView struct {
	MonitorID          int64                 `json:"monitor_id"`
	Status             string                `json:"status"`
	HealthPolicy       string                `json:"health_policy"`
	ProjectionVersion  int64                 `json:"projection_version,string"`
	AsOf               time.Time             `json:"as_of"`
	UptimePercent      *float64              `json:"uptime_percent"`
	CoveragePercent    *float64              `json:"coverage_percent"`
	KnownSeconds       float64               `json:"known_seconds"`
	UnknownSeconds     float64               `json:"unknown_seconds"`
	MaintenanceSeconds float64               `json:"maintenance_seconds"`
	ProbeCounts        ProbeHealthCountsView `json:"probe_counts"`
	Regions            []MonitorRegionView   `json:"regions"`
}

// ProbeHealthCountsView is the explicit wire map for the complete quorum.
type ProbeHealthCountsView struct {
	Assigned    int `json:"assigned"`
	Up          int `json:"up"`
	Down        int `json:"down"`
	Pending     int `json:"pending"`
	Unknown     int `json:"unknown"`
	Maintenance int `json:"maintenance"`
	Paused      int `json:"paused"`
}

// MonitorRegionView never includes endpoint, credentials, raw checker messages,
// configuration payloads or fleet totals. Unreported diagnostics are null.
type MonitorRegionView struct {
	ProbeID          string     `json:"probe_id"`
	Name             string     `json:"name"`
	Location         string     `json:"location"`
	Status           string     `json:"status"`
	ConnectionStatus *string    `json:"connection_status"`
	ObservedAt       *time.Time `json:"observed_at"`
	ReceivedAt       *time.Time `json:"received_at"`
	FreshUntil       *time.Time `json:"fresh_until"`
	ConfigSyncStatus *string    `json:"config_sync_status"`
	Reason           *string    `json:"reason"`
}

type regionalErrorView struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// Assignments handles GET /api/monitors/:id/probes.
func (h *MonitorRegionalHandlers) Assignments(c echo.Context) error {
	userID, id, ok := h.request(c)
	if !ok {
		return nil
	}
	result, err := h.svc.Assignments(c.Request().Context(), userID, id, h.now().UTC())
	if err != nil {
		return regionalReadError(c, err)
	}
	if h.browser != nil {
		h.browser.ObserveMonitor(c.Request().Context(), id)
	}
	return c.JSON(http.StatusOK, toAssignmentsView(result, nil))
}

// ReplaceMonitorAssignmentsRequest is the PUT body: one complete desired set.
// expected_revision is mandatory and decimal-string; bindings is optional and
// distinguishes omission (preserve retained bindings) from an explicit list.
type ReplaceMonitorAssignmentsRequest struct {
	ExpectedRevision string                        `json:"expected_revision"`
	ProbeIDs         []string                      `json:"probe_ids"`
	HealthPolicy     string                        `json:"health_policy"`
	AlertDelivery    string                        `json:"alert_delivery"`
	Bindings         *[]MonitorProbeBindingRequest `json:"bindings"`
}

// MonitorProbeBindingRequest selects one probe-local resource for a member.
type MonitorProbeBindingRequest struct {
	ProbeID    string `json:"probe_id"`
	Kind       string `json:"kind"`
	BindingKey string `json:"binding_key"`
}

// Replace handles PUT /api/monitors/:id/probes. It validates the complete set
// before the atomic write, returns 409 for a stale expected revision, and
// answers the same frozen view as GET with pending application marked for the
// members this write changed.
func (h *MonitorRegionalHandlers) Replace(c echo.Context) error {
	userID, id, ok := h.request(c)
	if !ok {
		return nil
	}
	if h.writer == nil {
		return regionalError(c, http.StatusServiceUnavailable, "assignment_unavailable", "assignment writes unavailable")
	}
	var body ReplaceMonitorAssignmentsRequest
	if err := c.Bind(&body); err != nil {
		return regionalError(c, http.StatusBadRequest, "invalid_request", "invalid request body")
	}
	revision, err := strconv.ParseInt(body.ExpectedRevision, 10, 64)
	if err != nil || revision < 1 {
		return regionalError(c, http.StatusBadRequest, "invalid_expected_revision", "expected_revision must be a positive decimal string")
	}
	req := services.ProbeAssignmentRequest{
		ExpectedRevision: revision, ProbeIDs: body.ProbeIDs,
		HealthPolicy: domain.HealthPolicy(body.HealthPolicy), AlertDelivery: body.AlertDelivery,
	}
	if body.Bindings != nil {
		list := make([]domain.ProbeAssignmentBinding, 0, len(*body.Bindings))
		for _, binding := range *body.Bindings {
			list = append(list, domain.ProbeAssignmentBinding{ProbeID: binding.ProbeID,
				ProbeResourceBinding: domain.ProbeResourceBinding{Kind: binding.Kind, BindingKey: binding.BindingKey}})
		}
		req.Bindings = &list
	}
	written, err := h.writer.Replace(c.Request().Context(), id, req)
	if err != nil {
		if errors.Is(err, services.ErrFleetNotAssignmentAware) {
			// The wrapped error names the offending workers. That is operator
			// infrastructure detail, so it goes to the log and not into the
			// response body, which keeps the documented fixed message.
			slog.WarnContext(c.Request().Context(),
				"regional assignment refused: hub workers do not all enforce assignment ownership",
				"monitor_id", id, "reason", err.Error())
		}
		return regionalWriteError(c, err)
	}
	result, err := h.svc.Assignments(c.Request().Context(), userID, id, h.now().UTC())
	if err != nil {
		return regionalReadError(c, err)
	}
	if h.browser != nil {
		h.browser.OnRegionalEvidence(c.Request().Context(), []int64{id})
	}
	return c.JSON(http.StatusOK, toAssignmentsView(result, written.PendingProbes))
}

// toAssignmentsView renders the frozen desired-set view. Members in pending
// carry this write's unproven desired state and must read pending, never an
// earlier proven application.
func toAssignmentsView(result *services.MonitorRegionalAssignments, pending map[string]bool) MonitorProbeAssignmentsView {
	out := MonitorProbeAssignmentsView{Revision: result.Set.Revision, HealthPolicy: string(result.Set.HealthPolicy), AlertDelivery: string(domain.AlertDeliveryRegional), Assignments: make([]MonitorProbeAssignmentView, 0, len(result.Set.Assignments))}
	for _, a := range result.Set.Assignments {
		p := result.Probes[a.ProbeID]
		diag := result.Diag[a.ProbeID]
		row := MonitorProbeAssignmentView{ProbeID: a.ProbeID, Name: p.Name, Location: p.Location, Generation: a.Generation, Bindings: []MonitorProbeBindingView{},
			DesiredConfigRevision: fleetRevision(diag.DesiredConfigRevision), AppliedConfigRevision: fleetRevision(diag.AppliedConfigRevision), SyncStatus: optionalString(diag.ConfigSyncStatus)}
		if a.ResourceBinding != nil {
			row.Bindings = append(row.Bindings, MonitorProbeBindingView{Kind: a.ResourceBinding.Kind, BindingKey: a.ResourceBinding.BindingKey})
		}
		if pending[a.ProbeID] {
			row.SyncStatus = optionalString(services.ProbeConfigSyncPending)
		}
		out.Assignments = append(out.Assignments, row)
	}
	return out
}

func regionalWriteError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, services.ErrStaleRevision):
		return regionalError(c, http.StatusConflict, "stale_revision", "expected revision is stale")
	case errors.Is(err, services.ErrUnknownProbe):
		return regionalError(c, http.StatusConflict, "unknown_probe", "one or more probes are not registered")
	case errors.Is(err, services.ErrProbeUnavailable):
		return regionalError(c, http.StatusConflict, "probe_unavailable", "one or more probes are disabled")
	case errors.Is(err, services.ErrUnsupportedAssignment):
		return regionalError(c, http.StatusUnprocessableEntity, "unsupported_assignment", "the requested assignment cannot execute this monitor")
	case errors.Is(err, services.ErrInvalidProbeIDs):
		return regionalError(c, http.StatusBadRequest, "invalid_probe_ids", "probe_ids must be distinct registered probe ids")
	case errors.Is(err, services.ErrInvalidPolicy):
		return regionalError(c, http.StatusBadRequest, "invalid_health_policy", "health_policy must be any_down or all_down")
	case errors.Is(err, services.ErrInvalidDelivery):
		return regionalError(c, http.StatusBadRequest, "invalid_alert_delivery", "alert_delivery must be regional")
	case errors.Is(err, services.ErrInvalidBindings):
		return regionalError(c, http.StatusBadRequest, "invalid_bindings", "bindings must reference member probes with a supported resource")
	case errors.Is(err, services.ErrFleetNotAssignmentAware):
		// T34: the rollout is not finished, so the fleet would run this monitor
		// both remotely and locally. Retryable once every worker attests.
		return regionalError(c, http.StatusConflict, "worker_fleet_unaware", "one or more hub workers do not enforce probe assignment ownership")
	case errors.Is(err, services.ErrFleetReadinessUnavailable):
		return regionalError(c, http.StatusServiceUnavailable, "worker_readiness_unavailable", "hub worker readiness could not be verified")
	case errors.Is(err, ports.ErrNotFound), errors.Is(err, domain.ErrNotFound):
		return regionalError(c, http.StatusNotFound, "monitor_not_found", "monitor not found")
	case errors.Is(err, domain.ErrValidation):
		return regionalError(c, http.StatusBadRequest, "invalid_request", "invalid assignment request")
	default:
		return regionalError(c, http.StatusServiceUnavailable, "assignment_unavailable", "assignment data unavailable")
	}
}

// Health handles GET /api/monitors/:id/health. Hours is an integer in [1,720]
// and defaults to 24; it affects coverage only, never the current freshness read.
func (h *MonitorRegionalHandlers) Health(c echo.Context) error {
	userID, id, ok := h.request(c)
	if !ok {
		return nil
	}
	hours := 24
	if raw, exists := c.QueryParams()["hours"]; exists {
		var err error
		if len(raw) != 1 {
			return regionalError(c, http.StatusBadRequest, "invalid_hours", "hours must be an integer between 1 and 720")
		}
		hours, err = strconv.Atoi(raw[0])
		if err != nil || hours < 1 || hours > 720 {
			return regionalError(c, http.StatusBadRequest, "invalid_hours", "hours must be an integer between 1 and 720")
		}
	}
	result, err := h.svc.Health(c.Request().Context(), userID, id, hours, h.now().UTC())
	if err != nil {
		return regionalReadError(c, err)
	}
	if h.browser != nil {
		h.browser.ObserveMonitor(c.Request().Context(), id)
	}
	out, err := toMonitorHealthView(result)
	if err != nil {
		return regionalReadError(c, err)
	}
	return c.JSON(http.StatusOK, out)
}

func (h *MonitorRegionalHandlers) request(c echo.Context) (int64, int64, bool) {
	c.Response().Header().Set("Cache-Control", "no-store")
	userID, ok := userIDFromContext(c)
	if !ok || userID <= 0 {
		_ = regionalError(c, http.StatusUnauthorized, "unauthenticated", "authentication required")
		return 0, 0, false
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		_ = regionalError(c, http.StatusBadRequest, "invalid_monitor_id", "invalid monitor id")
		return 0, 0, false
	}
	if !h.enabled {
		_ = regionalError(c, http.StatusServiceUnavailable, "probes_disabled", "multi-region probes are disabled")
		return 0, 0, false
	}
	if h.svc == nil {
		_ = regionalError(c, http.StatusServiceUnavailable, "regional_unavailable", "regional data unavailable")
		return 0, 0, false
	}
	return userID, id, true
}

func toMonitorHealthView(result *services.MonitorRegionalHealth) (MonitorHealthView, error) {
	current, coverage := result.Current, result.Coverage
	counts := current.Health.Counts
	out := MonitorHealthView{MonitorID: current.MonitorID, Status: regionalHTTPStatus(current.Health.Status), HealthPolicy: string(current.Policy), ProjectionVersion: result.ProjectionVersion, AsOf: current.AsOf.UTC(), UptimePercent: coverage.UptimePercent, CoveragePercent: coverage.CoveragePercent, KnownSeconds: coverage.Known.Seconds(), UnknownSeconds: coverage.Unknown.Seconds(), MaintenanceSeconds: coverage.Maintenance.Seconds(), ProbeCounts: ProbeHealthCountsView{Assigned: counts.Assigned, Up: counts.Up, Down: counts.Down, Pending: counts.Pending, Unknown: counts.Unknown, Maintenance: counts.Maintenance, Paused: counts.Paused}, Regions: make([]MonitorRegionView, 0, len(current.Regions))}
	for _, e := range current.Regions {
		status, reason, err := services.RegionalDisplayHealth(current.AsOf, e)
		if err != nil {
			return MonitorHealthView{}, err
		}
		p := result.Probes[e.ProbeID]
		diag := result.Diag[e.ProbeID]
		row := MonitorRegionView{ProbeID: e.ProbeID, Name: p.Name, Location: p.Location, Status: regionalHTTPStatus(status), ObservedAt: regionalTime(e.ObservedAt), ReceivedAt: regionalTime(e.ReceivedAt),
			ConnectionStatus: optionalString(diag.ConnectionStatus), ConfigSyncStatus: optionalString(diag.ConfigSyncStatus)}
		if !e.ObservedAt.IsZero() && e.FreshFor > 0 {
			row.FreshUntil = regionalTime(e.ObservedAt.Add(e.FreshFor))
		}
		if reason != "" {
			row.Reason = &reason
		}
		out.Regions = append(out.Regions, row)
	}
	return out, nil
}

func regionalTime(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	at = at.UTC()
	return &at
}

// Keep this separate from legacy heartbeat/browser mappings: UNKNOWN must not
// become pending, and maintenance must not become a transport disconnect.
func regionalHTTPStatus(status domain.Status) string {
	switch status {
	case domain.StatusUp:
		return "up"
	case domain.StatusDown:
		return "down"
	case domain.StatusPending:
		return "pending"
	case domain.StatusMaintenance:
		return "maintenance"
	default:
		return "unknown"
	}
}

func regionalReadError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, ports.ErrNotFound), errors.Is(err, domain.ErrNotFound):
		return regionalError(c, http.StatusNotFound, "monitor_not_found", "monitor not found")
	case errors.Is(err, domain.ErrValidation):
		return regionalError(c, http.StatusBadRequest, "invalid_request", "invalid regional request")
	default:
		return regionalError(c, http.StatusServiceUnavailable, "regional_unavailable", "regional data unavailable")
	}
}
func regionalError(c echo.Context, status int, code, message string) error {
	return c.JSON(status, regionalErrorView{Error: message, Code: code})
}
