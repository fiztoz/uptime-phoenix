package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// ProbeAlertView contains safe regional attribution and no command plaintext.
type ProbeAlertView struct {
	SourceAlertID        string     `json:"source_alert_id"`
	MonitorID            int64      `json:"monitor_id"`
	ProbeID              string     `json:"probe_id"`
	ProbeName            string     `json:"probe_name"`
	Location             string     `json:"location"`
	AssignmentGeneration int64      `json:"assignment_generation,string"`
	Status               string     `json:"status"`
	SubjectKind          string     `json:"subject_kind"`
	Reason               string     `json:"reason"`
	StartedAt            time.Time  `json:"started_at"`
	AckedAt              *time.Time `json:"acked_at"`
	ResolvedAt           *time.Time `json:"resolved_at"`
}

// ProbeCommandView is the closed browser command receipt vocabulary.
type ProbeCommandView struct {
	CommandID       string `json:"command_id"`
	Status          string `json:"status"`
	RemoteConfirmed bool   `json:"remote_confirmed"`
}

// SetAlerts wires source-owned incident reads and remote acknowledgement.
func (h *MonitorRegionalHandlers) SetAlerts(alerts *services.ProbeAlertService) { h.alerts = alerts }

// ListAlerts returns a bare array of this visible monitor's regional incidents.
func (h *MonitorRegionalHandlers) ListAlerts(c echo.Context) error {
	userID, monitorID, ok := h.request(c)
	if !ok {
		return nil
	}
	if h.alerts == nil {
		return fleetError(c, 503, "regional_unavailable", "regional alerts unavailable")
	}
	rows, err := h.alerts.List(c.Request().Context(), userID, monitorID)
	if err != nil {
		return regionalReadError(c, err)
	}
	out := make([]ProbeAlertView, 0, len(rows))
	for _, row := range rows {
		i := row.Incident
		out = append(out, ProbeAlertView{SourceAlertID: i.SourceAlertID, MonitorID: i.MonitorID,
			ProbeID: i.ProbeID, ProbeName: row.Name, Location: row.Location,
			AssignmentGeneration: i.AssignmentGeneration, Status: i.Status, SubjectKind: i.SubjectKind,
			Reason: i.Reason, StartedAt: i.StartedAt.UTC(), AckedAt: optionalTime(i.AckedAt), ResolvedAt: optionalTime(i.ResolvedAt)})
	}
	return c.JSON(http.StatusOK, out)
}

// AcknowledgeAlert records a command before answering 202; it never updates the
// mirrored incident as if remote suppression had already taken effect.
func (h *MonitorRegionalHandlers) AcknowledgeAlert(c echo.Context) error {
	userID, monitorID, ok := h.request(c)
	if !ok {
		return nil
	}
	if h.alerts == nil {
		return fleetError(c, 503, "regional_unavailable", "regional alerts unavailable")
	}
	var request struct {
		CommandID string  `json:"command_id"`
		Note      *string `json:"note"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(c.Response(), c.Request().Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return fleetError(c, 400, "invalid_command", "invalid acknowledgement request")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return fleetError(c, 400, "invalid_command", "invalid acknowledgement request")
	}
	command, err := h.alerts.Acknowledge(c.Request().Context(), userID, monitorID, c.Param("alert_id"), request.CommandID, request.Note)
	if err != nil {
		return probeAlertError(c, err)
	}
	return c.JSON(http.StatusAccepted, toProbeCommandView(command))
}

// AlertCommand reads the exact source receipt for a monitor-bound command.
func (h *MonitorRegionalHandlers) AlertCommand(c echo.Context) error {
	userID, monitorID, ok := h.request(c)
	if !ok {
		return nil
	}
	if h.alerts == nil {
		return fleetError(c, 503, "regional_unavailable", "regional alerts unavailable")
	}
	command, err := h.alerts.Command(c.Request().Context(), userID, monitorID, c.Param("alert_id"), c.Param("command_id"))
	if err != nil {
		return probeAlertError(c, err)
	}
	return c.JSON(http.StatusOK, toProbeCommandView(command))
}

func toProbeCommandView(command *domain.ProbeCommand) ProbeCommandView {
	status := command.Status
	switch status {
	case "pending", "applied", "expired":
	default:
		status = "failed"
	}
	return ProbeCommandView{CommandID: command.CommandID, Status: status, RemoteConfirmed: command.RemoteConfirmed}
}

func probeAlertError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, domain.ErrValidation):
		return fleetError(c, 400, "invalid_command", "invalid acknowledgement request")
	case errors.Is(err, ports.ErrConflict):
		return fleetError(c, 409, "command_conflict", "command conflicts with durable state")
	default:
		return regionalReadError(c, err)
	}
}
