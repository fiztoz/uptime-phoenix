package services

import (
	"context"
	"errors"
	"fmt"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// Assignment write errors. The HTTP layer maps these to the protocol section
// 7.3 statuses: 400 malformed input, 409 revision/identity conflicts, and 422
// a supported request with an invalid configuration.
var (
	ErrStaleRevision         = errors.New("stale expected revision")
	ErrUnknownProbe          = errors.New("unknown probe registration")
	ErrProbeUnavailable      = errors.New("probe registration is disabled")
	ErrUnsupportedAssignment = errors.New("unsupported assignment")
	ErrInvalidProbeIDs       = errors.New("invalid probe member set")
	ErrInvalidPolicy         = errors.New("invalid health policy")
	ErrInvalidDelivery       = errors.New("invalid alert delivery")
	ErrInvalidBindings       = errors.New("invalid resource bindings")
	ErrRemoteCloneForbidden  = errors.New("cloning a remote monitor requires admin")
)

// ProbeAssignmentRequest is one complete desired-set replacement: membership,
// policy and (optionally) resource bindings. Bindings is nil when the request
// omitted the field — retained members keep their bindings and new members get
// none. A non-nil (possibly empty) list is explicit and replaces every binding.
type ProbeAssignmentRequest struct {
	ExpectedRevision int64
	ProbeIDs         []string
	HealthPolicy     domain.HealthPolicy
	AlertDelivery    string
	Bindings         *[]domain.ProbeAssignmentBinding
}

// InitialAssignments is the optional complete desired set of a monitor create.
// The same membership, policy and binding rules apply; creation plus this set
// commits in one transaction.
type InitialAssignments struct {
	ProbeIDs     []string
	HealthPolicy domain.HealthPolicy
	Bindings     *[]domain.ProbeAssignmentBinding
}

// ProbeAssignmentWriteResult reports the committed desired set and which
// members this write left without provable application. PendingProbes are the
// members whose desired state changed in this write (added, re-bound, or the
// policy changed for everyone): no publication can cover that state yet, so
// their sync status must read pending in this response. A no-op write proves
// nothing new and leaves the map empty.
type ProbeAssignmentWriteResult struct {
	Set           domain.MonitorProbeAssignments
	PendingProbes map[string]bool
}

// ProbeAssignmentService is the revisioned live-operator write surface for
// desired assignment sets. It validates the complete set before any write,
// then commits through ProbeAssignmentWriter (live Replace semantics: every
// member must be a currently enabled registration). Authorization is the
// middleware's; this service never widens who may write.
type ProbeAssignmentService struct {
	writer       ports.ProbeAssignmentWriter
	assignments  ports.MonitorProbeAssignmentRepository
	registry     ports.ProbeRegistryRepository
	monitors     ports.MonitorRepository
	capabilities ports.ProbeAssignmentCapabilities
	fleet        FleetActivationGate
}

// NewProbeAssignmentService binds the assignment write dependencies. fleet is
// required to commit any set with a remote member and is consulted only then; a
// zero-value gate makes those writes fail closed instead of silently skipping
// the mixed-version check.
func NewProbeAssignmentService(
	writer ports.ProbeAssignmentWriter,
	assignments ports.MonitorProbeAssignmentRepository,
	registry ports.ProbeRegistryRepository,
	monitors ports.MonitorRepository,
	capabilities ports.ProbeAssignmentCapabilities,
	fleet FleetActivationGate,
) *ProbeAssignmentService {
	return &ProbeAssignmentService{writer: writer, assignments: assignments, registry: registry, monitors: monitors, capabilities: capabilities, fleet: fleet}
}

// Replace commits one complete desired set. expectedRevision is mandatory and
// positive; a stale precondition returns ErrStaleRevision and writes nothing.
func (s *ProbeAssignmentService) Replace(ctx context.Context, monitorID int64, req ProbeAssignmentRequest) (*ProbeAssignmentWriteResult, error) {
	if s == nil || s.writer == nil || s.assignments == nil || s.registry == nil || s.monitors == nil || s.capabilities == nil {
		return nil, domain.ErrInternal
	}
	if monitorID < 1 || req.ExpectedRevision < 1 {
		return nil, fmt.Errorf("invalid monitor or expected revision: %w", domain.ErrValidation)
	}
	monitor, err := s.monitors.GetByID(ctx, monitorID)
	if err != nil {
		return nil, err
	}
	// A legacy monitor without an assignment row reads ErrNotFound here. The
	// writer initializes the set inside its own transaction; revision zero is
	// never a valid precondition (clients pass the post-initialization one).
	previous, err := s.assignments.GetByMonitorID(ctx, monitorID)
	if err != nil && !errors.Is(err, ports.ErrNotFound) && !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}
	if previous != nil && previous.Revision != req.ExpectedRevision {
		return nil, ErrStaleRevision
	}
	if previous == nil && req.ExpectedRevision != 1 {
		return nil, ErrStaleRevision
	}
	if err := ValidateDesiredAssignments(ctx, monitor, previous, req.ProbeIDs, req.HealthPolicy, req.Bindings, req.AlertDelivery, s.registry, s.capabilities); err != nil {
		return nil, err
	}
	// T34: a set with a remote member makes this monitor remote-executed, so the
	// whole live fleet must already honor assignment ownership. Checked after
	// input validation and before any write, so a refusal leaves no partial state.
	if err := s.fleet.EnsureRemoteActivationAllowed(ctx, req.ProbeIDs); err != nil {
		return nil, err
	}
	var bindings []domain.ProbeAssignmentBinding
	if req.Bindings != nil {
		bindings = *req.Bindings
	}
	result, err := s.writer.ReplaceWithBindings(ctx, monitorID, req.ExpectedRevision, req.ProbeIDs, req.HealthPolicy, bindings)
	if err != nil {
		if errors.Is(err, ports.ErrConflict) {
			return nil, fmt.Errorf("replace assignments: %w", ErrStaleRevision)
		}
		return nil, err
	}
	return &ProbeAssignmentWriteResult{Set: *result, PendingProbes: pendingAfterWrite(previous, *result)}, nil
}

