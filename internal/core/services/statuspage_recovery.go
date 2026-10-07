package services

import (
	"context"
	"fmt"
	"log/slog"
)

// RegionalRecoveryCandidates filters one committed evidence batch to monitors
// linked to auto-resolving pages with active incidents. Nothing is cached or
// resolved here: AutoResolveOnRecovery still reads fresh policy, links and
// incidents before applying its existing recovery and notification behavior.
func (s *StatusPageService) RegionalRecoveryCandidates(ctx context.Context, monitorIDs []int64) ([]int64, error) {
	wanted := make(map[int64]bool, len(monitorIDs))
	for _, id := range monitorIDs {
		if id > 0 {
			wanted[id] = true
		}
	}
	if len(wanted) == 0 {
		return nil, nil
	}
	pages, err := s.repo.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("status page service: recovery candidates: list pages: %w", err)
	}
	eligible := make(map[int64]bool)
	for _, page := range pages {
		if !page.AutoResolveIncidents {
			continue
		}
		links, err := s.spMonitorRepo.ListByStatusPage(ctx, page.ID)
		if err != nil {
			slog.ErrorContext(ctx, "status page service: recovery candidates: list monitors", "status_page_id", page.ID, "error", err)
			continue
		}
		var linked []int64
		for _, link := range links {
			if wanted[link.MonitorID] {
				linked = append(linked, link.MonitorID)
			}
		}
		if len(linked) == 0 {
			continue
		}
		incidents, err := s.incidentRepo.ListByStatusPage(ctx, page.ID)
		if err != nil {
			slog.ErrorContext(ctx, "status page service: recovery candidates: list incidents", "status_page_id", page.ID, "error", err)
			continue
		}
		for _, incident := range incidents {
			if incident.Active {
				for _, id := range linked {
					eligible[id] = true
				}
				break
			}
		}
	}
	// Preserve source order and the caller's full slice for browser/group fan-out.
	ids := make([]int64, 0, len(eligible))
	for _, id := range monitorIDs {
		if eligible[id] {
			ids = append(ids, id)
			delete(eligible, id)
		}
	}
	return ids, nil
}
