package services

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// LocalProbeConfigBuilder builds prepared local snapshots from saved hub state.
// A successful preparation grants no execution or provider-delivery authority.
type LocalProbeConfigBuilder struct {
	source   ports.LocalProbeConfigSourceRepository
	encoder  ports.LocalProbeConfigEncoder
	prepared *ProbeConfigService
}

// NewLocalProbeConfigBuilder requires an authoritative source and protected store.
func NewLocalProbeConfigBuilder(source ports.LocalProbeConfigSourceRepository, encoder ports.LocalProbeConfigEncoder, prepared *ProbeConfigService) *LocalProbeConfigBuilder {
	return &LocalProbeConfigBuilder{source: source, encoder: encoder, prepared: prepared}
}

// Prepare builds expectedRevision+1 from a consistent source read. The caller
// supplies trusted hub identity and fixed timestamps for an exact retry. Concurrent
// source changes require a new revision; stale prepared content is never activated
// here. Ordinary callers receive metadata only, not credentials or document bytes.
func (b *LocalProbeConfigBuilder) Prepare(ctx context.Context, hubID string, expectedRevision int64, createdAt, effectiveAt time.Time) (domain.ProbeConfigMetadata, error) {
	if err := ctx.Err(); err != nil {
		return domain.ProbeConfigMetadata{}, err
	}
	target := domain.ProbeConfigTarget{HubID: hubID, ProbeID: domain.LocalProbeID}
	if b == nil || b.source == nil || b.encoder == nil || b.prepared == nil ||
		!domain.ValidProbeConfigTarget(target) || expectedRevision < 0 || createdAt.IsZero() || effectiveAt.IsZero() {
		return domain.ProbeConfigMetadata{}, domain.ErrValidation
	}
	if expectedRevision == math.MaxInt64 {
		return domain.ProbeConfigMetadata{}, ports.ErrConflict
	}
	source, err := b.source.ReadLocal(ctx)
	if err != nil {
		return domain.ProbeConfigMetadata{}, configBuildError(ctx, "read configuration source", err)
	}
	definition, err := ResolveLocalProbeConfig(source)
	if err != nil {
		return domain.ProbeConfigMetadata{}, err
	}
	definition.Target, definition.Revision = target, expectedRevision+1
	definition.CreatedAt = createdAt.UTC().Truncate(time.Microsecond)
	definition.EffectiveAt = effectiveAt.UTC().Truncate(time.Microsecond)
	document, err := b.encoder.EncodeLocal(definition)
	if err != nil {
		return domain.ProbeConfigMetadata{}, configBuildError(ctx, "encode configuration source", err)
	}
	return b.prepared.Prepare(ctx, target, document, expectedRevision)
}

func configBuildError(ctx context.Context, operation string, cause error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// SQL/extension decoder errors can include confidential values. Only a fixed
	// operation and sentinel cross this internal preparation boundary.
	for _, sentinel := range []error{domain.ErrValidation, ports.ErrNotFound, ports.ErrConflict, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(cause, sentinel) {
			return fmt.Errorf("%s: %w", operation, sentinel)
		}
	}
	return fmt.Errorf("%s: %w", operation, domain.ErrInternal)
}