// ValidateDesiredAssignments enforces the complete-set write policy before any
// commit: canonical distinct members, a supported policy and delivery mode,
// well-formed bindings bound to members, every member registered and enabled,
// and a configuration this build can actually execute remotely — push is
// local-only, remote members need an installed pull checker, a Docker monitor's
// remote members need their explicit resource binding, and a monitor that
// references an outbound proxy needs a checker that dials one. Remote probe
// advertisements stay a publication/activation concern.
func ValidateDesiredAssignments(
	ctx context.Context,
	monitor *domain.Monitor,
	previous *domain.MonitorProbeAssignments,
	probeIDs []string,
	policy domain.HealthPolicy,
	bindings *[]domain.ProbeAssignmentBinding,
	alertDelivery string,
	registry ports.ProbeRegistryRepository,
	capabilities ports.ProbeAssignmentCapabilities,
) error {
	if monitor == nil || registry == nil || capabilities == nil {
		return domain.ErrInternal
	}
	if len(probeIDs) == 0 {
		return fmt.Errorf("empty member set: %w", ErrInvalidProbeIDs)
	}
	if policy != domain.HealthPolicyAnyDown && policy != domain.HealthPolicyAllDown {
		return fmt.Errorf("unsupported policy %q: %w", policy, ErrInvalidPolicy)
	}
	if alertDelivery != "" && alertDelivery != string(domain.AlertDeliveryRegional) {
		return fmt.Errorf("unsupported alert delivery %q: %w", alertDelivery, ErrInvalidDelivery)
	}
	seen := make(map[string]bool, len(probeIDs))
	for _, id := range probeIDs {
		if (id != domain.LocalProbeID && !domain.ValidHubID(id)) || seen[id] {
			return fmt.Errorf("invalid or duplicate probe %q: %w", id, ErrInvalidProbeIDs)
		}
		seen[id] = true
	}
	resources := map[string]domain.ProbeResourceBinding{}
	if bindings != nil {
		for _, binding := range *bindings {
			if binding.ProbeID == domain.LocalProbeID || !seen[binding.ProbeID] ||
				!domain.ValidProbeResourceBinding(binding.ProbeResourceBinding) || resources[binding.ProbeID].BindingKey != "" {
				return fmt.Errorf("invalid binding for %q: %w", binding.ProbeID, ErrInvalidBindings)
			}
			resources[binding.ProbeID] = binding.ProbeResourceBinding
		}
	} else if previous != nil {
		// Omitted bindings preserve retained members' bindings; new members get none.
		for _, member := range previous.Assignments {
			if member.ResourceBinding != nil && seen[member.ProbeID] {
				resources[member.ProbeID] = *member.ResourceBinding
			}
		}
	}
	for _, id := range probeIDs {
		probe, err := registry.GetByID(ctx, id)
		if err != nil {
			if errors.Is(err, ports.ErrNotFound) || errors.Is(err, domain.ErrNotFound) {
				return fmt.Errorf("probe %q: %w", id, ErrUnknownProbe)
			}
			return err
		}
		if !probe.Enabled {
			return fmt.Errorf("probe %q: %w", id, ErrProbeUnavailable)
		}
		_, bound := resources[id]
		if id == domain.LocalProbeID {
			if bound {
				return fmt.Errorf("local probe cannot carry a resource binding: %w", ErrInvalidBindings)
			}
			continue
		}
		if !capabilities.RemoteCapable(monitor.Type) {
			return fmt.Errorf("monitor type %q cannot run remotely: %w", monitor.Type, ErrUnsupportedAssignment)
		}
		if monitor.Type == "docker" && !bound {
			return fmt.Errorf("remote docker assignment for %q needs a resource binding: %w", id, ErrUnsupportedAssignment)
		}
		if monitor.Type != "docker" && bound {
			return fmt.Errorf("resource binding on %q for monitor type %q: %w", id, monitor.Type, ErrUnsupportedAssignment)
		}
		if monitor.ProxyID != nil && !capabilities.ProxyCapable(monitor.Type) {
			return fmt.Errorf("monitor type %q ignores the configured proxy: %w", monitor.Type, ErrUnsupportedAssignment)
		}
	}
	return nil
}

