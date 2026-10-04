package repository

import (
	"context"
	"slices"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func replacementResourceBindings(ctx context.Context, tx bun.Tx, monitorID int64, ids []string, previous []probeAssignmentModel, bindings []domain.ProbeAssignmentBinding) (map[string]domain.ProbeResourceBinding, bool, error) {
	var monitorType string
	if err := tx.NewSelect().Table("monitors").Column("type").Where("id = ?", monitorID).Scan(ctx, &monitorType); err != nil {
		return nil, false, err
	}
	resources := make(map[string]domain.ProbeResourceBinding)
	if bindings == nil {
		for _, row := range previous {
			if row.Active && slices.Contains(ids, row.ProbeID) && row.ResourceBindingKey != "" {
				resources[row.ProbeID] = domain.ProbeResourceBinding{BindingKey: row.ResourceBindingKey, Kind: row.ResourceBindingKind}
			}
		}
	} else {
		for _, binding := range bindings {
			if _, exists := resources[binding.ProbeID]; exists || binding.ProbeID == domain.LocalProbeID || !slices.Contains(ids, binding.ProbeID) || !domain.ValidProbeResourceBinding(binding.ProbeResourceBinding) {
				return nil, false, domain.ErrValidation
			}
			resources[binding.ProbeID] = binding.ProbeResourceBinding
		}
	}
	for _, id := range ids {
		binding, present := resources[id]
		if (monitorType == "docker" && id != domain.LocalProbeID) != present || present && !domain.ValidProbeResourceBinding(binding) || monitorType == "push" && id != domain.LocalProbeID {
			return nil, false, domain.ErrValidation
		}
	}
	changed := false
	for _, row := range previous {
		if row.Active && slices.Contains(ids, row.ProbeID) {
			binding := resources[row.ProbeID]
			changed = changed || binding.BindingKey != row.ResourceBindingKey || binding.Kind != row.ResourceBindingKind
		}
	}
	return resources, changed, nil
}
