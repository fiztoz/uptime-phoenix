package services

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ProbeWatchdogInput is one ordered owner event. Elapsed retains the process
// monotonic clock; At is source wall time for durable history only. A health frame
// requires HealthGeneration authority. Disconnect is a local interruption and
// must never be used to disguise a rejected health frame as an unfenced tick.
type ProbeWatchdogInput struct {
	Elapsed    time.Duration
	At         time.Time
	Health     *bool
	Disconnect bool
}

// ProbeWatchdogSource serializes one owner's timer proposals with source storage.
// The caller owns event ordering and lifetime. A failed transaction cannot publish
// a speculative healthy streak or overwrite a concurrently acknowledged incident.
type ProbeWatchdogSource struct {
	repo      ports.ProbeWatchdogRepository
	peer      string
	timer     *ProbeWatchdogTimer
	timing    domain.ProbeWatchdogTiming
	persisted domain.ProbeWatchdogState
	authority domain.ProbeWatchdogAuthority
	bound     bool
}

// NewProbeWatchdogSource creates a source owner for the hub or probe peer.
func NewProbeWatchdogSource(repo ports.ProbeWatchdogRepository, peer string) (*ProbeWatchdogSource, error) {
	if repo == nil || peer != "hub" && peer != "probe" {
		return nil, domain.ErrValidation
	}
	return &ProbeWatchdogSource{repo: repo, peer: peer}, nil
}

// Step commits progress, lifecycle and delivery intents together. A graph must be
// durably applied before reaching this method; the repository rechecks its revision
// and current ownership. Only successful commits change the process-local timer.
func (s *ProbeWatchdogSource) Step(ctx context.Context, authority domain.ProbeWatchdogAuthority, config *domain.EdgeResolvedConfig, input ProbeWatchdogInput) (domain.ProbeWatchdogState, error) {
	if s == nil || s.repo == nil || config == nil || config.Metadata.Revision <= 0 ||
		config.Metadata.ProbeID != authority.ProbeID || config.Metadata.HubID != authority.HubID ||
		input.Elapsed < 0 || input.At.IsZero() || input.Health != nil && (authority.HealthGeneration <= 0 || input.Disconnect) ||
		domain.ValidateProbeWatchdogSettings(config.Watchdog) != nil {
		return domain.ProbeWatchdogState{}, domain.ErrValidation
	}
	owner := authority
	owner.HealthGeneration = 0
	if s.bound && s.authority != owner {
		return domain.ProbeWatchdogState{}, ports.ErrConflict
	}
	for range 3 {
		before, err := s.repo.ReadWatchdog(ctx, authority)
		if err != nil {
			return domain.ProbeWatchdogState{}, err
		}
		proposal, timer, err := s.propose(authority.ProbeID, before, config, input)
		if err != nil {
			return domain.ProbeWatchdogState{}, err
		}
		committed, err := s.repo.CommitWatchdog(ctx, authority, proposal)
		if errors.Is(err, ports.ErrStaleLocalState) {
			continue
		}
		if err != nil {
			return domain.ProbeWatchdogState{}, err
		}
		s.timer, s.timing, s.persisted = timer, config.Watchdog.Timing(), committed
		s.authority, s.bound = owner, true
		return committed, nil
	}
	return domain.ProbeWatchdogState{}, ports.ErrStaleLocalState
}