// pendingAfterWrite marks members whose desired state this write changed: new
// members and members whose binding changed, plus everyone when the policy
// changed. A no-op write (revision preserved) marks nobody.
func pendingAfterWrite(previous *domain.MonitorProbeAssignments, result domain.MonitorProbeAssignments) map[string]bool {
	pending := map[string]bool{}
	if previous == nil || result.Revision == previous.Revision {
		// Legacy initialization or no-op: only an unchanged set keeps its
		// proven application. A legacy monitor's first write always produces
		// unproven desired state for its remote members.
		if previous == nil {
			for _, member := range result.Assignments {
				if member.ProbeID != domain.LocalProbeID {
					pending[member.ProbeID] = true
				}
			}
		}
		return pending
	}
	if previous.HealthPolicy != result.HealthPolicy {
		for _, member := range result.Assignments {
			pending[member.ProbeID] = true
		}
		return pending
	}
	retained := make(map[string]domain.ProbeAssignment, len(previous.Assignments))
	for _, member := range previous.Assignments {
		retained[member.ProbeID] = member
	}
	for _, member := range result.Assignments {
		before, existed := retained[member.ProbeID]
		if !existed || !sameResourceBinding(before.ResourceBinding, member.ResourceBinding) {
			pending[member.ProbeID] = true
		}
	}
	return pending
}

func sameResourceBinding(a, b *domain.ProbeResourceBinding) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// CreateWithAssignments creates a monitor together with its explicit initial
// desired set atomically. Omitting the set keeps today's local default. A
// non-admin caller must never reach this path with remote members: the handler
// enforces that before calling.
func (s *MonitorService) CreateWithAssignments(ctx context.Context, m *domain.Monitor, initial InitialAssignments) error {
	if s == nil || s.assignWriter == nil || s.probeRegistry == nil || s.probeCapabilities == nil {
		return domain.ErrInternal
	}
	if err := ValidateDesiredAssignments(ctx, m, nil, initial.ProbeIDs, initial.HealthPolicy, initial.Bindings, "", s.probeRegistry, s.probeCapabilities); err != nil {
		return err
	}
	// T34, same rule as Replace: creating a monitor that is remote-executed from
	// birth is a remote activation and needs an assignment-aware fleet. This also
	// covers Clone, which reproduces a remote source set through this path.
	if err := s.fleetGate.EnsureRemoteActivationAllowed(ctx, initial.ProbeIDs); err != nil {
		return err
	}
	normalizeHTTPMonitorURL(m)
	if m.Weight == 0 {
		m.Weight = 2000
	}
	if err := s.validateGroup(ctx, m); err != nil {
		return err
	}
	if err := s.validateProxy(ctx, m); err != nil {
		return err
	}
	var bindings []domain.ProbeAssignmentBinding
	if initial.Bindings != nil {
		bindings = *initial.Bindings
	}
	if _, err := s.assignWriter.CreateMonitorWithAssignments(ctx, m, initial.ProbeIDs, initial.HealthPolicy, bindings); err != nil {
		return fmt.Errorf("monitor service: create: %w", err)
	}
	if err := s.attachDefaultNotifications(ctx, m); err != nil {
		return fmt.Errorf("monitor service: create: attach defaults: %w", err)
	}
	_ = s.bus.Publish(ctx, ports.Event{Type: "monitor.update", Payload: m})
	return nil
}

