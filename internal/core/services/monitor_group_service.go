// Package services contains the use-case implementations.
// Services depend ONLY on ports and domain — never on adapters.
package services

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// maxGroupWalkDepth bounds the ParentID walk used for cycle detection so a
// pre-existing cycle in the DB (bad data, not something the service should
// ever produce itself) cannot hang a request by spinning forever.
const maxGroupWalkDepth = 64

// MonitorGroupService handles monitor group (folder) CRUD and status rollup.
type MonitorGroupService struct {
	repo        ports.MonitorGroupRepository
	monitorRepo ports.MonitorRepository
	hbRepo      ports.HeartbeatRepository
	logger      ports.Logger
	bus         ports.EventBus
}

// SetEventBus wires the event bus so Delete can announce the monitors it
// re-homed. Optional: without it the re-homing still happens correctly, but
// already-connected clients keep showing the deleted group's membership until
// they reload, because nothing on the wire tells them the monitors moved.
func (s *MonitorGroupService) SetEventBus(bus ports.EventBus) { s.bus = bus }

// NewMonitorGroupService creates a new MonitorGroupService.
func NewMonitorGroupService(
	repo ports.MonitorGroupRepository,
	monitorRepo ports.MonitorRepository,
	hbRepo ports.HeartbeatRepository,
	logger ports.Logger,
) *MonitorGroupService {
	return &MonitorGroupService{
		repo:        repo,
		monitorRepo: monitorRepo,
		hbRepo:      hbRepo,
		logger:      logger,
	}
}

// Create creates a new monitor group after validation.
func (s *MonitorGroupService) Create(ctx context.Context, g *domain.MonitorGroup) error {
	if err := s.validate(ctx, g); err != nil {
		return err
	}
	if err := s.repo.Create(ctx, g); err != nil {
		return fmt.Errorf("monitor group service: create: %w", err)
	}
	s.logger.Info("monitor group created", "group_id", g.ID, "user_id", g.UserID, "name", g.Name)
	return nil
}

// GetByID retrieves a monitor group by its ID.
func (s *MonitorGroupService) GetByID(ctx context.Context, id int64) (*domain.MonitorGroup, error) {
	return s.repo.GetByID(ctx, id)
}

// List retrieves every monitor group owned by userID.
func (s *MonitorGroupService) List(ctx context.Context, userID int64) ([]*domain.MonitorGroup, error) {
	return s.repo.List(ctx, userID)
}

// ListAll retrieves every monitor group in the install, regardless of owner.
//
// This is the RBAC listing path: an admin sees every group, and a non-admin sees
// the subset the AccessService says is visible to them — which can include groups
// owned by an admin. Neither of those questions can be answered by the
// owner-scoped List. It performs no authorization of its own; the caller filters.
func (s *MonitorGroupService) ListAll(ctx context.Context) ([]*domain.MonitorGroup, error) {
	return s.repo.ListAll(ctx)
}

// Update updates a monitor group after validation, including cycle detection.
func (s *MonitorGroupService) Update(ctx context.Context, g *domain.MonitorGroup) error {
	if err := s.validate(ctx, g); err != nil {
		return err
	}
	if err := s.repo.Update(ctx, g); err != nil {
		return fmt.Errorf("monitor group service: update: %w", err)
	}
	s.logger.Info("monitor group updated", "group_id", g.ID, "user_id", g.UserID)
	return nil
}

// Delete deletes a monitor group by its ID. The repository is responsible for
// re-homing children (monitors and subgroups) to the deleted group's own
// parent rather than cascading — Delete never removes a group's contents.
func (s *MonitorGroupService) Delete(ctx context.Context, id int64) error {
	// Capture the affected monitors up front: the repo re-homes them to this
	// group's parent inside its own transaction and does not report which rows
	// it touched, so after the delete there is no way to find them again.
	group, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return fmt.Errorf("monitor group service: delete: %w", err)
	}
	rehomed, err := s.monitorRepo.List(ctx, ports.MonitorFilter{GroupID: &id})
	if err != nil {
		return fmt.Errorf("monitor group service: delete: list monitors: %w", err)
	}

	if err := s.repo.Delete(ctx, id); err != nil {
		return fmt.Errorf("monitor group service: delete: %w", err)
	}
	s.logger.Info("monitor group deleted", "group_id", id, "monitors_rehomed", len(rehomed))

	// Tell connected clients the monitors moved up to the deleted group's parent.
	// Without this they would keep rendering them inside a folder that is gone.
	if s.bus != nil {
		for _, m := range rehomed {
			m.GroupID = group.ParentID
			_ = s.bus.Publish(ctx, ports.Event{Type: "monitor.update", Payload: m})
		}
	}
	return nil
}

// validate enforces every rule in the shared contract: name required,
// condition valid (defaulting empty to worst_of_children), threshold rules,
// and parent existence/ownership/cycle checks.
func (s *MonitorGroupService) validate(ctx context.Context, g *domain.MonitorGroup) error {
	name := strings.TrimSpace(g.Name)
	if name == "" {
		return fmt.Errorf("monitor group service: %w: name is required", domain.ErrValidation)
	}
	g.Name = name

	if g.Condition == "" {
		g.Condition = domain.GroupConditionWorstOfChildren
	}
	if !g.Condition.Valid() {
		return fmt.Errorf("monitor group service: %w: invalid condition %q", domain.ErrValidation, g.Condition)
	}

	if g.Condition == domain.GroupConditionThreshold {
		if g.ThresholdIsPercent {
			if g.Threshold < 1 || g.Threshold > 100 {
				return fmt.Errorf("monitor group service: %w: percent threshold must be between 1 and 100", domain.ErrValidation)
			}
		} else if g.Threshold < 1 {
			return fmt.Errorf("monitor group service: %w: threshold must be >= 1", domain.ErrValidation)
		}
	}

	return s.validateParent(ctx, g)
}