func (s *ProbeWatchdogSource) propose(probeID string, before domain.ProbeWatchdogState, config *domain.EdgeResolvedConfig, input ProbeWatchdogInput) (domain.ProbeWatchdogRecord, *ProbeWatchdogTimer, error) {
	at := input.At.UTC().Truncate(time.Microsecond)
	record := domain.ProbeWatchdogRecord{ExpectedVersion: before.Version, ConfigRevision: config.Metadata.Revision, At: at}
	open := before.Incident != nil && before.Incident.Status != domain.AlertStatusResolved
	timing := config.Watchdog.Timing()
	if !config.Watchdog.Enabled {
		timer, err := NewProbeWatchdogTimer(timing, input.Elapsed, false)
		if err != nil {
			return record, nil, err
		}
		record.Status = domain.ProbeWatchdogUnarmed
		if open {
			record.Incident, err = resolvedWatchdogIncident(before.Incident, config.Metadata.Revision, at, "Connection watchdog disabled by operator")
			if err != nil {
				return record, nil, err
			}
		}
		// Administrative disable closes source identity, without inventing an
		// application-health recovery or enqueuing an UP notification.
		return record, timer, nil
	}
	var timer *ProbeWatchdogTimer
	if s.timer == nil || timing != s.timing || before.Checkpoint != s.persisted.Checkpoint {
		var err error
		timer, err = RestoreProbeWatchdogTimer(timing, input.Elapsed, before.Checkpoint)
		if err != nil {
			return record, nil, err
		}
	} else {
		copy := *s.timer
		timer = &copy
	}
	if err := timer.Arm(input.Elapsed); err != nil {
		return record, nil, err
	}
	if input.Health != nil {
		if err := timer.Health(input.Elapsed, *input.Health); err != nil {
			return record, nil, err
		}
	} else if input.Disconnect {
		if err := timer.Disconnect(input.Elapsed); err != nil {
			return record, nil, err
		}
	}
	evaluation, err := timer.Evaluate(input.Elapsed, open)
	if err != nil {
		return record, nil, err
	}
	record.Status = evaluation.Status
	record.Checkpoint, err = timer.Checkpoint(input.Elapsed)
	if err != nil {
		return record, nil, err
	}
	incident, enqueue := before.Incident, false
	switch evaluation.Action {
	case domain.ProbeWatchdogOpen:
		id, err := newUUIDv4()
		if err != nil {
			return record, nil, err
		}
		incident = &domain.RegionalIncident{SourceAlertID: id, ProbeID: probeID, Scope: domain.IncidentScopeProbeConnection,
			SubjectKind: domain.IncidentSubjectWatchdog, Status: domain.AlertStatusFiring, TransitionVersion: 1,
			ConfigRevision: record.ConfigRevision, StartedAt: at, Reason: s.peer + " application health unavailable"}
		record.Incident, enqueue = incident, true
	case domain.ProbeWatchdogResolve:
		incident, err = resolvedWatchdogIncident(before.Incident, record.ConfigRevision, at, s.peer+" application health restored")
		if err != nil {
			return record, nil, err
		}
		record.Incident, enqueue = incident, true
	default:
		if evaluation.Status == domain.ProbeWatchdogLost && incident != nil && incident.Status == domain.AlertStatusFiring && config.Watchdog.ResendInterval > 0 {
			enqueue = before.LastEnqueuedAt == nil || at.Sub(*before.LastEnqueuedAt) >= time.Duration(config.Watchdog.ResendInterval)*time.Minute
		}
	}
	if enqueue {
		for _, id := range config.Watchdog.NotificationIDs {
			channel, ok := config.Channels[id]
			if !ok || channel.Notification == nil || channel.Notification.ID != id || channel.Version != record.ConfigRevision {
				return record, nil, domain.ErrValidation
			}
			if !channel.Notification.Active {
				continue
			}
			deliveryID, err := newUUIDv4()
			if err != nil {
				return record, nil, err
			}
			record.DeliveryIntents = append(record.DeliveryIntents, domain.DeliveryIntent{DeliveryID: deliveryID, SourceAlertID: incident.SourceAlertID,
				SourceTransitionVersion: incident.TransitionVersion, ProbeID: probeID, NotificationID: id, NotificationVersion: channel.Version,
				EventKind: domain.DeliveryEventProbeConnection, AvailableAt: at})
		}
	}
	return record, timer, domain.ValidateProbeWatchdogCommit(probeID, before, record)
}

func resolvedWatchdogIncident(prior *domain.RegionalIncident, revision int64, at time.Time, reason string) (*domain.RegionalIncident, error) {
	if prior == nil || prior.TransitionVersion == math.MaxInt64 {
		return nil, ports.ErrConflict
	}
	resolved := *prior
	resolved.Status, resolved.ResolvedAt, resolved.Reason = domain.AlertStatusResolved, &at, reason
	resolved.ConfigRevision = revision
	resolved.TransitionVersion++
	return &resolved, nil
}