// ResolveLocalProbeConfig resolves source dependencies and policies into a complete
// local probe configuration definition.
func ResolveLocalProbeConfig(source *domain.LocalProbeConfigSource) (domain.LocalProbeConfigDefinition, error) {
	var out domain.LocalProbeConfigDefinition
	if source == nil || source.Probe.ID != domain.LocalProbeID || source.Probe.Kind != domain.ProbeKindLocal || !source.Probe.Enabled {
		return out, domain.ErrValidation
	}
	monitors := make(map[int64]bool, len(source.Assignments))
	policies, channels, templates, proxies := map[int64]bool{}, map[int64]bool{}, map[int64]bool{}, map[int64]bool{}
	out.Assignments = make([]domain.ProbeConfigAssignment, 0, len(source.Assignments))
	for _, assignment := range source.Assignments {
		m := assignment.Monitor
		if m == nil || m.ID <= 0 || assignment.Generation <= 0 || monitors[m.ID] {
			return out, domain.ErrValidation
		}
		monitors[m.ID] = true
		assignment.EffectiveOwner = m.EffectiveOwner(source.Groups)
		assignment.MaintenanceIDs = []int64{}
		assignment.EscalationPolicyID = nil
		policyID, err := configPolicyForMonitor(source, m)
		if err != nil {
			return out, err
		}
		if policyID != 0 {
			assignment.EscalationPolicyID = &policyID
			policies[policyID] = true
		}
		for _, link := range assignment.NotificationLinks {
			if link.MonitorID != m.ID {
				return out, domain.ErrValidation
			}
			channels[link.NotificationID] = true
		}
		if m.ProxyID != nil {
			proxies[*m.ProxyID] = true
		}
		out.Assignments = append(out.Assignments, assignment)
	}
	for _, id := range sortedConfigIDs(policies) {
		policy := source.Policies[id]
		if policy == nil || policy.ID != id {
			return out, domain.ErrValidation
		}
		// Disabled/empty policies still suppress inheritance and retain references.
		out.Policies = append(out.Policies, policy)
		for _, step := range policy.Steps {
			for _, channelID := range step.NotificationIDs {
				channels[channelID] = true
			}
		}
	}
	for _, id := range sortedConfigIDs(channels) {
		channel := source.Notifications[id]
		if channel == nil || channel.ID != id {
			return out, domain.ErrValidation
		}
		out.Notifications = append(out.Notifications, channel)
		if channel.TemplateID != nil {
			templates[*channel.TemplateID] = true
		}
	}
	for _, id := range sortedConfigIDs(templates) {
		template := source.Templates[id]
		if template == nil || template.ID != id {
			return out, domain.ErrValidation
		}
		out.Templates = append(out.Templates, template)
	}
	for _, id := range sortedConfigIDs(proxies) {
		proxy := source.Proxies[id]
		if proxy == nil || proxy.ID != id {
			return out, domain.ErrValidation
		}
		out.Proxies = append(out.Proxies, proxy)
	}
	if err := resolveConfigMaintenance(source.Maintenance, monitors, &out); err != nil {
		return out, err
	}
	slices.SortFunc(out.Assignments, func(a, b domain.ProbeConfigAssignment) int { return configIDCompare(a.Monitor.ID, b.Monitor.ID) })
	return out, nil
}

func configPolicyForMonitor(source *domain.LocalProbeConfigSource, m *domain.Monitor) (int64, error) {
	if id, exists := source.MonitorPolicies[m.ID]; exists {
		if id <= 0 {
			return 0, domain.ErrValidation
		}
		return id, nil
	}
	visited := map[int64]bool{}
	for groupID := m.GroupID; groupID != nil; {
		if visited[*groupID] || len(visited) >= 64 {
			return 0, domain.ErrValidation
		}
		visited[*groupID] = true
		group := source.Groups[*groupID]
		if group == nil {
			return 0, domain.ErrValidation
		}
		if id, exists := source.GroupPolicies[*groupID]; exists {
			if id <= 0 {
				return 0, domain.ErrValidation
			}
			return id, nil
		}
		groupID = group.ParentID
	}
	return 0, nil
}

func resolveConfigMaintenance(windows []domain.ProbeConfigMaintenance, monitors map[int64]bool, out *domain.LocalProbeConfigDefinition) error {
	byMonitor := make(map[int64][]int64)
	seen := make(map[int64]bool)
	for _, entry := range windows {
		if entry.Window == nil || entry.Window.ID <= 0 || seen[entry.Window.ID] {
			return domain.ErrValidation
		}
		seen[entry.Window.ID] = true
		ids := map[int64]bool{}
		for _, id := range entry.MonitorIDs {
			if monitors[id] {
				ids[id] = true
			}
		}
		if len(ids) == 0 {
			continue
		}
		window := *entry.Window
		if window.Timezone == "" {
			window.Timezone = "UTC"
		}
		entry = domain.ProbeConfigMaintenance{Window: &window, MonitorIDs: sortedConfigIDs(ids)}
		out.Maintenance = append(out.Maintenance, entry)
		for _, id := range entry.MonitorIDs {
			byMonitor[id] = append(byMonitor[id], window.ID)
		}
	}
	for i := range out.Assignments {
		ids := byMonitor[out.Assignments[i].Monitor.ID]
		slices.Sort(ids)
		out.Assignments[i].MaintenanceIDs = append([]int64{}, ids...)
	}
	slices.SortFunc(out.Maintenance, func(a, b domain.ProbeConfigMaintenance) int { return configIDCompare(a.Window.ID, b.Window.ID) })
	return nil
}

func sortedConfigIDs(values map[int64]bool) []int64 {
	ids := make([]int64, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func configIDCompare(a, b int64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
