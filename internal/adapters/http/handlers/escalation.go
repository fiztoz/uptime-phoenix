package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// EscalationHandlers serves the F2.3 escalation policy API.
//
// RBAC split, and it is deliberate:
//   - Policy CRUD sits behind can_manage_notifications (admins hold it
//     implicitly). A policy is a notification-routing object and its steps name
//     channels that are already behind that capability.
//   - Monitor and group ASSIGNMENT sits behind RequireAdmin. Assigning a policy
//     changes what a monitor does when it fails, and AGENTS.md makes monitors
//     and groups admin-write whatever else a user holds. Without this split a
//     non-admin notification manager could rewire the paging of a monitor they
//     are not even permitted to see.
//
// The routes enforce both; see router.go.
type EscalationHandlers struct {
	svc *services.EscalationService
}

// NewEscalationHandlers creates handlers bound to the escalation service.
func NewEscalationHandlers(svc *services.EscalationService) *EscalationHandlers {
	return &EscalationHandlers{svc: svc}
}

// EscalationStepView is the wire shape of one rung of a ladder. WaitMinutes is
// the delay after the PREVIOUS step (for step 1, after the initial DOWN
// notification), not an absolute offset from the alert's start.
type EscalationStepView struct {
	StepOrder       int     `json:"step_order"`
	WaitMinutes     int     `json:"wait_minutes"`
	NotificationIDs []int64 `json:"notification_ids"`
}

// EscalationPolicyView is the wire shape of an escalation policy.
type EscalationPolicyView struct {
	ID          int64                `json:"id"`
	Name        string               `json:"name"`
	Description string               `json:"description"`
	Enabled     bool                 `json:"enabled"`
	Steps       []EscalationStepView `json:"steps"`
	CreatedAt   string               `json:"created_at"`
	UpdatedAt   string               `json:"updated_at"`
}

// EscalationAssignmentView reports which policy is directly assigned to a
// monitor or group. PolicyID is 0 when nothing is assigned.
type EscalationAssignmentView struct {
	PolicyID int64 `json:"policy_id"`
}

func toEscalationPolicyView(p *domain.EscalationPolicy) EscalationPolicyView {
	v := EscalationPolicyView{
		ID:          p.ID,
		Name:        p.Name,
		Description: p.Description,
		Enabled:     p.Enabled,
		Steps:       make([]EscalationStepView, 0, len(p.Steps)),
		CreatedAt:   p.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   p.UpdatedAt.UTC().Format(time.RFC3339),
	}
	for _, st := range p.Steps {
		ids := st.NotificationIDs
		if ids == nil {
			ids = []int64{}
		}
		v.Steps = append(v.Steps, EscalationStepView{
			StepOrder:       st.StepOrder,
			WaitMinutes:     st.WaitMinutes,
			NotificationIDs: ids,
		})
	}
	return v
}

// escalationStepRequest is one step in a save. Step order comes from the array
// position — the UI reorders by moving elements, and accepting a client-side
// step_order would let a caller create gaps or duplicates.
type escalationStepRequest struct {
	WaitMinutes     int     `json:"wait_minutes"`
	NotificationIDs []int64 `json:"notification_ids"`
}

// escalationPolicyRequest is the body of create and update.
//
// Steps is a REPLACE-SET on update: the stored ladder becomes exactly what you
// send, and omitted steps are deleted. Send the whole ladder every time.
type escalationPolicyRequest struct {
	Name        string                  `json:"name"`
	Description string                  `json:"description"`
	Enabled     *bool                   `json:"enabled"`
	Steps       []escalationStepRequest `json:"steps"`
}

func (r escalationPolicyRequest) toDomain(userID int64) *domain.EscalationPolicy {
	enabled := true
	if r.Enabled != nil {
		enabled = *r.Enabled
	}
	p := &domain.EscalationPolicy{
		UserID:      userID,
		Name:        r.Name,
		Description: r.Description,
		Enabled:     enabled,
		Steps:       make([]domain.EscalationStep, 0, len(r.Steps)),
	}
	for i, st := range r.Steps {
		p.Steps = append(p.Steps, domain.EscalationStep{
			StepOrder:       i + 1,
			WaitMinutes:     st.WaitMinutes,
			NotificationIDs: st.NotificationIDs,
		})
	}
	return p
}

// escalationAssignRequest is the body of the assignment endpoints. A PolicyID of
// 0 (or null) unassigns.
type escalationAssignRequest struct {
	PolicyID *int64 `json:"policy_id"`
}

// List handles GET /api/escalation-policies.
func (h *EscalationHandlers) List(c echo.Context) error {
	policies, err := h.svc.ListPolicies(c.Request().Context())
	if err != nil {
		return mapEscalationError(c, err)
	}
	out := make([]EscalationPolicyView, 0, len(policies))
	for _, p := range policies {
		out = append(out, toEscalationPolicyView(p))
	}
	return c.JSON(http.StatusOK, out)
}

// Get handles GET /api/escalation-policies/:id.
func (h *EscalationHandlers) Get(c echo.Context) error {
	id, err := escalationPathID(c)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorBody("invalid policy id"))
	}
	p, err := h.svc.GetPolicy(c.Request().Context(), id)
	if err != nil {
		return mapEscalationError(c, err)
	}
	return c.JSON(http.StatusOK, toEscalationPolicyView(p))
}