// validateParent enforces the group-nesting rules: the parent must exist and
// belong to the same user as g; a group cannot be its own parent; and
// reparenting must never introduce a cycle. Mirrors MonitorService's
// validateProxy/validateGroup ownership-check pattern.
func (s *MonitorGroupService) parentForCycleCheck(ctx context.Context, id int64) (*domain.MonitorGroup, bool) {
	next, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, false
	}
	return next, true
}

func (s *MonitorGroupService) validateParent(ctx context.Context, g *domain.MonitorGroup) error {
	if g.ParentID == nil {
		return nil
	}
	if g.ID != 0 && *g.ParentID == g.ID {
		return fmt.Errorf("monitor group service: %w: a group cannot be its own parent", domain.ErrValidation)
	}
	parent, err := s.repo.GetByID(ctx, *g.ParentID)
	if err != nil {
		if errors.Is(err, ports.ErrNotFound) || errors.Is(err, domain.ErrNotFound) {
			return fmt.Errorf("monitor group service: %w: parent group not found", domain.ErrValidation)
		}
		return fmt.Errorf("monitor group service: validate parent: %w", err)
	}
	if parent.UserID != g.UserID {
		// Do not leak existence of another user's group — same message as "not found".
		return fmt.Errorf("monitor group service: %w: parent group not found", domain.ErrValidation)
	}

	// Cycle detection: only relevant once g already exists (a brand-new group
	// cannot yet be anyone's ancestor). Walk the proposed parent's ParentID
	// chain upward; if it ever reaches g.ID, assigning this parent would close
	// a loop. Bounded so a pre-existing cycle in the DB cannot hang forever.
	if g.ID != 0 {
		cur := parent
		for depth := 0; depth < maxGroupWalkDepth; depth++ {
			if cur.ID == g.ID {
				return fmt.Errorf("monitor group service: %w: assigning this parent would create a cycle", domain.ErrValidation)
			}
			if cur.ParentID == nil {
				break
			}
			next, ok := s.parentForCycleCheck(ctx, *cur.ParentID)
			if !ok {
				// Dangling parent reference — nothing further to walk.
				break
			}
			cur = next
		}
	}
	return nil
}

// ResolveStatuses builds the group tree for userID, loads every monitor's
// latest heartbeat status, and rolls statuses up bottom-up (deepest
// subgroups resolved before their ancestors) via domain.MonitorGroup.Rollup.
// Groups with no status — an "ignore" group, or a group with no children with
// a status of their own — are absent from the returned map.
//
// userID == 0 means "the whole install": every group, rolled up over every
// monitor. That is the RBAC listing path — a group's status must be derived from
// ALL of its children, not just the ones the requesting user can see, or a folder
// containing one visible UP monitor and one invisible DOWN monitor would falsely
// report UP. The caller then shows the status only for the groups the user may
// see; it never widens which groups those are.
func (s *MonitorGroupService) ResolveStatuses(ctx context.Context, userID int64) (map[int64]domain.Status, error) {
	var groups []*domain.MonitorGroup
	var err error
	if userID > 0 {
		groups, err = s.repo.List(ctx, userID)
	} else {
		groups, err = s.repo.ListAll(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("monitor group service: resolve statuses: list groups: %w", err)
	}
	result := make(map[int64]domain.Status)
	if len(groups) == 0 {
		return result, nil
	}

	childGroups := make(map[int64][]*domain.MonitorGroup, len(groups))
	for _, g := range groups {
		if g.ParentID != nil {
			childGroups[*g.ParentID] = append(childGroups[*g.ParentID], g)
		}
	}

	monitors, err := s.monitorRepo.List(ctx, ports.MonitorFilter{UserID: userID})
	if err != nil {
		return nil, fmt.Errorf("monitor group service: resolve statuses: list monitors: %w", err)
	}
	monitorsByGroup := make(map[int64][]*domain.Monitor, len(groups))
	for _, m := range monitors {
		if m.GroupID == nil {
			continue
		}
		monitorsByGroup[*m.GroupID] = append(monitorsByGroup[*m.GroupID], m)
	}

	visited := make(map[int64]bool, len(groups))
	resolving := make(map[int64]bool, len(groups)) // cycle guard for bad data

	var resolve func(g *domain.MonitorGroup) (domain.Status, bool)
	resolve = func(g *domain.MonitorGroup) (domain.Status, bool) {
		if visited[g.ID] {
			status, ok := result[g.ID]
			return status, ok
		}
		if resolving[g.ID] {
			// Defensive: a pre-existing cycle in the data. The service itself
			// never creates one (see validateParent), but don't recurse forever
			// if the DB has one anyway.
			return domain.StatusPending, false
		}
		resolving[g.ID] = true
		defer delete(resolving, g.ID)

		var children []domain.Status

		for _, m := range monitorsByGroup[g.ID] {
			hb, err := s.hbRepo.GetLatest(ctx, m.ID)
			if err != nil {
				// No heartbeat yet (never checked) — contributes no status.
				continue
			}
			children = append(children, hb.Status)
		}

		for _, cg := range childGroups[g.ID] {
			if status, ok := resolve(cg); ok {
				children = append(children, status)
			}
		}

		status, ok := g.Rollup(children)
		visited[g.ID] = true
		if ok {
			result[g.ID] = status
		}
		return status, ok
	}

	for _, g := range groups {
		resolve(g)
	}

	return result, nil
}
