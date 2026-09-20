package bootstrap

import (
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// Command output is deliberately metadata only: neither the protected request,
// operator note nor credential-bearing future command payload is serialized.
type probeAdminCommandView struct {
	CommandID            string         `json:"command_id"`
	Status               string         `json:"status"`
	RemoteConfirmed      bool           `json:"remote_confirmed"`
	SourceAlertID        *string        `json:"source_alert_id"`
	AssignmentGeneration *probe.Decimal `json:"assignment_generation"`
	CreatedAt            time.Time      `json:"created_at"`
	ExpiresAt            time.Time      `json:"expires_at"`
	Attempts             probe.Decimal  `json:"attempts"`
	AppliedAt            *time.Time     `json:"applied_at"`
	Code                 *string        `json:"code"`
	PendingMessage       string         `json:"pending_message,omitempty"`
}

func probeAdminCommand(c *domain.ProbeCommand) *probeAdminCommandView {
	v := &probeAdminCommandView{CommandID: c.CommandID, Status: c.Status, RemoteConfirmed: c.RemoteConfirmed, SourceAlertID: c.SourceAlertID, CreatedAt: c.CreatedAt.UTC(), ExpiresAt: c.ExpiresAt.UTC(), Attempts: probe.Decimal(c.Attempts)}
	if c.AssignmentGeneration != nil {
		g := probe.Decimal(*c.AssignmentGeneration)
		v.AssignmentGeneration = &g
	}
	if c.Outcome != nil {
		v.AppliedAt = c.Outcome.AppliedAt
		if c.Outcome.Code != "" {
			code := c.Outcome.Code
			v.Code = &code
		}
	}
	if !c.RemoteConfirmed {
		v.PendingMessage = "Remote alerts may continue until the probe confirms. Expiry does not prove whether a previous attempt applied."
		if c.Kind == "credential.prepare" || c.Kind == "credential.activate" {
			switch c.Status {
			case "blocked":
				v.PendingMessage = "Waiting for durable source preparation before activation can be sent."
			case "canceled":
				v.PendingMessage = "Activation was canceled locally because preparation failed. No source activation is confirmed."
			default:
				v.PendingMessage = "Waiting for the source's durable rotation result. Expiry or a successful connection does not prove activation."
			}
		}
	}
	return v
}