// Create handles POST /api/escalation-policies.
func (h *EscalationHandlers) Create(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}
	var req escalationPolicyRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errorBody("invalid request body"))
	}
	p := req.toDomain(userID)
	if err := h.svc.CreatePolicy(c.Request().Context(), p); err != nil {
		return mapEscalationError(c, err)
	}
	return c.JSON(http.StatusCreated, toEscalationPolicyView(p))
}

// Update handles PUT /api/escalation-policies/:id. The step list is replaced
// wholesale.
func (h *EscalationHandlers) Update(c echo.Context) error {
	id, err := escalationPathID(c)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorBody("invalid policy id"))
	}
	existing, err := h.svc.GetPolicy(c.Request().Context(), id)
	if err != nil {
		return mapEscalationError(c, err)
	}
	var req escalationPolicyRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errorBody("invalid request body"))
	}
	p := req.toDomain(existing.UserID)
	p.ID = id
	p.CreatedAt = existing.CreatedAt
	if err := h.svc.UpdatePolicy(c.Request().Context(), p); err != nil {
		return mapEscalationError(c, err)
	}
	return c.JSON(http.StatusOK, toEscalationPolicyView(p))
}

// Delete handles DELETE /api/escalation-policies/:id.
func (h *EscalationHandlers) Delete(c echo.Context) error {
	id, err := escalationPathID(c)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorBody("invalid policy id"))
	}
	if _, err := h.svc.GetPolicy(c.Request().Context(), id); err != nil {
		return mapEscalationError(c, err)
	}
	if err := h.svc.DeletePolicy(c.Request().Context(), id); err != nil {
		return mapEscalationError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// GetMonitorAssignment handles GET /api/monitors/:id/escalation-policy.
func (h *EscalationHandlers) GetMonitorAssignment(c echo.Context) error {
	id, err := escalationPathID(c)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorBody("invalid monitor id"))
	}
	policyID, err := h.svc.PolicyIDForMonitor(c.Request().Context(), id)
	if err != nil {
		return mapEscalationError(c, err)
	}
	return c.JSON(http.StatusOK, EscalationAssignmentView{PolicyID: policyID})
}

// SetMonitorAssignment handles PUT /api/monitors/:id/escalation-policy.
func (h *EscalationHandlers) SetMonitorAssignment(c echo.Context) error {
	id, err := escalationPathID(c)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorBody("invalid monitor id"))
	}
	var req escalationAssignRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errorBody("invalid request body"))
	}
	ctx := c.Request().Context()
	if req.PolicyID == nil || *req.PolicyID == 0 {
		if err := h.svc.UnassignMonitor(ctx, id); err != nil {
			return mapEscalationError(c, err)
		}
		return c.JSON(http.StatusOK, EscalationAssignmentView{PolicyID: 0})
	}
	if err := h.svc.AssignMonitor(ctx, id, *req.PolicyID); err != nil {
		return mapEscalationError(c, err)
	}
	return c.JSON(http.StatusOK, EscalationAssignmentView{PolicyID: *req.PolicyID})
}

// GetGroupAssignment handles GET /api/monitor-groups/:id/escalation-policy.
func (h *EscalationHandlers) GetGroupAssignment(c echo.Context) error {
	id, err := escalationPathID(c)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorBody("invalid group id"))
	}
	policyID, err := h.svc.PolicyIDForGroup(c.Request().Context(), id)
	if err != nil {
		return mapEscalationError(c, err)
	}
	return c.JSON(http.StatusOK, EscalationAssignmentView{PolicyID: policyID})
}

// SetGroupAssignment handles PUT /api/monitor-groups/:id/escalation-policy.
func (h *EscalationHandlers) SetGroupAssignment(c echo.Context) error {
	id, err := escalationPathID(c)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorBody("invalid group id"))
	}
	var req escalationAssignRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errorBody("invalid request body"))
	}
	ctx := c.Request().Context()
	if req.PolicyID == nil || *req.PolicyID == 0 {
		if err := h.svc.UnassignGroup(ctx, id); err != nil {
			return mapEscalationError(c, err)
		}
		return c.JSON(http.StatusOK, EscalationAssignmentView{PolicyID: 0})
	}
	if err := h.svc.AssignGroup(ctx, id, *req.PolicyID); err != nil {
		return mapEscalationError(c, err)
	}
	return c.JSON(http.StatusOK, EscalationAssignmentView{PolicyID: *req.PolicyID})
}

// escalationPathID reads the :id path parameter. Every escalation route names
// its subject "id" — the monitor and group routes included — so the parameter
// name is not worth threading through each call.
func escalationPathID(c echo.Context) (int64, error) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid id")
	}
	return id, nil
}

func mapEscalationError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, ports.ErrNotFound), errors.Is(err, domain.ErrNotFound):
		return c.JSON(http.StatusNotFound, errorBody("escalation policy not found"))
	case errors.Is(err, domain.ErrValidation):
		return c.JSON(http.StatusBadRequest, errorBody(err.Error()))
	case errors.Is(err, domain.ErrConflict), errors.Is(err, ports.ErrConflict):
		return c.JSON(http.StatusConflict, errorBody("escalation policy conflict"))
	default:
		return c.JSON(http.StatusInternalServerError, errorBody("internal error"))
	}
}
