package probe

import "errors"

func validateConfigReferences(snapshot ConfigSnapshot) error {
	assignments, err := configIndex(snapshot.Assignments, func(a ConfigAssignment) int64 { return a.MonitorID })
	if err != nil {
		return err
	}
	channels, err := configIndex(snapshot.NotificationChannels, func(c ConfigChannel) int64 { return c.ID })
	if err != nil {
		return err
	}
	templates, err := configIndex(snapshot.NotificationTemplates, func(t ConfigTemplate) int64 { return t.ID })
	if err != nil {
		return err
	}
	windows, err := configIndex(snapshot.MaintenanceWindows, func(w ConfigMaintenance) int64 { return w.ID })
	if err != nil {
		return err
	}
	policies, err := configIndex(snapshot.EscalationPolicies, func(p ConfigEscalation) int64 { return p.ID })
	if err != nil {
		return err
	}
	proxies, err := configIndex(snapshot.ProxyBindings, func(p ConfigProxy) string { return p.BindingKey })
	if err != nil {
		return err
	}
	for _, template := range snapshot.NotificationTemplates {
		if template.Version != snapshot.Revision {
			return errors.New("template version differs from snapshot revision")
		}
	}
	for _, channel := range snapshot.NotificationChannels {
		if channel.Version != snapshot.Revision {
			return errors.New("channel version differs from snapshot revision")
		}
		if channel.TemplateID != nil {
			template, exists := templates[*channel.TemplateID]
			if !exists || template.Provider != channel.Type {
				return errors.New("channel template is missing or has a different provider")
			}
		}
	}
	for _, proxy := range snapshot.ProxyBindings {
		if proxy.Version != snapshot.Revision {
			return errors.New("proxy version differs from snapshot revision")
		}
	}
	for _, policy := range snapshot.EscalationPolicies {
		if policy.Version != snapshot.Revision {
			return errors.New("policy version differs from snapshot revision")
		}
		for _, step := range policy.Steps {
			if !configReferencesExist(step.NotificationIDs, channels) {
				return errors.New("escalation channel is missing")
			}
		}
	}
	if !configReferencesExist(snapshot.Watchdog.NotificationIDs, channels) {
		return errors.New("watchdog channel is missing")
	}
	// Compare maintenance edges in O(assignments + edges), not a nested fleet scan.
	edges := make(map[[2]int64]bool)
	for _, assignment := range snapshot.Assignments {
		links, err := configIndex(assignment.NotificationLinks, func(l ConfigNotificationLink) int64 { return l.NotificationID })
		if err != nil {
			return err
		}
		if len(links) != len(assignment.NotificationIDs) || !configReferencesExist(assignment.NotificationIDs, links) || !configReferencesExist(assignment.NotificationIDs, channels) {
			return errors.New("assignment channel references or target-visibility links disagree")
		}
		if assignment.ProxyBindingKey != nil {
			if _, exists := proxies[*assignment.ProxyBindingKey]; !exists {
				return errors.New("assignment proxy binding is missing")
			}
		}
		if assignment.EscalationPolicyID != nil {
			if _, exists := policies[*assignment.EscalationPolicyID]; !exists {
				return errors.New("assignment escalation policy is missing")
			}
		}
		if !configReferencesExist(assignment.MaintenanceIDs, windows) {
			return errors.New("assignment maintenance window is missing")
		}
		for _, id := range assignment.MaintenanceIDs {
			edges[[2]int64{assignment.MonitorID, id}] = true
		}
	}
	for _, window := range snapshot.MaintenanceWindows {
		if !configReferencesExist(window.MonitorIDs, assignments) {
			return errors.New("maintenance references an unassigned monitor")
		}
		for _, id := range window.MonitorIDs {
			edge := [2]int64{id, window.ID}
			if !edges[edge] {
				return errors.New("maintenance applicability differs from assignment links")
			}
			delete(edges, edge)
		}
	}
	if len(edges) != 0 {
		return errors.New("assignment maintenance links lack reverse applicability")
	}
	if len(configCapabilities(snapshot)) > MaxCapabilities {
		return errors.New("config capability union exceeds limit")
	}
	return nil
}

func configIndex[T any, K comparable](entries []T, key func(T) K) (map[K]T, error) {
	index := make(map[K]T, len(entries))
	for _, entry := range entries {
		id := key(entry)
		if _, exists := index[id]; exists {
			return nil, errors.New("duplicate configuration identity")
		}
		index[id] = entry
	}
	return index, nil
}

func configReferencesExist[T any](ids []int64, entries map[int64]T) bool {
	for _, id := range ids {
		if _, exists := entries[id]; !exists {
			return false
		}
	}
	return true
}

func configCapabilities(snapshot ConfigSnapshot) map[string]bool {
	required := map[string]bool{"snapshot.v1": true}
	for _, assignment := range snapshot.Assignments {
		for _, capability := range assignment.RequiredCapabilities {
			required[capability] = true
		}
	}
	for _, channel := range snapshot.NotificationChannels {
		required["notifier."+channel.Type+".v1"] = true
	}
	return required
}
