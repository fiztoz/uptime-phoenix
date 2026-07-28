package services

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// TagService handles tag CRUD and monitor-tag assignments.
type TagService struct {
	tagRepo        ports.TagRepository
	monitorTagRepo ports.MonitorTagRepository
}

// NewTagService creates a new TagService.
func NewTagService(tagRepo ports.TagRepository, monitorTagRepo ports.MonitorTagRepository) *TagService {
	return &TagService{tagRepo: tagRepo, monitorTagRepo: monitorTagRepo}
}

// Create creates a new tag.
func (s *TagService) Create(ctx context.Context, t *domain.Tag) error {
	return s.tagRepo.Create(ctx, t)
}

// GetByID retrieves a tag by its ID.
func (s *TagService) GetByID(ctx context.Context, id int64) (*domain.Tag, error) {
	return s.tagRepo.GetByID(ctx, id)
}

// List retrieves all tags.
func (s *TagService) List(ctx context.Context) ([]*domain.Tag, error) {
	return s.tagRepo.List(ctx)
}

// Update updates a tag.
func (s *TagService) Update(ctx context.Context, t *domain.Tag) error {
	return s.tagRepo.Update(ctx, t)
}

// Delete deletes a tag by its ID.
func (s *TagService) Delete(ctx context.Context, id int64) error {
	return s.tagRepo.Delete(ctx, id)
}

// AssignTagToMonitor assigns a tag (with optional value) to a monitor.
func (s *TagService) AssignTagToMonitor(ctx context.Context, monitorID, tagID int64, value string) error {
	return s.monitorTagRepo.Assign(ctx, monitorID, tagID, value)
}

// RemoveTagFromMonitor removes a tag assignment from a monitor.
func (s *TagService) RemoveTagFromMonitor(ctx context.Context, monitorID, tagID int64) error {
	return s.monitorTagRepo.Remove(ctx, monitorID, tagID)
}

// ListMonitorTags lists the raw assignments for a monitor.
func (s *TagService) ListMonitorTags(ctx context.Context, monitorID int64) ([]*domain.MonitorTag, error) {
	return s.monitorTagRepo.ListByMonitor(ctx, monitorID)
}

// ListTagsForMonitor returns the full Tag objects assigned to a monitor.
func (s *TagService) ListTagsForMonitor(ctx context.Context, monitorID int64) ([]*domain.Tag, error) {
	links, err := s.monitorTagRepo.ListByMonitor(ctx, monitorID)
	if err != nil {
		return nil, err
	}
	tags := make([]*domain.Tag, 0, len(links))
	for _, link := range links {
		t, err := s.tagRepo.GetByID(ctx, link.TagID)
		if err != nil {
			continue
		}
		tags = append(tags, t)
	}
	return tags, nil
}

// MonitorTagDetail is one tag as it appears ON a monitor: the tag definition
// (id/name/color) joined with the per-monitor value from the assignment row.
//
// It exists because neither domain type alone is enough for the wire: domain.Tag
// has the name and color but no value, domain.MonitorTag has the value but only a
// tag id. Every caller that renders a monitor's tags needs both halves.
type MonitorTagDetail struct {
	TagID int64
	Name  string
	Color string
	Value string
}

// TagsForMonitors returns the tags of many monitors at once, keyed by monitor id.
//
// Two queries total regardless of how many monitors are passed — the batch
// assignment lookup plus one full tag-definition list — instead of the N+1 that
// calling ListTagsForMonitor in a loop would produce. Monitors with no tags are
// absent from the map; callers must render them as an empty array, never null.
// An assignment whose tag definition has since been deleted is skipped rather
// than emitted with a blank name.
func (s *TagService) TagsForMonitors(ctx context.Context, monitorIDs []int64) (map[int64][]MonitorTagDetail, error) {
	out := make(map[int64][]MonitorTagDetail, len(monitorIDs))
	if len(monitorIDs) == 0 {
		return out, nil
	}
	links, err := s.monitorTagRepo.ListByMonitors(ctx, monitorIDs)
	if err != nil {
		return nil, err
	}
	if len(links) == 0 {
		return out, nil
	}

	tags, err := s.tagRepo.List(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]*domain.Tag, len(tags))
	for _, t := range tags {
		byID[t.ID] = t
	}

	for monitorID, assignments := range links {
		for _, a := range assignments {
			t, ok := byID[a.TagID]
			if !ok {
				continue // tag deleted out from under the assignment
			}
			out[monitorID] = append(out[monitorID], MonitorTagDetail{
				TagID: t.ID,
				Name:  t.Name,
				Color: t.Color,
				Value: a.Value,
			})
		}
	}
	return out, nil
}

// TagsForMonitor is the single-monitor form of TagsForMonitors.
func (s *TagService) TagsForMonitor(ctx context.Context, monitorID int64) ([]MonitorTagDetail, error) {
	byMonitor, err := s.TagsForMonitors(ctx, []int64{monitorID})
	if err != nil {
		return nil, err
	}
	return byMonitor[monitorID], nil
}