// SetAssignmentProvisioning wires the atomic create-with-assignments path.
// fleet gates remote members on fleet-wide assignment ownership (T34); leaving
// it zero makes a create with remote members fail closed.
//
// Deliberately not applied to Restore: a backup import or config apply must
// succeed while the fleet is degraded or mid-rollout, and restored identities
// are created disabled pending reenrollment, so a restore does not hand live
// work to a remote probe.
func (s *MonitorService) SetAssignmentProvisioning(writer ports.ProbeAssignmentWriter, registry ports.ProbeRegistryRepository, capabilities ports.ProbeAssignmentCapabilities, fleet FleetActivationGate) {
	s.assignWriter = writer
	s.probeRegistry = registry
	s.probeCapabilities = capabilities
	s.fleetGate = fleet
}

// SetAssignmentReader wires assignment reads so Clone can honor the source
// monitor's desired set instead of silently rerouting it to local execution.
func (s *MonitorService) SetAssignmentReader(assignments ports.MonitorProbeAssignmentRepository) {
	s.assignReader = assignments
}

// Clone copies a monitor. A clone of a remotely assigned monitor is rejected
// for a non-admin rather than silently rerouted to local execution; an admin
// clone reproduces the source's complete desired set atomically.
func (s *MonitorService) Clone(ctx context.Context, id, userID int64, isAdmin bool) (*domain.Monitor, error) {
	src, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("monitor service: clone: %w", err)
	}
	if src.UserID != userID {
		return nil, domain.ErrNotFound
	}
	var source *domain.MonitorProbeAssignments
	if s.assignReader != nil {
		source, err = s.assignReader.GetByMonitorID(ctx, id)
		if err != nil && !errors.Is(err, ports.ErrNotFound) && !errors.Is(err, domain.ErrNotFound) {
			return nil, fmt.Errorf("monitor service: clone: %w", err)
		}
	}
	remote := false
	if source != nil {
		for _, member := range source.Assignments {
			if member.ProbeID != domain.LocalProbeID {
				remote = true
			}
		}
	}
	if remote && !isAdmin {
		return nil, ErrRemoteCloneForbidden
	}
	clone := *src
	clone.ID = 0
	clone.Name = src.Name + " (copy)"
	clone.UserID = userID
	clone.PushToken = ""
	// clone.GroupID is intentionally left as copied from src: cloning a
	// monitor filed under a folder should produce a copy that stays in that
	// same folder, not one that gets kicked out to top-level.
	if clone.Config != nil {
		cfg := make(map[string]any, len(clone.Config))
		for k, v := range clone.Config {
			cfg[k] = v
		}
		if clone.Type == "push" {
			token, err := generatePushToken()
			if err != nil {
				return nil, fmt.Errorf("monitor service: clone: generate push token: %w", err)
			}
			cfg["push_token"] = token
			clone.PushToken = token
		} else {
			delete(cfg, "push_token")
		}
		clone.Config = cfg
	}
	if source != nil && s.assignWriter != nil {
		members := make([]string, 0, len(source.Assignments))
		bindings := make([]domain.ProbeAssignmentBinding, 0, len(source.Assignments))
		for _, member := range source.Assignments {
			members = append(members, member.ProbeID)
			if member.ResourceBinding != nil {
				bindings = append(bindings, domain.ProbeAssignmentBinding{ProbeID: member.ProbeID, ProbeResourceBinding: *member.ResourceBinding})
			}
		}
		if err := s.CreateWithAssignments(ctx, &clone, InitialAssignments{ProbeIDs: members, HealthPolicy: source.HealthPolicy, Bindings: &bindings}); err != nil {
			return nil, fmt.Errorf("monitor service: clone: %w", err)
		}
		return &clone, nil
	}
	if err := s.Create(ctx, &clone); err != nil {
		return nil, fmt.Errorf("monitor service: clone: %w", err)
	}
	return &clone, nil
}
